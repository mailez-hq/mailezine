// QRESYNC expunge tombstones: every deletion records (expunge modseq → UID)
// in the same atomic batch, and ExpungedSince reads the log back in
// ascending UID order with the "strictly after since" filter.
package mailstore

import (
	"context"
	"slices"
	"testing"

	"mailezine/internal/store"
)

func TestKVExpungeTombstones(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	const who = "alice@example.com"

	u1, err := k.Deliver(ctx, who, "INBOX", &Message{Data: []byte("Subject: a\r\n\r\n1")})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := k.Deliver(ctx, who, "INBOX", &Message{Data: []byte("Subject: b\r\n\r\n2")})
	if err != nil {
		t.Fatal(err)
	}
	u3, err := k.Deliver(ctx, who, "INBOX", &Message{Data: []byte("Subject: c\r\n\r\n3")})
	if err != nil {
		t.Fatal(err)
	}

	// No tombstones yet.
	got, err := k.ExpungedSince(ctx, who, "INBOX", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("ExpungedSince before any delete = %v, %v; want none", got, err)
	}

	// Expunge u2: one tombstone, modseq == HIGHESTMODSEQ after the batch.
	if err := k.SetFlags(ctx, who, "INBOX", u2, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	deleted, err := k.Expunge(ctx, who, "INBOX", nil)
	if err != nil || !slices.Equal(deleted, []uint32{u2}) {
		t.Fatalf("expunge deleted = %v, %v; want [%d]", deleted, err, u2)
	}
	mb, err := k.MailboxStatus(ctx, who, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	got, err = k.ExpungedSince(ctx, who, "INBOX", 0)
	if err != nil || !slices.Equal(got, []uint32{u2}) {
		t.Fatalf("ExpungedSince(0) = %v, %v; want [%d]", got, err, u2)
	}
	// "Strictly after": the tombstone's own modseq is not "after" itself.
	got, err = k.ExpungedSince(ctx, who, "INBOX", mb.HighestModSeq)
	if err != nil || len(got) != 0 {
		t.Fatalf("ExpungedSince(%d) = %v, %v; want none", mb.HighestModSeq, got, err)
	}

	// MOVE: the source mailbox gets tombstones; the destination does not.
	if _, err := k.CreateMailbox(ctx, who, "Archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Move(ctx, who, "INBOX", "Archive", []uint32{u1}); err != nil {
		t.Fatal(err)
	}
	got, err = k.ExpungedSince(ctx, who, "INBOX", 0)
	if err != nil || !slices.Equal(got, []uint32{u1, u2}) {
		t.Fatalf("source tombstones = %v, %v; want [%d %d] ascending", got, err, u1, u2)
	}
	got, err = k.ExpungedSince(ctx, who, "Archive", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("destination tombstones = %v, %v; want none", got, err)
	}

	// PER-MAILBOX isolation: tombstones of INBOX do not leak elsewhere.
	if _, err := k.CreateMailbox(ctx, who, "Other"); err != nil {
		t.Fatal(err)
	}
	got, err = k.ExpungedSince(ctx, who, "Other", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("cross-mailbox leak = %v, %v; want none", got, err)
	}

	// Expunging nothing (no \Deleted flags left) must not bump HIGHESTMODSEQ.
	before, _ := k.MailboxStatus(ctx, who, "INBOX")
	if _, err := k.Expunge(ctx, who, "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	after, _ := k.MailboxStatus(ctx, who, "INBOX")
	if before.HighestModSeq != after.HighestModSeq {
		t.Fatalf("empty expunge bumped modseq: %d -> %d", before.HighestModSeq, after.HighestModSeq)
	}

	// u3 still lives.
	msgs, err := k.ListMessages(ctx, who, "INBOX")
	if err != nil || len(msgs) != 1 || msgs[0].UID != u3 {
		t.Fatalf("INBOX after expunges = %v, %v; want only uid %d", msgs, err, u3)
	}
}

func TestKVExpungeNothingTombstoneFree(t *testing.T) {
	ctx := context.Background()
	k := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	const who = "alice@example.com"
	// POP3-style explicit-UID expunge of an absent UID: no tombstone, no bump.
	u1, err := k.Deliver(ctx, who, "INBOX", &Message{Data: []byte("Subject: a\r\n\r\n1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Expunge(ctx, who, "INBOX", []uint32{u1 + 100}); err != nil {
		t.Fatal(err)
	}
	got, err := k.ExpungedSince(ctx, who, "INBOX", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("bogus-uid expunge tombstones = %v, %v; want none", got, err)
	}
}
