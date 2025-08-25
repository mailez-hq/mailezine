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
