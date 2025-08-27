//go:build unix

package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"mailezine/internal/config"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
	maildirpkg "mailezine/internal/store/maildir"
	"mailezine/internal/store/s3test"
)

// TestMigrateMaildirToPebbleS3Blob verifies the --s3-* path of migrate:
// blobs land in the object store (not the local FS).
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
	if code := migrateMaildirToKV(src, s3.storageConfig("pebble", kvPath), false, logger); code != 0 {
		t.Fatalf("migrate maildir→pebble(s3) exit = %d", code)
	}
	if got := len(objects()); got == 0 {
		t.Fatal("no blobs landed in the object store")
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

// TestMigrateMaildirToTiDB exercises the -to tidb target against a live
// server (env-gated, MAILEZINE_TEST_TIDB_DSN). The engine's fixed
// mailezine_kv table is dropped before and after the run.
func TestMigrateMaildirToTiDB(t *testing.T) {
	dsn := os.Getenv("MAILEZINE_TEST_TIDB_DSN")
	if dsn == "" {
		t.Skip("set MAILEZINE_TEST_TIDB_DSN to exercise the TiDB migration target")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		if _, err := db.Exec("DROP TABLE IF EXISTS mailezine_kv"); err != nil {
			t.Fatalf("drop kv table: %v", err)
		}
	}
	drop()
	t.Cleanup(func() {
		drop()
		db.Close()
	})

	src := t.TempDir()
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
	if _, err := inbox.Append(strings.NewReader("From: b@x.test\r\nSubject: two\r\n\r\nbody two\r\n"),
		nil, mustTime(t, "2026-01-02T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if code := migrateMaildirToKV(src, config.StorageConfig{Backend: "tidb", DSN: dsn}, false, logger); code != 0 {
		t.Fatalf("migrate maildir→tidb exit = %d", code)
	}

	// Read back through the TiDB-backed store.
	kv, err := store.OpenTiDB(dsn, "mailezine_kv")
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	ms := mailstore.NewKV(store.New(kv, store.NewMemoryBlob()))
	msgs, err := ms.ListMessages(context.Background(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("tidb message count = %d, want 2", len(msgs))
	}
	if !mailstore.HasFlag(msgs[0].Flags, "\\Seen") {
		t.Fatalf("flags lost: %+v", msgs[0].Flags)
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
