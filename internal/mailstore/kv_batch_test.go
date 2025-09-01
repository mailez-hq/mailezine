package mailstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

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
