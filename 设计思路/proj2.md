# Project2 RaftKV文档



## 概述

实现一个基于Raft算法的可容错KV存储服务。该部分首先需要对Raft算法有一个基本的了解包括（Leader选举、日志复制、安全性、日志压缩），Raft算法的实现除了参考论文的实现以外，也可以通过阅读etcd对算法的实现得到一些启发。然后由于tinykv是参考TiKV架构实现的，在PartB部分当中需要对项目有一个整体的把握，因此了解TiVK源码部分，也对PartB有一定的帮助，最后是PartC部分，这个算是Raft算法的一个优化手段，通过快照的方式使得日志的大小得到控制，保证日志大小不会无限增长。



## Raft算法解读

Raft是一个分布式一致性协议算法，它结果等价于Paxos算法，但是易于理解，并且实现起来比Paxos简单。它是一个强Leader协议，即leader节点用于处理client日志数据，并将数据同步给其它节点，只有当超过半数的节点同步成功，leader才会提交给日志数据，从而将数据应用到状态机。Raft论文将Raft算法拆解了以下几个子问题，使得Raft算法更加易于理解。

* leader选举：集群中leader的选举
* 日志复制：leader节点要将客户的请求序列化成日志数据然后同步给其它节点过程
* 安全性：确保每个节点的状态机应用相同的索引上的日志数据执行的都是相同的命令



### Raft算法基础

Raft算法中，节点的状态有以下三种

* Leader：接收客户请求，并同步给其它节点，超过半数同意则提交日志
* Follower：Follower被动接收数据，然后同步到日志，如果接收到用户请求则会重定向给Leader
* Candidate：介于Leader和Follower的中间状态，用于选举Leader



