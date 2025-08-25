// Delivery agents for the queue. The SMTPDeliverer uses the engine's own
// SMTP client (internal/mailsmtp) and DNS abstraction (internal/maildns),
// handles MX resolution + per-recipient results, and applies the outbound
// TLS policy from tlspolicy.go (MTA-STS/DANE).
package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"mailezine/internal/maildns"
	"mailezine/internal/mailsmtp"
)

// Deliverer sends a spooled message to every recipient and reports the
// outcome per recipient (same order as to). A non-nil error means the whole
// transaction failed and no per-recipient results are available.
type Deliverer interface {
	Deliver(ctx context.Context, from string, to []string, msg io.Reader) ([]Result, error)
}

// Result is the outcome for one recipient.
type Result struct {
	To        string
	OK        bool
	Permanent bool
	Err       error
}

// MXResolver abstracts MX and address lookup so tests can avoid DNS.
type MXResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// SMTPDeliverer delivers over SMTP using internal/mailsmtp.
type SMTPDeliverer struct {
	Logger     *slog.Logger
	Hostname   string // our EHLO name
	Resolver   MXResolver
	Port       int // SMTP port, default 25
	Dialer     *net.Dialer
	RequireTLS bool
	// FixedHost routes every delivery to one smarthost (directory relay
	// transport "smtp:[host]"), skipping MX resolution.
	FixedHost string
	FixedPort int // optional port for FixedHost; defaults to Port/25
	// Username/Password enable SASL AUTH (preferring PLAIN) for smarthost
	// deliveries; ignored for direct MX deliveries.
	Username string
	Password string
	// PolicyResolver drives MTA-STS/DANE lookups for outbound TLS policy.
	// nil disables policy enforcement (opportunistic TLS only).
	PolicyResolver maildns.Resolver
	// MTSTS supplies MTA-STS policies; nil skips MTA-STS (DANE still runs).
	MTSTS mtastsSource
}

