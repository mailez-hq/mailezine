package imap

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// sortWithoutCachedMetadata hides the header blocks the store caches, so a
// sort against it has to open every candidate blob — the behaviour the cached
// path replaces.
type sortWithoutCachedMetadata struct{ mailstore.MailboxStore }

func (s *sortWithoutCachedMetadata) ListMessages(ctx context.Context, account, mailbox string) ([]*mailstore.Message, error) {
	msgs, err := s.MailboxStore.ListMessages(ctx, account, mailbox)
	for _, m := range msgs {
		m.Head = nil
	}
	return msgs, err
}

// Header sort keys must be answered from the header block cached at delivery.
// Opening one blob per message is what made a 600-message UID SORT take tens
// of seconds, and the listing already carries the block.
func TestSortReadsNoBlobs(t *testing.T) {
	kv := mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	cs := &countingStore{MailboxStore: kv}
	cached := startTestServerWith(t, cs)
	walked := startTestServerWith(t, &sortWithoutCachedMetadata{MailboxStore: kv})

	bodies := []string{
		"From: zulu@example.com\r\nSubject: alpha\r\n\r\n1\r\n",
		"From: alpha@example.com\r\nSubject: charlie\r\n\r\n2\r\n",
		"From: mike@example.com\r\nSubject: bravo\r\n\r\n3\r\n",
	}
	for _, b := range bodies {
		appendMessage(t, cached, "INBOX", b, nil)
	}
	if _, err := cached.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := walked.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	sortBy := func(c *imapclient.Client, key imapclient.SortCriterion) []uint32 {
		t.Helper()
		data, err := c.Sort(&imapclient.SortOptions{
			SortCriteria:   []imapclient.SortCriterion{key},
			SearchCriteria: &imap.SearchCriteria{},
		}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	cases := []struct {
		name string
		key  imapclient.SortCriterion
		want []uint32
	}{
		{"from", imapclient.SortCriterion{Key: imapclient.SortKeyFrom}, []uint32{2, 3, 1}},
		{"subject", imapclient.SortCriterion{Key: imapclient.SortKeySubject}, []uint32{1, 3, 2}},
		{"reverse subject", imapclient.SortCriterion{Key: imapclient.SortKeySubject, Reverse: true}, []uint32{2, 3, 1}},
	}
	for _, tc := range cases {
		before := cs.opened
		got := sortBy(cached, tc.key)
		if cs.opened != before {
			t.Fatalf("%s: sort opened %d blob(s), want 0", tc.name, cs.opened-before)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: seqs = %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: seqs = %v, want %v", tc.name, got, tc.want)
			}
		}
		// A deployment whose messages predate the header cache still sorts
		// correctly, just slower — the blob fallback has to stay.
		walkedGot := sortBy(walked, tc.key)
		for i := range walkedGot {
			if walkedGot[i] != tc.want[i] {
				t.Fatalf("%s (walk): seqs = %v, want %v", tc.name, walkedGot, tc.want)
			}
		}
	}
}