![raft states](https://cdn.jsdelivr.net/gh/lichuang/lichuang.github.io/media/imgs/20180921-raft/raft-states.jpg)



**性质**

* 选举安全性（Election Safety）：每个任期最多只有一个Leader
* Leader日志只追加（Leader Append-Only）：Leader永远不会覆盖或者删除自己的entry，只会追加
* 日志匹配性（Log Matching）：如果两个节点的日志中相同的index中的entry包含的任期相同，那么这个日志之前的日志数据完全匹配
* Leader完备性（Leader Completeness）：一条日志在某个任期被提交，那么在任期更高的Leader中仍然存在
* 状态机安全性（State Machine Safety）：如果一个节点将一条已经提交过的日志应用到了状态机，那么其他节点就不可能将相同索引的另一条日志应用





### Leader选举

刚开始启动时，所有节点状态均为Follower，不会收到Leader的心跳，因此选举超时器（election timeout）超时，触发Leader选举，首先会任期+1，状态变更为Candidate，然后向其它节点发送选举请求

选举请求基本格式为

| term         | 选举任期           |
| ------------ | ------------------ |
| candidateId  | 候选人Id           |
| lastLogIndex | 最后一条日志的索引 |
| lastLogTerm  | 最后一条日志的任期 |



响应格式

| term   | 节点的任期 |
| ------ | ---------- |
| Reject | 是否拒绝   |





选举请求的节点有以下几种**响应**可能

* 任期号小于当前任期则拒绝
* 如果未投票或者以及为candidate投过票，同时满足安全性的选举限制见Raft论文的5.4.1，那么同意投票，否则拒绝



**选举的结果**有以下几种可能

* 选举成功，candidate成为Leader
* 选举失败，其它节点当选为Leader
* 既没成功也没失败，那么会再次发起一轮投票



**分票**

第三种选举可能理论上是可能无限进行下去的，为了减少分票的情况，可以对每个节点的election timeout随机化，在etcd中，election timeout随机的值在[election timeout, 2 * election timeout)中



### 日志复制

一旦Leader确定，那么就可以开始处理客户的请求，每个客户的请求包含一条可以应用在状态机的命令，Leader首选会将命令封装成entry，然后使用AppendEntries RPC同步给每个节点，一旦超过多数节点确认，那么entry会被提交，Leader就可以将该条命令应用在状态机并将结果返回给客户。



如何进行日志同步？

首先需要确定Follower**当前日志的状态**

```go
// Leader维护一个数据结构Progress，它是Leader对每个节点当前日志状态的视图
// Match表示已知当前Follower与Leader已同步的最新日志索引，每次选举成功后被初始化为0
// Next表示当前需要给Follower发送的下一条日志索引，被初始化为当前Leader的最后一条日志索引
// Next与Match数量关系满足：Match + 1 <= Next
// Match + 1 == Next，即Match是Leader当前最后一条日志索引，已经没有新的日志同步给Leader了
type Progress struct {
	Match, Next  uint64
}
```



然后利用AppendEntries RPC同步给每个节点，这里需要知道Raft算法满足**Log Matching**原则，所以确认同步的方法就是通过从后向前试探当前索引的日志是否同步，如果相同说明**该条索引以及之前的日志同步**，将RPC中的entries并入Follower日志中，否则继续向前试探



追加请求格式

| term         | 当前节点任期                    |
| ------------ | ------------------------------- |
| leaderId     | 当前任期的Leader                |
| prevLogIndex | 日志entries之前的一个日志索引   |
| prevLogTerm  | prevLogIndex日志对应的任期      |
| entries[]    | 需要存储的日志entries，如果为空 |
| leaderCommit | Leader当前的提交索引            |



收到追加请求的节点处理流程如下

* 任期小于当前任期：直接拒绝
* prevLogIndex对应的Term不等于prevLogTerm，说明还未同步，直接拒绝
* prevLogIndex等于对应的Term则将消息中的entries同步到当前的日志中
  * 如果entries中的entry的对应索引存在entry且term不相同说明冲突了，将老的entry替换成新的entry
  * 对应索引不存在entry则直接追加（如果当前日志索引存在而entries中没有的entry则保留）
* 如果leaderCommitIndex大于当前的commitIndex，则更新当前节点的commitIndex = min(leaderCommitIndex, 最后一条日志索引)



Leader收到追加请求说明此次同步是失败的

* 如果拒绝，则Match自减，然后继续尝试发送
* 如果成功，则尝试提交，更新自己的commitIndex





### 安全性

安全性简单来说就是为了保证Leader拥有的是当前最新的日志数据，以及已提交的日志不会被覆盖，主要从以下两点展开

![pre term log](https://cdn.jsdelivr.net/gh/lichuang/lichuang.github.io/media/imgs/20180921-raft/preterm-log.jpeg)

#### 选举限制



为了让Leader拥有当前最新日志，Leader选举需要增加一条选举限制（Election restriction），数据的新旧判断逻辑如下

* 如果两个节点的日志最后一条索引的任期不同，任期大的节点日志数据更加新（more up-to-date）
* 如果任期相同，那么索引大的节点日志数据更加新

只有candidate的日志数据更加新才能同意本次选举



#### 提交前任日志

考虑以下情况

* a：S1成为Leader，2的数据未被提交
* b：S5成为了Leader，3的数据也未被提交
* c：S1又成为了Leader，此时会将2数据被同步到了大多数节点，并且待同步数据4

如果此时在提交前index2的数据2，S1崩溃了，可能出现d1和d2两种情况：已被半数以上节点同意并提交的情况下，仍然存在已提交数据的情况，因此提交的日志作出限制，只能提交本任期的日志条目（前任日志条目被间接提交），所以每次Leader选举完毕后，需要向Follower发送no-op日志，如果当前任期的日志提交成功，那么前任的日志也就提交成功



## Raft算法拓展与优化



### 日志压缩

随着时间推移，存储的日志会越来越多，会导致磁盘空间被耗尽。日志压缩就是对日志大小的优化，基本思路就是将过期的信息丢弃，例如将x原来为2，设置为3之后，x=2即为过期的数据。一旦日志记录被提交且应用在状态机上，那么该记录的中间状态就可以被压缩掉。日志压缩具体需要考虑有以下几点



* 由谁进行日志压缩：每个节点独立进行日志压缩，这样做的好处是减少了Leader处理消息的负担

* 什么时候进行日志压缩：日志压缩如果太过频繁进行日志压缩，将会浪费磁盘和带宽，太不频繁有可能导致存储空间早已耗尽，一个合适的方法是设置一个阈值，一旦日志的大小超过阈值则做日志压缩

* 如何保存快照：对于日志压缩后的快照，通过对其序列化写入磁盘，加载快照则使用反序列化。写快照可能存在性能问题，导致其它的请求阻塞，解决办法使用COW技术并发写快照
* 如何传输快照：将传输快照当作普通的消息来发送，但是可能存在一些问题，快照的消息大小可能远大于其它消息，这样需要花费更加长的时间发送，可能存在网络拥塞问题。对于这种情况，一种解决办法是考虑单独为每个Snapshot开辟一条网络连接，将Snapshot拆分成多个Chunk进行传输







### 成员变更

某些情况下，需要手动变更集群中的成员，显然不能直接增加或者删除节点，这样可能会带来一些安全性问题。比如出现多个Leader，为了简化问题。每次变更成员最多增加或者删除一个节点。

Raft采用将修改集群配置命令放在日志来处理，这样的好处是：

* 可以继续沿用原来的方式同步日志，而配置变更可以看作是一种特殊的命令
* 可以继续处理客户的请求



#### 添加节点

集群中的节点按照新的配置进行选举，即被添加的节点是合法的投票人，由于新的节点落后其它节点，因此不可能成为Leader，只有完成日志同步后新增的节点才有可能成为Leader，这种方式的好处是不需要设置其它复杂的方式，只需要简单的加入节点即可。缺点是当遇到网络拥塞或者分区时，由于新的节点远远落后于集群日志会导致集群出现的故障大大增加。解决方法是可以采用etcd的learner机制，新加入的节点暂时步参与选举，集群中的节点按照旧的配置进行选举，同时learner需要追上最新日志，这种方式的复杂地方在于需要新增消息进行处理，同时还要考虑如何通知集群中的节点，新节点已经被提升。





#### 删除节点

删除节点需要考虑该节点是否为Leader，如果是那么只有当配置变更命令被提交Leader才能下线，这样在选举超时后就会发起新的一轮选举





## PartA

PartA是实现一个最基本的Raft算法，根据文档说明，该部分可以拆解成三个部分的实现





### 2AA Leader选举

**总览**

为了实现Leader选举，可以从`raft.Raft.tick()` 开始，它是一个逻辑时钟用来驱动election timeout或者heartbeat timeout（可以简单理解为上层会有一个worker定时调用tick来增加electionElapsed或者heartbeatElapsed，如果Elapsed>=timeout就执行个事件）如果想要发送消息则直接追加到`raft.Raft.msgs`数组中，经过上层路由转发到各自的状态机，然后会调用Step去执行对应的事件。因此`raft.Raft.Step()`就是raft消息的入口。



**需要了解的消息类型**

* MsgHup：本地消息用于选举。如果节点为follower或者candidate，那么tick方法会执行tickElection，节点在election timeout前未收到心跳，那么节点将会发起新一轮的选举
* MsgRequestVote：发起选举时向其它节点发起的消息类型
* MsgRequestVoteResponse：对MsgRequestVote消息类型的回应



**newRaft**

调用`c.Storage.InitialState()`可以在重启后，读取之前的状态（hardState、confState），然后更新持久化的Term、committed以及Vote，`c.peers`如果为空则c.peers更新为`confState.Nodes`，然后调用`becomeFollower`完成初始化



新增randomElectionTimeout：为了避免无限制的Leader选举，每次选举超时时随机化randomElectionTimeout值位于[electionTimeout, 2*electionTimeout)区间



**becomeFollower**

更新term、Leader、state、调用reset（重置electionElapsed、heartbeatElapsed为0，votes更新为空，更新randomElectionTimeout）



**becomeCandidate**

term++，更新state、Vote、votes，调用reset



**becomeLeader**

更新state，Lead，更新Follower的Prs，Match=0，Next=最后日志索引+1，如果是自己Match=最后日志索引，Next=最后日志索引+1

向所有Follower发送noop entry（数据为空）



**hup**

hup用于非Leader执行选举，首先调用becomeCandidate成为候选人，如果只有一个节点，自动称为leader，如果有多个节点，分别向这些节点发送MsgRequestVote消息



**tickElection**

electionElapsed++，如果超时则发起Hup消息进入Leader选举



**tick**

如果状态为Leader，则调用tickHeartBeat，否则调用tickElection



**handlerRequestVoteResponse**

消息任期需要与当前任期相同，同时为候选人状态。对选票进行统计，如果超过半数则成为Leader，如果拒绝数超过半数成为Follower







### 2AB 日志复制





**需要了解的消息类型**

* MsgBeat：本地消息用于发送心跳。
* MsgPropose：本地消息用于发送日志。如果candidate收到该消息则忽略，follower收到该消息则重定向到Leader，Leader则会调用`appendEntry`方法追加entries到日志，然后调用`bcasetAppend`方法发送向follower发送entries
* MsgAppend：包含需要复制的entries消息，Leader当调用sendAppend会调用bcastAppend向其他follower同步日志，当candidate收到该消息则会变成follower
* MsgAppendResponse：对MsgAppend的响应，当candidate或者follower收到该消息，会调用`handleAppendEntries`方法
* MsgHeartbeat：Leader定期向Follower发送心跳的消息
* MsgHeartbeatResponse：对MsgHeartbeat的心跳回应





`log.go`代码实现

RaftLog部分，一开始接触是一个比较迷惑的内容，关键就是需要将每个index关联起来，给到的注释中很好的给出了答案。

```go
//  snapshot/first.....applied....committed....stabled.....last
//  --------|------------------------------------------------|
//                            log entries
```



首先在2C部分Raft实现了日志压缩功能，简而言之就是将多个entries最终合并成一个然后snapshot的index记作压缩前最大的一个index，那么first可以理解成未压缩的最开始的index，applied就是已经提交了并且被上层应用的最后一个index，commit就是raft算法中，被大多数节点确认并提交的index，stabled就是需要对raft的entries进行持久化的最后一个index，(stable，last]就是未持久化且未提交的entry，理解了才能更加清晰实现RaftLog



RaftLog数据结构

```go
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry
    
	// Your Data Here (2A).
	offset uint64
}

```



由于快照的存在，firstIndex就可能不是从0开始，这个时候entries数组的索引以及entry上的日志索引存在一个偏移量的关系，因此需要添加一个offset字段作为关系的映射，这个也是让我一开始搞不清每个index关系的重要原因，offset就是为了方便`unstableEntries`、`nextEnts`等方法，用于找到所需要的目标entry，根据注释解读的Index，可以知道offset=firstIndex，每次LogIndex-offset就是entry在当前entries数组的索引。

为了方便LogRaft方法的实现，参考自etcd提取entries的过程，可以实现一个`slice(lo, hi)`方法用于获取日志索引在[lo, hi)的entries，可以统一处理entries的处理过程。







**注意点**

* HeartBeat的逻辑与Append类似，发送心跳时只需要标注是谁发送的心跳信息即可，不需要多余的内容，Leader收到心跳直接开始sendAppend即可
* Leader提交时采用尝试的方式来更新commitIndex，对于某个Index有超过半数的Match>=Index，并且Index>commitIndex则更新，每次收到FollowerAppend响应就可以尝试更新



### 2AC 实现RawNode接口

RawNode可以理解未上层应用与Raft模块的桥梁，上层应用可以使用像`RawNode.Tick()`、`RawNode.Step()`、`RawNode.Propose()`这些方法与Raft算法交互，上层应用还使用`Ready`方法处理消息，运行逻辑时钟，Raft模块也依赖于上层应用，比如

* 发送消息给其它peer
* 持久化raft的关键信息
* 将已提交的日志应用到状态机上

这些都通过Ready方法返回Raft状态和信息给上层应用，在上层应用处理完毕后，再通过 `RawNode.Advance()`来更新Raft的状态如applied、stabled等



状态机应用流程

```go
for {
  select {
  case <-s.Ticker:
    Node.Tick()
  default:
    if Node.HasReady() {
      rd := Node.Ready()
      saveToStorage(rd.State, rd.Entries, rd.Snapshot)
      send(rd.Messages)
      for _, entry := range rd.CommittedEntries {
        process(entry)
      }
      s.Node.Advance(rd)
    }
}
```



首先上层应用会通过Ready函数取得当前的Raft信息，将Ready中待发送的Message发送给其他peer，然后会对已提交的entry进行应用，应用完毕之后，会使用Advance方法来更新Raft模块的状态





实现大致思路

* Ready数据结构：一言以蔽之，Ready封装了准备读取的entry和消息
* RawNode数据结构：需要判断Raft状态是否发生变化，如果发生才能去调用Ready并读取Raft相关状态，所以需要添加HardState以及SoftState
* HasReady：关注Raft以及RaftLog中的状态是否发生变化，比如是否新增待发送信息，是否有未持久化和已提交待应用的entry，对于像Vote、LeaderId需要使用HardState和SoftState数据结构进行封装，然后判断，更新返回true
* Ready：将Raft以及RaftLog中的状态和信息都封装到Ready，并更新当前的RawNode中的HardState以及SoftState，便于下次比较
* Advance：通知当前的Raft状态已经更新，首先是对Raft中的stabled和applied更新，如果HardState为空则赋值，对于SoftState需要为nil才赋值



## PartB

在这一部分，将会基于Raft算法构建一个可容错的KV存储服务。这里首先需要了解`Store`, `Peer` and `Region` 这几个概念

* Store代表一个kv服务的示例
* Peer代表运行在Store的一个Raft节点
* Region表示Peer的集合，也被称为Raft Group

这一部分暂时不需要考虑多Region的问题，这个会放在3B中



思路以该代码框架入手

```go
for {
  select {
  case <-s.Ticker:
    Node.Tick()
  default:
    if Node.HasReady() {
      rd := Node.Ready()
      saveToStorage(rd.State, rd.Entries, rd.Snapshot)
      send(rd.Messages)
      for _, entry := range rd.CommittedEntries {
        process(entry)
      }
      s.Node.Advance(rd)
    }
}
```



### peer storage

peer storage会持久化一些元数据，便于重启时恢复

* RaftLocalState：存储Raft的HardState以及最后的日志索引
* RaftApplyState：存储Raft最后应用的日志索引以及截断的日志信息
* RegionLocalState：存储Store的Region以及对应的Peer信息

TinyKV架构参考TiKV设计，对应的TinyKV也有两个数据库raftdb、kvdb

* raftdb存储Raft日志以及RaftLocalState
* kvdb 存储KV对的列簇，RaftApplyState以及RegionLocalState，这个数据库可以简单认为是Raft提到的状态机



这部分只要实现`PeerStorage.SaveReadyState`，该方法的作用是将RaftHardStae和Raft entry保存到badger

* Append：主要做三件事情，首先会将给定entry数组进行保存，然后针对每个entry更新ps.raftState，将LastIndex\LastTerm更新为最新，同时删除一些永远不会被提交的entry，使用meta.RaftLogKey，将entry的Key进行编码
* SaveReadyState：这PartB只需要简单的使用WriteBatch调用Append最后使用WriteToDB将raft信息进行持久化，然后更新HardState



### raft ready process

这一部分能看到整个KV数据库读写操作执行的过程

* 客户使用RawGet/RawPut/RawDelete/RawScan RPC请求
* RPC调用`RaftStorage` 的相关的方法然后会发送一个Raft命令请求给raftstore，等待响应
* `RaftStore`请求Raft命令会被封装成日志
* Raft模块会追加该日志并持久化到 `PeerStorage`
* Raft模块提交日志
* 在处理Raft ready时，Raft worker会执行Raft命令并通过回调函数返回结果
* `RaftStorage`从回调函数收到响应然后会将结果以RPC的形式返回给客户



实现内容

* proposeRaftCommand：peer#proposals数组用来存储Raft命令的回调函数数组，先创建Raft命令的回调函数追加到proposals数组nextProposalIndex，用于获取index，然后使用Propose，给Raft模块完成提交
* 处理Raft ready时，在process根据对应的命令类型执行指定的命令，这部分只需要简单处理Get/Put/Delete/Snap命令，这里关键的地方是如何正确找到对应的回调函数返回结果给客户。在测试的时候，如果处理不当，会导致callback函数没有被处理而出现卡死的情况，整个框架处理如下：

```go
for len(d.proposals) > 0 {
    p := d.proposals[0]
    d.proposals = d.proposals[1:]
    // 先判断index是否相等，不相等则忽略
    if p.index != index {
        continue
    }
    // 再判断term是否相等，不相等代表请求过期
    if p.term != term {
        NotifyStaleReq(term, p.cb)
        continue
    }
    // 回调完成
    p.cb.Done(resp)
}
```

* 对于Snap回调函数时候还需要返回一个事务





## PartC

Raft日志不能无限制增加，而是会定期去检查Raft日志大小，如果发现超过阈值，就会通过压缩日志的方式来节省空间。



### Raft部分



**需要了解的消息类型**

* MsgSnapshot：请求设置快照。当节点是Leader处理sendAppend方法时，如果获取term或者entries失败，那么Leader就会发送MsgSnapshot消息给Leader



**sendAppend**

这里参考了etcd发送快照消息的流程，对sendAppend方法改造，在获取term或者entries中如果失败，那么会调用r.RaftLog.Snapshot()获得快照并发送，对于Follower的Next更新为快照Index+1



**handleSnapshot**

如果任期小于当前任期则忽略，否则更新firstIndex、committed、applied、stabled，entries清空同时设置第一个entiry为快照的index与term，pendingSnapshot设置为消息中的snapshot，表示有未初始化的快照，使用ConfState更新prs，最后发送MsgAppendResponse消息回应Leader



**maybeCompact**

持久化完快照，对删除内存中多余的entries，使用storage.FirstIndex()方法来获得第一个未被压缩的索引，再与之前的offset偏移量相减得到删除后的entries第一个entry索引index，index以前的删除，然后将offset更新成FirstIndex



**RawNode部分**

Ready的数据结构包含需要持久化的快照，HasReady增加判断快照是否为空逻辑，如果不为空Ready的快照设置为pendingSnapshot，在快照持久化完毕后，调用Advance，此时调用maybeCompact删除内存中不必要的entry



### raftstore部分



**peer_storage部分**

SaveReadyState增加更新快照逻辑部分，如果快照不为空则调用ApplySnapshot方法，首先调用clearMeta，clearExtraData删除过期的数据，然后更新raftState、applyState的状态，然后写入kvdb和raftdb，将`PeerStorage.snapState` 设置为 `snap.SnapState_Applying`，最后是向regionSched发送RegionTaskApply任务







**peer_msg_handler部分**

请求类型增加AdminCmdType_CompactLog，用来更新RaftTruncatedState，然后调用ScheduleCompactLog.Raftlog-gc worker将会异步执行日志删除任务，需要注意的是CompactLog需要保证CompactIndex>truncatedIndex，否则测试会会出现过多的no need gc

