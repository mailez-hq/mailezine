// Package config defines the typed configuration of mailezine.
//
// Values come from MAILEZINE_* environment variables with safe defaults;
// a TOML overlay is planned for M1. Configuration is validated before any
// listener starts: a bad config refuses to boot with an actionable message.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"mailezine/internal/license"
	"mailezine/internal/limits"
)

// Default listener ports, aligned with PLAN.md §4.3.
const (
	DefaultHealthPort     = 11480
	DefaultManagementPort = 8090
)

// Config is the validated process configuration.
type Config struct {
	Log            LogConfig
	HealthAddr     string
	Listeners      ListenersConfig
	Storage        StorageConfig
	FTS            FTSConfig
	HA             HAConfig
	Cluster        ClusterConfig
	AccountGate    AccountGateConfig
	Directory      DirectoryConfig
	Auth           AuthConfig
	Management     ManagementConfig
	BackendAddress string
	Hostname       string
	// StackSecret authenticates the internal /stack API toward the mailez
	// control plane (MAILEZINE_STACK_SECRET). Empty keeps the legacy
	// unauthenticated local-dev mode; both sides must agree on the value.
	StackSecret        string
	RecipientDelimiter string
	TrustedNets        []string // CIDRs that are authenticated by the gateway
	Rspamd             RspamdConfig
	Junk               JunkConfig
	DKIMVaultURL       string
	Outbound           OutboundConfig
	TLS                TLSConfig
	ProxyProtocol      []string // listener ports expecting a PROXY v1 header
	// ProxyTrusted are the CIDRs whose connections may carry the PROXY v1
	// header; headers from other peers are rejected (client-IP forgery
	// guard). Defaults to loopback and private ranges.
	ProxyTrusted []string
	Queue        QueueConfig
	Limits       limits.Config
	Features     FeaturesConfig
	Notify       NotifyConfig
	// SnoozeInterval is the seconds between snooze wake-up sweeps; 0
	// disables the sweeper (due messages then only resurface lazily when
	// the snoozed view is opened).
	SnoozeInterval int
	// IMAPCacheSizeBytes is the weight budget (bytes) of the IMAP
	// envelope/body-structure memo.
	IMAPCacheSizeBytes int64
	// MetaCacheSizeBytes is the weight budget of the mailbox metadata cache
	// (mailbox/message lists) and the directory lookup cache.
	MetaCacheSizeBytes int64
	// AuthCacheSizeBytes is the weight budget of the authentication-result
	// cache.
	AuthCacheSizeBytes int64
	// AuthCacheTTL bounds how long a successful authentication stays cached
	// (MAILEZINE_AUTH_CACHE_TTL, integer seconds).
	// The key is the credential pair, so a password change invalidates the new
	// password immediately; the old pair remains valid for at most one TTL,
	// same as legacy IMAP's auth_cache_ttl. Keep it >= the control-plane IMAP
	// connection-pool idle time so a re-dial after pool eviction still hits
	// the cache instead of paying a full control-plane round trip.
	AuthCacheTTL time.Duration
	// Archive captures compliance copies of inbound/outbound mail and
	// forwards them to the control plane archive store.
	Archive ArchiveConfig
	// DLP scans outbound submissions against control-plane rules
	// (敏感词过滤 + 审批); failures fail open.
	DLP DLPConfig
	// License fields: the enterprise engine validates its license at startup.
	LicenseFile     string
	LicenseInline   string
	LicenseRequired bool
	License         license.License
}

// LogConfig controls the structured logger.
type LogConfig struct {
	Level  string // debug|info|warn|error
	Format string // text|json
}

// ListenersConfig is the port matrix for protocol listeners.
type ListenersConfig struct {
	SMTP        string
	Submission  string
	IMAP        string
	ManageSieve string
	POP3        string
	// Implicit-TLS variants (RFC 8314 mode, the submissions/imaps/pop3s
	// ports): TLS is negotiated before the first protocol byte. Empty
	// disables; requires MAILEZINE_TLS_CERT_FILE/KEY_FILE.
	SMTPS string
	IMAPS string
	POP3S string
}

// StorageConfig selects the storage backend (ARCHITECTURE.md §3).
type StorageConfig struct {
	Backend   string // pebble|tidb
	RocksPath string
	// DSN is the TiDB/MySQL connection string used when Backend is "tidb".
	DSN string
	// S3 (MinIO/cloud) blob settings. Empty Endpoint ⇒ local FS blob
	// (decision D4).
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool
	// Compression gzips blob contents at rest. Text mail compresses
	// 60-80%; attachments stay near their original size.
	Compression bool
}

