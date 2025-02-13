package server

import (
	"context"

	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4A/4B)
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	// Your Code Here (4B).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.Version)
	lock, err := txn.GetLock(req.Key)
	if err != nil {
		panic(err)
	}
	if lock != nil && lock.Ts < req.Version {
		return &kvrpcpb.GetResponse{
			Error: &kvrpcpb.KeyError{Locked: lock.Info(req.Key)},
		}, err
	}
	value, err := txn.GetValue(req.Key)
	if value == nil {
		return &kvrpcpb.GetResponse{
			NotFound: true,
		}, nil
	}
	if err != nil {
		panic(err)
	}
	return &kvrpcpb.GetResponse{
		Value: value,
	}, nil
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	// Your Code Here (4B).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	res := &kvrpcpb.PrewriteResponse{Errors: make([]*kvrpcpb.KeyError, 0)}
	// 1. check lock
	// 2. write value and lock
	for _, mutation := range req.Mutations {
		op, k, v := mutation.Op, mutation.Key, mutation.Value
		write, st, err := txn.MostRecentWrite(k)
		if write != nil && st >= req.StartVersion {
			res.Errors = append(res.Errors,
				&kvrpcpb.KeyError{Conflict: &kvrpcpb.WriteConflict{
					StartTs:    req.StartVersion,
					ConflictTs: write.StartTS,
					Key:        k,
				}})
			continue
		}
		lock, err := txn.GetLock(k)
		if err != nil {
			panic(err)
		}
		if lock != nil && lock.Ts < req.StartVersion {
			res.Errors = append(res.Errors, &kvrpcpb.KeyError{Locked: lock.Info(k)})
			continue
		}

		if len(res.Errors) == 0 {
			txn.PutValue(k, v)
			txn.PutLock(k, &mvcc.Lock{
				Primary: req.PrimaryLock,
				Ts:      req.StartVersion,
				Ttl:     req.LockTtl,
				Kind:    mvcc.WriteKindFromProto(op),
			})
		}
	}
	server.storage.Write(req.Context, txn.Writes())
	return res, err
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	for _, key := range req.Keys {
		lock, err := txn.GetLock(key)
		if err != nil {
			panic(err)
		}
		if lock == nil {
			return &kvrpcpb.CommitResponse{}, nil
		}
		if lock != nil && lock.Ts != req.StartVersion {
			return &kvrpcpb.CommitResponse{Error: &kvrpcpb.KeyError{}}, nil
		}
		txn.PutWrite(key, req.CommitVersion, &mvcc.Write{StartTS: req.StartVersion, Kind: lock.Kind})
		txn.DeleteLock(key)
	}
	server.storage.Write(req.Context, txn.Writes())
	return &kvrpcpb.CommitResponse{}, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	kv := make([]*kvrpcpb.KvPair, 0)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.Version)
	scanner := mvcc.NewScanner(req.StartKey, txn)
	defer scanner.Close()
	for i := 0; i < int(req.Limit); i++ {
		k, v, err := scanner.Next()
		if k == nil {
			break
		}
		if v == nil || err != nil {
			continue
		}
		kv = append(kv, &kvrpcpb.KvPair{Key: k, Value: v})
	}
	return &kvrpcpb.ScanResponse{Pairs: kv}, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.LockTs)
	write, _, _ := txn.CurrentWrite(req.PrimaryKey)
	if write != nil {
		if write.Kind == mvcc.WriteKindRollback {
			return &kvrpcpb.CheckTxnStatusResponse{Action: kvrpcpb.Action_NoAction}, nil
		} else {
			return &kvrpcpb.CheckTxnStatusResponse{
				CommitVersion: write.StartTS,
				Action:        kvrpcpb.Action_NoAction,
			}, nil
		}
	}
	lock, err := txn.GetLock(req.PrimaryKey)
	if err != nil {
		panic(err)
	}
	if lock == nil || lock.Ttl < mvcc.PhysicalTime(req.CurrentTs)-mvcc.PhysicalTime(req.LockTs) {
		txn.DeleteLock(req.PrimaryKey)
		txn.DeleteValue(req.PrimaryKey)
		txn.PutWrite(req.PrimaryKey, req.LockTs, &mvcc.Write{StartTS: req.LockTs, Kind: mvcc.WriteKindRollback})
		server.storage.Write(req.Context, txn.Writes())
		if lock == nil {
			return &kvrpcpb.CheckTxnStatusResponse{Action: kvrpcpb.Action_LockNotExistRollback}, nil
		}
		return &kvrpcpb.CheckTxnStatusResponse{Action: kvrpcpb.Action_TTLExpireRollback}, nil
	} else {
		return &kvrpcpb.CheckTxnStatusResponse{Action: kvrpcpb.Action_NoAction, LockTtl: lock.Ttl}, nil
	}
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	for _, key := range req.Keys {
		write, _, _ := txn.CurrentWrite(key)
		if write != nil {
			if write.StartTS == req.StartVersion {
				if write.Kind != mvcc.WriteKindRollback {
					return &kvrpcpb.BatchRollbackResponse{Error: &kvrpcpb.KeyError{}}, nil
				} else {
					return &kvrpcpb.BatchRollbackResponse{}, nil
				}
			} else {
				return &kvrpcpb.BatchRollbackResponse{}, nil
			}
		}
		lock, err := txn.GetLock(key)
		if err != nil {
			panic(err)
		}
		if lock != nil && lock.Ts == req.StartVersion {
			txn.DeleteValue(key)
			txn.DeleteLock(key)
		}
		txn.PutWrite(key, req.StartVersion, &mvcc.Write{StartTS: req.StartVersion, Kind: mvcc.WriteKindRollback})
	}
	server.storage.Write(req.Context, txn.Writes())
	return &kvrpcpb.BatchRollbackResponse{}, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		panic(err)
	}
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	kls, err := mvcc.AllLocksForTxn(txn)
	if err != nil {
		panic(err)
	}
	keys := make([][]byte, len(kls))
	for i, kl := range kls {
		keys[i] = kl.Key
	}
	if req.CommitVersion == 0 {
		server.KvBatchRollback(nil, &kvrpcpb.BatchRollbackRequest{
			Context:      req.Context,
			Keys:         keys,
			StartVersion: req.StartVersion,
		})
	} else {
		server.KvCommit(nil, &kvrpcpb.CommitRequest{
			Context:       req.Context,
			Keys:          keys,
			StartVersion:  req.StartVersion,
			CommitVersion: req.CommitVersion,
		})
	}
	return &kvrpcpb.ResolveLockResponse{}, nil
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}
