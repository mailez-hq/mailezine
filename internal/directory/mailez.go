// Mailez implements Service against the mailez control plane
// (/stack/directory/*, see mailez/backend/internal/stack/directory.go).
// Responses are cached briefly so hot paths (SMTP recipient resolution) do
// not hammer the control plane; 404s are not cached (they are rare and the
// control plane answers them cheaply).
package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Mailez is a Service backed by the mailez directory contract.
type Mailez struct {
	base  string // e.g. http://backend:8080/stack/directory
	hc    *http.Client
	cache *ttlCache
}

// NewMailez returns a directory client for the mailez control plane.
func NewMailez(base string, cacheTTL time.Duration) *Mailez {
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Second
	}
	return &Mailez{
		base:  base,
		hc:    &http.Client{Timeout: 5 * time.Second},
		cache: newTTLCache(cacheTTL),
	}
}

func (m *Mailez) User(ctx context.Context, email string) (User, error) {
	if v, ok := m.cache.get("user:" + email); ok {
		return v.(User), nil
	}
	var u User
	if err := m.getJSON(ctx, "/users/"+url.PathEscape(email), &u); err != nil {
		return User{}, err
	}
	m.cache.put("user:"+email, u)
	return u, nil
}

func (m *Mailez) Domain(ctx context.Context, name string) (Domain, error) {
	if v, ok := m.cache.get("domain:" + name); ok {
		return v.(Domain), nil
	}
	var d Domain
	if err := m.getJSON(ctx, "/domains/"+url.PathEscape(name), &d); err != nil {
		return Domain{}, err
	}
	m.cache.put("domain:"+name, d)
	return d, nil
}

func (m *Mailez) Aliases(ctx context.Context, addr string) ([]string, error) {
	if v, ok := m.cache.get("alias:" + addr); ok {
		return append([]string(nil), v.([]string)...), nil
	}
	var out struct {
		Targets []string `json:"targets"`
	}
	if err := m.getJSON(ctx, "/aliases/"+url.PathEscape(addr), &out); err != nil {
		return nil, err
	}
	m.cache.put("alias:"+addr, out.Targets)
	return append([]string(nil), out.Targets...), nil
}

func (m *Mailez) Relay(ctx context.Context, email string) (Relay, error) {
	var r Relay
	if err := m.getJSON(ctx, "/relays/"+url.PathEscape(email), &r); err != nil {
		return Relay{}, err
	}
	return r, nil
}

func (m *Mailez) Sender(ctx context.Context, email string) (Sender, error) {
	var s Sender
	if err := m.getJSON(ctx, "/senders/"+url.PathEscape(email), &s); err != nil {
		return Sender{}, err
	}
	return s, nil
}

func (m *Mailez) SenderRate(ctx context.Context, email string) (SenderRate, error) {
	var r SenderRate
	if err := m.getJSON(ctx, "/senders/"+url.PathEscape(email)+"/rate", &r); err != nil {
		return SenderRate{}, err
	}
	return r, nil
}

func (m *Mailez) SRSForward(ctx context.Context, sender string) (string, error) {
	var out struct {
		Rewritten string `json:"rewritten"`
	}
	if err := m.getJSON(ctx, "/srs/"+url.PathEscape(sender), &out); err != nil {
		return "", err
	}
	return out.Rewritten, nil
}

func (m *Mailez) SRSRestore(ctx context.Context, recipient string) (string, error) {
	var out struct {
		Original string `json:"original"`
	}
	if err := m.getJSON(ctx, "/srs/restore/"+url.PathEscape(recipient), &out); err != nil {
		return "", err
	}
	return out.Original, nil
}

func (m *Mailez) Quota(ctx context.Context, email string) (Quota, error) {
	if v, ok := m.cache.get("quota:" + email); ok {
		return v.(Quota), nil
	}
	var q Quota
	if err := m.getJSON(ctx, "/quota/"+url.PathEscape(email), &q); err != nil {
		return Quota{}, err
	}
	m.cache.put("quota:"+email, q)
	return q, nil
}

func (m *Mailez) UpdateQuotaUsed(ctx context.Context, email string, used int64) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		m.base+"/quota/"+url.PathEscape(email),
		bytes.NewBufferString(strconv.FormatInt(used, 10)),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("directory: quota update %s: status %d", email, resp.StatusCode)
	}
	m.cache.delete("quota:" + email)
	return nil
}

func (m *Mailez) Sieve(ctx context.Context, email string) (SieveScript, error) {
	var s SieveScript
	if err := m.getJSON(ctx, "/sieve/"+url.PathEscape(email), &s); err != nil {
		return SieveScript{}, err
	}
	return s, nil
}

func (m *Mailez) Close() error { return nil }

func (m *Mailez) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	op := "get:" + path
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("directory: %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("directory: decode %s: %w", path, err)
	}
	_ = op // reserved for the metrics hook
	return nil
}

// ttlCache is a small TTL cache for successful lookups.
type ttlCache struct {
	mu  sync.Mutex
	m   map[string]ttlEntry
	ttl time.Duration
}

type ttlEntry struct {
	value  any
	expiry time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{m: make(map[string]ttlEntry), ttl: ttl}
}

func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiry) {
		delete(c.m, key)
		return nil, false
	}
	return e.value, true
}

func (c *ttlCache) put(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = ttlEntry{value: v, expiry: time.Now().Add(c.ttl)}
}

func (c *ttlCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

var _ Service = (*Mailez)(nil)
