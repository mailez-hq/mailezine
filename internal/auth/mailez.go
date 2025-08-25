// Mailez validates credentials against the mailez control plane. The
// /stack/auth/email endpoint is the single authentication authority; this
// client speaks the same header contract nginx uses (Auth-Method,
// Auth-Protocol, Auth-User, Auth-Pass, Client-Ip) and treats
// "Auth-Status: OK" as success.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Mailez is a Service backed by the mailez auth contract.
type Mailez struct {
	base string // e.g. http://backend:8080/stack
	hc   *http.Client
}

// NewMailez returns an auth client for the mailez control plane.
func NewMailez(base string) *Mailez {
	return &Mailez{
		base: base,
		hc:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (m *Mailez) Authenticate(ctx context.Context, email, password string, opts Options) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/auth/email", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Auth-Method", "PLAIN")
	req.Header.Set("Auth-Protocol", strings.ToLower(opts.Protocol))
	req.Header.Set("Auth-Port", opts.Port)
	req.Header.Set("Auth-User", email)
	req.Header.Set("Auth-Pass", password)
	req.Header.Set("Client-Ip", opts.ClientIP)

	resp, err := m.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("auth: /auth/email: status %d", resp.StatusCode)
	}
	return resp.Header.Get("Auth-Status") == "OK", nil
}

func (m *Mailez) Close() error { return nil }

var _ Service = (*Mailez)(nil)
