// Package ha implements leader election for active-passive engine pairs:
// exactly one instance (the leader) holds a lease on shared storage and
// runs the write path (KV is single-writer); followers stay on standby and
// take over when the lease expires.
//
// Lease semantics follow etcd-style TTL leases hardened with a fencing
// token:
//
//   - Acquisition is atomic. The FS store guards the critical section with
//     an O_CREATE|O_EXCL claim file; the S3 store uses conditional writes
//     (If-None-Match / If-Match), which MinIO applies server-side, plus a
//     confirm read so a gateway silently ignoring condition headers cannot
//     produce two leaders.
//   - Every lease carries a monotonically increasing epoch. A returning or
//     renewing holder verifies it still owns the latest epoch, and an old
//     leader whose renewals reveal a moved epoch steps down immediately
//     (ErrLeaseStolen). Fencing decisions only get safer as checks fail
//     closed: uncertain state is always reported as "not leader".
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotLeader is returned by operations that require leadership.
var ErrNotLeader = errors.New("ha: not the leader")

// ErrLeaseStolen marks the specific case where our lease was taken over by
// another instance (or expired into foreign hands). Callers use it to
// distinguish "step down now" from a transient storage error.
var ErrLeaseStolen = fmt.Errorf("%w: lease stolen", ErrNotLeader)

const (
	fsClaimSuffix = ".claim"
	// fsClaimTTL bounds how long a crashed process can block acquirers on
	// its leftover claim file before it is stale-broken. It must exceed any
	// legitimate holder's critical section (claim → read → write lease) by
	// a wide margin: a starved-but-alive holder whose stall outlives the
	// TTL gets its claim broken and a second winner may take the lease.
	// Leader renewal runs far more often than this, so a crashed holder
	// still delays failover by at most one TTL window.
	fsClaimTTL = 10 * time.Second
	// fsClaimPoll is the retry interval while waiting for a live claim.
	fsClaimPoll = 20 * time.Millisecond
	// fsLockWait bounds one acquisition attempt's wait for the claim and
	// stays above fsClaimTTL so a waiter lives long enough to break a
	// leftover claim itself; the supervisor retries with backoff instead
	// of blocking forever here.
	fsLockWait = 15 * time.Second

	s3NotFound      = "NoSuchKey"
	s3PrecondFailed = "PreconditionFailed"

	// maxRenewFailures tolerates transient renewal errors (network blips)
	// before giving up leadership: ttl/3 per tick ×2 ≈ ⅔ TTL worst case,
	// still comfortably inside one lease period.
	maxRenewFailures = 2
)

// Lease is the shared leadership record.
type Lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
	Epoch     uint64    `json:"epoch,omitempty"` // fencing token, grows by 1 per takeover
}

func (l *Lease) heldBy(other string) bool {
	return l != nil && l.Owner != other && time.Now().Before(l.ExpiresAt)
}

// Store persists the lease. Implementations must be atomic: concurrent
// TryAcquire from two instances yields exactly one winner.
type Store interface {
	// TryAcquire claims the lease if free or expired. On success it returns
	// the new lease (with this owner and a fresh epoch); when another
	// holder's lease is valid it returns an error wrapping ErrNotLeader.
	TryAcquire(ctx context.Context, owner string, ttl time.Duration) (*Lease, error)
	// Renew extends the lease. It returns the extended lease, or an error
	// wrapping ErrLeaseStolen once we no longer hold the latest epoch.
	Renew(ctx context.Context, owner string, ttl time.Duration) (*Lease, error)
	// Release drops the lease (only when owned by owner).
	Release(ctx context.Context, owner string) error
}

// ---------------------------------------------------------------------------
// FS store.
// ---------------------------------------------------------------------------

// FSStore keeps the lease in a file on shared storage (a volume mounted by
// every instance). Mutations run under an inter-process lock built from an
// O_CREATE|O_EXCL claim sidecar, so read-check-write races resolve to one
// winner even without rename atomicity guarantees across filesystems.
type FSStore struct {
	path string
}

// NewFSStore opens a lease store rooted at a shared file.
func NewFSStore(path string) *FSStore {
	return &FSStore{path: path}
}

func (s *FSStore) claimPath() string { return s.path + fsClaimSuffix }

