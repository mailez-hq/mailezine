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
	"net/http/pprof"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/accountgate"
	"mailezine/internal/auth"
	"mailezine/internal/config"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/dkim"
	"mailezine/internal/fts"
	"mailezine/internal/ftssync"
	"mailezine/internal/imap"
	"mailezine/internal/imapserver"
	"mailezine/internal/junk"
	"mailezine/internal/kvlease"
	"mailezine/internal/mailcache"
	"mailezine/internal/mailstore"
	"mailezine/internal/management"
	"mailezine/internal/metrics"
	"mailezine/internal/notify"
	"mailezine/internal/pop3"
	"mailezine/internal/queue"
	"mailezine/internal/server"
	"mailezine/internal/sieve"
	"mailezine/internal/smtp"
	"mailezine/internal/snooze"
	"mailezine/internal/store"
	"mailezine/internal/telemetry"
	"mailezine/internal/verify"
	"mailezine/internal/version"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Open-core seams (edition split): the interfaces below are the complete
// contract the app holds on enterprise subsystems. The community build
// leaves every field nil; the enterprise build (build tag mailez_ee)
// registers live implementations through the paired hook files.

// spamClassifier is the ML spam-scanning surface (rspamd): verdicts for the
// inbound pipeline plus Junk-boundary learning for IMAP.
type spamClassifier interface {
	delivery.Classifier
	LearnWithFuzzy(ctx context.Context, isSpam bool, data []byte) error
}

// complianceSpool is the compliance-capture archive (SMTP wrap + durable
// store + forwarder to the control plane).
type complianceSpool interface {
	Run(ctx context.Context)
	Wrap(direction string, next submitHandler) submitHandler
	Close()
}

// leaderLease is the HA leadership lease; nil means single-node.
type leaderLease interface {
	TTL() time.Duration
	TryAcquire(ctx context.Context) error
	Release(ctx context.Context) error
	Run(ctx context.Context, onLost func())
}

// App is the fully assembled engine. Depending on mode, New completes part
// or all of the startup work that can fail; Run serves until ctx is
// cancelled and then drains gracefully. Close releases durable backends and
// is safe to call in any lifecycle state.
type App struct {
	cfg    config.Config
	ctx    context.Context // root context supplied at construction
	logger *slog.Logger
	m      *metrics.Metrics

	dir          directory.Service
	auth         auth.Service
	st           *Storage
	fts          *fts.Indexer
	pipeline     *delivery.Pipeline
	classifier   spamClassifier
	sieveEngine  *sieve.Engine
	qm           *queue.Manager
	qmDone       chan struct{}
	arch         complianceSpool
	notifyClient *notify.Client

	tlsConf   *tls.Config
	startedAt time.Time

	smtpInbound    *gosmtp.Server
	smtpSubmission *gosmtp.Server
	imapSrv        *imapserver.Server
	sieveSrv       *sieve.Server
	pop3Srv        *pop3.Server
	healthSrv      *http.Server
	mgmtSrv        *http.Server

	leader     leaderLease
	haCtx      context.Context    // app-lifetime; cancelled by Close
	haCancel   context.CancelFunc // cancels haCtx
	termCtx    context.Context    // current HA term scope (listeners, queue)
	termCancel context.CancelFunc // cancels termCtx

	ready      atomic.Bool // write path bound and served
	assembled  bool        // services currently open on this process
	teardownMu sync.Mutex  // serializes teardown paths (term loss + Close)

	clusterNodeID     string
	clusterNodeIDOnce sync.Once
	kvReplayGauge     prometheus.Collector
	gate              *accountgate.Gate
	gateGauge         prometheus.Collector
}

