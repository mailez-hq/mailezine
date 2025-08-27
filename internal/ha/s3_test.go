package ha

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeS3 implements just enough of the S3 REST API for minio-go to run the
// lease store against it: object PUT/GET/HEAD/DELETE on a single flat
// namespace plus the conditional-write headers the correctness leans on —
// If-None-Match:* rejects creates of existing keys and If-Match compares
// the caller's ETag against the stored one (412 PreconditionFailed).
//
// One failure-injection knob lets the tests rehearse a hostile gateway:
// blackhole makes PUT answer success but discard the payload, simulating a
// write that never becomes visible; confirm() must fail closed.
type fakeS3 struct {
	mu        sync.Mutex
	objs      map[string]*fakeS3Object
	blackhole atomic.Bool
	nextETag  atomic.Int64

	srv *httptest.Server
}

type fakeS3Object struct {
	data []byte
	etag string // stored without quotes; quoted form goes on the wire
}

func startFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objs: map[string]*fakeS3Object{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// store binds an S3Store to this fake endpoint (path-style addressing,
// plain HTTP — httptest terminates on 127.0.0.1).
func (f *fakeS3) store(t *testing.T) *S3Store {
	t.Helper()
	endpoint := strings.TrimPrefix(f.srv.URL, "http://")
	st, err := NewS3Store(endpoint, "minioadmin", "minioadmin", "leases", "leader.json", false)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *fakeS3) replyErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Error><Code>%s</Code><Message>fake</Message></Error>", code)
}

