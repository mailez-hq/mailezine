package server

import (
	"net"
	"testing"
)

type captureConn struct {
	net.Conn
	written []byte
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.written = append(c.written, p...)
	return len(p), nil
}

// A command that arrives before MAIL FROM must answer 503, not the library's
// 502 (RFC 5321 §4.3.2).
func TestBadSequenceReplyIsRewritten(t *testing.T) {
	cases := map[string]string{
		"502 5.5.1 Missing MAIL FROM command.\r\n":       "503 5.5.1 Missing MAIL FROM command.\r\n",
		"502 5.5.1 Missing RCPT TO command.\r\n":         "503 5.5.1 Missing RCPT TO command.\r\n",
		"502 5.5.1 Please introduce yourself first.\r\n": "503 5.5.1 Please introduce yourself first.\r\n",
	}
	for in, want := range cases {
		if got := string(rewriteBadSequence([]byte(in))); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}

// Everything else — other 502s, other commands' replies, split writes, and
// encrypted bytes — must pass through untouched.
func TestBadSequenceLeavesOtherTrafficAlone(t *testing.T) {
	untouched := []string{
		"502 5.5.1 command not implemented\r\n",
		"250 2.0.0 OK\r\n",
		"221 2.0.0 Bye\r\n",
		"502 5.5.1 Missing MAIL",                       // split write: not rewritten
		"\x16\x03\x01\x00\xa5\x01\x00\x00\xa1\x03\x03", // TLS record
	}
	for _, in := range untouched {
		if got := string(rewriteBadSequence([]byte(in))); got != in {
			t.Errorf("rewrite(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestBadSequenceListenerWrapsAcceptedConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wrapped := NewBadSequenceListener(ln)
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			c.Close()
		}
	}()
	conn, err := wrapped.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(*badSequenceConn); !ok {
		t.Fatalf("accepted %T, want *badSequenceConn", conn)
	}
}
