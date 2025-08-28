package delivery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailezine/internal/directory"
	"mailezine/internal/fts"
	"mailezine/internal/mailstore"
	"mailezine/internal/sieve"
	"mailezine/internal/spam"
	"mailezine/internal/store"
)

func TestNormalizeMailboxName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"INBOX", "INBOX"},
		{"Inbox", "INBOX"},
		{"inbox", "INBOX"},
		{"Inbox/Sub", "INBOX/Sub"},
		{"INBOX/Sub", "INBOX/Sub"},
		{"inbox/Sub/Folder", "INBOX/Sub/Folder"},
		// Literal folder names that only share a prefix must stay untouched.
		{"Inboxx", "Inboxx"},
		{"Inboxx/Sub", "Inboxx/Sub"},
		{"Sent", "Sent"},
		{"Projects/Inbox", "Projects/Inbox"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeMailboxName(c.in); got != c.want {
			t.Errorf("normalizeMailboxName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func newTestPipeline(t *testing.T, quotaLimit int64) (*Pipeline, *mailstore.KV, *directory.Dev) {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: quotaLimit},
			"bob@example.com":   {Email: "bob@example.com", Enabled: true, QuotaBytes: quotaLimit},
		},
		Domains: []string{"example.com"},
		Aliases: map[string][]string{
			"team@example.com": {"alice@example.com", "bob@example.com"},
		},
	})
	kv := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(kv)
	p := &Pipeline{
		Directory: dir,
		Store:     ms,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return p, ms, dir
}

func TestDeliverToUser(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if email.From != "sender@remote.test" || email.Mailbox != "INBOX" {
		t.Fatalf("email: %+v", email)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	got := blob.String()
	if !strings.HasPrefix(got, "Received: from ") || !strings.HasSuffix(got, body) {
		t.Fatalf("blob missing Received trace: %q", got)
	}
}

func TestDeliverExpandsAlias(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	body := "Subject: to the team\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"team@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ms.EmailByUID(ctx, "bob@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.UID != 1 || b.UID != 1 {
		t.Fatalf("uids: alice=%d bob=%d", a.UID, b.UID)
	}
}

func TestDeliverToDelimitedAddress(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.RecipientDelimiter = "+"
	body := "Subject: plus\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice+tag@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatalf("mail did not land in base user INBOX: %v", err)
	}
	if email.Mailbox != "INBOX" || email.From != "sender@remote.test" {
		t.Fatalf("email: %+v", email)
	}
}

func TestDeliverQuotaEnforced(t *testing.T) {
	p, _, _ := newTestPipeline(t, 4)
	body := "Subject: big\r\n\r\n0123456789\r\n"
	err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice@example.com"}, []byte(body))
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("expected ErrQuota, got %v", err)
	}
}

func TestDeliverUnknownTarget(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"nobody@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n"))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestQuotaWriteBack(t *testing.T) {
	p, _, dir := newTestPipeline(t, 1<<20)
	body := "Subject: x\r\n\r\n0123456789\r\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	u, err := dir.User(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.QuotaBytesUsed == 0 {
		t.Fatalf("quota not written back: %+v", u)
	}
}

// stubClassifier returns a canned classification for the pipeline tests.
type stubClassifier struct {
	result spam.Result
	err    error
}

func (s stubClassifier) Classify(context.Context, net.IP, string, []string, []byte) (spam.Result, error) {
	return s.result, s.err
}

func TestDeliverRejectedBySpam(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	p.Classifier = stubClassifier{result: spam.Result{Action: "reject"}}
	err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"},
		[]byte("Subject: x\r\n\r\nbody\r\n"))
	if !errors.Is(err, ErrReject) {
		t.Fatalf("expected ErrReject, got %v", err)
	}
}

func TestDeliverGreylisted(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	p.Classifier = stubClassifier{result: spam.Result{Action: "greylist"}}
	err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"},
		[]byte("Subject: x\r\n\r\nbody\r\n"))
	if !errors.Is(err, ErrGreylist) {
		t.Fatalf("expected ErrGreylist, got %v", err)
	}
}

