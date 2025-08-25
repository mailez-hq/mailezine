//go:build unix

package app

import (
	"log/slog"

	"mailezine/internal/config"
	"mailezine/internal/mailstore"
)

// openMailbox returns the native maildir mailbox store on POSIX systems.
// For the RocksDB/Pebble backends it returns nil (KV mailbox is used).
func openMailbox(cfg config.Config, _ *slog.Logger) (mailboxBackend, error) {
	if cfg.Storage.Backend == "maildir" {
		return mailstore.NewMaildir(cfg.Storage.MaildirPath), nil
	}
	return nil, nil
}
