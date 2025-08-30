package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestParseProxyLine(t *testing.T) {
	cases := []struct {
		line string
		src  string
		dst  string
		ok   bool
	}{
		{"PROXY TCP4 203.0.113.9 10.0.0.1 12345 25", "203.0.113.9", "10.0.0.1", true},
		{"PROXY TCP6 ::1 2001:db8::2 12345 25", "::1", "2001:db8::2", true},
		{"PROXY UNKNOWN", "", "", false},
		{"PROXY TCP4 bad 10.0.0.1 1 2", "", "", false},
		{"EHLO mail.example", "", "", false},
	}
	for _, tc := range cases {
		src, sport, dst, dport, ok := parseProxyLine(tc.line)
		if ok != tc.ok {
			t.Errorf("%q ok=%v want %v", tc.line, ok, tc.ok)
			continue
		}
		if ok && (src.String() != tc.src || dst.String() != tc.dst || sport == 0 || dport == 0) {
			t.Errorf("%q → %v:%d %v:%d", tc.line, src, sport, dst, dport)
		}
	}
}

func TestNewProxyConn(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		_, _ = io.WriteString(server, "PROXY TCP4 203.0.113.9 10.0.0.1 12345 25\r\nEHLO hello\r\n")
		_ = server.Close()
	}()

	pc, err := NewProxyConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if got := pc.RemoteAddr().String(); got != "203.0.113.9:12345" {
		t.Fatalf("remote = %s", got)
	}
	if got := pc.LocalAddr().String(); got != "10.0.0.1:25" {
		t.Fatalf("local = %s", got)
	}
	// Remaining bytes are readable after the header.
	line, err := bufio.NewReader(pc).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "EHLO") {
		t.Fatalf("after header: %q", line)
	}
}

func TestNewProxyConnMissingHeader(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		_, _ = io.WriteString(server, "EHLO direct\r\n")
		_ = server.Close()
	}()
	_, err := NewProxyConn(client)
	if !errors.Is(err, ErrNotProxy) {
		t.Fatalf("expected ErrNotProxy, got %v", err)
	}
}

func TestNewProxyConnUnknownKeepsSocket(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		_, _ = io.WriteString(server, "PROXY UNKNOWN\r\nNOOP\r\n")
		_ = server.Close()
	}()
	pc, err := NewProxyConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if pc.RemoteAddr().String() == "" {
		t.Fatal("remote lost for UNKNOWN")
	}
}

// dialTCPPair gives NewProxyConn a real TCP conn so the peer-IP trust check
// is exercised (net.Pipe peers have no IP and are always allowed).
func dialTCPPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.c, client
}

func TestNewProxyConnTrustedLoopbackPeer(t *testing.T) {
	server, client := dialTCPPair(t)
	defer client.Close()
	go func() {
		_, _ = io.WriteString(server, "PROXY TCP4 203.0.113.9 10.0.0.1 12345 25\r\nEHLO hello\r\n")
		_ = server.Close()
	}()
	// Default trust set (nil) includes loopback, so the header is honored.
	pc, err := NewProxyConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if got := pc.RemoteAddr().String(); got != "203.0.113.9:12345" {
		t.Fatalf("remote = %s", got)
	}
}

func TestNewProxyConnUntrustedPeerRejected(t *testing.T) {
	server, client := dialTCPPair(t)
	defer client.Close()
	go func() {
		_, _ = io.WriteString(server, "PROXY TCP4 203.0.113.9 10.0.0.1 12345 25\r\n")
		_ = server.Close()
	}()
	// Trust set that excludes the actual peer (127.0.0.1): the header must
	// be rejected — otherwise any direct client could forge its IP.
	_, testnet, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewProxyConnTrusted(client, []*net.IPNet{testnet})
	if !errors.Is(err, ErrUntrustedProxyPeer) {
		t.Fatalf("expected ErrUntrustedProxyPeer, got %v", err)
	}
}

func TestNewProxyConnOversizedHeader(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		// A header line far beyond the 107-byte protocol limit: the read
		// must cap out instead of buffering an arbitrary line length.
		_, _ = io.WriteString(server, "PROXY TCP4 "+strings.Repeat("9", 4096)+" 10.0.0.1 1 2\r\n")
		_ = server.Close()
	}()
	_, err := NewProxyConn(client)
	if err == nil {
		t.Fatal("expected oversized PROXY header to be rejected")
	}
}
