// Package server provides the connection lifecycle shared by every protocol
// listener: accept loop, connection limits, per-connection logging and
// graceful shutdown (ARCHITECTURE.md §7).
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// Handler processes one accepted connection. It must return when ctx is
// cancelled so the listener can drain in-flight work during shutdown.
type Handler func(ctx context.Context, conn net.Conn) error

// LimitListener wraps a net.Listener and applies backpressure at max
// concurrent connections. The slot is acquired without blocking before the
// connection is handed out: an owner of its own accept loop (e.g. go-smtp,
// go-imap) would otherwise block inside Accept while holding an accepted but
// unserved connection — with every slot pinned by idle peers, that accepted
// socket (and the whole accept loop behind it) would hang forever. With the
// non-blocking acquire, an over-limit connection is closed immediately and
// the accept loop keeps draining.
type LimitListener struct {
	ln  net.Listener
	sem chan struct{}
}

// NewLimitListener caps concurrent connections accepted through ln.
func NewLimitListener(ln net.Listener, max int) *LimitListener {
	if max <= 0 {
		max = 256
	}
	return &LimitListener{ln: ln, sem: make(chan struct{}, max)}
}

// Accept returns the next connection that fits within the limit; over-limit
// connections are closed immediately. The returned conn releases its slot
// when closed.
func (l *LimitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-l.sem }}, nil
		default:
			_ = conn.Close()
		}
	}
}

// Close closes the underlying listener.
func (l *LimitListener) Close() error { return l.ln.Close() }

// Addr returns the underlying listener address.
func (l *LimitListener) Addr() net.Addr { return l.ln.Addr() }

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

var _ net.Listener = (*LimitListener)(nil)

// Listener is a named TCP listener with bounded concurrency.
type Listener struct {
	Name          string
	Addr          string
	MaxConn       int
	ProxyProtocol bool         // expect a PROXY v1 header on every connection
	ProxyTrusted  []*net.IPNet // peers allowed to speak the header (nil = loopback/private defaults)
	TLSConfig     *tls.Config  // optional: serve implicit TLS (RFC 8314)
	Logger        *slog.Logger
	Handler       Handler
}

// Serve accepts connections until ctx is cancelled, then waits for in-flight
// handlers to finish.
func (l *Listener) Serve(ctx context.Context) error {
	if l.MaxConn <= 0 {
		l.MaxConn = 256
	}
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	ln, err := net.Listen("tcp", l.Addr)
	if err != nil {
		return fmt.Errorf("%s: listen: %w", l.Name, err)
	}
	return l.ServeListener(ctx, ln)
}

// ServeListener runs the accept loop over an existing listener. It is split
// from Serve so tests and embedders can bind a listener themselves and know
// its address (mirrors net/http's Server.Serve).
func (l *Listener) ServeListener(ctx context.Context, ln net.Listener) error {
	if l.MaxConn <= 0 {
		l.MaxConn = 256
	}
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	l.Logger.Info("listening", "component", l.Name, "addr", ln.Addr().String(), "tls", l.TLSConfig != nil)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	sem := make(chan struct{}, l.MaxConn)
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // listener closed by shutdown
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("%s: accept: %w", l.Name, err)
		}
		select {
		case sem <- struct{}{}:
		default:
			_ = conn.Close()
			l.Logger.Warn("connection rejected", "component", l.Name, "reason", "limit")
			continue
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer func() { <-sem }()
			if l.ProxyProtocol {
				wrapped, err := NewProxyConnTrusted(c, l.ProxyTrusted)
				if err != nil {
					l.Logger.Warn("connection rejected", "component", l.Name, "reason", "proxy protocol", "err", err)
					return
				}
				c = wrapped
			}
			// PROXY header (plaintext) precedes TLS: wrap after it is
			// consumed so implicit-TLS ports can sit behind a gateway/LB.
			if l.TLSConfig != nil {
				c = tls.Server(c, l.TLSConfig)
			}
			sessionID := newSessionID()
			l.Logger.Debug("connection open", "component", l.Name, "session", sessionID)
			err := l.Handler(ctx, c)
			_ = c.Close()
			if err != nil && ctx.Err() == nil {
				l.Logger.Debug("connection closed", "component", l.Name, "session", sessionID, "err", err)
			}
		}(conn)
	}
	wg.Wait()
	return nil
}

func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}
