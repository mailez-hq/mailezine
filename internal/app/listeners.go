package app

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"

	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/config"
	"mailezine/internal/server"
)

// serveSMTP binds addr and serves; the server is drained in the shutdown
// phase via Shutdown. A non-nil tlsConf turns the listener into implicit
// TLS (RFC 8314 submissions port), negotiated before the first SMTP byte.
// proxyTrusted gates which peers may carry the PROXY v1 header.
func serveSMTP(ctx context.Context, srv *gosmtp.Server, addr string, maxConn int, proxy bool, proxyTrusted []*net.IPNet, tlsConf *tls.Config, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "component", "smtp", "addr", ln.Addr().String())
	var lim net.Listener = server.NewLimitListener(ln, maxConn)
	if proxy {
		lim = server.NewProxyListener(lim, logger, proxyTrusted)
	}
	// go-smtp answers an out-of-order command with 502; RFC 5321 asks for
	// 503. Wrap before the TLS layer (an encrypted stream is passed through
	// untouched, so this covers the plaintext and pre-STARTTLS replies).
	lim = server.NewBadSequenceListener(lim)
	if tlsConf != nil {
		lim = tls.NewListener(lim, tlsConf)
	}
	go func() {
		if err := srv.Serve(lim); err != nil && ctx.Err() == nil {
			logger.Error("smtp server", "addr", addr, "err", err)
		}
	}()
	return nil
}

// tcpServer is the accept-loop surface shared by protocol servers that own
// their own listener (go-imap imapserver).
type tcpServer interface {
	Serve(net.Listener) error
	Close() error
}

// serveTCP binds addr and serves a tcpServer (LimitListener backpressure).
// A non-nil tlsConf turns the listener into implicit TLS (imaps port).
// proxyTrusted gates which peers may carry the PROXY v1 header.
func serveTCP(ctx context.Context, srv tcpServer, name, addr string, maxConn int, proxy bool, proxyTrusted []*net.IPNet, tlsConf *tls.Config, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "component", name, "addr", ln.Addr().String())
	var lim net.Listener = server.NewLimitListener(ln, maxConn)
	if proxy {
		lim = server.NewProxyListener(lim, logger, proxyTrusted)
	}
	if tlsConf != nil {
		lim = tls.NewListener(lim, tlsConf)
	}
	go func() {
		if err := srv.Serve(lim); err != nil && ctx.Err() == nil {
			logger.Error(name+" server", "addr", addr, "err", err)
		}
	}()
	return nil
}

// proxyEnabled reports whether a listener port is configured for PROXY
// protocol ("all" matches every port).
func proxyEnabled(ports []string, addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	for _, p := range ports {
		if p == "all" || p == port {
			return true
		}
	}
	return false
}

// serveHTTP binds addr and serves; the server shuts down when ctx is done.
func serveHTTP(ctx context.Context, srv *http.Server, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "addr", srv.Addr, "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	return nil
}

// parseNets converts validated CIDR strings to IPNets.
func parseNets(cidrs []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// loadTLS loads the optional certificate for direct deployments. A nil
// config means the gateway terminates TLS and STARTTLS stays off.
func loadTLS(cfg config.Config, logger *slog.Logger) (*tls.Config, error) {
	if cfg.TLS.CertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, err
	}
	logger.Info("tls: STARTTLS enabled", "cert", cfg.TLS.CertFile)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
