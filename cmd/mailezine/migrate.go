//go:build unix

// maildir ⇄ KV migration. The maildir side is POSIX-only (D7), so this
// command compiles on unix; other platforms get a stub that explains the
// constraint. Migration preserves per-mailbox order, flags, keywords and
// internal dates; UIDs and UIDVALIDITY are freshly allocated (the store's
// invariants, INV-UID/INV-CHANGE, require that) — clients see a new
// UIDVALIDITY and re-sync, which is the correct RFC 3501 behaviour.
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
	"sort"

	"mailezine/internal/app"
	"mailezine/internal/config"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
	maildirpkg "mailezine/internal/store/maildir"
)

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("mailezine migrate", flag.ContinueOnError)
	src := fs.String("src", "", "source path: maildir root or KV path")
	from := fs.String("from", "maildir", "source backend: maildir|pebble|rocksdb")
	to := fs.String("to", "pebble", "target backend: maildir|pebble|rocksdb")
	dst := fs.String("dst", "", "target path: maildir root or KV path")
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
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "migrate: --src and --dst are required")
		fs.Usage()
		return 2
	}
	if *from != "maildir" && *from != "pebble" && *from != "rocksdb" {
		fmt.Fprintf(os.Stderr, "migrate: unsupported source backend %q\n", *from)
		return 2
	}
	if *to != "maildir" && *to != "pebble" && *to != "rocksdb" {
		fmt.Fprintf(os.Stderr, "migrate: unsupported target backend %q\n", *to)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch {
	case *from == "maildir" && *to != "maildir":
		return migrateMaildirToKV(*src, *to, *dst, *dryRun, s3, logger)
	case *from != "maildir" && *to == "maildir":
		return migrateKVToMaildir(*from, *src, *dst, *dryRun, s3, logger)
	default:
		fmt.Fprintln(os.Stderr, "migrate: only maildir ⇄ pebble|rocksdb migrations are supported")
		return 2
	}
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

func migrateMaildirToKV(src, to, dst string, dryRun bool, s3 s3Flags, logger *slog.Logger) int {
	kv, blob, err := app.OpenKVBlob(config.Config{
		Storage: s3.storageConfig(to, dst),
	}, logger)
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
				flags := append(mailstore.MaildirToIMAPFlags(msg.Flags), keywords...)
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
		fmt.Printf("migrated: %d accounts, %d messages, %d bytes -> %s\n", migrated, totalMessages, totalBytes, dst)
	}
	return 0
}

// migrateKVToMaildir exports every account/mailbox of a KV store into a
// maildir root (one directory per account, Maildir++ layout).
func migrateKVToMaildir(from, src, dst string, dryRun bool, s3 s3Flags, logger *slog.Logger) int {
	kv, blob, err := app.OpenKVBlob(config.Config{
		Storage: s3.storageConfig(from, src),
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: open source: %v\n", err)
		return 2
	}
	defer func() { _ = kv.Close() }()
	srcStore := store.New(kv, blob)
	ms := mailstore.NewKV(srcStore)
	dstStore := mailstore.NewMaildir(dst)
	ctx := context.Background()

	accounts, err := srcStore.ListAccounts(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: list accounts: %v\n", err)
		return 2
	}
	sort.Strings(accounts)
	var totalMessages, totalBytes int64
	for _, account := range accounts {
		mailboxes, err := ms.ListMailboxes(ctx, account)
		if err != nil {
			return fail("list mailboxes %s: %v", account, err)
		}
		var accountMessages int64
		for _, mb := range mailboxes {
			msgs, err := ms.ListMessages(ctx, account, mb.Name)
			if err != nil {
				return fail("list %s/%s: %v", account, mb.Name, err)
			}
			for _, msg := range msgs {
				totalMessages++
				accountMessages++
				totalBytes += msg.Size
				if dryRun {
					continue
				}
				rc, err := ms.OpenMessage(ctx, account, mb.Name, msg.UID)
				if err != nil {
					return fail("open %s/%s uid=%d: %v", account, mb.Name, msg.UID, err)
				}
				var buf bytes.Buffer
				_, err = buf.ReadFrom(rc)
				_ = rc.Close()
				if err != nil {
					return fail("read %s/%s uid=%d: %v", account, mb.Name, msg.UID, err)
				}
				if _, err := dstStore.Deliver(ctx, account, mb.Name, &mailstore.Message{
					Data:         buf.Bytes(),
					Flags:        msg.Flags,
					Keywords:     msg.Keywords,
					InternalDate: msg.InternalDate,
				}); err != nil {
					return fail("store maildir %s/%s uid=%d: %v", account, mb.Name, msg.UID, err)
				}
			}
		}
		if accountMessages > 0 {
			logger.Info("migrate", "account", account, "messages", accountMessages)
		}
	}
	if dryRun {
		fmt.Printf("dry-run: %d accounts, %d messages, %d bytes\n", len(accounts), totalMessages, totalBytes)
	} else {
		fmt.Printf("migrated: %d accounts, %d messages, %d bytes -> %s\n", len(accounts), totalMessages, totalBytes, dst)
	}
	return 0
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	return 2
}
