package directory

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockMailez serves the directory contract shapes used by the client.
func mockMailez(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var userHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/directory/users/", func(w http.ResponseWriter, r *http.Request) {
		userHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"alice@example.com","enabled":true,"quotaBytes":1073741824,"quotaBytesUsed":0,"forwardEnabled":false,"forwardKeep":false,"forwardTargets":[],"recipientDelimiter":"+"}`))
	})
	mux.HandleFunc("/directory/domains/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"isLocal":true,"name":"example.com"}`))
	})
	mux.HandleFunc("/directory/aliases/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"targets":["alice@example.com"]}`))
	})
	mux.HandleFunc("/directory/relays/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"domain":"relay.example.net","transport":"smtp:relay.example.net:25"}`))
	})
	mux.HandleFunc("/directory/senders/", func(w http.ResponseWriter, r *http.Request) {
		// The control plane only permits an address when the authenticated
		// user (X-Auth-User) matches it (own address, alias or grant).
		user := r.Header.Get("X-Auth-User")
		email := strings.TrimPrefix(r.URL.Path, "/directory/senders/")
		if user == "" || !strings.EqualFold(user, email) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":true,"addresses":["alice@example.com"]}`))
	})
	mux.HandleFunc("/directory/srs/restore/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"original":"bob@remote.net"}`))
	})
	mux.HandleFunc("/directory/srs/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rewritten":"SRS0=abc=bob=remote.net"}`))
	})
	mux.HandleFunc("/directory/quota/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"limit":1073741824,"used":42}`))
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			used, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
			if err != nil || used < 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/directory/sieve/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"default","script":"keep;"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &userHits
}

func TestMailezDirectory(t *testing.T) {
	srv, userHits := mockMailez(t)
	m := NewMailez(srv.URL+"/directory", time.Minute, 1<<20)
	ctx := context.Background()

	u, err := m.User(ctx, "alice@example.com")
	if err != nil || !u.Enabled {
		t.Fatalf("user: %+v err=%v", u, err)
	}
	// Cached: second lookup must not hit the server.
	if _, err := m.User(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := userHits.Load(); got != 1 {
		t.Fatalf("expected 1 user hit after cache, got %d", got)
	}

	d, err := m.Domain(ctx, "example.com")
	if err != nil || !d.IsLocal || d.Name != "example.com" {
		t.Fatalf("domain: %+v err=%v", d, err)
	}
	targets, err := m.Aliases(ctx, "team@example.com")
	if err != nil || len(targets) != 1 {
		t.Fatalf("aliases: %v err=%v", targets, err)
	}
	r, err := m.Relay(ctx, "x@relay.example.net")
	if err != nil || r.Transport == "" {
		t.Fatalf("relay: %+v err=%v", r, err)
	}
	s, err := m.Sender(ctx, "alice@example.com", "alice@example.com")
	if err != nil || !s.Allowed {
		t.Fatalf("sender: %+v err=%v", s, err)
	}
	if _, err := m.Sender(ctx, "mallory@example.com", "alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign sender, got %v", err)
	}
	rewritten, err := m.SRSForward(ctx, "bob@remote.net")
	if err != nil || rewritten == "" {
		t.Fatalf("srs forward: %q err=%v", rewritten, err)
	}
	original, err := m.SRSRestore(ctx, "SRS0=abc=bob=remote.net")
	if err != nil || original != "bob@remote.net" {
		t.Fatalf("srs restore: %q err=%v", original, err)
	}
	q, err := m.Quota(ctx, "alice@example.com")
	if err != nil || q.Used != 42 {
		t.Fatalf("quota: %+v err=%v", q, err)
	}
	if err := m.UpdateQuotaUsed(ctx, "alice@example.com", 100); err != nil {
		t.Fatal(err)
	}
	sieve, err := m.Sieve(ctx, "alice@example.com")
	if err != nil || sieve.Name != "default" {
		t.Fatalf("sieve: %+v err=%v", sieve, err)
	}
}

func TestMailezNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/directory/users/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := NewMailez(srv.URL+"/directory", time.Minute, 1<<20)
	if _, err := m.User(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMailezQuotaUpdateBody(t *testing.T) {
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/directory/quota/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			buf := make([]byte, 64)
			n, _ := r.Body.Read(buf)
			gotBody = string(buf[:n])
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := NewMailez(srv.URL+"/directory", time.Minute, 1<<20)
	if err := m.UpdateQuotaUsed(context.Background(), "alice@example.com", 1234); err != nil {
		t.Fatal(err)
	}
	if gotBody != "1234" {
		t.Fatalf("quota update body = %q, want 1234", gotBody)
	}
}
