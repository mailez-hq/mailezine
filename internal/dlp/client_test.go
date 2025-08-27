package dlp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCheckVerdicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		found := false
		for {
			p, err := meta.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.FormName() == "raw" {
				b, _ := io.ReadAll(p)
				if !strings.Contains(string(b), "机密") {
					t.Errorf("raw missing keyword")
				}
				found = true
			}
			p.Close()
		}
		if !found {
			t.Error("raw part missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "hold", "reason": "命中规则", "id": 42})
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, testLogger())
	dec, err := c.Check(context.Background(), "alice@example.com", "alice@example.com",
		[]string{"bob@other.test"}, []byte("Subject: 机密\r\n\r\n正文\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "hold" || dec.ID != 42 || dec.Reason == "" {
		t.Fatalf("decision = %+v", dec)
	}
}

func TestCheckFailOpen(t *testing.T) {
	// 500 response: verdict must be pass (mail keeps flowing).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTP(srv.URL, testLogger())
	dec, err := c.Check(context.Background(), "a@b.c", "a@b.c", []string{"x@y.z"}, []byte("raw"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "pass" {
		t.Fatalf("action = %q, want pass", dec.Action)
	}
}

func TestCheckBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "block", "reason": "blocked by rule"})
	}))
	defer srv.Close()
	c := NewHTTP(srv.URL, testLogger())
	dec, err := c.Check(context.Background(), "a@b.c", "a@b.c", []string{"x@y.z"}, []byte("raw"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "block" || dec.Reason != "blocked by rule" {
		t.Fatalf("decision = %+v", dec)
	}
}
