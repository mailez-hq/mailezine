package mailsmtp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"mailezine/internal/maildns"
)

// mockServer is a scripted SMTP server for wire-level tests.
type mockServer struct {
	ln       net.Listener
	startTLS bool
	auth     bool

	mu       sync.Mutex
	authLine string
	msgCount int
}

func newMockServer(t *testing.T, startTLS, auth bool) (addr string, s *mockServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s = &mockServer{ln: ln, startTLS: startTLS, auth: auth}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), s
}

func (s *mockServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *mockServer) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}
	w("220 mock ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if s.startTLS {
				w("250-mock\r\n250 STARTTLS")
			} else if s.auth {
				w("250-mock\r\n250 AUTH PLAIN")
			} else {
				w("250 mock")
			}
		case upper == "STARTTLS":
			w("220 2.0.0 Ready to start TLS")
			tlsConn := tls.Server(conn, &tls.Config{GetCertificate: mockCertificate})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			// Continue the SMTP session over TLS: reader/writer now TLS.
			tr := bufio.NewReader(tlsConn)
			tw := func(format string, args ...any) {
				_, _ = fmt.Fprintf(tlsConn, format+"\r\n", args...)
			}
			for {
				line, err := tr.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				upper := strings.ToUpper(line)
				switch {
				case strings.HasPrefix(upper, "EHLO"):
					tw("250 mock-tls")
				case strings.HasPrefix(upper, "AUTH PLAIN"):
					s.mu.Lock()
					s.authLine = line
					s.mu.Unlock()
					tw("235 2.7.0 ok")
				case strings.HasPrefix(upper, "QUIT"):
					tw("221 2.0.0 bye")
					return
				default:
					tw("250 ok")
				}
			}
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.mu.Lock()
			s.authLine = line
			s.mu.Unlock()
			w("235 2.7.0 ok")
		case strings.HasPrefix(upper, "MAIL FROM"):
			w("250 2.1.0 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			addr := between(line, "<", ">")
			if strings.HasPrefix(strings.ToLower(addr), "reject@") {
				w("550 5.1.1 no such user")
			} else {
				w("250 2.1.5 ok")
			}
		case upper == "DATA":
			w("354 go ahead")
			var msg strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				msg.WriteString(l)
			}
			s.mu.Lock()
			s.msgCount++
			s.mu.Unlock()
			w("250 2.0.0 queued")
		case upper == "QUIT":
			w("221 2.0.0 bye")
			return
		default:
			w("250 ok")
		}
	}
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

var (
	mockCertOnce sync.Once
	mockCertTLS  *tls.Certificate
)

func mockCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	mockCertOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "mock"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		cert := &tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        nil,
		}
		mockCertTLS = cert
	})
	return mockCertTLS, nil
}

func mockCertDER() []byte {
	_, _ = mockCertificate(nil)
	return mockCertTLS.Certificate[0]
}

