// Persistent vacation throttle state: RFC 5230 :days must survive engine
// restarts (the in-memory map alone could send duplicate auto-replies after
// a restart). The auto-reply state lives under the store's meta space.
package mailstore

import (
	"context"
	"encoding/binary"
	"time"

	"mailezine/internal/store"
)

var _ VacationStateStore = (*KV)(nil)

// VacationLastSent returns the last auto-reply time (zero when never).
func (k *KV) VacationLastSent(ctx context.Context, account, sender string) (time.Time, error) {
	raw, err := k.s.GetRaw(ctx, store.MetaVacationKey(account, sender))
	if err != nil {
		if err == store.ErrNotFound {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if len(raw) != 8 {
		return time.Time{}, nil
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(raw))), nil
}

// SetVacationLastSent records the last auto-reply time.
func (k *KV) SetVacationLastSent(ctx context.Context, account, sender string, t time.Time) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(t.UnixNano()))
	return k.s.PutRaw(ctx, store.MetaVacationKey(account, sender), buf[:])
}
