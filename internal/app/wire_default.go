// Default wiring for the optional subsystem seams. Every hook either
// reports the subsystem as unavailable or degrades with a loud warning;
// nothing here links add-on implementation packages.
package app

import (
	"context"
	"errors"
	"log/slog"

	"mailezine/internal/config"
	"mailezine/internal/rspamd"
)

var errHAUnavailable = errors.New("ha: unavailable in this build")

// haAvailable reports whether this build can run HA terms.
func haAvailable() bool { return false }

// bootstrapHA is unreachable: New downgrades cfg.HA.Enabled before any
// HA bootstrap can run. The stub exists so shared lifecycle code links.
func (a *App) bootstrapHA() error {
	return errHAUnavailable
}

// superviseTerms is unreachable; see bootstrapHA.
func (a *App) superviseTerms(ctx context.Context) error {
	return errHAUnavailable
}

// newSpamClassifier: rspamd inbound scanning works in this build, but
// classifier learning does not. The client is built with empty learning
// endpoints, which turns Learn/Fuzzy calls into no-ops.
func newSpamClassifier(cfg config.Config, logger *slog.Logger) spamClassifier {
	if cfg.Rspamd.LearnURL != "" || cfg.Rspamd.Password != "" {
		logger.Warn("rspamd: classifier learning is not available in this build; scanning without learning")
	}
	return rspamd.New(cfg.Rspamd.URL, "", "", cfg.Hostname, logger)
}

// wireArchive: compliance capture is not part of this build.
func (a *App) wireArchive(runCtx context.Context) {
	if a.cfg.Archive.Enabled {
		a.logger.Warn("archive: compliance capture is not available in this build")
	}
}

// startupChecks runs build-specific pre-flight validation. Nothing to
// check in this build.
func (a *App) startupChecks() error { return nil }

// extraStatus returns build-specific entries merged into the management
// status payload. This build adds nothing.
func extraStatus() map[string]any { return nil }