func TestDialGreetAuthDeliver(t *testing.T) {
	addr, srv := newMockServer(t, false, true)
	ctx := context.Background()
	c, err := Dial(ctx, addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Greet(ctx, "mail.mailez.test", TLSModeOpportunistic, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := c.AuthPlain(ctx, "relayuser", "relaypass"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	auth := srv.authLine
	srv.mu.Unlock()
	raw := strings.TrimPrefix(auth, "AUTH PLAIN ")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "\x00relayuser\x00relaypass" {
		t.Fatalf("auth payload = %q", decoded)
	}

	body := []byte("Subject: x\r\n\r\nline1\r\n.dotline\r\n")
	resps, err := c.Deliver(ctx, "a@example.com", []string{"b@example.com", "c@example.com"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 2 {
		t.Fatalf("resps = %+v", resps)
	}
	for _, r := range resps {
		if r.Code/100 != 2 {
			t.Fatalf("unexpected response: %+v", r)
		}
	}
	srv.mu.Lock()
	n := srv.msgCount
	srv.mu.Unlock()
	if n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}

func TestDeliverRejectsRecipient(t *testing.T) {
	addr, _ := newMockServer(t, false, false)
	ctx := context.Background()
	c, err := Dial(ctx, addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Greet(ctx, "mail.mailez.test", TLSModeOpportunistic, nil, false); err != nil {
		t.Fatal(err)
	}
	resps, err := c.Deliver(ctx, "a@example.com", []string{"good@example.com", "reject@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 2 || resps[1].Code/100 != 5 || !resps[1].Permanent {
		t.Fatalf("resps = %+v", resps)
	}
	if resps[0].Code/100 != 2 {
		t.Fatalf("good recipient failed: %+v", resps[0])
	}
}

func TestRequiredTLSWithoutStartTLSFails(t *testing.T) {
	addr, _ := newMockServer(t, false, false)
	c, err := Dial(context.Background(), addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Greet(context.Background(), "mail.mailez.test", TLSModeRequired, nil, false); err == nil {
		t.Fatal("expected required TLS error")
	}
}

func TestStartTLSOpportunistic(t *testing.T) {
	addr, _ := newMockServer(t, true, false)
	c, err := Dial(context.Background(), addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Opportunistic + advertised STARTTLS: the mock cert is self-signed, so
	// the upgrade attempt fails and Greet reports ErrNoTLSUpgrade, letting
	// the caller retry over plaintext (fail-open policy).
	err = c.Greet(context.Background(), "mail.mailez.test", TLSModeOpportunistic, nil, false)
	if err != ErrNoTLSUpgrade {
		t.Fatalf("err = %v, want ErrNoTLSUpgrade", err)
	}

	// The documented fallback: retry with NoTLS and deliver in plaintext.
	c2, err := Dial(context.Background(), addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Greet(context.Background(), "mail.mailez.test", TLSModeOpportunistic, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Deliver(context.Background(), "a@example.com", []string{"b@example.com"}, []byte("Subject: x\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
}

func TestDANEVerification(t *testing.T) {
	certDER := mockCertDER()
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	spki := cert.RawSubjectPublicKeyInfo
	sum := sha256.Sum256(spki)
	match := maildns.TLSA{Usage: 3, Selector: 1, MatchingType: 1, Cert: sum[:]}

	addr, _ := newMockServer(t, true, false)
	c, err := Dial(context.Background(), addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// DANE-EE with a self-signed cert: handshake must succeed (no PKIX) and
	// the TLSA record must match.
	if err := c.Greet(context.Background(), "mail.mailez.test", TLSModeRequired, []maildns.TLSA{match}, false); err != nil {
		t.Fatal(err)
	}
	if !c.tlsUpgraded() {
		t.Fatal("connection not upgraded")
	}

	// A mismatching record must fail closed.
	c2, err := Dial(context.Background(), addr, ConnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	bad := maildns.TLSA{Usage: 3, Selector: 1, MatchingType: 1, Cert: make([]byte, 32)}
	if err := c2.Greet(context.Background(), "mail.mailez.test", TLSModeRequired, []maildns.TLSA{bad}, false); err == nil {
		t.Fatal("expected DANE mismatch error")
	}
}

func (c *Client) tlsUpgraded() bool {
	_, ok := c.conn.(*tls.Conn)
	return ok
}

func TestParseEHLO(t *testing.T) {
	c := parseEHLO([]string{
		"mail.example.com",
		"8BITMIME",
		"AUTH PLAIN LOGIN",
		"SIZE 10485760",
		"STARTTLS",
	})
	if !c.StartTLS || !c.EightBitMIME || c.Size != 10485760 || len(c.Auth) != 2 {
		t.Fatalf("caps = %+v", c)
	}
	if !c.AdvertisesAuth("plain") || c.AdvertisesAuth("CRAM-MD5") {
		t.Fatalf("auth advertisement wrong: %+v", c.Auth)
	}
}
