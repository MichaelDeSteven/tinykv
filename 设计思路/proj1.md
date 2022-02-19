# Project1 StandaloneKV

## 概述

在本次Project，你将实现一个支持CF（列簇）的单机KV存储引擎。列簇可以类似键名字空间的属于，可以简单认为是多个列簇分离成了多个小型数据库。它是用来支持Project4事务模型。你将了解为什么TinyKV需要CF

服务支持四种基本操作：Put/Delete/Get/Scan。

* `Put` 操作用于替换指定的CF中某个key的值

* `Delete` 操作用于删除指定CF中某个key
* `Get`用于获取指定CF中某个key当前的value
* `Scan`用于获取指定CF一系列key的value



project1可以被分解成2个步骤完成

* 实现一个单机存储引擎
* 实现存储引擎相关的API





## 实现过程

 `gRPC` 服务被`kv/main.go` 初始化，`tinykv.Server`包含了一个提供 `gRPC` 服务即`TinyKv`。它被protocol-buffer定义，rpc的请求响应的具体内容位于`proto/proto/tinykvpb.proto`

通常你不需要改变proto文件，因为所有需要的字段都已经定义给你了。如果想要改变proto文件，可以修改proto文件，然后运行

`make proto`更新

除此之外，`Server` 依赖于`Storage`，这个是你需要为单机存储引擎实现的接口，位于`kv/storage/standalone_storage/standalone_storage.go`.一旦`Storage`接口的实现类`StandaloneStorage`被实现，你就可以实现一个原始kv服务



### 实现单机存储引擎

首先需要实现一个封装了badger（使用Go语言实现的一个可嵌入的、持久化的kv数据库）的kvAPI。在这部分内容中单机存储引擎就是一个封装了badger的kvAPI，提供如下两个方法



```go
type Storage interface {
    // Other stuffs
    Write(ctx *kvrpcpb.Context, batch []Modify) error
    Reader(ctx *kvrpcpb.Context) (StorageReader, error)
}
```



`Write` 应该提供应用多个修改操作方法

`Reader` 应该返回一个`StorageReader` 提供了kv基于快照的get和scan操作



这一部分思路可以参考`raft_storage/raft_server.go`



* 数据结构定义：模仿NewRaftStorage函数，TinyKV有两个数据库（kvDB和raftDB），单机版的只需要提供一个kvDB即可，然后就是config

```go
type StandAloneStorage struct {
	// Your Data Here (1).
	engines *engine_util.Engines
	config  *config.Config
}
func NewStandAloneStorage(conf *config.Config) *StandAloneStorage {
	// Your Code Here (1).
	dbPath := conf.DBPath
	kvPath := filepath.Join(dbPath, "kv")

	kvDB := engine_util.CreateDB(kvPath, false)
	engines := engine_util.NewEngines(kvDB, nil, kvPath, "")

	return &StandAloneStorage{engines: engines, config: conf}
}
```



Modify数据结构

```go
// Modify is a single modification to TinyKV's underlying storage.
type Modify struct {
	Data interface{}
}

type Put struct {
	Key   []byte
	Value []byte
	Cf    string
}

type Delete struct {
	Key []byte
	Cf  string
}
```



* Write方法：使用engine_util提供的PutCF方法，将Modify转为Put，然后依次将Cf、Key、Value存储在Kv数据库



```go
type StorageReader interface {
	// When the key doesn't exist, return nil for the value
	GetCF(cf string, key []byte) ([]byte, error)
	IterCF(cf string) engine_util.DBIterator
	Close()
}
```

* Reader方法：提供StorageReader，用来读取或者遍历CF，这里参考RegionReader需要实现一个类，由于是单机版，所以数据结构只需要提供事务



```go
type StandaloneReader struct {
	txn *badger.Txn
}

func (r *StandaloneReader) GetCF(cf string, key []byte) ([]byte, error) {
	value, err := engine_util.GetCFFromTxn(r.txn, cf, key)
	return value, err
}

func (r *StandaloneReader) IterCF(cf string) engine_util.DBIterator {
	return engine_util.NewCFIterator(cf, r.txn)
}

func (r *StandaloneReader) Close() {
	r.txn.Discard()
}
```





### 实现存储引擎相关API

project1最后一步是实现rawKV服务即RawGet/ RawScan/ RawPut/ RawDelete



* RawPut：将request的Cf、Key、Value封装成Put数据结构，然后使用Write写入存储引擎



* RawDelete：与RawPut同理，只不过数据结构为Delete



* RawGet：首先是获取reader，然后根据Cf和Key获得value，这里需要注意`RawGetResponse`数据结构，可能存在找不到的情况，如果value为nil，那么将NotFound设置为true

```go
type RawGetResponse struct {
    RegionError *errorpb.Error 
    Error       string         
    Value       []byte         
    // True if the requested key doesn't exist; another error will not be signalled.
    NotFound             bool  
    ...
}
```



* RawScan：这个方法稍微复杂，使用req中的Cf以及IterCF方法获取迭代器，然后从req给的startKey开始，使用迭代器遍历，获取kv对，将kv对封装成`kvrpcpb.KvPair`，然后返回，注意req有一个Limit字段，表示最多能够读取的kv对数量


