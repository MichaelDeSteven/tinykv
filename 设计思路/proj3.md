# Project3 MultiRaftKV

在project2实现了一个基于Raft的高可用kv服务。这次Project将会实现一个multi raftKV服务，即一个Node包含多个Region，不同的Node相同的Region通过Raft算法提供该属于Region的Key的服务，使得KV数据库是一个分布式的可拓展可容错的服务



在这次project中有三个部分，包括

* 实现成员变更和Leader变更
* 实现配置变更以及分割region
* 引入scheduler



## Part A



成员变更是通过RawNode中ProposeConfChange和ApplyConfChange实现的，因此addNode、removeNode方法只简单变更Prs即可，特别的，Leader删除Node节点后，可以尝试更新commitIndex



**需要了解的消息类型**

* MsgTransferLeader：Leader请求变更
* MsgTimeoutNow：发送给转化为Leader的节点，使得该节点立即进入新一轮选举



Leader如果接收到MsgTransferLeader会先检查变更为Leader节点是否有资格担任Leader，首先判断节点是否合法，合法则设置leadTransferee为该节点，然后检查日志是否为最新数据（more up-date），不是的话Leader会继续向该节点sendAppend，直到为最新后，向该节点发送MsgTimeoutNow，收到MsgTimeoutNow消息的节点只需要简单发起选举即可，针对TestTransferNonMember3A情况，增加判断该配置中有无本节点，没有该节点MsgTimeoutNow会被直接丢弃





## Part B

PartB部分是基于前面的内容完善admin命令，admin有以下四种命令

* CompactLog (已经在3C中实现)
* TransferLeader
* ChangePeer
* Split



### TransferLeader

这一部分非常简单，只要将TransferLeader作为一个普通的命令但是并不需要复制给其它peer，所以只需要调用TransferLeader，然后直接调用cb.Done完成命令



### ChangePeer

**总体思路**

* 这里将命令序列化成ConfChange，不要忘了放Context，然后调用`ProposeConfChange`提议配置变更命令
* 由于配置命令视为特殊的命令，因此特判EntryType是否为配置类型，如果是则将entriy的Data反序列化为ConfChange，这里测试代码直到配置变更最终被应用前会一直发送同一份配置变更，这种需要使用PendingConfIndex忽略多余的配置变更消息，3ARaft模块做配置变更时设置PendingConfIndex为当前日志索引，如果当前AppliedIndex小于当前的PendingConfIndex说明是过期的命令可以直接忽略
* 当该命令日志被提交后，再由process进行处理对Add和Remove操作分别进行处理，在增删节点时需要判断节点是否在peer
  * 如果是Add且不在peer执行Add逻辑，首先时更新region的Peers，ConfVer++，还需要storeMeta里的region，PeerCache插入peer
  * 如果是Remove且不在peer执行Remove逻辑，首先对于当前节点是被删除节点需要调用destroyPeer删除，这里使用的是NodeId和d.Meta.Id判断，然后使用util.RemovePeer移除指定的region，ConfVer++，PeerCache删除peer
* 把RegionState更新为Normal，最后ApplyConfChange并响应callback函数给用户



**其它**

* 如果handler以及regionState确定没问题，有可能是因为3A中ChangePeer出现了问题。比如我在测试TestBasicConfChange3B时在add peer (2, 2)卡住不动了，经过日志排查发现是Leader发送心跳时，未接收到导致无法创建Peer。这是因为在3A中Step函数增加了判断节点不在Prs直接忽略消息（TestTransferNonMember3A已被移除的节点需要忽略消息）为了TestBasicConfChange3B和TestTransferNonMember3A都能通过，可以缩小范围，只在处理TimeoutNow消息时判断节点是否在Prs。
* unexpected raft log index: lastIndex < appliedIndex：TestBasicConfChange3B还遇到了这样的一个问题，反复ChangePeer的handler没问题，推断可能是遗漏了什么检查，AppliedIndex在process完成后才会更新，但是节点被移除后peer应该是停止运行了，因此在更新AppliedIndex，还需要判断peer是否被移除了，peer使用stopped字段标记peer是否已经被销毁



### Split

这一部分首先是考虑命令是否正确，从以下几个方面考虑

* RegionEpoch
* RegionId
* RegionKey
* Get/Put/Delete Key

针对这几个可以封装几个函数在处理请求前进行额外判断，如果err不为空则提前使用cb.Done返回



然后是需要考虑split region要更新些什么东西

* newRegion：创建一个新的region，startKey为命令给定的splitKey，endKey为原来region的endKey，RegionEpoch的初始化为InitEpochConfVer、InitEpochVer，对于命令给定的NewPeerIds创建新的peer，storeId应该与原来Region的storeId对应
* Region：endKey更新为splitKey，Version++，storeMeta更新regionRanges和regions
* RegionState：两个RegionState都设置为Normal
* newPeer：使用createPeer创建newPeer
* 使用路由注册peer并向newPeer发送Start信息



关于OneSplit卡死的问题

* 网络分区情况下，如果5当选Leader后无法响应客户的请求，导致卡死的请求。一个简单的办法是引入Leader Lease机制，具体做法是Prs数据结构加入recentActive字段代表当前Follower是否活跃，如果收到Follower的消息那么recentActive为true，然后Leader每次发送心跳时，检测每个Follower的recentActive，如果超过大多数Follower没回应那么认定出现了网络分区，此时Leader下线



## Part C

这一部分是实现调度器一些功能，调度器的作用主要有

* 发送AddPeer或者RemovePeer命令

* 定期检查集群中是否有不平衡的情况，然后通过调整来达到平衡
* 处理region的心跳，来更新region的信息



### 处理region心跳

首先获得region的epoch信息，如果为空则直接返回，然后判断ConfVer和Version，其中一个小于原来的epoch则认为该信息为过期直接返回，使用GetRegion本地是否有该region，如果有则直接使用`RaftCluster.core.PutRegion`以及`RaftCluster.core.UpdateStoreStatus`更新该region以及store的状态。如果找不到region那么就使用ScanRegions覆盖该region的位于[startKey, endKey)之间的region，判断epoch是否过期，未过期则更新





### 平衡region

* 首选Scheduler挑选说有合适的stores（状态为up同时down time不大于`MaxStoreDownTime`），按照region大小从大到小排序
* 然后依次使用`GetPendingRegionsWithLock`, `GetFollowersWithLock` and `GetLeadersWithLock`.直到选择了一个合适的region
* 从小到大选择一个最小的targeStore，需要保证region的GetStoreIds包含该storeId，同时原来存储region的store的regionStore与迁移到目标store的差要小于2*region的approximateSize
* 使用AllocPeer创建一个peer，然后使用CreateMovePeerOperator仿佛创建一个`MovePeer` 操作，将结果返回即可

