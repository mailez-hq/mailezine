// Package app is the composition root of mailezine: it loads configuration,
// wires every service (storage, delivery, queue, protocol servers) and runs
// their lifecycle. The main package only parses flags, installs signal
// handling and translates errors into exit codes.
//
// Lifecycle model:
//
//	non-HA: New assembles every service eagerly (startup failures surface
//	        from New); Run binds all listeners and serves until cancelled.
//	HA:     New bootstraps the lease infrastructure only; the instance binds
//	        its health endpoint immediately (a standby answers probes and
//	        reports role=standby) and activates the full write path only
//	        while it holds the lease. Losing the lease — a foreign holder or
//	        the fencing epoch moving past us detected at renewal — drains
//	        SMTP gracefully, stops every writer and loops back to standby.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/archive"
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

// App is the fully assembled engine. Depending on mode, New completes part
// or all of the startup work that can fail; Run serves until ctx is
// cancelled and then drains gracefully. Close releases durable backends and
// is safe to call in any lifecycle state.
type App struct {
	cfg    config.Config
	ctx    context.Context // root context supplied at construction
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
	arch       *archive.Spool

	tlsConf   *tls.Config
	startedAt time.Time

	smtpInbound    *gosmtp.Server
	smtpSubmission *gosmtp.Server
	imapSrv        *imapserver.Server
	sieveSrv       *sieve.Server
	pop3Srv        *pop3.Server
	healthSrv      *http.Server
	mgmtSrv        *http.Server

	leader     *ha.Leader
	haCtx      context.Context    // app-lifetime; cancelled by Close
	haCancel   context.CancelFunc // cancels haCtx
	termCtx    context.Context    // current HA term scope (listeners, queue)
	termCancel context.CancelFunc // cancels termCtx

	ready      atomic.Bool // write path bound and served
	assembled  bool        // services currently open on this process
	teardownMu sync.Mutex  // serializes teardown paths (term loss + Close)
}

// New builds the engine.
//
//	non-HA: assembles everything that can fail here.
//	HA:     bootstraps the lease store only; a follower returns immediately
//	        instead of blocking, so its health endpoint can come up while it
//	        waits for leadership.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	logger := telemetry.NewLogger(cfg.Log.Level, cfg.Log.Format)
	m := metrics.New()
	logger.Info("starting", "version", version.Version, "summary", cfg.Summary())
	a := &App{cfg: cfg, ctx: ctx, logger: logger, m: m, startedAt: time.Now()}

	if cfg.HA.Enabled {
		if err := a.bootstrapHA(); err != nil {
			return nil, err
		}
		return a, nil
	}
	if err := a.openServices(); err != nil {
		a.Close()
		return nil, err
	}
	if err := a.wirePipeline(a.ctx); err != nil {
		a.Close()
		return nil, err
	}
	a.wireArchive(a.ctx)
	if err := a.wireServers(); err != nil {
		a.Close()
		return nil, err
	}
	a.assembled = true
	a.ready.Store(true)
	return a, nil
}

// Run starts every listener and blocks until ctx is cancelled, then drains
// gracefully. In HA mode the instance additionally loops through
// standby→leader terms until ctx ends.
func (a *App) Run(ctx context.Context) error {
	if err := a.serveHealth(ctx); err != nil {
		return err
	}
	a.logger.Info("listening", "component", "health", "addr", a.cfg.HealthAddr)

	var runErr error
	if a.cfg.HA.Enabled {
		runErr = a.superviseTerms(ctx)
	} else if err := a.serveManagement(ctx); err != nil {
		runErr = err
	} else if err := a.serveMailListeners(ctx); err != nil {
		runErr = err
	} else {
		a.logger.Info("mail path ready", "inbound", a.cfg.Listeners.SMTP,
			"submission", a.cfg.Listeners.Submission, "imap", a.cfg.Listeners.IMAP,
			"managesieve", a.cfg.Listeners.ManageSieve)
		<-ctx.Done()
	}

	a.logger.Info("shutting down")
	// HA terms are torn down by their own supervisor before it returns; the
	// non-HA assembly is drained here. Both paths are idempotent.
	a.stopTerm("")
	a.shutdownControlServers()
	a.logger.Info("stopped")
	return runErr
}

