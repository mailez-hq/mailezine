package imapserver

import (
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/text/encoding/htmlindex"
)

// encode renders a string the way a sender in that charset would.
func encode(t *testing.T, label, s string) []byte {
	t.Helper()
	enc, err := htmlindex.Get(label)
	if err != nil {
		t.Fatalf("htmlindex %s: %v", label, err)
	}
	out, err := enc.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode %s: %v", label, err)
	}
	return out
}

// The searchable rendering has to reach text that the raw bytes do not carry:
// a GBK part behind base64, in a multipart message whose headers are ASCII.
// That is exactly the 126.com shape, and before this the only text the index
// and the matcher saw was the base64.
func TestMessageTextDecodesCharsetAndTransferEncoding(t *testing.T) {
	want := "接收邮件呢？有问题吗？"
	raw := "From: a@example.com\r\nSubject: =?GBK?B?" +
		base64.StdEncoding.EncodeToString(encode(t, "gbk", "测试主题")) + "?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(encode(t, "gbk", want)) + "\r\n" +
		"--b1\r\nContent-Type: text/html; charset=GBK\r\n\r\n" +
		string(encode(t, "gbk", "<p>"+want+"</p>")) + "\r\n" +
		"--b1--\r\n"

	text := MessageText([]byte(raw))
	if !strings.Contains(text, want) {
		t.Fatalf("decoded text missing the body: %q", text)
	}
	if !strings.Contains(text, "测试主题") {
		t.Fatalf("decoded text missing the subject: %q", text)
	}
	body := MessageBodyText([]byte(raw))
	if !strings.Contains(body, want) {
		t.Fatalf("body text missing the body: %q", body)
	}
	if strings.Contains(body, "a@example.com") {
		t.Fatalf("body text leaked headers: %q", body)
	}
}

// Other charsets the registry knows travel the same path.
func TestMessageTextOtherCharsets(t *testing.T) {
	cases := []struct{ label, want string }{
		{"big5", "郵件預覽"},
		{"shift_jis", "メールのプレビュー"},
		{"euc-kr", "메일 미리보기"},
		{"windows-1251", "Почта"},
		{"iso-8859-7", "Ελληνικά"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := "Subject: test\r\nContent-Type: text/plain; charset=" + tc.label + "\r\n\r\n" +
				string(encode(t, tc.label, tc.want))
			if got := MessageText([]byte(raw)); !strings.Contains(got, tc.want) {
				t.Fatalf("%s: %q", tc.label, got)
			}
		})
	}
}

// Plain UTF-8 messages keep working, headers included.
func TestMessageTextPlain(t *testing.T) {
	raw := "Subject: hello\r\n\r\nbody text\r\n"
	text := MessageText([]byte(raw))
	if !strings.Contains(text, "hello") || !strings.Contains(text, "body text") {
		t.Fatalf("text = %q", text)
	}
}

// A single-part non-UTF-8 message (no multipart wrapper) takes the same path.
func TestMessageTextSinglePartGBK(t *testing.T) {
	want := "单段正文"
	raw := "Subject: one part\r\nContent-Type: text/plain; charset=GBK\r\n\r\n" +
		string(encode(t, "gbk", want))
	if got := MessageText([]byte(raw)); !strings.Contains(got, want) {
		t.Fatalf("text = %q", got)
	}
	if got := MessageBodyText([]byte(raw)); strings.TrimSpace(got) != want {
		t.Fatalf("body = %q", got)
	}
}
