package mvcc

import (
	"bytes"

	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
)

// Scanner is used for reading multiple sequential key/value pairs from the storage layer. It is aware of the implementation
// of the storage layer and returns results suitable for users.
// Invariant: either the scanner is finished and cannot be used, or it is ready to return a value immediately.
type Scanner struct {
	// Your Data Here (4C).
	txn  *MvccTxn
	key  []byte
	iter engine_util.DBIterator
}

// NewScanner creates a new scanner ready to read from the snapshot in txn.
func NewScanner(startKey []byte, txn *MvccTxn) *Scanner {
	// Your Code Here (4C).
	scan := &Scanner{
		txn:  txn,
		key:  startKey,
		iter: txn.Reader.IterCF(engine_util.CfWrite),
	}
	return scan
}

func (scan *Scanner) Close() {
	// Your Code Here (4C).
	scan.iter.Close()
}

// Next returns the next key/value pair from the scanner. If the scanner is exhausted, then it will return `nil, nil, nil`.
func (scan *Scanner) Next() ([]byte, []byte, error) {
	// Your Code Here (4C).
	k := scan.key
	if k == nil {
		return nil, nil, nil
	}
	scan.iter.Seek(EncodeKey(k, scan.txn.StartTS))
	if !scan.iter.Valid() {
		scan.key = nil
		return nil, nil, nil
	}
	item := scan.iter.Item()
	iTs, iKey := decodeTimestamp(item.Key()), DecodeUserKey(item.Key())
	for scan.iter.Valid() && iTs > scan.txn.StartTS {
		scan.iter.Seek(EncodeKey(iKey, scan.txn.StartTS))
		item = scan.iter.Item()
		iTs, iKey = decodeTimestamp(item.Key()), DecodeUserKey(item.Key())
	}
	if !scan.iter.Valid() {
		scan.key = nil
		return nil, nil, nil
	}
	for ; scan.iter.Valid(); scan.iter.Next() {
		nextUserKey := DecodeUserKey(scan.iter.Item().Key())
		if !bytes.Equal(nextUserKey, iKey) {
			scan.key = nextUserKey
			break
		}
	}
	if !scan.iter.Valid() {
		scan.key = nil
	}
	v, err := scan.txn.GetValue(iKey)
	return iKey, v, err
}
