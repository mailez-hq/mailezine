// Client matrix: drive mailezine's IMAP server with go-imap's client (an
// independent, strict implementation from the emersion mail stack). This
// is the interop half of the client matrix — commands a real mail client
// (desktop/mobile) issues must parse and answer with this
// client's wire expectations.
package imap

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func startGoImapClient(t *testing.T) *client.Client {
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
	t.Cleanup(func() { _ = conn.Close() })
	c, err := client.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Logout() })
	if err := c.Login("alice@example.com", "s3cret"); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestGoImapClientLifecycle runs the mailbox management + message flow every
// real client performs: list, create, append, select, search, flag, copy,
// move, expunge, rename, delete.
func TestGoImapClientLifecycle(t *testing.T) {
	c := startGoImapClient(t)

	if err := c.Create("Projects"); err != nil {
		t.Fatalf("create: %v", err)
	}
	listCh := make(chan *imap.MailboxInfo, 10)
	if err := c.List("", "*", listCh); err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for info := range listCh {
		if info.Name == "Projects" {
			found = true
		}
	}
	if !found {
		t.Fatal("Projects missing from LIST")
	}

	msg1 := `From: a@x.test` + "\r\n" + `Subject: one` + "\r\n\r\n" + `body one` + "\r\n"
	msg2 := `From: b@x.test` + "\r\n" + `Subject: two` + "\r\n\r\n" + `body two` + "\r\n"
	if err := c.Append("INBOX", nil, time.Now(), bytes.NewBufferString(msg1)); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := c.Append("INBOX", nil, time.Now(), bytes.NewBufferString(msg2)); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatalf("select: %v", err)
	}
	status, err := c.Status("INBOX", []imap.StatusItem{imap.StatusMessages, imap.StatusUnseen})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Messages != 2 || status.Unseen != 2 {
		t.Fatalf("status = %d msgs %d unseen, want 2/2", status.Messages, status.Unseen)
	}

	// UID SEARCH finds the subject (rendered as SUBJECT "one" on the wire).
	criteria := imap.NewSearchCriteria()
	criteria.Header.Set("Subject", "one")
	uids, err := c.UidSearch(criteria)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !containsUID(uids, 1) {
		t.Fatalf("search result missing UID 1: %v", uids)
	}

	// UID STORE marks \Seen.
	seqset := new(imap.SeqSet)
	seqset.AddNum(1)
	storeCh := make(chan *imap.Message, 1)
	if err := c.UidStore(seqset, imap.AddFlags, []interface{}{imap.SeenFlag}, storeCh); err != nil {
		t.Fatalf("store: %v", err)
	}
	<-storeCh
	status, err = c.Status("INBOX", []imap.StatusItem{imap.StatusUnseen})
	if err != nil {
		t.Fatalf("status after store: %v", err)
	}
	if status.Unseen != 1 {
		t.Fatalf("unseen after store = %d, want 1", status.Unseen)
	}

	// UID COPY and UID MOVE.
	if err := c.UidCopy(seqset, "Projects"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	seqset2 := new(imap.SeqSet)
	seqset2.AddNum(2)
	if err := c.UidMove(seqset2, "Projects"); err != nil {
		t.Fatalf("move: %v", err)
	}
	status, err = c.Status("Projects", []imap.StatusItem{imap.StatusMessages})
	if err != nil {
		t.Fatalf("status projects: %v", err)
	}
	if status.Messages != 2 {
		t.Fatalf("projects msgs = %d, want 2", status.Messages)
	}

	// Expunge the deleted (moved) message.
	expungeCh := make(chan uint32, 10)
	if err := c.Expunge(expungeCh); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	status, err = c.Status("INBOX", []imap.StatusItem{imap.StatusMessages})
	if err != nil {
		t.Fatalf("status inbox: %v", err)
	}
	if status.Messages != 1 {
		t.Fatalf("inbox msgs = %d, want 1", status.Messages)
	}

	// Rename and delete.
	if err := c.Rename("Projects", "Archive"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := c.Delete("Archive"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestGoImapClientFetch drives FETCH over the high-level API, as the client
// matrix needs the full envelope/body/flag response shapes.
func TestGoImapClientFetch(t *testing.T) {
	c := startGoImapClient(t)
	msg := `From: "Alice" <alice@example.com>` + "\r\n" +
		`To: bob@example.com` + "\r\n" +
		`Subject: hello from go-imap` + "\r\n" +
		`Message-ID: <goimap@x.test>` + "\r\n\r\n" +
		`the body` + "\r\n"
	if err := c.Append("INBOX", nil, time.Now(), bytes.NewBufferString(msg)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(1)
	fetchCh := make(chan *imap.Message, 1)
	items := []imap.FetchItem{
		imap.FetchFlags,
		imap.FetchRFC822Size,
		"BODY.PEEK[HEADER.FIELDS (SUBJECT)]",
	}
	if err := c.UidFetch(seqset, items, fetchCh); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	fetched := <-fetchCh
	if len(fetched.Flags) != 0 {
		t.Fatalf("flags = %v, want empty", fetched.Flags)
	}
	if fetched.Size != uint32(len(msg)) {
		t.Fatalf("rfc822.size = %d, want %d", fetched.Size, len(msg))
	}
	header := ""
	for section, lit := range fetched.Body {
		if section.Specifier == imap.HeaderSpecifier {
			want := false
			for _, f := range section.Fields {
				if strings.EqualFold(f, "SUBJECT") {
					want = true
				}
			}
			if !want {
				continue
			}
			b, err := io.ReadAll(lit)
			if err != nil {
				t.Fatal(err)
			}
			header = string(b)
		}
	}
	if !strings.Contains(header, "hello from go-imap") {
		t.Fatalf("header fetch missing subject: %q", header)
	}
}

func containsUID(uids []uint32, want uint32) bool {
	for _, u := range uids {
		if u == want {
			return true
		}
	}
	return false
}
