// backup / restore sub-commands: full-archive export and import of the
// KV+blob store (internal/backup). Unlike migrate, these are platform-
// neutral — they only touch the KV and blob abstractions, never maildir —
// so they build everywhere and carry no build tags. Identifiers are prefixed
// to stay clear of migrate.go, which compiles alongside on unix.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"mailezine/internal/app"
	"mailezine/internal/backup"
	"mailezine/internal/config"
)

// bkStorage mirrors the storage flags of migrate for one store: the one the
// backup reads from, or the one the restore writes into.
type bkStorage struct {
	backend, path, dsn       string
	s3Endpoint               string
	s3AccessKey, s3SecretKey string
	s3Bucket                 string
	s3UseSSL                 bool
}

func newBKStorage(fs *flag.FlagSet) *bkStorage {
	s := &bkStorage{}
	fs.StringVar(&s.backend, "backend", "pebble", "storage backend: pebble|tidb")
	fs.StringVar(&s.path, "path", "", "Pebble data directory (required for -backend pebble)")
	fs.StringVar(&s.dsn, "dsn", "", "TiDB DSN (required for -backend tidb)")
	fs.StringVar(&s.s3Endpoint, "s3-endpoint", "", "S3/MinIO endpoint (enables S3 blob)")
	fs.StringVar(&s.s3AccessKey, "s3-access-key", "", "S3 access key")
	fs.StringVar(&s.s3SecretKey, "s3-secret-key", "", "S3 secret key")
	fs.StringVar(&s.s3Bucket, "s3-bucket", "", "S3 bucket")
	fs.BoolVar(&s.s3UseSSL, "s3-use-ssl", false, "use SSL for S3")
	return s
}

func (s *bkStorage) config() (config.StorageConfig, error) {
	switch s.backend {
	case "pebble":
		if s.path == "" {
			return config.StorageConfig{}, fmt.Errorf("--path is required for -backend pebble")
		}
		return config.StorageConfig{
			Backend:     "pebble",
			KVPath:   s.path,
			S3Endpoint:  s.s3Endpoint,
			S3AccessKey: s.s3AccessKey,
			S3SecretKey: s.s3SecretKey,
			S3Bucket:    s.s3Bucket,
			S3UseSSL:    s.s3UseSSL,
		}, nil
	case "tidb":
		if s.dsn == "" {
			return config.StorageConfig{}, fmt.Errorf("--dsn is required for -backend tidb")
		}
		return config.StorageConfig{
			Backend:     "tidb",
			DSN:         s.dsn,
			S3Endpoint:  s.s3Endpoint,
			S3AccessKey: s.s3AccessKey,
			S3SecretKey: s.s3SecretKey,
			S3Bucket:    s.s3Bucket,
			S3UseSSL:    s.s3UseSSL,
		}, nil
	default:
		return config.StorageConfig{}, fmt.Errorf("unsupported backend %q (pebble|tidb)", s.backend)
	}
}

func runBackup(args []string) int {
	fs := flag.NewFlagSet("mailezine backup", flag.ContinueOnError)
	out := fs.String("out", "", "archive file to write (required)")
	compress := fs.Bool("compress", true, "gzip-compress the archive")
	st := newBKStorage(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "backup: --out is required")
		fs.Usage()
		return 2
	}
	sc, err := st.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	kv, blob, err := app.OpenKVBlob(config.Config{Storage: sc}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: open store: %v\n", err)
		return 2
	}
	defer func() { _ = kv.Close() }()

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: create %s: %v\n", *out, err)
		return 2
	}
	defer f.Close()

	m, err := backup.Backup(ctx, kv, blob, f, backup.Options{Backend: st.backend, Compress: *compress, Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\narchive %s is incomplete and will be rejected by restore\n", err, *out)
		return 1
	}
	fmt.Printf("backed up: %d kv entries (%d bytes), %d blobs (%d bytes) -> %s\n",
		m.KVEntries, m.KVBytes, m.Blobs, m.BlobBytes, *out)
	if m.MissingBlobs > 0 {
		fmt.Printf("warning: %d referenced blobs were missing from the blob store\n", m.MissingBlobs)
	}
	if m.Drifted {
		fmt.Println("warning: the store changed during the walk; stop the server next time for a point-in-time backup")
	}
	return 0
}

func runRestore(args []string) int {
	fs := flag.NewFlagSet("mailezine restore", flag.ContinueOnError)
	in := fs.String("in", "", "archive file to read (required)")
	overwrite := fs.Bool("overwrite", false, "merge into a non-empty target instead of refusing")
	dryRun := fs.Bool("dry-run", false, "verify the archive without writing anything")
	st := newBKStorage(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "restore: --in is required")
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: open %s: %v\n", *in, err)
		return 2
	}
	defer f.Close()

	if *dryRun {
		// Verify-only never touches the store; nil backends are safe by
		// contract (Restore writes nothing in this mode).
		m, err := backup.Restore(ctx, nil, nil, f, backup.RestoreOptions{VerifyOnly: true})
		if err != nil {
			fmt.Fprintf(os.Stderr, "restore: verify %s: %v\n", *in, err)
			return 1
		}
		fmt.Printf("verified: %d kv entries (%d bytes), %d blobs (%d bytes), sha256 %s\n",
			m.KVEntries, m.KVBytes, m.Blobs, m.BlobBytes, m.SHA256)
		return 0
	}

	sc, err := st.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 2
	}
	kv2, blob2, err := app.OpenKVBlob(config.Config{Storage: sc}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: open store: %v\n", err)
		return 2
	}
	defer func() { _ = kv2.Close() }()

	m, err := backup.Restore(ctx, kv2, blob2, f, backup.RestoreOptions{Overwrite: *overwrite, Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	fmt.Printf("restored: %d kv entries (%d bytes), %d blobs (%d bytes) <- %s\n",
		m.KVEntries, m.KVBytes, m.Blobs, m.BlobBytes, *in)
	if m.MissingBlobs > 0 {
		fmt.Printf("warning: the archive references %d blobs that were already missing at backup time\n", m.MissingBlobs)
	}
	return 0
}
