package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestClientDeliveredPostsPayloadWithSecret(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stack/notify/delivered" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(b, &gotBody)
		gotSecret = r.Header.Get("X-Stack-Secret")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL+"/stack/notify/delivered", nil, "sekrit")
	if err := c.Delivered(context.Background(), "amy@example.com", []Delivered{
		{Mailbox: "INBOX", UID: 42},
		{Mailbox: "INBOX/Sub", UID: 7},
	}); err != nil {
		t.Fatalf("Delivered: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSecret != "sekrit" {
		t.Fatalf("secret header = %q", gotSecret)
	}
	if gotBody["account"] != "amy@example.com" {
		t.Fatalf("account = %v", gotBody["account"])
	}
	refs, ok := gotBody["deliveries"].([]any)
	if !ok || len(refs) != 2 {
		t.Fatalf("deliveries = %v", gotBody["deliveries"])
	}
	first, _ := refs[0].(map[string]any)
	if first["mailbox"] != "INBOX" || first["uid"] != float64(42) {
		t.Fatalf("first ref = %v", first)
	}
}

func TestClientAsyncCompletesWithinDeadline(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		close(done)
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.DeliveredAsync("amy@example.com", []Delivered{{Mailbox: "INBOX", UID: 1}})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("async delivery receipt never arrived")
	}
}

func TestClientBackendErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if err := c.Delivered(context.Background(), "amy@example.com", []Delivered{{Mailbox: "INBOX", UID: 1}}); err == nil {
		t.Fatal("expected error from 503 backend")
	}
	// Async variant must swallow the same failure (it only logs).
	c.DeliveredAsync("amy@example.com", []Delivered{{Mailbox: "INBOX", UID: 1}})
}

func TestClientEmptyRefsAreNoops(t *testing.T) {
	c := New("http://127.0.0.1:1", nil)
	if err := c.Delivered(context.Background(), "amy@example.com", nil); err != nil {
		t.Fatalf("empty refs: %v", err)
	}
}
