//go:build unix

package mailstore

import (
	"path/filepath"
	"testing"
)

func TestDefaultMailboxesMaildir(t *testing.T) {
	defaultMailboxesSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewMaildir(filepath.Join(t.TempDir(), "mail"))
	})
}
