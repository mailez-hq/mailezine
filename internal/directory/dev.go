// Dev is a standalone directory provider for development and tests
// (PLAN.md §4.4). It is file-backed at startup and keeps quota updates in
// memory; it is intentionally not durable and never used in production.
package directory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// DevData is the JSON shape of a dev directory file.
type DevData struct {
	Users   map[string]User       `json:"users"`
	Domains []string              `json:"domains"`
	Aliases map[string][]string   `json:"aliases"`
	Relays  map[string]Relay      `json:"relays"`
	Senders map[string][]string   `json:"senders"`
	Rates   map[string]SenderRate `json:"rates"`
	Sieve   map[string]string     `json:"sieve"`
}

// NewDev returns an in-memory dev directory. The caller owns data; the
// service never mutates the maps concurrently with reads by other parties.
func NewDev(data DevData) *Dev {
	if data.Users == nil {
		data.Users = map[string]User{}
	}
	if data.Aliases == nil {
		data.Aliases = map[string][]string{}
	}
	if data.Relays == nil {
		data.Relays = map[string]Relay{}
	}
	if data.Senders == nil {
		data.Senders = map[string][]string{}
	}
	if data.Rates == nil {
		data.Rates = map[string]SenderRate{}
	}
	if data.Sieve == nil {
		data.Sieve = map[string]string{}
	}
	return &Dev{data: data}
}

// LoadDevFile loads a dev directory from a JSON file.
func LoadDevFile(path string) (*Dev, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data DevData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return NewDev(data), nil
}

// Dev implements Service from a static in-memory directory.
type Dev struct {
	mu   sync.RWMutex
	data DevData
}

func (d *Dev) User(_ context.Context, email string) (User, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	u, ok := d.data.Users[email]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (d *Dev) Domain(_ context.Context, name string) (Domain, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, dom := range d.data.Domains {
		if strings.EqualFold(dom, name) {
			return Domain{IsLocal: true, Name: dom}, nil
		}
	}
	return Domain{}, ErrNotFound
}

func (d *Dev) Aliases(_ context.Context, addr string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	local, domain, ok := splitAddr(addr)
	if !ok {
		return nil, ErrNotFound
	}
	// Bare domain resolves to itself (post-office delivery), matching the
	// mailez contract.
	if local == "" {
		if d.localDomain(domain) {
			return []string{domain}, nil
		}
		return nil, ErrNotFound
	}
	if targets, ok := d.data.Aliases[addr]; ok {
		return append([]string(nil), targets...), nil
	}
	if _, ok := d.data.Users[addr]; ok {
		return []string{addr}, nil
	}
	return nil, ErrNotFound
}

func (d *Dev) Relay(_ context.Context, email string) (Relay, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, domain, ok := splitAddr(email)
	if !ok {
		return Relay{}, ErrNotFound
	}
	r, ok := d.data.Relays[domain]
	if !ok {
		return Relay{}, ErrNotFound
	}
	return r, nil
}

func (d *Dev) Sender(_ context.Context, user, email string) (Sender, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// The dev directory's Senders map lists, per authenticated user, the
	// extra addresses that user may send as (delegation / send-as). The
	// user's own address is always allowed.
	if strings.EqualFold(user, email) {
		return Sender{Allowed: true, Addresses: []string{email}}, nil
	}
	if addrs, ok := d.data.Senders[user]; ok {
		for _, a := range addrs {
			if strings.EqualFold(a, email) {
				return Sender{Allowed: true, Addresses: []string{email}}, nil
			}
		}
	}
	return Sender{}, ErrNotFound
}

func (d *Dev) SenderRate(_ context.Context, email string) (SenderRate, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if r, ok := d.data.Rates[email]; ok {
		return r, nil
	}
	if _, ok := d.data.Users[email]; ok {
		return SenderRate{Allowed: true}, nil
	}
	return SenderRate{}, ErrNotFound
}

// SRSForward implements a dev-only, unsigned SRS scheme. It is deliberately
// non-interoperable: the real mode delegates to the mailez srsCodec via
// /stack/directory/srs/*. Local senders are not rewritten (404, as in mailez).
// The rewritten form is a valid SMTP path: SRS0=dev=<local>=<domain>@<host>.
func (d *Dev) SRSForward(_ context.Context, sender string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	local, domain, ok := splitAddr(sender)
	if !ok {
		return "", ErrNotFound
	}
	if d.localDomain(domain) {
		return "", ErrNotFound
	}
	host := ""
	for _, dom := range d.data.Domains {
		host = dom
		break
	}
	if host == "" {
		return "", ErrNotFound
	}
	return "SRS0=dev=" + local + "=" + domain + "@" + host, nil
}

func (d *Dev) SRSRestore(_ context.Context, recipient string) (string, error) {
	const prefix = "SRS0=dev="
	if !strings.HasPrefix(recipient, prefix) {
		return "", ErrNotFound
	}
	body := recipient
	if at := strings.LastIndex(recipient, "@"); at >= 0 {
		body = recipient[:at] // strip the hosting domain part
	}
	rest := strings.TrimPrefix(body, prefix)
	parts := strings.SplitN(rest, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ErrNotFound
	}
	return parts[0] + "@" + parts[1], nil
}

func (d *Dev) Quota(_ context.Context, email string) (Quota, error) {
	u, err := d.User(context.Background(), email)
	if err != nil {
		return Quota{}, err
	}
	return Quota{Limit: u.QuotaBytes, Used: u.QuotaBytesUsed}, nil
}

func (d *Dev) UpdateQuotaUsed(_ context.Context, email string, used int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.data.Users[email]
	if !ok {
		return ErrNotFound
	}
	u.QuotaBytesUsed = used
	d.data.Users[email] = u
	return nil
}

func (d *Dev) Sieve(_ context.Context, email string) (SieveScript, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if _, ok := d.data.Users[email]; !ok {
		return SieveScript{}, ErrNotFound
	}
	if script, ok := d.data.Sieve[email]; ok {
		return SieveScript{Name: "default", Script: script}, nil
	}
	return SieveScript{Name: "default", Script: "keep;"}, nil
}

func (d *Dev) Close() error { return nil }

func (d *Dev) localDomain(domain string) bool {
	for _, dom := range d.data.Domains {
		if strings.EqualFold(dom, domain) {
			return true
		}
	}
	return false
}

func splitAddr(addr string) (local, domain string, ok bool) {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		// Bare domain (post-office delivery).
		if addr == "" {
			return "", "", false
		}
		return "", addr, true
	}
	return addr[:at], addr[at+1:], true
}

var _ Service = (*Dev)(nil)