// shutdownControlServers stops the process-wide HTTP servers that outlive
// individual HA terms.
func (a *App) shutdownControlServers() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a.healthSrv != nil {
		if err := a.healthSrv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("shutdown", "component", "health", "err", err)
		}
	}
}

// Close releases durable backends and any held leadership lease. Idempotent;
// safe whether or not Run completed.
func (a *App) Close() {
	a.teardownMu.Lock()
	defer a.teardownMu.Unlock()
	a.closeTermBackendsLocked()
	if a.haCancel != nil {
		a.haCancel()
	}
}

// ---------------------------------------------------------------------------
// Service assembly (shared by non-HA eager start and per-term activation).
// ---------------------------------------------------------------------------

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
	if a.cfg.AuthCacheSizeBytes > 0 {
		a.auth = auth.NewCached(a.auth,
			mailcache.NewCacheWithTTL(a.cfg.AuthCacheSizeBytes, 30*time.Second))
	}
	if a.st, err = NewStorage(a.cfg, a.logger); err != nil {
		return err
	}
	// One-shot backfill of the KV secondary indexes (name and per-mailbox
	// email indexes). Idempotent — existing entries are only rewritten when
	// divergent — so it is safe to repeat per HA term. Async: never block
	// start-up on a large store.
	if kvms, ok := a.st.mailbox.(*mailstore.KV); ok {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			n, err := kvms.Reindex(ctx)
			switch {
			case err != nil:
				a.logger.Warn("storage: index backfill incomplete", "fixed", n, "err", err)
			case n > 0:
				a.logger.Info("storage: index backfill", "entries", n)
			}
		}()
	}
	// Metadata cache: message/mailbox lists are memoized per account with a
	// TTL safety net; every write path invalidates the affected mailboxes.
	if a.cfg.MetaCacheSizeBytes > 0 {
		a.st.mailbox = mailstore.NewCached(a.st.mailbox,
			mailcache.NewCacheWithTTL(a.cfg.MetaCacheSizeBytes, 30*time.Second))
	}
	// Embedded full-text index (bleve); a failure disables FTS but never
	// startup (search falls back to the scan).
	if a.cfg.FTS.Enabled {
		a.fts = openFTS(a.cfg, a.logger)
	}
	return nil
}

// wirePipeline builds the inbound pipeline and the outbound queue under
// runCtx (per-term when HA is enabled).
func (a *App) wirePipeline(runCtx context.Context) error {
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
	return a.wireQueue(runCtx)
}

// wireArchive builds the compliance-capture spool when enabled and starts
// its forwarding worker under runCtx.
func (a *App) wireArchive(runCtx context.Context) {
	if !a.cfg.Archive.Enabled || a.st == nil {
		return
	}
	a.arch = archive.New(a.st.Facade(), a.cfg.Archive.URL, a.cfg.Archive.MaxAttempts, a.logger, a.cfg.StackSecret)
	a.arch.Run(runCtx)
	a.logger.Info("archive", "enabled", true, "endpoint", a.cfg.Archive.URL)
}

