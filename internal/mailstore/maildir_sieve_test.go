//go:build unix

package mailstore

import (
	"path/filepath"
	"testing"
)

func TestSieveStoreSuiteMaildir(t *testing.T) {
	sieveStoreSuite(t, func(t *testing.T) SieveStore {
		t.Helper()
		return NewMaildir(filepath.Join(t.TempDir(), "mail"))
	})
}
