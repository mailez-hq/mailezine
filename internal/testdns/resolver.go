// Package testdns provides a fake maildns.Resolver for tests. It serves
// TXT/MX/IP/TLSA records by exact name (TXT keys include the trailing dot,
// matching net.LookupTXT semantics) and fails every other lookup, which is
// enough for SPF/DKIM/DMARC verification and outbound TLS policy tests.
package testdns

import (
	"context"
	"net"
	"strings"

	"mailezine/internal/maildns"
)

// Resolver is a maildns.Resolver stub.
type Resolver struct {
	TXT  map[string][]string
	MX   map[string][]*net.MX
	IPs  map[string][]net.IPAddr
	PTR  map[string][]string
	TLSA map[string][]maildns.TLSA
}

func (r *Resolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r.TXT[name]; ok {
		return v, nil
	}
	if v, ok := r.TXT[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	if v, ok := r.TXT[name+"."]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no txt", Name: name, IsNotFound: true}
}

func (r *Resolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := r.MX[name]; ok {
		return v, nil
	}
	if v, ok := r.MX[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	if v, ok := r.MX[name+"."]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no mx", Name: name, IsNotFound: true}
}

func (r *Resolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if v, ok := r.IPs[host]; ok {
		return v, nil
	}
	if v, ok := r.IPs[strings.TrimSuffix(host, ".")]; ok {
		return v, nil
	}
	if v, ok := r.IPs[host+"."]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no address", Name: host, IsNotFound: true}
}

func (r *Resolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if v, ok := r.PTR[addr]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no ptr", Name: addr, IsNotFound: true}
}

func (r *Resolver) LookupTLSA(_ context.Context, _ int, _ string, host string) ([]maildns.TLSA, error) {
	if v, ok := r.TLSA[host]; ok {
		return v, nil
	}
	if v, ok := r.TLSA[strings.TrimSuffix(host, ".")]; ok {
		return v, nil
	}
	if v, ok := r.TLSA[host+"."]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no tlsa", Name: host, IsNotFound: true}
}
