// Storage assembly for the composition root (ARCHITECTURE.md §3, D3/D4).
//
// The engine always opens a KV+blob pair: it is the spool of the outbound
// queue and, for the RocksDB/Pebble backend, also the mailbox index. The
// maildir backend (POSIX-only) replaces the mailbox surface while the queue
// still spools under <maildir>/.mailezine.
package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"mailezine/internal/config"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// storage bundles the durable backends of one process.
type storage struct {
	kv      store.KV
	blob    store.Blob
	mailbox mailboxBackend
	facade  *store.Store // account-scoped logical store (ListAccounts etc.)
}

// mailboxBackend is the full per-account surface the engine needs: mailboxes
// (IMAP/POP3/delivery) and Sieve scripts (ManageSieve/filtering). Both KV
// and maildir backends implement it.
type mailboxBackend interface {
	mailstore.MailboxStore
	mailstore.SieveStore
}

// Close releases the KV database. The blob backends have no close; the
// mailbox implementations are stateless.
func (s *storage) Close() error {
	if s.kv != nil {
		return s.kv.Close()
	}
	return nil
}

// newStorage opens the configured backend pair. mailbox is nil for the
// RocksDB/Pebble backends (the caller wraps KV+blob with mailstore.NewKV).
func newStorage(cfg config.Config, logger *slog.Logger) (*storage, error) {
	kv, blob, err := openKVBlob(cfg, logger)
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
	return &storage{kv: kv, blob: blob, mailbox: mailbox, facade: facade}, nil
}

// openKVBlob picks the KV and blob implementations from the config:
// RocksDB (build tag) or Pebble for KV; MinIO/S3 or local FS for blob.
func openKVBlob(cfg config.Config, logger *slog.Logger) (store.KV, store.Blob, error) {
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
