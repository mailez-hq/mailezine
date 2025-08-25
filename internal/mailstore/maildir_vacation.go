//go:build unix

package mailstore

import (
	"context"
	"path/filepath"
	"time"

	maildirpkg "mailezine/internal/store/maildir"
)

var _ VacationStateStore = (*Maildir)(nil)

// VacationLastSent returns the last auto-reply time (zero when never).
func (m *Maildir) VacationLastSent(ctx context.Context, account, sender string) (time.Time, error) {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return time.Time{}, err
	}
	return acct.VacationLastSent(sender)
}

// SetVacationLastSent records the last auto-reply time.
func (m *Maildir) SetVacationLastSent(ctx context.Context, account, sender string, t time.Time) error {
	acct, err := maildirpkg.OpenAccount(filepath.Join(m.root, account))
	if err != nil {
		return err
	}
	return acct.SetVacationLastSent(sender, t)
}
