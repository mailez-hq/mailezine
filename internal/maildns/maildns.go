// Package maildns is the DNS surface for outbound mail decisions (MX, TXT,
// TLSA) and inbound verification. It wraps net.Resolver for standard
// records and miekg/dns for TLSA, exposing a small interface the engine
// controls (tests inject fakes).
package maildns

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

// TLSA is one TLSA record (RFC 6698).
type TLSA struct {
	Usage        uint8
	Selector     uint8
	MatchingType uint8
	Cert         []byte
}

// Resolver answers the DNS queries the engine needs.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupTLSA(ctx context.Context, port int, proto, name string) ([]TLSA, error)
}

// ValidatingResolver is implemented by resolvers that can report whether a
// TLSA answer was DNSSEC-validated (AD bit set by a validating recursive
// resolver). DANE (RFC 7672 §2.2) may only act on TLSA records whose
// authenticity is cryptographically established; resolvers without this
// capability keep the legacy take-records-at-face-value behavior.
type ValidatingResolver interface {
	Resolver
	// LookupTLSAValidated returns TLSA records plus whether the answer
	// carried the authenticated-data bit.
	LookupTLSAValidated(ctx context.Context, port int, proto, name string) ([]TLSA, bool, error)
}

// SystemResolver answers with the system resolver (net.DefaultResolver plus
// miekg/dns for TLSA). DNSSEC validation is left to the configured
// resolver; records are returned verbatim and authenticated-data handling
// stays in the policy layer.
type SystemResolver struct {
	// Dial optionally overrides the DNS transport (tests).
	dnsClient *dns.Client
	mu        sync.Mutex
	servers   []string
}

var _ Resolver = (*SystemResolver)(nil)

// NewSystemResolver builds a resolver using the system DNS configuration.
func NewSystemResolver() *SystemResolver {
	return &SystemResolver{dnsClient: &dns.Client{Net: "udp"}}
}

// LookupTXT queries TXT records.
func (r *SystemResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// LookupMX queries MX records.
func (r *SystemResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return net.DefaultResolver.LookupMX(ctx, name)
}

// LookupIPAddr resolves A/AAAA.
func (r *SystemResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// LookupAddr resolves an IP to names (PTR).
func (r *SystemResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return net.DefaultResolver.LookupAddr(ctx, addr)
}

// LookupTLSA queries TLSA records via the system DNS servers. The query
// sets the DNSSEC OK (DO) bit so validating resolvers establish the chain
// and set the authenticated-data flag; records are returned verbatim.
func (r *SystemResolver) LookupTLSA(ctx context.Context, port int, proto, name string) ([]TLSA, error) {
	recs, _, err := r.lookupTLSA(ctx, port, proto, name)
	return recs, err
}

// LookupTLSAValidated additionally reports whether the answer carried the
// DNSSEC authenticated-data bit (a secure answer). An insecure or
// unvalidated answer yields validated=false even when records exist.
func (r *SystemResolver) LookupTLSAValidated(ctx context.Context, port int, proto, name string) ([]TLSA, bool, error) {
	return r.lookupTLSA(ctx, port, proto, name)
}

var _ ValidatingResolver = (*SystemResolver)(nil)

func (r *SystemResolver) lookupTLSA(ctx context.Context, port int, proto, name string) ([]TLSA, bool, error) {
	servers, err := r.serversLocked()
	if err != nil {
		return nil, false, err
	}
	fqdn := dns.Fqdn(fmt.Sprintf("_%d._%s.%s", port, proto, name))
	var lastErr error
	for _, srv := range servers {
		msg := new(dns.Msg)
		msg.SetQuestion(fqdn, dns.TypeTLSA)
		msg.RecursionDesired = true
		// DNSSEC OK: without DO a validating resolver performs validation
		// upstream but RFC 6840 §5.7 forbids setting AD on the answer, so
		// the caller could never distinguish secure from insecure.
		msg.SetEdns0(4096, true)
		resp, _, err := r.dnsClient.ExchangeContext(ctx, msg, srv)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("maildns: tlsa lookup %s: %s", fqdn, dns.RcodeToString[resp.Rcode])
			continue
		}
		var out []TLSA
		for _, rr := range resp.Answer {
			t, ok := rr.(*dns.TLSA)
			if !ok {
				continue
			}
			cert, err := base64.StdEncoding.DecodeString(t.Certificate)
			if err != nil {
				continue
			}
			out = append(out, TLSA{
				Usage:        t.Usage,
				Selector:     t.Selector,
				MatchingType: t.MatchingType,
				Cert:         cert,
			})
		}
		return out, resp.AuthenticatedData, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("maildns: no TLSA response for %s", fqdn)
	}
	return nil, false, lastErr
}

func (r *SystemResolver) serversLocked() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.servers) > 0 {
		return r.servers, nil
	}
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	for _, s := range cfg.Servers {
		r.servers = append(r.servers, net.JoinHostPort(s, cfg.Port))
	}
	if len(r.servers) == 0 {
		return nil, fmt.Errorf("maildns: no DNS servers configured")
	}
	return r.servers, nil
}

// NormalizeDomain lowercases and strips a trailing dot.
func NormalizeDomain(s string) string {
	return strings.ToLower(strings.TrimSuffix(s, "."))
}
