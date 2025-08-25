//go:build unix

package pop3

import (
	"path/filepath"
	"testing"

	"mailezine/internal/mailstore"
)

// TestPOP3LifecycleMaildir runs the wire-level POP3 flow against the maildir
// backend (dual-backend parity; executes in CI on Linux).
func TestPOP3LifecycleMaildir(t *testing.T) {
	ms := mailstore.NewMaildir(filepath.Join(t.TempDir(), "mail"))
	seedMailbox(t, ms)
	runPOP3Lifecycle(t, ms)
	ms2 := mailstore.NewMaildir(filepath.Join(t.TempDir(), "mail"))
	seedMailbox(t, ms2)
	runPOP3TopAndUnknown(t, ms2)
}
