package mailstore

import (
	"testing"

	"mailezine/internal/store"
)

func TestMailboxSuiteKV(t *testing.T) {
	mailboxSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
		return NewKV(s)
	})
}

// TestReindexBackfill wipes the secondary indexes (simulating data written
// before they existed), runs the backfill and verifies every read path
// returns to working order without writing new mail.
func TestReindexBackfill(t *testing.T) {
	ctx := t.Context()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := NewKV(s)
	const acct = "alice@example.com"
	for _, mb := range []string{"INBOX", "Arch"} {
		if _, err := ms.CreateMailbox(ctx, acct, mb); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := ms.Deliver(ctx, acct, "INBOX", &Message{Data: []byte("Subject: m\r\n\r\nbody")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.RenameMailbox(ctx, acct, "Arch", "Archive"); err != nil {
		t.Fatal(err)
	}

	// Drop every index entry of the account.
	prefix := []byte{store.SpaceIndex}
	var keys [][]byte
	if err := s.ScanRaw(ctx, prefix, func(key, _ []byte) error {
		keys = append(keys, append([]byte(nil), key...))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("no index entries found to wipe")
	}
	for _, key := range keys {
		if err := s.DeleteRaw(ctx, key); err != nil {
			t.Fatal(err)
		}
	}

	n, err := ms.Reindex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < len(keys) {
		t.Fatalf("reindex restored %d entries, want >= %d", n, len(keys))
	}

	// Read paths work again via the index; no new deliveries needed.
	msgs, err := ms.ListMessages(ctx, acct, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("ListMessages after reindex: %d messages, want 3", len(msgs))
	}
	e, err := ms.EmailByUID(ctx, acct, "INBOX", msgs[2].UID)
	if err != nil || e == nil {
		t.Fatalf("EmailByUID after reindex: %v", err)
	}
}

func TestMailboxSuiteKVQuotaAfterExpunge(t *testing.T) {
	ctx := t.Context()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := NewKV(s)
	body := []byte("Subject: x\r\n\r\n0123456789\r\n")
	u1, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: body})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ms.QuotaUsedBytes(ctx, "alice@example.com")
	if before != int64(len(body)) {
		t.Fatalf("quota before: %d want %d", before, len(body))
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", u1, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(ctx, "alice@example.com", "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	after, _ := ms.QuotaUsedBytes(ctx, "alice@example.com")
	if after != 0 {
		t.Fatalf("quota after expunge: %d want 0", after)
	}
}
