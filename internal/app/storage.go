// Storage assembly (ARCHITECTURE.md §3, D3/D4): the engine always opens a
// KV+blob pair — it is the spool of the outbound queue and the mailbox
// index. Pebble is the single-node KV, TiDB the distributed path.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"mailezine/internal/config"
	"mailezine/internal/fts"
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

// NewStorage opens the configured backend pair. The mailbox surface is
// always the KV+blob implementation (mailstore.NewKV).
func NewStorage(cfg config.Config, logger *slog.Logger) (*Storage, error) {
	kv, blob, err := OpenKVBlob(cfg, logger)
	if err != nil {
		return nil, err
	}
	facade := store.New(kv, blob)
	mailbox := mailstore.NewKV(facade)
	return &Storage{kv: kv, blob: blob, mailbox: mailbox, facade: facade}, nil
}

// OpenKVBlob picks the KV and blob implementations from the config:
// Pebble for KV and the local FS for blobs in the community build; the
// enterprise build additionally registers TiDB (KV) and S3 (blob) openers
// into the store registry.
func OpenKVBlob(cfg config.Config, logger *slog.Logger) (store.KV, store.Blob, error) {
	kvPath, blobRoot := cfg.Storage.RocksPath, cfg.Storage.RocksPath+".blobs"

	var kv store.KV
	var err error
	switch cfg.Storage.Backend {
	case "pebble":
		kv, err = store.OpenPebble(kvPath)
	case "tidb":
		// Scale-out KV: registered by the enterprise build only.
		if op := store.LookupKVOpener("tidb"); op != nil {
			kv, err = op(cfg.Storage.DSN, "mailezine_kv")
		} else {
			err = fmt.Errorf("storage: backend %q requires the enterprise edition", cfg.Storage.Backend)
		}
	default:
		err = errors.New("storage: unknown backend (validated earlier)")
	}
	if err != nil {
		return nil, nil, err
	}

	var blob store.Blob
	if cfg.Storage.S3Endpoint != "" {
		op := store.S3BlobOpenerFor()
		if op == nil {
			_ = kv.Close()
			return nil, nil, fmt.Errorf("storage: s3 blob backend requires the enterprise edition")
		}
		b, berr := op(
			cfg.Storage.S3Endpoint,
			cfg.Storage.S3AccessKey,
			cfg.Storage.S3SecretKey,
			cfg.Storage.S3Bucket,
			cfg.Storage.S3UseSSL,
			cfg.Storage.Compression,
		)
		if berr != nil {
			_ = kv.Close()
			return nil, nil, berr
		}
		if s3c, ok := b.(interface {
			EnsureBucket(ctx context.Context) error
		}); ok {
			if berr := s3c.EnsureBucket(context.Background()); berr != nil {
				logger.Warn("storage: s3 bucket ensure", "err", berr)
			}
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
