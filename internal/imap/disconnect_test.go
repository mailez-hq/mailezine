package imap

import (
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// TestDisconnectDropsLiveSessions covers the containment path: after the
// control plane revokes access, the account's open IMAP connections end.
func TestDisconnectDropsLiveSessions(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	core := &Server{
		Store:           mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob())),
		Auth:            auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory:       dir,
		MaxMessageBytes: 1 << 20,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := New(core)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	client, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("authenticated session should work: %v", err)
	}

	if n := core.Disconnect("nobody@example.com"); n != 0 {
		t.Fatalf("disconnect of an unknown account dropped %d sessions", n)
	}
	if n := core.Disconnect("alice@example.com"); n != 1 {
		t.Fatalf("disconnect dropped %d sessions, want 1", n)
	}
	if err := client.Noop().Wait(); err == nil {
		t.Fatal("session survived the disconnect")
	}
	if n := core.Disconnect("alice@example.com"); n != 0 {
		t.Fatalf("second disconnect dropped %d sessions, want 0", n)
	}
}