// decodeAWSChunked unwraps the streaming-signature-v4 chunk framing
// minio-go wraps uploads in over plain HTTP:
//
//	<hex-size>;chunk-signature=<sig>\r\n<data>\r\n ... 0;chunk-signature=...\r\n
//
// It reports ok=false when the payload does not look chunked.
func decodeAWSChunked(b []byte) (out []byte, ok bool) {
	if !bytes.Contains(b[:min(len(b), 64)], []byte(";chunk-signature=")) {
		return nil, false
	}
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			return nil, false
		}
		sizePart, _, _ := strings.Cut(string(bytes.TrimSpace(b[:nl])), ";")
		n, err := strconv.ParseInt(sizePart, 16, 64)
		if err != nil || n < 0 || int(n) > len(b)-nl-1 {
			return nil, false
		}
		if n == 0 {
			return out, true // final chunk; trailers after this are ignored
		}
		out = append(out, b[nl+1:nl+1+int(n)]...)
		b = b[nl+1+int(n):]
		b = bytes.TrimPrefix(b, []byte("\r\n"))
	}
	return out, true
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	bucket, key, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || bucket == "" || key == "" {
		f.replyErr(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	id := bucket + "/" + key

	switch r.Method {
	case http.MethodHead:
		f.mu.Lock()
		o := f.objs[id]
		f.mu.Unlock()
		if o == nil {
			f.replyErr(w, http.StatusNotFound, s3NotFound)
			return
		}
		w.Header().Set("ETag", `"`+o.etag+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		f.mu.Lock()
		o := f.objs[id]
		f.mu.Unlock()
		if o == nil {
			f.replyErr(w, http.StatusNotFound, s3NotFound)
			return
		}
		w.Header().Set("ETag", `"`+o.etag+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write(o.data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.replyErr(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		if payload, ok := decodeAWSChunked(body); ok {
			body = payload
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		cur := f.objs[id]
		if inm := r.Header.Get("If-None-Match"); inm == "*" && cur != nil {
			f.replyErr(w, http.StatusPreconditionFailed, s3PrecondFailed)
			return
		}
		if im := r.Header.Get("If-Match"); im != "" {
			if cur == nil || strings.Trim(im, `"`) != cur.etag {
				f.replyErr(w, http.StatusPreconditionFailed, s3PrecondFailed)
				return
			}
		}
		if f.blackhole.Load() {
			// Lie about success: the object is not replaced.
			w.WriteHeader(http.StatusOK)
			return
		}
		etag := fmt.Sprintf("%x", f.nextETag.Add(1))
		f.objs[id] = &fakeS3Object{data: body, etag: etag}
		w.Header().Set("ETag", `"`+etag+`"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objs, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		f.replyErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// Generous relative to assertions below so they hold even when the whole
// test binary runs under heavy cross-package scheduling pressure.
const s3TestTTL = 300 * time.Millisecond

func TestS3LeaseLifecycle(t *testing.T) {
	st := startFakeS3(t).store(t)
	ctx := context.Background()

	l, err := st.TryAcquire(ctx, "a", s3TestTTL)
	if err != nil {
		t.Fatal(err)
	}
	if l.Epoch != 1 {
		t.Fatalf("first lease epoch = %d, want 1", l.Epoch)
	}
	// Second instance cannot acquire while a holds a valid lease.
	if _, err := st.TryAcquire(ctx, "b", s3TestTTL); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("b acquired while a holds: %v", err)
	}
	// Renew works only for the holder (and exercises the confirm path).
	if got, err := st.Renew(ctx, "a", s3TestTTL); err != nil || got.Epoch != 1 {
		t.Fatalf("renew: lease=%+v err=%v", got, err)
	}
	if _, err := st.Renew(ctx, "b", s3TestTTL); !errors.Is(err, ErrLeaseStolen) {
		t.Fatalf("b renewed without holding: %v", err)
	}
	// Expiry lets b take over via compare-and-swap; the epoch advances.
	time.Sleep(1000 * time.Millisecond)
	l, err = st.TryAcquire(ctx, "b", s3TestTTL)
	if err != nil {
		t.Fatalf("b takeover after expiry: %v", err)
	}
	if l.Epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", l.Epoch)
	}
	// The evicted holder sees its lease as stolen.
	if _, err := st.Renew(ctx, "a", s3TestTTL); !errors.Is(err, ErrLeaseStolen) {
		t.Fatalf("stale holder renew err = %v, want ErrLeaseStolen", err)
	}
	// Release by owner frees the lease.
	if err := st.Release(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryAcquire(ctx, "c", s3TestTTL); err != nil {
		t.Fatal(err)
	}
}

// Concurrent acquirers racing for a free (and expired) lease must yield
// exactly one winner per round; every loser reports leadership loss.
func TestS3LeaseConcurrentAcquire(t *testing.T) {
	st := startFakeS3(t).store(t)
	ctx := context.Background()
	if _, err := st.TryAcquire(ctx, "seed", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  int
		lostOut  int
		otherErr []error
	)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.TryAcquire(ctx, fmt.Sprintf("r%d", i), time.Second)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, ErrNotLeader):
				lostOut++
			default:
				otherErr = append(otherErr, err)
			}
		}(i)
	}
	wg.Wait()
	if len(otherErr) > 0 {
		t.Fatalf("unexpected acquire errors: %v", otherErr)
	}
	if winners != 1 || lostOut != racers-1 {
		t.Fatalf("race produced winners=%d losers-not-leader=%d, want 1/%d", winners, lostOut, racers-1)
	}
}

// confirm() must fail closed when a gateway accepted the write but never
// made it visible — both on create and on takeover.
func TestS3ConfirmDetectsSilentWriteLoss(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		f := startFakeS3(t)
		st := f.store(t)
		f.blackhole.Store(true)
		if _, err := st.TryAcquire(context.Background(), "a", time.Second); !errors.Is(err, ErrLeaseStolen) {
			t.Fatalf("create against write-eating backend err=%v, want ErrLeaseStolen", err)
		}
	})
	t.Run("takeover", func(t *testing.T) {
		f := startFakeS3(t)
		st := f.store(t)
		ctx := context.Background()
		if _, err := st.TryAcquire(ctx, "seed", 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		f.blackhole.Store(true)
		if _, err := st.TryAcquire(ctx, "next", time.Second); !errors.Is(err, ErrLeaseStolen) {
			t.Fatalf("takeover against write-eating backend err=%v, want ErrLeaseStolen", err)
		}
	})
}

// The full Leader lifecycle stays healthy across many compare-and-swap
// renewals over real S3 semantics, and steps down cleanly on shutdown.
func TestS3LeaderRunRenewsAcrossETags(t *testing.T) {
	st := startFakeS3(t).store(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := NewLeader(st, "self", s3TestTTL, nil)
	if err := l.TryAcquire(ctx); err != nil {
		t.Fatal(err)
	}

	lost := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx, func() { close(lost) })
	}()

	// ttl/3 ticker fires ~8 times inside the window; every renewal rewrites
	// the object through a fresh If-Match ETag.
	time.Sleep(800 * time.Millisecond)
	select {
	case <-lost:
		t.Fatal("leadership lost despite healthy conditional renewals")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}
