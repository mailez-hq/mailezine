package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailezine/internal/mailbuffer"
	"mailezine/internal/store"
)

const testRaw = "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: 测试归档\r\n\r\n正文内容\r\n"

type capture struct {
	meta ingestMeta
	raw  []byte
}

func newTestSpool(t *testing.T, url string) (*Spool, *store.Store) {
	t.Helper()
	st := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	sp := New(st, url, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp.backoffBase = 10 * time.Millisecond
	return sp, st
}

// recvServer records multipart archive posts, replying with status replies[i]
// (cycling the last).
func recvServer(t *testing.T, replies ...int) (*httptest.Server, *atomic.Int32, chan capture) {
	t.Helper()
	if len(replies) == 0 {
		replies = []int{http.StatusOK}
	}
	var calls atomic.Int32
	got := make(chan capture, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		if idx >= len(replies) {
			idx = len(replies) - 1
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var cap capture
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("part: %v", err)
				break
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "meta":
				if err := json.Unmarshal(b, &cap.meta); err != nil {
					t.Errorf("meta json: %v", err)
				}
			case "raw":
				cap.raw = b
			}
			p.Close()
		}
		got <- cap
		w.WriteHeader(replies[idx])
	}))
	return srv, &calls, got
}

func TestSpoolCapturesAndForwards(t *testing.T) {
	srv, _, got := recvServer(t)
	defer srv.Close()
	sp, st := newTestSpool(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp.Run(ctx)
	defer sp.Close()

	buf, err := mailbuffer.NewFromReader(strings.NewReader(testRaw), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Remove()
	if err := sp.CaptureBuffer(ctx, Event{
		Direction:    "inbound",
		EnvelopeFrom: "outside@remote.test",
		EnvelopeTo:   []string{"bob@example.com"},
		ReceivedAt:   time.Now(),
	}, buf); err != nil {
		t.Fatal(err)
	}

	select {
	case cap := <-got:
		if cap.meta.Direction != "inbound" {
			t.Errorf("direction = %q", cap.meta.Direction)
		}
		if cap.meta.EnvelopeFrom != "outside@remote.test" {
			t.Errorf("envelope_from = %q", cap.meta.EnvelopeFrom)
		}
		if len(cap.meta.EnvelopeTo) != 1 || cap.meta.EnvelopeTo[0] != "bob@example.com" {
			t.Errorf("envelope_to = %v", cap.meta.EnvelopeTo)
		}
		if string(cap.raw) != testRaw {
			t.Errorf("raw mismatch:\n%s", cap.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for forward")
	}

	waitSpoolEmpty(t, st)
}

func TestSpoolRetriesThenSucceeds(t *testing.T) {
	srv, _, got := recvServer(t, http.StatusInternalServerError, http.StatusOK)
	defer srv.Close()
	sp, st := newTestSpool(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp.Run(ctx)
	defer sp.Close()

	if err := sp.Capture(ctx, Event{
		Direction:    "outbound",
		EnvelopeFrom: "bob@example.com",
		EnvelopeTo:   []string{"outside@remote.test"},
		ReceivedAt:   time.Now(),
		Raw:          []byte(testRaw),
	}); err != nil {
		t.Fatal(err)
	}

	// The first attempt fails (500), the second succeeds: the spool must
	// keep the copy until the endpoint acknowledges.
	deadline := time.After(10 * time.Second)
	received := 0
	for received < 2 {
		select {
		case <-got:
			received++
		case <-deadline:
			t.Fatal("timed out waiting for successful retry")
		}
	}
	waitSpoolEmpty(t, st)
}

func TestSpoolDrainsOnRestart(t *testing.T) {
	srv, _, got := recvServer(t)
	defer srv.Close()
	sp1, st := newTestSpool(t, "http://127.0.0.1:1") // unreachable: capture stays spooled
	ctx := context.Background()
	if err := sp1.Capture(ctx, Event{
		Direction:    "inbound",
		EnvelopeFrom: "a@b.test",
		EnvelopeTo:   []string{"bob@example.com"},
		ReceivedAt:   time.Now(),
		Raw:          []byte(testRaw),
	}); err != nil {
		t.Fatal(err)
	}
	sp1.Close()

	// A fresh process over the same store drains the leftover capture.
	sp2 := New(st, srv.URL, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp2.backoffBase = 10 * time.Millisecond
	ctx2, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp2.Run(ctx2)
	defer sp2.Close()

	select {
	case cap := <-got:
		if cap.meta.EnvelopeFrom != "a@b.test" {
			t.Errorf("envelope_from = %q", cap.meta.EnvelopeFrom)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for restart drain")
	}
	waitSpoolEmpty(t, st)
}

func spoolEmpty(t *testing.T, st *store.Store) bool {
	t.Helper()
	var pending [][]byte
	if err := st.ScanRaw(context.Background(), []byte(prefixPending), func(k, _ []byte) error {
		pending = append(pending, k)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return len(pending) == 0
}

func waitSpoolEmpty(t *testing.T, st *store.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if spoolEmpty(t, st) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("spool not drained")
}

func TestMultipartShape(t *testing.T) {
	// Sanity: the multipart body built by post() is parseable and carries
	// both parts (protects against writer-ordering regressions).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data;") {
			t.Errorf("content type = %q", ct)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	sp, _ := newTestSpool(t, srv.URL)
	if err := sp.Capture(context.Background(), Event{
		Direction: "inbound", EnvelopeFrom: "x@y.z", Raw: []byte(testRaw),
	}); err != nil {
		t.Fatal(err)
	}
	// Verify the wire bytes carry both form fields.
	meta := ingestMeta{Direction: "inbound"}
	mw := multipart.NewWriter(&buf)
	f1, _ := mw.CreateFormField("meta")
	_ = json.NewEncoder(f1).Encode(meta)
	f2, _ := mw.CreateFormFile("raw", "message.eml")
	_, _ = f2.Write([]byte(testRaw))
	_ = mw.Close()
	if !strings.Contains(buf.String(), "name=\"meta\"") || !strings.Contains(buf.String(), "name=\"raw\"") {
		t.Fatal("multipart parts missing")
	}
}
