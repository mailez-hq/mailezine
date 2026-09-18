//go:build docker

// Real-container e2e for the rspamd /checkv2 client. Run with a live rspamd:
//
//	docker run -d -p 11333:11333 ... # rspamd listening on 11333
//	go test -tags docker ./internal/rspamd -run TestRealRspamdCheckV2 -v
//
// The URL comes from RSPAMD_TEST_URL (default http://127.0.0.1:11333/checkv2).
package rspamd

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func TestRealRspamdCheckV2(t *testing.T) {
	url := os.Getenv("RSPAMD_TEST_URL")
	if url == "" {
		url = "http://127.0.0.1:11333/checkv2"
	}
	c := New(url, "", "", "mail.mailez.test", discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := []byte("From: sender@remote.test\r\n" +
		"To: alice@example.com\r\n" +
		"Subject: hello\r\n" +
		"Message-ID: <smoke-1@remote.test>\r\n" +
		"Date: Mon, 24 Aug 2026 10:00:00 +0800\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n" +
		"Hello, this is a well-formed test message.\r\n")
	res, err := c.Classify(ctx, net.ParseIP("203.0.113.9"), "sender@remote.test",
		[]string{"alice@example.com"}, data)
	if err != nil {
		t.Fatalf("classify against real rspamd: %v", err)
	}
	if res.Action == "" {
		t.Fatal("empty action from real rspamd")
	}
	t.Logf("action=%q score=%v required=%v headers=%v",
		res.Action, res.Score, res.RequiredScore, res.Headers)
}
