// Relay-aware outbound routing: recipients of domains with a directory
// relay entry (transport "smtp:[host][:port]", the smarthost form) are
// delivered to that fixed host; everyone else goes through the normal MX
// deliverer. LMTP and "smtp:host" (MX-of-host) transports fall back to
// normal delivery for v1.
package queue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"mailezine/internal/directory"
)

// RelayDeliverer splits recipients by directory relay transport.
type RelayDeliverer struct {
	Directory directory.Service
	Direct    Deliverer
	// NewFixed builds the smarthost deliverer for one host:port. Defaults
	// to a clone of Direct when Direct is an *SMTPDeliverer.
	NewFixed func(host string, port int) Deliverer
	Logger   *slog.Logger
}

// Deliver routes direct recipients through Direct and relayed recipients
// through per-host smarthost deliverers. The result order follows to.
func (d *RelayDeliverer) Deliver(ctx context.Context, from string, to []string, msg io.Reader) ([]Result, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	body, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	type target struct {
		host string
		port int
	}
	var direct []string
	groups := map[target][]string{}
	var order []target
	for _, addr := range to {
		host, port, ok := d.relayTarget(ctx, addr)
		if !ok {
			direct = append(direct, addr)
			continue
		}
		t := target{host: host, port: port}
		if _, seen := groups[t]; !seen {
			order = append(order, t)
		}
		groups[t] = append(groups[t], addr)
	}

	var results []Result
	if len(direct) > 0 {
		res, err := d.Direct.Deliver(ctx, from, direct, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		results = append(results, res...)
	}
	for _, t := range order {
		deliver := d.fixedDeliverer(t.host, t.port)
		res, err := deliver.Deliver(ctx, from, groups[t], bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		results = append(results, res...)
	}
	return results, nil
}

func (d *RelayDeliverer) relayTarget(ctx context.Context, addr string) (string, int, bool) {
	r, err := d.Directory.Relay(ctx, addr)
	if err != nil {
		if !errors.Is(err, directory.ErrNotFound) {
			d.Logger.Warn("queue: relay lookup", "to", addr, "err", err)
		}
		return "", 0, false
	}
	host, port, ok := parseRelayTransport(r.Transport)
	if !ok {
		d.Logger.Debug("queue: relay transport not supported, using MX", "to", addr, "transport", r.Transport)
		return "", 0, false
	}
	return host, port, true
}

func (d *RelayDeliverer) fixedDeliverer(host string, port int) Deliverer {
	if d.NewFixed != nil {
		return d.NewFixed(host, port)
	}
	if direct, ok := d.Direct.(*SMTPDeliverer); ok {
		clone := *direct
		clone.FixedHost = host
		clone.FixedPort = port
		return &clone
	}
	d.Logger.Warn("queue: no smarthost deliverer configured; relaying via MX", "host", host)
	return d.Direct
}

// parseRelayTransport accepts the smarthost form "smtp:[host]" or
// "smtp:[host]:port". Other forms (MX-of-host, LMTP) return ok=false so the
// caller falls back to normal delivery.
func parseRelayTransport(transport string) (host string, port int, ok bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(transport), ":")
	if !found || !strings.EqualFold(scheme, "smtp") {
		return "", 0, false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "[") {
		return "", 0, false
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return "", 0, false
	}
	host = rest[1:end]
	if host == "" {
		return "", 0, false
	}
	after := rest[end+1:]
	if strings.HasPrefix(after, ":") {
		p, err := strconv.Atoi(after[1:])
		if err != nil || p <= 0 || p > 65535 {
			return "", 0, false
		}
		port = p
	}
	return host, port, true
}
