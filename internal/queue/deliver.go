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
	"sync"
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
	// AllowPlaintextAuth opts into sending credentials over unencrypted
	// connections (smarthost on a trusted loopback/LAN). Default false:
	// AUTH is attempted only after STARTTLS succeeded.
	AllowPlaintextAuth bool
	// PolicyResolver drives MTA-STS/DANE lookups for outbound TLS policy.
	// nil disables policy enforcement (opportunistic TLS only).
	PolicyResolver maildns.Resolver
	// MTSTS supplies MTA-STS policies; nil skips MTA-STS (DANE still runs).
	MTSTS mtastsSource
	// DomainConcurrency bounds the parallel delivery across a message's
	// destination domains (0 → default 8; negative → fully serial, the
	// pre-parallel behaviour). Per-domain MX fallback stays sequential.
	DomainConcurrency int
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

	results := make([]Result, len(to))
	// domains maps each destination domain to the indexes of its recipients
	// in `to`; order keeps the first-appearance order for determinism.
	domains := map[string][]int{}
	var order []string
	for i, addr := range to {
		domain, ok := domainOf(addr)
		if !ok {
			// Malformed recipient: resolve permanently right away so it
			// cannot linger pending in the queue (and eventually vanish
			// without even a DSN — the address is unparseable for bounces
			// too, but the sender at least gets the rejection at RCPT time
			// semantics via the queue result).
			results[i] = Result{
				To:        addr,
				Permanent: true,
				Err:       fmt.Errorf("queue: invalid recipient address %q", addr),
			}
			continue
		}
		if _, seen := domains[domain]; !seen {
			order = append(order, domain)
		}
		domains[domain] = append(domains[domain], i)
	}

	// Domains deliver in parallel with a bounded fan-out: a newsletter to 20
	// domains costs the slowest domain, not their sum, while the cap keeps
	// the peak connection/MX-resolver load predictable. Each task writes
	// only its own recipients' indexes, so the shared results slice needs
	// no locking.
	conc := d.DomainConcurrency
	if conc == 0 {
		conc = defaultDomainConcurrency
	}
	if conc < 0 {
		conc = 1
	} else if conc > len(order) {
		conc = len(order)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for _, domain := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string, idxs []int) {
			defer wg.Done()
			defer func() { <-sem }()
			d.deliverDomain(ctx, from, domain, to, idxs, body, port, results)
		}(domain, domains[domain])
	}
	wg.Wait()
	return results, nil
}

// defaultDomainConcurrency bounds the per-message cross-domain fan-out.
const defaultDomainConcurrency = 8

// deliverDomain resolves MX and delivers one domain's recipients over one
// connection (trying MX hosts in preference order), writing every outcome
// into results at the recipient's original index.
func (d *SMTPDeliverer) deliverDomain(ctx context.Context, from, domain string, to []string, idxs []int, body []byte, port int, results []Result) {
	addrs := make([]string, len(idxs))
	for i, idx := range idxs {
		addrs[i] = to[idx]
	}
	hosts, err := d.mxCandidates(ctx, domain)
	if err != nil {
		for _, idx := range idxs {
			results[idx] = Result{To: to[idx], Permanent: true, Err: err}
		}
		return
	}
	// RFC 5321 §5.1: try each MX host in preference order; a transient
	// failure (unreachable primary) advances to the next secondary
	// within the same attempt instead of burning a retry round.
	var resps []mailsmtp.Response
	var lastErr error
	delivered := false
	for _, host := range hosts {
		tlsMode, daneRecords, policyErr := outboundTLSPolicy(ctx, d.PolicyResolver, d.MTSTS, d.Logger, domain, host)
		if policyErr != nil {
			lastErr = policyErr
			continue
		}
		resps, lastErr = d.deliverGroup(ctx, from, host, addrs, body, port, tlsMode, daneRecords)
		if lastErr == nil || len(resps) > 0 {
			delivered = true
			break
		}
		if permanentError(lastErr) {
			break
		}
		d.Logger.Info("queue: MX host failed, trying next", "domain", domain, "host", host, "err", lastErr)
	}
	if !delivered {
		permanent := lastErr != nil && permanentError(lastErr)
		for _, idx := range idxs {
			results[idx] = Result{To: to[idx], Permanent: permanent, Err: lastErr}
		}
		return
	}
	for i, idx := range idxs {
		res := Result{To: to[idx], OK: true}
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
		results[idx] = res
	}
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
	// Whole-session cap so a wedged server cannot pin a queue worker
	// forever (large bodies over slow links still fit in 10 minutes).
	const sessionTimeout = 10 * time.Minute
	connect := func(noTLS bool) (*mailsmtp.Client, error) {
		client, err := mailsmtp.Dial(ctx, addr, mailsmtp.ConnOptions{
			Dialer:  d.Dialer,
			Host:    host,
			TLSMode: tlsMode,
			DANE:    daneRecords,
			Timeout: sessionTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("queue: dial %s: %w", host, err)
		}
		if err := client.Greet(ctx, strings.ToLower(d.Hostname), tlsMode, daneRecords, noTLS); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("queue: smtp handshake with %s: %w", host, err)
		}
		if d.Username != "" && d.FixedHost != "" && client.Capabilities().AdvertisesAuth("PLAIN") {
			// Credentials only travel over TLS: SASL PLAIN base64 is not
			// encryption, and the plaintext reconnect fallback would
			// otherwise leak them to any on-path observer.
			if !client.TLSEnabled() && !d.AllowPlaintextAuth {
				_ = client.Close()
				return nil, fmt.Errorf("queue: %s offers AUTH PLAIN without TLS: refusing to send credentials (enable STARTTLS or set allow-plaintext-auth)", host)
			}
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

// mxCandidates returns delivery hosts in preference order: every MX host
// (preference ascending, duplicates collapsed), or — when the domain has no
// usable MX records — its A/AAAA addresses as the implicit MX (RFC 5321
// §5.1). A null MX (".", RFC 7505) is terminal: the domain accepts no mail
// and the address fallback must not kick in.
func (d *SMTPDeliverer) mxCandidates(ctx context.Context, domain string) ([]string, error) {
	var candidates []string
	seen := map[string]bool{}
	mxs, mxErr := d.Resolver.LookupMX(ctx, domain)
	nullMX := false
	for _, mx := range mxs {
		if mx.Host == "." {
			nullMX = true
			continue
		}
		host := strings.TrimSuffix(mx.Host, ".")
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		candidates = append(candidates, host)
	}
	if nullMX && len(candidates) == 0 {
		return nil, fmt.Errorf("queue: domain %s does not accept mail (null MX, RFC 7505)", domain)
	}
	if len(candidates) > 0 {
		return candidates, nil
	}
	ips, ipErr := d.Resolver.LookupIPAddr(ctx, domain)
	for _, ip := range ips {
		s := ip.IP.String()
		if !seen[s] {
			seen[s] = true
			candidates = append(candidates, s)
		}
	}
	if len(candidates) > 0 {
		return candidates, nil
	}
	if ipErr == nil && mxErr == nil {
		return nil, fmt.Errorf("queue: no MX or address for %s", domain)
	}
	if mxErr != nil {
		return nil, fmt.Errorf("queue: no MX or address for %s: %w", domain, mxErr)
	}
	return nil, fmt.Errorf("queue: no MX or address for %s: %w", domain, ipErr)
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
