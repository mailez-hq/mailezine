package queue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"mailezine/internal/store"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fakeDeliverer struct {
	mu      sync.Mutex
	calls   int
	results map[string]Result
	err     error
}

func (f *fakeDeliverer) Deliver(_ context.Context, from string, to []string, msg io.Reader) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	_, _ = io.Copy(io.Discard, msg)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Result, 0, len(to))
	for _, addr := range to {
		if r, ok := f.results[addr]; ok {
			out = append(out, r)
			continue
		}
		out = append(out, Result{To: addr, OK: true})
	}
	return out, nil
}

func (f *fakeDeliverer) callsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestManager(d Deliverer, opts Options, clock *fakeClock) (*Manager, *store.MemoryKV, *store.MemoryBlob) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	if clock != nil {
		opts.Now = clock.Now
	}
	m := New(kv, blob, d, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, kv, blob
}

func okResult(addrs ...string) map[string]Result {
	m := map[string]Result{}
	for _, a := range addrs {
		m[a] = Result{To: a, OK: true}
	}
	return m
}

func TestSubmitAndDeliverSuccess(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com", "b@example.com")}
	m, _, blob := newTestManager(d, DefaultOptions(), clock)
	ctx := context.Background()

	id, err := m.Submit(ctx, "sender@example.com", []string{"a@example.com", "b@example.com"}, "hi", strings.NewReader("Subject: hi\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := m.List(ctx)
	if len(msgs) != 1 || msgs[0].State != StateQueued || msgs[0].ID != id {
		t.Fatalf("after submit: %+v", msgs)
	}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ = m.List(ctx)
	if len(msgs) != 1 || msgs[0].State != StateDelivered {
		t.Fatalf("after delivery: %+v", msgs)
	}
	for _, r := range msgs[0].Recipients {
		if r.Status != RecipientDelivered {
			t.Fatalf("recipient not delivered: %+v", r)
		}
	}
	if err := blob.Get(ctx, msgs[0].BlobID, io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("body blob not cleaned up: %v", err)
	}
	if d.callsCount() != 1 {
		t.Fatalf("deliver calls = %d, want 1", d.callsCount())
	}
}

func TestDeferredThenRetried(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", Err: errors.New("451 temp")},
	}}
	opts := DefaultOptions()
	opts.BaseRetry = time.Minute
	m, _, _ := newTestManager(d, opts, clock)
	ctx := context.Background()

	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateDeferred || msgs[0].Attempts != 1 {
		t.Fatalf("expected deferred after temp failure: %+v", msgs[0])
	}
	if !msgs[0].NextAttempt.After(clock.now) {
		t.Fatalf("next attempt not in future: %v", msgs[0].NextAttempt)
	}
	// Not due yet: no second call.
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if d.callsCount() != 1 {
		t.Fatalf("deliver calls before due = %d, want 1", d.callsCount())
	}
	// Advance past the retry and succeed.
	clock.Advance(2 * time.Hour)
	d.results["a@example.com"] = Result{To: "a@example.com", OK: true}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ = m.List(ctx)
	if msgs[0].State != StateDelivered || msgs[0].Attempts != 2 {
		t.Fatalf("expected delivered after retry: %+v", msgs[0])
	}
	if d.callsCount() != 2 {
		t.Fatalf("deliver calls = %d, want 2", d.callsCount())
	}
}

func TestBounceAfterMaxAttempts(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", Err: errors.New("451 temp")},
	}}
	opts := DefaultOptions()
	opts.MaxAttempts = 2
	opts.BaseRetry = time.Minute
	m, _, blob := newTestManager(d, opts, clock)
	ctx := context.Background()

	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	_ = m.ProcessDue(ctx) // attempt 1 → deferred
	clock.Advance(2 * time.Hour)
	_ = m.ProcessDue(ctx) // attempt 2 ≥ max → bounced
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateBounced {
		t.Fatalf("expected bounced: %+v", msgs[0])
	}
	if msgs[0].Recipients[0].Status != RecipientBounced {
		t.Fatalf("recipient not bounced: %+v", msgs[0].Recipients[0])
	}
	if err := blob.Get(ctx, msgs[0].BlobID, io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("body blob not cleaned up: %v", err)
	}
}

func TestPermanentFailureBouncesImmediately(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", Permanent: true, Err: errors.New("550 no such user")},
	}}
	m, _, _ := newTestManager(d, DefaultOptions(), clock)
	ctx := context.Background()
	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	_ = m.ProcessDue(ctx)
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateBounced {
		t.Fatalf("expected immediate bounce: %+v", msgs[0])
	}
}

func TestTransportErrorDefers(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{err: errors.New("dial: connection refused")}
	opts := DefaultOptions()
	opts.BaseRetry = time.Minute
	m, _, _ := newTestManager(d, opts, clock)
	ctx := context.Background()
	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	_ = m.ProcessDue(ctx)
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateDeferred {
		t.Fatalf("expected deferred on transport error: %+v", msgs[0])
	}
}

func TestPersistenceAcrossManagers(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	opts := DefaultOptions()
	opts.Now = clock.Now
	m1 := New(kv, blob, d, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	_, _ = m1.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))

	m2 := New(kv, blob, d, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	msgs, err := m2.List(ctx)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("message lost across managers: %v %v", msgs, err)
	}
	if err := m2.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ = m2.List(ctx)
	if msgs[0].State != StateDelivered {
		t.Fatalf("delivery after reopen: %+v", msgs[0])
	}
}

