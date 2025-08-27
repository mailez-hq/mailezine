//go:build !unix

package main

import (
	"fmt"
	"os"
)

// runMigrate on non-POSIX platforms reports the maildir constraint: the
// source layout relies on ':' in filenames (DECISIONS.md D7).
func runMigrate([]string) int {
	fmt.Fprintln(os.Stderr, "migrate: maildir source is POSIX-only; run on Linux (or use maildir→pebble/tidb on a Linux host)")
	return 2
}
