// Package accountgate serializes per-account writers ACROSS nodes in
// multi-active deployments.
//
// Correctness never depends on it: mailbox writes are transactional and
// replay on conflict, so a gate miss only costs tail latency. The gate
// exists to remove that cost under contention (a flood of inbound
// deliveries to one account, round-robined by the LB onto every node):
// the node that first touches an account takes a cheap KV lease on it,
// other nodes' writers WAIT briefly for the lease instead of colliding on
// the account's hot keys (UID counter, modseq, quota, changelog).
//
// Semantics, in one breath: advisory (bounded wait, then proceed anyway —
// mail is never delayed or dropped by the gate), lazy (leases are taken
// only for accounts with traffic), held-while-hot (renewed in the
// background, released after an idle window), and node-scoped (a lease is
// shared by all local goroutines; intra-node serialization stays with the
// Store's account locks).
package accountgate

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mailezine/internal/kvlease"
	"mailezine/internal/metrics"
	"mailezine/internal/store"
)

// Options tunes the gate. Zero values fall back to DefaultOptions.
type Options struct {
	// TTL is the lease lifetime (default 30s); renewals run at TTL/3.
	TTL time.Duration
	// IdleRelease drops a held account after this window without use
	// (default 2×TTL): hot accounts stay pinned, quiet ones cost nothing.
	IdleRelease time.Duration
	// Wait bounds how long a foreign holder blocks a writer before the
	// gate falls back to proceeding without the lease (default 5s). It
	// must stay well under the SMTP DATA deadline.
	Wait time.Duration
}

// DefaultOptions returns the production tuning.
func DefaultOptions() Options {
	return Options{TTL: 30 * time.Second, IdleRelease: 60 * time.Second, Wait: 5 * time.Second}
}

type heldAccount struct {
	lease    *kvlease.Lease
	lastUsed time.Time
}

// Gate is the per-node account write serializer. Safe for concurrent use.
type Gate struct {
	kv     store.KV
	txn    store.TxnKV
	nodeID string
	opts   Options
	logger *slog.Logger
	mtr    *metrics.Metrics // optional

	mu   sync.Mutex
	held map[string]*heldAccount
}