func TestCancel(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m, _, blob := newTestManager(d, DefaultOptions(), clock)
	ctx := context.Background()
	id, _ := m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	if err := m.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m.List(ctx)
	if len(msgs) != 0 {
		t.Fatalf("cancel did not remove message: %+v", msgs)
	}
	if err := blob.Get(ctx, "q-0000000000000001", io.Discard); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel did not remove blob: %v", err)
	}
}

func TestRetry(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", Err: errors.New("451 temp")},
	}}
	opts := DefaultOptions()
	opts.BaseRetry = time.Hour
	m, _, _ := newTestManager(d, opts, clock)
	ctx := context.Background()
	id, _ := m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	_ = m.ProcessDue(ctx)
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateDeferred {
		t.Fatalf("setup: %+v", msgs[0])
	}
	d.results["a@example.com"] = Result{To: "a@example.com", OK: true}
	if err := m.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ = m.List(ctx)
	if msgs[0].State != StateDelivered {
		t.Fatalf("retry did not deliver: %+v", msgs[0])
	}
}

func TestListOrder(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m, _, _ := newTestManager(&fakeDeliverer{}, DefaultOptions(), clock)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "x", bytes.NewReader([]byte("body")))
	}
	msgs, _ := m.List(ctx)
	if len(msgs) != 3 || msgs[0].ID != 1 || msgs[2].ID != 3 {
		t.Fatalf("list order: %+v", msgs)
	}
}

type fakeSigner struct {
	calls int
	sign  func(from string, msg []byte) []byte
	err   error
}

func (f *fakeSigner) Sign(_ context.Context, from string, msg []byte) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.sign(from, msg), nil
}

func TestSubmitSignsOnceAtEnqueue(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m, _, blob := newTestManager(d, DefaultOptions(), clock)
	signer := &fakeSigner{sign: func(_ string, msg []byte) []byte {
		return append([]byte("DKIM-Signature: v=1;\r\n"), msg...)
	}}
	m.SetSigner(signer)
	ctx := context.Background()

	id, err := m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("Subject: hi\r\n\r\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if signer.calls != 1 {
		t.Fatalf("sign calls = %d, want 1", signer.calls)
	}
	// The spooled blob holds the signed message; delivery does not re-sign.
	var spool bytes.Buffer
	if err := blob.Get(ctx, "q-0000000000000001", &spool); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(spool.Bytes(), []byte("DKIM-Signature: v=1;")) {
		t.Fatalf("spooled message not signed: %q", spool.String())
	}
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if signer.calls != 1 {
		t.Fatalf("sign calls after delivery = %d, want 1", signer.calls)
	}
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateDelivered || msgs[0].ID != id {
		t.Fatalf("unexpected final state: %+v", msgs[0])
	}
}

func TestSubmitSignerErrorRejects(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m, _, _ := newTestManager(&fakeDeliverer{}, DefaultOptions(), clock)
	m.SetSigner(&fakeSigner{err: errors.New("vault down")})
	if _, err := m.Submit(context.Background(), "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body")); err == nil {
		t.Fatal("expected signer error to fail the submit")
	}
}

func TestLifecycleEvents(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m, _, _ := newTestManager(d, DefaultOptions(), clock)
	var events []string
	m.SetOnEvent(func(e string) { events = append(events, e) })
	ctx := context.Background()
	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))
	_ = m.ProcessDue(ctx)
	want := []string{"submitted", "delivered"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

func TestPauseResume(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m, _, _ := newTestManager(d, DefaultOptions(), clock)
	ctx := context.Background()
	_, _ = m.Submit(ctx, "s@example.com", []string{"a@example.com"}, "hi", strings.NewReader("body"))

	m.Pause()
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if d.callsCount() != 0 {
		t.Fatalf("delivered while paused: calls=%d", d.callsCount())
	}
	m.Resume()
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if d.callsCount() != 1 {
		t.Fatalf("calls after resume = %d, want 1", d.callsCount())
	}
}

func TestBounceHandlerInvoked(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	d := &fakeDeliverer{results: map[string]Result{
		"a@example.com": {To: "a@example.com", Permanent: true, Err: errors.New("550 no such user")},
	}}
	m, _, _ := newTestManager(d, DefaultOptions(), clock)
	var gotFrom string
	var gotFailures []BounceFailure
	m.SetBounceHandler(func(_ context.Context, from string, _ *Message, _ []byte, failures []BounceFailure) {
		gotFrom = from
		gotFailures = failures
	})
	ctx := context.Background()
	_, _ = m.Submit(ctx, "sender@example.com", []string{"a@example.com"}, "hi", strings.NewReader("Subject: hi\r\n\r\nbody"))
	_ = m.ProcessDue(ctx)
	msgs, _ := m.List(ctx)
	if msgs[0].State != StateBounced {
		t.Fatalf("state: %+v", msgs[0])
	}
	if gotFrom != "sender@example.com" || len(gotFailures) != 1 || gotFailures[0].To != "a@example.com" {
		t.Fatalf("bounce: from=%q failures=%v", gotFrom, gotFailures)
	}
}

func TestComposeBounceDSN(t *testing.T) {
	msg := &Message{ID: 7, From: "sender@example.com", CreatedAt: clockNow, UpdatedAt: clockNow}
	dsnBytes, err := ComposeBounceDSN("sender@example.com", msg, []BounceFailure{
		{To: "missing@example.net", Status: "5.1.1", Comment: "550 no such user"},
	}, "mail.mailez.test")
	if err != nil {
		t.Fatal(err)
	}
	text := string(dsnBytes)
	for _, want := range []string{
		"Content-Type: multipart/report",
		"message/delivery-status",
		"Action: failed",
		"missing@example.net",
		"mail.mailez.test",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("DSN missing %q:\n%s", want, text)
		}
	}
}

var clockNow = time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
