// Storage assembly (ARCHITECTURE.md §3, D3/D4): the engine always opens a
// KV+blob pair — it is the spool of the outbound queue and, for the
// RocksDB/Pebble backend, also the mailbox index. The maildir backend
// (POSIX-only) replaces the mailbox surface while the queue still spools
// under <maildir>/.mailezine.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"

	"mailezine/internal/config"
	"mailezine/internal/fts"
	"mailezine/internal/ha"
	"mailezine/internal/maildns"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// Storage bundles the durable backends of one process. It is exported so
// the migrate/reindex sub-commands can open the same backend pair.
type Storage struct {
	kv      store.KV
	blob    store.Blob
	mailbox mailboxBackend
	facade  *store.Store // account-scoped logical store (ListAccounts etc.)
}

// MailboxBackend is the full per-account surface the engine needs: mailboxes
// (IMAP/POP3/delivery) and Sieve scripts (ManageSieve/filtering).
type MailboxBackend interface {
	mailstore.MailboxStore
	mailstore.SieveStore
	mailstore.ACLStore
}

type mailboxBackend = MailboxBackend

// Facade returns the account-scoped logical store.
func (s *Storage) Facade() *store.Store { return s.facade }

// Mailbox returns the mailbox backend (KV or maildir).
func (s *Storage) Mailbox() MailboxBackend { return s.mailbox }

// Close releases the KV database. The blob backends have no close; the
// mailbox implementations are stateless.
func (s *Storage) Close() error {
	if s.kv != nil {
		return s.kv.Close()
	}
	return nil
}

// NewStorage opens the configured backend pair. mailbox is nil for the
// RocksDB/Pebble backends (the caller wraps KV+blob with mailstore.NewKV).
func NewStorage(cfg config.Config, logger *slog.Logger) (*Storage, error) {
	kv, blob, err := OpenKVBlob(cfg, logger)
	if err != nil {
		return nil, err
	}
	mailbox, err := openMailbox(cfg, logger)
	if err != nil {
		_ = kv.Close()
		return nil, err
	}
	facade := store.New(kv, blob)
	if mailbox == nil {
		mailbox = mailstore.NewKV(facade)
	}
	return &Storage{kv: kv, blob: blob, mailbox: mailbox, facade: facade}, nil
}

// OpenKVBlob picks the KV and blob implementations from the config:
// RocksDB (build tag) or Pebble for KV; MinIO/S3 or local FS for blob.
func OpenKVBlob(cfg config.Config, logger *slog.Logger) (store.KV, store.Blob, error) {
	kvPath, blobRoot := cfg.Storage.RocksPath, cfg.Storage.RocksPath+".blobs"
	if cfg.Storage.Backend == "maildir" {
		base := filepath.Join(cfg.Storage.MaildirPath, ".mailezine")
		kvPath, blobRoot = filepath.Join(base, "queue.kv"), filepath.Join(base, "blobs")
	}

	var kv store.KV
	var err error
	switch cfg.Storage.Backend {
	case "rocksdb":
		kv, err = openRocks(kvPath, logger)
	case "pebble", "maildir":
		kv, err = store.OpenPebble(kvPath)
	case "tidb":
		kv, err = store.OpenTiDB(cfg.Storage.DSN, "mailezine_kv")
	default:
		err = errors.New("storage: unknown backend (validated earlier)")
	}
	if err != nil {
		return nil, nil, err
	}

	var blob store.Blob
	if cfg.Storage.S3Endpoint != "" {
		newBlob := store.NewS3Blob
		if cfg.Storage.Compression {
			newBlob = store.NewS3BlobCompressed
		}
		b, berr := newBlob(
			cfg.Storage.S3Endpoint,
			cfg.Storage.S3AccessKey,
			cfg.Storage.S3SecretKey,
			cfg.Storage.S3Bucket,
			cfg.Storage.S3UseSSL,
		)
		if berr != nil {
			_ = kv.Close()
			return nil, nil, berr
		}
		if berr := b.EnsureBucket(context.Background()); berr != nil {
			logger.Warn("storage: s3 bucket ensure", "err", berr)
		}
		blob = b
		logger.Info("storage: blob", "backend", "s3", "bucket", cfg.Storage.S3Bucket, "compressed", cfg.Storage.Compression)
	} else {
		var b *store.FSBlob
		var berr error
		if cfg.Storage.Compression {
			b, berr = store.NewFSBlobCompressed(blobRoot)
		} else {
			b, berr = store.NewFSBlob(blobRoot)
		}
		if berr != nil {
			_ = kv.Close()
			return nil, nil, berr
		}
		blob = b
		logger.Info("storage: blob", "backend", "fs", "root", blobRoot, "compressed", cfg.Storage.Compression)
	}
	return kv, blob, nil
}

// openFTS opens the embedded bleve index; a corrupt/unusable index disables
// FTS (search falls back to a full scan).
func openFTS(cfg config.Config, logger *slog.Logger) *fts.Indexer {
	path := cfg.FTS.Path
	if path == "" {
		base := cfg.Storage.RocksPath
		if cfg.Storage.Backend == "maildir" {
			base = filepath.Join(cfg.Storage.MaildirPath, ".mailezine")
		}
		path = base + ".fts"
	}
	idx, err := fts.Open(path, cfg.FTS.TikaURL, logger)
	if err != nil {
		logger.Warn("fts: disabled", "path", path, "err", err)
		return nil
	}
	logger.Info("fts: enabled", "path", path, "tika", cfg.FTS.TikaURL != "")
	return idx
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

func newSystemResolver() *maildns.SystemResolver {
	return maildns.NewSystemResolver()
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
