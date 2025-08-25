// Package app is the composition root of mailezine: it loads configuration,
// wires every service (storage, delivery, queue, protocol servers) and runs
// their lifecycle. The main package only parses flags, installs signal
// handling and translates errors into exit codes.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/auth"
	"mailezine/internal/config"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/fts"
	"mailezine/internal/ha"
	"mailezine/internal/imap"
	"mailezine/internal/imapserver"
	"mailezine/internal/mailcache"
	"mailezine/internal/mailstore"
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

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// App is the fully assembled engine. New completes all startup work that can
// fail (leadership acquisition, storage open, service wiring); Run serves
// until ctx is cancelled and then drains gracefully. Close releases the
// durable backends.
type App struct {
	cfg    config.Config
	ctx    context.Context
	logger *slog.Logger
	m      *metrics.Metrics

	dir        directory.Service
	auth       auth.Service
	st         *Storage
	fts        *fts.Indexer
	pipeline   *delivery.Pipeline
	classifier *spam.Client
	qm         *queue.Manager
	qmDone     chan struct{}

	leader   *ha.Leader
	haCtx    context.Context
	haCancel context.CancelFunc

	startedAt time.Time

	smtpInbound    *gosmtp.Server
	smtpSubmission *gosmtp.Server
	imapSrv        *imapserver.Server
	sieveSrv       *sieve.Server
	pop3Srv        *pop3.Server
	healthSrv      *http.Server
	mgmtSrv        *http.Server
}

// New assembles every service. It blocks while a follower waits for the HA
// lease, so the returned App is ready to serve.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	logger := telemetry.NewLogger(cfg.Log.Level, cfg.Log.Format)
	m := metrics.New()
	logger.Info("starting", "version", version.Version, "summary", cfg.Summary())
	a := &App{cfg: cfg, ctx: ctx, logger: logger, m: m, startedAt: time.Now()}

	if err := a.acquireLeadership(ctx); err != nil {
		return nil, err
	}
	if err := a.openServices(); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.wirePipeline(); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.wireServers(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Run starts every listener and blocks until ctx is cancelled, then drains
// the queue and shuts the servers down gracefully.
func (a *App) Run(ctx context.Context) error {
	if err := a.serveListeners(ctx); err != nil {
		return err
	}
	<-ctx.Done()

	a.logger.Info("shutting down")
	// Drain queue workers before closing storage (Pebble/RocksDB must not
	// be touched after Close).
	if a.qmDone != nil {
		<-a.qmDone
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range []*gosmtp.Server{a.smtpInbound, a.smtpSubmission} {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("shutdown", "component", "smtp", "err", err)
		}
	}
	if a.imapSrv != nil {
		a.imapSrv.Close()
	}
	for _, srv := range []*http.Server{a.healthSrv, a.mgmtSrv} {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("shutdown", "component", srv.Addr, "err", err)
			return err
		}
	}
	a.logger.Info("stopped")
	return nil
}

// Close releases the durable backends and the leadership lease. Idempotent.
func (a *App) Close() {
	if a.haCancel != nil {
		a.haCancel()
	}
	if a.leader != nil {
		_ = a.leader.Release(context.Background())
	}
	if a.fts != nil {
		_ = a.fts.Close()
	}
	if a.st != nil {
		_ = a.st.Close()
	}
	if a.auth != nil {
		_ = a.auth.Close()
	}
	if a.dir != nil {
		_ = a.dir.Close()
	}
}

