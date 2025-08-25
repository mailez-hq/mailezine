// FTS integration: a message delivered through the pipeline is indexed,
// IMAP SEARCH TEXT finds it via the index, and expunge removes it from the
// index. Without an index the same search still works (scan fallback).
package imap

import (
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/fts"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func startFTSIMAP(t *testing.T, ix *fts.Indexer) *imapclient.Client {
	t.Helper()
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
		FTS:             ix,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Logout().Wait() })
	if err := c.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIMAPFTSSearch(t *testing.T) {
	ix, err := fts.Open(filepath.Join(t.TempDir(), "fts"), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	c := startFTSIMAP(t, ix)

	// Append through IMAP so the append index hook runs.
	body := "From: alice@example.com\r\nTo: alice@example.com\r\nSubject: weekly report\r\n\r\ninvoice attached q3\n"
	uid := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Word/prefix search must find the message through the index.
	search := func(term string) bool {
		t.Helper()
		data, err := c.UIDSearch(&imap.SearchCriteria{Text: []string{term}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		return data.All != nil && data.All.String() != ""
	}
	if !search("invoice") {
		t.Fatal("FTS search did not find indexed message")
	}
	if !search("weekly") {
		t.Fatal("FTS search did not find subject term")
	}
	if search("missingterm") {
		t.Fatal("FTS search matched nothing")
	}

	// Expunge removes the message and its index entry.
	if _, err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatal(err)
	}
	if search("invoice") {
		t.Fatal("expunged message still searchable")
	}
}

// TestIMAPSearchScanFallback: without an index the same search works.
func TestIMAPSearchScanFallback(t *testing.T) {
	c := startFTSIMAP(t, nil)
	body := "From: a@x.test\r\nSubject: hello world\r\n\r\nneedle in haystack\n"
	appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Text: []string{"needle"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if data.All == nil || data.All.String() == "" {
		t.Fatal("scan fallback search missed")
	}
}