// FTSConfig enables the embedded full-text index (bleve).
type FTSConfig struct {
	Enabled bool
	// Path is the index directory; empty derives <rockspath>.fts.
	Path string
	// TikaURL optionally enables attachment text extraction (Apache Tika
	// /tika endpoint); failures degrade to indexing without attachments.
	TikaURL string
}

// HAConfig enables active-passive leader election (D31).
type HAConfig struct {
	Enabled bool
	// LeasePath is the shared lease file for FS-backed leases (a volume
	// mounted by every instance); empty falls back to the S3 bucket when
	// MAILEZINE_S3_* is configured.
	LeasePath string
	// TTLSeconds is the leadership lease lifetime; the holder renews at
	// TTL/3. Default 15.
	TTLSeconds int
}

// DirectoryConfig selects the directory provider (PLAN.md §4.4).
type DirectoryConfig struct {
	Mode     string        // dev|mailez
	File     string        // dev-mode JSON directory (required when Mode=dev)
	CacheTTL time.Duration // mailez-mode cache TTL
}

// AuthConfig selects the credential validation provider.
type AuthConfig struct {
	Mode             string // dev|mailez
	DevPasswordsFile string // dev-mode password file (required when Mode=dev)
}

// ManagementConfig enables the internal management API (optional).
type ManagementConfig struct {
	Addr   string // empty = disabled
	Secret string // required when Addr is set
}

// RspamdConfig configures the inbound spam classifier (optional).
type RspamdConfig struct {
	URL string // e.g. http://mail-filter:11333/checkv2; empty disables scanning
	// LearnURL is the rspamd controller endpoint used for supervised
	// learning (e.g. http://mail-filter:11334); empty disables learning.
	LearnURL string
	// Password is the rspamd controller password (rspamc -P).
	Password string
}

// JunkConfig configures the community baseline spam classifier (optional;
// the enterprise rspamd client takes precedence when configured).
type JunkConfig struct {
	// Enabled turns the baseline classifier on. Default true in community
	// builds without rspamd; harmless in enterprise builds that configure
	// rspamd (the rspamd client wins).
	Enabled bool
	// RejectScore / HeaderScore: scores at or above which messages are
	// rejected / flagged. Defaults 12 / 4.5 — deliberately lenient.
	RejectScore float64
	HeaderScore float64
	// RBLs lists DNSBL zones queried with the reversed peer IP. Empty
	// disables DNSBL checks.
	RBLs []string
	// Whitelist / Blacklist hold full addresses or bare domains.
	Whitelist []string
	Blacklist []string
	// Greylist greylists first-seen senders scoring in the ambiguous
	// band. Default false.
	Greylist bool
}

// OutboundConfig controls relay submission to the queue.
type OutboundConfig struct {
	Enabled bool
	Port    int // delivery port for remote MXes, default 25
	// FixedHost/FixedPort relay every outbound message to one smarthost,
	// skipping MX resolution (equivalent to legacy MTA relayhost). Empty
	// FixedHost keeps direct MX delivery.
	FixedHost string
	FixedPort int
	// SmarthostUsername/Password enable SASL AUTH when delivering through
	// a configured smarthost (directory relay transport "smtp:[host]").
	SmarthostUsername string
	SmarthostPassword string
	// SmarthostAllowPlaintextAuth opts into SASL AUTH before STARTTLS
	// (trusted loopback/LAN smarthost only). Credentials are otherwise
	// sent exclusively over TLS.
	SmarthostAllowPlaintextAuth bool
}

// TLSConfig enables STARTTLS for direct (non-gateway) deployments. Both
// files must be set; empty disables TLS (the mailez gateway terminates TLS).
type TLSConfig struct {
	CertFile string
	KeyFile  string
}

// QueueConfig tunes the outbound queue scheduler.
type QueueConfig struct {
	MaxAttempts  int
	BaseRetry    time.Duration // seconds
	MaxRetry     time.Duration // seconds
	PollInterval time.Duration // seconds
	// DelayWarning is the interval after which a still-queued message
	// triggers a delay warning; 0 disables.
	DelayWarning time.Duration
	// ClaimLease bounds one delivery attempt's claim in multi-active
	// deployments; 0 keeps the 10m default.
	ClaimLease time.Duration
}

