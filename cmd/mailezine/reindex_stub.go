//go:build !unix

package main

import (
	"fmt"
	"os"
)

// runReindex on non-POSIX platforms is unavailable (the maildir backend is
// POSIX-only and pebble/rocksdb reindex is typically run on the server).
func runReindex([]string) int {
	fmt.Fprintln(os.Stderr, "reindex: run on Linux (POSIX storage paths)")
	return 2
}