// nodeID is this instance's cluster identity: the owner recorded in queue
// claims and singleton leases. Explicitly configured or generated once per
// process; stable across HA terms.
func (a *App) nodeID() string {
	a.clusterNodeIDOnce.Do(func() {
		a.clusterNodeID = a.cfg.Cluster.NodeID
		if a.clusterNodeID == "" {
			a.clusterNodeID = kvlease.RandomNodeID()
		}
	})
	return a.clusterNodeID
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

	if cfg.HA.Enabled && haAvailable() {
		if err := a.bootstrapHA(); err != nil {
			return nil, err
		}
		return a, nil
	}
	if cfg.HA.Enabled {
		// HA (shared lease + standby/leader terms) is an enterprise
		// capability; a copied config must never wedge community startup,
		// so degrade to single-node with a loud warning. Mutate the App's
		// copy too — Run() branches on a.cfg.HA.Enabled and would otherwise
		// fall into the (CE-stub) HA supervisor and exit with an error.
		a.logger.Warn("ha: requires the enterprise edition; continuing single-node")
		cfg.HA.Enabled = false
		a.cfg.HA.Enabled = false
	}
	if err := a.openServices(a.ctx); err != nil {
		a.Close()
		return nil, err
	}
	if a.cfg.Cluster.Mode == "multi" {
		// Multi-active fencing (queue claims, singleton leases) is only as
		// strong as the KV's transactional guarantees: require a native
		// TxnKV backend. The config layer already pinned the backend to
		// "tidb"; this re-checks what actually opened (registry,
		// edition), so a buffer-mode fallback can never run silently.
		if _, ok := a.st.kv.(store.TxnKV); !ok {
			a.Close()
			return nil, fmt.Errorf("cluster: multi-active requires a transactional KV backend (tidb); got %T", a.st.kv)
		}
		a.logger.Info("cluster mode", "mode", "multi-active", "node", a.nodeID())
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

// openServices opens the directory, auth and storage backends plus FTS
// under runCtx (the HA term scope when HA is enabled: background helpers
// started here must not outlive the KV they were opened against).
func (a *App) openServices(runCtx context.Context) error {
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
			mailcache.NewCacheWithTTL(a.cfg.AuthCacheSizeBytes, a.cfg.AuthCacheTTL))
	}
	if a.st, err = NewStorage(a.cfg, a.logger); err != nil {
		return err
	}
	if a.cfg.Cluster.Mode == "multi" && a.cfg.Storage.S3Endpoint == "" {
		// Without shared blob storage every replica writes bodies to its
		// own disk: cross-node IMAP reads, FTS convergence and claimed
		// outbound deliveries all break SILENTLY. A same-host deployment
		// sharing one volume is legitimate — hence a loud warning, not a
		// hard error.
		a.logger.Warn("cluster: multi-active without MAILEZINE_S3_*: blob storage is node-local — every replica must share the same blob volume, or bodies, search and cross-node delivery break")
	}
	// Optimistic-transaction replay gauge (contention signal). HA terms
	// re-run this assembly with fresh KV instances: replace the collector
	// so it reports the live one instead of freezing on the first term.
	if rc, ok := a.st.kv.(interface{ Replays() uint64 }); ok {
		if a.kvReplayGauge != nil {
			a.m.Registry.Unregister(a.kvReplayGauge)
		}
		a.kvReplayGauge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "mailezine_kv_txn_replays_total",
			Help: "KV optimistic transactions replayed due to write conflicts (multi-writer contention on shared keys; the signal for per-account write pinning).",
		}, func() float64 { return float64(rc.Replays()) })
		_ = a.m.Registry.Register(a.kvReplayGauge)
	}
	// One-shot backfill of the KV secondary indexes (name and per-mailbox
	// email indexes). Idempotent — existing entries are only rewritten when
	// divergent — so it is safe to repeat per HA term. Async: never block
	// start-up on a large store. The backfill is scoped to runCtx (the HA
	// term): without it, a term switch would leave it running on the
	// already-closed KV for up to 10 minutes.
	if kvms, ok := a.st.mailbox.(*mailstore.KV); ok {
		go func() {
			ctx, cancel := context.WithTimeout(runCtx, 10*time.Minute)
			defer cancel()
			n, err := kvms.Reindex(ctx)
			switch {
			case err != nil && !errors.Is(err, context.Canceled):
				a.logger.Warn("storage: index backfill incomplete", "fixed", n, "err", err)
			case n > 0:
				a.logger.Info("storage: index backfill", "entries", n)
			}
		}()
	}
	// Metadata cache: message/mailbox lists are memoized per account with a
	// TTL safety net; every write path invalidates the affected mailboxes.
	// Multi-active keeps it off: the cache is per-process and invalidation
	// only covers local writes, so a folder created on node A would stay
	// invisible (or still visible after deletion) on nodes B/C for up to a
	// full TTL — SELECT/DELETE on those nodes then disagree with reality.
	// Re-enable once cross-node invalidation (e.g. via the change log) lands.
	if a.cfg.MetaCacheSizeBytes > 0 && a.cfg.Cluster.Mode != "multi" {
		a.st.mailbox = mailstore.NewCached(a.st.mailbox,
			mailcache.NewCacheWithTTL(a.cfg.MetaCacheSizeBytes, 30*time.Second))
	}
	// Embedded full-text index (bleve); a failure disables FTS but never
	// startup (search falls back to the scan) — except in multi-active,
	// where a silently missing index on one node diverges search results
	// across the cluster: fail fast instead.
	if a.cfg.FTS.Enabled {
		a.fts = openFTS(a.cfg, a.logger)
		if a.fts == nil && a.cfg.Cluster.Mode == "multi" {
			return fmt.Errorf("fts: per-node index unavailable at %s (multi-active requires a writable index path; set MAILEZINE_FTS_PATH or MAILEZINE_ROCKS_PATH)", ftsIndexPath(a.cfg))
		}
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
	var classifier spamClassifier
	if a.cfg.Rspamd.URL != "" {
		// rspamd tier: the client re-scans the whole message and takes
		// precedence over the built-in baseline (learning is enterprise-only).
		classifier = newSpamClassifier(a.cfg, a.logger)
	} else if a.cfg.Junk.Enabled {
		// Community baseline: score the verifier's authentication results
		// plus DNSBL hits and sender lists. Lenient by design — flag more,
		// reject only on hard signals.
		classifier = junk.New(newSystemResolver(), junk.Config{
			HeaderScore: a.cfg.Junk.HeaderScore,
			RejectScore: a.cfg.Junk.RejectScore,
			RBLs:        a.cfg.Junk.RBLs,
			Whitelist:   a.cfg.Junk.Whitelist,
			Blacklist:   a.cfg.Junk.Blacklist,
			Greylist:    a.cfg.Junk.Greylist,
		}, a.logger)
	}
	a.classifier = classifier
	a.sieveEngine = sieve.NewEngine(a.logger)
	pipelineFTS := a.fts
	if a.cfg.Cluster.Mode == "multi" && a.fts != nil {
		// Multi-active: indexing converges via the change-log tailer below
		// (every node, including this one), so the delivery path stays
		// index-free — each copy is indexed exactly once per node instead
		// of twice on the delivering node. The read path (IMAP SEARCH)
		// keeps using a.fts.
		pipelineFTS = nil
	}
	a.pipeline = &delivery.Pipeline{
		Directory:          a.dir,
		Store:              a.st.mailbox,
		Verifier:           verifier,
		Classifier:         classifier,
		Sieve:              a.sieveEngine,
		ScriptSource:       sieve.DefaultScriptSource{Store: a.st.mailbox, Directory: a.dir},
		Hostname:           a.cfg.Hostname,
		RecipientDelimiter: a.cfg.RecipientDelimiter,
		Logger:             a.logger,
		FTS:                pipelineFTS,
	}
	if a.cfg.Cluster.Mode == "multi" && a.fts != nil {
		// bleve is node-local: tail the shared change logs so this node's
		// index sees every node's deliveries (eventual consistency, one
		// interval of lag; a fresh/corrupt local index replays from
		// watermark zero and rebuilds in full).
		sw := &ftssync.Worker{
			Store:     a.st.Facade(),
			Index:     a.fts,
			StatePath: ftsIndexPath(a.cfg) + ".sync.json",
			Logger:    a.logger,
		}
		go sw.Run(runCtx)
		a.logger.Info("fts: change-log tailer", "state", ftsIndexPath(a.cfg)+".sync.json")
	}
	if a.cfg.Cluster.Mode == "multi" && a.cfg.AccountGate.Enabled {
		// Per-account write pinning: inbound writers for an account queue
		// on its owning node instead of colliding on the account's hot KV
		// keys. Advisory (bounded wait, then proceed — correctness is
		// carried by the transactional store either way).
		opts := accountgate.DefaultOptions()
		if w := a.cfg.AccountGate.WaitSeconds; w > 0 {
			opts.Wait = time.Duration(w) * time.Second
		}
		a.gate = accountgate.New(a.st.kv, a.nodeID(), opts, a.logger)
		a.gate.SetMetrics(a.m)
		a.pipeline.Gate = a.gate
		go a.gate.Run(runCtx)
		if a.gateGauge != nil {
			a.m.Registry.Unregister(a.gateGauge)
		}
		a.gateGauge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "mailezine_account_gate_held",
			Help: "Accounts whose inbound write path this node currently owns (per-account write pinning).",
		}, func() float64 { return float64(a.gate.Held()) })
		_ = a.m.Registry.Register(a.gateGauge)
		a.logger.Info("account write gate", "wait", opts.Wait, "ttl", opts.TTL)
	}
	if a.cfg.Notify.Enabled {
		// Delivery receipts: the control plane raises push/webhooks/SSE the
		// moment mail lands instead of at its poller's next tick. The same
		// client serves the snooze sweeper's wake-up receipts.
		a.notifyClient = notify.New(a.cfg.Notify.URL, a.logger, a.cfg.StackSecret)
		a.pipeline.Notifier = a.notifyClient
		a.logger.Info("delivery notify", "url", a.cfg.Notify.URL)
	}
	if a.cfg.SnoozeInterval > 0 {
		// Snooze wake-up sweeper: due messages return to the inbox unread
		// and raise push/SSE immediately (the lazy wake in the snoozed
		// view stays as fallback).
		sw := &snooze.Sweeper{
			Accounts: func(ctx context.Context) ([]string, error) { return a.st.Facade().ListAccounts(ctx) },
			Store:    a.st.mailbox,
			Notify:   a.notifyClient,
			Logger:   a.logger,
		}
		interval := time.Duration(a.cfg.SnoozeInterval) * time.Second
		if a.cfg.Cluster.Mode == "multi" {
			// Exactly one node sweeps at a time: the flag rewrite is
			// idempotent, but the receipt fires push/SSE and must not
			// repeat per node. The lease must cover at least two sweep
			// intervals (renewal runs on each tick): a fixed 90 s would
			// expire between sweeps once operators raise the interval,
			// causing repeated lease flapping and double receipts.
			leaseTTL := 2 * interval
			if leaseTTL < 90*time.Second {
				leaseTTL = 90 * time.Second
			}
			l := kvlease.New(a.st.kv, "snooze-sweeper", a.nodeID(), leaseTTL)
			go l.Run(runCtx, interval, func(ctx context.Context) { sw.SweepOnce(ctx) })
			a.logger.Info("snooze sweeper", "interval", a.cfg.SnoozeInterval, "leased", true)
		} else {
			go sw.Run(runCtx, interval)
			a.logger.Info("snooze sweeper", "interval", a.cfg.SnoozeInterval)
		}
	}
	{
		// Blob GC: deletes stage reclaim claims; the sweep physically
		// reclaims a blob only once no account links it anywhere (blob IDs
		// are content-global, link counts per-account). Leased in multi
		// mode so exactly one node sweeps. Interval 0 = mailstore default
		// (10 minutes). Only the KV backend carries blobs; the assertion
		// skips other mailbox backends.
		if kv, ok := a.st.mailbox.(*mailstore.KV); ok {
			if a.cfg.Cluster.Mode == "multi" {
				l := kvlease.New(a.st.kv, "blob-gc", a.nodeID(), 20*time.Minute)
				go kv.RunBlobGC(runCtx, l, 0, 0, a.logger)
				a.logger.Info("blob gc sweeper", "interval", "10m", "leased", true)
			} else {
				go kv.RunBlobGC(runCtx, nil, 0, 0, a.logger)
				a.logger.Info("blob gc sweeper", "interval", "10m")
			}
		}
	}
	if classifier != nil {
		// Never assign a typed nil to the interface: an unconfigured
		// classifier must leave the field nil (delivery fails open).		a.pipeline.Classifier = classifier
	}
	return a.wireQueue(runCtx)
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
	// Locally delivered submissions get DKIM-signed as well when a vault is
	// configured; without one (plain dev tier) they deliver unsigned.
	var submitSign submitSigner
	if a.cfg.DKIMVaultURL != "" {
		submitSign = opportunisticSigner{dkim.NewSigner(a.cfg.DKIMVaultURL, a.logger, a.cfg.StackSecret), a.logger}
	}
	submit := newSubmit(a.dir, a.pipeline, a.qm, submitSign, a.logger)
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
		Metrics:            a.m,
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
		Metrics:            a.m,
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
		Metrics:         a.m,
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
		Engine:    a.sieveEngine,
		Logger:    a.logger,
	}
	a.pop3Srv = &pop3.Server{
		Store:     a.st.mailbox,
		Auth:      a.auth,
		Directory: a.dir,
		Port:      listenerPort(a.cfg.Listeners.POP3),
		TLSConfig: tlsConf,
		Logger:    a.logger,
		Metrics:   a.m,
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
// orchestrator does not mistake "waiting for the lease" for a dead pod.
func (a *App) serveHealth(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/health").Inc()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q,"role":%q,"cluster":%q}`,
			version.Version, a.role(), a.cfg.Cluster.Mode)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		a.m.HealthChecks.WithLabelValues("/ready").Inc()
		fmt.Fprintf(w, `{"status":"ok","role":%q,"cluster":%q,"storage":%q,"directory":%q,"rspamd":%v,"outbound":%v,"fts":%v}`,
			a.role(), a.cfg.Cluster.Mode, a.cfg.Storage.Backend, a.cfg.Directory.Mode,
			a.classifier != nil, a.cfg.Outbound.Enabled, a.fts != nil)
	})
	mux.Handle("/metrics", promhttp.HandlerFor(a.m.Registry, promhttp.HandlerOpts{}))
	if a.cfg.Pprof {
		// Sample mutex/block events so /debug/pprof/{mutex,block} carry
		// signal under load. Rates are only paid when profiling is on.
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(10_000) // record blocking >= 10µs
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	a.healthSrv = &http.Server{Addr: a.cfg.HealthAddr, Handler: mux}
	return serveHTTP(ctx, a.healthSrv, a.logger)
}