// wireServers builds the protocol servers (listeners start later).
func (a *App) wireServers() error {
	trustedNets, err := parseNets(a.cfg.TrustedNets)
	if err != nil {
		return err
	}
	tlsConf, err := loadTLS(a.cfg, a.logger)
	if err != nil {
		return err
	}
	a.tlsConf = tlsConf
	submit := newSubmit(a.dir, a.pipeline, a.qm, a.logger)
	submitInbound := submit
	submitOutbound := submit
	if a.arch != nil {
		submitInbound = a.arch.Wrap("inbound", submit)
		submitOutbound = a.arch.Wrap("outbound", submit)
	}

	a.smtpInbound = smtp.NewServer(&smtp.Backend{
		Hostname:           a.cfg.Hostname,
		Directory:          a.dir,
		Auth:               a.auth,
		Port:               listenerPort(a.cfg.Listeners.SMTP),
		TrustedNets:        trustedNets,
		AllowRelay:         a.cfg.Outbound.Enabled,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		MaxRecipients:      a.cfg.Limits.MaxRecipients,
		MaxMessageBytes:    a.cfg.Limits.MaxMessageSize,
		MaxLineLength:      a.cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             a.logger,
		Submit:             submitInbound,
	})
	a.smtpSubmission = smtp.NewServer(&smtp.Backend{
		Hostname:           a.cfg.Hostname,
		Directory:          a.dir,
		Auth:               a.auth,
		Port:               listenerPort(a.cfg.Listeners.Submission),
		TrustedNets:        trustedNets,
		RequireAuth:        true,
		AllowRelay:         true,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		MaxRecipients:      a.cfg.Limits.MaxRecipients,
		MaxMessageBytes:    a.cfg.Limits.MaxMessageSize,
		MaxLineLength:      a.cfg.Limits.MaxLineLength,
		TLSConfig:          tlsConf,
		Logger:             a.logger,
		Submit:             submitOutbound,
	})

	imapCfg := &imap.Server{
		Store:           a.st.mailbox,
		Auth:            a.auth,
		Directory:       a.dir,
		Port:            listenerPort(a.cfg.Listeners.IMAP),
		MaxMessageBytes: a.cfg.Limits.MaxMessageSize,
		TLSConfig:       tlsConf,
		Logger:          a.logger,
		FTS:             a.fts,
		CacheSizeBytes:  a.cfg.IMAPCacheSizeBytes,
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
		Port:      listenerPort(a.cfg.Listeners.ManageSieve),
		TLSConfig: tlsConf,
		Logger:    a.logger,
	}
	a.pop3Srv = &pop3.Server{
		Store:     a.st.mailbox,
		Auth:      a.auth,
		Directory: a.dir,
		Port:      listenerPort(a.cfg.Listeners.POP3),
		TLSConfig: tlsConf,
		Logger:    a.logger,
	}
	return nil
}

// listenerPort extracts the port from a "host:port" listen address (":1143"
// → "1143"). Empty on malformed input.
func listenerPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// ---------------------------------------------------------------------------
// HTTP endpoints.
// ---------------------------------------------------------------------------

// serveHealth binds the health/readiness/metrics endpoint. It runs in every
// lifecycle state: a standby answers probes too, reporting its role, so an
// orchestrator no longer mistakes "waiting for the lease" for a dead pod.
func (a *App) serveHealth(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/health").Inc()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"role":%q}`,
			version.Version, a.role())
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/ready").Inc()
		fmt.Fprintf(w, `{"status":"ok","role":%q,"storage":%q,"directory":%q,"rspamd":%v,"outbound":%v}`,
			a.role(), a.cfg.Storage.Backend, a.cfg.Directory.Mode,
			a.cfg.Rspamd.URL != "", a.cfg.Outbound.Enabled)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(a.m.Registry, promhttp.HandlerOpts{}))
	a.healthSrv = &http.Server{Addr: a.cfg.HealthAddr, Handler: mux}
	return serveHTTP(ctx, a.healthSrv, a.logger)
}

func (a *App) role() string {
	if a.ready.Load() {
		return "leader"
	}
	return "standby"
}

// serveManagement binds the management API when configured. In HA mode it
// belongs to the leader term (it exposes the live queue manager).
func (a *App) serveManagement(ctx context.Context) error {
	if a.cfg.Management.Addr == "" {
		return nil
	}
	handler := management.WithSecret(
		management.NewHandler(management.Info{
			Version:       version.Version,
			Storage:       a.cfg.Storage.Backend,
			DirectoryMode: a.cfg.Directory.Mode,
			AuthMode:      a.cfg.Auth.Mode,
			StartedAt:     a.startedAt,
		}, a.qm, a.st.mailbox, a.st.facade, a.logger),
		a.cfg.Management.Secret,
	)
	a.mgmtSrv = &http.Server{Addr: a.cfg.Management.Addr, Handler: handler}
	if err := serveHTTP(ctx, a.mgmtSrv, a.logger); err != nil {
		return fmt.Errorf("management: %w", err)
	}
	a.logger.Info("listening", "component", "management", "addr", a.cfg.Management.Addr)
	return nil
}

// ---------------------------------------------------------------------------
// Mail listeners.
// ---------------------------------------------------------------------------

