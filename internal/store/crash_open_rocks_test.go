//go:build rocksdb

package store

import "testing"

// crashOpen opens the crash-test KV backend: RocksDB under the rocksdb tag.
func crashOpen(path string) (KV, error) {
	return OpenRocks(path)
}

// TestCrashConsistencyRocks runs the same kill-and-recover matrix on
// RocksDB (executed by the CI rocksdb job).
func TestCrashConsistencyRocks(t *testing.T) {
	// The parent/child logic is shared with TestCrashConsistency; RocksDB
	// exercises the same invariants through WAL recovery.
	runCrashConsistency(t)
}
