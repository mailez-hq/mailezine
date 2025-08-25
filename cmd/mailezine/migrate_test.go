//go:build unix

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailezine/internal/mailstore"
	"mailezine/internal/store"
	maildirpkg "mailezine/internal/store/maildir"
	"mailezine/internal/store/s3test"
)

// TestMigrateMaildirToPebbleS3Blob verifies the --s3-* path of migrate:
// blobs land in the object store (not the local FS) and a full maildir →
// KV(S3 blob) → maildir round trip preserves messages, flags and keywords.
func TestMigrateMaildirToPebbleS3Blob(t *testing.T) {
	endpoint, objects, cleanup := s3test.New(t)
	defer cleanup()
	s3 := s3Flags{endpoint: endpoint, accessKey: "test", secretKey: "test", bucket: "blobs"}

	src := t.TempDir()
	acct, err := maildirpkg.OpenAccount(filepath.Join(src, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Append(strings.NewReader("From: a@x.test\r\nSubject: s3 one\r\n\r\ns3 body one\r\n"),
		[]maildirpkg.Flag{maildirpkg.FlagSeen}, mustTime(t, "2026-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	u2, err := inbox.Append(strings.NewReader("From: b@x.test\r\nSubject: s3 two\r\n\r\ns3 body two\r\n"),
		nil, mustTime(t, "2026-01-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.SetKeywords(u2, []string{"s3label"}); err != nil {
		t.Fatal(err)
	}

	// maildir → KV with S3 blob: the KV lives on local disk, blobs in S3.
	kvPath := filepath.Join(t.TempDir(), "rocks")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if code := migrateMaildirToKV(src, "pebble", kvPath, false, s3, logger); code != 0 {
		t.Fatalf("migrate maildir→pebble(s3) exit = %d", code)
	}
	if got := len(objects()); got == 0 {
		t.Fatal("no blobs landed in the object store")
	}

	// KV(S3 blob) → maildir: read back through the S3-backed store.
	dst := t.TempDir()
	if code := migrateKVToMaildir("pebble", kvPath, dst, false, s3, logger); code != 0 {
		t.Fatalf("migrate pebble(s3)→maildir exit = %d", code)
	}
	dstMS := mailstore.NewMaildir(dst)
	msgs, err := dstMS.ListMessages(context.Background(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("round-trip message count = %d, want 2", len(msgs))
	}
	if msgs[0].UID != 1 || !mailstore.HasFlag(msgs[0].Flags, "\\Seen") {
		t.Fatalf("msg 1 flags lost: %+v", msgs[0])
	}
	if !mailstore.HasFlag(msgs[1].Keywords, "s3label") {
		t.Fatalf("msg 2 keyword lost: %+v", msgs[1])
	}
	rc, err := dstMS.OpenMessage(context.Background(), "alice@example.com", "INBOX", msgs[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(body), "s3 body one") {
		t.Fatalf("round-trip body: %q", body)
	}
}

func TestMigrateMaildirToPebble(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "rocks")

	// Account alice@example.com with INBOX (2 messages) and .Sent (1).
	acct, err := maildirpkg.OpenAccount(filepath.Join(src, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Append(strings.NewReader("From: a@x.test\r\nSubject: one\r\n\r\nbody one\r\n"),
		[]maildirpkg.Flag{maildirpkg.FlagSeen}, mustTime(t, "2026-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	u2, err := inbox.Append(strings.NewReader("From: b@x.test\r\nSubject: two\r\n\r\nbody two\r\n"),
		nil, mustTime(t, "2026-01-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.SetKeywords(u2, []string{"important"}); err != nil {
		t.Fatal(err)
	}
	sent, err := acct.OpenMailbox("Sent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sent.Append(strings.NewReader("From: alice@example.com\r\nSubject: out\r\n\r\nbye\r\n"),
		[]maildirpkg.Flag{maildirpkg.FlagReplied}, mustTime(t, "2026-01-03T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	if code := runMigrate([]string{"--src", src, "--to", "pebble", "--dst", dst}); code != 0 {
		t.Fatalf("runMigrate exit = %d", code)
	}

	// Verify the target store.
	kv, err := store.OpenPebble(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	blob, err := store.NewFSBlob(dst + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	ms := mailstore.NewKV(store.New(kv, blob))

	boxes, err := ms.ListMailboxes(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, b := range boxes {
		names[b.Name] = true
	}
	if !names["INBOX"] || !names["Sent"] {
		t.Fatalf("mailboxes: %v", names)
	}

	inboxMsgs, err := ms.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(inboxMsgs) != 2 || inboxMsgs[0].UID != 1 || inboxMsgs[1].UID != 2 {
		t.Fatalf("inbox order: %+v", inboxMsgs)
	}
	if !mailstore.HasFlag(inboxMsgs[0].Flags, "\\Seen") {
		t.Fatalf("seen flag lost: %v", inboxMsgs[0].Flags)
	}
	if !mailstore.HasFlag(inboxMsgs[1].Keywords, "important") {
		t.Fatalf("keyword lost: %v", inboxMsgs[1].Keywords)
	}
	sentMsgs, err := ms.ListMessages(ctx, "alice@example.com", "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if len(sentMsgs) != 1 || !mailstore.HasFlag(sentMsgs[0].Flags, "\\Answered") {
		t.Fatalf("sent: %+v", sentMsgs)
	}
	rc, err := ms.OpenMessage(ctx, "alice@example.com", "INBOX", inboxMsgs[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "body one") {
		t.Fatalf("body: %q", body)
	}
}

func TestMigratePebbleToMaildir(t *testing.T) {
	ctx := context.Background()
	kvPath := filepath.Join(t.TempDir(), "kv")
	kv, err := store.OpenPebble(kvPath)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.NewFSBlob(kvPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	ms := mailstore.NewKV(store.New(kv, blob))
	if _, err := ms.CreateMailbox(ctx, "alice@example.com", "Sent"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{
		Data:         []byte("From: a@x.test\r\nSubject: one\r\n\r\nbody one\r\n"),
		Flags:        []string{"\\Seen"},
		InternalDate: mustTime(t, "2026-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatal(err)
	}
	u2, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{
		Data:         []byte("From: b@x.test\r\nSubject: two\r\n\r\nbody two\r\n"),
		Keywords:     []string{"important"},
		InternalDate: mustTime(t, "2026-01-02T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.SetFlags(ctx, "alice@example.com", "INBOX", u2, []string{"\\Answered", "important"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "alice@example.com", "Sent", &mailstore.Message{
		Data: []byte("From: alice@example.com\r\nSubject: out\r\n\r\nbye\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}

	maildirRoot := filepath.Join(t.TempDir(), "mail")
	if code := runMigrate([]string{"--from", "pebble", "--src", kvPath, "--to", "maildir", "--dst", maildirRoot}); code != 0 {
		t.Fatalf("runMigrate exit = %d", code)
	}

	// Verify through the maildir backend.
	out := mailstore.NewMaildir(maildirRoot)
	boxes, err := out.ListMailboxes(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, b := range boxes {
		names[b.Name] = true
	}
	if !names["INBOX"] || !names["Sent"] {
		t.Fatalf("mailboxes: %v", names)
	}
	inbox, err := out.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 2 {
		t.Fatalf("inbox messages: %d", len(inbox))
	}
	if !mailstore.HasFlag(inbox[0].Flags, "\\Seen") {
		t.Fatalf("seen lost: %v", inbox[0].Flags)
	}
	if !mailstore.HasFlag(inbox[1].Flags, "\\Answered") || !mailstore.HasFlag(inbox[1].Keywords, "important") {
		t.Fatalf("flags/keywords lost: %v %v", inbox[1].Flags, inbox[1].Keywords)
	}
	rc, err := out.OpenMessage(ctx, "alice@example.com", "INBOX", inbox[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "body one") {
		t.Fatalf("body: %q", body)
	}

	// The uidlist written by the export is readable by the maildir package
	// (uidlist v3 shape).
	acct, err := maildirpkg.OpenAccount(filepath.Join(maildirRoot, "alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := mb.Messages(); err != nil || len(got) != 2 {
		t.Fatalf("maildir package sees %d messages err=%v", len(got), err)
	}
	raw, err := os.ReadFile(filepath.Join(maildirRoot, "alice@example.com", "dovecot-uidlist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "V") || !strings.Contains(string(raw), "G") {
		t.Fatalf("uidlist missing metadata:\n%s", raw)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
