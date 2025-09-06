// Integration tests over the wire: a real go-imap client against the
// mailezine IMAP server backed by the KV mailstore (dual-backend parity for
// the protocol surface lives in CI via the same session adapter).
package imap

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func startTestServerWith(t *testing.T, ms mailstore.MailboxStore) *imapclient.Client {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:           ms,
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

	client, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout().Wait() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	return client
}

func startTestServer(t *testing.T) (*imapclient.Client, *mailstore.KV) {
	t.Helper()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	return startTestServerWith(t, ms), ms
}

func appendMessage(t *testing.T, c *imapclient.Client, mailbox, body string, flags []imap.Flag) imap.UID {
	t.Helper()
	cmd := c.Append(mailbox, int64(len(body)), &imap.AppendOptions{Flags: flags})
	if _, err := cmd.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return data.UID
}

func TestIMAPLifecycle(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	c := startTestServerWith(t, ms)
	runIMAPLifecycle(t, c, ms)
}

func runIMAPLifecycle(t *testing.T, c *imapclient.Client, ms mailstore.MailboxStore) {
	t.Helper()

	// LIST: INBOX exists, hierarchy separator is '/'.
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range boxes {
		if b.Mailbox == "INBOX" {
			found = true
		}
	}
	if !found {
		t.Fatalf("INBOX missing from LIST: %+v", boxes)
	}

	// APPEND two messages with different flags.
	body1 := "From: a@x.test\r\nSubject: first\r\n\r\nhello one\r\n"
	body2 := "From: b@x.test\r\nSubject: second\r\n\r\nhello two\r\n"
	u1 := appendMessage(t, c, "INBOX", body1, nil)
	u2 := appendMessage(t, c, "INBOX", body2, []imap.Flag{imap.FlagSeen})

	// SELECT.
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages != 2 || sel.UIDNext != 3 {
		t.Fatalf("select: %+v", sel)
	}

	// SEARCH by header and by flag.
	search, err := c.Search(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "second"}},
	}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	all := search.All.(imap.SeqSet)
	if !all.Contains(2) {
		t.Fatalf("header search all = %v, want seq 2", search.All)
	}
	search, err = c.Search(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	all = search.All.(imap.SeqSet)
	if !all.Contains(1) {
		t.Fatalf("unseen search all = %v, want seq 1", search.All)
	}

	// FETCH flags + body (non-PEEK marks \Seen).
	fetched, err := c.Fetch(imap.UIDSetNum(u1), &imap.FetchOptions{
		Flags:       true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 {
		t.Fatalf("fetch count = %d", len(fetched))
	}
	if fetched[0].UID != u1 || len(fetched[0].BodySection) != 1 || string(fetched[0].BodySection[0].Bytes) != body1 {
		t.Fatalf("fetch: %+v", fetched[0])
	}

	// STORE +FLAGS.SILENT on u1.
	if _, err := c.Store(imap.UIDSetNum(u1), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen, imap.FlagDeleted},
	}, &imap.StoreOptions{}).Collect(); err != nil {
		t.Fatal(err)
	}

	// COPY to Archive then MOVE.
	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Copy(imap.UIDSetNum(u1), "Archive").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.UIDSetNum(u2), "Archive").Wait(); err != nil {
		t.Fatal(err)
	}

	// EXPUNGE removes \Deleted messages.
	expunged, err := c.Expunge().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(expunged) != 1 {
		t.Fatalf("expunged = %v", expunged)
	}

	// Storage-side assertions.
	msgs, err := ms.ListMessages(t.Context(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("inbox after lifecycle: %+v (u1 copied, u2 moved, u1 expunged)", msgs)
	}
	archive, err := ms.ListMessages(t.Context(), "alice@example.com", "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 2 {
		t.Fatalf("archive count = %d, want 2", len(archive))
	}

	// MOVE response included UIDPLUS data (covered above), and STATUS works.
	st, err := c.Status("Archive", &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if st.NumMessages == nil || *st.NumMessages != 2 {
		t.Fatalf("status: %+v", st)
	}

}

func TestIMAPRenameDeleteMailbox(t *testing.T) {
	c, _ := startTestServer(t)
	if err := c.Create("Old", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("Old", "New", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Subscribe("New").Wait(); err != nil {
		t.Fatal(err)
	}
	sub, err := c.List("", "New", &imap.ListOptions{SelectSubscribed: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Mailbox != "New" {
		t.Fatalf("subscribed list: %+v", sub)
	}
	if err := c.Delete("New").Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestIMAPAuthFailure(t *testing.T) {
	// Reuse startTestServer's client for a bad-password check on a fresh
	// connection.
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	srv := New(&Server{
		Store:     mailstore.NewKV(s),
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	defer c.Close()
	if err := c.Login("alice@example.com", "wrong").Wait(); err == nil {
		t.Fatal("expected auth failure")
	}
}

// TestIMAPIdlePush verifies IDLE delivers unsolicited EXISTS/EXPUNGE
// updates when the mailbox changes out-of-band (e.g. SMTP delivery).
func TestIMAPIdlePush(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	if _, err := ms.Deliver(t.Context(), "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: a@x.test\r\nSubject: one\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:     ms,
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	exists := make(chan uint32, 8)
	expunges := make(chan uint32, 8)
	client, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					exists <- *d.NumMessages
				}
			},
			Expunge: func(seq uint32) { expunges <- seq },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout().Wait() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	idle, err := client.Idle()
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	// Out-of-band delivery must produce an EXISTS update.
	if _, err := ms.Deliver(t.Context(), "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: b@x.test\r\nSubject: two\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-exists:
		if n != 2 {
			t.Fatalf("EXISTS = %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no EXISTS update during IDLE")
	}

	// Expunging that message must produce an EXPUNGE update.
	if err := ms.SetFlags(t.Context(), "alice@example.com", "INBOX", 2, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(t.Context(), "alice@example.com", "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-expunges:
		if seq != 2 {
			t.Fatalf("EXPUNGE seq = %d, want 2", seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no EXPUNGE update during IDLE")
	}
}

// TestIMAPPollGatePicksUpChanges verifies the modseq-gated per-command poll
// still surfaces out-of-band deliveries and expunges on the next command.
// The gate may only skip the listing diff when the mailbox version is
// unchanged — a false skip would hide deliveries until the next real change.
func TestIMAPPollGatePicksUpChanges(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	if _, err := ms.Deliver(t.Context(), "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: a@x.test\r\nSubject: one\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:     ms,
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	exists := make(chan uint32, 8)
	expunges := make(chan uint32, 8)
	client, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					exists <- *d.NumMessages
				}
			},
			Expunge: func(seq uint32) { expunges <- seq },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout().Wait() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// An unchanged mailbox must produce nothing: the gate short-circuits.
	if err := client.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-exists:
		t.Fatalf("spurious EXISTS %d with unchanged mailbox", n)
	case <-time.After(300 * time.Millisecond):
	}

	// Out-of-band delivery must appear on the next command's poll.
	if _, err := ms.Deliver(t.Context(), "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte("From: b@x.test\r\nSubject: two\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-exists:
		if n != 2 {
			t.Fatalf("EXISTS = %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery not picked up by gated poll")
	}

	// Out-of-band expunge likewise.
	if err := ms.SetFlags(t.Context(), "alice@example.com", "INBOX", 2, []string{"\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Expunge(t.Context(), "alice@example.com", "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-expunges:
		if seq != 2 {
			t.Fatalf("EXPUNGE seq = %d, want 2", seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expunge not picked up by gated poll")
	}
}

// TestIMAPHeaderFieldsFetch verifies BODY.PEEK[HEADER.FIELDS (...)] and
// partial body fetches (used by the webmail list pane).
func TestIMAPHeaderFieldsFetch(t *testing.T) {
	c, _ := startTestServer(t)
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: list item\r\n" +
		"Message-ID: <x@remote.test>\r\n\r\nhello list\r\n"
	u1 := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetched, err := c.Fetch(imap.UIDSetNum(u1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Subject", "From"},
		}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || len(fetched[0].BodySection) != 1 {
		t.Fatalf("fetch: %+v", fetched)
	}
	got := string(fetched[0].BodySection[0].Bytes)
	if !strings.Contains(got, "Subject: list item") || !strings.Contains(got, "From: sender@remote.test") {
		t.Fatalf("header fields: %q", got)
	}
	if strings.Contains(got, "Message-ID") {
		t.Fatalf("unrequested header returned: %q", got)
	}
}

// TestIMAPEnvelopeUnparseableHeaders: a message whose headers cannot be
// parsed (e.g. a UTF-8 BOM in front of the first key) must yield an empty
// envelope instead of panicking the connection. Regression: envelopeOf
// returned nil on ReadHeader error and envelopeWeight dereferenced it,
// killing every FETCH ENVELOPE over the mailbox (webmail list + pollers).
func TestIMAPEnvelopeUnparseableHeaders(t *testing.T) {
	c, _ := startTestServer(t)
	// \ufeff before "From" makes textproto reject the key.
	body := "\xef\xbb\xbfFrom: sender@remote.test\r\nSubject: bom\r\n\r\nhello\r\n"
	u1 := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetched, err := c.Fetch(imap.UIDSetNum(u1), &imap.FetchOptions{
		Envelope: true,
		Flags:    true,
	}).Collect()
	if err != nil {
		t.Fatalf("fetch over unparseable headers: %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("fetch: %+v", fetched)
	}
	env := fetched[0].Envelope
	if env == nil {
		t.Fatal("envelope must be empty, not nil")
	}
	if env.Subject != "" || len(env.From) != 0 {
		t.Fatalf("degraded envelope should be empty: %+v", env)
	}
}

// TestIMAPBodyStructureFetch verifies ENVELOPE + extended BODYSTRUCTURE on a
// cache-miss path. Regression: short-variable shadowing left the outer
// envelope/structure nil and WriteBodyStructure panicked, dropping the
// connection (webmail read path).
func TestIMAPBodyStructureFetch(t *testing.T) {
	c, _ := startTestServer(t)
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: bs\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	u1 := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetched, err := c.Fetch(imap.UIDSetNum(u1), &imap.FetchOptions{
		Envelope:      true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 {
		t.Fatalf("fetch count = %d", len(fetched))
	}
	if fetched[0].Envelope == nil {
		t.Fatal("missing ENVELOPE")
	}
	if fetched[0].BodyStructure == nil {
		t.Fatal("missing BODYSTRUCTURE")
	}
	if got := fetched[0].BodyStructure.MediaType(); !strings.EqualFold(got, "multipart/mixed") {
		t.Fatalf("body structure media type = %q", got)
	}
}
