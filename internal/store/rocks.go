//go:build rocksdb

// RocksKV implements KV over RocksDB through grocksdb (cgo).
//
// This file compiles only with the rocksdb build tag AND a RocksDB C++
// library (librocksdb) present. CI builds and verifies it; local development
// uses PebbleKV, which satisfies the same KV contract (D3 in DECISIONS.md).
package store

import (
	"bytes"

	"github.com/linxGnu/grocksdb"
)

// RocksKV implements KV over a RocksDB database.
type RocksKV struct {
	db *grocksdb.DB
	ro *grocksdb.ReadOptions
	wo *grocksdb.WriteOptions
}

// OpenRocks opens (or creates) a RocksDB database at path.
func OpenRocks(path string) (*RocksKV, error) {
	opts := grocksdb.NewDefaultOptions()
	opts.SetCreateIfMissing(true)
	db, err := grocksdb.OpenDb(opts, path)
	if err != nil {
		return nil, err
	}
	return &RocksKV{
		db: db,
		ro: grocksdb.NewDefaultReadOptions(),
		wo: grocksdb.NewDefaultWriteOptions(),
	}, nil
}

func (r *RocksKV) Get(key []byte) ([]byte, error) {
	slice, err := r.db.Get(r.ro, key)
	if err != nil {
		return nil, err
	}
	defer slice.Free()
	if !slice.Exists() {
		return nil, ErrNotFound
	}
	return append([]byte(nil), slice.Data()...), nil
}

func (r *RocksKV) Put(key, value []byte) error {
	return r.db.Put(r.wo, key, value)
}

func (r *RocksKV) Delete(key []byte) error {
	return r.db.Delete(r.wo, key)
}

func (r *RocksKV) Scan(prefix []byte, fn func(k, v []byte) error) error {
	iter := r.db.NewIterator(r.ro)
	defer iter.Close()
	for iter.Seek(prefix); iter.Valid(); iter.Next() {
		k := iter.Key()
		if !bytes.HasPrefix(k.Data(), prefix) {
			k.Free()
			break
		}
		v := iter.Value()
		err := fn(append([]byte(nil), k.Data()...), append([]byte(nil), v.Data()...))
		k.Free()
		v.Free()
		if err != nil {
			return err
		}
	}
	return iter.Err()
}

func (r *RocksKV) Batch(ops []Op) error {
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	for _, op := range ops {
		if op.Delete {
			wb.Delete(op.Key)
			continue
		}
		wb.Put(op.Key, op.Value)
	}
	return r.db.Write(r.wo, wb)
}

func (r *RocksKV) Close() error {
	r.ro.Destroy()
	r.wo.Destroy()
	r.db.Close()
	return nil
}

var _ KV = (*RocksKV)(nil)