// New builds a gate. Call Run to maintain (renew/release) held leases;
// without it leases still work but expire after one TTL and rely on
// re-acquisition.
func New(kv store.KV, nodeID string, opts Options, logger *slog.Logger) *Gate {
	// Per-field fallback: a caller tuning only the Wait budget must not
	// silently reset TTL/IdleRelease to defaults.
	def := DefaultOptions()
	if opts.TTL <= 0 {
		opts.TTL = def.TTL
	}
	if opts.IdleRelease <= 0 {
		opts.IdleRelease = def.IdleRelease
	}
	if opts.Wait <= 0 {
		opts.Wait = def.Wait
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{
		kv:     kv,
		txn:    store.AsTxn(kv),
		nodeID: nodeID,
		opts:   opts,
		logger: logger,
		held:   map[string]*heldAccount{},
	}
}

// SetMetrics attaches the optional health instrumentation.
func (g *Gate) SetMetrics(m *metrics.Metrics) { g.mtr = m }

// leaseFor builds the per-account lease record.
func (g *Gate) leaseFor(account string) *kvlease.Lease {
	return kvlease.New(g.kv, "acct-write:"+account, g.nodeID, g.opts.TTL)
}

// WithAccount runs fn under this node's ownership of the account's write
// path: it returns once the lease is held locally, after a bounded wait on
// a foreign holder — or, on timeout/storage error, WITHOUT the lease
// (advisory: the transactional store keeps the write correct either way).
func (g *Gate) WithAccount(ctx context.Context, account string, fn func() error) error {
	if !g.ensure(ctx, account) {
		g.mtr.AccountGateFallback()
		g.logger.Warn("accountgate: proceeding without lease (advisory)", "account", account)
		return fn()
	}
	err := fn()
	g.touch(account)
	return err
}

// ensure acquires or reuses the account lease; false when a foreign holder
// outlasted the wait budget or storage hiccuped.
func (g *Gate) ensure(ctx context.Context, account string) bool {
	g.mu.Lock()
	if h, ok := g.held[account]; ok {
		h.lastUsed = time.Now()
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()

	lease := g.leaseFor(account)
	waited := false
	deadline := time.Now().Add(g.opts.Wait)
	for {
		ok, err := lease.Acquire(ctx)
		if err != nil {
			// Storage trouble: never block mail on the optimizer.
			return false
		}
		if ok {
			if waited {
				g.mtr.AccountGateWait()
			}
			g.mu.Lock()
			g.held[account] = &heldAccount{lease: lease, lastUsed: time.Now()}
			g.mu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			if waited {
				g.mtr.AccountGateWait()
			}
			return false
		}
		waited = true
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// touch refreshes the account's idle clock after a completed write.
func (g *Gate) touch(account string) {
	g.mu.Lock()
	if h, ok := g.held[account]; ok {
		h.lastUsed = time.Now()
	}
	g.mu.Unlock()
}

// Held reports how many accounts this node currently owns (metrics gauge).
func (g *Gate) Held() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}

// Run maintains held leases until ctx is cancelled: every TTL/3 it renews
// accounts still inside their idle window (a failed renewal means someone
// stole an expired lease — the account is simply re-acquired on next use)
// and releases the quiet ones so ownership follows traffic.
func (g *Gate) Run(ctx context.Context) {
	tick := g.opts.TTL / 3
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// The loop ctx is already cancelled here; releases need a
			// fresh budget or every lease Release would fail on
			// context.Canceled and ownership would linger to TTL.
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			g.releaseAll(rctx)
			cancel()
			return
		case <-t.C:
		}
		g.sweep()
	}
}

func (g *Gate) sweep() {
	now := time.Now()
	g.mu.Lock()
	var todo []maintenance
	for account, h := range g.held {
		todo = append(todo, maintenance{account, h.lease, now.Sub(h.lastUsed) > g.opts.IdleRelease})
	}
	g.mu.Unlock()

	// Renew and release with a small worker pool: a serial sweep with a
	// 5 s per-account budget lets a handful of slow accounts push the tail
	// of the list past its TTL, systematically losing ownership.
	const workers = 8
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, a := range todo {
		sem <- struct{}{}
		wg.Add(1)
		go func(a maintenance) {
			defer func() { <-sem; wg.Done() }()
			g.maintain(a)
		}(a)
	}
	wg.Wait()
}

// maintenance is one sweep action for a held account.
type maintenance struct {
	account string
	lease   *kvlease.Lease
	release bool // true = idle, release it; false = renew it
}

// maintain renews or releases one held account.
func (g *Gate) maintain(a maintenance) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.release {
		_ = a.lease.Release(ctx)
		g.drop(a.account, a.lease)
		return
	}
	ok, err := a.lease.Acquire(ctx)
	if err != nil {
		// Storage trouble: conservatively drop local ownership. Keeping
		// the entry would let the fast path admit writers with no lease
		// held (and no signal that the gate stopped gating).
		g.logger.Warn("accountgate: renewal failed; dropping ownership", "account", a.account, "err", err)
		g.drop(a.account, a.lease)
		return
	}
	if !ok {
		// Stolen after expiry: drop local ownership; the next writer
		// re-acquires (and the current one already proceeded under
		// transactional correctness).
		g.drop(a.account, a.lease)
	}
}

func (g *Gate) drop(account string, lease *kvlease.Lease) {
	g.mu.Lock()
	if h, ok := g.held[account]; ok && h.lease == lease {
		delete(g.held, account)
	}
	g.mu.Unlock()
}

func (g *Gate) releaseAll(ctx context.Context) {
	g.mu.Lock()
	held := make([]*heldAccount, 0, len(g.held))
	for _, h := range g.held {
		held = append(held, h)
	}
	g.held = map[string]*heldAccount{}
	g.mu.Unlock()
	for _, h := range held {
		_ = h.lease.Release(ctx)
	}
}
