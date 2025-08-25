// Package ha implements leader election for active-passive engine pairs:
// exactly one instance (the leader) holds a lease on shared storage and
// runs the write path (KV is single-writer); followers stay on standby and
// take over when the lease expires. Lease semantics follow etcd-style TTL:
// the holder must renew before expiry, otherwise any instance may claim it.
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

// Lease is the shared leadership record.
type Lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Store persists the lease. Implementations must be atomic: concurrent
// TryAcquire from two instances yields exactly one winner.
type Store interface {
	// TryAcquire claims the lease if it is free or expired, returning
	// ErrNotLeader when another holder's lease is still valid.
	TryAcquire(ctx context.Context, owner string, ttl time.Duration) error
	// Renew extends the lease; ErrNotLeader when we no longer hold it.
	Renew(ctx context.Context, owner string, ttl time.Duration) error
	// Release drops the lease (only when owned by owner).
	Release(ctx context.Context, owner string) error
}

// FSStore keeps the lease in a file on shared storage (a volume mounted by
// every instance).
type FSStore struct {
	path string
}

// NewFSStore opens a lease store rooted at a shared file.
func NewFSStore(path string) *FSStore {
	return &FSStore{path: path}
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

// TryAcquire claims the lease atomically (write-then-rename wins races).
func (s *FSStore) TryAcquire(_ context.Context, owner string, ttl time.Duration) error {
	cur, err := s.read()
	if err != nil {
		return err
	}
	if cur != nil && cur.Owner != owner && time.Now().Before(cur.ExpiresAt) {
		return fmt.Errorf("%w: held by %s until %s", ErrNotLeader, cur.Owner, cur.ExpiresAt)
	}
	return s.write(&Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl)})
}

// Renew extends the lease when we hold it.
func (s *FSStore) Renew(_ context.Context, owner string, ttl time.Duration) error {
	cur, err := s.read()
	if err != nil {
		return err
	}
	if cur == nil || cur.Owner != owner {
		return ErrNotLeader
	}
	return s.write(&Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl)})
}

// Release drops the lease when owned by owner.
func (s *FSStore) Release(_ context.Context, owner string) error {
	cur, err := s.read()
	if err != nil {
		return err
	}
	if cur == nil || cur.Owner != owner {
		return nil
	}
	return os.Remove(s.path)
}

// S3Store keeps the lease as an object in a shared bucket (MinIO/cloud
// S3). MinIO objects are strongly consistent, so the write-then-read race
// resolves to one winner per TTL.
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

func (s *S3Store) read(ctx context.Context) (*Lease, error) {
	obj, err := s.mc.GetObject(ctx, s.bucket, s.key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		return nil, nil // not found
	}
	var l Lease
	if err := json.NewDecoder(obj).Decode(&l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *S3Store) write(ctx context.Context, l *Lease) error {
	raw, _ := json.Marshal(l)
	_, err := s.mc.PutObject(ctx, s.bucket, s.key, bytes.NewReader(raw), int64(len(raw)), minio.PutObjectOptions{})
	return err
}

// TryAcquire claims the lease when free or expired.
func (s *S3Store) TryAcquire(ctx context.Context, owner string, ttl time.Duration) error {
	cur, err := s.read(ctx)
	if err != nil {
		return err
	}
	if cur != nil && cur.Owner != owner && time.Now().Before(cur.ExpiresAt) {
		return fmt.Errorf("%w: held by %s until %s", ErrNotLeader, cur.Owner, cur.ExpiresAt)
	}
	return s.write(ctx, &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl)})
}

// Renew extends the lease when we hold it.
func (s *S3Store) Renew(ctx context.Context, owner string, ttl time.Duration) error {
	cur, err := s.read(ctx)
	if err != nil {
		return err
	}
	if cur == nil || cur.Owner != owner {
		return ErrNotLeader
	}
	return s.write(ctx, &Lease{Owner: owner, ExpiresAt: time.Now().Add(ttl)})
}

// Release drops the lease when owned by owner.
func (s *S3Store) Release(ctx context.Context, owner string) error {
	cur, err := s.read(ctx)
	if err != nil {
		return err
	}
	if cur == nil || cur.Owner != owner {
		return nil
	}
	return s.mc.RemoveObject(ctx, s.bucket, s.key, minio.RemoveObjectOptions{})
}

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

// TryAcquire attempts to become leader.
func (l *Leader) TryAcquire(ctx context.Context) error {
	return l.store.TryAcquire(ctx, l.owner, l.ttl)
}

// Run renews the lease until ctx is cancelled; it must only be called after
// TryAcquire succeeded.
func (l *Leader) Run(ctx context.Context) {
	ticker := time.NewTicker(l.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.store.Renew(ctx, l.owner, l.ttl); err != nil {
				l.logger.Error("ha: lease renew", "err", err)
			}
		}
	}
}

// Release drops the lease.
func (l *Leader) Release(ctx context.Context) error {
	return l.store.Release(ctx, l.owner)
}
