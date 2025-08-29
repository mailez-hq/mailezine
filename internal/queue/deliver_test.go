package queue

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
)

// testSMTPServer is a minimal in-process SMTP server used to validate the
// outbound SMTP client without network access.
type testSMTPServer struct {
	ln       net.Listener
	mu       sync.Mutex
	received [][]byte
	reject   map[string]bool
	auth     bool   // advertise AUTH PLAIN
	gotAuth  string // raw AUTH line received
}

func newTestSMTPServer(t *testing.T, reject map[string]bool) (*testSMTPServer, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSMTPServer{ln: ln, reject: reject}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s, ln.Addr().(*net.TCPAddr).Port
}

func (s *testSMTPServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *testSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}
	write("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if s.auth {
				write("250-test\r\n250-8BITMIME\r\n250-AUTH PLAIN\r\n250 SMTPUTF8")
			} else {
				write("250-test\r\n250-8BITMIME\r\n250 SMTPUTF8")
			}
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.gotAuth = line
			write("235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 2.1.0 Ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := between(line, "<", ">")
			if s.reject[strings.ToLower(addr)] {
				write("550 5.1.1 No such user")
			} else {
				write("250 2.1.5 Ok")
			}
		case upper == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var msg bytes.Buffer
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				msg.WriteString(l)
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
			}
			s.mu.Lock()
			s.received = append(s.received, msg.Bytes())
			s.mu.Unlock()
			write("250 2.0.0 Ok: queued")
		case upper == "QUIT":
			write("221 2.0.0 Bye")
			return
		case upper == "RSET":
			write("250 2.0.0 Ok")
		default:
			write("250 2.0.0 Ok")
		}
	}
}

func (s *testSMTPServer) messageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

type staticResolver struct {
	mx  map[string][]*net.MX
	ips map[string][]net.IPAddr
}

func (r *staticResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if mxs, ok := r.mx[strings.ToLower(name)]; ok {
		return mxs, nil
	}
	return nil, &net.DNSError{Err: "no mx", Name: name, IsNotFound: true}
}

func (r *staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if ips, ok := r.ips[strings.ToLower(host)]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no address", Name: host, IsNotFound: true}
}

func testDeliverer(port int, resolver MXResolver) *SMTPDeliverer {
	return &SMTPDeliverer{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hostname: "mail.mailez.test",
		Resolver: resolver,
		Port:     port,
	}
}

func TestSMTPDeliverSuccess(t *testing.T) {
	srv, port := newTestSMTPServer(t, nil)
	resolver := &staticResolver{mx: map[string][]*net.MX{
		"example.com": {{Host: "127.0.0.1."}},
	}}
	d := testDeliverer(port, resolver)
	ctx := context.Background()
	body := "Subject: hello\r\n\r\nworld\r\n"
	results, err := d.Deliver(ctx, "sender@example.com", []string{"a@example.com", "b@example.com"}, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Fatalf("delivery failed: %+v", r)
		}
	}
	if n := srv.messageCount(); n != 1 {
		t.Fatalf("server received %d messages, want 1", n)
	}
}

func TestSMTPDeliverPartialReject(t *testing.T) {
	srv, port := newTestSMTPServer(t, map[string]bool{"bad@example.com": true})
	resolver := &staticResolver{mx: map[string][]*net.MX{
		"example.com": {{Host: "127.0.0.1."}},
	}}
	d := testDeliverer(port, resolver)
	results, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"good@example.com", "bad@example.com"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	byAddr := map[string]Result{}
	for _, r := range results {
		byAddr[r.To] = r
	}
	if !byAddr["good@example.com"].OK {
		t.Fatalf("good recipient failed: %+v", byAddr["good@example.com"])
	}
	if byAddr["bad@example.com"].OK || !byAddr["bad@example.com"].Permanent {
		t.Fatalf("bad recipient should bounce permanently: %+v", byAddr["bad@example.com"])
	}
	if srv.messageCount() != 1 {
		t.Fatalf("server received %d messages, want 1 (good recipient only)", srv.messageCount())
	}
}

