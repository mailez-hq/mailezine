package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/auth"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/mailbuffer"
	"mailezine/internal/mailstore"
	"mailezine/internal/server"
	"mailezine/internal/store"
)

type capture struct {
	mu      sync.Mutex
	from    string
	to      []string
	data    []byte
	peer    net.IP
	submits int
}

func testBackend(t *testing.T, requireAuth bool) (*Backend, *capture) {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	authSvc := auth.NewDev(map[string]string{"alice@example.com": "s3cret"})
	cap := &capture{}
	b := &Backend{
		Hostname:        "mail.mailez.test",
		Directory:       dir,
		Auth:            authSvc,
		TrustedNets:     []*net.IPNet{mustCIDR(t, "127.0.0.0/8")},
		RequireAuth:     requireAuth,
		MaxRecipients:   10,
		MaxMessageBytes: 64 * 1024,
		MaxLineLength:   1000,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: func(_ context.Context, peer net.IP, _ string, from string, to []string, data mailbuffer.Buffer) error {
			cap.mu.Lock()
			defer cap.mu.Unlock()
			cap.submits++
			raw, _ := data.ReadAll()
			cap.from, cap.to, cap.data = from, append([]string(nil), to...), raw
			cap.peer = peer
			return nil
		},
	}
	return b, cap
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func startTestServer(t *testing.T, b *Backend) *gosmtp.Client {
	t.Helper()
	srv := NewServer(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	client, err := gosmtp.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func sendMessage(t *testing.T, client *gosmtp.Client, from string, to []string, data string) error {
	t.Helper()
	if err := client.Mail(from, nil); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt, nil); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	_, werr := io.WriteString(w, data)
	cerr := w.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func TestInboundDelivery(t *testing.T) {
	b, cap := testBackend(t, false)
	client := startTestServer(t, b)

	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nhello\r\n"
	if err := sendMessage(t, client, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 1 || cap.from != "sender@remote.test" || len(cap.to) != 1 || string(cap.data) != body {
		t.Fatalf("submit: from=%q to=%v data=%q", cap.from, cap.to, cap.data)
	}
}

func TestUnknownRecipientRejected(t *testing.T) {
	b, cap := testBackend(t, false)
	client := startTestServer(t, b)
	err := sendMessage(t, client, "sender@remote.test", []string{"nobody@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil {
		t.Fatal("expected 550 for unknown recipient")
	}
	if !strings.Contains(err.Error(), "550") {
		t.Fatalf("expected 550, got %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 0 {
		t.Fatalf("message submitted despite unknown recipient")
	}
}

func TestSubmissionRequiresAuth(t *testing.T) {
	b, cap := testBackend(t, true)
	client := startTestServer(t, b)

	// Trusted peer (127.0.0.0/8) may submit without AUTH (gateway model).
	if err := sendMessage(t, client, "alice@example.com", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n"); err != nil {
		t.Fatalf("trusted peer rejected: %v", err)
	}
	cap.mu.Lock()
	submits := cap.submits
	cap.mu.Unlock()
	if submits != 1 {
		t.Fatalf("expected 1 submit, got %d", submits)
	}
}

func TestAuthPlain(t *testing.T) {
	b, cap := testBackend(t, true)
	// Remove the trusted subnet so AUTH is the only way in.
	b.TrustedNets = nil
	client := startTestServer(t, b)

	if err := client.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := sendMessage(t, client, "alice@example.com", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n"); err != nil {
		t.Fatal(err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 1 {
		t.Fatalf("expected 1 submit after AUTH, got %d", cap.submits)
	}
}

func TestAuthRejected(t *testing.T) {
	b, _ := testBackend(t, true)
	b.TrustedNets = nil
	client := startTestServer(t, b)
	if err := client.Auth(sasl.NewPlainClient("", "alice@example.com", "wrong")); err == nil {
		t.Fatal("expected auth failure")
	}
}

func TestMessageSizeLimit(t *testing.T) {
	b, cap := testBackend(t, false)
	b.MaxMessageBytes = 16
	client := startTestServer(t, b)
	// Many short lines, total far above the 16-byte limit.
	var big strings.Builder
	for i := 0; i < 100; i++ {
		big.WriteString("xxxxxxxxxxxxxxxxxxxx\r\n")
	}
	err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, "Subject: x\r\n\r\n"+big.String())
	if err == nil || !strings.Contains(err.Error(), "552") {
		t.Fatalf("expected 552 for oversized message, got %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 0 {
		t.Fatalf("oversized message submitted")
	}
}

func TestSubmitErrorReturnsTemporary(t *testing.T) {
	b, _ := testBackend(t, false)
	b.Submit = func(context.Context, net.IP, string, string, []string, mailbuffer.Buffer) error {
		return errors.New("queue full")
	}
	client := startTestServer(t, b)
	err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("expected 451 on submit error, got %v", err)
	}
}

func TestTrustedRelayAccepted(t *testing.T) {
	b, cap := testBackend(t, false)
	b.AllowRelay = true
	client := startTestServer(t, b)
	if err := sendMessage(t, client, "s@remote.test", []string{"relay@example.net"}, "Subject: x\r\n\r\nbody\r\n"); err != nil {
		t.Fatalf("trusted peer relay rejected: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.to) != 1 || cap.to[0] != "relay@example.net" {
		t.Fatalf("relay recipient not submitted: %v", cap.to)
	}
}

func TestRecipientDelimiter(t *testing.T) {
	b, cap := testBackend(t, false)
	b.RecipientDelimiter = "+"
	client := startTestServer(t, b)
	if err := sendMessage(t, client, "sender@remote.test", []string{"alice+tag@example.com"}, "Subject: plus\r\n\r\nbody\r\n"); err != nil {
		t.Fatal(err)
	}
	if cap.submits != 1 {
		t.Fatalf("expected 1 submit, got %d", cap.submits)
	}
	if len(cap.to) != 1 || cap.to[0] != "alice@example.com" {
		t.Fatalf("delimiter not stripped: %v", cap.to)
	}
}

func TestRecipientDelimiterDisabled(t *testing.T) {
	b, cap := testBackend(t, false)
	if err := sendMessage(t, startTestServer(t, b), "sender@remote.test", []string{"alice+tag@example.com"}, "Subject: plus\r\n\r\nbody\r\n"); err == nil {
		t.Fatal("unknown plus address accepted with delimiter disabled")
	}
	if cap.submits != 0 {
		t.Fatalf("message submitted despite unknown recipient")
	}
}

func TestUntrustedRelayDenied(t *testing.T) {
	b, cap := testBackend(t, false)
	b.AllowRelay = true
	b.TrustedNets = nil // no gateway, no AUTH ⇒ not trusted
	client := startTestServer(t, b)
	err := sendMessage(t, client, "s@remote.test", []string{"relay@example.net"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("expected 550 relay denied, got %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 0 {
		t.Fatalf("relay submitted without trust")
	}
}

func TestRejectMappedTo554(t *testing.T) {
	b, _ := testBackend(t, false)
	b.Submit = func(context.Context, net.IP, string, string, []string, mailbuffer.Buffer) error {
		return delivery.ErrReject
	}
	client := startTestServer(t, b)
	err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "554") {
		t.Fatalf("expected 554 for spam reject, got %v", err)
	}
}

func TestGreylistMappedTo451(t *testing.T) {
	b, _ := testBackend(t, false)
	b.Submit = func(context.Context, net.IP, string, string, []string, mailbuffer.Buffer) error {
		return delivery.ErrGreylist
	}
	client := startTestServer(t, b)
	err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("expected 451 for greylist, got %v", err)
	}
}

func TestSieveRejectMappedTo550(t *testing.T) {
	b, _ := testBackend(t, false)
	b.Submit = func(context.Context, net.IP, string, string, []string, mailbuffer.Buffer) error {
		return fmt.Errorf("%w: not for you", delivery.ErrSieveReject)
	}
	client := startTestServer(t, b)
	err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("expected 550 for sieve reject, got %v", err)
	}
}

func TestSenderRateLimitEnforced(t *testing.T) {
	b, cap := testBackend(t, false)
	// 127.0.0.0/8 is trusted, so the rate check applies.
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
		Rates: map[string]directory.SenderRate{
			"alice@example.com": {Allowed: false, Reason: "too many messages"},
		},
	})
	b.Directory = dir
	client := startTestServer(t, b)
	err := sendMessage(t, client, "alice@example.com", []string{"bob@example.com"}, "Subject: x\r\n\r\nbody\r\n")
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("expected 550 for rate limit, got %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 0 {
		t.Fatalf("rate-limited message submitted")
	}
}

func TestSRSRecipientRestored(t *testing.T) {
	b, cap := testBackend(t, false)
	srv := NewServer(b)
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
	cmd := func(line string) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\r\n", line); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(prefix string) {
		t.Helper()
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("expected %q..., got %q", prefix, line)
		}
	}
	expect("220")
	cmd("EHLO test")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}
	cmd("MAIL FROM:<sender@remote.test>")
	expect("250")
	cmd("RCPT TO:<SRS0=dev=alice=example.com@example.com>")
	expect("250")
	cmd("DATA")
	expect("354")
	cmd("Subject: forward loop\r\n\r\nbody\r\n.")
	expect("250")
	cmd("QUIT")
	expect("221")

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.to) != 1 || cap.to[0] != "alice@example.com" {
		t.Fatalf("SRS not restored: %v", cap.to)
	}
}

func TestAuthSenderIdentityEnforced(t *testing.T) {
	b, cap := testBackend(t, true)
	b.TrustedNets = nil // force AUTH
	// Spoofing: authenticated as alice but sending from bob.
	spoof := startTestServer(t, b)
	if err := spoof.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := sendMessage(t, spoof, "bob@example.com", []string{"alice@example.com"}, "Subject: x\r\n\r\nbody\r\n"); err == nil {
		t.Fatal("expected spoofed sender rejected")
	}
	cap.mu.Lock()
	if cap.submits != 0 {
		t.Fatalf("spoofed message submitted")
	}
	cap.mu.Unlock()

	// The user's own address passes.
	own := startTestServer(t, b)
	if err := own.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := sendMessage(t, own, "alice@example.com", []string{"alice@example.com"}, "Subject: ok\r\n\r\nbody\r\n"); err != nil {
		t.Fatalf("own address rejected: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 1 {
		t.Fatalf("submits = %d, want 1", cap.submits)
	}
}

func TestSMTPProxyProtocolPeerIP(t *testing.T) {
	b, cap := testBackend(t, false)
	srv := NewServer(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(server.NewProxyListener(ln, nil)) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("PROXY TCP4 203.0.113.9 127.0.0.1 12345 25\r\n")); err != nil {
		t.Fatal(err)
	}
	client := gosmtp.NewClient(conn)
	body := "Subject: via proxy\r\n\r\nbody\r\n"
	if err := sendMessage(t, client, "s@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.peer == nil || cap.peer.String() != "203.0.113.9" {
		t.Fatalf("peer IP = %v, want 203.0.113.9", cap.peer)
	}
}

// TestInboundFullPath wires the real pipeline: SMTP → recipient resolution →
// delivery into the KV mailbox store.
func TestInboundFullPath(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	kvStore := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	pipe := &delivery.Pipeline{
		Directory: dir,
		Store:     mailstore.NewKV(kvStore),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	b := &Backend{
		Hostname:        "mail.mailez.test",
		Directory:       dir,
		Auth:            auth.NewDev(map[string]string{}),
		TrustedNets:     []*net.IPNet{mustCIDR(t, "127.0.0.0/8")},
		MaxRecipients:   10,
		MaxMessageBytes: 64 * 1024,
		MaxLineLength:   1000,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Submit: func(ctx context.Context, peer net.IP, _ string, from string, to []string, data mailbuffer.Buffer) error {
			raw, err := data.ReadAll()
			if err != nil {
				return err
			}
			return pipe.Deliver(ctx, peer, from, to, raw)
		},
	}
	client := startTestServer(t, b)

	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: full path\r\n\r\nstored!\r\n"
	if err := sendMessage(t, client, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	email, err := mailstore.NewKV(kvStore).EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if email.From != "sender@remote.test" || email.Size < int64(len(body)) {
		t.Fatalf("stored email: %+v", email)
	}
}
