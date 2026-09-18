package server

import (
	"bytes"
	"net"
	"strings"
)

// go-smtp answers a command that arrives out of order with 502 — "Missing MAIL
// FROM command." and friends. RFC 5321 §4.3.2 specifies 503 for a bad sequence
// of commands, and the library (v0.25.0, its current release) has no hook to
// change it.
//
// badSequenceReplies are the exact refusals it emits for that case.
var badSequenceReplies = []string{
	"Please introduce yourself first.",
	"Missing MAIL FROM command.",
	"Missing RCPT TO command.",
	"MAIL not allowed during message transfer",
}

// rewriteBadSequence turns one of those refusals into the 503 the RFC asks
// for. It only touches a write that starts with the exact status line, so
// every other byte on the wire passes through unchanged: an encrypted stream
// never matches, and a reply split across writes is left alone rather than
// guessed at.
func rewriteBadSequence(p []byte) []byte {
	const prefix = "502 5.5.1 "
	if !bytes.HasPrefix(p, []byte(prefix)) {
		return p
	}
	line := string(p)
	for _, text := range badSequenceReplies {
		if strings.HasPrefix(line, prefix+text) {
			out := make([]byte, len(p))
			copy(out, p)
			copy(out, "503")
			return out
		}
	}
	return p
}

// badSequenceConn rewrites those replies on their way out.
type badSequenceConn struct{ net.Conn }

func (c *badSequenceConn) Write(p []byte) (int, error) {
	return c.Conn.Write(rewriteBadSequence(p))
}

// badSequenceListener wraps a listener whose protocol refuses out-of-order
// commands with the wrong status code.
type badSequenceListener struct{ net.Listener }

func (l badSequenceListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &badSequenceConn{Conn: conn}, nil
}

// NewBadSequenceListener applies the SMTP bad-sequence status fix to every
// connection accepted from ln. Wrap the plaintext listener: a stream that is
// already encrypted is left byte-for-byte alone.
func NewBadSequenceListener(ln net.Listener) net.Listener {
	return badSequenceListener{Listener: ln}
}

var _ net.Listener = badSequenceListener{}
