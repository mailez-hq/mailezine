package delivery

import (
	"strings"
	"testing"
)

func TestOutcleanStripsPrivateHeaders(t *testing.T) {
	in := "Received: from internal (10.0.0.1) by gateway\r\n" +
		"\twith ESMTPS id abc123\r\n" +
		"From: alice@example.com\r\n" +
		"User-Agent: Thunderbird 115.0\r\n" +
		"X-Mailer: Snappymail\r\n" +
		"X-Originating-IP: 203.0.113.7\r\n" +
		"X-Enigmail: OpenPGP\r\n" +
		"X-Pgp-Agent: GnuPG\r\n" +
		"Mime-Version: 1.0 (Mac OS X Mail 8.1 (2010.6))\r\n" +
		"Subject: hello\r\n" +
		"\r\n" +
		"body with Received: line\r\n"
	got := string(Outclean([]byte(in)))

	for _, leak := range []string{"Received: from internal", "User-Agent:", "X-Mailer:", "X-Originating-IP:", "X-Enigmail:", "X-Pgp-Agent:", "\twith ESMTPS id"} {
		if strings.Contains(got, leak) {
			t.Errorf("private header %q still present:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "Mime-Version: 1.0\r\n") {
		t.Errorf("Mime-Version not normalized:\n%s", got)
	}
	if strings.Contains(got, "Mac OS X Mail") {
		t.Errorf("Mime-Version comment not stripped:\n%s", got)
	}
	if !strings.Contains(got, "From: alice@example.com") || !strings.Contains(got, "Subject: hello") {
		t.Errorf("legitimate headers lost:\n%s", got)
	}
	if !strings.Contains(got, "body with Received: line") {
		t.Errorf("body corrupted:\n%s", got)
	}
}

func TestOutcleanKeepsLFAndPlainMessages(t *testing.T) {
	lf := "Subject: hi\nX-Mailer: x\n\nbody\n"
	got := string(Outclean([]byte(lf)))
	if got != "Subject: hi\n\nbody\n" {
		t.Errorf("LF message: %q", got)
	}
	noHeader := Outclean([]byte("just a body"))
	if string(noHeader) != "just a body" {
		t.Errorf("plain message corrupted: %q", noHeader)
	}
}

func TestOutcleanMimeVersionVariants(t *testing.T) {
	cases := map[string]string{
		"Mime-Version: 1.0 (Comment)\r\n": "Mime-Version: 1.0\r\n",
		"MIME-VERSION: 1.0 (x)\r\n":       "Mime-Version: 1.0\r\n",
		"mime-version:1.0\r\n":            "Mime-Version: 1.0\r\n",
		"  Mime-Version: 1.0 (y)\r\n":     "  Mime-Version: 1.0\r\n",
		"Mime-Version: 1.0\r\n":           "Mime-Version: 1.0\r\n",
	}
	for in, want := range cases {
		got := string(Outclean([]byte(in)))
		if got != want {
			t.Errorf("Outclean(%q) = %q, want %q", in, got, want)
		}
	}
}
