// Package kvlease implements distributed singleton election over the KV
// store: named leases with owner + expiry, renewed on every use, stolen
// after expiry. Multi-active engine deployments use it to run exactly-once
// background workers (the snooze sweeper, future blob GC) across N nodes
// pointing at the same transactional storage.
//
// The fencing primitive is the TxnKV read-modify-write: on TiDB the
// database serializes conflicting commits (with replay), on the in-memory
// backend the write lock serializes everything. Backends without native
// transactions (Pebble via the AsTxn buffer) only get process-local
// correctness — which is fine, because those backends are single-process
// by construction.
package kvlease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"mailezine/internal/store"
)

// record is the durable lease state stored under MetaLeaseKey.
type record struct {
	Owner string    `json:"owner"`
	Until time.Time `json:"until"`
}

// Lease is a named mutex in the KV store, safe for concurrent use by many
// goroutines and many processes.
type Lease struct {
	txn    store.TxnKV
	key    []byte
	owner  string
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger
}

// New builds a lease. owner must uniquely identify this process within the
// deployment (RandomNodeID is the default choice); ttl is how long a
// holder survives without re-acquiring.
func New(kv store.KV, name, owner string, ttl time.Duration) *Lease {
	return &Lease{
		txn:    store.AsTxn(kv),
		key:    store.MetaLeaseKey(name),
		owner:  owner,
		ttl:    ttl,
		now:    time.Now,
		logger: slog.Default(),
	}
}

// SetClock overrides time.Now (deterministic tests).
func (l *Lease) SetClock(now func() time.Time) { l.now = now }

// SetLogger overrides the default logger.
func (l *Lease) SetLogger(log *slog.Logger) { l.logger = log }

// Acquire claims the lease when free or expired, and renews it when this
// process already holds it. It returns false when a foreign holder is
// still live. Acquire doubles as Renew: callers need not track which of
// the two applies.
func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	held := false
	err := l.txn.WithTxn(ctx, func(t store.TxnOps) error {
		held = false
		now := l.now()
		if v, err := t.Get(l.key); err == nil {
			var cur record
			if jerr := json.Unmarshal(v, &cur); jerr != nil {
				return jerr
			}
			if cur.Owner != l.owner && cur.Until.After(now) {
				return nil // live foreign holder
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		data, err := json.Marshal(record{Owner: l.owner, Until: now.Add(l.ttl)})
		if err != nil {
			return err
		}
		t.Put(l.key, data)
		held = true
		return nil
	})
	return held, err
}

// Release drops the lease when this process holds it; a foreign holder's
// record is never touched.
func (l *Lease) Release(ctx context.Context) error {
	return l.txn.WithTxn(ctx, func(t store.TxnOps) error {
		v, err := t.Get(l.key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var cur record
		if err := json.Unmarshal(v, &cur); err != nil {
			return err
		}
		if cur.Owner != l.owner {
			return nil
		}
		t.Delete(l.key)
		return nil
	})
}

// Run acquires (or renews) the lease on every tick and invokes work only
// while this process is the holder, so exactly one node in the deployment
// runs the protected worker at a time. work runs synchronously inside the
// tick; a pass that outlives the TTL lets the lease expire and a successor
// start concurrently — the usual at-least-once trade-off. Ticks continue
// after ctx is cancelled only long enough to release cleanly.
func (l *Lease) Run(ctx context.Context, every time.Duration, work func(ctx context.Context)) {
	if every <= 0 {
		every = l.ttl / 3
		if every <= 0 {
			every = time.Second
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = l.Release(releaseCtx)
			cancel()
			return
		case <-t.C:
		}
		held, err := l.Acquire(ctx)
		if err != nil {
			// Transient storage error: skip this tick without stopping —
			// the lease will expire on its own if the outage persists.
			l.logger.Warn("kvlease: acquire", "key", string(l.key), "err", err)
			continue
		}
		if !held {
			continue
		}
		work(ctx)
	}
}

// RandomNodeID derives a collision-safe per-process identity for lease
// ownership. Leases are transient, so uniqueness — not stability across
// restarts — is the requirement.
func RandomNodeID() string {
	host, _ := os.Hostname()
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
