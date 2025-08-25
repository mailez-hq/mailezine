package directory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMailezRetriesTransientFailure(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/directory/users/u@example.com", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"email":"u@example.com","enabled":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL+"/stack/directory", 30*time.Second, 1<<20)
	u, err := m.User(context.Background(), "u@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Enabled {
		t.Fatal("expected enabled after retry")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2 (one retry)", hits)
	}
}

func TestMailezNoRetryOnNotFound(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/directory/users/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL+"/stack/directory", 30*time.Second, 1<<20)
	if _, err := m.User(context.Background(), "nobody@example.com"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (404 must not be retried)", hits)
	}
}
