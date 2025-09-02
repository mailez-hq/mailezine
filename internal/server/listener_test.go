package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestListenerEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := &Listener{
		Name:    "test",
		Addr:    "127.0.0.1:0",
		MaxConn: 4,
		Logger:  discardLogger(),
		Handler: func(_ context.Context, conn net.Conn) error {
			_, err := conn.Write([]byte("ok\n"))
			return err
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.ServeListener(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 4)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "ok\n" {
		t.Fatalf("echo: %q err=%v", buf[:n], err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestListenerRejectsOverLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	handler := func(_ context.Context, conn net.Conn) error {
		<-release
		return nil
	}
	l := &Listener{
		Name:    "limit-test",
		Addr:    "127.0.0.1:0",
		MaxConn: 1,
		Logger:  discardLogger(),
		Handler: handler,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.ServeListener(ctx, ln) }()

	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Give the accept loop time to take the first connection.
	time.Sleep(50 * time.Millisecond)

	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// The second connection must be closed immediately (limit reached).
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("expected closed connection, read %d bytes", n)
	}

	close(release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestLimitListenerBackpressure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lim := NewLimitListener(ln, 1)

	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			conn, err := lim.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	first, err := net.Dial("tcp", lim.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	conn1 := <-accepted
	defer conn1.Close()

	// Second connection while the slot is held: the listener must CLOSE it
	// immediately instead of queueing it inside Accept — a blocked Accept
	// holding an accepted socket stalls the whole accept loop (one idle peer
	// per slot = permanent port outage for accept loops owned by protocol
	// libraries).
	second, err := net.Dial("tcp", lim.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case <-accepted:
		t.Fatal("second connection accepted while first is open")
	case <-time.After(150 * time.Millisecond):
	}
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-limit connection still open; want immediate close")
	}

	// Once the slot is released, a NEW connection is accepted.
	if err := conn1.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := net.Dial("tcp", lim.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("new connection not accepted after slot release")
	}
}

func TestListenerImplicitTLSAfterProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, err := testCert()
	if err != nil {
		t.Fatal(err)
	}
	l := &Listener{
		Name:          "test",
		Addr:          "127.0.0.1:0",
		MaxConn:       4,
		ProxyProtocol: true,
		TLSConfig:     &tls.Config{Certificates: []tls.Certificate{cert}},
		Logger:        discardLogger(),
		Handler: func(_ context.Context, conn net.Conn) error {
			if _, ok := conn.(*tls.Conn); !ok {
				return errors.New("handler did not receive a TLS connection")
			}
			_, err := conn.Write([]byte("secure\n"))
			return err
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.ServeListener(ctx, ln) }()

	// The PROXY v1 header is plaintext and must precede the TLS handshake.
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Write([]byte("PROXY TCP4 203.0.113.9 10.0.0.1 4242 143\r\n")); err != nil {
		t.Fatal(err)
	}
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	buf := make([]byte, 7) // "secure\n"
	if _, err := io.ReadFull(tlsConn, buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(buf), "secure") {
		t.Fatalf("handler reply = %q", buf)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
	}
}

func testCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
