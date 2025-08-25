package mailstore

import (
	"context"
	"testing"

	"mailezine/internal/store"
)

func defaultMailboxesSuite(t *testing.T, newStore func(*testing.T) MailboxStore) {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	boxes, err := s.ListMailboxes(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Mailbox{}
	for _, b := range boxes {
		byName[b.Name] = b
	}
	for name, attr := range DefaultMailboxes {
		mb, ok := byName[name]
		if !ok {
			t.Fatalf("default mailbox %s missing after first LIST", name)
		}
		if !mb.Subscribed {
			t.Fatalf("%s not subscribed", name)
		}
		if !HasFlag(mb.Attrs, attr) {
			t.Fatalf("%s missing special-use %s: %v", name, attr, mb.Attrs)
		}
	}
	// Idempotent: a second LIST must not error and keeps the same boxes.
	boxes2, err := s.ListMailboxes(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes2) != len(boxes) {
		t.Fatalf("mailbox count changed on second LIST: %d -> %d", len(boxes), len(boxes2))
	}
	// Drafts is usable for APPEND right away (webmail save-draft path).
	if _, err := s.Append(ctx, "alice@example.com", "Drafts", &Message{
		Data: []byte("Subject: draft\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("append to auto-created Drafts: %v", err)
	}
}

func TestDefaultMailboxesKV(t *testing.T) {
	defaultMailboxesSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	})
}
