// Command mailezine is the mailez first-party mail engine (engine C).
//
// The composition root wires the mail path end to end: SMTP inbound and
// submission listeners, verification (SPF/DKIM/DMARC), spam classification
// (rspamd), local delivery through the directory, and the outbound queue
// with opportunistic DKIM signing. Health, metrics and the management API
// share the process, alongside the IMAP/POP3/ManageSieve listeners.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	gosmtp "github.com/emersion/go-smtp"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mailezine/internal/auth"
	"mailezine/internal/config"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/dkim"
	"mailezine/internal/fts"
	"mailezine/internal/ha"
	"mailezine/internal/imap"
	"mailezine/internal/maildns"
	"mailezine/internal/mailmtasts"
	"mailezine/internal/management"
	"mailezine/internal/metrics"
	"mailezine/internal/pop3"
	"mailezine/internal/queue"
	"mailezine/internal/server"
	"mailezine/internal/sieve"
	"mailezine/internal/smtp"
	"mailezine/internal/spam"
	"mailezine/internal/telemetry"
	"mailezine/internal/verify"
	"mailezine/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "reindex" {
		os.Exit(runReindex(os.Args[2:]))
	}
	os.Exit(runCtx(ctx, os.Args[1:]))
}

func runCtx(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("mailezine", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailezine: %v\n", err)
		return 2
	}
	logger := telemetry.NewLogger(cfg.Log.Level, cfg.Log.Format)
	m := metrics.New()
	logger.Info("starting", "version", version.Version, "summary", cfg.Summary())

	// Active-passive HA: acquire the leadership lease before opening the
	// single-writer KV. Followers stay in a standby loop and take over
	// when the lease expires (D31).
	var leader *ha.Leader
	if cfg.HA.Enabled {
		store, err := openHAStore(cfg, logger)
		if err != nil {
			logger.Error("ha: lease store", "err", err)
			return 2
		}
		owner := cfg.Hostname + "-" + strconv.Itoa(os.Getpid())
		ttl := time.Duration(cfg.HA.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = 15 * time.Second
		}
		leader = ha.NewLeader(store, owner, ttl, logger)
		haCtx, haCancel := context.WithCancel(context.Background())
		defer haCancel()
		for {
			if err := leader.TryAcquire(haCtx); err == nil {
				break
			} else if !errors.Is(err, ha.ErrNotLeader) {
				logger.Error("ha: lease acquire", "err", err)
				return 2
			}
			logger.Info("ha: standby, waiting for leadership", "owner", owner)
			select {
			case <-haCtx.Done():
				return 2
			case <-time.After(ttl / 2):
			}
		}
		logger.Info("ha: leadership acquired", "owner", owner, "ttl", ttl)
		go leader.Run(haCtx)
		defer func() { _ = leader.Release(context.Background()) }()
	}

	dir, err := newDirectory(cfg, logger)
	if err != nil {
		logger.Error("directory", "err", err)
		return 2
	}
	defer dir.Close()
	authSvc, err := newAuth(cfg, logger)
	if err != nil {
		logger.Error("auth", "err", err)
		return 2
	}
	defer authSvc.Close()

	st, err := newStorage(cfg, logger)
	if err != nil {
		logger.Error("storage", "err", err)
		return 2
	}
	defer func() { _ = st.Close() }()

	// Embedded full-text index (bleve); a failure disables FTS but never
	// startup (search falls back to the scan).
	var ftsIndexer *fts.Indexer
	if cfg.FTS.Enabled {
		path := cfg.FTS.Path
		if path == "" {
			base := cfg.Storage.RocksPath
			if cfg.Storage.Backend == "maildir" {
				base = filepath.Join(cfg.Storage.MaildirPath, ".mailezine")
			}
			path = base + ".fts"
		}
		ftsIndexer, err = fts.Open(path, cfg.FTS.TikaURL, logger)
		if err != nil {
			logger.Warn("fts: disabled", "path", path, "err", err)
		} else {
			logger.Info("fts: enabled", "path", path, "tika", cfg.FTS.TikaURL != "")
		}
	}
	if ftsIndexer != nil {
		defer func() { _ = ftsIndexer.Close() }()
	}

	startedAt := time.Now()

	// Inbound pipeline: verify → classify → deliver (ARCHITECTURE.md §4).
	verifier := &verify.Verifier{
		Logger:   logger,
		Resolver: maildns.NewSystemResolver(),
		Hostname: cfg.Hostname,
		LocalIP:  localIP(cfg.Hostname),
	}
	var classifier *spam.Client
	if cfg.Rspamd.URL != "" {
		classifier = spam.New(cfg.Rspamd.URL, cfg.Rspamd.LearnURL, cfg.Rspamd.Password, cfg.Hostname, logger)
	}
	pipeline := &delivery.Pipeline{
		Directory:          dir,
		Store:              st.mailbox,
		Verifier:           verifier,
		Sieve:              sieve.NewEngine(logger),
		ScriptSource:       sieve.DefaultScriptSource{Store: st.mailbox, Directory: dir},
		Hostname:           cfg.Hostname,
		RecipientDelimiter: cfg.RecipientDelimiter,
		Logger:             logger,
		FTS:                ftsIndexer,
	}
	if classifier != nil {
		// Never assign a typed nil to the interface: an unconfigured
		// classifier must leave the field nil (delivery fails open).
		pipeline.Classifier = classifier
	}

	// Outbound queue with opportunistic DKIM signing at enqueue.
	var qm *queue.Manager
	var qmDone chan struct{}
	if cfg.Outbound.Enabled {
		dnsResolver := maildns.NewSystemResolver()
		directDeliverer := &queue.SMTPDeliverer{
			Logger:   logger,
			Hostname: cfg.Hostname,
			Resolver: net.DefaultResolver,
			Port:     cfg.Outbound.Port,
			Username: cfg.Outbound.SmarthostUsername,
			Password: cfg.Outbound.SmarthostPassword,
			// MTA-STS/DANE policy for outbound TLS (opportunistic fallback).
			PolicyResolver: dnsResolver,
			MTSTS:          mailmtasts.NewFetcher(),
		}
		deliverer := &queue.RelayDeliverer{
			Directory: dir,
			Direct:    directDeliverer,
			Logger:    logger,
		}
		qOpts := queue.DefaultOptions()
		if cfg.Queue.MaxAttempts > 0 {
			qOpts.MaxAttempts = cfg.Queue.MaxAttempts
		}
		if cfg.Queue.BaseRetry > 0 {
			qOpts.BaseRetry = cfg.Queue.BaseRetry
		}
		if cfg.Queue.MaxRetry > 0 {
			qOpts.MaxRetry = cfg.Queue.MaxRetry
		}
		if cfg.Queue.PollInterval > 0 {
			qOpts.PollInterval = cfg.Queue.PollInterval
		}
		if cfg.Queue.DelayWarning > 0 {
			qOpts.DelayWarning = cfg.Queue.DelayWarning
		}
		qm = queue.New(st.kv, st.blob, deliverer, qOpts, logger)
		qm.SetSigner(opportunisticSigner{dkim.NewSigner(cfg.DKIMVaultURL, logger), logger})
		qm.SetOnEvent(func(event string) {
			m.QueueMessages.WithLabelValues(event).Inc()
		})
		qm.SetBounceHandler(func(ctx context.Context, from string, msg *queue.Message, body []byte, failures []queue.BounceFailure) {
			// RFC 5321 §6.1: never generate a DSN for a null sender.
			if from == "" {
				return
			}
			dsnBytes, derr := queue.ComposeBounceDSN(from, msg, failures, cfg.Hostname)
			if derr != nil {
				logger.Error("bounce: compose dsn", "from", from, "err", derr)
				return
			}
			// Local recipients get the DSN through the delivery pipeline;
			// external recipients are spooled with a null envelope sender
			// (which can never bounce again).
			if _, err := dir.Aliases(ctx, from); err == nil {
				if err := pipeline.Deliver(ctx, nil, "", []string{from}, dsnBytes); err != nil {
					logger.Error("bounce: local deliver", "from", from, "err", err)
				}
				return
			}
			if _, err := qm.Submit(ctx, "", []string{from}, "Delivery Status Notification (Failure)", bytes.NewReader(dsnBytes)); err != nil {
				logger.Error("bounce: queue dsn", "from", from, "err", err)
			}
		})
		qm.SetDelayWarningHandler(func(ctx context.Context, from string, msg *queue.Message, data []byte, waited time.Duration) {
			if from == "" {
				return
			}
			dsnBytes, derr := queue.ComposeDelayDSN(from, msg, waited, cfg.Hostname)
			if derr != nil || len(dsnBytes) == 0 {
				if derr != nil {
					logger.Error("delay: compose dsn", "from", from, "err", derr)
				}
				return
			}
			// Same routing as bounces: local via the pipeline, external
			// spooled with a null envelope sender.
			if _, err := dir.Aliases(ctx, from); err == nil {
				if err := pipeline.Deliver(ctx, nil, "", []string{from}, dsnBytes); err != nil {
					logger.Error("delay: local deliver", "from", from, "err", err)
				}
				return
			}
			if _, err := qm.Submit(ctx, "", []string{from}, "Delayed Mail Notification", bytes.NewReader(dsnBytes)); err != nil {
				logger.Error("delay: queue dsn", "from", from, "err", err)
			}
		})
		qmDone = make(chan struct{})
		go func() {
			defer close(qmDone)
			if err := qm.Run(ctx); err != nil {
				logger.Error("queue", "err", err)
			}
		}()
		logger.Info("outbound queue", "port", cfg.Outbound.Port, "dkimVault", cfg.DKIMVaultURL)
		pipeline.Redirect = func(ctx context.Context, from, to string, data []byte) error {
			// Sieve redirect: rewrite the envelope sender like relayed mail
			// so bounces route back through us.
			data = delivery.Outclean(data)
			relayFrom := from
			if rewritten, err := dir.SRSForward(ctx, from); err == nil && rewritten != "" {
				relayFrom = rewritten
			}
			if _, err := qm.Submit(ctx, relayFrom, []string{to}, subjectOf(data), bytes.NewReader(data)); err != nil {
				return err
			}
			logger.Info("sieve redirect queued", "from", relayFrom, "to", to, "bytes", len(data))
			return nil
		}
	}

	submit := newSubmit(dir, pipeline, qm, logger)
	trustedNets := parseNets(cfg.TrustedNets)
	tlsConf, err := loadTLS(cfg, logger)
	if err != nil {
		logger.Error("tls", "err", err)
		return 2
	}
	smtpInbound := smtp.NewServer(&smtp.Backend{
		Hostname:           cfg.Hostname,
		Directory:          dir,
		Auth:               authSvc,
		TrustedNets:        trustedNets,
		AllowRelay:         cfg.Outbound.Enabled,
		RecipientDelimiter: cfg.RecipientDelimiter,
		MaxRecipients:      cfg.Limits.MaxRecipients,
		MaxMessageBytes:    cfg.Limits.MaxMessageSize,
		MaxLineLength:      cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             logger,
		Submit:             submit,
	})
	smtpSubmission := smtp.NewServer(&smtp.Backend{
		Hostname:           cfg.Hostname,
		Directory:          dir,
		Auth:               authSvc,
		TrustedNets:        trustedNets,
		RequireAuth:        true,
		AllowRelay:         true,
		RecipientDelimiter: cfg.RecipientDelimiter,
		MaxRecipients:      cfg.Limits.MaxRecipients,
		MaxMessageBytes:    cfg.Limits.MaxMessageSize,
		MaxLineLength:      cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             logger,
		Submit:             submit,
	})
	imapCfg := &imap.Server{
		Store:           st.mailbox,
		Auth:            authSvc,
		Directory:       dir,
		MaxMessageBytes: cfg.Limits.MaxMessageSize,
		TLSConfig:       tlsConf,
		Logger:          logger,
		FTS:             ftsIndexer,
	}
	if classifier != nil {
		// Junk-boundary learning on APPEND/COPY/MOVE across Junk.
		learn := classifier
		imapCfg.Learn = func(ctx context.Context, _ string, isSpam bool, data []byte) {
			if err := learn.LearnWithFuzzy(ctx, isSpam, data); err != nil {
				logger.Error("imap: rspamd learn", "isSpam", isSpam, "err", err)
			}
		}
	}
	imapSrv := imap.New(imapCfg)
	manageSieve := &sieve.Server{
		Auth:      authSvc,
		Directory: dir,
		Scripts:   st.mailbox,
		TLSConfig: tlsConf,
		Logger:    logger,
	}
	pop3Srv := &pop3.Server{
		Store:     st.mailbox,
		Auth:      authSvc,
		Directory: dir,
		TLSConfig: tlsConf,
		Logger:    logger,
	}

	// Health, readiness and metrics.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		m.HealthChecks.WithLabelValues("/health").Inc()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version.Version)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		m.HealthChecks.WithLabelValues("/ready").Inc()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","storage":%q,"directory":%q,"rspamd":%v,"outbound":%v}`,
			cfg.Storage.Backend, cfg.Directory.Mode, cfg.Rspamd.URL != "", cfg.Outbound.Enabled)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	health := &http.Server{Addr: cfg.HealthAddr, Handler: mux}

	servers := []*http.Server{health}
	if err := serveHTTP(ctx, health, logger); err != nil {
		logger.Error("listen", "component", "health", "addr", cfg.HealthAddr, "err", err)
		return 2
	}
	logger.Info("listening", "component", "health", "addr", cfg.HealthAddr)

	if cfg.Management.Addr != "" {
		mgmtHandler := management.WithSecret(
			management.NewHandler(management.Info{
				Version:       version.Version,
				Storage:       cfg.Storage.Backend,
				DirectoryMode: cfg.Directory.Mode,
				AuthMode:      cfg.Auth.Mode,
				StartedAt:     startedAt,
			}, qm, st.mailbox, st.facade, logger),
			cfg.Management.Secret,
		)
		mgmt := &http.Server{Addr: cfg.Management.Addr, Handler: mgmtHandler}
		servers = append(servers, mgmt)
		if err := serveHTTP(ctx, mgmt, logger); err != nil {
			logger.Error("listen", "component", "management", "addr", cfg.Management.Addr, "err", err)
			return 2
		}
		logger.Info("listening", "component", "management", "addr", cfg.Management.Addr)
	}

	// SMTP listeners (inbound + submission). TLS is terminated by the
	// mailez gateway; direct deployments must add a reverse proxy.
	smtpServers := []*gosmtp.Server{smtpInbound, smtpSubmission}
	proxySMTP := proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTP)
	proxySubmission := proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.Submission)
	proxyIMAP := proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAP)
	if err := serveSMTP(ctx, smtpInbound, cfg.Listeners.SMTP, cfg.Limits.MaxConnections, proxySMTP, nil, logger); err != nil {
		logger.Error("listen", "component", "smtp", "addr", cfg.Listeners.SMTP, "err", err)
		return 2
	}
	if err := serveSMTP(ctx, smtpSubmission, cfg.Listeners.Submission, cfg.Limits.MaxConnections, proxySubmission, nil, logger); err != nil {
		logger.Error("listen", "component", "submission", "addr", cfg.Listeners.Submission, "err", err)
		return 2
	}
	if err := serveTCP(ctx, imapSrv, "imap", cfg.Listeners.IMAP, cfg.Limits.MaxConnections, proxyIMAP, nil, logger); err != nil {
		logger.Error("listen", "component", "imap", "addr", cfg.Listeners.IMAP, "err", err)
		return 2
	}
	// Implicit-TLS variants (RFC 8314): submissions:465 / imaps:993 /
	// pop3s:995. The engine terminates TLS itself, so no gateway is needed
	// in front of these ports (the reference server deployment model).
	if cfg.Listeners.SMTPS != "" && tlsConf != nil {
		if err := serveSMTP(ctx, smtpSubmission, cfg.Listeners.SMTPS, cfg.Limits.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTPS), tlsConf, logger); err != nil {
			logger.Error("listen", "component", "smtps", "addr", cfg.Listeners.SMTPS, "err", err)
			return 2
		}
	}
	if cfg.Listeners.IMAPS != "" && tlsConf != nil {
		if err := serveTCP(ctx, imapSrv, "imaps", cfg.Listeners.IMAPS, cfg.Limits.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAPS), tlsConf, logger); err != nil {
			logger.Error("listen", "component", "imaps", "addr", cfg.Listeners.IMAPS, "err", err)
			return 2
		}
	}
	msieve := &server.Listener{
		Name:          "managesieve",
		Addr:          cfg.Listeners.ManageSieve,
		MaxConn:       cfg.Limits.MaxConnections,
		ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.ManageSieve),
		Logger:        logger,
		Handler:       manageSieve.ManageSieveSession,
	}
	go func() {
		if err := msieve.Serve(ctx); err != nil && ctx.Err() == nil {
			logger.Error("managesieve server", "addr", cfg.Listeners.ManageSieve, "err", err)
		}
	}()
	logger.Info("mail path ready", "inbound", cfg.Listeners.SMTP, "submission", cfg.Listeners.Submission,
		"imap", cfg.Listeners.IMAP, "managesieve", cfg.Listeners.ManageSieve)
	if cfg.Features.POP3Enabled {
		pop3L := &server.Listener{
			Name:          "pop3",
			Addr:          cfg.Listeners.POP3,
			MaxConn:       cfg.Limits.MaxConnections,
			ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.POP3),
			Logger:        logger,
			Handler:       pop3Srv.ServeConn,
		}
		go func() {
			if err := pop3L.Serve(ctx); err != nil && ctx.Err() == nil {
				logger.Error("pop3 server", "addr", cfg.Listeners.POP3, "err", err)
			}
		}()
		if cfg.Listeners.POP3S != "" && tlsConf != nil {
			pop3sL := &server.Listener{
				Name:          "pop3s",
				Addr:          cfg.Listeners.POP3S,
				MaxConn:       cfg.Limits.MaxConnections,
				ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.POP3S),
				TLSConfig:     tlsConf,
				Logger:        logger,
				Handler:       pop3Srv.ServeConn,
			}
			go func() {
				if err := pop3sL.Serve(ctx); err != nil && ctx.Err() == nil {
					logger.Error("pop3s server", "addr", cfg.Listeners.POP3S, "err", err)
				}
			}()
		}
	}

	<-ctx.Done()

	logger.Info("shutting down")
	// Drain the queue workers before closing storage (Pebble/RocksDB must
	// not be touched after Close).
	if qmDone != nil {
		<-qmDone
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range smtpServers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown", "component", "smtp", "err", err)
		}
	}
	imapSrv.Close()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown", "component", srv.Addr, "err", err)
			return 1
		}
	}
	logger.Info("stopped")
	return 0
}

