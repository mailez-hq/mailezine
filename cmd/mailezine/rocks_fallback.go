//go:build !rocksdb

package main

import (
	"log/slog"

	"mailezine/internal/store"
)

// openRocks without the rocksdb build tag falls back to Pebble, the pure-Go
// stand-in satisfying the same KV contract (DECISIONS.md D3).
func openRocks(path string, logger *slog.Logger) (store.KV, error) {
	logger.Warn("storage: rocksdb requires -tags rocksdb (cgo); falling back to Pebble", "path", path)
	return store.OpenPebble(path)
}
