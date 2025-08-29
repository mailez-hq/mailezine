package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/mailstore"
)

// FuzzMatchHeader ensures arbitrary header blocks and patterns never panic.
func FuzzMatchHeader(f *testing.F) {
	f.Add([]byte("Subject: hello\r\nX-Test: 1\r\n"), "Subject", "hello")
	f.Add([]byte("garbage"), "", "")
	f.Add([]byte{}, "a", "b")
	f.Fuzz(func(t *testing.T, block []byte, key, value string) {
		_ = matchHeader(block, key, value)
	})
}

// FuzzMatchSearch runs the full search matcher over arbitrary bodies.
func FuzzMatchSearch(f *testing.F) {
	f.Add([]byte("From: a@b.c\r\nSubject: news\r\n\r\nbody text\r\n"), "Subject", "news", "body")
	f.Add([]byte{}, "x", "y", "z")
	f.Fuzz(func(t *testing.T, raw []byte, headerKey, headerVal, text string) {
		msg := &mailstore.Message{UID: 1, Size: int64(len(raw))}
		criteria := &imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: headerKey, Value: headerVal}},
			Text:   []string{text},
		}
		_, _ = matchSearch(msg, 1, criteria, 1, 1, func() ([]byte, error) { return raw, nil })
	})
}
