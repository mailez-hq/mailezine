package fts

import (
	"context"
	"encoding/base64"
	"testing"

	// The indexer decodes each part through go-message's process-wide charset
	// hook; importing imapserver is what installs it in this binary (the engine
	// links it the same way).
	_ "mailezine/internal/imapserver"

	"golang.org/x/text/encoding/htmlindex"
)

func gbk(t *testing.T, s string) []byte {
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

// A 126.com-shaped message: GBK text behind base64 in a multipart body. The
// index walks MIME parts, so it must index the decoded text — otherwise a
// query for the words the message plainly shows finds nothing.
func TestIndexDecodesCharsetAndTransferEncoding(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	want := "接收邮件呢？有问题吗？"
	msg := "From: gushing@126.com\r\nTo: admin@example.com\r\n" +
		"Subject: =?GBK?B?" + base64.StdEncoding.EncodeToString(gbk(t, "测试一下对外发送邮件")) + "?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(gbk(t, want)) + "\r\n" +
		"--b1--\r\n"
	if err := ix.IndexMessage(ctx, "admin@example.com", "INBOX", 1, []byte(msg)); err != nil {
		t.Fatal(err)
	}

	for _, term := range []string{"接收邮件", "有问题吗", "测试一下"} {
		got, err := ix.SearchText(ctx, "admin@example.com", "INBOX", []string{term})
		if err != nil {
			t.Fatalf("%s: %v", term, err)
		}
		if _, ok := got[1]; !ok {
			t.Fatalf("%s: not found, hits=%v", term, got)
		}
	}
	// And a term the message does not contain stays out.
	if got, err := ix.SearchText(ctx, "admin@example.com", "INBOX", []string{"zzz-absent"}); err == nil {
		if _, ok := got[1]; ok {
			t.Fatalf("absent term matched: %v", got)
		}
	}
}
