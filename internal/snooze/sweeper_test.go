package snooze

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"mailezine/internal/mailstore"
	"mailezine/internal/notify"
	"mailezine/internal/store"
)

func newStore(t *testing.T) mailstore.MailboxStore {
	t.Helper()
	kv := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	return mailstore.NewKV(kv)
}

func deliver(t *testing.T, ms mailstore.MailboxStore, account, subject string, keywords []string, seen bool) uint32 {
	t.Helper()
	uid, err := ms.Deliver(context.Background(), account, "INBOX", &mailstore.Message{
		From:     "s@remote.test",
		Data:     []byte("From: s@remote.test\r\nSubject: " + subject + "\r\n\r\nbody\r\n"),
		Keywords: keywords,
		Seen:     seen,
	})
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

// recordingNotify captures wake-up receipts without any HTTP.
type recordingNotify struct {
	mu    sync.Mutex
	calls []string
	refs  [][]notify.Delivered
}

func (r *recordingNotify) DeliveredAsync(account string, refs []notify.Delivered) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, account)
	r.refs = append(r.refs, refs)
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func TestUntilFromKeywords(t *testing.T) {
	until, ok := UntilFromKeywords([]string{"\\Seen", "$snoozeduntil-1712345678"})
	if !ok || until.Unix() != 1712345678 {
		t.Fatalf("until = %v ok=%v", until, ok)
	}
	if _, ok := UntilFromKeywords([]string{"$Snoozed", "$snoozeduntil-notanumber"}); ok {
		t.Fatal("garbage until must not parse")
	}
	if _, ok := UntilFromKeywords(nil); ok {
		t.Fatal("no keywords must not parse")
	}
}

// TestSweepOnceWakesDueMessages: a message whose until has passed loses its
// snooze keywords and \Seen (returns unread), a future message is untouched,
// and a plain message is never flagged.
func TestSweepOnceWakesDueMessages(t *testing.T) {
	ms := newStore(t)
	const acct = "alice@example.com"
	// 1712345678 = 2024-04-05, safely in the past.
	due := deliver(t, ms, acct, "due", []string{"$Snoozed", "$SnoozedUntil-1712345678"}, true)
	later := deliver(t, ms, acct, "later", []string{"$Snoozed", "$SnoozedUntil-9999999999"}, true)
	plain := deliver(t, ms, acct, "plain", nil, false)

	rn := &recordingNotify{}
	s := &Sweeper{
		Accounts: func(context.Context) ([]string, error) { return []string{acct}, nil },
		Store:    ms,
		Notify:   rn,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if n := s.SweepOnce(context.Background()); n != 1 {
		t.Fatalf("woken = %d, want 1", n)
	}

	msg, err := ms.MessageByUID(context.Background(), acct, "INBOX", due)
	if err != nil {
		t.Fatal(err)
	}
	for _, kw := range msg.Keywords {
		if kw == "$Snoozed" || len(kw) > 10 && kw[:10] == "$SnoozedUn" {
			t.Fatalf("due message still carries snooze keyword: %v", msg.Keywords)
		}
	}
	if hasFlag(msg.Flags, "\\Seen") {
		t.Fatalf("due message must return unread, flags=%v", msg.Flags)
	}

	future, err := ms.MessageByUID(context.Background(), acct, "INBOX", later)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(future.Flags, "\\Seen") || len(future.Keywords) != 2 {
		t.Fatalf("future snooze was disturbed: flags=%v keywords=%v", future.Flags, future.Keywords)
	}

	plainMsg, err := ms.MessageByUID(context.Background(), acct, "INBOX", plain)
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(plainMsg.Flags, "\\Seen") || len(plainMsg.Keywords) != 0 {
		t.Fatalf("plain message was disturbed: %+v", plainMsg)
	}

	if len(rn.calls) != 1 || rn.calls[0] != acct {
		t.Fatalf("receipt calls = %v, want one for alice", rn.calls)
	}
	if len(rn.refs) != 1 || len(rn.refs[0]) != 1 || rn.refs[0][0].UID != due {
		t.Fatalf("receipt refs = %v, want the due message", rn.refs)
	}
}

// TestSweepOnceMultipleMailboxes: a snoozed message in a non-INBOX mailbox
// wakes with its folder named in the receipt.
func TestSweepOnceMultipleMailboxes(t *testing.T) {
	ms := newStore(t)
	const acct = "bob@example.com"
	if _, err := ms.CreateMailbox(context.Background(), acct, "Archive"); err != nil {
		t.Fatal(err)
	}
	uid, err := ms.Deliver(context.Background(), acct, "Archive", &mailstore.Message{
		From:     "s@remote.test",
		Data:     []byte("From: s@remote.test\r\nSubject: filed\r\n\r\nbody\r\n"),
		Keywords: []string{"$snoozeduntil-1712345678"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rn := &recordingNotify{}
	s := &Sweeper{
		Accounts: func(context.Context) ([]string, error) { return []string{acct}, nil },
		Store:    ms,
		Notify:   rn,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if n := s.SweepOnce(context.Background()); n != 1 {
		t.Fatalf("woken = %d, want 1", n)
	}
	if len(rn.refs) != 1 || rn.refs[0][0].Mailbox != "Archive" || rn.refs[0][0].UID != uid {
		t.Fatalf("receipt = %v", rn.refs)
	}
}

// TestSweepEmptyAndNoAccounts: nil account lister or store is a no-op.
func TestSweepEmptyAndNoAccounts(t *testing.T) {
	s := &Sweeper{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if n := s.SweepOnce(context.Background()); n != 0 {
		t.Fatalf("no-op sweep woke %d", n)
	}
	ms := newStore(t)
	s2 := &Sweeper{
		Accounts: func(context.Context) ([]string, error) { return nil, nil },
		Store:    ms,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if n := s2.SweepOnce(context.Background()); n != 0 {
		t.Fatalf("empty accounts sweep woke %d", n)
	}
}
