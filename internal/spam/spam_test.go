package spam

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestClassify(t *testing.T) {
	var gotFrom, gotRcpt1, gotRcpt2, gotIP, gotHostname, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFrom = r.Header.Get("From")
		rcpts := r.Header.Values("Rcpt")
		if len(rcpts) > 0 {
			gotRcpt1 = rcpts[0]
		}
		if len(rcpts) > 1 {
			gotRcpt2 = rcpts[1]
		}
		gotIP = r.Header.Get("IP")
		gotHostname = r.Header.Get("Hostname")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"action":         "add header",
			"score":          5.2,
			"required_score": 7,
			"milter": map[string]any{
				"add_headers": map[string]any{
					"X-Spam-Flag":   map[string]any{"value": "YES", "order": 0},
					"X-Spam-Score":  map[string]any{"value": "5.2", "order": 1},
					"X-Spam-Status": map[string]any{"value": "Yes, score=5.2", "order": 2},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL+"/checkv2", "", "", "mail.mailez.test", discardLogger())
	res, err := c.Classify(context.Background(), net.ParseIP("203.0.113.9"),
		"sender@remote.test", []string{"a@example.com", "b@example.com"},
		[]byte("From: sender@remote.test\r\nSubject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "add header" || res.Score != 5.2 || res.RequiredScore != 7 {
		t.Fatalf("result: %+v", res)
	}
	if gotFrom != "sender@remote.test" || gotRcpt1 != "a@example.com" || gotRcpt2 != "b@example.com" ||
		gotIP != "203.0.113.9" || gotHostname != "mail.mailez.test" || !strings.Contains(gotCT, "message/rfc822") {
		t.Fatalf("request headers: from=%q rcpt1=%q rcpt2=%q ip=%q host=%q ct=%q",
			gotFrom, gotRcpt1, gotRcpt2, gotIP, gotHostname, gotCT)
	}
	if len(res.Headers) != 3 || !strings.HasPrefix(res.Headers[0], "X-Spam-Flag: YES") {
		t.Fatalf("headers: %v", res.Headers)
	}
}

func TestClassifyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/checkv2", "", "", "mail.mailez.test", discardLogger())
	if _, err := c.Classify(context.Background(), nil, "a@b.c", []string{"x@y.z"}, []byte("body")); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestLearn(t *testing.T) {
	var gotPath, gotPass string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPass = r.Header.Get("Password")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New("", srv.URL, "mailez", "mail.mailez.test", discardLogger())
	ctx := context.Background()
	if err := c.Learn(ctx, true, []byte("Subject: spam\r\n\r\nx\r\n")); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/learnspam" || gotPass != "mailez" || string(gotBody) != "Subject: spam\r\n\r\nx\r\n" {
		t.Fatalf("learn spam: path=%q pass=%q body=%q", gotPath, gotPass, gotBody)
	}
	if err := c.Learn(ctx, false, []byte("Subject: ham\r\n\r\ny\r\n")); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/learnham" {
		t.Fatalf("learn ham path = %q", gotPath)
	}
}

func TestLearnDisabled(t *testing.T) {
	c := New("", "", "", "mail.mailez.test", discardLogger())
	if err := c.Learn(context.Background(), true, []byte("x")); err != nil {
		t.Fatalf("learn without URL should no-op, got %v", err)
	}
}

func TestFuzzy(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New("", srv.URL, "mailez", "mail.mailez.test", discardLogger())
	ctx := context.Background()
	if err := c.Fuzzy(ctx, 13, true, []byte("spam body")); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/fuzzyadd?flag=13" {
		t.Fatalf("fuzzy add = %v", paths)
	}
	if err := c.Fuzzy(ctx, 11, false, []byte("ham body")); err != nil {
		t.Fatal(err)
	}
	if paths[1] != "/fuzzydel?flag=11" {
		t.Fatalf("fuzzy del = %v", paths)
	}
}

func TestLearnWithFuzzySequence(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New("", srv.URL, "mailez", "mail.mailez.test", discardLogger())
	ctx := context.Background()
	if err := c.LearnWithFuzzy(ctx, true, []byte("Subject: spam\r\n\r\nx")); err != nil {
		t.Fatal(err)
	}
	want := []string{"/learnspam?", "/fuzzyadd?flag=13", "/fuzzydel?flag=11"}
	if len(paths) != len(want) {
		t.Fatalf("spam sequence = %v", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("spam seq[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}
