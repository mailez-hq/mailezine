//go:build docker

// Full-stack e2e against a real rspamd container. Prerequisite:
//
//	docker run -d -p 11333:11333 mailez/rspamd:local /usr/bin/rspamd -f --insecure
//	go test -tags docker ./cmd/mailezine -run TestEndToEndRspamd -v
//
// RSPAMD_TEST_URL defaults to http://127.0.0.1:11333/checkv2. The test
// asserts SMTP delivery completes, the message is stored, and the
// Authentication-Results header from the verify stage is present — i.e. the
// real rspamd scan runs on the inbound path without blocking delivery.
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gosmtp "github.com/emersion/go-smtp"
)

func TestEndToEndRspamd(t *testing.T) {
	if os.Getenv("RSPAMD_TEST_URL") == "" {
		t.Setenv("RSPAMD_TEST_URL", "http://127.0.0.1:11333/checkv2")
	}
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, false, "")
	t.Setenv("MAILEZINE_RSPAMD_URL", os.Getenv("RSPAMD_TEST_URL"))

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	client, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: hello\r\n" +
		"Message-ID: <real-rspamd@example.com>\r\nDate: Mon, 24 Aug 2026 10:00:00 +0800\r\n\r\n" +
		"Hello, this message goes through a real rspamd.\r\n"
	if err := smtpSend(client, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatalf("SMTP delivery with real rspamd: %v", err)
	}
	_ = client.Close()

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}

	ms := openMailstore(t, rocksPath)
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatalf("message not stored: %v", err)
	}
	var buf bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &buf); err != nil {
		t.Fatal(err)
	}
	raw := buf.String()
	if !strings.Contains(raw, "Authentication-Results:") {
		t.Fatalf("Authentication-Results missing from stored message:\n%s", raw)
	}
	if !strings.Contains(raw, "goes through a real rspamd") {
		t.Fatalf("body missing:\n%s", raw)
	}
}
