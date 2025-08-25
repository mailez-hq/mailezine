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
