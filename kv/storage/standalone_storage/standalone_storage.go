package standalone_storage

import (
	"github.com/Connor1996/badger"
	"github.com/pingcap-incubator/tinykv/kv/config"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"path/filepath"
)

// StandAloneStorage is an implementation of `Storage` for a single-node TinyKV instance. It does not
// communicate with other nodes and all data is stored locally.
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

func (s *StandAloneStorage) Start() error {
	// Your Code Here (1).
	return nil
}

func (s *StandAloneStorage) Stop() error {
	// Your Code Here (1).
	if err := s.engines.Kv.Close(); err != nil {
		return err
	}
	return nil
}

func (s *StandAloneStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	// Your Code Here (1).
	txn := s.engines.Kv.NewTransaction(false)
	return &StandaloneReader{txn}, nil
}

func (s *StandAloneStorage) Write(ctx *kvrpcpb.Context, batch []storage.Modify) error {
	// Your Code Here (1).
	for _, m := range batch {
		engine_util.PutCF(s.engines.Kv, m.Cf(), m.Key(), m.Value())
	}
	return nil
}

type StandaloneReader struct {
	txn *badger.Txn
}

func (r *StandaloneReader) GetCF(cf string, key []byte) ([]byte, error) {
	return engine_util.GetCFFromTxn(r.txn, cf, key)
}

func (r *StandaloneReader) IterCF(cf string) engine_util.DBIterator {
	return engine_util.NewCFIterator(cf, r.txn)
}

func (r *StandaloneReader) Close() {
	r.txn.Discard()
}
