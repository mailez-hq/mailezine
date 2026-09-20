package imap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// countingStore counts message-blob reads so the body-section memo can be
// observed from outside: a cache hit must not open the message again.
type countingStore struct {
	mailstore.MailboxStore
	// opened is hit from every prefetch goroutine a search or fetch spawns,
	// so the counter has to be atomic even though it only exists for tests.
	opened atomic.Int64
}

func (c *countingStore) OpenMessage(ctx context.Context, account, mailbox string, uid uint32) (io.ReadCloser, error) {
	c.opened.Add(1)
	return c.MailboxStore.OpenMessage(ctx, account, mailbox, uid)
}

// The list path fetches BODY.PEEK[HEADER] for every row on every page load.
// The section bytes are content, which is immutable for a given UID within a
// uidvalidity, so the second page must be served from the memo — same bytes,
// no blob read.
func TestFetchServesBodySectionsFromMemo(t *testing.T) {
	cs := &countingStore{MailboxStore: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	body := "From: a@example.com\r\nSubject: memo\r\nX-Marker: 42\r\n\r\nhello world\r\n"
	appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	listShaped := func() ([]byte, []byte) {
		t.Helper()
		cmd := c.Fetch(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}}, &imap.FetchOptions{
			Envelope:      true,
			UID:           true,
			BodyStructure: &imap.FetchItemBodyStructure{},
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierHeader, Peek: true},
				{Specifier: imap.PartSpecifierText, Partial: &imap.SectionPartial{Offset: 0, Size: 64}, Peek: true},
			},
		})
		msgs, err := cmd.Collect()
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || len(msgs[0].BodySection) != 2 {
			t.Fatalf("unexpected fetch result: %+v", msgs)
		}
		return msgs[0].BodySection[0].Bytes, msgs[0].BodySection[1].Bytes
	}

	header1, preview1 := listShaped()
	afterFirst := int(cs.opened.Load())
	if afterFirst == 0 {
		t.Fatal("first fetch must read the message blob")
	}
	header2, preview2 := listShaped()
	if int(cs.opened.Load()) != afterFirst {
		t.Fatalf("second fetch reopened the blob: %d -> %d opens", afterFirst, int(cs.opened.Load()))
	}
	if string(header1) != string(header2) {
		t.Fatalf("header bytes differ across fetches:\n%q\n%q", header1, header2)
	}
	if string(preview1) != string(preview2) {
		t.Fatalf("partial section bytes differ across fetches:\n%q\n%q", preview1, preview2)
	}
	if want := "X-Marker: 42\r\n"; !strings.Contains(string(header2), want) {
		t.Fatalf("header section missing %q: %q", want, header2)
	}
}

// TestFetchBatchesPrefetchUnderBudget covers a FETCH whose messages overflow
// one prefetch budget: every message still returns in order, byte-identical,
// and each blob is opened once.
func TestFetchBatchesPrefetchUnderBudget(t *testing.T) {
	mem := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(mem)
	cs := &countingStore{MailboxStore: ms}
	ctx := context.Background()

	chunk := strings.Repeat("x", prefetchBudget/2+1024) // two per batch at most
	bodies := make([][]byte, 3)
	for i := range bodies {
		bodies[i] = []byte(fmt.Sprintf("From: a@example.com\r\nSubject: big-%d\r\n\r\n%s", i, chunk))
		if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{From: "a@example.com", Data: bodies[i]}); err != nil {
			t.Fatal(err)
		}
	}

	c := startTestServerWith(t, cs)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(1, 2, 3), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(bodies) {
		t.Fatalf("batched fetch returned %d messages, want %d", len(msgs), len(bodies))
	}
	for i, m := range msgs {
		if uint32(m.UID) != uint32(i+1) {
			t.Fatalf("message %d out of order: UID %d", i, m.UID)
		}
		if len(m.BodySection) != 1 || !bytes.Equal(m.BodySection[0].Bytes, bodies[i]) {
			t.Fatalf("message %d body corrupted across batches", i)
		}
	}
	if got := cs.opened.Load(); got != int64(len(bodies)) {
		t.Fatalf("prefetch opened %d blobs, want %d (one per message)", got, len(bodies))
	}
}
