// Cached wraps a Service with a short-TTL memo of successful authentications.
//
// Only webmail session tokens (token-*) are memoised: the control plane
// re-presents one on every pooled IMAP/SMTP dial, which is the round trip
// this exists to absorb. Passwords and app tokens always reach the control
// plane, so one protocol's success cannot authorize another and policy
// changes apply at the next login.
//
// The key is derived from the credential pair (email + password hash), so a
// cache hit only ever replays the exact credentials that already succeeded —
// a wrong password can never ride a prior success. Failures are never
// cached (brute-force protection).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"mailezine/internal/mailcache"
)

// sessionTokenPrefix is the control plane's webmail session credential
// (Manager.CreateTempToken).
const sessionTokenPrefix = "token-"

// Cached is a Service decorator with positive-result caching.
type Cached struct {
	inner Service
	cache *mailcache.Cache
}

// NewCached wraps inner. cache must be non-nil.
func NewCached(inner Service, cache *mailcache.Cache) *Cached {
	return &Cached{inner: inner, cache: cache}
}

func (c *Cached) Authenticate(ctx context.Context, email, password string, opts Options) (bool, error) {
	if !strings.HasPrefix(password, sessionTokenPrefix) {
		return c.inner.Authenticate(ctx, email, password, opts)
	}
	key := credentialKey(email, password)
	if v, ok := c.cache.Get(key); ok && v.(bool) {
		return true, nil
	}
	ok, err := c.inner.Authenticate(ctx, email, password, opts)
	if err == nil && ok {
		c.cache.Put(key, true, 1)
	}
	return ok, err
}

func (c *Cached) Close() error { return c.inner.Close() }

// credentialKey derives a cache key from the credential pair. The plaintext
// password is never stored; only its SHA-256 digest participates.
func credentialKey(email, password string) string {
	sum := sha256.Sum256([]byte(email + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

var _ Service = (*Cached)(nil)
