package junk

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// checkRBLs queries the configured DNSBL zones for the peer IP and returns
// the zones that list it. Results are cached: hits for dnsblHitTTL, clean
// answers for dnsblCleanTTL, so a busy MX does not hammer the zones. All
// uncached zones are queried concurrently; the first error is reported but
// partial hits still count.
func (c *Classifier) checkRBLs(ctx context.Context, peer net.IP) ([]string, error) {
	if len(c.cfg.RBLs) == 0 || peer == nil {
		return nil, nil
	}
	ip4 := peer.To4()
	if ip4 == nil {
		// The widely used DNSBL zones only carry IPv4 data.
		return nil, nil
	}
	now := time.Now()

	var toQuery []string
	var hits []string
	c.rblMu.Lock()
	for _, zone := range c.cfg.RBLs {
		q := queryName(ip4, zone)
		if e, ok := c.rblCache[q]; ok && now.Before(e.until) {
			if e.hit {
				hits = append(hits, zone)
			}
			continue
		}
		delete(c.rblCache, q)
		toQuery = append(toQuery, zone)
	}
	c.rblMu.Unlock()

	type answer struct {
		zone string
		hit  bool
		err  error
	}
	answers := make(chan answer, len(toQuery))
	var wg sync.WaitGroup
	for _, zone := range toQuery {
		wg.Add(1)
		go func(zone string) {
			defer wg.Done()
			q := queryName(ip4, zone)
			cctx, cancel := context.WithTimeout(ctx, defaultRBLTimeout)
			defer cancel()
			addrs, err := c.resolver.LookupIPAddr(cctx, q)
			a := answer{zone: zone, hit: len(addrs) > 0, err: err}
			if err != nil && isNotFound(err) {
				// NXDOMAIN-style clean answers surface as errors on some
				// resolvers; for DNSBL semantics they mean "not listed".
				a.err = nil
				a.hit = false
			}
			answers <- a
		}(zone)
	}
	wg.Wait()
	close(answers)

	var firstE error
	for a := range answers {
		if a.err != nil {
			if firstE == nil {
				firstE = a.err
			}
			continue // don't cache failures: retry next mail
		}
		if a.hit {
			hits = append(hits, a.zone)
		}
		ttl := dnsblCleanTTL
		if a.hit {
			ttl = dnsblHitTTL
		}
		c.rblMu.Lock()
		c.rblCache[queryName(ip4, a.zone)] = dnsblEntry{hit: a.hit, until: now.Add(ttl)}
		c.rblMu.Unlock()
	}
	return hits, firstE
}

// queryName builds the standard reversed-octet DNSBL query.
func queryName(ip4 net.IP, zone string) string {
	b := ip4.To4()
	parts := [4]string{}
	for i := 0; i < 4; i++ {
		parts[3-i] = itoa(int(b[i]))
	}
	return strings.Join(parts[:], ".") + "." + zone
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// isNotFound reports whether err is a DNS name-not-found answer, which for
// DNSBL semantics means "not listed".
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nxdomain") || strings.Contains(msg, "no such host")
}
