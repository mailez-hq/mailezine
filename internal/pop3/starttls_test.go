package pop3

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func pop3SelfSigned(t *testing.T) *tls.Config {
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
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if cert.Leaf, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func TestPOP3StartTLS(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	seedMailbox(t, ms)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := &Server{
		Store:     ms,
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		TLSConfig: pop3SelfSigned(t),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = srv.ServeConn(context.Background(), conn) }()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, _ := r.ReadString('\n')
	if !strings.HasPrefix(line, "+OK") {
		t.Fatalf("greeting: %q", line)
	}
	// CAPA advertises STLS.
	fmt.Fprintf(conn, "CAPA\r\n")
	var caps []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "." {
			break
		}
		caps = append(caps, line)
	}
	found := false
	for _, c := range caps {
		if strings.EqualFold(c, "STLS") {
			found = true
		}
	}
	if !found {
		t.Fatalf("STLS not advertised: %v", caps)
	}

	// Upgrade and authenticate over TLS.
	fmt.Fprintf(conn, "STLS\r\n")
	line, _ = r.ReadString('\n')
	if !strings.HasPrefix(line, "+OK") {
		t.Fatalf("STLS: %q", line)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	tr := bufio.NewReader(tlsConn)
	fmt.Fprintf(tlsConn, "USER alice@example.com\r\n")
	line, _ = tr.ReadString('\n')
	if !strings.HasPrefix(line, "+OK") {
		t.Fatalf("USER over TLS: %q", line)
	}
	fmt.Fprintf(tlsConn, "PASS s3cret\r\n")
	line, _ = tr.ReadString('\n')
	if !strings.HasPrefix(line, "+OK") {
		t.Fatalf("PASS over TLS: %q", line)
	}
	fmt.Fprintf(tlsConn, "STAT\r\n")
	line, _ = tr.ReadString('\n')
	if !strings.HasPrefix(line, "+OK 2") {
		t.Fatalf("STAT over TLS: %q", line)
	}
}

func TestPOP3AuthRequiresSTLSWhenConfigured(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := &Server{
		Store:     mailstore.NewKV(s),
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		TLSConfig: pop3SelfSigned(t),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = srv.ServeConn(context.Background(), conn) }()
		}
	}()

	c := dialPOP(t, ln.Addr().String())
	c.cmd("USER alice@example.com")
	if got := c.cmd("PASS s3cret"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("plaintext PASS allowed: %q", got)
	}
}
