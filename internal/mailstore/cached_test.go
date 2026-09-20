package mailstore

import (
	"context"
	"slices"
	"testing"

	"mailezine/internal/mailcache"
)

func newTestCached(t *testing.T) (*Cached, *KV) {
	t.Helper()
	kv, _ := newTestKV(t)
	return NewCached(kv, mailcache.NewCache(1<<20)), kv
}

// TestCachedListMessagesNoAlias covers the memo holding a private copy: a
// caller marking a listed message \Seen must not change what the next reader
// gets.
func TestCachedListMessagesNoAlias(t *testing.T) {
	c, _ := newTestCached(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	if _, err := c.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	first, err := c.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	first[0].Flags = append(first[0].Flags, "\\Seen")

	second, err := c.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("messages = %d, want 1", len(second))
	}
	if slices.Contains(second[0].Flags, "\\Seen") {
		t.Fatal("caller mutation of a listed message leaked into the cached snapshot")
	}
}

// TestCachedMessageByUIDStaleMemo covers a memo that predates a delivery made
// through the inner store: the lookup still finds the new UID.
func TestCachedMessageByUIDStaleMemo(t *testing.T) {
	c, kv := newTestCached(t)
	ctx := context.Background()
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"

	if _, err := c.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListMessages(ctx, "alice@example.com", "INBOX"); err != nil {
		t.Fatal(err)
	}

	uid, err := kv.Deliver(ctx, "alice@example.com", "INBOX", &Message{From: "s@remote.test", Data: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MessageByUID(ctx, "alice@example.com", "INBOX", uid); err != nil {
		t.Fatalf("uid %d behind a stale memo: %v", uid, err)
	}
}
