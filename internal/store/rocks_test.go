//go:build rocksdb

package store

import "testing"

// TestStoreRocks runs the full KV contract suite against RocksDB. It only
// compiles with -tags rocksdb and needs librocksdb (CI job).
func TestStoreRocks(t *testing.T) {
	runStoreSuite(t, func(t *testing.T) *Store {
		t.Helper()
		kv, err := OpenRocks(t.TempDir())
		if err != nil {
			t.Fatalf("open rocks: %v", err)
		}
		t.Cleanup(func() { _ = kv.Close() })
		return New(kv, NewMemoryBlob())
	})
}
