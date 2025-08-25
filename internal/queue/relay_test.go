package queue

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"mailezine/internal/directory"
)

func TestParseRelayTransport(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
		ok   bool
	}{
		{"smtp:[10.0.0.5]:2525", "10.0.0.5", 2525, true},
		{"smtp:[smtp.example.net]", "smtp.example.net", 0, true},
		{"smtp:example.net", "", 0, false}, // MX-of-host: fallback
		{"lmtp:[127.0.0.1]:2525", "", 0, false},
		{"smtp:[]", "", 0, false},
		{"bogus", "", 0, false},
	}
	for _, tc := range cases {
		host, port, ok := parseRelayTransport(tc.in)
		if host != tc.host || port != tc.port || ok != tc.ok {
			t.Errorf("parseRelayTransport(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tc.in, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}

// recordingDeliverer records the recipients it receives, grouped per call.
type recordingDeliverer struct {
	mu        sync.Mutex
	from      string
	recipient []string
	calls     int
}

func (r *recordingDeliverer) Deliver(_ context.Context, from string, to []string, msg io.Reader) ([]Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.from = from
	r.recipient = append(r.recipient, to...)
	_, _ = io.Copy(io.Discard, msg)
	out := make([]Result, 0, len(to))
	for _, addr := range to {
		out = append(out, Result{To: addr, OK: true})
	}
	return out, nil
}

func (r *recordingDeliverer) got() (string, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.from, append([]string(nil), r.recipient...), r.calls
}

func TestRelayDelivererSplitsByTransport(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
		Relays: map[string]directory.Relay{
			"relay.example.net": {Domain: "relay.example.net", Transport: "smtp:[127.0.0.1]:2525"},
		},
	})
	direct := &recordingDeliverer{}
	fixed := &recordingDeliverer{}
	d := &RelayDeliverer{
		Directory: dir,
		Direct:    direct,
		NewFixed: func(host string, port int) Deliverer {
			if host != "127.0.0.1" || port != 2525 {
				t.Fatalf("NewFixed(%q,%d)", host, port)
			}
			return fixed
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	results, err := d.Deliver(context.Background(), "alice@example.com",
		[]string{"a@example.com", "b@relay.example.net", "c@example.com"},
		strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	_, directTo, _ := direct.got()
	if len(directTo) != 2 || directTo[0] != "a@example.com" || directTo[1] != "c@example.com" {
		t.Fatalf("direct recipients: %v", directTo)
	}
	_, fixedTo, fixedCalls := fixed.got()
	if fixedCalls != 1 || len(fixedTo) != 1 || fixedTo[0] != "b@relay.example.net" {
		t.Fatalf("fixed recipients: %v calls=%d", fixedTo, fixedCalls)
	}
}

func TestRelayDelivererFallsBackOnUnsupportedTransport(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Domains: []string{"example.com"},
		Relays: map[string]directory.Relay{
			"mx.example.net": {Domain: "mx.example.net", Transport: "smtp:mx.example.net"},
		},
	})
	direct := &recordingDeliverer{}
	d := &RelayDeliverer{
		Directory: dir,
		Direct:    direct,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := d.Deliver(context.Background(), "s@example.com",
		[]string{"x@mx.example.net"}, strings.NewReader("body")); err != nil {
		t.Fatal(err)
	}
	_, to, calls := direct.got()
	if calls != 1 || len(to) != 1 || to[0] != "x@mx.example.net" {
		t.Fatalf("expected direct fallback: %v calls=%d", to, calls)
	}
}