// acquireLeadership takes the active-passive lease before the single-writer
// KV is opened; followers poll until the lease expires (D31).
func (a *App) acquireLeadership(ctx context.Context) error {
	if !a.cfg.HA.Enabled {
		return nil
	}
	store, err := openHAStore(a.cfg, a.logger)
	if err != nil {
		return fmt.Errorf("ha: lease store: %w", err)
	}
	owner := a.cfg.Hostname + "-" + fmt.Sprint(os.Getpid())
	ttl := time.Duration(a.cfg.HA.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	a.haCtx, a.haCancel = context.WithCancel(context.Background())
	leader := ha.NewLeader(store, owner, ttl, a.logger)
	a.leader = leader
	for {
		if err := leader.TryAcquire(a.haCtx); err == nil {
			break
		} else if !errors.Is(err, ha.ErrNotLeader) {
			return fmt.Errorf("ha: lease acquire: %w", err)
		}
		a.logger.Info("ha: standby, waiting for leadership", "owner", owner)
		select {
		case <-a.haCtx.Done():
			return errors.New("ha: interrupted while waiting for leadership")
		case <-time.After(ttl / 2):
		}
	}
	a.logger.Info("ha: leadership acquired", "owner", owner, "ttl", ttl)
	go leader.Run(a.haCtx)
	return nil
}

// openServices opens the directory, auth and storage backends plus FTS.
func (a *App) openServices() error {
	var err error
	if a.dir, err = newDirectory(a.cfg, a.logger); err != nil {
		return err
	}
	if a.auth, err = newAuth(a.cfg, a.logger); err != nil {
		return err
	}
	// Successful authentications are memoised briefly (credential-keyed, so
	// a cache hit only replays the exact pair that succeeded); this cuts the
	// control-plane round trip from the hot AUTH path.
	a.auth = auth.NewCached(a.auth,
		mailcache.NewCacheWithTTL(a.cfg.CacheSizeBytes, 30*time.Second))
	if a.st, err = NewStorage(a.cfg, a.logger); err != nil {
		return err
	}
	// Metadata cache: message/mailbox lists are memoized per account with a
	// TTL safety net; every write path invalidates the affected mailboxes.
	if a.cfg.CacheSizeBytes > 0 {
		a.st.mailbox = mailstore.NewCached(a.st.mailbox,
			mailcache.NewCacheWithTTL(a.cfg.CacheSizeBytes, 30*time.Second))
	}
	// Embedded full-text index (bleve); a failure disables FTS but never
	// startup (search falls back to the scan).
	if a.cfg.FTS.Enabled {
		a.fts = openFTS(a.cfg, a.logger)
	}
	return nil
}

// wirePipeline builds the inbound pipeline and the outbound queue.
func (a *App) wirePipeline() error {
	verifier := &verify.Verifier{
		Logger:   a.logger,
		Resolver: newSystemResolver(),
		Hostname: a.cfg.Hostname,
		LocalIP:  localIP(a.cfg.Hostname),
	}
	var classifier *spam.Client
	if a.cfg.Rspamd.URL != "" {
		classifier = spam.New(a.cfg.Rspamd.URL, a.cfg.Rspamd.LearnURL, a.cfg.Rspamd.Password, a.cfg.Hostname, a.logger)
	}
	a.classifier = classifier
	a.pipeline = &delivery.Pipeline{
		Directory:          a.dir,
		Store:              a.st.mailbox,
		Verifier:           verifier,
		Sieve:              sieve.NewEngine(a.logger),
		ScriptSource:       sieve.DefaultScriptSource{Store: a.st.mailbox, Directory: a.dir},
		Hostname:           a.cfg.Hostname,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		Logger:             a.logger,
		FTS:                a.fts,
	}
	if classifier != nil {
		// Never assign a typed nil to the interface: an unconfigured
		// classifier must leave the field nil (delivery fails open).
		a.pipeline.Classifier = classifier
	}
	if err := a.wireQueue(); err != nil {
		return err
	}
	return nil
}

// wireServers builds the protocol servers (listeners start in Run).
func (a *App) wireServers() error {
	trustedNets, err := parseNets(a.cfg.TrustedNets)
	if err != nil {
		return err
	}
	tlsConf, err := loadTLS(a.cfg, a.logger)
	if err != nil {
		return err
	}
	submit := newSubmit(a.dir, a.pipeline, a.qm, a.logger)

	a.smtpInbound = smtp.NewServer(&smtp.Backend{
		Hostname:           a.cfg.Hostname,
		Directory:          a.dir,
		Auth:               a.auth,
		TrustedNets:        trustedNets,
		AllowRelay:         a.cfg.Outbound.Enabled,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		MaxRecipients:      a.cfg.Limits.MaxRecipients,
		MaxMessageBytes:    a.cfg.Limits.MaxMessageSize,
		MaxLineLength:      a.cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             a.logger,
		Submit:             submit,
	})
	a.smtpSubmission = smtp.NewServer(&smtp.Backend{
		Hostname:           a.cfg.Hostname,
		Directory:          a.dir,
		Auth:               a.auth,
		TrustedNets:        trustedNets,
		RequireAuth:        true,
		AllowRelay:         true,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		MaxRecipients:      a.cfg.Limits.MaxRecipients,
		MaxMessageBytes:    a.cfg.Limits.MaxMessageSize,
		MaxLineLength:      a.cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             a.logger,
		Submit:             submit,
	})

	imapCfg := &imap.Server{
		Store:           a.st.mailbox,
		Auth:            a.auth,
		Directory:       a.dir,
		MaxMessageBytes: a.cfg.Limits.MaxMessageSize,
		TLSConfig:       tlsConf,
		Logger:          a.logger,
		FTS:             a.fts,
		CacheSizeBytes:  a.cfg.CacheSizeBytes,
	}
	if classifier := a.classifier; classifier != nil {
		// Junk-boundary learning on APPEND/COPY/MOVE across Junk.
		learn := classifier
		imapCfg.Learn = func(ctx context.Context, _ string, isSpam bool, data []byte) {
			if err := learn.LearnWithFuzzy(ctx, isSpam, data); err != nil {
				a.logger.Error("imap: rspamd learn", "isSpam", isSpam, "err", err)
			}
		}
	}
	a.imapSrv = imap.New(imapCfg)
	a.sieveSrv = &sieve.Server{
		Auth:      a.auth,
		Directory: a.dir,
		Scripts:   a.st.mailbox,
		TLSConfig: tlsConf,
		Logger:    a.logger,
	}
	a.pop3Srv = &pop3.Server{
		Store:     a.st.mailbox,
		Auth:      a.auth,
		Directory: a.dir,
		TLSConfig: tlsConf,
		Logger:    a.logger,
	}
	return nil
}

// serveListeners binds and serves every HTTP and protocol listener.
func (a *App) serveListeners(ctx context.Context) error {
	cfg := a.cfg
	a.healthSrv = a.healthServer()
	if err := serveHTTP(ctx, a.healthSrv, a.logger); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	a.logger.Info("listening", "component", "health", "addr", cfg.HealthAddr)

	if cfg.Management.Addr != "" {
		mgmtHandler := management.WithSecret(
			management.NewHandler(management.Info{
				Version:       version.Version,
				Storage:       cfg.Storage.Backend,
				DirectoryMode: cfg.Directory.Mode,
				AuthMode:      cfg.Auth.Mode,
				StartedAt:     a.startedAt,
			}, a.qm, a.st.mailbox, a.st.facade, a.logger),
			cfg.Management.Secret,
		)
		a.mgmtSrv = &http.Server{Addr: cfg.Management.Addr, Handler: mgmtHandler}
		if err := serveHTTP(ctx, a.mgmtSrv, a.logger); err != nil {
			return fmt.Errorf("management: %w", err)
		}
		a.logger.Info("listening", "component", "management", "addr", cfg.Management.Addr)
	}

	if err := a.serveMailListeners(ctx); err != nil {
		return err
	}
	a.logger.Info("mail path ready", "inbound", cfg.Listeners.SMTP, "submission", cfg.Listeners.Submission,
		"imap", cfg.Listeners.IMAP, "managesieve", cfg.Listeners.ManageSieve)
	return nil
}

// healthServer builds the health/readiness/metrics mux.
func (a *App) healthServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/health").Inc()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version.Version)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/ready").Inc()
		fmt.Fprintf(w, `{"status":"ok","storage":%q,"directory":%q,"rspamd":%v,"outbound":%v}`,
			a.cfg.Storage.Backend, a.cfg.Directory.Mode, a.cfg.Rspamd.URL != "", a.cfg.Outbound.Enabled)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(a.m.Registry, promhttp.HandlerOpts{}))
	return &http.Server{Addr: a.cfg.HealthAddr, Handler: mux}
}

