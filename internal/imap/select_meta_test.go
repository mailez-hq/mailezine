package imap

import (
	"context"
	"testing"

	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// statusCountingStore promotes the KV's metadata-only read (MailboxMeta) and
// counts the expensive one, so a test can tell which path SELECT took.
type statusCountingStore struct {
	*mailstore.KV
	statusCalls int
}

func (s *statusCountingStore) MailboxStatus(ctx context.Context, account, mailbox string) (mailstore.Mailbox, error) {
	s.statusCalls++
	return s.KV.MailboxStatus(ctx, account, mailbox)
}

// SELECT lists the mailbox to build its snapshot, so it must not also ask
// MailboxStatus to walk the mailbox for counters it will not use.
func TestSelectSkipsMailboxStatusCounters(t *testing.T) {
	cs := &statusCountingStore{KV: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	body := "From: a@example.com\r\nSubject: meta\r\n\r\nbody\r\n"
	appendMessage(t, c, "INBOX", body, nil)
	appendMessage(t, c, "INBOX", body, nil)
	before := cs.statusCalls

	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 2 {
		t.Fatalf("EXISTS = %d, want 2 (counters must come from the listing)", sel.NumMessages)
	}
	if sel.UIDValidity == 0 || sel.UIDNext == 0 {
		t.Fatalf("identity fields missing: %+v", sel)
	}
	if cs.statusCalls != before {
		t.Fatalf("SELECT walked the mailbox counters: %d extra MailboxStatus calls", cs.statusCalls-before)
	}

	// A store without the metadata read still gets a correct SELECT through
	// the MailboxStatus fallback.
	wrapped := &mailboxStoreOnly{MailboxStore: cs.KV}
	c2 := startTestServerWith(t, wrapped)
	if _, err := c2.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

// mailboxStoreOnly hides the optional MailboxMeta method, forcing the
// fallback path.
type mailboxStoreOnly struct {
	mailstore.MailboxStore
}
