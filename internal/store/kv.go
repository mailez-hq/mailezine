// Package store defines the storage contracts of mailezine.
//
// Protocol layers never see these interfaces directly; they operate on the
// logical model through higher-level account APIs (ARCHITECTURE.md §3.1).
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned by KV and Blob lookups for missing keys/blobs.
var ErrNotFound = errors.New("store: not found")

// ErrExists is returned when creating an object that already exists.
var ErrExists = errors.New("store: already exists")

// Op is a single mutation inside a Batch.
type Op struct {
	Key   []byte
	Value []byte
	// Delete removes Key. When Delete is true, Value must be nil.
	Delete bool
}

// KV is a byte-level ordered key-value store. Implementations: Pebble
// (primary), TiDB (distributed), MemoryKV (tests/dev).
type KV interface {
	Get(key []byte) ([]byte, error)
	Put(key, value []byte) error
	Delete(key []byte) error
	// Scan visits every key with the given prefix in ascending key order.
	// Returning an error from fn aborts the scan and is propagated.
	Scan(prefix []byte, fn func(k, v []byte) error) error
	// Batch applies ops atomically: either all take effect or none do.
	Batch(ops []Op) error
	Close() error
}

// TxnOps is the read-write handle handed to a WithTxn closure. Writes are
// staged by the backend and become visible to later reads inside the same
// transaction (read-your-writes); nothing hits the engine until commit.
type TxnOps interface {
	Get(key []byte) ([]byte, error)
	Put(key, val []byte)
	Delete(key []byte)
	// Append stages pre-built Batch ops inside the transaction.
	Append(ops ...Op)
	// Scan visits every key with the given prefix in ascending key order,
	// merged with the staged writes (read-your-writes): staged deletes hide
	// base keys, staged puts surface their buffered values. Returning an
	// error from fn aborts the scan and is propagated. Scans are part of
	// the transaction read set: on backends with conflict detection they
	// participate in commit-time validation.
	Scan(prefix []byte, fn func(k, v []byte) error) error
}

// TxnKV extends KV with optimistic transactions. WithTxn runs fn against a
// snapshot and retries the WHOLE closure from scratch when the backend
// detects a conflicting commit, so fn must be idempotent and must derive all
// of its writes from reads performed inside the closure (no values captured
// before WithTxn). On success the staged mutations commit atomically.
type TxnKV interface {
	KV
	WithTxn(ctx context.Context, fn func(t TxnOps) error) error
}
