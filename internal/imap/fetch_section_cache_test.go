package imap

import (
	"context"
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