func TestDeliverAddsSpamHeaders(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Classifier = stubClassifier{result: spam.Result{
		Action:  "add header",
		Score:   5.2,
		Headers: []string{"X-Spam-Flag: YES", "X-Spam-Score: 5.2"},
	}}
	body := "From: s@remote.test\r\nSubject: x\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	got := blob.String()
	if !strings.HasPrefix(got, "Received: from ") ||
		!strings.Contains(got, "X-Spam-Flag: YES\r\nX-Spam-Score: 5.2\r\n") ||
		!strings.HasSuffix(got, body) {
		t.Fatalf("stored message: %q", got)
	}
}

func TestDeliverClassifierFailOpen(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Classifier = stubClassifier{err: errors.New("rspamd down")}
	body := "Subject: x\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatalf("classifier outage must not stop delivery: %v", err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1); err != nil {
		t.Fatal(err)
	}
}

// TestDeliverTypedNilClassifier guards the typed-nil trap: a nil *spam.Client
// stored in the interface must behave like no classifier, not panic.
func TestDeliverTypedNilClassifier(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	var c *spam.Client
	p.Classifier = c
	body := "Subject: x\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1); err != nil {
		t.Fatal(err)
	}
}

func TestDeliverInjectsMessageID(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	body := "From: s@x.test\r\nSubject: no mid\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	got := blob.String()
	if !strings.Contains(got, "Message-ID: <") {
		t.Fatalf("Message-ID not injected: %q", got)
	}
	if strings.Count(got, "Message-ID:") != 1 {
		t.Fatalf("Message-ID duplicated: %q", got)
	}
}

func TestDeliverKeepsExistingMessageID(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	body := "From: s@x.test\r\nMessage-ID: <keep-me@x.test>\r\nSubject: x\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob.String(), "<keep-me@x.test>") {
		t.Fatalf("existing Message-ID lost: %q", blob.String())
	}
}

func TestDeliverSieveRoutesToJunk(t *testing.T) {
	p, ms, dir := newTestPipeline(t, 1<<20)
	dir2 := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{"alice@example.com": `require "fileinto";
if header :contains "Subject" "spam" { fileinto "Junk"; }`,
		},
	})
	p.Directory = dir2
	_ = dir
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	spamBody := "From: s@x.test\r\nSubject: buy spam now\r\n\r\nbody\r\n"
	if err := p.Deliver(ctx, nil, "s@x.test", []string{"alice@example.com"}, []byte(spamBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "Junk", 1); err != nil {
		t.Fatalf("spam not filed into Junk: %v", err)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spam leaked into INBOX: %v", err)
	}

	okBody := "From: s@x.test\r\nSubject: hello\r\n\r\nbody\r\n"
	if err := p.Deliver(ctx, nil, "s@x.test", []string{"alice@example.com"}, []byte(okBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("normal mail not kept in INBOX: %v", err)
	}
}

func TestDeliverSieveDiscard(t *testing.T) {
	p, ms, dir := newTestPipeline(t, 1<<20)
	dir2 := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `if header :is "From" "bounce@x.test" { discard; }`,
		},
	})
	p.Directory = dir2
	_ = dir
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	body := "From: bounce@x.test\r\nSubject: undeliverable\r\n\r\nbody\r\n"
	if err := p.Deliver(ctx, nil, "bounce@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("discarded message stored: %v", err)
	}
}

// TestDeliverLocalSieveOverridesDirectory pins the precedence: a locally
// stored active script (ManageSieve) wins over the control plane default.
func TestDeliverLocalSieveOverridesDirectory(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `require "fileinto";
if true { fileinto "DirBox"; }`,
		},
	})
	// Directory says DirBox; the local active script says Junk.
	if err := ms.PutSieveScript(context.Background(), "alice@example.com", "rules",
		`require "fileinto";
if true { fileinto "Junk"; }`, true); err != nil {
		t.Fatal(err)
	}
	p.ScriptSource = sieve.DefaultScriptSource{Store: ms, Directory: dir}

	body := "From: s@x.test\r\nSubject: news\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "Junk", 1); err != nil {
		t.Fatalf("local script not applied: %v", err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "DirBox", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("directory script used despite local active script: %v", err)
	}
}