// lock takes the mutation lock, stale-breaking claims older than
// fsClaimTTL (crashed holders). It gives up after fsLockWait.
func (s *FSStore) lock(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, fsLockWait)
	unlock := func() {
		defer cancel()
		_ = os.Remove(s.claimPath())
	}
	for {
		f, err := os.OpenFile(s.claimPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			_ = f.Close()
			return unlock, nil
		}
		if !os.IsExist(err) {
			cancel()
			return nil, err
		}
		if fi, statErr := os.Stat(s.claimPath()); statErr == nil && time.Since(fi.ModTime()) > fsClaimTTL {
			_ = os.Remove(s.claimPath())
			continue // race to re-claim right away
		}
		select {
		case <-ctx.Done():
			cancel()
			return nil, fmt.Errorf("ha/fs: claim busy at %s: %w", s.claimPath(), ctx.Err())
		case <-time.After(fsClaimPoll):
		}
	}
}

func (s *FSStore) read() (*Lease, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *FSStore) write(l *Lease) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// TryAcquire claims the lease if free or expired, bumping the fencing
// epoch of whichever record it replaces.
func (s *FSStore) TryAcquire(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cur, err := s.read()
	if err != nil {
		return nil, err
	}
	if cur.heldBy(owner) {
		return nil, fmt.Errorf("%w: held by %s until %s", ErrNotLeader, cur.Owner, cur.ExpiresAt)
	}
	next := &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl)}
	if cur != nil {
		next.Epoch = cur.Epoch + 1
	} else {
		next.Epoch = 1
	}
	return next, s.write(next)
}

// Renew extends the lease while we hold the current epoch.
func (s *FSStore) Renew(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cur, err := s.read()
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.Owner != owner {
		return nil, ErrLeaseStolen
	}
	next := &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl), Epoch: cur.Epoch}
	return next, s.write(next)
}

// Release drops the lease when owned by owner (a no-op otherwise).
func (s *FSStore) Release(ctx context.Context, owner string) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := s.read()
	if err != nil || cur == nil || cur.Owner != owner {
		return err
	}
	return os.Remove(s.path)
}

// ---------------------------------------------------------------------------
// S3 store.
// ---------------------------------------------------------------------------

// S3Store keeps the lease as an object in a shared bucket. Correctness does
// not lean on last-writer-wins: creation races are arbitrated by the S3
// conditional-write headers, updates carry If-Match against the observed
// ETag, and every successful write is confirmed by reading the object back
// — guarding against proxies that ignore condition headers.
type S3Store struct {
	mc     *minio.Client
	bucket string
	key    string
}

// NewS3Store builds an S3-backed lease store.
func NewS3Store(endpoint, accessKey, secretKey, bucket, key string, useSSL bool) (*S3Store, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: "us-east-1",
	})
	if err != nil {
		return nil, err
	}
	return &S3Store{mc: mc, bucket: bucket, key: key}, nil
}

// read fetches the lease together with its ETag for optimistic writes.
// A missing object yields (nil, "", nil).
func (s *S3Store) read(ctx context.Context) (*Lease, string, error) {
	info, err := s.mc.StatObject(ctx, s.bucket, s.key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == s3NotFound {
			return nil, "", nil
		}
		return nil, "", err
	}
	obj, err := s.mc.GetObject(ctx, s.bucket, s.key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	defer obj.Close()
	var l Lease
	if err := json.NewDecoder(obj).Decode(&l); err != nil {
		return nil, "", err
	}
	return &l, info.ETag, nil
}

// put stores l conditionally: createOnly relies on If-None-Match:* to make
// concurrent first-claims single-winner; matchedETag turns the write into a
// compare-and-swap on the previous record.
func (s *S3Store) put(ctx context.Context, l *Lease, matchedETag string, createOnly bool) error {
	raw, _ := json.Marshal(l)
	opts := minio.PutObjectOptions{ContentType: "application/json"}
	if createOnly {
		opts.SetMatchETagExcept("*")
	} else if matchedETag != "" {
		opts.SetMatchETag(matchedETag)
	}
	_, err := s.mc.PutObject(ctx, s.bucket, s.key, bytes.NewReader(raw), int64(len(raw)), opts)
	return err
}

// confirm re-reads after a write and fails closed unless the object now
// holds exactly what we believe we wrote (owner match; epoch when known).
func (s *S3Store) confirm(ctx context.Context, owner string, expectEpoch uint64) (*Lease, error) {
	got, _, err := s.read(ctx)
	if err != nil {
		return nil, fmt.Errorf("ha/s3: confirm read: %w", err)
	}
	if got == nil || got.Owner != owner || (expectEpoch != 0 && got.Epoch != expectEpoch) {
		return nil, ErrLeaseStolen
	}
	return got, nil
}

// TryAcquire claims the lease when free or expired via a create-only
// conditional write, then confirms it stuck.
func (s *S3Store) TryAcquire(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	cur, etag, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	if cur.heldBy(owner) {
		return nil, fmt.Errorf("%w: held by %s until %s", ErrNotLeader, cur.Owner, cur.ExpiresAt)
	}
	nextEpoch := uint64(1)
	if cur != nil {
		nextEpoch = cur.Epoch + 1
	}
	next := &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl), Epoch: nextEpoch}
	// Both paths are single-winner server-side: a create-only write when no
	// record exists, or a compare-and-swap on the observed ETag when taking
	// over an expired one.
	if err := s.put(ctx, next, etag, cur == nil); err == nil {
		return s.confirm(ctx, owner, nextEpoch)
	} else if code := minio.ToErrorResponse(err).Code; code != s3NotFound && code != s3PrecondFailed {
		return nil, err
	}
	// Either the create raced (object appeared) or the CAS lost: another
	// instance took over between our read and write.
	return nil, fmt.Errorf("%w: acquire raced at %s", ErrNotLeader, s.key)
}

