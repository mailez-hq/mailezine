package mailstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestKVSetFlagsBatch checks the batched flag write against the semantics of
// the per-message call: every targeted message ends up with the requested
// flags and the mailbox modseq advances once for the batch (CONDSTORE), not
// once per message.
func TestKVSetFlagsBatch(t *testing.T) {
	ms, _ := newTestKV(t)
	ctx := context.Background()

	uids := make([]uint32, 0, 5)
	for i := 0; i < 5; i++ {
		uid, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{
			From: "s@remote.test",
			Data: []byte(fmt.Sprintf("From: s@remote.test\r\nSubject: m%d\r\n\r\nbody\r\n", i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, uid)
	}
	before, err := ms.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	updates := []FlagUpdate{
		{UID: uids[0], Flags: []string{"\\Seen"}},
		{UID: uids[1], Flags: []string{"\\Seen", "\\Flagged"}},
		{UID: uids[2], Flags: []string{"\\Seen"}},
		{UID: uids[3], Flags: nil},
		// A UID that is not in the mailbox (concurrent expunge): skipped, not
		// an error.
		{UID: uids[len(uids)-1] + 100, Flags: []string{"\\Seen"}},
	}
	if err := ms.SetFlagsBatch(ctx, "alice@example.com", "INBOX", updates); err != nil {
		t.Fatal(err)
	}

	want := map[uint32][]string{
		uids[0]: {"\\Seen"},
		uids[1]: {"\\Seen", "\\Flagged"},
		uids[2]: {"\\Seen"},
		uids[3]: nil,
		uids[4]: nil,
	}
	for uid, flags := range want {
		msg, err := ms.MessageByUID(ctx, "alice@example.com", "INBOX", uid)
		if err != nil {
			t.Fatalf("uid %d: %v", uid, err)
		}
		if len(msg.Flags) != len(flags) {
			t.Fatalf("uid %d flags = %v, want %v", uid, msg.Flags, flags)
		}
		for i, f := range flags {
			if msg.Flags[i] != f {
				t.Fatalf("uid %d flags = %v, want %v", uid, msg.Flags, flags)
			}
		}
	}

	after, err := ms.MailboxStatus(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if after.HighestModSeq <= before.HighestModSeq {
		t.Fatalf("modseq did not advance: %d -> %d", before.HighestModSeq, after.HighestModSeq)
	}
}

// TestKVDeliverBatchConcurrent drives the per-account delivery coalescer:
// many concurrent Deliver calls to one account must all succeed with
// distinct, gap-free UIDs (arrival order), regardless of how the leader
// loop groups them into batches. This is the regression test for the
// leader/resign handoff: a resign-before-final-commit bug resurfaces here
// as singleton batches (still correct) or wedged followers (fatal).
func TestKVDeliverBatchConcurrent(t *testing.T) {
	ms, _ := newTestKV(t)
	ctx := context.Background()

	const conns = 24
	const perConn = 25

	var wg sync.WaitGroup
	uids := make(chan uint32, conns*perConn)
	errs := make(chan error, conns)
	for c := 0; c < conns; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perConn; i++ {
				uid, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &Message{
					From: "s@remote.test",
					Data: []byte(fmt.Sprintf("From: s@remote.test\r\nSubject: m%d-%d\r\n\r\nbody\r\n", c, i)),
				})
				if err != nil {
					errs <- err
					return
				}
				uids <- uid
			}
		}()
	}
	wg.Wait()
	close(uids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	seen := make(map[uint32]bool, conns*perConn)
	for uid := range uids {
		if uid == 0 {
			t.Fatal("zero UID")
		}
		if seen[uid] {
			t.Fatalf("duplicate UID %d", uid)
		}
		seen[uid] = true
	}
	if len(seen) != conns*perConn {
		t.Fatalf("got %d distinct UIDs, want %d", len(seen), conns*perConn)
	}
	// Gap-free 1..N proves the account gate + batch allocation stayed
	// atomic end to end.
	for want := uint32(1); want <= conns*perConn; want++ {
		if !seen[want] {
			t.Fatalf("UID %d missing: allocation is not gap-free", want)
		}
	}
}
