package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestMatchHeader(t *testing.T) {
	block := []byte("From: b@x.test\r\nSubject: second\r\nX-Custom: 1\r\n\r\nbody")
	cases := []struct {
		key, value string
		want       bool
	}{
		{"Subject", "second", true},
		{"subject", "SECOND", true},
		{"Subject", "first", false},
		{"X-Custom", "1", true},
		{"Missing", "", false},
	}
	for _, tc := range cases {
		got := matchHeader(block, tc.key, tc.value)
		if got != tc.want {
			t.Errorf("matchHeader(%q,%q) = %v, want %v", tc.key, tc.value, got, tc.want)
		}
	}
}

func TestMatchSearchHeader(t *testing.T) {
	msg := testMessage(1, "Subject: second\r\n\r\nbody")
	ok, err := matchSearch(msg, 1, &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "second"}},
	}, func() ([]byte, error) {
		return []byte("Subject: second\r\n\r\nbody"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected match")
	}
}
