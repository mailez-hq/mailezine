package mailstore

import (
	"context"
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
