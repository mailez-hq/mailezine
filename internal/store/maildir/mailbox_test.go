//go:build unix

package maildir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newTestAccount(t *testing.T) *Account {
	t.Helper()
	acct, err := OpenAccount(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

func TestAppendScanAndFlags(t *testing.T) {
	acct := newTestAccount(t)
	inbox, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	u1, err := inbox.Append(bytes.NewReader([]byte("one")), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u2, err := inbox.Append(bytes.NewReader([]byte("two!")), []Flag{FlagSeen, FlagFlagged}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if u1 != 1 || u2 != 2 {
		t.Fatalf("uids = %d,%d want 1,2", u1, u2)
	}

	msgs, err := inbox.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].UID != 1 || msgs[0].Subdir != "new" || len(msgs[0].Flags) != 0 || msgs[0].Size != 3 {
		t.Fatalf("msg1: %+v", msgs[0])
	}
	if msgs[1].UID != 2 || msgs[1].Subdir != "cur" || !msgs[1].Has(FlagSeen) || !msgs[1].Has(FlagFlagged) {
		t.Fatalf("msg2: %+v", msgs[1])
	}

	rc, err := inbox.Open(u1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "one" {
		t.Fatalf("body = %q", body)
	}

	if n, err := inbox.UnseenCount(); err != nil || n != 1 {
		t.Fatalf("unseen = %d err=%v, want 1", n, err)
	}
	if v, err := inbox.UIDValidity(); err != nil || v == 0 {
		t.Fatalf("uidvalidity = %d err=%v", v, err)
	}
}

func TestUIDNextNoReuseAfterDelete(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	for i := 0; i < 3; i++ {
		if _, err := inbox.Append(bytes.NewReader([]byte("x")), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := inbox.Delete(3); err != nil {
		t.Fatal(err)
	}
	u, err := inbox.Append(bytes.NewReader([]byte("y")), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if u != 4 {
		t.Fatalf("uid reused after delete: got %d, want 4", u)
	}
}

func TestSetFlagsMovesBetweenDirs(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	uid, _ := inbox.Append(bytes.NewReader([]byte("x")), nil, time.Now())
	if err := inbox.SetFlags(uid, []Flag{FlagSeen}); err != nil {
		t.Fatal(err)
	}
	msg, err := inbox.Message(uid)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subdir != "cur" || !msg.Has(FlagSeen) {
		t.Fatalf("after seen: %+v", msg)
	}
	if err := inbox.SetFlags(uid, nil); err != nil {
		t.Fatal(err)
	}
	msg, _ = inbox.Message(uid)
	if msg.Subdir != "new" || len(msg.Flags) != 0 {
		t.Fatalf("after unsee: %+v", msg)
	}
}

func TestKeywordsLifecycle(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	uid, _ := inbox.Append(bytes.NewReader([]byte("x")), nil, time.Now())
	kws := []string{"$Snoozed", "$SnoozedUntil-1712345678"}
	if err := inbox.SetKeywords(uid, kws); err != nil {
		t.Fatal(err)
	}
	got, err := inbox.Keywords(uid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, kws) {
		t.Fatalf("keywords = %v, want %v", got, kws)
	}
	if err := inbox.Delete(uid); err != nil {
		t.Fatal(err)
	}
	got, _ = inbox.Keywords(uid)
	if len(got) != 0 {
		t.Fatalf("keywords survive delete: %v", got)
	}
	if _, err := inbox.Message(uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMove(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	sent, err := acct.OpenMailbox("Sent")
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := inbox.Append(bytes.NewReader([]byte("hello")), []Flag{FlagSeen}, time.Now())
	_ = inbox.SetKeywords(uid, []string{"$Snoozed"})

	if err := inbox.Move(uid, sent); err != nil {
		t.Fatal(err)
	}
	src, _ := inbox.Messages()
	if len(src) != 0 {
		t.Fatalf("source not empty: %+v", src)
	}
	dst, _ := sent.Messages()
	if len(dst) != 1 || dst[0].UID != 1 || string(mustRead(t, sent, dst[0].UID)) != "hello" || !dst[0].Has(FlagSeen) {
		t.Fatalf("destination: %+v", dst)
	}
	kws, _ := sent.Keywords(dst[0].UID)
	if !reflect.DeepEqual(kws, []string{"$Snoozed"}) {
		t.Fatalf("keywords not moved: %v", kws)
	}
}

func TestReindexWhenUIDListMissing(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	if _, err := inbox.Append(bytes.NewReader([]byte("a")), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Append(bytes.NewReader([]byte("b")), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(inbox.dir, "dovecot-uidlist")); err != nil {
		t.Fatal(err)
	}
	msgs, err := inbox.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("after reindex: %d messages, want 2", len(msgs))
	}
	// UIDs reassigned deterministically (sorted filenames).
	if msgs[0].UID != 1 || msgs[1].UID != 2 {
		t.Fatalf("reindexed uids: %d,%d", msgs[0].UID, msgs[1].UID)
	}
	if _, err := os.Stat(filepath.Join(inbox.dir, "dovecot-uidlist")); err != nil {
		t.Fatalf("uidlist not rewritten: %v", err)
	}
}

func TestMailboxesAndQuota(t *testing.T) {
	acct := newTestAccount(t)
	inbox, _ := acct.OpenMailbox("INBOX")
	_, _ = acct.OpenMailbox("Sent")
	_, _ = acct.OpenMailbox("Archive/2026")

	names, err := acct.Mailboxes()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"INBOX", "Archive/2026", "Sent"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("mailboxes = %v, want %v", names, want)
	}

	if q, err := acct.QuotaBytes(); err != nil || q != 0 {
		t.Fatalf("quota before = %d err=%v", q, err)
	}
	if _, err := inbox.Append(bytes.NewReader([]byte("12345")), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Append(bytes.NewReader([]byte("1234567")), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if q, err := acct.QuotaBytes(); err != nil || q != 12 {
		t.Fatalf("quota after = %d err=%v, want 12", q, err)
	}
}

func mustRead(t *testing.T, mb *Mailbox, uid uint32) []byte {
	t.Helper()
	rc, err := mb.Open(uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
