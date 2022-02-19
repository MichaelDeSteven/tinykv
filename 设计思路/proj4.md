# Project4 Transactions



## 概述

在前面的实验中实现了一个基于Raft协议multple node的可容错的kv数据库，为了使得数据库可拓展，该数据库必须能处理多个client请求。多clieant请求可能出现以下问题，如果两个client同时去尝试写同一个key会发生什么，如果client写并且立即读该key，他们读到的值会与之前所写的值相同吗？在project4，你将会解决这些问题

事务系统是TinySQL和TinyKV之间的协作遵循的协议。两者需要被正确实现以确保事务的性质。我们拥有完整的事务请求API，独立于project1实现的raw请求（事实上，如果客户同时使用raw和事务api，那么就不能保证事务的性质）

事务保证了快照隔离，即在一个事务中，客户将从数据库中读取的数据仿佛是在事务开始的一个时刻（事务看到的是数据库的一致性视图）事务所有写操作要么成功要么失败（与其它事务存在冲突）

为了遵循SI，需要改变存储数据的方式，使用key和timestamp来存储value，这种方式也叫做多版本并发控制（MVCC），使用这种方式的原因是对于每一个key可能有不同版本的value

在本次Project中，将实现MVCC以及有关事务的API



## Percolator

TinyKV采用Percolator来实现事务，它是一个两阶段提交的协议（2PC）

事务就是读和写操作组成的序列，一个事务开始时和提交均有一个时间戳分别是start timestamp和commit timestamp（一个事务的commit timestamp必须大于start timestamp）对一个key写操作时start与commit时间戳之间必须没有没有被其它事务写，否则事务将取消（即写冲突）



* prewrite阶段
  * 首先向所在行的写操作选一个作为primary key，其他的作为secondary key
  * 获取start timestamp尝试对primary key上锁，锁记录了start timestamp、primary key以及锁的过期时间TTL
  * 上锁之前判断是否存在其他事务，如果存在表明写冲突，后判断该key是否有锁，存在被其它事务上锁
  * 若不存在上面两种情况则成功上锁，并将数据写入，primary key上锁后，secondary key进入prewrite

* commit阶段
  * 提交primary key，时间戳为commit timestamp，同时包含对应事务的start timestamp，表明为该事务的提交记录
  * 若存在key被其它事务上锁，则提交失败
  * 若提交成功，则删除对应的锁，异步提交secondary key
* 读操作
  * 判断key是否被上锁，如果一直读不到将会清除锁



事实上，因为key可能在多个region因而存储在不同的raft group中，所以client将会发送多次`KvPrewrite`请求给每一个region的leader。每个prewrite包含只对该region的修改操作。如果所有的prewrite都成功了，clieant将会发送commit请求给包含了primary key的region。commit请求有commit时间戳（从TinyScheduler获取），该时间戳是事务写操作被提交的时间，引起对其它事务是可见的

如果其中一个prewrite失败，客户端会给所有region发送`KvBatchRollback` 请求让事务回滚（为了解锁事务中的key以及移除prewrite的value）

在TinyKV中TTL检测自动执行的。为了初始化过期检测，客户端会向TinyKV发送包含当前时间的 `KvCheckTxnStatus`请求。请求会通过primary key和start timestamp找出事务。首先判断锁是否丢失或者已经提交，如果不是那么TinyKV会比较锁的TTL和`KvCheckTxnStatus`请求的时间戳，如果锁超时，那么TinyKV将对锁回滚，在任何情况下，TinyKV会对锁的状态做出回应以便让客户端发送`KvResolveLock` 请求采取措施。由于存在其它事务的锁导致事务prewrite失败，通常客户端会检查事务的状态

如果primary key提交成功，那么客户端将会向其它region提交key，因为对prewrite请求总是响应，所以这些请求通常应该成功。服务端保证如果收到事务的commit请求那么将成功。一旦客户端收到所有的prewrite响应，事务唯一可能失败的原因是超时了，这种情况下，提交primary key应该失败。一旦primary key被提交，其它key将不再有超时

如果primary提交失败，客户端将发送`KvBatchRollback`请求回滚事务





