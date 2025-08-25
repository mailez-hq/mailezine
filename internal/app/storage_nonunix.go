//go:build !unix

package app

import (
	"log/slog"

	"mailezine/internal/config"
)

// openMailbox on non-POSIX platforms falls back to the KV+FS mailbox: the
// maildir layout relies on ':' in filenames, which is unsafe on Windows
// (DECISIONS.md D7).
func openMailbox(cfg config.Config, logger *slog.Logger) (mailboxBackend, error) {
	if cfg.Storage.Backend == "maildir" {
		logger.Warn("storage: maildir is POSIX-only; using KV+FS (Pebble)", "path", cfg.Storage.MaildirPath)
	}
	return nil, nil
}
