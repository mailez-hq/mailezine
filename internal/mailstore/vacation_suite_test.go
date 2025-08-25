// Shared vacation throttle state suite, executed against every backend.
package mailstore

import (
	"context"
	"testing"
	"time"
)

func vacationSuite(t *testing.T, newStore func(*testing.T) MailboxStore) {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	vs, ok := s.(VacationStateStore)
	if !ok {
		t.Fatal("store does not implement VacationStateStore")
	}
	last, err := vs.VacationLastSent(ctx, "alice@example.com", "friend@remote.test")
	if err != nil {
		t.Fatal(err)
	}
	if !last.IsZero() {
		t.Fatalf("initial last-sent = %v, want zero", last)
	}
	now := time.Now().Add(-time.Hour)
	if err := vs.SetVacationLastSent(ctx, "alice@example.com", "friend@remote.test", now); err != nil {
		t.Fatal(err)
	}
	got, err := vs.VacationLastSent(ctx, "alice@example.com", "friend@remote.test")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now) {
		t.Fatalf("last-sent = %v, want %v", got, now)
	}
	// Another sender stays untouched.
	other, err := vs.VacationLastSent(ctx, "alice@example.com", "other@remote.test")
	if err != nil {
		t.Fatal(err)
	}
	if !other.IsZero() {
		t.Fatalf("other sender last-sent = %v, want zero", other)
	}
}
