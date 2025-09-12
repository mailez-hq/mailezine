package imap

import (
	"context"
	"testing"

	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// listingCountingStore counts mailbox listings so the snapshot memo can be
// observed from outside.
type listingCountingStore struct {
	*mailstore.KV
	lists int
}

func (s *listingCountingStore) ListMessages(ctx context.Context, account, mailbox string) ([]*mailstore.Message, error) {
	s.lists++
	return s.KV.ListMessages(ctx, account, mailbox)
}

// The control plane re-selects the same folder on every request over one
// pooled connection. A second SELECT of an unchanged mailbox must reuse the
// listing, and any change must invalidate it.
func TestSelectReusesUnchangedListing(t *testing.T) {
	cs := &listingCountingStore{KV: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	body := "From: a@example.com\r\nSubject: memo\r\n\r\nbody\r\n"
	appendMessage(t, c, "INBOX", body, nil)

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	afterFirst := cs.lists
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("EXISTS = %d, want 1", sel.NumMessages)
	}
	if cs.lists != afterFirst {
		t.Fatalf("re-selecting an unchanged mailbox re-listed it: %d extra listings", cs.lists-afterFirst)
	}

	// A delivery bumps the mailbox version, so the memo must miss.
	appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if cs.lists == afterFirst {
		t.Fatal("a delivery must invalidate the listing memo")
	}
}

// Sessions mutate the flags of their snapshot entries (a non-PEEK FETCH sets
// \Seen), so every caller must get its own copy.
func TestCloneListingIsDeep(t *testing.T) {
	in := []*mailstore.Message{{
		UID:      1,
		Flags:    []string{"\\Seen"},
		Keywords: []string{"k"},
		To:       []string{"a@example.com"},
	}}
	out := cloneListing(in)
	in[0].UID = 9
	in[0].Flags[0] = "\\Deleted"
	in[0].Keywords[0] = "changed"
	in[0].To[0] = "b@example.com"
	if out[0].UID != 1 || out[0].Flags[0] != "\\Seen" || out[0].Keywords[0] != "k" || out[0].To[0] != "a@example.com" {
		t.Fatalf("clone shares state with the original: %+v", out[0])
	}
	if cloneListing(nil) != nil {
		t.Fatal("nil listing must stay nil, not become an empty slice")
	}
}
