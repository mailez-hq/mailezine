package mailmtasts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	p, err := Parse("version: STSv1\nmode: enforce\nmax_age: 86400\nmx: mx1.example.com\nmx: *.example.net\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != ModeEnforce || p.MaxAge != 24*time.Hour || len(p.MX) != 2 {
		t.Fatalf("policy = %+v", p)
	}
	if !p.Matches("mx1.example.com") || !p.Matches("a.example.net") || !p.Matches("evil.example.net") || p.Matches("example.net") {
		t.Fatalf("matching failed: %+v", p)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"no version", "mode: enforce\nmax_age: 86400\nmx: mx.example.com\n"},
		{"bad mode", "version: STSv1\nmode: maybe\nmax_age: 86400\nmx: mx.example.com\n"},
		{"no mx", "version: STSv1\nmode: enforce\nmax_age: 86400\n"},
		{"bad max", "version: STSv1\nmode: enforce\nmax_age: 0\nmx: mx.example.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.doc); err == nil {
				t.Fatalf("expected error for %q", tc.doc)
			}
		})
	}
}

func TestFetchAndCache(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("version: STSv1\nmode: enforce\nmax_age: 86400\nmx: mx.example.com\n"))
	}))
	defer srv.Close()

	hc := srv.Client()
	f := NewFetcher().WithClient(hc)
	// Point the fetcher at the test server by overriding the transport for
	// the mta-sts host: easiest is a client that rewrites the URL host.
	orig := hc.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	hc.Transport = rewriteTransport{base: orig, from: "mta-sts.example.com", to: srv.Listener.Addr().String()}

	ctx := context.Background()
	p, err := f.Lookup(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Mode != ModeEnforce {
		t.Fatalf("policy = %+v", p)
	}
	if _, err := f.Lookup(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("fetches = %d, want 1 (cached)", hits)
	}

	// A failing fetch is cached negatively and returns nil policy.
	f2 := NewFetcher().WithClient(&http.Client{Timeout: time.Second})
	if p2, err := f2.Lookup(ctx, "no-policy.example"); err != nil || p2 != nil {
		t.Fatalf("failed lookup: p=%v err=%v", p2, err)
	}
}

// rewriteTransport rewrites the request host so tests can run without DNS.
type rewriteTransport struct {
	base http.RoundTripper
	from string
	to   string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.URL.Host == r.from {
		clone.URL.Host = r.to
	}
	return r.base.RoundTrip(clone)
}
