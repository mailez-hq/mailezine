package sieve

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func route(t *testing.T, script, from string, data []byte) Result {
	t.Helper()
	res, err := NewEngine(discardLogger()).Route(context.Background(), script, from, []string{"alice@example.com"}, data)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFileIntoBySubject(t *testing.T) {
	script := `require "fileinto";
if header :contains "Subject" "spam" { fileinto "Junk"; }`
	spamMsg := []byte("From: s@x.test\r\nSubject: this is spam\r\n\r\nbody\r\n")
	res := route(t, script, "s@x.test", spamMsg)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0] != "Junk" {
		t.Fatalf("fileinto result: %+v", res)
	}
	// Non-matching subject keeps the implicit INBOX copy.
	okMsg := []byte("From: s@x.test\r\nSubject: hello\r\n\r\nbody\r\n")
	res = route(t, script, "s@x.test", okMsg)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0] != "INBOX" {
		t.Fatalf("implicit keep result: %+v", res)
	}
}

func TestDiscard(t *testing.T) {
	script := `require "fileinto";
if header :is "From" "bounce@x.test" { discard; }`
	res := route(t, script, "bounce@x.test", []byte("From: bounce@x.test\r\nSubject: b\r\n\r\nbody\r\n"))
	if !res.Discard {
		t.Fatalf("expected discard, got %+v", res)
	}
}

func TestKeepPlusFileInto(t *testing.T) {
	script := `require "fileinto";
if header :contains "Subject" "news" {
  fileinto "Lists";
  keep;
}`
	res := route(t, script, "s@x.test", []byte("From: s@x.test\r\nSubject: news today\r\n\r\nbody\r\n"))
	if len(res.Mailboxes) != 2 || res.Mailboxes[0] != "Lists" || res.Mailboxes[1] != "INBOX" {
		t.Fatalf("keep+fileinto result: %+v", res)
	}
}

func TestBrokenScriptFails(t *testing.T) {
	if _, err := NewEngine(discardLogger()).Route(context.Background(),
		"require \"fileinto\"; if {", "s@x.test", []string{"a@b.c"}, []byte("Subject: x\r\n\r\nb\r\n")); err == nil {
		t.Fatal("expected compile error")
	}
}

func TestEnvelopeTest(t *testing.T) {
	script := `require "envelope";
if envelope :is "to" "postmaster@example.com" { discard; }`
	res, err := NewEngine(discardLogger()).Route(context.Background(), script, "s@x.test",
		[]string{"postmaster@example.com"}, []byte("Subject: x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Discard {
		t.Fatalf("expected envelope discard, got %+v", res)
	}
}

func TestRedirect(t *testing.T) {
	script := `if header :contains "Subject" "fwd" { redirect "forward@remote.test"; }`
	res, err := NewEngine(discardLogger()).Route(context.Background(), script, "s@x.test",
		[]string{"alice@example.com"}, []byte("From: s@x.test\r\nSubject: fwd me\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Redirects) != 1 || res.Redirects[0] != "forward@remote.test" {
		t.Fatalf("redirects: %v", res.Redirects)
	}
	// A plain redirect cancels the implicit keep: no INBOX copy.
	if len(res.Mailboxes) != 0 {
		t.Fatalf("mailboxes: %v", res.Mailboxes)
	}
}

func TestReject(t *testing.T) {
	script := `require "reject";
if header :contains "Subject" "vip only" { reject "this list is not for you"; }`
	res, err := NewEngine(discardLogger()).Route(context.Background(), script, "s@x.test",
		[]string{"alice@example.com"}, []byte("From: s@x.test\r\nSubject: vip only\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Reject != "this list is not for you" {
		t.Fatalf("reject reason: %q", res.Reject)
	}
	if len(res.Mailboxes) != 0 {
		t.Fatalf("rejected message must not keep: %v", res.Mailboxes)
	}
}