// Deliver groups recipients by domain, resolves MX (with A/AAAA fallback)
// and delivers per domain over one connection.
func (d *SMTPDeliverer) Deliver(ctx context.Context, from string, to []string, msg io.Reader) ([]Result, error) {
	if d.Resolver == nil {
		d.Resolver = net.DefaultResolver
	}
	if d.Dialer == nil {
		d.Dialer = &net.Dialer{Timeout: 30 * time.Second}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	port := d.Port
	if port == 0 {
		port = 25
	}
	body, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	if d.FixedHost != "" {
		if d.FixedPort != 0 {
			port = d.FixedPort
		}
		tlsMode, daneRecords, _ := outboundTLSPolicy(ctx, d.PolicyResolver, d.MTSTS, d.Logger, "", d.FixedHost)
		return d.deliverToHost(ctx, from, to, body, d.FixedHost, port, tlsMode, daneRecords)
	}

	groups := map[string][]string{}
	var domains []string
	for _, addr := range to {
		domain, ok := domainOf(addr)
		if !ok {
			continue // malformed; reported as missing result by caller
		}
		if _, seen := groups[domain]; !seen {
			domains = append(domains, domain)
		}
		groups[domain] = append(groups[domain], addr)
	}

	var results []Result
	for _, domain := range domains {
		host, err := d.mxFor(ctx, domain)
		if err != nil {
			for _, addr := range groups[domain] {
				results = append(results, Result{To: addr, Permanent: true, Err: err})
			}
			continue
		}
		addrs := groups[domain]
		tlsMode, daneRecords, policyErr := outboundTLSPolicy(ctx, d.PolicyResolver, d.MTSTS, d.Logger, domain, host)
		if policyErr != nil {
			for _, addr := range addrs {
				results = append(results, Result{To: addr, Permanent: true, Err: policyErr})
			}
			continue
		}
		resps, err := d.deliverGroup(ctx, from, host, addrs, body, port, tlsMode, daneRecords)
		if err != nil && len(resps) == 0 {
			permanent := permanentError(err)
			for _, addr := range addrs {
				results = append(results, Result{To: addr, Permanent: permanent, Err: err})
			}
			continue
		}
		for i, addr := range addrs {
			res := Result{To: addr, OK: true}
			if i < len(resps) {
				r := resps[i]
				// Success is code 2xx. Rejected recipients carry their RCPT
				// response, so Err alone is not a reliable signal.
				if r.Code/100 != 2 {
					res.OK = false
					res.Permanent = r.Permanent
					if r.Err != nil {
						res.Err = r.Err
					} else {
						res.Err = &mailsmtp.ResponseError{Response: r}
					}
				}
			}
			results = append(results, res)
		}
	}
	return results, nil
}

// deliverToHost sends every recipient to one fixed host (smarthost path).
func (d *SMTPDeliverer) deliverToHost(ctx context.Context, from string, to []string, body []byte, host string, port int, tlsMode mailsmtp.TLSMode, daneRecords []maildns.TLSA) ([]Result, error) {
	if len(to) == 0 {
		return nil, nil
	}
	resps, err := d.deliverGroup(ctx, from, host, to, body, port, tlsMode, daneRecords)
	if err != nil && len(resps) == 0 {
		permanent := permanentError(err)
		var results []Result
		for _, addr := range to {
			results = append(results, Result{To: addr, Permanent: permanent, Err: err})
		}
		return results, nil
	}
	var results []Result
	for i, addr := range to {
		res := Result{To: addr, OK: true}
		if i < len(resps) {
			r := resps[i]
			if r.Code/100 != 2 {
				res.OK = false
				res.Permanent = r.Permanent
				if r.Err != nil {
					res.Err = r.Err
				} else {
					res.Err = &mailsmtp.ResponseError{Response: r}
				}
			}
		}
		results = append(results, res)
	}
	return results, nil
}

func (d *SMTPDeliverer) deliverGroup(ctx context.Context, from, host string, addrs []string, body []byte, port int, tlsMode mailsmtp.TLSMode, daneRecords []maildns.TLSA) ([]mailsmtp.Response, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	connect := func(noTLS bool) (*mailsmtp.Client, error) {
		client, err := mailsmtp.Dial(ctx, addr, mailsmtp.ConnOptions{
			Dialer:  d.Dialer,
			Host:    host,
			TLSMode: tlsMode,
			DANE:    daneRecords,
		})
		if err != nil {
			return nil, fmt.Errorf("queue: dial %s: %w", host, err)
		}
		if err := client.Greet(ctx, strings.ToLower(d.Hostname), tlsMode, daneRecords, noTLS); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("queue: smtp handshake with %s: %w", host, err)
		}
		if d.Username != "" && d.FixedHost != "" && client.Capabilities().AdvertisesAuth("PLAIN") {
			// Third-party smarthost relays: authenticate with SASL PLAIN
			// when advertised.
			if err := client.AuthPlain(ctx, d.Username, d.Password); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("queue: smtp auth with %s: %w", host, err)
			}
		}
		return client, nil
	}
	client, err := connect(false)
	if errors.Is(err, mailsmtp.ErrNoTLSUpgrade) {
		// Fail-open policy (D11): an opportunistic upgrade that cannot
		// complete falls back to a plaintext retry on a fresh connection.
		client, err = connect(true)
	}
	if err != nil {
		return nil, err
	}
	resps, rerr := client.Deliver(ctx, from, addrs, body)
	_ = client.Close()
	return resps, rerr
}

func (d *SMTPDeliverer) PortOrDefault() int {
	if d.Port == 0 {
		return 25
	}
	return d.Port
}

// mxFor resolves the delivery host: the lowest-preference MX, or the domain
// itself when it has A/AAAA records and no MX exists (RFC 5321 fallback).
func (d *SMTPDeliverer) mxFor(ctx context.Context, domain string) (string, error) {
	mxs, err := d.Resolver.LookupMX(ctx, domain)
	if err == nil {
		for _, mx := range mxs {
			if host := strings.TrimSuffix(mx.Host, "."); host != "" {
				return host, nil
			}
		}
	}
	ips, ipErr := d.Resolver.LookupIPAddr(ctx, domain)
	if ipErr == nil && len(ips) > 0 {
		return ips[0].IP.String(), nil
	}
	if err != nil {
		return "", fmt.Errorf("queue: no MX or address for %s: %w", domain, err)
	}
	return "", fmt.Errorf("queue: no MX or address for %s", domain)
}

func domainOf(addr string) (string, bool) {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return "", false
	}
	return addr[at+1:], true
}

func permanentError(err error) bool {
	var pe *mailsmtp.ResponseError
	if errors.As(err, &pe) {
		return pe.Permanent
	}
	return false
}
