// Shared mailbox test suite, executed against every MailboxStore backend
// (dual-backend parity, ARCHITECTURE.md §11): KV+blob on all platforms,
// maildir on POSIX.
package mailstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"mailezine/internal/store"
)

func mailboxSuite(t *testing.T, newStore func(t *testing.T) MailboxStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("create and list mailboxes", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Sent"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Work/Project"); err != nil {
			t.Fatal(err)
		}
		boxes, err := s.ListMailboxes(ctx, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, b := range boxes {
			names[b.Name] = true
			if b.UIDValidity == 0 {
				t.Fatalf("mailbox %s has no UIDVALIDITY", b.Name)
			}
		}
		for _, want := range []string{"INBOX", "Sent", "Work/Project"} {
			if !names[want] {
				t.Fatalf("missing mailbox %q in %v", want, boxes)
			}
		}
	})

	t.Run("deliver, list, status", func(t *testing.T) {
		s := newStore(t)
		u1, err := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{
			From: "a@x.test", Data: []byte("Subject: one\r\n\r\n1\r\n"), Seen: false,
		})
		if err != nil {
			t.Fatal(err)
		}
		u2, err := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{
			From: "b@x.test", Data: []byte("Subject: two\r\n\r\n22\r\n"), Seen: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if u1 != 1 || u2 != 2 {
			t.Fatalf("uids = %d,%d want 1,2", u1, u2)
		}
		msgs, err := s.ListMessages(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 || msgs[0].UID != 1 || msgs[1].UID != 2 {
			t.Fatalf("messages: %+v", msgs)
		}
		if !HasFlag(msgs[1].Flags, "\\Seen") || HasFlag(msgs[0].Flags, "\\Seen") {
			t.Fatalf("seen flags wrong: %v %v", msgs[0].Flags, msgs[1].Flags)
		}
		st, err := s.MailboxStatus(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if st.NumMessages != 2 || st.NumUnseen != 1 || st.UIDNext != 3 {
			t.Fatalf("status: %+v", st)
		}
	})

	t.Run("set flags and expunge", func(t *testing.T) {
		s := newStore(t)
		u1, _ := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("m1\r\n")})
		u2, _ := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("m2\r\n")})
		if err := s.SetFlags(ctx, "alice@example.com", "INBOX", u1, []string{"\\Seen", "\\Deleted"}); err != nil {
			t.Fatal(err)
		}
		msg, err := s.MessageByUID(ctx, "alice@example.com", "INBOX", u1)
		if err != nil {
			t.Fatal(err)
		}
		if !HasFlag(msg.Flags, "\\Deleted") {
			t.Fatalf("flags: %v", msg.Flags)
		}
		deleted, err := s.Expunge(ctx, "alice@example.com", "INBOX", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 1 || deleted[0] != u1 {
			t.Fatalf("expunged: %v", deleted)
		}
		if _, err := s.MessageByUID(ctx, "alice@example.com", "INBOX", u1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expunged message still present: %v", err)
		}
		left, _ := s.ListMessages(ctx, "alice@example.com", "INBOX")
		if len(left) != 1 || left[0].UID != u2 {
			t.Fatalf("remaining: %+v", left)
		}
	})

	t.Run("copy and move", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Archive"); err != nil {
			t.Fatal(err)
		}
		u1, _ := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("Subject: m\r\n\r\nbody\r\n"), Flags: []string{"\\Seen"}})
		mapping, err := s.Copy(ctx, "alice@example.com", "INBOX", "Archive", []uint32{u1})
		if err != nil {
			t.Fatal(err)
		}
		dstUID, ok := mapping[u1]
		if !ok {
			t.Fatalf("copy mapping missing %d: %v", u1, mapping)
		}
		cp, err := s.MessageByUID(ctx, "alice@example.com", "Archive", dstUID)
		if err != nil {
			t.Fatal(err)
		}
		if !HasFlag(cp.Flags, "\\Seen") {
			t.Fatalf("copy flags: %v", cp.Flags)
		}
		mapping, err = s.Move(ctx, "alice@example.com", "INBOX", "Archive", []uint32{u1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.MessageByUID(ctx, "alice@example.com", "INBOX", u1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("moved message still in source: %v", err)
		}
		if _, err := s.MessageByUID(ctx, "alice@example.com", "Archive", mapping[u1]); err != nil {
			t.Fatalf("moved message missing in dest: %v", err)
		}
	})

	t.Run("rename preserves uids", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Old"); err != nil {
			t.Fatal(err)
		}
		u1, _ := s.Deliver(ctx, "alice@example.com", "Old", &Message{Data: []byte("m\r\n")})
		if err := s.RenameMailbox(ctx, "alice@example.com", "Old", "New"); err != nil {
			t.Fatal(err)
		}
		msg, err := s.MessageByUID(ctx, "alice@example.com", "New", u1)
		if err != nil {
			t.Fatal(err)
		}
		if msg.UID != u1 {
			t.Fatalf("uid changed across rename: %d → %d", u1, msg.UID)
		}
		u2, err := s.Deliver(ctx, "alice@example.com", "New", &Message{Data: []byte("m2\r\n")})
		if err != nil {
			t.Fatal(err)
		}
		if u2 <= u1 {
			t.Fatalf("uid not monotonic after rename: %d → %d", u1, u2)
		}
	})

	t.Run("delete mailbox removes messages", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Trash"); err != nil {
			t.Fatal(err)
		}
		_, _ = s.Deliver(ctx, "alice@example.com", "Trash", &Message{Data: []byte("junk\r\n")})
		if err := s.DeleteMailbox(ctx, "alice@example.com", "Trash"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MailboxStatus(ctx, "alice@example.com", "Trash"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("mailbox still present: %v", err)
		}
		quota, err := s.QuotaUsedBytes(ctx, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if quota != 0 {
			t.Fatalf("quota after delete: %d, want 0", quota)
		}
	})

	t.Run("subscribe", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "List"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSubscribed(ctx, "alice@example.com", "List", true); err != nil {
			t.Fatal(err)
		}
		st, err := s.MailboxStatus(ctx, "alice@example.com", "List")
		if err != nil {
			t.Fatal(err)
		}
		if !st.Subscribed {
			t.Fatal("mailbox not subscribed after SetSubscribed(true)")
		}
	})

	t.Run("uidvalidity changes on recreate", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		v1, err := s.CreateMailbox(ctx, "alice@example.com", "Box")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteMailbox(ctx, "alice@example.com", "Box"); err != nil {
			t.Fatal(err)
		}
		v2, err := s.CreateMailbox(ctx, "alice@example.com", "Box")
		if err != nil {
			t.Fatal(err)
		}
		if v1 == 0 || v2 == 0 || v1 == v2 {
			t.Fatalf("UIDVALIDITY must change on recreate: %d then %d", v1, v2)
		}
	})

	t.Run("open message body", func(t *testing.T) {
		s := newStore(t)
		body := []byte("From: a@x.test\r\nSubject: read\r\n\r\nhello\r\n")
		u1, _ := s.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: body})
		rc, err := s.OpenMessage(ctx, "alice@example.com", "INBOX", u1)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body mismatch: %q", got)
		}
	})
}
