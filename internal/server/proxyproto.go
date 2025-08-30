// Package-level policy for PROXY v1 header parsing.
//
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
	"time"
)

// ErrNotProxy is returned when a connection expected to carry a PROXY v1
// header does not start with "PROXY ".
var ErrNotProxy = errors.New("server: missing PROXY protocol header")

// ErrUntrustedProxyPeer is returned when the directly connected peer is not
// in the PROXY trust set: accepting its header would let any client that
// reaches the port forge the client IP (auth/relay decisions downstream
// trust RemoteAddr).
var ErrUntrustedProxyPeer = errors.New("server: PROXY header from untrusted peer")

const (
	// proxyHeaderTimeout bounds the header read: the first bytes arrive
	// immediately after the TCP handshake on a healthy gateway, so a slow
	// or silent peer is hostile or broken either way.
	proxyHeaderTimeout = 30 * time.Second
	// maxProxyLineLen caps the header line: PROXY v1 lines are at most 107
	// bytes (protocol limit), so anything longer is hostile padding.
	maxProxyLineLen = 128
)

// defaultProxyTrustedNets is the trust set applied when none is configured:
// loopback plus the private/link-local ranges a gateway or load balancer
// normally sits in. A gateway on a public address must be added explicitly
// via MAILEZINE_PROXY_TRUSTED.
var defaultProxyTrustedNets = mustParseNets([]string{
	"127.0.0.1/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
	"fe80::/10", "fc00::/7",
})

func mustParseNets(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("server: bad default proxy trust CIDR " + s)
		}
		out = append(out, n)
	}
	return out
}

// peerTrusted reports whether the directly connected peer may speak the
// PROXY header. trusted == nil selects the default set. Non-TCP conns
// (pipes, unix sockets) have no peer IP to forge and are allowed.
func peerTrusted(conn net.Conn, trusted []*net.IPNet) bool {
	if trusted == nil {
		trusted = defaultProxyTrustedNets
	}
	addr := conn.RemoteAddr()
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP == nil {
		return true
	}
	for _, n := range trusted {
		if n.Contains(tcp.IP) {
			return true
		}
	}
	return false
}

// ProxyListener wraps a listener and parses the PROXY v1 header on every
// accepted connection. Connections without the header are closed and the
// accept loop continues (a library-owned accept loop must never see a
// rejection error).
type ProxyListener struct {
	ln      net.Listener
	logger  *slog.Logger
	trusted []*net.IPNet
}

// NewProxyListener wraps ln. logger is optional. trusted is the set of peer
// networks allowed to send the header (nil = loopback/private defaults);
// headers from other peers are rejected to prevent client-IP forgery.
func NewProxyListener(ln net.Listener, logger *slog.Logger, trusted []*net.IPNet) *ProxyListener {
	return &ProxyListener{ln: ln, logger: logger, trusted: trusted}
}

func (l *ProxyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return nil, err
		}
		wrapped, perr := NewProxyConnTrusted(conn, l.trusted)
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
// a connection whose RemoteAddr/LocalAddr reflect the real endpoints. The
// default trust set (loopback/private) gates which peers may speak the
// header.
func NewProxyConn(conn net.Conn) (net.Conn, error) {
	return NewProxyConnTrusted(conn, nil)
}

// NewProxyConnTrusted is NewProxyConn with an explicit PROXY trust set.
func NewProxyConnTrusted(conn net.Conn, trusted []*net.IPNet) (net.Conn, error) {
	if !peerTrusted(conn, trusted) {
		_ = conn.Close()
		return nil, ErrUntrustedProxyPeer
	}
	// Bound the header read so a silent peer cannot hold the connection
	// (and its concurrency slot) open forever; cleared once the header is
	// consumed so protocol timeouts apply afterwards.
	_ = conn.SetDeadline(time.Now().Add(proxyHeaderTimeout))
	r := bufio.NewReaderSize(conn, maxProxyLineLen)
	line, err := r.ReadSlice('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("server: read PROXY header: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	s := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(s, "PROXY ") {
		_ = conn.Close()
		return nil, ErrNotProxy
	}
	if s == "PROXY UNKNOWN" {
		return &proxyConn{Conn: conn, r: r, remote: conn.RemoteAddr(), local: conn.LocalAddr()}, nil
	}
	srcIP, srcPort, dstIP, dstPort, ok := parseProxyLine(s)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("server: malformed PROXY header %q", s)
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
