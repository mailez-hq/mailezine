// PROXY protocol v1 (HAProxy) support for listeners behind a load balancer
// or the mailez gateway (MAILEZ_PROXY_PROTOCOL). Explicitly enabled per
// listener; the header must be present on every connection.
package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// ErrNotProxy is returned when a connection expected to carry a PROXY v1
// header does not start with "PROXY ".
var ErrNotProxy = errors.New("server: missing PROXY protocol header")

// ProxyListener wraps a listener and parses the PROXY v1 header on every
// accepted connection. Connections without the header are closed and the
// accept loop continues (a library-owned accept loop must never see a
// rejection error).
type ProxyListener struct {
	ln     net.Listener
	logger *slog.Logger
}

// NewProxyListener wraps ln. logger is optional.
func NewProxyListener(ln net.Listener, logger *slog.Logger) *ProxyListener {
	return &ProxyListener{ln: ln, logger: logger}
}

func (l *ProxyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return nil, err
		}
		wrapped, perr := NewProxyConn(conn)
		if perr != nil {
			if l.logger != nil {
				l.logger.Warn("connection rejected", "reason", "proxy protocol", "err", perr)
			}
			continue
		}
		return wrapped, nil
	}
}

func (l *ProxyListener) Close() error   { return l.ln.Close() }
func (l *ProxyListener) Addr() net.Addr { return l.ln.Addr() }

var _ net.Listener = (*ProxyListener)(nil)

// proxyConn wraps a connection whose first line is a PROXY v1 header; the
// reported remote address is the real client.
type proxyConn struct {
	net.Conn
	r      *bufio.Reader
	remote net.Addr
	local  net.Addr
}

// NewProxyConn reads and validates the PROXY v1 header from conn, returning
// a connection whose RemoteAddr/LocalAddr reflect the real endpoints.
func NewProxyConn(conn net.Conn) (net.Conn, error) {
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("server: read PROXY header: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "PROXY ") {
		_ = conn.Close()
		return nil, ErrNotProxy
	}
	if strings.TrimSpace(line) == "PROXY UNKNOWN" {
		return &proxyConn{Conn: conn, r: r, remote: conn.RemoteAddr(), local: conn.LocalAddr()}, nil
	}
	srcIP, srcPort, dstIP, dstPort, ok := parseProxyLine(line)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("server: malformed PROXY header %q", line)
	}
	return &proxyConn{
		Conn:   conn,
		r:      r,
		remote: &net.TCPAddr{IP: srcIP, Port: srcPort},
		local:  &net.TCPAddr{IP: dstIP, Port: dstPort},
	}, nil
}

func (c *proxyConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *proxyConn) RemoteAddr() net.Addr { return c.remote }
func (c *proxyConn) LocalAddr() net.Addr  { return c.local }

// parseProxyLine handles "PROXY TCP4 src dst sport dport" / TCP6.
func parseProxyLine(line string) (srcIP net.IP, srcPort int, dstIP net.IP, dstPort int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 6 {
		return nil, 0, nil, 0, false
	}
	switch fields[1] {
	case "TCP4":
		srcIP = net.ParseIP(fields[2])
		dstIP = net.ParseIP(fields[3])
		if srcIP == nil || dstIP == nil || srcIP.To4() == nil || dstIP.To4() == nil {
			return nil, 0, nil, 0, false
		}
	case "TCP6":
		srcIP = net.ParseIP(fields[2])
		dstIP = net.ParseIP(fields[3])
		if srcIP == nil || dstIP == nil {
			return nil, 0, nil, 0, false
		}
	default:
		return nil, 0, nil, 0, false
	}
	var err error
	if srcPort, err = strconv.Atoi(fields[4]); err != nil || srcPort < 0 || srcPort > 65535 {
		return nil, 0, nil, 0, false
	}
	if dstPort, err = strconv.Atoi(fields[5]); err != nil || dstPort < 0 || dstPort > 65535 {
		return nil, 0, nil, 0, false
	}
	return srcIP, srcPort, dstIP, dstPort, true
}

var _ io.ReadWriteCloser = (*proxyConn)(nil)