func TestDeliverSieveRedirect(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `if header :contains "Subject" "fwd" { redirect "forward@remote.test"; keep; }`,
		},
	})
	p.Directory = dir
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var redirected []string
	p.Redirect = func(_ context.Context, _, to string, _ []byte) error {
		redirected = append(redirected, to)
		return nil
	}
	ctx := context.Background()
	body := "From: s@x.test\r\nSubject: fwd me\r\n\r\nbody\r\n"
	if err := p.Deliver(ctx, nil, "s@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if len(redirected) != 1 || redirected[0] != "forward@remote.test" {
		t.Fatalf("redirected: %v", redirected)
	}
	// keep preserved the local INBOX copy alongside the redirect.
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("local copy missing: %v", err)
	}
}

func TestDeliverSieveRedirectWithoutQueue(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `redirect "forward@remote.test";`,
		},
	})
	p.Directory = dir
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	body := "From: s@x.test\r\nSubject: fwd\r\n\r\nbody\r\n"
	if err := p.Deliver(ctx, nil, "s@x.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	// Without a Redirect hook the redirect is skipped, not fatal, and the
	// local copy (from keep semantics) still lands.
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("local copy missing: %v", err)
	}
}

func TestDeliverSieveReject(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `require "reject";
if header :contains "Subject" "vip" { reject "not for you"; }`,
		},
	})
	p.Directory = dir
	p.Sieve = sieve.NewEngine(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	body := "From: s@x.test\r\nSubject: vip stuff\r\n\r\nbody\r\n"
	err := p.Deliver(ctx, nil, "s@x.test", []string{"alice@example.com"}, []byte(body))
	if !errors.Is(err, ErrSieveReject) {
		t.Fatalf("expected ErrSieveReject, got %v", err)
	}
	if _, err := ms.EmailByUID(ctx, "alice@example.com", "INBOX", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected message stored: %v", err)
	}
}

// TestDeliverSieveEditHeader verifies RFC 5293 deleteheader/addheader are
// applied to the stored copy (the mailez default template rewrites
// Delivered-To from the Received trace).
func TestDeliverSieveEditHeader(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Sieve = sieve.NewEngine(nil)
	script := `require "editheader";
require "variables";
require "index";
if header :index 2 :matches "Received" "from * by * for <*>; *"
{
  deleteheader "Delivered-To";
  addheader "Delivered-To" "<${3}>";
}
`
	p.ScriptSource = sieve.StaticSource(script)
	body := "Received: from mx (10.0.0.1) by mail (10.0.0.2) for <alice@example.com>; Mon, 1 Jan 2026 10:00:00 +0000\r\n" +
		"Received: from gateway (10.0.0.9) by mail (10.0.0.2) for <alice@example.com>; Mon, 1 Jan 2026 10:00:01 +0000\r\n" +
		"Delivered-To: old@example.com\r\n" +
		"Subject: hi\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	got := blob.String()
	if strings.Contains(got, "Delivered-To: old@example.com") {
		t.Fatalf("old Delivered-To not deleted:\n%s", got)
	}
	if !strings.Contains(got, "Delivered-To: <alice@example.com>") {
		t.Fatalf("new Delivered-To not added:\n%s", got)
	}
}