// serveMailListeners starts SMTP (plain + implicit TLS), IMAP, ManageSieve
// and POP3 listeners.
func (a *App) serveMailListeners(ctx context.Context) error {
	cfg := a.cfg
	lim := cfg.Limits
	if err := serveSMTP(ctx, a.smtpInbound, cfg.Listeners.SMTP, lim.MaxConnections,
		proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTP), nil, a.logger); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := serveSMTP(ctx, a.smtpSubmission, cfg.Listeners.Submission, lim.MaxConnections,
		proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.Submission), nil, a.logger); err != nil {
		return fmt.Errorf("submission: %w", err)
	}
	if err := serveTCP(ctx, a.imapSrv, "imap", cfg.Listeners.IMAP, lim.MaxConnections,
		proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAP), nil, a.logger); err != nil {
		return fmt.Errorf("imap: %w", err)
	}
	tlsConf, _ := loadTLS(cfg, a.logger)
	if cfg.Listeners.SMTPS != "" && tlsConf != nil {
		if err := serveSMTP(ctx, a.smtpSubmission, cfg.Listeners.SMTPS, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTPS), tlsConf, a.logger); err != nil {
			return fmt.Errorf("smtps: %w", err)
		}
	}
	if cfg.Listeners.IMAPS != "" && tlsConf != nil {
		if err := serveTCP(ctx, a.imapSrv, "imaps", cfg.Listeners.IMAPS, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAPS), tlsConf, a.logger); err != nil {
			return fmt.Errorf("imaps: %w", err)
		}
	}
	msieve := &server.Listener{
		Name:          "managesieve",
		Addr:          cfg.Listeners.ManageSieve,
		MaxConn:       lim.MaxConnections,
		ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.ManageSieve),
		Logger:        a.logger,
		Handler:       a.sieveSrv.ManageSieveSession,
	}
	go func() {
		if err := msieve.Serve(ctx); err != nil && ctx.Err() == nil {
			a.logger.Error("managesieve server", "addr", cfg.Listeners.ManageSieve, "err", err)
		}
	}()
	if cfg.Features.POP3Enabled {
		pop3L := &server.Listener{
			Name:          "pop3",
			Addr:          cfg.Listeners.POP3,
			MaxConn:       lim.MaxConnections,
			ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.POP3),
			Logger:        a.logger,
			Handler:       a.pop3Srv.ServeConn,
		}
		go func() {
			if err := pop3L.Serve(ctx); err != nil && ctx.Err() == nil {
				a.logger.Error("pop3 server", "addr", cfg.Listeners.POP3, "err", err)
			}
		}()
		if cfg.Listeners.POP3S != "" && tlsConf != nil {
			pop3sL := &server.Listener{
				Name:          "pop3s",
				Addr:          cfg.Listeners.POP3S,
				MaxConn:       lim.MaxConnections,
				ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.POP3S),
				TLSConfig:     tlsConf,
				Logger:        a.logger,
				Handler:       a.pop3Srv.ServeConn,
			}
			go func() {
				if err := pop3sL.Serve(ctx); err != nil && ctx.Err() == nil {
					a.logger.Error("pop3s server", "addr", cfg.Listeners.POP3S, "err", err)
				}
			}()
		}
	}
	return nil
}
