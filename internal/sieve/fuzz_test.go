package sieve

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// FuzzRoute ensures arbitrary Sieve scripts and messages never panic the
// interpreter or the header parser.
func FuzzRoute(f *testing.F) {
	f.Add(`require "fileinto"; if header :contains "Subject" "spam" { fileinto "Junk"; }`,
		"From: a@b.c\r\nSubject: spam\r\n\r\nbody\r\n")
	f.Add(`if true { discard; }`, "garbage")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, script, msg string) {
		e := NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
		_, _ = e.Route(context.Background(), script, "sender@x.test",
			"a@example.com", []byte(msg))
	})
}
