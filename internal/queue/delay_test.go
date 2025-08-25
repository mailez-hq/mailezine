package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"mailezine/internal/store"
)

func TestDelayWarning(t *testing.T) {
	kv := store.NewMemoryKV()
	blob := store.NewMemoryBlob()
	m := New(kv, blob, &fakeDeliverer{err: errors.New("temporary")}, DefaultOptions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.opts.DelayWarning = time.Minute
	m.opts.PollInterval = time.Hour // manual ProcessDue only

	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	m.opts.Now = func() time.Time { return now }

	var warnings []string
	m.SetDelayWarningHandler(func(_ context.Context, from string, msg *Message, _ []byte, waited time.Duration) {
		warnings = append(warnings, from+"|"+waited.String())
	})

	id, err := m.Submit(context.Background(), "alice@example.com", []string{"bob@remote.test"}, "subj", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	// Before the threshold: no warning.
	if err := m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warning fired too early: %v", warnings)
	}
	// Past the threshold: one warning.
	now = now.Add(90 * time.Second)
	if err := m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want 1", warnings)
	}
	// Within the repeat interval: no second warning.
	now = now.Add(30 * time.Second)
	if err := m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("duplicate warning fired: %v", warnings)
	}
	// Past the repeat interval and the retry backoff: second warning.
	now = now.Add(130 * time.Second)
	if err := m.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", warnings)
	}
	// The message survives (still queued) and keeps its warning state.
	msgs, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != id || msgs[0].State != StateDeferred {
		t.Fatalf("queued message = %+v", msgs)
	}
}

func TestComposeDelayDSN(t *testing.T) {
	msg := &Message{
		ID:        1,
		From:      "alice@example.com",
		Subject:   "slow",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Recipients: []Recipient{
			{Address: "bob@remote.test", Status: RecipientPending},
			{Address: "done@remote.test", Status: RecipientDelivered},
		},
	}
	raw, err := ComposeDelayDSN("alice@example.com", msg, 10*time.Minute, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "Action: delayed") || !strings.Contains(s, "4.4.1") {
		t.Fatalf("DSN missing delayed action:\n%s", s)
	}
	if strings.Contains(s, "done@remote.test") {
		t.Fatalf("delivered recipient leaked into DSN:\n%s", s)
	}
	if strings.Contains(s, "multipart/report") || strings.Contains(s, "delivery-status") {
		// dsn.Compose may use either format; both are acceptable.
	}
}
