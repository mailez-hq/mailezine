package imap

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// searchWithoutCachedMetadata hides the header blocks the store caches, so a
// search against it has to read every candidate blob — the behaviour the
// cached path replaces.
type searchWithoutCachedMetadata struct{ mailstore.MailboxStore }

func (s *searchWithoutCachedMetadata) ListMessages(ctx context.Context, account, mailbox string) ([]*mailstore.Message, error) {
	msgs, err := s.MailboxStore.ListMessages(ctx, account, mailbox)
	for _, m := range msgs {
		m.Head = nil
	}
	return msgs, err
}

// Header conditions (SUBJECT/FROM/HEADER and the sent-date range) are the
// common webmail searches. They must be answered from the header block the
// store caches at delivery — no candidate blob — and still match exactly what
// reading the messages would.
func TestSearchHeaderReadsNoBlobs(t *testing.T) {
	kv := mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	cs := &countingStore{MailboxStore: kv}
	cached := startTestServerWith(t, cs)
	walked := startTestServerWith(t, &searchWithoutCachedMetadata{MailboxStore: kv})

	bodies := []string{
		"From: alice@example.com\r\nSubject: quarterly report\r\nDate: Wed, 10 Sep 2026 09:00:00 +0800\r\n\r\ncontains the word zebra\r\n",
		"From: bob@example.com\r\nSubject: lunch?\r\nX-Tag: zebra\r\nDate: Thu, 11 Sep 2026 09:00:00 +0800\r\n\r\nno special word\r\n",
		"From: carol@example.com\r\nSubject: Re: quarterly report\r\nDate: Fri, 12 Sep 2026 09:00:00 +0800\r\n\r\nnothing\r\n",
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

	cases := []struct {
		name     string
		criteria *imap.SearchCriteria
		want     []uint32
	}{
		{"subject substring", &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "quarterly"}}}, []uint32{1, 3}},
		{"custom header", &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Tag", Value: "zebra"}}}, []uint32{2}},
		{"from", &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "bob@"}}}, []uint32{2}},
	}
	for _, tc := range cases {
		before := cs.opened
		data, err := cached.UIDSearch(tc.criteria, nil).Wait()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cs.opened != before {
			t.Fatalf("%s: header search opened %d blob(s), want 0", tc.name, cs.opened-before)
		}
		if !sameUIDs(allUIDs(data), tc.want) {
			t.Fatalf("%s: uids = %v, want %v", tc.name, allUIDs(data), tc.want)
		}
		walkedData, err := walked.UIDSearch(tc.criteria, nil).Wait()
		if err != nil {
			t.Fatalf("%s (walk): %v", tc.name, err)
		}
		if !sameUIDs(allUIDs(walkedData), tc.want) {
			t.Fatalf("%s: walked uids = %v, want %v", tc.name, allUIDs(walkedData), tc.want)
		}
	}

	// TEXT still needs the message body, and only matches the body.
	before := cs.opened
	data, err := cached.UIDSearch(&imap.SearchCriteria{Text: []string{"zebra"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if cs.opened == before {
		t.Fatal("a TEXT search must read the message bodies")
	}
	if !sameUIDs(allUIDs(data), []uint32{1, 2}) {
		t.Fatalf("TEXT search uids = %v, want [1 2]", allUIDs(data))
	}
}

// allUIDs flattens a UID SEARCH result into ascending UIDs (the server sends
// spans).
func allUIDs(data *imap.SearchData) []uint32 {
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil
	}
	var out []uint32
	for _, r := range set {
		for u := r.Start; u <= r.Stop; u++ {
			out = append(out, uint32(u))
		}
	}
	return out
}

func sameUIDs(got []uint32, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
