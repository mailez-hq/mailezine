//go:build unix

package imap

import (
	"path/filepath"
	"testing"

	"mailezine/internal/mailstore"
)

// TestIMAPLifecycleMaildir runs the same wire-level lifecycle against the
// maildir backend (dual-backend parity; executes in CI on Linux).
func TestIMAPLifecycleMaildir(t *testing.T) {
	ms := mailstore.NewMaildir(filepath.Join(t.TempDir(), "mail"))
	c := startTestServerWith(t, ms)
	runIMAPLifecycle(t, c, ms)
}
