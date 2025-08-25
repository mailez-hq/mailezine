// Package mailmtasts implements MTA-STS (RFC 8461): fetching, parsing and
// matching the policy published at https://mta-sts.<domain>/.well-known/
// mta-sts.txt. It is a plain HTTP fetch with an in-memory TTL cache, an
// injectable client for tests, and strict size limits so a misbehaving
// policy host cannot exhaust memory.
package mailmtasts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Mode is the RFC 8461 policy mode.
type Mode int

const (
	// ModeNone is returned when no usable policy exists (or mode=none).
	ModeNone Mode = iota
	// ModeTesting publishes a policy without enforcing it.
	ModeTesting
	// ModeEnforce requires TLS with the listed MX hosts.
	ModeEnforce
)

func (m Mode) String() string {
	switch m {
	case ModeTesting:
		return "testing"
	case ModeEnforce:
		return "enforce"
	default:
		return "none"
	}
}

const (
	// maxPolicyBytes caps a policy fetch (RFC 8461 §3.2 recommends 64 KiB).
	maxPolicyBytes = 64 * 1024
	// enforceTTL caps how long a fetched policy is trusted. RFC 8461
	// requires publishers to use max_age >= 1 day; caching for a full day
	// is the longest a stale enforced policy should remain in effect.
	enforceTTL = 24 * time.Hour
	// negativeTTL is how long a failed lookup is remembered.
	negativeTTL = 5 * time.Minute
)

// Policy is one parsed MTA-STS policy.
type Policy struct {
	Mode   Mode
	MaxAge time.Duration
	MX     []string // lowercased, trailing dot stripped
}

// Matches reports whether host is covered by the policy's mx list. A policy
// entry may be a wildcard ("*.example.com") per RFC 8461 §3.1.
func (p *Policy) Matches(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, mx := range p.MX {
		mx = strings.ToLower(strings.TrimSuffix(mx, "."))
		if mx == host {
			return true
		}
		if strings.HasPrefix(mx, "*.") && strings.HasSuffix(host, mx[1:]) {
			return true
		}
	}
	return false
}

type cacheEntry struct {
	policy *Policy
	expiry time.Time
}

// Fetcher retrieves MTA-STS policies with an in-memory TTL cache.
type Fetcher struct {
	hc *http.Client

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// NewFetcher builds a Fetcher with a 5-second HTTP client, matching the
// MTA-STS recommendation for policy fetches.
func NewFetcher() *Fetcher {
	return &Fetcher{
		hc:      &http.Client{Timeout: 5 * time.Second},
		entries: map[string]cacheEntry{},
	}
}

// WithClient returns a copy using an explicit HTTP client (tests).
func (f *Fetcher) WithClient(hc *http.Client) *Fetcher {
	return &Fetcher{hc: hc, entries: map[string]cacheEntry{}}
}

// Lookup returns the current policy for domain, caching the result. A nil
// policy with nil error means "no usable policy" (mode=none, fetch failure,
// or malformed record) — callers fail open, per the engine's documented
// policy (DECISIONS.md D11).
func (f *Fetcher) Lookup(ctx context.Context, domain string) (*Policy, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	f.mu.Lock()
	if e, ok := f.entries[domain]; ok && time.Now().Before(e.expiry) {
		f.mu.Unlock()
		return e.policy, nil
	}
	f.mu.Unlock()

	policy, err := Fetch(ctx, f.hc, domain)
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.entries[domain] = cacheEntry{expiry: now.Add(negativeTTL)}
		return nil, nil
	}
	ttl := negativeTTL
	if policy != nil {
		if ttl = policy.MaxAge; ttl > enforceTTL {
			ttl = enforceTTL
		}
		if ttl <= 0 {
			ttl = enforceTTL
		}
	}
	f.entries[domain] = cacheEntry{policy: policy, expiry: now.Add(ttl)}
	return policy, nil
}

// Fetch retrieves and parses the policy for domain without caching.
func Fetch(ctx context.Context, hc *http.Client, domain string) (*Policy, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if !validDomain(domain) {
		return nil, fmt.Errorf("mailmtasts: invalid domain %q", domain)
	}
	u := "https://mta-sts." + domain +
		"/.well-known/mta-sts.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mailezine-mtasts")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mailmtasts: fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mailmtasts: fetch %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mailmtasts: fetch %s: %w", u, err)
	}
	if len(body) > maxPolicyBytes {
		return nil, fmt.Errorf("mailmtasts: fetch %s: policy exceeds %d bytes", u, maxPolicyBytes)
	}
	return Parse(string(body))
}

// Parse parses an RFC 8461 policy document.
func Parse(s string) (*Policy, error) {
	var (
		p        Policy
		version  string
		seenMode bool
		seenMax  bool
		lineNo   int
	)
	for _, raw := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		lineNo++
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("mailmtasts: line %d: missing ':'", lineNo)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "version":
			version = value
		case "mode":
			seenMode = true
			switch strings.ToLower(value) {
			case "enforce":
				p.Mode = ModeEnforce
			case "testing":
				p.Mode = ModeTesting
			case "none":
				p.Mode = ModeNone
			default:
				return nil, fmt.Errorf("mailmtasts: line %d: invalid mode %q", lineNo, value)
			}
		case "max_age":
			seenMax = true
			var age uint64
			if _, err := fmt.Sscanf(value, "%d", &age); err != nil || age == 0 || age > 31557600 {
				return nil, fmt.Errorf("mailmtasts: line %d: invalid max_age %q", lineNo, value)
			}
			p.MaxAge = time.Duration(age) * time.Second
		case "mx":
			if value == "" {
				return nil, fmt.Errorf("mailmtasts: line %d: empty mx", lineNo)
			}
			p.MX = append(p.MX, strings.ToLower(strings.TrimSuffix(value, ".")))
		}
	}
	if version != "STSv1" {
		return nil, fmt.Errorf("mailmtasts: unsupported version %q", version)
	}
	if !seenMode {
		return nil, fmt.Errorf("mailmtasts: missing mode")
	}
	if !seenMax {
		return nil, fmt.Errorf("mailmtasts: missing max_age")
	}
	if p.Mode != ModeNone && len(p.MX) == 0 {
		return nil, fmt.Errorf("mailmtasts: no mx entries")
	}
	return &p, nil
}

// validDomain guards against policy URL injection.
func validDomain(domain string) bool {
	return domain != "" && !strings.ContainsAny(domain, "/?#@ ")
}
