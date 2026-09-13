package imapserver

import (
	"bufio"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/emersion/go-message/textproto"
)

// word builds an RFC 2047 encoded word the way Chinese providers send it:
// base64 over GBK.
func word(t *testing.T, s string) string {
	t.Helper()
	enc, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return "=?GBK?B?" + base64.StdEncoding.EncodeToString(enc) + "?="
}

// A GBK subject (every mail from 126.com/163.com) must reach the envelope
// decoded: the stdlib word decoder knows only UTF-8/ISO-8859-1, so it used to
// pass "=?GBK?B?...?=" straight through to the list row and reader title.
func TestExtractEnvelopeDecodesGBKSubject(t *testing.T) {
	want := "Re:测试一下回复邮件"
	raw := "From: =?GBK?B?" + base64.StdEncoding.EncodeToString([]byte("顾行")) + "?= <gushing@126.com>\r\n" +
		"To: admin@mailez.cn\r\n" +
		"Subject: " + word(t, want) + "\r\n" +
		"Date: Mon, 14 Sep 2026 05:09:29 +0800\r\n" +
		"Message-ID: <25c8e17f.8.1a09c9a8ecf.Coremail.gushing@126.com>\r\n" +
		"\r\nbody\r\n"

	header, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	env := ExtractEnvelope(header)
	if env.Subject != want {
		t.Fatalf("subject = %q, want %q", env.Subject, want)
	}
	// The display name is decoded by the address decoder, which already knew
	// GBK; the subject was the gap.
	if len(env.From) != 1 || env.From[0].Name == "" {
		t.Fatalf("from = %+v", env.From)
	}
}

// An undecodable word keeps its raw form rather than disappearing.
func TestDecodeHeaderTextKeepsUnknownCharset(t *testing.T) {
	raw := "=?X-NONSENSE?B?aGVsbG8=?="
	if got := decodeHeaderText(raw); got != raw {
		t.Fatalf("unknown charset = %q, want the raw value", got)
	}
	if got := decodeHeaderText(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := decodeHeaderText("plain subject"); got != "plain subject" {
		t.Fatalf("plain = %q", got)
	}
}
