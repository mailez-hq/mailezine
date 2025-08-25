package smtp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// selfSignedTLS returns a server TLS config with a freshly generated
// self-signed certificate.
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mail.mailez.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"mail.mailez.test", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
	if cert.Leaf, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func TestSMTPStartTLS(t *testing.T) {
	b, cap := testBackend(t, false)
	b.TLSConfig = selfSignedTLS(t)
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
	if ok, _ := client.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS not advertised")
	}
	_ = client.Close()

	// The full upgrade path via DialStartTLS.
	secure, err := gosmtp.DialStartTLS(ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("DialStartTLS: %v", err)
	}
	t.Cleanup(func() { _ = secure.Close() })
	body := "From: s@remote.test\r\nTo: alice@example.com\r\nSubject: secure\r\n\r\nbody\r\n"
	if err := sendMessage(t, secure, "s@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 1 || !strings.Contains(string(cap.data), "secure") {
		t.Fatalf("submit after STARTTLS: from=%q data=%q", cap.from, cap.data)
	}
}

func TestSMTPServerCapabilities(t *testing.T) {
	b, _ := testBackend(t, false)
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
	defer client.Close()
	for _, cap := range []string{"8BITMIME", "SIZE", "PIPELINING", "DSN", "SMTPUTF8", "ENHANCEDSTATUSCODES"} {
		ok, _ := client.Extension(cap)
		if !ok {
			t.Fatalf("capability %s not advertised", cap)
		}
	}
}

func TestSMTPPlainAuthRequiresTLSWhenConfigured(t *testing.T) {
	b, cap := testBackend(t, true)
	b.TLSConfig = selfSignedTLS(t)
	srv := NewServer(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	// Plaintext AUTH must be refused when TLS is configured.
	client, err := gosmtp.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err == nil {
		t.Fatal("plaintext AUTH allowed with TLS configured")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.submits != 0 {
		t.Fatalf("message submitted before TLS auth")
	}
}
