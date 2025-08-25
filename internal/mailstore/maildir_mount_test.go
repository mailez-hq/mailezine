//go:build unix

// Mount rehearsal: a maildir exactly as legacy IMAP leaves it (V/N/G uidlist,
// ":" prefixed new/ paths, ",S=,W=" info on flag-less files) must be
// readable by mailezine, and appends must keep the legacy IMAP uidlist shape so
// legacy IMAP can come back later (the postdove → mailezine → rollback story).
package mailstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	maildirpkg "mailezine/internal/store/maildir"
)

func TestMountDovecotMaildir(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mail")
	acctDir := filepath.Join(root, "alice@example.com")
	inbox := filepath.Join(acctDir, "cur", "..") // root doubles as INBOX
	_ = inbox
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(acctDir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// One dovecot-delivered message: new/, flag-less, ",S=,W=" info; uidlist
	// in real legacy IMAP v3 shape.
	msg := "From: sender@remote.test\r\nSubject: seeded\r\n\r\ndovecot body\r\n"
	fname := "1787599608.M325463P18.d386b03ffafa,S=431,W=445"
	if err := os.WriteFile(filepath.Join(acctDir, "new", fname), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
	uidlist := "3 V1787599608 N2 Gd82b6613f89a8c6a12000000ef8c9014\n" +
		"1 :" + fname + "\n"
	if err := os.WriteFile(filepath.Join(acctDir, "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	ms := NewMaildir(root)
	msgs, err := ms.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != 1 {
		t.Fatalf("legacy IMAP message not read with UID 1: %+v", msgs)
	}
	rc, err := ms.OpenMessage(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !strings.Contains(buf.String(), "legacy IMAP body") {
		t.Fatalf("body mismatch: %q", buf.String())
	}

	// mailezine appends into the same layout; the uidlist must keep the
	// legacy IMAP V/N/G shape and ":" new/ prefix.
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{
		Data: []byte("From: a@x.test\r\nSubject: appended\r\n\r\nmailezine body\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := ms.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].UID != 1 || after[1].UID != 2 {
		t.Fatalf("uids after append: %+v", after)
	}
	raw, err := os.ReadFile(filepath.Join(acctDir, "dovecot-uidlist"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "V1787599608") || !strings.Contains(text, "Gd82b6613f89a8c6a12000000ef8c9014") {
		t.Fatalf("uidlist lost legacy IMAP metadata:\n%s", text)
	}
	if !strings.Contains(text, "1 :"+fname) {
		t.Fatalf("legacy IMAP entry not preserved:\n%s", text)
	}
	// The appended message must be resolvable again through the maildir
	// package (legacy IMAP would read it the same way).
	acct, err := maildirpkg.OpenAccount(acctDir)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	got, err := mb.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("maildir package sees %d messages", len(got))
	}
}
