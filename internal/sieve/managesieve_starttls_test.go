package sieve

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

func TestManageSieveStartTLS(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := &Server{
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Scripts:   mailstore.NewKV(s),
		TLSConfig: sieveSelfSigned(t),
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
			go func() { _ = srv.ManageSieveSession(context.Background(), conn) }()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	readOK := func(what string) {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "OK") {
				return
			}
			if strings.HasPrefix(line, "NO") {
				t.Fatalf("%s: %q", what, line)
			}
		}
	}
	// Greeting advertises STARTTLS (scan content lines for it).
	var sawStartTLS bool
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "OK") {
			break
		}
		if strings.Contains(line, `"STARTTLS"`) {
			sawStartTLS = true
		}
	}
	if !sawStartTLS {
		t.Fatal("STARTTLS not advertised in greeting")
	}

	fmt.Fprintf(conn, "STARTTLS\r\n")
	readOK("STARTTLS")
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	tr := bufio.NewReader(tlsConn)
	fmt.Fprintf(tlsConn, `AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`+"\r\n")
	for {
		line, err := tr.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK") {
			break
		}
	}
	fmt.Fprintf(tlsConn, "NOOP\r\n")
	line, err := tr.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK") {
		t.Fatalf("NOOP over TLS: %q", line)
	}
}

func TestManageSieveAuthRequiresTLSWhenConfigured(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := &Server{
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Scripts:   mailstore.NewKV(s),
		TLSConfig: sieveSelfSigned(t),
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
			go func() { _ = srv.ManageSieveSession(context.Background(), conn) }()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK") {
			break
		}
	}
	fmt.Fprintf(conn, `AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`+"\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "NO") {
			return // plaintext auth refused as expected
		}
		if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK") {
			t.Fatal("plaintext AUTH allowed with TLS configured")
		}
	}
}

func sieveSelfSigned(t *testing.T) *tls.Config {
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