// serveSMTP binds addr and serves; the server is drained in the shutdown
// phase via Shutdown. A non-nil tlsConf turns the listener into implicit
// TLS (RFC 8314 submissions port), negotiated before the first SMTP byte.
func serveSMTP(ctx context.Context, srv *gosmtp.Server, addr string, maxConn int, proxy bool, tlsConf *tls.Config, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "component", "smtp", "addr", ln.Addr().String())
	var lim net.Listener = server.NewLimitListener(ln, maxConn)
	if proxy {
		lim = server.NewProxyListener(lim, logger)
	}
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
func serveTCP(ctx context.Context, srv tcpServer, name, addr string, maxConn int, proxy bool, tlsConf *tls.Config, logger *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logger.Info("listening", "component", name, "addr", ln.Addr().String())
	var lim net.Listener = server.NewLimitListener(ln, maxConn)
	if proxy {
		lim = server.NewProxyListener(lim, logger)
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

// newSubmit routes envelope recipients: local addresses go through the
// delivery pipeline; external addresses are spooled for relay when the
// outbound queue is enabled.
func newSubmit(dir directory.Service, pipeline *delivery.Pipeline, qm *queue.Manager, logger *slog.Logger) func(context.Context, net.IP, string, string, []string, []byte) error {
	return func(ctx context.Context, peer net.IP, user, from string, to []string, data []byte) error {
		// Outbound mail never carries internal Received chains or client
		// fingerprints collected on the way in.
		data = delivery.Outclean(data)
		var local, relay []string
		for _, rcpt := range to {
			targets, err := dir.Aliases(ctx, rcpt)
			if err == nil && len(targets) > 0 {
				local = append(local, rcpt)
			} else if qm != nil {
				relay = append(relay, rcpt)
			} else {
				return fmt.Errorf("smtp: outbound relay disabled for %s", rcpt)
			}
		}
		if len(local) > 0 {
			if err := pipeline.Deliver(ctx, peer, from, local, data); err != nil {
				return err
			}
		}
		if len(relay) > 0 {
			// SRS: rewrite the envelope sender when relaying mail that did
			// not originate locally, so bounces route back through us.
			relayFrom := from
			if user == "" || user != from {
				if rewritten, err := dir.SRSForward(ctx, from); err == nil && rewritten != "" {
					relayFrom = rewritten
				} else if err != nil && !errors.Is(err, directory.ErrNotFound) {
					logger.Warn("smtp: srs forward", "from", from, "err", err)
				}
			}
			if _, err := qm.Submit(ctx, relayFrom, relay, subjectOf(data), bytes.NewReader(data)); err != nil {
				return err
			}
			logger.Info("queued outbound", "from", relayFrom, "to", relay, "bytes", len(data))
		}
		return nil
	}
}

// opportunisticSigner degrades a DKIM signing failure to unsigned delivery
// (signing is best-effort: a vault outage must not stop mail).
type opportunisticSigner struct {
	s      *dkim.Signer
	logger *slog.Logger
}

func (o opportunisticSigner) Sign(ctx context.Context, from string, msg []byte) ([]byte, error) {
	signed, err := o.s.Sign(ctx, from, msg)
	if err != nil {
		o.logger.Warn("dkim: sign failed; sending unsigned", "from", from, "err", err)
		return msg, nil
	}
	return signed, nil
}

// subjectOf extracts the first Subject header for queue metadata.
func subjectOf(data []byte) string {
	for _, line := range strings.Split(string(data), "\r\n") {
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Subject:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseNets converts validated CIDR strings to IPNets.
func parseNets(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range cidrs {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
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

// localIP picks a non-loopback IPv4 for our hostname (used by SPF %{c}).
func localIP(hostname string) net.IP {
	if addrs, err := net.LookupIP(hostname); err == nil {
		for _, ip := range addrs {
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4
			}
		}
		for _, ip := range addrs {
			if ip4 := ip.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return net.IPv4(127, 0, 0, 1)
}

// openHAStore picks the leadership lease store: an explicit FS path (shared
// volume) first, then the S3 bucket when configured.
func openHAStore(cfg config.Config, logger *slog.Logger) (ha.Store, error) {
	if cfg.HA.LeasePath != "" {
		logger.Info("ha: lease store", "backend", "fs", "path", cfg.HA.LeasePath)
		return ha.NewFSStore(cfg.HA.LeasePath), nil
	}
	if cfg.Storage.S3Endpoint != "" {
		logger.Info("ha: lease store", "backend", "s3", "bucket", cfg.Storage.S3Bucket)
		return ha.NewS3Store(cfg.Storage.S3Endpoint, cfg.Storage.S3AccessKey,
			cfg.Storage.S3SecretKey, cfg.Storage.S3Bucket, "mailezine/lease", cfg.Storage.S3UseSSL)
	}
	return nil, errors.New("ha: enabled but no shared lease storage (set MAILEZINE_HA_LEASE_PATH or MAILEZINE_S3_*)")
}

func newDirectory(cfg config.Config, logger *slog.Logger) (directory.Service, error) {
	switch cfg.Directory.Mode {
	case "dev":
		d, err := directory.LoadDevFile(cfg.Directory.File)
		if err != nil {
			return nil, err
		}
		logger.Info("directory", "mode", "dev", "file", cfg.Directory.File)
		return d, nil
	case "mailez":
		base := "http://" + cfg.BackendAddress + "/stack/directory"
		logger.Info("directory", "mode", "mailez", "base", base, "cacheTTL", cfg.Directory.CacheTTL)
		return directory.NewMailez(base, cfg.Directory.CacheTTL), nil
	default:
		return nil, fmt.Errorf("unsupported directory mode %q", cfg.Directory.Mode)
	}
}

func newAuth(cfg config.Config, logger *slog.Logger) (auth.Service, error) {
	switch cfg.Auth.Mode {
	case "dev":
		a, err := auth.LoadDevFile(cfg.Auth.DevPasswordsFile)
		if err != nil {
			return nil, err
		}
		logger.Info("auth", "mode", "dev", "file", cfg.Auth.DevPasswordsFile)
		return a, nil
	case "mailez":
		base := "http://" + cfg.BackendAddress + "/stack"
		logger.Info("auth", "mode", "mailez", "base", base)
		return auth.NewMailez(base), nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.Auth.Mode)
	}
}