func (a *App) role() string {
	if !a.ready.Load() {
		return "standby"
	}
	if a.cfg.Cluster.Mode == "multi" {
		return "active"
	}
	return "leader"
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
			License:       a.cfg.License.Status(time.Now()),
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
	proxyTrusted, err := parseNets(cfg.ProxyTrusted)
	if err != nil {
		return fmt.Errorf("proxy trusted nets: %w", err)
	}
	if cfg.Listeners.SMTP != "" {
		if err := serveSMTP(ctx, a.smtpInbound, cfg.Listeners.SMTP, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTP), proxyTrusted, nil, a.logger); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	if cfg.Listeners.Submission != "" {
		if err := serveSMTP(ctx, a.smtpSubmission, cfg.Listeners.Submission, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.Submission), proxyTrusted, nil, a.logger); err != nil {
			return fmt.Errorf("submission: %w", err)
		}
	}
	if cfg.Listeners.IMAP != "" {
		if err := serveTCP(ctx, a.imapSrv, "imap", cfg.Listeners.IMAP, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAP), proxyTrusted, nil, a.logger); err != nil {
			return fmt.Errorf("imap: %w", err)
		}
	}
	tlsConf := a.tlsConf
	if cfg.Listeners.SMTPS != "" && tlsConf != nil {
		if err := serveSMTP(ctx, a.smtpSubmission, cfg.Listeners.SMTPS, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.SMTPS), proxyTrusted, tlsConf, a.logger); err != nil {
			return fmt.Errorf("smtps: %w", err)
		}
	}
	if cfg.Listeners.IMAPS != "" && tlsConf != nil {
		if err := serveTCP(ctx, a.imapSrv, "imaps", cfg.Listeners.IMAPS, lim.MaxConnections,
			proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.IMAPS), proxyTrusted, tlsConf, a.logger); err != nil {
			return fmt.Errorf("imaps: %w", err)
		}
	}
	if cfg.Listeners.ManageSieve != "" {
		msieve := &server.Listener{
			Name:          "managesieve",
			Addr:          cfg.Listeners.ManageSieve,
			MaxConn:       lim.MaxConnections,
			ProxyProtocol: proxyEnabled(cfg.ProxyProtocol, cfg.Listeners.ManageSieve),
			ProxyTrusted:  proxyTrusted,
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
				ProxyTrusted:  proxyTrusted,
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
				ProxyTrusted:  proxyTrusted,
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

	if err := a.openServices(tctx); err != nil {
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
		// The queue workers exit on termCancel; a wedged delivery must not
		// hang shutdown (and the KV close below is what finally unblocks
		// anything still stuck in a transaction).
		select {
		case <-a.qmDone:
		case <-time.After(30 * time.Second):
			a.logger.Error("shutdown: queue drain timed out after 30s; closing stores anyway")
		}
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
	a.mgmtSrv, a.qm, a.qmDone, a.pipeline, a.classifier, a.sieveEngine = nil, nil, nil, nil, nil, nil
	a.tlsConf = nil
	a.assembled = false
}