func TestSMTPDeliverIPFallback(t *testing.T) {
	_, port := newTestSMTPServer(t, nil)
	resolver := &staticResolver{
		ips: map[string][]net.IPAddr{
			"iponly.example.com": {{IP: net.ParseIP("127.0.0.1")}},
		},
	}
	d := testDeliverer(port, resolver)
	results, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"a@iponly.example.com"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("IP fallback delivery failed: %+v", results)
	}
}

func TestSMTPDeliverNoRoute(t *testing.T) {
	_, port := newTestSMTPServer(t, nil)
	d := testDeliverer(port, &staticResolver{})
	results, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"a@nowhere.test"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].OK || !results[0].Permanent {
		t.Fatalf("expected permanent no-route failure: %+v", results)
	}
}

// TestSMTPDeliverSmarthostAuth verifies that a smarthost delivery with
// configured credentials authenticates with SASL PLAIN. The fake smarthost
// is plaintext, so this exercises the explicit AllowPlaintextAuth opt-in
// (loopback/LAN smarthosts).
func TestSMTPDeliverSmarthostAuth(t *testing.T) {
	srv, port := newTestSMTPServer(t, nil)
	srv.auth = true

	d := &SMTPDeliverer{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hostname:           "mail.mailez.test",
		Dialer:             &net.Dialer{},
		FixedHost:          "127.0.0.1",
		FixedPort:          port,
		Username:           "relayuser",
		Password:           "relaypass",
		AllowPlaintextAuth: true,
	}
	results, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"rcpt@remote.test"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v", results)
	}
	if srv.gotAuth == "" {
		t.Fatal("smarthost delivery did not authenticate")
	}
	raw := strings.TrimPrefix(srv.gotAuth, "AUTH PLAIN ")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	if string(decoded) != "\x00relayuser\x00relaypass" {
		t.Fatalf("auth payload = %q", decoded)
	}
}

// TestSMTPDeliverSmarthostPlaintextAuthRefused: without the explicit opt-in,
// credentials must never travel over an unencrypted connection even when the
// smarthost advertises AUTH PLAIN.
func TestSMTPDeliverSmarthostPlaintextAuthRefused(t *testing.T) {
	srv, port := newTestSMTPServer(t, nil)
	srv.auth = true

	d := &SMTPDeliverer{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hostname:  "mail.mailez.test",
		Dialer:    &net.Dialer{},
		FixedHost: "127.0.0.1",
		FixedPort: port,
		Username:  "relayuser",
		Password:  "relaypass",
	}
	results, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"rcpt@remote.test"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if srv.gotAuth != "" {
		t.Fatalf("credentials sent over plaintext: %q", srv.gotAuth)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected delivery failure without plaintext AUTH: %+v", results)
	}
}

// TestSMTPDeliverNoAuthToMX: credentials must not be offered when
// delivering directly to a remote MX (no smarthost).
func TestSMTPDeliverNoAuthToMX(t *testing.T) {
	srv, port := newTestSMTPServer(t, nil)
	srv.auth = true

	d := &SMTPDeliverer{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hostname: "mail.mailez.test",
		Dialer:   &net.Dialer{},
		Resolver: &staticResolver{mx: map[string][]*net.MX{
			"remote.test": {{Host: "127.0.0.1", Pref: 10}},
		}, ips: map[string][]net.IPAddr{
			"127.0.0.1": {{IP: net.ParseIP("127.0.0.1")}},
		}},
		Port:     port,
		Username: "relayuser",
		Password: "relaypass",
	}
	if _, err := d.Deliver(context.Background(), "sender@example.com",
		[]string{"rcpt@remote.test"}, strings.NewReader("Subject: x\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	if srv.gotAuth != "" {
		t.Fatalf("direct delivery authenticated: %q", srv.gotAuth)
	}
}
