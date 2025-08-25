//go:build rocksdb

package app

import (
	"log/slog"

	"mailezine/internal/store"
)

func openRocks(path string, _ *slog.Logger) (store.KV, error) {
	return store.OpenRocks(path)
}
