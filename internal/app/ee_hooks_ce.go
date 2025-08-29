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

// newSpamClassifier: ML spam scoring (rspamd) ships in the enterprise
// edition; the community build scans nothing (delivery fails open).
func newSpamClassifier(cfg config.Config, logger *slog.Logger) spamClassifier {
	logger.Warn("rspamd: classifier requires the enterprise edition; inbound scanning disabled")
	return nil
}

// wireArchive: compliance capture ships in the enterprise edition.
func (a *App) wireArchive(runCtx context.Context) {
	if a.cfg.Archive.Enabled {
		a.logger.Warn("archive: compliance capture requires the enterprise edition")
	}
}
