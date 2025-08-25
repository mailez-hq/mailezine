//go:build unix

package mailstore

import (
	"context"
	"path/filepath"
	"testing"

	maildirpkg "mailezine/internal/store/maildir"
)

func TestMaildirDeliverAndRead(t *testing.T) {
	root := t.TempDir()
	ms := NewMaildir(root)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	u1, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body), Seen: true})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if u1 != 1 || u2 != 2 {
		t.Fatalf("uids = %d,%d want 1,2", u1, u2)
	}

	acct, err := maildirpkg.OpenAccount(filepath.Join(root, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mb.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].UID != 1 || !msgs[0].Has(maildirpkg.FlagSeen) || msgs[1].UID != 2 {
		t.Fatalf("maildir messages: %+v", msgs)
	}
	q, err := ms.QuotaUsedBytes(ctx, "alice@example.com")
	if err != nil || q != 2*int64(len(body)) {
		t.Fatalf("quota = %d err=%v, want %d", q, err, 2*int64(len(body)))
	}
}
