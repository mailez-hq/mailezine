package queue

import (
	"strings"
	"testing"
	"time"
)

func TestPostmasterAddress(t *testing.T) {
	cases := []struct {
		rcpt, hostname, want string
	}{
		// The postmaster must answer at the sender's MAIL domain, not the
		// MTA hostname (RFC 5321): admin@mailez.cn hears from
		// postmaster@mailez.cn even though the MTA is mail.mailez.cn.
		{"admin@mailez.cn", "mail.mailez.cn", "postmaster@mailez.cn"},
		{"user@example.com", "mx.example.org", "postmaster@example.com"},
		// Unusable recipient: keep the historical hostname fallback.
		{"", "mail.mailez.cn", "postmaster@mail.mailez.cn"},
		{"not-an-address", "mail.mailez.cn", "postmaster@mail.mailez.cn"},
		{"@", "mail.mailez.cn", "postmaster@mail.mailez.cn"},
	}
	for _, c := range cases {
		if got := postmasterAddress(c.rcpt, c.hostname); got != c.want {
			t.Errorf("postmasterAddress(%q, %q) = %q, want %q", c.rcpt, c.hostname, got, c.want)
		}
	}
}

func TestComposeDelayDSNPostmasterDomain(t *testing.T) {
	msg := &Message{
		From:        "admin@mailez.cn",
		Recipients:  []Recipient{{Address: "someone@126.com", Status: RecipientPending}},
		CreatedAt:   time.Now().Add(-35 * time.Minute),
	}
	raw, err := ComposeDelayDSN("admin@mailez.cn", msg, 35*time.Minute, "mail.mailez.cn")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "From: postmaster@mailez.cn") {
		t.Errorf("DSN From should use the mail domain; got:\n%s", firstHeaderLines(s))
	}
	if strings.Contains(s, "From: postmaster@mail.mailez.cn") {
		t.Errorf("DSN From must not use the MTA hostname")
	}
}

func firstHeaderLines(s string) string {
	if i := strings.Index(s, "\r\n\r\n"); i > 0 {
		return s[:i]
	}
	return s
}
