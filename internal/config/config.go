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
	Directory      DirectoryConfig
	Auth           AuthConfig
	Management     ManagementConfig
	BackendAddress string
	Hostname       string
	// RecipientDelimiter is the extended-address separator: mail to
	// "user+tag@domain" delivers to the base user when the full address is
	// unknown. Empty disables plus addressing.
	RecipientDelimiter string
	TrustedNets        []string // CIDRs that are authenticated by the gateway
	Rspamd             RspamdConfig
	DKIMVaultURL       string
	Outbound           OutboundConfig
	TLS                TLSConfig
	ProxyProtocol      []string // listener ports expecting a PROXY v1 header
	Queue              QueueConfig
	Limits             limits.Config
	Features           FeaturesConfig
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
	Backend     string // maildir|rocksdb
	MaildirPath string
	RocksPath   string
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
	// Path is the index directory; empty derives <rockspath>.fts (or the
	// maildir .mailezine/fts).
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

// OutboundConfig controls relay submission to the queue.
type OutboundConfig struct {
	Enabled bool
	Port    int // delivery port for remote MXes, default 25
	// SmarthostUsername/Password enable SASL AUTH when delivering through
	// a configured smarthost (directory relay transport "smtp:[host]").
	SmarthostUsername string
	SmarthostPassword string
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
}

// FeaturesConfig gates optional components; switches only decide whether a
// component starts, never the semantics of the core path (ARCHITECTURE.md §10.3).
type FeaturesConfig struct {
	POP3Enabled bool
	JunkEnabled bool
	JMAPEnabled bool
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
			Backend:     getenv("MAILEZINE_STORAGE_BACKEND", "maildir"),
			MaildirPath: getenv("MAILEZINE_MAILDIR_PATH", ""),
			RocksPath:   getenv("MAILEZINE_ROCKS_PATH", ""),
			S3Endpoint:  getenv("MAILEZINE_S3_ENDPOINT", ""),
			S3AccessKey: getenv("MAILEZINE_S3_ACCESS_KEY", ""),
			S3SecretKey: getenv("MAILEZINE_S3_SECRET_KEY", ""),
			S3Bucket:    getenv("MAILEZINE_S3_BUCKET", ""),
			S3UseSSL:    envBool("MAILEZINE_S3_USE_SSL", false),
			Compression: envBool("MAILEZINE_BLOB_COMPRESSION", false),
		},
		FTS: FTSConfig{
			Enabled: envBool("MAILEZINE_FTS_ENABLED", false),
			Path:    getenv("MAILEZINE_FTS_PATH", ""),
			TikaURL: getenv("MAILEZINE_FTS_TIKA_URL", ""),
		},
		HA: HAConfig{
			Enabled:    envBool("MAILEZINE_HA_ENABLED", false),
			LeasePath:  getenv("MAILEZINE_HA_LEASE_PATH", ""),
			TTLSeconds: envInt("MAILEZINE_HA_TTL_SECONDS", 15),
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
		Management: ManagementConfig{
			Addr:   getenv("MAILEZINE_MANAGEMENT_ADDR", ""),
			Secret: getenv("MAILEZINE_MANAGEMENT_SECRET", ""),
		},
		BackendAddress: getenv("MAILEZINE_BACKEND_ADDRESS", "127.0.0.1:8080"),
		TrustedNets:    splitCSV(getenv("MAILEZINE_TRUSTED_NETS", "127.0.0.1/8,::1/128")),
		Rspamd: RspamdConfig{
			URL:      getenv("MAILEZINE_RSPAMD_URL", ""),
			LearnURL: getenv("MAILEZINE_RSPAMD_LEARN_URL", ""),
			Password: getenv("MAILEZINE_RSPAMD_PASSWORD", ""),
		},
		DKIMVaultURL: getenv("MAILEZINE_DKIM_VAULT_URL", ""),
		Outbound: OutboundConfig{
			Enabled:           envBool("MAILEZINE_OUTBOUND_ENABLED", true),
			Port:              envInt("MAILEZINE_OUTBOUND_PORT", 25),
			SmarthostUsername: getenv("MAILEZINE_OUTBOUND_SMTP_USERNAME", ""),
			SmarthostPassword: getenv("MAILEZINE_OUTBOUND_SMTP_PASSWORD", ""),
		},
		TLS: TLSConfig{
			CertFile: getenv("MAILEZINE_TLS_CERT_FILE", ""),
			KeyFile:  getenv("MAILEZINE_TLS_KEY_FILE", ""),
		},
		ProxyProtocol: splitCSV(getenv("MAILEZINE_PROXY_PROTOCOL", "")),
		Queue: QueueConfig{
			MaxAttempts:  envInt("MAILEZINE_QUEUE_MAX_ATTEMPTS", 10),
			BaseRetry:    time.Duration(envInt("MAILEZINE_QUEUE_BASE_RETRY_SECONDS", 60)) * time.Second,
			MaxRetry:     time.Duration(envInt("MAILEZINE_QUEUE_MAX_RETRY_SECONDS", 24*3600)) * time.Second,
			PollInterval: time.Duration(envInt("MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS", 5)) * time.Second,
			DelayWarning: time.Duration(envInt("MAILEZINE_QUEUE_DELAY_WARNING_SECONDS", 300)) * time.Second,
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
	if cfg.DKIMVaultURL == "" {
		cfg.DKIMVaultURL = "http://" + cfg.BackendAddress + "/stack/rspamd/vault"
	}

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
	case "maildir":
		if c.Storage.MaildirPath == "" {
			return fmt.Errorf("config: storage backend maildir requires MAILEZINE_MAILDIR_PATH")
		}
	case "rocksdb", "pebble":
		if c.Storage.RocksPath == "" {
			return fmt.Errorf("config: storage backend %s requires MAILEZINE_ROCKS_PATH", c.Storage.Backend)
		}
	default:
		return fmt.Errorf("config: unsupported storage backend %q (want maildir|rocksdb|pebble)", c.Storage.Backend)
	}
	if c.Storage.S3Endpoint != "" {
		if c.Storage.S3AccessKey == "" || c.Storage.S3SecretKey == "" || c.Storage.S3Bucket == "" {
			return fmt.Errorf("config: S3 blob requires MAILEZINE_S3_ACCESS_KEY, MAILEZINE_S3_SECRET_KEY and MAILEZINE_S3_BUCKET")
		}
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
	return fmt.Sprintf(
		"storage=%s directory=%s auth=%s backend=%s hostname=%s tls=%v rspamd=%v outbound=%v health=%s listeners=[smtp:%s imap:%s submission:%s sieve:%s pop3:%s] pop3=%v junk=%v jmap=%v maxMsg=%d",
		c.Storage.Backend, c.Directory.Mode, c.Auth.Mode, c.BackendAddress, c.Hostname, c.TLS.CertFile != "", c.Rspamd.URL != "", c.Outbound.Enabled, c.HealthAddr,
		c.Listeners.SMTP, c.Listeners.IMAP, c.Listeners.Submission, c.Listeners.ManageSieve, c.Listeners.POP3,
		c.Features.POP3Enabled, c.Features.JunkEnabled, c.Features.JMAPEnabled,
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
