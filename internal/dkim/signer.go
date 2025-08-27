// Package dkim signs outbound messages with keys served by the mailez
// control plane — the same vault contract rspamd consumes
// (/stack/rspamd/vault/v1/dkim/<domain>) — using go-msgauth's RFC 6376
// implementation (MIT). Signing happens once at enqueue
// (ARCHITECTURE.md §5).
package dkim

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"mailezine/internal/stackhttp"
)

const (
	positiveTTL = 5 * time.Minute
	negativeTTL = 30 * time.Second
	expiration  = 72 * time.Hour
)

// signHeaders is the DKIM header list (RFC 6376 §5.4.1 recommended set).
var signHeaders = []string{
	"From", "Sender", "Reply-To", "Subject", "Date", "Message-ID",
	"To", "Cc", "MIME-Version", "Content-Type", "Content-Transfer-Encoding",
	"List-Id", "List-Help", "List-Unsubscribe", "List-Subscribe", "List-Post",
	"List-Owner", "List-Archive",
}

// Key is one signing key for a domain.
type Key struct {
	Domain   string
	Selector string
	PEM      []byte
}

// Signer fetches and caches DKIM keys and signs messages.
type Signer struct {
	logger *slog.Logger
	hc     *http.Client
	vault  string // base URL of the vault, e.g. http://backend:8080/stack/rspamd/vault/v1/dkim/

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	key      *Key
	fetched  time.Time
	negative time.Time
}

// NewSigner builds a signer against the mailez DKIM vault. The optional
// secret authenticates requests to the /stack API; empty keeps the legacy
// unauthenticated local-dev mode.
func NewSigner(vaultBase string, logger *slog.Logger, secret ...string) *Signer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Signer{
		logger: logger,
		hc:     stackhttp.New(stackhttp.First(secret), 5*time.Second),
		vault:  strings.TrimSuffix(vaultBase, "/") + "/v1/dkim/",
		cache:  map[string]cacheEntry{},
	}
}

// Sign prepends a DKIM-Signature header to msg. When no key is configured
// for the sender domain the message is returned unchanged.
func (s *Signer) Sign(ctx context.Context, from string, msg []byte) ([]byte, error) {
	_, domain, ok := splitFrom(from)
	if !ok {
		return msg, nil
	}
	key, err := s.keyFor(ctx, domain)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return msg, nil
	}
	priv, err := parsePrivateKey(key.PEM)
	if err != nil {
		return nil, fmt.Errorf("dkim: key for %s: %w", domain, err)
	}
	selector := key.Selector
	if selector == "" {
		selector = "dkim"
	}
	var out bytes.Buffer
	err = dkim.Sign(&out, bytes.NewReader(msg), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 priv,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             signHeaders,
		Expiration:             time.Now().Add(expiration),
	})
	if err != nil {
		return nil, fmt.Errorf("dkim: sign for %s: %w", domain, err)
	}
	// dkim.Sign writes the DKIM-Signature header field followed by the full
	// signed message; the output is ready to store or deliver as-is.
	return out.Bytes(), nil
}

func (s *Signer) keyFor(ctx context.Context, domain string) (*Key, error) {
	s.mu.Lock()
	if e, ok := s.cache[domain]; ok {
		if e.key != nil && time.Since(e.fetched) < positiveTTL {
			s.mu.Unlock()
			return e.key, nil
		}
		if e.key == nil && time.Since(e.negative) < negativeTTL {
			s.mu.Unlock()
			return nil, nil
		}
	}
	s.mu.Unlock()

	key, err := s.fetch(ctx, domain)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if key != nil {
		s.cache[domain] = cacheEntry{key: key, fetched: time.Now()}
	} else {
		s.cache[domain] = cacheEntry{negative: time.Now()}
	}
	s.mu.Unlock()
	return key, nil
}

// vaultResponse mirrors /stack/rspamd/vault/v1/dkim/<domain>.
type vaultResponse struct {
	Data struct {
		Selectors []struct {
			Domain   string `json:"domain"`
			Key      string `json:"key"`
			Selector string `json:"selector"`
		} `json:"selectors"`
	} `json:"data"`
}

func (s *Signer) fetch(ctx context.Context, domain string) (*Key, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.vault+url.PathEscape(domain), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dkim: vault %s: %w", domain, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dkim: vault %s: status %d", domain, resp.StatusCode)
	}
	var vr vaultResponse
	if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
		return nil, fmt.Errorf("dkim: vault %s: decode: %w", domain, err)
	}
	if len(vr.Data.Selectors) == 0 {
		return nil, nil
	}
	sel := vr.Data.Selectors[0]
	return &Key{Domain: sel.Domain, Selector: sel.Selector, PEM: []byte(sel.Key)}, nil
}

// parsePrivateKey accepts RSA PKCS#1 or PKCS#8 PEM.
func parsePrivateKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := k.(crypto.Signer); ok {
			return signer, nil
		}
	}
	return nil, errors.New("unsupported private key format")
}

func splitFrom(addr string) (local, domain string, ok bool) {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", "", false
	}
	return addr[:at], addr[at+1:], true
}
