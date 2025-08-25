// Mailez implements Service against the mailez control plane
// (/stack/directory/*, see mailez/backend/internal/stack/directory.go).
// Responses are cached briefly so hot paths (SMTP recipient resolution) do
// not hammer the control plane; 404s get a short negative cache so address
// probing cannot either.
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
	"time"

	"mailezine/internal/mailcache"
)

// Mailez is a Service backed by the mailez directory contract.
type Mailez struct {
	base  string // e.g. http://backend:8080/stack/directory
	hc    *http.Client
	cache *mailcache.Cache
}

// NewMailez returns a directory client for the mailez control plane.
func NewMailez(base string, cacheTTL time.Duration, maxWeight int64) *Mailez {
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Second
	}
	return &Mailez{
		base:  base,
		hc:    &http.Client{Timeout: 6 * time.Second},
		cache: mailcache.NewCacheWithNegative(maxWeight, cacheTTL, 5*time.Second),
	}
}

func (m *Mailez) User(ctx context.Context, email string) (User, error) {
	if v, ok := m.cache.Get("user:" + email); ok {
		if v == nil {
			return User{}, ErrNotFound
		}
		return v.(User), nil
	}
	var u User
	if err := m.getJSON(ctx, "user:"+email, "/users/"+url.PathEscape(email), &u); err != nil {
		return User{}, err
	}
	m.cache.Put("user:"+email, u, 256)
	return u, nil
}

func (m *Mailez) Domain(ctx context.Context, name string) (Domain, error) {
	if v, ok := m.cache.Get("domain:" + name); ok {
		if v == nil {
			return Domain{}, ErrNotFound
		}
		return v.(Domain), nil
	}
	var d Domain
	if err := m.getJSON(ctx, "domain:"+name, "/domains/"+url.PathEscape(name), &d); err != nil {
		return Domain{}, err
	}
	m.cache.Put("domain:"+name, d, 128)
	return d, nil
}

func (m *Mailez) Aliases(ctx context.Context, addr string) ([]string, error) {
	if v, ok := m.cache.Get("alias:" + addr); ok {
		if v == nil {
			return nil, ErrNotFound
		}
		return append([]string(nil), v.([]string)...), nil
	}
	var out struct {
		Targets []string `json:"targets"`
	}
	if err := m.getJSON(ctx, "alias:"+addr, "/aliases/"+url.PathEscape(addr), &out); err != nil {
		return nil, err
	}
	m.cache.Put("alias:"+addr, out.Targets, int64(len(out.Targets))*48+64)
	return append([]string(nil), out.Targets...), nil
}

func (m *Mailez) Relay(ctx context.Context, email string) (Relay, error) {
	var r Relay
	if err := m.getJSON(ctx, "relay:"+email, "/relays/"+url.PathEscape(email), &r); err != nil {
		return Relay{}, err
	}
	return r, nil
}

func (m *Mailez) Sender(ctx context.Context, email string) (Sender, error) {
	var s Sender
	if err := m.getJSON(ctx, "sender:"+email, "/senders/"+url.PathEscape(email), &s); err != nil {
		return Sender{}, err
	}
	return s, nil
}

func (m *Mailez) SenderRate(ctx context.Context, email string) (SenderRate, error) {
	var r SenderRate
	if err := m.getJSON(ctx, "rate:"+email, "/senders/"+url.PathEscape(email)+"/rate", &r); err != nil {
		return SenderRate{}, err
	}
	return r, nil
}

func (m *Mailez) SRSForward(ctx context.Context, sender string) (string, error) {
	var out struct {
		Rewritten string `json:"rewritten"`
	}
	if err := m.getJSON(ctx, "srs:"+sender, "/srs/"+url.PathEscape(sender), &out); err != nil {
		return "", err
	}
	return out.Rewritten, nil
}

func (m *Mailez) SRSRestore(ctx context.Context, recipient string) (string, error) {
	var out struct {
		Original string `json:"original"`
	}
	if err := m.getJSON(ctx, "srsr:"+recipient, "/srs/restore/"+url.PathEscape(recipient), &out); err != nil {
		return "", err
	}
	return out.Original, nil
}

func (m *Mailez) Quota(ctx context.Context, email string) (Quota, error) {
	if v, ok := m.cache.Get("quota:" + email); ok {
		if v == nil {
			return Quota{}, ErrNotFound
		}
		return v.(Quota), nil
	}
	var q Quota
	if err := m.getJSON(ctx, "quota:"+email, "/quota/"+url.PathEscape(email), &q); err != nil {
		return Quota{}, err
	}
	m.cache.Put("quota:"+email, q, 64)
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
	m.cache.Remove("quota:" + email)
	return nil
}

func (m *Mailez) Sieve(ctx context.Context, email string) (SieveScript, error) {
	var s SieveScript
	if err := m.getJSON(ctx, "sieve:"+email, "/sieve/"+url.PathEscape(email), &s); err != nil {
		return SieveScript{}, err
	}
	return s, nil
}

func (m *Mailez) Close() error { return nil }

func (m *Mailez) getJSON(ctx context.Context, key, path string, out any) error {
	// Retry transient transport/5xx failures with backoff: under concurrent
	// logins the shared control plane's bcrypt gate queues auth requests,
	// and a directory lookup that shares the queue must not fail the whole
	// login. 404s are definitive and are cached as negatives, never retried.
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * 500 * time.Millisecond):
			}
		}
		retry, err := m.tryGetJSON(ctx, key, path, out)
		if err == nil {
			return nil
		}
		if !retry {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// tryGetJSON performs one request. retry=false marks a definitive outcome
// (404 or a 4xx client error) that must not be retried.
func (m *Mailez) tryGetJSON(ctx context.Context, key, path string, out any) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return true, err
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		m.cache.PutNegative(key)
		return false, ErrNotFound
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusGatewayTimeout:
		return true, fmt.Errorf("directory: %s: status %d", path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("directory: %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("directory: decode %s: %w", path, err)
	}
	return false, nil
}

var _ Service = (*Mailez)(nil)