// TestDeliverVacation verifies the auto-reply is submitted with a null
// envelope sender and that an auto-replied inbound message never triggers a
// second reply.
func TestDeliverVacation(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Sieve = sieve.NewEngine(nil)
	p.ScriptSource = sieve.StaticSource(`require "vacation"; vacation :days 1 :subject "away" "on holiday";`)
	var outbound [][]byte
	p.Redirect = func(_ context.Context, from, _ string, data []byte) error {
		if from != "" {
			t.Fatalf("vacation envelope sender = %q, want null", from)
		}
		outbound = append(outbound, data)
		return nil
	}
	body := "From: friend@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := p.Deliver(context.Background(), nil, "friend@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if len(outbound) != 1 {
		t.Fatalf("vacation replies = %d, want 1", len(outbound))
	}
	if !strings.Contains(string(outbound[0]), "Auto-Submitted: auto-replied") ||
		!strings.Contains(string(outbound[0]), "on holiday") {
		t.Fatalf("vacation reply malformed:\n%s", outbound[0])
	}
	// The local copy is still stored.
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1); err != nil {
		t.Fatal(err)
	}
	// Throttle: a second message from the same sender within :days does not
	// produce another reply.
	if err := p.Deliver(context.Background(), nil, "friend@remote.test", []string{"alice@example.com"}, []byte("From: friend@remote.test\r\nSubject: two\r\n\r\nagain\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(outbound) != 1 {
		t.Fatalf("vacation replies after throttle = %d, want 1", len(outbound))
	}
}

// TestDeliverNoAutoReplyLoop: a message that already carries Auto-Submitted
// must never trigger vacation.
func TestDeliverNoAutoReplyLoop(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	p.Sieve = sieve.NewEngine(nil)
	p.ScriptSource = sieve.StaticSource(`require "vacation"; vacation :days 1 "away";`)
	called := false
	p.Redirect = func(context.Context, string, string, []byte) error {
		called = true
		return nil
	}
	body := "From: friend@remote.test\r\nAuto-Submitted: auto-replied\r\nSubject: loop\r\n\r\nx\r\n"
	if err := p.Deliver(context.Background(), nil, "friend@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("vacation fired for an auto-replied message")
	}
}

// TestDeliverSpamLevelFallback verifies X-Spam-Level is synthesized when the
// classifier did not provide it, so spamtest keeps working.
func TestDeliverSpamLevelFallback(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)
	p.Classifier = stubClassifier{result: spam.Result{Action: "add header", Score: 13, Headers: []string{"X-Spam-Flag: YES"}}}
	body := "From: s@remote.test\r\nSubject: spam\r\n\r\nbody\r\n"
	if err := p.Deliver(context.Background(), nil, "s@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &blob); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob.String(), "X-Spam-Level: *************") {
		t.Fatalf("X-Spam-Level not synthesized:\n%s", blob.String())
	}
}

// TestDeliverFTSIndex: messages delivered through the pipeline are indexed,
// and the index is queryable for the body text.
func TestDeliverFTSIndex(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	ix, err := fts.Open(filepath.Join(t.TempDir(), "fts"), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	p.FTS = ix

	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: fts delivery\r\n\r\nquarterly invoice numbers\n"
	if err := p.Deliver(context.Background(), nil, "sender@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := ix.SearchText(context.Background(), "alice@example.com", "INBOX", []string{"quarterly"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered message not indexed: %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDeliverVacationPersistentThrottle: the :days throttle survives a
// fresh pipeline (a restart) because it is stored in the mailstore.
func TestDeliverVacationPersistentThrottle(t *testing.T) {
	kv := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(kv)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	newPipeline := func() *Pipeline {
		return &Pipeline{
			Directory:    dir,
			Store:        ms,
			Sieve:        sieve.NewEngine(nil),
			ScriptSource: sieve.StaticSource(`require "vacation"; vacation :days 1 "away";`),
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	var replies int
	newPipeline().Redirect = func(context.Context, string, string, []byte) error { replies++; return nil }

	// First delivery sends a reply; a second pipeline (restart) remembers
	// the throttle and does not reply again.
	p1 := newPipeline()
	p1.Redirect = func(context.Context, string, string, []byte) error { replies++; return nil }
	body := "From: friend@remote.test\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := p1.Deliver(context.Background(), nil, "friend@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	p2 := newPipeline()
	p2.Redirect = func(context.Context, string, string, []byte) error { replies++; return nil }
	if err := p2.Deliver(context.Background(), nil, "friend@remote.test", []string{"alice@example.com"}, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Fatalf("vacation replies = %d, want 1 (throttle must survive restart)", replies)
	}
}