// ClusterConfig selects the engine clustering mode.
type ClusterConfig struct {
	// Mode is "single" (default) or "multi". Multi runs every engine
	// service on every node against shared transactional storage (TiDB):
	// the outbound queue claims deliveries per message, singleton workers
	// are leased across nodes and any node can serve any account. It
	// replaces the HA leader/standby model — the two are mutually
	// exclusive.
	Mode string
	// NodeID identifies this node in claim ownership and singleton leases;
	// empty auto-generates a per-process ID. Set it when logs and queue
	// states should name a stable node.
	NodeID string
}

// AccountGateConfig controls the per-account write serializer in
// multi-active deployments: writers for one account queue on the node
// holding the account's ownership lease instead of colliding on its hot
// KV keys. Advisory (bounded wait, then proceed) — never affects
// correctness.
type AccountGateConfig struct {
	Enabled     bool
	WaitSeconds int // bounded wait on a foreign owner before proceeding
}

// FeaturesConfig gates optional components; switches only decide whether a
// component starts, never the semantics of the core path (ARCHITECTURE.md §10.3).
type FeaturesConfig struct {
	POP3Enabled bool
	JunkEnabled bool
	JMAPEnabled bool
}

// ArchiveConfig controls the compliance copy capture (归档).
type ArchiveConfig struct {
	Enabled bool
	// URL is the control-plane ingest endpoint; empty derives
	// http://<BackendAddress>/stack/archive.
	URL string
	// MaxAttempts bounds the forwarding retries per message; copies that
	// exhaust it stay spooled (marked failed) for manual recovery.
	MaxAttempts int
}

// DLPConfig controls the outbound content filter client.
type DLPConfig struct {
	Enabled bool
	// URL is the control-plane check endpoint; empty derives
	// http://<BackendAddress>/stack/dlp/check.
	URL string
}