// serveMailListeners starts SMTP (plain + implicit TLS), IMAP, ManageSieve
// and POP3 listeners for the given scope. Empty addresses are skipped so a
// deployment can bring up a subset of listeners.
func (a *App) serveMailListeners(ctx context.Context) error {
	cfg := a.cfg
	lim := cfg.Limits
	if cfg.Listeners.SMTP != "" {
		if err := serveSMTP(ctx, a.smtpInbound, cfg.Listeners.SMTP, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTP), nil, a.logger); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	if cfg.Listeners.Submission != "" {
		if err := serveSMTP(ctx, a.smtpSubmission, cfg.Listeners.Submission, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.Submission), nil, a.logger); err != nil {
			return fmt.Errorf("submission: %w", err)
		}
	}
	if cfg.Listeners.IMAP != "" {
		if err := serveTCP(ctx, a.imapSrv, "imap", cfg.Listeners.IMAP, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAP), nil, a.logger); err != nil {
			return fmt.Errorf("imap: %w", err)
		}
	}
	tlsConf := a.tlsConf
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
	if cfg.Listeners.ManageSieve != "" {
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
	}
	if cfg.Features.POP3Enabled {
		if cfg.Listeners.POP3 != "" {
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
		}
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
	a.logger.Info("mail path ready", "inbound", cfg.Listeners.SMTP, "submission", cfg.Listeners.Submission,
		"imap", cfg.Listeners.IMAP, "managesieve", cfg.Listeners.ManageSieve, "role", a.role())
	return nil
}

// ---------------------------------------------------------------------------
// HA lifecycle: bootstrap, standby wait, per-term activation, demotion.
// ---------------------------------------------------------------------------

// bootstrapHA opens the lease store and prepares the leader helper without
// acquiring anything. Failures here are startup failures on purpose: they
// are deterministic misconfiguration, safe to abort on.
func (a *App) bootstrapHA() error {
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
	a.leader = ha.NewLeader(store, owner, ttl, a.logger)
	a.logger.Info("ha: standby starting", "owner", owner, "ttl", ttl)
	return nil
}

// superviseTerms loops: acquire the lease → activate the write path → wait
// for loss or shutdown → demote → repeat. It returns when ctx is done or a
// terminal activation failure persists.
func (a *App) superviseTerms(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.waitForLeadership(); err != nil {
			if errors.Is(err, context.Canceled) || a.haCtx.Err() != nil || ctx.Err() != nil {
				return nil
			}
			a.logger.Error("ha: leadership wait failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = time.Second

		if err := a.activateTerm(ctx); err != nil {
			a.logger.Error("ha: term activation failed", "err", err)
			_ = a.leader.Release(context.Background()) // pass the baton promptly
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		lost := make(chan struct{})
		var lostOnce sync.Once
		notifyLost := func() {
			lostOnce.Do(func() {
				// Cancel the term BEFORE teardown inspects it: a cancelled
				// termCtx is how stopTerm tells "lease lost" (never touch
				// the record — a successor may already hold it) apart from
				// a voluntary handover (release).
				if a.termCancel != nil {
					a.termCancel()
				}
				close(lost)
			})
		}
		go a.leader.Run(a.termCtx, notifyLost)

		select {
		case <-ctx.Done():
			a.stopTerm("shutdown")
			return nil
		case <-lost:
			a.stopTerm("lease lost")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second): // brief cool-down before re-acquiring
			}
		}
	}
}

// waitForLeadership polls TryAcquire until it wins, the lease error is not
// retryable, or the app context ends. Followers log periodic standby notes.
func (a *App) waitForLeadership() error {
	ttl := a.leader.TTL()
	interval := ttl / 2
	if interval <= 0 {
		interval = time.Second
	}
	notified := time.Now().Add(-interval / 2)
	for {
		err := a.leader.TryAcquire(a.haCtx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, context.Canceled), a.haCtx.Err() != nil:
			return context.Canceled
		case !errors.Is(err, ha.ErrNotLeader):
			return fmt.Errorf("ha: lease acquire: %w", err)
		}
		if time.Since(notified) >= interval*4 {
			a.logger.Info("ha: standby, waiting for leadership")
			notified = time.Now()
		}
		select {
		case <-a.haCtx.Done():
			return context.Canceled
		case <-time.After(interval):
		}
	}
}

