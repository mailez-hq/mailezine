//go:build unix

package mailstore

import (
	"path/filepath"
	"testing"
)

func TestACLSuiteMaildir(t *testing.T) {
	aclSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewMaildir(filepath.Join(t.TempDir(), "mail"))
	})
}
