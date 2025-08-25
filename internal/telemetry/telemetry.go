// Package telemetry provides structured logging. It depends only on the
// standard library so it can sit at the bottom of the dependency graph
// (ARCHITECTURE.md §1.1, layer L0).
package telemetry

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger from a level name and a format.
// Level: debug|info|warn|error (default info). Format: text|json (default text).
func NewLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