// NotifyConfig controls the delivery-receipt client: after a message lands
// in a local mailbox the engine tells the control plane immediately, so web
// push/webhooks/SSE fire without waiting for the poller.
type NotifyConfig struct {
	Enabled bool
	// URL is the control-plane receipt endpoint; empty derives
	// http://<BackendAddress>/stack/notify/delivered.
	URL string
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	// Optional TOML overlay (MAILEZINE_CONFIG): a flat map of the same
	// MAILEZINE_* keys. Explicit environment variables take precedence over
	// the file (12-factor convention).
	overlay := map[string]string{}
	if path := os.Getenv("MAILEZINE_CONFIG"); path != "" {
		var raw map[string]any
		if _, err := toml.DecodeFile(path, &raw); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
		for k, v := range raw {
			overlay[k] = tomlString(v)
		}
	}
	getenv := func(key, def string) string {
		// Explicit environment variables win over the file (12-factor).
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v, ok := overlay[key]; ok {
			return v
		}
		return def
	}
	envBool := func(key string, def bool) bool {
		v := getenv(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return def
		}
		return b
	}
	envInt := func(key string, def int) int {
		v := getenv(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return def
		}
		return n
	}
	envFloat := func(key string, def float64) float64 {
		v := getenv(key, "")
		if v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return def
		}
		return f
	}
	envInt64 := func(key string, def int64) int64 {
		v := getenv(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return def
		}
		return n
	}
	envDuration := func(key string, def time.Duration) time.Duration {
		v := getenv(key, "")
		if v == "" {
			return def
		}
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs <= 0 {
			return def
		}
		return time.Duration(secs) * time.Second
	}
	cfg := Config{
		Log: LogConfig{
			Level:  getenv("MAILEZINE_LOG_LEVEL", "info"),
			Format: getenv("MAILEZINE_LOG_FORMAT", "text"),
		},
		HealthAddr:         getenv("MAILEZINE_HEALTH_ADDR", fmt.Sprintf(":%d", DefaultHealthPort)),
		Hostname:           getenv("MAILEZINE_HOSTNAME", defaultHostname()),
		RecipientDelimiter: getenv("MAILEZINE_RECIPIENT_DELIMITER", "+"),
		Listeners: ListenersConfig{
			SMTP:        getenv("MAILEZINE_SMTP_ADDR", ":1025"),
			Submission:  getenv("MAILEZINE_SUBMISSION_ADDR", ":1587"),
			IMAP:        getenv("MAILEZINE_IMAP_ADDR", ":1143"),
			ManageSieve: getenv("MAILEZINE_MANAGESIEVE_ADDR", ":11490"),
			POP3:        getenv("MAILEZINE_POP3_ADDR", ":10110"),
			SMTPS:       getenv("MAILEZINE_SMTPS_ADDR", ""),
			IMAPS:       getenv("MAILEZINE_IMAPS_ADDR", ""),
			POP3S:       getenv("MAILEZINE_POP3S_ADDR", ""),
		},
		Storage: StorageConfig{
			Backend:     getenv("MAILEZINE_STORAGE_BACKEND", "pebble"),
			RocksPath:   getenv("MAILEZINE_ROCKS_PATH", ""),
			DSN:         getenv("MAILEZINE_STORAGE_DSN", ""),
			S3Endpoint:  getenv("MAILEZINE_S3_ENDPOINT", ""),
			S3AccessKey: getenv("MAILEZINE_S3_ACCESS_KEY", ""),
			S3SecretKey: getenv("MAILEZINE_S3_SECRET_KEY", ""),
			S3Bucket:    getenv("MAILEZINE_S3_BUCKET", ""),
			S3UseSSL:    envBool("MAILEZINE_S3_USE_SSL", false),
			Compression: envBool("MAILEZINE_BLOB_COMPRESSION", false),
		},
		FTS: FTSConfig{
			// Full-text search defaults ON: indexed search is the expected
			// baseline (mainstream providers parity) and the pipeline degrades to
			// a scan whenever the index is unavailable, so the only cost is
			// index memory/disk. Operators with tight memory budgets can
			// opt out with MAILEZINE_FTS_ENABLED=false.
			Enabled: envBool("MAILEZINE_FTS_ENABLED", true),
			Path:    getenv("MAILEZINE_FTS_PATH", ""),
			TikaURL: getenv("MAILEZINE_FTS_TIKA_URL", ""),
		},
		HA: HAConfig{
			Enabled:    envBool("MAILEZINE_HA_ENABLED", false),
			LeasePath:  getenv("MAILEZINE_HA_LEASE_PATH", ""),
			TTLSeconds: envInt("MAILEZINE_HA_TTL_SECONDS", 15),
		},
		Cluster: ClusterConfig{
			Mode:   getenv("MAILEZINE_CLUSTER_MODE", "single"),
			NodeID: getenv("MAILEZINE_CLUSTER_NODE_ID", ""),
		},
		AccountGate: AccountGateConfig{
			Enabled:     envBool("MAILEZINE_ACCOUNT_GATE_ENABLED", true),
			WaitSeconds: envInt("MAILEZINE_ACCOUNT_GATE_WAIT_SECONDS", 5),
		},
		Directory: DirectoryConfig{
			Mode:     getenv("MAILEZINE_DIRECTORY_MODE", "dev"),
			File:     getenv("MAILEZINE_DIRECTORY_FILE", ""),
			CacheTTL: envDuration("MAILEZINE_DIRECTORY_CACHE_TTL", 30*time.Second),
		},
		Auth: AuthConfig{
			Mode:             getenv("MAILEZINE_AUTH_MODE", "dev"),
			DevPasswordsFile: getenv("MAILEZINE_AUTH_DEV_FILE", ""),
		},
		Archive: ArchiveConfig{
			Enabled:     envBool("MAILEZINE_ARCHIVE_ENABLED", false),
			URL:         getenv("MAILEZINE_ARCHIVE_URL", ""),
			MaxAttempts: envInt("MAILEZINE_ARCHIVE_MAX_ATTEMPTS", 10),
		},
		DLP: DLPConfig{
			Enabled: envBool("MAILEZINE_DLP_ENABLED", false),
			URL:     getenv("MAILEZINE_DLP_URL", ""),
		},
		Management: ManagementConfig{
			Addr:   getenv("MAILEZINE_MANAGEMENT_ADDR", ""),
			Secret: getenv("MAILEZINE_MANAGEMENT_SECRET", ""),
		},
		BackendAddress: getenv("MAILEZINE_BACKEND_ADDRESS", "127.0.0.1:8080"),
		StackSecret:    getenv("MAILEZINE_STACK_SECRET", ""),
		TrustedNets:    splitCSV(getenv("MAILEZINE_TRUSTED_NETS", "127.0.0.1/8,::1/128")),
		Rspamd: RspamdConfig{
			URL:      getenv("MAILEZINE_RSPAMD_URL", ""),
			LearnURL: getenv("MAILEZINE_RSPAMD_LEARN_URL", ""),
			Password: getenv("MAILEZINE_RSPAMD_PASSWORD", ""),
		},
		Junk: JunkConfig{
			// Enabled by default: the community baseline classifier is
			// harmless where rspamd is configured (rspamd wins the wiring)
			// and is the only anti-spam tier otherwise.
			Enabled:     envBool("MAILEZINE_JUNK_ENABLED", true),
			HeaderScore: envFloat("MAILEZINE_JUNK_HEADER_SCORE", 0),
			RejectScore: envFloat("MAILEZINE_JUNK_REJECT_SCORE", 0),
			RBLs:        splitCSV(getenv("MAILEZINE_JUNK_RBLS", "bl.spamcop.net")),
			Whitelist:   splitCSV(getenv("MAILEZINE_JUNK_WHITELIST", "")),
			Blacklist:   splitCSV(getenv("MAILEZINE_JUNK_BLACKLIST", "")),
			Greylist:    envBool("MAILEZINE_JUNK_GREYLIST", false),
		},
		DKIMVaultURL: getenv("MAILEZINE_DKIM_VAULT_URL", ""),
		Outbound: OutboundConfig{
			Enabled:                     envBool("MAILEZINE_OUTBOUND_ENABLED", true),
			Port:                        envInt("MAILEZINE_OUTBOUND_PORT", 25),
			FixedHost:                   getenv("MAILEZINE_OUTBOUND_FIXED_HOST", ""),
			FixedPort:                   envInt("MAILEZINE_OUTBOUND_FIXED_PORT", 0),
			SmarthostUsername:           getenv("MAILEZINE_OUTBOUND_SMTP_USERNAME", ""),
			SmarthostPassword:           getenv("MAILEZINE_OUTBOUND_SMTP_PASSWORD", ""),
			SmarthostAllowPlaintextAuth: envBool("MAILEZINE_OUTBOUND_SMTP_ALLOW_PLAINTEXT_AUTH", false),
		},
		TLS: TLSConfig{
			CertFile: getenv("MAILEZINE_TLS_CERT_FILE", ""),
			KeyFile:  getenv("MAILEZINE_TLS_KEY_FILE", ""),
		},
		ProxyProtocol: splitCSV(getenv("MAILEZINE_PROXY_PROTOCOL", "")),
		ProxyTrusted:  splitCSV(getenv("MAILEZINE_PROXY_TRUSTED", "127.0.0.1/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fe80::/10,fc00::/7")),
		Queue: QueueConfig{
			MaxAttempts:  envInt("MAILEZINE_QUEUE_MAX_ATTEMPTS", 10),
			BaseRetry:    time.Duration(envInt("MAILEZINE_QUEUE_BASE_RETRY_SECONDS", 60)) * time.Second,
			MaxRetry:     time.Duration(envInt("MAILEZINE_QUEUE_MAX_RETRY_SECONDS", 24*3600)) * time.Second,
			PollInterval: time.Duration(envInt("MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS", 5)) * time.Second,
			DelayWarning: time.Duration(envInt("MAILEZINE_QUEUE_DELAY_WARNING_SECONDS", 300)) * time.Second,
			ClaimLease:   time.Duration(envInt("MAILEZINE_QUEUE_CLAIM_LEASE_SECONDS", 600)) * time.Second,
		},
		Limits: limits.Defaults(),
		Features: FeaturesConfig{
			POP3Enabled: envBool("MAILEZINE_POP3_ENABLED", true),
			JunkEnabled: envBool("MAILEZINE_JUNK_ENABLED", false),
			JMAPEnabled: envBool("MAILEZINE_JMAP_ENABLED", false),
		},
	}
	cfg.Limits.MaxMessageSize = envInt64("MAILEZINE_MAX_MESSAGE_SIZE", cfg.Limits.MaxMessageSize)
	cfg.Limits.MaxRecipients = envInt("MAILEZINE_MAX_RECIPIENTS", cfg.Limits.MaxRecipients)
	cfg.Limits.MaxConnections = envInt("MAILEZINE_MAX_CONNECTIONS", cfg.Limits.MaxConnections)
	cfg.Limits.MaxLineLength = envInt("MAILEZINE_MAX_LINE_LENGTH", cfg.Limits.MaxLineLength)
	cfg.IMAPCacheSizeBytes = envInt64("MAILEZINE_CACHE_SIZE", 8<<20)
	cfg.MetaCacheSizeBytes = envInt64("MAILEZINE_META_CACHE_SIZE", 32<<20)
	cfg.AuthCacheSizeBytes = envInt64("MAILEZINE_AUTH_CACHE_SIZE", 1<<20)
	cfg.AuthCacheTTL = envDuration("MAILEZINE_AUTH_CACHE_TTL", 10*time.Minute)
	if cfg.DKIMVaultURL == "" {
		cfg.DKIMVaultURL = "http://" + cfg.BackendAddress + "/stack/rspamd/vault"
	}
	if cfg.Archive.Enabled && cfg.Archive.URL == "" {
		cfg.Archive.URL = "http://" + cfg.BackendAddress + "/stack/archive"
	}
	if cfg.Archive.MaxAttempts <= 0 {
		cfg.Archive.MaxAttempts = 10
	}
	if cfg.DLP.Enabled && cfg.DLP.URL == "" {
		cfg.DLP.URL = "http://" + cfg.BackendAddress + "/stack/dlp/check"
	}
	cfg.Notify = NotifyConfig{
		Enabled: envBool("MAILEZINE_DELIVERY_NOTIFY", true),
		URL:     getenv("MAILEZINE_NOTIFY_URL", ""),
	}
	if cfg.Notify.Enabled && cfg.Notify.URL == "" {
		cfg.Notify.URL = "http://" + cfg.BackendAddress + "/stack/notify/delivered"
	}
	cfg.SnoozeInterval = envInt("MAILEZINE_SNOOZE_INTERVAL", 60)
	cfg.LicenseFile = getenv("MAILEZINE_LICENSE_FILE", "")
	cfg.LicenseInline = getenv("MAILEZINE_LICENSE", "")
	cfg.LicenseRequired = envBool("MAILEZINE_LICENSE_REQUIRED", false) && license.EnterpriseBuild
	lic, err := license.Load(cfg.LicenseFile, cfg.LicenseInline, cfg.LicenseRequired)
	if err != nil {
		return Config{}, err
	}
	cfg.License = lic

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects unsupported or incomplete configurations.
func (c Config) Validate() error {
	for name, addr := range map[string]string{
		"health":      c.HealthAddr,
		"smtp":        c.Listeners.SMTP,
		"submission":  c.Listeners.Submission,
		"imap":        c.Listeners.IMAP,
		"managesieve": c.Listeners.ManageSieve,
		"pop3":        c.Listeners.POP3,
	} {
		if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
			return fmt.Errorf("config: %s addr %q must be host:port (e.g. :1143)", name, addr)
		}
	}
	switch c.Storage.Backend {
	case "pebble":
		if c.Storage.RocksPath == "" {
			return fmt.Errorf("config: storage backend pebble requires MAILEZINE_ROCKS_PATH")
		}
	case "tidb":
		if c.Storage.DSN == "" {
			return fmt.Errorf("config: storage backend tidb requires MAILEZINE_STORAGE_DSN")
		}
	default:
		return fmt.Errorf("config: unsupported storage backend %q (want pebble|tidb)", c.Storage.Backend)
	}
	if c.Storage.S3Endpoint != "" {
		if c.Storage.S3AccessKey == "" || c.Storage.S3SecretKey == "" || c.Storage.S3Bucket == "" {
			return fmt.Errorf("config: S3 blob requires MAILEZINE_S3_ACCESS_KEY, MAILEZINE_S3_SECRET_KEY and MAILEZINE_S3_BUCKET")
		}
	}
	switch c.Cluster.Mode {
	case "", "single":
	case "multi":
		if c.HA.Enabled {
			return fmt.Errorf("config: cluster.mode=multi and ha.enabled are mutually exclusive (multi-active replaces leader/standby)")
		}
		if c.Storage.Backend != "tidb" {
			return fmt.Errorf("config: cluster.mode=multi requires storage backend \"tidb\" (claims and singleton leases need transactional fencing)")
		}
	default:
		return fmt.Errorf("config: unsupported cluster mode %q (want single|multi)", c.Cluster.Mode)
	}
	switch c.Directory.Mode {
	case "dev":
		if c.Directory.File == "" {
			return fmt.Errorf("config: directory mode dev requires MAILEZINE_DIRECTORY_FILE")
		}
	case "mailez":
		if c.BackendAddress == "" {
			return fmt.Errorf("config: directory mode mailez requires MAILEZINE_BACKEND_ADDRESS")
		}
	default:
		return fmt.Errorf("config: unsupported directory mode %q (want dev|mailez)", c.Directory.Mode)
	}
	switch c.Auth.Mode {
	case "dev":
		if c.Auth.DevPasswordsFile == "" {
			return fmt.Errorf("config: auth mode dev requires MAILEZINE_AUTH_DEV_FILE")
		}
	case "mailez":
		if c.BackendAddress == "" {
			return fmt.Errorf("config: auth mode mailez requires MAILEZINE_BACKEND_ADDRESS")
		}
	default:
		return fmt.Errorf("config: unsupported auth mode %q (want dev|mailez)", c.Auth.Mode)
	}
	if c.Management.Addr != "" {
		if _, port, err := net.SplitHostPort(c.Management.Addr); err != nil || port == "" {
			return fmt.Errorf("config: management addr %q must be host:port", c.Management.Addr)
		}
		if c.Management.Secret == "" {
			return fmt.Errorf("config: MAILEZINE_MANAGEMENT_SECRET is required when the management API is enabled")
		}
	}
	if c.Hostname == "" {
		return fmt.Errorf("config: MAILEZINE_HOSTNAME must not be empty")
	}
	for _, cidr := range c.TrustedNets {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: trusted net %q is not a valid CIDR: %v", cidr, err)
		}
	}
	for _, cidr := range c.ProxyTrusted {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: proxy trusted net %q is not a valid CIDR: %v", cidr, err)
		}
	}
	if c.Rspamd.URL != "" && !strings.HasPrefix(c.Rspamd.URL, "http://") && !strings.HasPrefix(c.Rspamd.URL, "https://") {
		return fmt.Errorf("config: rspamd URL %q must start with http:// or https://", c.Rspamd.URL)
	}
	if c.Outbound.Port <= 0 || c.Outbound.Port > 65535 {
		return fmt.Errorf("config: outbound port %d out of range", c.Outbound.Port)
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("config: MAILEZINE_TLS_CERT_FILE and MAILEZINE_TLS_KEY_FILE must be set together")
	}
	for _, p := range c.ProxyProtocol {
		if p != "all" {
			if n, err := strconv.Atoi(p); err != nil || n <= 0 || n > 65535 {
				return fmt.Errorf("config: proxy protocol port %q invalid (want a port or \"all\")", p)
			}
		}
	}
	return c.Limits.Validate()
}

