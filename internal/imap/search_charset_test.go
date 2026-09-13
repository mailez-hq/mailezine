package imap

import (
	"encoding/base64"
	"testing"

	"github.com/emersion/go-imap/v2"
	"golang.org/x/text/encoding/htmlindex"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	enc, err := htmlindex.Get("gbk")
	if err != nil {
		t.Fatal(err)
	}
	out, err := enc.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A message sent in GBK with a base64 body carries the query nowhere in its
// raw bytes: the text is GBK, and the transfer encoding hides it entirely.
// TEXT used to match the raw buffer only, so searching for the very words the
// message contains found nothing.
func TestSearchTextMatchesDecodedCharset(t *testing.T) {
	want := "接收邮件呢？有问题吗？"
	raw := "From: a@example.com\r\nSubject: =?GBK?B?" +
		base64.StdEncoding.EncodeToString(gbkBytes(t, "测试一下对外发送邮件")) + "?=\r\n" +
		"Content-Type: text/plain; charset=GBK\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(gbkBytes(t, want)) + "\r\n"
	msg := testMessage(1, raw)
	match := func(c *imap.SearchCriteria) bool {
		t.Helper()
		ok, err := matchSearch(msg, 1, c, 1, 1, func() ([]byte, error) {
			return []byte(raw), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !match(&imap.SearchCriteria{Text: []string{"有问题吗"}}) {
		t.Fatal("TEXT did not find the GBK body text")
	}
	if !match(&imap.SearchCriteria{Body: []string{"接收邮件"}}) {
		t.Fatal("BODY did not find the GBK body text")
	}
	if !match(&imap.SearchCriteria{Text: []string{"测试一下"}}) {
		t.Fatal("TEXT did not find the encoded-word subject")
	}
	if match(&imap.SearchCriteria{Text: []string{"không có gì"}}) {
		t.Fatal("TEXT matched text the message does not contain")
	}
	// An ASCII term in the (base64) body is still found after decoding.
	if match(&imap.SearchCriteria{Body: []string{"@@@ nope @@@"}}) {
		t.Fatal("BODY matched a term that is absent")
	}
}