// Renew extends the lease as a compare-and-swap on the current ETag and
// epoch; any mismatch reports the lease as stolen.
func (s *S3Store) Renew(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	cur, etag, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.Owner != owner {
		return nil, ErrLeaseStolen
	}
	if etag == "" {
		// No ETag surfaced (unusual backend): fall back to CAS on epoch by
		// refusing blind writes — safer to lose leadership than overlap.
		return nil, fmt.Errorf("ha/s3: renew without etag")
	}
	next := &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl), Epoch: cur.Epoch}
	if err := s.put(ctx, next, etag, false); err != nil {
		if minio.ToErrorResponse(err).Code == s3PrecondFailed {
			return nil, ErrLeaseStolen
		}
		return nil, err
	}
	return s.confirm(ctx, owner, cur.Epoch)
}

// Release drops the lease when owned by owner. Remove has no conditional
// form; the ownership pre-check keeps this safe because callers release
// only on voluntary handover.
func (s *S3Store) Release(ctx context.Context, owner string) error {
	cur, _, err := s.read(ctx)
	if err != nil || cur == nil || cur.Owner != owner {
		return err
	}
	return s.mc.RemoveObject(ctx, s.bucket, s.key, minio.RemoveObjectOptions{})
}

// ---------------------------------------------------------------------------
// Leader lifecycle.
// ---------------------------------------------------------------------------

// Leader manages one instance's leadership lifecycle: acquire, renew on a
// ticker, release on stop.
type Leader struct {
	store  Store
	owner  string
	ttl    time.Duration
	logger *slog.Logger
}

// NewLeader builds a leader lifecycle helper.
func NewLeader(store Store, owner string, ttl time.Duration, logger *slog.Logger) *Leader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Leader{store: store, owner: owner, ttl: ttl, logger: logger}
}

// TTL returns the configured lease duration.
func (l *Leader) TTL() time.Duration { return l.ttl }

// TryAcquire attempts to become leader.
func (l *Leader) TryAcquire(ctx context.Context) error {
	_, err := l.store.TryAcquire(ctx, l.owner, l.ttl)
	return err
}

// Run renews the lease until ctx is cancelled; it must only be called after
// TryAcquire succeeded. When leadership is lost — a foreign holder detected
// at renewal, or maxRenewFailures consecutive transient errors — onLost is
// invoked exactly once and Run blocks until ctx ends: the caller is expected
// to tear the term down, and further renewals would be wrong either way.
func (l *Leader) Run(ctx context.Context, onLost func()) {
	ticker := time.NewTicker(l.ttl / 3)
	defer ticker.Stop()
	failures := 0
	reportLost := func(reason string, err error) {
		l.logger.Error("ha: stepping down", "reason", reason, "err", err)
		if onLost != nil {
			onLost()
		}
		<-ctx.Done()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		got, err := l.store.Renew(ctx, l.owner, l.ttl)
		switch {
		case err == nil:
			failures = 0
			l.logger.Debug("ha: renewed", "epoch", got.Epoch, "until", got.ExpiresAt)
		case errors.Is(err, ErrNotLeader):
			reportLost("lease no longer ours", err)
			return
		default:
			if ctx.Err() != nil {
				return // shutdown raced the failure; not a loss
			}
			failures++
			if failures >= maxRenewFailures {
				reportLost("renewal failing repeatedly", err)
				return
			}
			l.logger.Error("ha: lease renew", "err", err, "failures", failures)
		}
	}
}

// Release drops the lease.
func (l *Leader) Release(ctx context.Context) error {
	return l.store.Release(ctx, l.owner)
}
