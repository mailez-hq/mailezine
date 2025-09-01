// Community-edition wiring for the enterprise seams: every hook is a
// loud no-op. Features degrade with a warning instead of wedging startup,
// and nothing here links any enterprise package (the public CE tree has
// none to link).
package app

import (
	"context"
	"errors"
	"log/slog"

	"mailezine/internal/config"
	"mailezine/internal/rspamd"
)

// errHAUnavailable pairs with the enterprise-side error of the same name.
var errHAUnavailable = errors.New("ha: unavailable in this build")

// haAvailable reports whether this build can run HA terms.
func haAvailable() bool { return false }

// bootstrapHA is unreachable in the community build (New downgrades
// cfg.HA.Enabled first); the stub exists so shared lifecycle code links.
func (a *App) bootstrapHA() error {
	return errHAUnavailable
}

// superviseTerms is unreachable in the community build; see bootstrapHA.
func (a *App) superviseTerms(ctx context.Context) error {
	return errHAUnavailable
}

// newSpamClassifier: rspamd inbound scanning ships in both editions, but
// supervised learning is enterprise-only: the community client is built
// with empty learning endpoints, which turns Learn/Fuzzy calls into no-ops.
func newSpamClassifier(cfg config.Config, logger *slog.Logger) spamClassifier {
	if cfg.Rspamd.LearnURL != "" || cfg.Rspamd.Password != "" {
		logger.Warn("rspamd: classifier learning requires the enterprise edition; scanning without learning")
	}
	return rspamd.New(cfg.Rspamd.URL, "", "", cfg.Hostname, logger)
}

// wireArchive: compliance capture ships in the enterprise edition.
func (a *App) wireArchive(runCtx context.Context) {
	if a.cfg.Archive.Enabled {
		a.logger.Warn("archive: compliance capture requires the enterprise edition")
	}
}
