package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(_ context.Context, req *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	// Your Code Here (1).
	reader, _ := server.storage.Reader(req.Context)
	value, _ := reader.GetCF(req.Cf, req.Key)
	return &kvrpcpb.RawGetResponse{Value: value}, nil
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be modified
	put := storage.Modify{storage.Put{Key: req.Key, Value: req.Value, Cf: req.Cf}}
	server.storage.Write(req.Context, []storage.Modify{put})
	return nil, nil
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be deleted
	delete := storage.Modify{storage.Delete{Key: req.Key, Cf: req.Cf}}
	server.storage.Write(req.Context, []storage.Modify{delete})
	return nil, nil
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	// Your Code Here (1).
	// Hint: Consider using reader.IterCF
	reader, _ := server.storage.Reader(req.Context)
	iter := reader.IterCF(req.Cf)
	kvs := []*kvrpcpb.KvPair{}
	for iter != nil {
		if iter.Valid() {
			item := iter.Item()
			k := item.Key()
			v, _ := item.Value()
			kvs = append(kvs, &kvrpcpb.KvPair{Key: k, Value: v})
		}
	}

	return &kvrpcpb.RawScanResponse{Kvs: kvs}, nil
}
