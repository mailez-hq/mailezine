package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// STATUS HIGHESTMODSEQ is the push path's change signal: it must report the
// mailbox version, move when the mailbox changes, and cost no message walk
// (the same store that counts MailboxStatus calls proves the last point).
func TestStatusHighestModSeqIsVersionedAndCheap(t *testing.T) {
	cs := &statusCountingStore{KV: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	body := "From: a@example.com\r\nSubject: v\r\n\r\nbody\r\n"
	appendMessage(t, c, "INBOX", body, nil)
	before := cs.statusCalls

	first, err := c.Status("INBOX", &imap.StatusOptions{HighestModSeq: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if first.HighestModSeq == 0 {
		t.Fatalf("HIGHESTMODSEQ missing from the STATUS response: %+v", first)
	}
	if cs.statusCalls != before {
		t.Fatalf("a version-only STATUS walked the mailbox: %d extra MailboxStatus calls", cs.statusCalls-before)
	}

	// Any change bumps the version; a flag-only change counts (that is what
	// the watcher must see to keep other clients in sync).
	appendMessage(t, c, "INBOX", body, nil)
	second, err := c.Status("INBOX", &imap.StatusOptions{HighestModSeq: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if second.HighestModSeq <= first.HighestModSeq {
		t.Fatalf("version did not advance on delivery: %d -> %d", first.HighestModSeq, second.HighestModSeq)
	}

	// Counters still come from the real walk when asked for.
	counted, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, HighestModSeq: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if counted.NumMessages == nil || *counted.NumMessages != 2 {
		t.Fatalf("MESSAGES = %v, want 2", counted.NumMessages)
	}
	if counted.HighestModSeq != second.HighestModSeq {
		t.Fatalf("HIGHESTMODSEQ differs between the two STATUS shapes: %d vs %d", counted.HighestModSeq, second.HighestModSeq)
	}
}
