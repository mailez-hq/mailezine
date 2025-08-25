//go:build !windows

// reindex rebuilds the full-text index from the stored mailboxes — the
// maintenance command for FTS (run after a corrupt/lost index, or after
// enabling FTS on existing data). Usage:
//
//	mailezine reindex --storage pebble --rocks-path /data/rocks [--s3-*]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"mailezine/internal/app"
	"mailezine/internal/config"
	"mailezine/internal/fts"
)

func runReindex(args []string) int {
	fs := flag.NewFlagSet("mailezine reindex", flag.ContinueOnError)
	backend := fs.String("storage", "pebble", "storage backend: pebble|rocksdb|maildir")
	rocksPath := fs.String("rocks-path", "", "KV path (pebble/rocksdb)")
	maildirPath := fs.String("maildir-path", "", "maildir root (maildir backend)")
	ftsPath := fs.String("fts-path", "", "index directory (default <rocks-path>.fts)")
	s3 := s3Flags{}
	fs.StringVar(&s3.endpoint, "s3-endpoint", "", "S3/MinIO endpoint (enables S3 blob)")
	fs.StringVar(&s3.accessKey, "s3-access-key", "", "S3 access key")
	fs.StringVar(&s3.secretKey, "s3-secret-key", "", "S3 secret key")
	fs.StringVar(&s3.bucket, "s3-bucket", "", "S3 bucket")
	fs.BoolVar(&s3.useSSL, "s3-use-ssl", false, "use SSL for S3")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := config.Config{
		Storage: s3.storageConfig(*backend, *rocksPath),
	}
	if *backend == "maildir" {
		cfg.Storage.MaildirPath = *maildirPath
	}
	if *backend == "pebble" || *backend == "rocksdb" {
		if *rocksPath == "" {
			fmt.Fprintln(os.Stderr, "reindex: --rocks-path is required for pebble/rocksdb")
			return 2
		}
	}
	if *backend == "maildir" && *maildirPath == "" {
		fmt.Fprintln(os.Stderr, "reindex: --maildir-path is required for maildir")
		return 2
	}
	if *ftsPath == "" {
		*ftsPath = *rocksPath + ".fts"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	st, err := app.NewStorage(cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: open storage: %v\n", err)
		return 2
	}
	defer func() { _ = st.Close() }()

	// Rebuild from scratch: drop any existing index.
	if err := os.RemoveAll(*ftsPath); err != nil {
		fmt.Fprintf(os.Stderr, "reindex: clear index: %v\n", err)
		return 2
	}
	ix, err := fts.Open(*ftsPath, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: open index: %v\n", err)
		return 2
	}
	defer func() { _ = ix.Close() }()

	ctx := context.Background()
	accounts, err := st.Facade().ListAccounts(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: list accounts: %v\n", err)
		return 2
	}
	total := 0
	for _, account := range accounts {
		boxes, err := st.Mailbox().ListMailboxes(ctx, account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reindex: list mailboxes %s: %v\n", account, err)
			return 2
		}
		for _, box := range boxes {
			msgs, err := st.Mailbox().ListMessages(ctx, account, box.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reindex: list %s/%s: %v\n", account, box.Name, err)
				return 2
			}
			for _, msg := range msgs {
				rc, err := st.Mailbox().OpenMessage(ctx, account, box.Name, msg.UID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "reindex: open %s/%s/%d: %v\n", account, box.Name, msg.UID, err)
					return 2
				}
				data, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil {
					fmt.Fprintf(os.Stderr, "reindex: read %s/%s/%d: %v\n", account, box.Name, msg.UID, err)
					return 2
				}
				if err := ix.IndexMessage(ctx, account, box.Name, msg.UID, data); err != nil {
					fmt.Fprintf(os.Stderr, "reindex: index %s/%s/%d: %v\n", account, box.Name, msg.UID, err)
					return 2
				}
				total++
			}
		}
	}
	fmt.Printf("reindexed %d messages from %d accounts into %s\n", total, len(accounts), *ftsPath)
	return 0
}