![](https://img1.www.pingcap.com/prod/1_684a7671be.jpg)

​                                                                                                          TiKV事务模型





## Part A

在之前的project1已经实现了基于存储底层存储引擎Badger提供rawAPI来提供KV服务。但是Badger并不在分布式事务层，为了处理TinyKV的事务和对用户KV操作需要提供MVCC，然后基于MVCC层分布式构建分布式事务模型，在这一部分将在TinyKV中实现一个MVCC层



MVCC层存储键值对将由原来的（Key，Value）变为（Key，Value，Timestamp）表明key的value值含有多个版本



TinyKV使用3种CF

* CfDefault：使用（Key，Start Timestamp）访问，它只存储value
* CfLock：存储锁，使用key访问，它存储了一个Lock数据结构的序列化信息（位于lock.go）
* CfWrite：write操作使用（Key，Commit Timestamp）访问，它存储Write数据结构的信息（位于write.go）

key和timestamp将组成*encoded key*，这些key首先按key升序排序然后按时间戳降序排序，确保了遍历时能先获得key的最新版本，key的编码解码API定义在transaction.go

本部分即需要实现一个独立的`MvccTxn`结构体，在partB和C将会使用`MvccTxn`API来实现事务的API。

`MvccTxn`由数据结构可以简单认为是提供了当前基于当前快照（StartTS决定）的事务的锁、write、value的类

```go
// MvccTxn groups together writes as part of a single transaction. It also provides an abstraction over low-level
// storage, lowering the concepts of timestamps, writes, and locks into plain keys and values.
type MvccTxn struct {
	StartTS uint64
	Reader  storage.StorageReader
	writes  []storage.Modify
}
```



根据提供的文档内容，不难实现`MvccTxn`的以下方法

* PutWrite/PutLock/PutValue：CF根据写入的类型确定，key由Key和Start Timestamp组成，使用EncodeKey编码组成*encoded key*追加到writes
* DeleteLock/DeleteValue：同上，数据结构由Put替换成Delete
* GetLock：使用GetCF读取key上的锁，如果不为nil则使用ParseLock字节数组反序列化得到锁，
* GetValue：用于获取最近提交的一次value，使用Reader遍历CfWrite列簇，得到item。item的key和value分别代表之前的（Key，Commit Timestamp）和write对象，value条件必须满足Key等于给定的key同时Commit Timestamp必须小于Start Timestamp，同时由于最新一次的write类型可能不是Put，这种情况需要继续遍历，直到得到一个满足条件的write，然后使用write的Start Timestamp，组成（key，Start Timestamp）从CfDefault列簇中获得最近的一次value
* CurrentWrite：CurrentWrite使用当前事务的Start Timestamp来查找当前的write对象，使用Reader遍历CfWrite列簇，得到item。解码得到write对象，判断对象的key以及时间戳是否与当前事务相同
* MostRecentWrite：MostRecentWrite使用key来寻找最近一次的write对象（最后一次commit），由于key在存储引擎的排序是先按key升序排序，再按时间戳降序，因此一旦key相同value即为key的最近一次write对象，直接返回Write和和Commit Timestamp



记录被保存在`MvccTxn`一旦所有的修改命令被收集起来，将会被立即写入底层数据库（Write方法）。这个确保了命令原子成功或原子失败。注意MVCC事务还不等同于TinySQL事务，一个MVCC事务包含单个命令的修改，而不是多个命令



## Part B

在PartB，将使用MvCCTxn实现`KvGet`, `KvPrewrite`, and `KvCommit`请求



* `KvGet` 通过提供的时间戳从数据库读取值。如果需要读取的key被其他事务锁住，则返回错误（响应字段Error添加kvrpcpb.KeyError，Locked字段添加当前锁的信息），否则TinyKV必须使用GetValue找到当前最新的value并返回

* `KvPrewrite` 将value真正写入数据库。遍历req中的Mutation数组（由Op、Key、Value组成）对于每一个key使用MostRecentWrite寻找最近一次的write，如果write对象的Start Timestamp大于req提供的StartVersion表明写冲突res追加对该key的错误信息，没有问题再判断该key有无锁，如果存在锁也失败，追加错误信息。如果key都没有问题则写入value和锁，**真正写入数据时要记得调用server.storage.Write**
* `KvCommit`并不改变数据库中的value，但是它记录value被提交。如果key被其他事务持有那么将失败，如果所有key都没有问题，那么删除key的锁同时写入write对象，表明提交成功，最后同样记得调用Write



## Part C

`KvCheckTxnStatus`, `KvBatchRollback`, and `KvResolveLock` 被客户端使用当尝试写事务时遭遇一些冲突。方法包含了改变已经存在的锁的状态

* `KvCheckTxnStatus` 用于检测超时，移除过期的锁以及返回锁的状态。查找事务当前的写获得write对象，write对象不为空说明事务要么被提交要么被回滚，如果write对象类型为WriteKindRollback，那么直接返回当前事务回滚信息，否则返回事务已被提交信息。write对象为空，再来检查锁的状态，如果锁为空或者过期那么需要进行回滚操作，删除当前的value和锁（如果存在的话），并写入类型为WriteKindRollback的write对象。锁如果为空，返回的消息类型为Action_LockNotExistRollback，不为空返回Action_TTLExpireRollback类型，最后一种情况是锁存在且未过期，则直接返回锁的Ttl信息
* `KvBatchRollback` 检查key是否被提交，已被提交返回错误，如果key被其它事务持有同样失败并返回，如果当前事务持有，移除锁，并对请求中的key进行回滚。
* `KvResolveLock` 检查批量锁住的key，并将其全部回滚或者提交。req给定了StartVersion使用AllLocksForTxn获得当前事务持有的所有锁，如果req的commit_version等于0则使用KvBatchRollback回滚事务，大于0则作为Commit Timestamp使用KvCommit提交事务





`KvScan`等价于 `RawScan`，它在一个时间点它从数据库读取多个value。由于MVCC，`KvScan`显然比`RawScan`更加复杂，因为key存在多个版本以及key的编码，所以不能单单遍历value。Scanner#Next方法迭代器模式将这一过程进行抽象。数据结构可以设计如下

```go
type Scanner struct {
	// Your Data Here (4C).
	txn  *MvccTxn
	key  []byte
	iter engine_util.DBIterator
}
```

* txn：表示需要当前事务
* key：表示下一个将要读取的值
* iter：从当前事务获取迭代器用于遍历并更新下次要读取的值

那么Next函数设计思路如下

* 根据key和当前事务的StartTS获得最新版本的iKey（即将返回的key）
* 由于key按key升序按时间戳降序排列，所以可以继续遍历直到key不等于iKey，用来更新Scanner下一次需要使用的key
* 使用GetValue和iKey获得value，并返回KV结果



有了Next函数`KvScan`自然就好做了，根据req创建Scanner和txn对象，使用Scanner#Next方法获取kv对并封装到res中，同时kv对数量不超过req给定的Limit

