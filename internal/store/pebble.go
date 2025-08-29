// PebbleKV is a disk-backed KV implementation over Pebble, a pure-Go LSM
// (CockroachDB production storage engine). It is the default disk backend in
// builds without cgo and the development-time stand-in for RocksDB; both
// satisfy the same KV contract (ARCHITECTURE.md §3.3).
package store

import (
	"errors"
	"sync"

	"github.com/cockroachdb/pebble"
)

// PebbleKV implements KV over a Pebble database.
type PebbleKV struct {
	db   *pebble.DB
	once sync.Once
}

// OpenPebble opens (or creates) a Pebble database at path.
func OpenPebble(path string) (*PebbleKV, error) {
	db, err := pebble.Open(path, &pebble.Options{})
	if err != nil {
		return nil, err
	}
	return &PebbleKV{db: db}, nil
}

func (p *PebbleKV) Get(key []byte) ([]byte, error) {
	v, closer, err := p.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer closer.Close()
	return append([]byte(nil), v...), nil
}

func (p *PebbleKV) Put(key, value []byte) error {
	return p.db.Set(key, value, pebble.Sync)
}

func (p *PebbleKV) Delete(key []byte) error {
	return p.db.Delete(key, pebble.Sync)
}

func (p *PebbleKV) Scan(prefix []byte, fn func(k, v []byte) error) error {
	opts := &pebble.IterOptions{LowerBound: prefix}
	if len(prefix) > 0 {
		opts.UpperBound = PrefixEnd(prefix)
	}
	iter, err := p.db.NewIter(opts)
	if err != nil {
		return err
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		if err := fn(append([]byte(nil), iter.Key()...), append([]byte(nil), iter.Value()...)); err != nil {
			return err
		}
	}
	return iter.Error()
}

func (p *PebbleKV) Batch(ops []Op) error {
	b := p.db.NewBatch()
	defer b.Close()
	for _, op := range ops {
		if op.Delete {
			if err := b.Delete(op.Key, nil); err != nil {
				return err
			}
			continue
		}
		if err := b.Set(op.Key, op.Value, nil); err != nil {
			return err
		}
	}
	return b.Commit(pebble.Sync)
}

// Close is idempotent: shutdown paths and tests may close the same store more
// than once without panicking.
func (p *PebbleKV) Close() error {
	var err error
	p.once.Do(func() { err = p.db.Close() })
	return err
}

// prefixEnd returns the smallest key greater than every key with the given
// prefix, or nil when the prefix covers the entire key space.
func PrefixEnd(prefix []byte) []byte {
	if len(prefix) == 0 {
		return nil
	}
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil // prefix is all 0xff
}

var _ KV = (*PebbleKV)(nil)
