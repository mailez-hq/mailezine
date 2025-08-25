// Queue crash consistency: a message spooled by a process that dies without
// a graceful close must survive WAL recovery and be deliverable after
// reopen.
package queue

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mailezine/internal/store"
)

func TestQueueCrashConsistency(t *testing.T) {
	if os.Getenv("MAILEZINE_QUEUE_CRASH_CHILD") == "1" {
		queueCrashChild()
		os.Exit(1)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestQueueCrashConsistency$")
	cmd.Env = append(os.Environ(),
		"MAILEZINE_QUEUE_CRASH_CHILD=1",
		"MAILEZINE_QUEUE_CRASH_DIR="+dbPath,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("child process unexpectedly exited cleanly")
	}

	kv, err := store.OpenPebble(dbPath)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer kv.Close()
	blob, err := store.NewFSBlob(dbPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDeliverer{results: okResult("a@example.com")}
	m := New(kv, blob, d, DefaultOptions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	msgs, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("queued message lost after crash: %+v", msgs)
	}
	msg := msgs[0]
	if msg.From != "alice@example.com" || len(msg.Recipients) != 1 ||
		msg.Recipients[0].Address != "a@example.com" || msg.State != StateQueued {
		t.Fatalf("queue message after crash: %+v", msg)
	}
	// The spooled body survives.
	var body strings.Builder
	if err := blob.Get(ctx, msg.BlobID, &body); err != nil {
		t.Fatalf("spooled body lost: %v", err)
	}
	if !strings.Contains(body.String(), "crash probe") {
		t.Fatalf("body mismatch: %q", body.String())
	}
	// And delivery works after reopen.
	if err := m.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, _ = m.List(ctx)
	if msgs[0].State != StateDelivered {
		t.Fatalf("not delivered after reopen: %+v", msgs[0])
	}
}

func queueCrashChild() {
	dir := os.Getenv("MAILEZINE_QUEUE_CRASH_DIR")
	kv, err := store.OpenPebble(dir)
	if err != nil {
		os.Exit(2)
	}
	blob, err := store.NewFSBlob(dir + ".blobs")
	if err != nil {
		os.Exit(2)
	}
	m := New(kv, blob, &fakeDeliverer{}, DefaultOptions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = m.Submit(context.Background(), "alice@example.com", []string{"a@example.com"},
		"crash probe", strings.NewReader("Subject: crash probe\r\n\r\nbody\r\n"))
	if err != nil {
		os.Exit(2)
	}
	// Intentionally no close.
}
