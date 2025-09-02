package mailstore

import (
	"context"
	"log/slog"
	"time"

	"mailezine/internal/kvlease"
)

// Blob GC defaults: the sweep runs on this interval and only reclaims blobs
// whose GC claim is at least the grace period old. The grace window absorbs
// a delivery that re-creates a previously-claimed content blob between its
// file write and its link commit; Deliver additionally re-checks the file
// after its commit, so the window is belt-and-braces, not load-bearing.
const (
	defaultBlobGCInterval = 10 * time.Minute
	defaultBlobGCGrace    = 10 * time.Minute
)

// RunBlobGC sweeps unreferenced blobs every interval until ctx is cancelled.
// The lease makes exactly one node sweep at a time in multi-active
// deployments (pass nil in single-process embeds). It is the janitor half of
// INV-BLOB: deletes stage GC claims, the sweep reclaims — inline reclamation
// would race a concurrent delivery of the same content-addressed blob and
// could delete another account's shared copy.
func (k *KV) RunBlobGC(ctx context.Context, lease *kvlease.Lease, interval, grace time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = defaultBlobGCInterval
	}
	if grace <= 0 {
		grace = defaultBlobGCGrace
	}
	if lease != nil {
		lease.Run(ctx, interval, func(ctx context.Context) { k.sweepOnce(ctx, grace, logger) })
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.sweepOnce(ctx, grace, logger)
		}
	}
}

func (k *KV) sweepOnce(ctx context.Context, grace time.Duration, logger *slog.Logger) {
	n, err := k.s.SweepBlobs(ctx, grace, time.Now(), k.s.DeleteBlob)
	if err != nil {
		if ctx.Err() == nil && logger != nil {
			logger.Warn("blob gc: sweep", "err", err)
		}
		return
	}
	if n > 0 && logger != nil {
		logger.Info("blob gc: reclaimed", "blobs", n)
	}
}
