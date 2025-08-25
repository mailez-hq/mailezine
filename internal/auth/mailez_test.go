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
