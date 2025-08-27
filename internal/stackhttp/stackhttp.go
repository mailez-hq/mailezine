// Package stackhttp builds HTTP clients that authenticate to the mailez
// control plane's internal /stack API. Both sides must share the same secret
// (MAILEZINE_STACK_SECRET on this engine, MAILEZ_STACK_SECRET on the control
// plane); an empty secret keeps the legacy unauthenticated local-dev mode.
package stackhttp

import (
	"context"
	"net/http"
	"time"
)

// SecretHeader carries the shared /stack API secret; the control plane also
// accepts it as ?stack_secret= for consumers that cannot set headers.
const SecretHeader = "X-Stack-Secret"

type secretTransport struct {
	base   http.RoundTripper
	secret string
}

func (t secretTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.secret != "" {
		req = req.Clone(req.Context())
		req.Header.Set(SecretHeader, t.secret)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// New returns a client that presents the shared stack secret on every request.
func New(secret string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: secretTransport{secret: secret},
	}
}

// First returns the first non-empty value in ss, or "" if none — used to
// peel the secret out of variadic constructor parameters.
func First(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Retry calls fn up to attempts times with linear backoff (attempt i waits
// i*base), while fn reports retry=true. A definitive outcome (retry=false)
// returns its error unchanged; after exhausting attempts the last retryable
// error is returned. ctx cancellation during backoff aborts immediately.
func Retry(ctx context.Context, attempts int, base time.Duration, fn func() (retry bool, err error)) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * base):
			}
		}
		retry, err := fn()
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
