// RFC 7162 CONDSTORE integration: SELECT CONDSTORE returns HIGHESTMODSEQ,
// FETCH MODSEQ returns per-message sequences, and STORE UNCHANGEDSINCE
// protects concurrently modified messages.
package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPCondstore(t *testing.T) {
	c, _ := startTestServer(t)
	if !c.Caps().Has(imap.CapCondStore) {
		t.Fatal("CONDSTORE capability not advertised")
	}
	body := "From: a@x.test\r\nSubject: condstore\r\n\r\nbody\r\n"
	u1 := appendMessage(t, c, "INBOX", body, nil)
	u2 := appendMessage(t, c, "INBOX", "From: b@x.test\r\nSubject: second\r\n\r\nbody2\r\n", nil)

	sel, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.HighestModSeq < 2 {
		t.Fatalf("highestmodseq = %d, want >= 2", sel.HighestModSeq)
	}

	// FETCH MODSEQ returns per-message sequences (u1=1, u2=2).
	msgs, err := c.Fetch(imap.UIDSetNum(u1, u2), &imap.FetchOptions{UID: true, ModSeq: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var modseqOf = map[imap.UID]uint64{}
	for _, m := range msgs {
		modseqOf[m.UID] = m.ModSeq
	}
	if modseqOf[u1] != 1 || modseqOf[u2] != 2 {
		t.Fatalf("message modseqs = %v, want uid%v=1 uid%v=2", modseqOf, u1, u2)
	}

	// STORE UNCHANGEDSINCE=1: u1 (modseq 1, unchanged since 1) is eligible;
	// u2 (modseq 2) changed after 1 and must be skipped.
	if _, err := c.Store(imap.UIDSetNum(u1, u2), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, &imap.StoreOptions{UnchangedSince: 1}).Collect(); err != nil {
		t.Fatal(err)
	}
	fetched, err := c.Fetch(imap.UIDSetNum(u1, u2), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range fetched {
		if m.UID == u1 && !hasIMAPFlag(m.Flags, "\\Seen") {
			t.Fatal("u1 was not modified (eligible for UNCHANGEDSINCE=1)")
		}
		if m.UID == u2 && hasIMAPFlag(m.Flags, "\\Seen") {
			t.Fatal("u2 was modified despite UNCHANGEDSINCE=1")
		}
	}
}

func hasIMAPFlag(flags []imap.Flag, want string) bool {
	for _, f := range flags {
		if string(f) == want {
			return true
		}
	}
	return false
}
