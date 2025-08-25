// RFC 2971 ID extension: the server advertises ID and answers with its own
// identity, in any connection state.
package imap

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// TestIMAPIDClient drives ID with the go-imap client (both NIL and a field
// list) and checks the server identity response.
func TestIMAPIDClient(t *testing.T) {
	c, _ := startTestServer(t)
	if !c.Caps().Has(imap.CapID) {
		t.Fatal("ID capability not advertised")
	}
	data, err := c.ID(&imap.IDData{Name: "test-client", Version: "1.0"}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if data.Name != "mailezine" || data.Vendor != "mailez" {
		t.Fatalf("server ID = %+v", data)
	}
	if data2, err := c.ID(nil).Wait(); err != nil {
		t.Fatal(err)
	} else if data2.Name != "mailezine" {
		t.Fatalf("server ID (nil request) = %+v", data2)
	}
}

// TestIMAPIDBeforeLogin: ID is legal before authentication (RFC 2971).
func TestIMAPIDBeforeLogin(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:           mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob())),
		Auth:            auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory:       dir,
		MaxMessageBytes: 1 << 20,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "ID") {
		t.Fatalf("greeting missing ID capability: %s", line)
	}
	if _, err := fmt.Fprintf(conn, "x1 ID NIL\r\n"); err != nil {
		t.Fatal(err)
	}
	var untagged, tagged string
	for tagged == "" {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(l, "* ID") {
			untagged = l
		}
		if strings.HasPrefix(l, "x1 OK") {
			tagged = l
		}
	}
	if !strings.Contains(untagged, `"name" "mailezine"`) || !strings.Contains(untagged, `"vendor" "mailez"`) {
		t.Fatalf("ID response = %q", untagged)
	}
}
