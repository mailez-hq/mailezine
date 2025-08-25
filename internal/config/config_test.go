package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mailezine/internal/limits"
)

func base() Config {
	return Config{
		Log:            LogConfig{Level: "info", Format: "text"},
		HealthAddr:     ":11480",
		Listeners:      ListenersConfig{SMTP: ":1025", Submission: ":1587", IMAP: ":1143", ManageSieve: ":11490", POP3: ":10110"},
		Storage:        StorageConfig{Backend: "maildir", MaildirPath: "./data/mail"},
		Directory:      DirectoryConfig{Mode: "dev", File: "testdata/directory.json", CacheTTL: 30 * time.Second},
		Auth:           AuthConfig{Mode: "dev", DevPasswordsFile: "testdata/passwords.json"},
		BackendAddress: "127.0.0.1:8080",
		Hostname:       "mail.mailez.test",
		TrustedNets:    []string{"127.0.0.1/8"},
		DKIMVaultURL:   "http://127.0.0.1:8080/stack/rspamd/vault",
		Outbound:       OutboundConfig{Enabled: true, Port: 25},
		Limits:         limits.Defaults(),
	}
}

func TestValidate(t *testing.T) {
	t.Run("valid maildir dev", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	})
	t.Run("maildir requires path", func(t *testing.T) {
		c := base()
		c.Storage.MaildirPath = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for empty maildir path")
		}
	})
	t.Run("unknown backend", func(t *testing.T) {
		c := base()
		c.Storage.Backend = "bogus"
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for unknown backend")
		}
	})
	t.Run("implicit TLS addresses parsed", func(t *testing.T) {
		t.Setenv("MAILEZINE_STORAGE_BACKEND", "pebble")
		t.Setenv("MAILEZINE_ROCKS_PATH", "/data/rocks")
		t.Setenv("MAILEZINE_DIRECTORY_FILE", "/data/directory.json")
		t.Setenv("MAILEZINE_AUTH_DEV_FILE", "/data/passwords.json")
		t.Setenv("MAILEZINE_SMTPS_ADDR", ":465")
		t.Setenv("MAILEZINE_IMAPS_ADDR", ":993")
		t.Setenv("MAILEZINE_POP3S_ADDR", ":995")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Listeners.SMTPS != ":465" || cfg.Listeners.IMAPS != ":993" || cfg.Listeners.POP3S != ":995" {
			t.Fatalf("implicit TLS listeners: %+v", cfg.Listeners)
		}
	})
	t.Run("dev directory requires file", func(t *testing.T) {
		c := base()
		c.Directory.File = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for missing directory file")
		}
	})
	t.Run("dev auth requires file", func(t *testing.T) {
		c := base()
		c.Auth.DevPasswordsFile = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for missing auth file")
		}
	})
	t.Run("rocksdb requires path", func(t *testing.T) {
		c := base()
		c.Storage.Backend = "rocksdb"
		c.Storage.RocksPath = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for empty rocks path")
		}
	})
	t.Run("pebble requires path", func(t *testing.T) {
		c := base()
		c.Storage.Backend = "pebble"
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for pebble without path")
		}
		c.Storage.RocksPath = "./data/rocks"
		if err := c.Validate(); err != nil {
			t.Fatalf("valid pebble config rejected: %v", err)
		}
	})
	t.Run("management requires secret", func(t *testing.T) {
		c := base()
		c.Management = ManagementConfig{Addr: ":8090"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for management without secret")
		}
		c.Management.Secret = "sekret"
		if err := c.Validate(); err != nil {
			t.Fatalf("valid management config rejected: %v", err)
		}
	})
	t.Run("bad listener addr", func(t *testing.T) {
		c := base()
		c.Listeners.IMAP = "1143" // missing colon
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for malformed listener addr")
		}
	})
	t.Run("bad trusted net", func(t *testing.T) {
		c := base()
		c.TrustedNets = []string{"not-a-cidr"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for malformed trusted net")
		}
	})
	t.Run("bad rspamd url", func(t *testing.T) {
		c := base()
		c.Rspamd.URL = "ftp://rspamd/checkv2"
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for non-http rspamd url")
		}
		c.Rspamd.URL = "http://rspamd:11333/checkv2"
		if err := c.Validate(); err != nil {
			t.Fatalf("valid rspamd config rejected: %v", err)
		}
	})
	t.Run("bad outbound port", func(t *testing.T) {
		c := base()
		c.Outbound.Port = 0
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for zero outbound port")
		}
	})
	t.Run("tls requires both files", func(t *testing.T) {
		c := base()
		c.TLS.CertFile = "cert.pem" // key missing
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for cert without key")
		}
		c.TLS.KeyFile = "key.pem"
		if err := c.Validate(); err != nil {
			t.Fatalf("valid tls config rejected: %v", err)
		}
	})
	t.Run("proxy protocol ports", func(t *testing.T) {
		c := base()
		c.ProxyProtocol = []string{"25", "143"}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid proxy config rejected: %v", err)
		}
		c.ProxyProtocol = []string{"bogus"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for invalid proxy port")
		}
	})
}

func TestSummary(t *testing.T) {
	s := base().Summary()
	if s == "" || s == " " {
		t.Fatalf("summary unexpectedly empty")
	}
}

func TestLoadTOMLOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mailezine.toml")
	content := `
MAILEZINE_LOG_LEVEL = "debug"
MAILEZINE_STORAGE_BACKEND = "pebble"
MAILEZINE_ROCKS_PATH = "/data/rocks"
MAILEZINE_POP3_ENABLED = false
MAILEZINE_DIRECTORY_FILE = "/conf/directory.json"
MAILEZINE_AUTH_DEV_FILE = "/conf/passwords.json"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILEZINE_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != "debug" || cfg.Storage.Backend != "pebble" ||
		cfg.Storage.RocksPath != "/data/rocks" || cfg.Features.POP3Enabled {
		t.Fatalf("toml overlay not applied: %+v", cfg)
	}

	// Explicit environment wins over the file.
	t.Setenv("MAILEZINE_STORAGE_BACKEND", "maildir")
	t.Setenv("MAILEZINE_MAILDIR_PATH", "/mail")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Backend != "maildir" || cfg.Storage.MaildirPath != "/mail" {
		t.Fatalf("env should override toml: %+v", cfg)
	}
}
