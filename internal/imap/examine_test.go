package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestIMAPExamineReadOnly locks the EXAMINE contract: an examined mailbox
// accepts reads, but STORE is rejected, FETCH body sections do not
// implicitly set \Seen, and APPEND into the examined mailbox is refused —
// while a normal SELECT right after keeps working as before.
func TestIMAPExamineReadOnly(t *testing.T) {
	c, _ := startTestServer(t)
	appendMessage(t, c, "INBOX", "From: a@x.test\r\nSubject: ro\r\n\r\nbody\r\n", nil)

	// EXAMINE.
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 1 {
		t.Fatalf("examine: %+v", sel)
	}

	// FETCH a body section (non-peek): allowed, but must not set \Seen.
	fc := c.Fetch(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}}, &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	msgs, err := fc.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("fetch: %d messages", len(msgs))
	}

	// STORE is rejected with NO.
	if _, err := c.Store(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}},
		&imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect(); err == nil {
		t.Fatal("expected STORE on examined mailbox to fail")
	}

	// APPEND into the examined mailbox is rejected with NO.
	appendCmd := c.Append("INBOX", int64(len("nope")), nil)
	if _, err := appendCmd.Write([]byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := appendCmd.Wait(); err == nil {
		t.Fatal("expected APPEND into examined mailbox to fail")
	}

	// Leave the selection.
	if err := c.Unselect().Wait(); err != nil {
		t.Fatal(err)
	}

	// A regular SELECT sees the message untouched: still unseen (the FETCH
	// above must not have set \Seen) and exactly one message (the rejected
	// APPEND must not have stored anything).
	sel2, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel2.NumMessages != 1 {
		t.Fatalf("messages after examined session: %d, want 1", sel2.NumMessages)
	}
	sc := c.Fetch(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}}, &imap.FetchOptions{Flags: true})
	list, err := sc.Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list[0].Flags {
		if f == imap.FlagSeen {
			t.Fatal("FETCH under EXAMINE set \\Seen")
		}
	}

	// STORE works again under a read-write selection.
	if _, err := c.Store(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}},
		&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
}