// Summary is a one-line startup description for logs.
func (c Config) Summary() string {
	lic := c.License.Edition
	if c.License.IsEnterprise() {
		lic = fmt.Sprintf("enterprise(max=%d)", c.License.MaxMailboxes)
	}
	// Anti-spam tier actually in effect: rspamd (both editions; supervised
	// learning is enterprise), basic (built-in Authentication-Results +
	// DNSBL classifier) or off.
	junk := "off"
	if c.Rspamd.URL != "" {
		junk = "rspamd"
	} else if c.Junk.Enabled {
		junk = "basic"
	}
	return fmt.Sprintf(
		"cluster=%s storage=%s directory=%s auth=%s backend=%s hostname=%s tls=%v outbound=%v license=%s health=%s listeners=[smtp:%s imap:%s submission:%s sieve:%s pop3:%s] pop3=%v junk=%s jmap=%v maxMsg=%d",
		c.Cluster.Mode, c.Storage.Backend, c.Directory.Mode, c.Auth.Mode, c.BackendAddress, c.Hostname, c.TLS.CertFile != "", c.Outbound.Enabled,
		lic, c.HealthAddr, c.Listeners.SMTP, c.Listeners.IMAP, c.Listeners.Submission, c.Listeners.ManageSieve, c.Listeners.POP3,
		c.Features.POP3Enabled, junk, c.Features.JMAPEnabled,
		c.Limits.MaxMessageSize,
	)
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "mail.mailez.test"
	}
	return h
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// tomlString renders a TOML scalar as the string form used by the env
// parsing helpers (numbers/booleans are formatted the way env vars would be).
func tomlString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case time.Duration:
		return strconv.FormatInt(int64(t/time.Second), 10)
	default:
		return fmt.Sprintf("%v", t)
	}
}
