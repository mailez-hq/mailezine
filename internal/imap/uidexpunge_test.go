package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// RFC 4315 §2.1: UID EXPUNGE removes messages that BOTH carry the \Deleted
// flag AND appear in the given set — the set narrows the candidates, it
// never bypasses the flag precondition.
func TestIMAPUIDExpungeRequiresDeleted(t *testing.T) {
	c, _ := startTestServer(t)
	body1 := "From: a@x.test\r\nSubject: one\r\n\r\n1\r\n"
	body2 := "From: b@x.test\r\nSubject: two\r\n\r\n2\r\n"
	u1 := appendMessage(t, c, "INBOX", body1, nil)
	u2 := appendMessage(t, c, "INBOX", body2, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Flag only u2 as \Deleted, then UID EXPUNGE the full set: only u2 may
	// be removed.
	if _, err := c.Store(imap.UIDSetNum(u2), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UIDExpunge(imap.UIDSetNum(u1, u2)).Collect(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(u1, u2), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != u1 {
		t.Fatalf("expected only u1 (no \\Deleted) to survive, got %+v", msgs)
	}
}

// Requesting an undeleted-only set removes nothing.
func TestIMAPUIDExpungeSkipsUndeleted(t *testing.T) {
	c, _ := startTestServer(t)
	u1 := appendMessage(t, c, "INBOX", "From: a@x.test\r\nSubject: keep\r\n\r\n1\r\n", nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UIDExpunge(imap.UIDSetNum(u1)).Collect(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(u1), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("undeleted message must survive UID EXPUNGE, got %+v", msgs)
	}
}
