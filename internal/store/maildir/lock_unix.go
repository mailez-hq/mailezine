//go:build unix

package maildir

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockMaildir takes the dovecot-uidlist.lock flock. This serializes against
// an external maildir process sharing the directory (rollback scenario); in-process the
// mailbox mutex is the primary guard.
func lockMaildir(dir string) (func(), error) {
	path := filepath.Join(dir, "dovecot-uidlist.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