// activateTerm assembles and binds the write path for a freshly acquired
// lease. Every listener (SMTP drain included) and background worker lives
// inside termCtx so teardown can scope them precisely.
func (a *App) activateTerm(root context.Context) error {
	a.teardownMu.Lock()
	defer a.teardownMu.Unlock()

	tctx, cancel := context.WithCancel(root)
	a.termCtx, a.termCancel = tctx, cancel
	reset := func() {
		cancel()
		a.termCtx, a.termCancel = nil, nil
	}
	cleanupAll := func(err error) error {
		a.closeTermBackendsLocked()
		reset()
		return err
	}

	if err := a.openServices(); err != nil {
		return cleanupAll(err)
	}
	if err := a.wirePipeline(tctx); err != nil {
		return cleanupAll(err)
	}
	a.wireArchive(tctx)
	if err := a.wireServers(); err != nil {
		return cleanupAll(err)
	}
	if err := a.serveManagement(tctx); err != nil {
		return cleanupAll(err)
	}
	if err := a.serveMailListeners(tctx); err != nil {
		return cleanupAll(err)
	}
	a.assembled = true
	a.ready.Store(true)
	return nil
}

// stopTerm tears the write path down after a normal shutdown or a lease
// loss. Safe against double invocation (supervisor exit + Run tail + Close):
// every step guards on nil/empty state. The lease is released only when the
// term is still ours to hand over — after a loss notification the holder is
// unknown, so the record stays untouched.
func (a *App) stopTerm(reason string) {
	a.teardownMu.Lock()
	a.closeTermBackendsLocked()
	if a.termCancel != nil {
		a.termCancel()
		a.termCtx, a.termCancel = nil, nil
	}
	a.teardownMu.Unlock()
	a.ready.Store(false)
	if reason != "" {
		a.logger.Warn("ha: term ended", "reason", reason)
	}
}

// closeTermBackendsLocked stops serving, drains the queue and closes every
// durable backend opened for the current assembly. Callers hold teardownMu.
// The ordering below mirrors process shutdown: stop accepting first, then
// drain workers, then close storage last.
func (a *App) closeTermBackendsLocked() {
	ctx := context.Background()
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Decide about the lease BEFORE cancelling the term context: once
	// termCtx is cancelled, "term still ours" is indistinguishable from a
	// loss we have already handled. Releasing after a loss would delete the
	// successor's lease, so the holder check below is load-bearing.
	mayReleaseLease := false
	if a.leader != nil {
		mayReleaseLease = a.termCtx == nil || a.termCtx.Err() == nil
	}

	for _, srv := range []*gosmtp.Server{a.smtpInbound, a.smtpSubmission} {
		if srv != nil {
			if err := srv.Shutdown(shutdownCtx); err != nil {
				a.logger.Error("shutdown", "component", "smtp", "err", err)
			}
		}
	}
	if a.imapSrv != nil {
		_ = a.imapSrv.Close()
	}
	// Cancelling the term context stops the ManageSieve/POP3 accept loops,
	// the management server and the queue workers.
	if a.termCancel != nil && a.termCtx != nil && a.termCtx.Err() == nil {
		a.termCancel()
	}
	if a.mgmtSrv != nil {
		if err := a.mgmtSrv.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("shutdown", "component", a.mgmtSrv.Addr, "err", err)
		}
	}
	if a.qmDone != nil {
		<-a.qmDone // queue drained before the KV store closes underneath it
	}
	if a.arch != nil {
		a.arch.Close()
		a.arch = nil
	}
	if a.fts != nil {
		_ = a.fts.Close()
		a.fts = nil
	}
	if a.st != nil {
		_ = a.st.Close()
		a.st = nil
	}
	if a.auth != nil {
		_ = a.auth.Close()
		a.auth = nil
	}
	if a.dir != nil {
		_ = a.dir.Close()
		a.dir = nil
	}
	if mayReleaseLease {
		_ = a.leader.Release(ctx)
	}
	a.smtpInbound, a.smtpSubmission = nil, nil
	a.imapSrv, a.sieveSrv, a.pop3Srv = nil, nil, nil
	a.mgmtSrv, a.qm, a.qmDone, a.pipeline, a.classifier = nil, nil, nil, nil, nil
	a.tlsConf = nil
	a.assembled = false
}
