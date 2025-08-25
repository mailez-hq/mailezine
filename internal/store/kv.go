// Package store defines the storage contracts of mailezine.
//
// Protocol layers never see these interfaces directly; they operate on the
// logical model through higher-level account APIs (ARCHITECTURE.md §3.1).
package store

import "errors"

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

// KV is a byte-level ordered key-value store. Implementations: RocksDB
// (primary), Pebble (pure-Go fallback), MemoryKV (tests/dev).
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
