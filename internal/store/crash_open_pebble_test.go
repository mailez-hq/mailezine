//go:build !rocksdb

package store

// crashOpen opens the crash-test KV backend: Pebble without the rocksdb
// build tag (the pure-Go default).
func crashOpen(path string) (KV, error) {
	return OpenPebble(path)
}
