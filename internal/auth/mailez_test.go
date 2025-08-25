package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailezAuthenticate(t *testing.T) {
	var gotUser, gotPass, gotProto, gotPort string
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/auth/email", func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("Auth-User")
		gotPass = r.Header.Get("Auth-Pass")
		gotProto = r.Header.Get("Auth-Protocol")
		gotPort = r.Header.Get("Auth-Port")
		if r.Header.Get("Auth-Method") != "PLAIN" {
			t.Errorf("Auth-Method = %q, want PLAIN", r.Header.Get("Auth-Method"))
		}
		w.Header().Set("Auth-Status", "OK")
		w.Header().Set("Auth-Server", "127.0.0.1")
		w.Header().Set("Auth-Port", "1143")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL + "/stack")
	ok, err := m.Authenticate(context.Background(), "alice@example.com", "s3cret", Options{
		Protocol: "imap",
		Port:     "1143",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected authenticated")
	}
	if gotUser != "alice@example.com" || gotPass != "s3cret" || gotProto != "imap" || gotPort != "1143" {
		t.Fatalf("headers: user=%q pass=%q proto=%q port=%q", gotUser, gotPass, gotProto, gotPort)
	}
}

func TestMailezRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/auth/email", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Auth-Status", "Authentication credentials invalid")
		w.Header().Set("Auth-Error-Code", "AUTHENTICATIONFAILED")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL + "/stack")
	ok, err := m.Authenticate(context.Background(), "alice@example.com", "wrong", Options{Protocol: "imap"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected rejected")
	}
}

func TestMailezRetriesTransientFailure(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/auth/email", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Auth-Status", "OK")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL + "/stack")
	ok, err := m.Authenticate(context.Background(), "alice@example.com", "s3cret", Options{Protocol: "imap"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected authenticated after retry")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2 (one retry)", hits)
	}
}

func TestMailezNoRetryOnRejection(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/auth/email", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Auth-Status", "Authentication credentials invalid")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL + "/stack")
	ok, err := m.Authenticate(context.Background(), "alice@example.com", "wrong", Options{Protocol: "imap"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected rejected")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (rejections must not be retried)", hits)
	}
}

func TestMailezServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stack/auth/email", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewMailez(srv.URL + "/stack")
	if _, err := m.Authenticate(context.Background(), "a@b.c", "x", Options{Protocol: "imap"}); err == nil {
		t.Fatal("expected error on non-200")
	}
}
