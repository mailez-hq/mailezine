//go:build unix

package mailstore

import (
	"path/filepath"
	"testing"
)

func TestVacationSuiteMaildir(t *testing.T) {
	vacationSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewMaildir(filepath.Join(t.TempDir(), "mail"))
	})
}
