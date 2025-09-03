//go:build unix

// maildir → KV migration. The maildir
// side is POSIX-only, so this command compiles on unix; other platforms get
// a stub that explains the constraint. Migration preserves per-mailbox
// order, flags, keywords and internal dates; UIDs and UIDVALIDITY are
// freshly allocated (the store's invariants, INV-UID/INV-CHANGE, require
// that) — clients see a new UIDVALIDITY and re-sync, which is the correct
// RFC 3501 behaviour.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"mailezine/internal/app"
	"mailezine/internal/config"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
	maildirpkg "mailezine/internal/store/maildir"
)

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("mailezine migrate", flag.ContinueOnError)
	src := fs.String("src", "", "source maildir root")
	from := fs.String("from", "maildir", "source backend: maildir")
	to := fs.String("to", "pebble", "target backend: pebble|tidb")
	dsn := fs.String("dsn", "", "TiDB DSN (required when -to tidb)")
	dst := fs.String("dst", "", "target KV path (required when -to pebble)")
	dryRun := fs.Bool("dry-run", false, "scan and count without writing")
	s3 := s3Flags{}
	fs.StringVar(&s3.endpoint, "s3-endpoint", "", "S3/MinIO endpoint (enables S3 blob)")
	fs.StringVar(&s3.accessKey, "s3-access-key", "", "S3 access key")
	fs.StringVar(&s3.secretKey, "s3-secret-key", "", "S3 secret key")
	fs.StringVar(&s3.bucket, "s3-bucket", "", "S3 bucket")
	fs.BoolVar(&s3.useSSL, "s3-use-ssl", false, "use SSL for S3")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *src == "" {
		fmt.Fprintln(os.Stderr, "migrate: --src is required")
		fs.Usage()
		return 2
	}
	if *from != "maildir" {
		fmt.Fprintf(os.Stderr, "migrate: unsupported source backend %q\n", *from)
		return 2
	}
	var target config.StorageConfig
	switch *to {
	case "pebble":
		if *dst == "" {
			fmt.Fprintln(os.Stderr, "migrate: --dst is required for -to pebble")
			return 2
		}
		target = s3.storageConfig("pebble", *dst)
	case "tidb":
		if *dsn == "" {
			fmt.Fprintln(os.Stderr, "migrate: --dsn is required for -to tidb")
			return 2
		}
		target = s3.storageConfigDSN("tidb", *dsn)
	default:
		fmt.Fprintf(os.Stderr, "migrate: unsupported target backend %q\n", *to)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	return migrateMaildirToKV(*src, target, *dryRun, logger)
}

type s3Flags struct {
	endpoint, accessKey, secretKey, bucket string
	useSSL                                 bool
}

func (f s3Flags) storageConfig(backend, path string) config.StorageConfig {
	return config.StorageConfig{
		Backend:     backend,
		RocksPath:   path,
		S3Endpoint:  f.endpoint,
		S3AccessKey: f.accessKey,
		S3SecretKey: f.secretKey,
		S3Bucket:    f.bucket,
		S3UseSSL:    f.useSSL,
	}
}

func (f s3Flags) storageConfigDSN(backend, dsn string) config.StorageConfig {
	return config.StorageConfig{
		Backend:     backend,
		DSN:         dsn,
		S3Endpoint:  f.endpoint,
		S3AccessKey: f.accessKey,
		S3SecretKey: f.secretKey,
		S3Bucket:    f.bucket,
		S3UseSSL:    f.useSSL,
	}
}

func migrateMaildirToKV(src string, target config.StorageConfig, dryRun bool, logger *slog.Logger) int {
	kv, blob, err := app.OpenKVBlob(config.Config{Storage: target}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: open target: %v\n", err)
		return 2
	}
	defer func() { _ = kv.Close() }()
	ms := mailstore.NewKV(store.New(kv, blob))

	ctx := context.Background()
	accounts, err := os.ReadDir(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: read %s: %v\n", src, err)
		return 2
	}
	var totalMessages, totalBytes int64
	migrated := 0
	for _, entry := range accounts {
		if !entry.IsDir() {
			continue
		}
		accountDir := filepath.Join(src, entry.Name())
		acct, err := maildirpkg.OpenAccount(accountDir)
		if err != nil {
			continue
		}
		mailboxes, err := acct.Mailboxes()
		if err != nil || len(mailboxes) == 0 {
			continue
		}
		var accountMessages int64
		for _, mailbox := range mailboxes {
			mb, err := acct.OpenMailbox(mailbox)
			if err != nil {
				return fail("open mailbox %s/%s: %v", entry.Name(), mailbox, err)
			}
			msgs, err := mb.Messages()
			if err != nil {
				return fail("list %s/%s: %v", entry.Name(), mailbox, err)
			}
			for _, msg := range msgs {
				totalMessages++
				accountMessages++
				totalBytes += msg.Size
				if dryRun {
					continue
				}
				rc, err := mb.Open(msg.UID)
				if err != nil {
					return fail("open %s/%s uid=%d: %v", entry.Name(), mailbox, msg.UID, err)
				}
				var buf bytes.Buffer
				_, err = io.Copy(&buf, rc)
				_ = rc.Close()
				if err != nil {
					return fail("read %s/%s uid=%d: %v", entry.Name(), mailbox, msg.UID, err)
				}
				keywords, _ := mb.Keywords(msg.UID)
				flags := append(maildirpkg.MaildirToIMAPFlags(msg.Flags), keywords...)
				if _, err := ms.Deliver(ctx, entry.Name(), mailbox, &mailstore.Message{
					Data:         buf.Bytes(),
					Flags:        flags,
					InternalDate: msg.InternalDate,
				}); err != nil {
					return fail("store %s/%s uid=%d: %v", entry.Name(), mailbox, msg.UID, err)
				}
			}
		}
		if accountMessages > 0 {
			logger.Info("migrate", "account", entry.Name(), "messages", accountMessages)
			migrated++
		}
	}
	if dryRun {
		fmt.Printf("dry-run: %d accounts, %d messages, %d bytes\n", migrated, totalMessages, totalBytes)
	} else {
		fmt.Printf("migrated: %d accounts, %d messages, %d bytes -> %s\n",
			migrated, totalMessages, totalBytes, targetLabel(target))
	}
	return 0
}

// targetLabel describes the migration target for the summary line.
func targetLabel(c config.StorageConfig) string {
	if c.Backend == "tidb" {
		return "tidb (" + c.DSN + ")"
	}
	return "pebble (" + c.RocksPath + ")"
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	return 2
}
