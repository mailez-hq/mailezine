// Shared SieveStore test suite (dual-backend parity): KV on all platforms,
// maildir sidecar on POSIX.
package mailstore

import (
	"context"
	"errors"
	"testing"

	"mailezine/internal/store"
)

func sieveStoreSuite(t *testing.T, newStore func(t *testing.T) SieveStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("put list get activate delete", func(t *testing.T) {
		s := newStore(t)
		if err := s.PutSieveScript(ctx, "alice@example.com", "rules", `if true { keep; }`, true); err != nil {
			t.Fatal(err)
		}
		content, err := s.GetSieveScript(ctx, "alice@example.com", "rules")
		if err != nil || content != `if true { keep; }` {
			t.Fatalf("get: %q err=%v", content, err)
		}
		scripts, err := s.ListSieveScripts(ctx, "alice@example.com")
		if err != nil || len(scripts) != 1 || scripts[0].Name != "rules" || !scripts[0].Active {
			t.Fatalf("list: %+v err=%v", scripts, err)
		}
		// A second script starts inactive; activating it flips the bit.
		if err := s.PutSieveScript(ctx, "alice@example.com", "vacation", `vacation "away";`, false); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSieveActive(ctx, "alice@example.com", "vacation"); err != nil {
			t.Fatal(err)
		}
		scripts, _ = s.ListSieveScripts(ctx, "alice@example.com")
		active := map[string]bool{}
		for _, sc := range scripts {
			active[sc.Name] = sc.Active
		}
		if !active["vacation"] || active["rules"] {
			t.Fatalf("active flags: %v", active)
		}
		// Deactivate everything (SETACTIVE "").
		if err := s.SetSieveActive(ctx, "alice@example.com", ""); err != nil {
			t.Fatal(err)
		}
		scripts, _ = s.ListSieveScripts(ctx, "alice@example.com")
		for _, sc := range scripts {
			if sc.Active {
				t.Fatalf("still active: %+v", scripts)
			}
		}
		if err := s.DeleteSieveScript(ctx, "alice@example.com", "rules"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSieveScript(ctx, "alice@example.com", "rules"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected not found: %v", err)
		}
	})
}
