package mailstore

import (
	"context"
	"errors"
	"testing"

	"mailezine/internal/store"
)

// TestKVModSeqSemantics: HIGHESTMODSEQ bumps on append/flag/expunge, and
// messages carry the modseq of their last change.
func TestKVModSeqSemantics(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))

	// Append #1: mailbox modseq 1, message modseq 1.
	u1, err := k.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("Subject: a\r\n\r\n1")})
	if err != nil {
		t.Fatal(err)
	}
	// Append #2: mailbox modseq 2.
	if _, err := k.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("Subject: b\r\n\r\n2")}); err != nil {
		t.Fatal(err)
	}
	mb, err := k.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if mb.HighestModSeq != 2 {
		t.Fatalf("highestmodseq = %d, want 2", mb.HighestModSeq)
	}
	msgs, err := k.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ModSeq != 1 || msgs[1].ModSeq != 2 {
		t.Fatalf("message modseqs = %d %d", msgs[0].ModSeq, msgs[1].ModSeq)
	}

	// Flag change: mailbox modseq 3, only the changed message gets 3.
	if err := k.SetFlags(ctx, "alice@example.com", "INBOX", u1, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	mb, _ = k.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if mb.HighestModSeq != 3 {
		t.Fatalf("highestmodseq after flag = %d, want 3", mb.HighestModSeq)
	}
	msgs, _ = k.ListMessages(ctx, "alice@example.com", "INBOX")
	if msgs[0].ModSeq != 3 || msgs[1].ModSeq != 2 {
		t.Fatalf("modseqs after flag = %d %d, want 3 2", msgs[0].ModSeq, msgs[1].ModSeq)
	}

	// Expunge: mailbox modseq 4.
	if err := k.SetFlags(ctx, "alice@example.com", "INBOX", msgs[1].UID, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Expunge(ctx, "alice@example.com", "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mb, _ = k.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if mb.HighestModSeq != 5 {
		t.Fatalf("highestmodseq after expunge = %d, want 5", mb.HighestModSeq)
	}
}

// TestKVMailboxModSeqGate: MailboxModSeq tracks MailboxStatus.HighestModSeq
// exactly, so a Poll gate can trust "unchanged modseq ⇒ unchanged listing".
// It stays supported for a created-but-never-changed mailbox (modseq 0,
// field absent) and reports unsupported — not an error — for a vanished
// mailbox, so the caller falls back to listing, which owns ErrNotFound.
func TestKVMailboxModSeqGate(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))

	// Created-but-unchanged mailbox: supported, modseq 0.
	if _, err := k.CreateMailbox(ctx, "alice@example.com", "Archive"); err != nil {
		t.Fatal(err)
	}
	ms, ok := k.MailboxModSeq(ctx, "alice@example.com", "Archive")
	if !ok || ms != 0 {
		t.Fatalf("fresh Archive gate = (%d, %v), want (0, true)", ms, ok)
	}

	u1, err := k.Deliver(ctx, "alice@example.com", "Archive", &Message{Data: []byte("Subject: a\r\n\r\n1")})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ = k.MailboxModSeq(ctx, "alice@example.com", "Archive")
	mb, _ := k.MailboxStatus(ctx, "alice@example.com", "Archive")
	if ms != mb.HighestModSeq {
		t.Fatalf("gate modseq %d != status modseq %d after deliver", ms, mb.HighestModSeq)
	}

	if err := k.SetFlags(ctx, "alice@example.com", "Archive", u1, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	ms2, _ := k.MailboxModSeq(ctx, "alice@example.com", "Archive")
	if ms2 <= ms {
		t.Fatalf("gate did not bump on flag change: %d -> %d", ms, ms2)
	}

	if err := k.DeleteMailbox(ctx, "alice@example.com", "Archive"); err != nil {
		t.Fatal(err)
	}
	if ms, ok := k.MailboxModSeq(ctx, "alice@example.com", "Archive"); ok {
		t.Fatalf("vanished mailbox gate = (%d, true), want (0, false)", ms)
	}
}

// TestKVIDCacheInvalidation: the id memo must not survive mailbox delete or
// account purge — a same-name re-creation gets a new doc ID (and new
// UIDVALIDITY), and a stale memo would serve the old, empty document.
func TestKVIDCacheInvalidation(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))

	// Warm the memo, then delete and re-create the mailbox.
	if _, err := k.Deliver(ctx, "alice@example.com", "Archive", &Message{Data: []byte("Subject: old\r\n\r\n1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateMailbox(ctx, "alice@example.com", "Keep"); err != nil {
		t.Fatal(err)
	}
	oldID, err := k.cachedMailboxDocID(ctx, 1, "Archive")
	_ = oldID
	if err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteMailbox(ctx, "alice@example.com", "Archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateMailbox(ctx, "alice@example.com", "Archive"); err != nil {
		t.Fatal(err)
	}
	newID, err := k.cachedMailboxDocID(ctx, 1, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if oldID == newID {
		t.Fatalf("doc id reused after delete+recreate: %d", newID)
	}
	// The re-created mailbox starts empty.
	msgs, err := k.ListMessages(ctx, "alice@example.com", "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("re-created mailbox serves %d stale messages", len(msgs))
	}
	if _, err := k.Deliver(ctx, "alice@example.com", "Archive", &Message{Data: []byte("Subject: new\r\n\r\n2")}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = k.ListMessages(ctx, "alice@example.com", "Archive")
	if len(msgs) != 1 {
		t.Fatalf("post-recreate delivery: got %d messages", len(msgs))
	}

	// Account purge invalidates the account resolution itself.
	if err := k.DeleteAccount(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.cachedAccountID(ctx, "alice@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account resolution after purge = %v, want ErrNotFound", err)
	}
}

// TestKVModSeqCopyMove: the destination mailbox bumps on COPY/MOVE; the
// source bumps on MOVE (removal) but not on COPY.
func TestKVModSeqCopyMove(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	if _, err := k.CreateMailbox(ctx, "alice@example.com", "Archive"); err != nil {
		t.Fatal(err)
	}
	u1, err := k.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("Subject: x\r\n\r\n1")})
	if err != nil {
		t.Fatal(err)
	}
	inboxBefore, _ := k.MailboxStatus(ctx, "alice@example.com", "INBOX")

	if _, err := k.Copy(ctx, "alice@example.com", "INBOX", "Archive", []uint32{u1}); err != nil {
		t.Fatal(err)
	}
	arch, _ := k.MailboxStatus(ctx, "alice@example.com", "Archive")
	if arch.HighestModSeq != 1 {
		t.Fatalf("archive modseq after copy = %d, want 1", arch.HighestModSeq)
	}
	inboxAfter, _ := k.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if inboxAfter.HighestModSeq != inboxBefore.HighestModSeq {
		t.Fatalf("source bumped on COPY: %d -> %d", inboxBefore.HighestModSeq, inboxAfter.HighestModSeq)
	}

	if _, err := k.Move(ctx, "alice@example.com", "INBOX", "Archive", []uint32{u1}); err != nil {
		t.Fatal(err)
	}
	inboxAfterMove, _ := k.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if inboxAfterMove.HighestModSeq != inboxAfter.HighestModSeq+1 {
		t.Fatalf("source not bumped on MOVE: %d -> %d", inboxAfter.HighestModSeq, inboxAfterMove.HighestModSeq)
	}
}
