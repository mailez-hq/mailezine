// Package spf implements Sender Policy Framework checking (RFC 7208): the
// classic mechanism set (all, ip4, ip6, a, mx, ptr, exists, include), the
// redirect modifier, macro expansion, and the DNS lookup budget. It is a
// from-scratch implementation on the engine's maildns.Resolver, with every
// DNS seam injectable so tests exercise the full decision tree locally.
package spf

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"mailezine/internal/maildns"
)

// Result is the RFC 7208 §2.6 result set.
type Result string

const (
	ResultNone      Result = "none"      // no SPF record
	ResultNeutral   Result = "neutral"   // explicit ?all or no match
	ResultPass      Result = "pass"      // authorized
	ResultFail      Result = "fail"      // explicit -all (or include fail)
	ResultSoftFail  Result = "softfail"  // explicit ~all
	ResultTempError Result = "temperror" // DNS failure
	ResultPermError Result = "permerror" // malformed record or limits exceeded
)

const (
	maxDNSLookups   = 10
	maxIncludeDepth = 20
)

// Check evaluates SPF for the RFC5321.MailFrom identity. helo is the client
// HELO/EHLO name (used by macro expansion only; the HELO identity itself is
// not checked, matching the engine's documented limitation).
func Check(ctx context.Context, resolver maildns.Resolver, ip net.IP, domain, localpart, helo string) Result {
	if resolver == nil {
		return ResultTempError
	}
	e := &eval{
		ctx:          ctx,
		r:            resolver,
		ip:           ip,
		helo:         helo,
		localpart:    localpart,
		senderDomain: domain,
	}
	return e.checkDomain(domain)
}

type eval struct {
	ctx          context.Context
	r            maildns.Resolver
	ip           net.IP
	helo         string
	localpart    string
	senderDomain string
	lookups      int
	depth        int
}

func (e *eval) checkDomain(domain string) Result {
	records, err := e.r.LookupTXT(e.ctx, domain)
	if err != nil {
		return ResultTempError
	}
	var rec string
	for _, txt := range records {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(txt)), "v=spf1") {
			continue
		}
		if rec != "" {
			return ResultPermError // multiple SPF records
		}
		rec = txt
	}
	if rec == "" {
		return ResultNone
	}
	return e.evalRecord(rec, domain)
}

// evalRecord evaluates one SPF record against the current identity.
func (e *eval) evalRecord(record, domain string) Result {
	terms := strings.Fields(record)
	if len(terms) == 0 || !strings.EqualFold(terms[0], "v=spf1") {
		return ResultPermError
	}
	result := ResultNeutral
	for _, term := range terms[1:] {
		if term == "" {
			continue
		}
		if isModifier(term) {
			key, target, _ := strings.Cut(term, "=")
			if strings.EqualFold(key, "redirect") {
				if target == "" {
					return ResultPermError
				}
				// The redirect is only honored when no earlier mechanism
				// matched (the result is still neutral at this point).
				if result == ResultNeutral {
					expanded, ok := e.expand(target, domain, false)
					if !ok {
						return ResultPermError
					}
					e.depth++
					if e.depth > maxIncludeDepth {
						return ResultPermError
					}
					if !e.budget() {
						return ResultPermError
					}
					res := e.checkDomain(expanded)
					e.depth--
					return res
				}
			}
			// exp= and unknown modifiers are ignored.
			continue
		}
		match, res, permErr := e.evalMechanism(term, domain)
		if permErr != nil {
			return ResultPermError
		}
		if res != "" {
			return res // include:fail short-circuits
		}
		if match {
			result = res
			break
		}
	}
	return result
}

// evalMechanism evaluates one mechanism term. match reports a hit; res is
// set for include:fail (propagated immediately); permErr reports malformed
// terms or budget exhaustion.
func (e *eval) evalMechanism(term, domain string) (match bool, res Result, permErr error) {
	qualifier := byte('+')
	body := term
	if len(term) > 1 && strings.ContainsRune("+-~?", rune(term[0])) {
		qualifier = term[0]
		body = term[1:]
	}
	mech, arg, hasArg := strings.Cut(body, ":")
	lowerMech := strings.ToLower(mech)
	if hasArg && (lowerMech == "a" || lowerMech == "mx" || lowerMech == "ptr" ||
		lowerMech == "exists" || lowerMech == "include" || lowerMech == "redirect") {
		expanded, ok := e.expand(arg, domain, true)
		if !ok {
			return false, "", fmt.Errorf("spf: macro expansion failed in %q", term)
		}
		arg = expanded
	}

	hit := false
	switch lowerMech {
	case "all":
		hit = true
	case "ip4", "ip6":
		pfx, err := parsePrefix(arg)
		if err != nil {
			return false, "", fmt.Errorf("spf: bad %s prefix %q: %w", lowerMech, arg, err)
		}
		ipA, ok := netip.AddrFromSlice(e.ip)
		if !ok {
			return false, "", fmt.Errorf("spf: bad peer ip %q", e.ip)
		}
		hit = pfx.Contains(ipA.Unmap())
	case "a":
		if !e.budget() {
			return false, "", fmt.Errorf("spf: DNS lookup budget exceeded")
		}
		target := domain
		cidr := -1
		if hasArg {
			target, cidr = splitCidr(arg)
			if target == "" {
				return false, "", fmt.Errorf("spf: empty a target")
			}
		}
		hit = e.ipInAddrs(target, cidr)
	case "mx":
		if !e.budget() {
			return false, "", fmt.Errorf("spf: DNS lookup budget exceeded")
		}
		target := domain
		cidr := -1
		if hasArg {
			target, cidr = splitCidr(arg)
			if target == "" {
				return false, "", fmt.Errorf("spf: empty mx target")
			}
		}
		hit = e.ipInMX(target, cidr)
	case "ptr":
		if !e.budget() {
			return false, "", fmt.Errorf("spf: DNS lookup budget exceeded")
		}
		target := domain
		if hasArg {
			target = arg
			if target == "" {
				return false, "", fmt.Errorf("spf: empty ptr target")
			}
		}
		hit = e.ptrMatches(target)
	case "exists":
		if !e.budget() {
			return false, "", fmt.Errorf("spf: DNS lookup budget exceeded")
		}
		if arg == "" {
			return false, "", fmt.Errorf("spf: exists without target")
		}
		ips, err := e.r.LookupIPAddr(e.ctx, arg)
		if err == nil {
			hit = len(ips) > 0
		} else if isNotFound(err) {
			hit = false
		} else {
			// DNS failure on exists is a temperror per RFC 7208 §5.7.
			return false, ResultTempError, nil
		}
	case "include":
		if !e.budget() {
			return false, "", fmt.Errorf("spf: DNS lookup budget exceeded")
		}
		if arg == "" {
			return false, "", fmt.Errorf("spf: include without target")
		}
		e.depth++
		sub := e.checkDomain(arg)
		e.depth--
		switch sub {
		case ResultPass:
			// RFC 7208 §5.2: include:pass matches with a pass result.
			return true, ResultPass, nil
		case ResultFail:
			// include:fail does not match but forces the overall result.
			return false, ResultFail, nil
		case ResultTempError:
			return false, ResultTempError, nil
		case ResultPermError:
			return false, ResultPermError, nil
		default:
			// neutral/softfail: not a match; evaluation continues.
		}
	default:
		return false, "", fmt.Errorf("spf: unknown mechanism %q", term)
	}
	// A hit carries its qualified result; a miss carries "". Non-empty res
	// is also used by the include short-circuit (fail/temperror) above.
	if hit {
		return true, resultForQualifier(qualifier), nil
	}
	return false, "", nil
}

func (e *eval) ipInAddrs(host string, cidr int) bool {
	ips, err := e.r.LookupIPAddr(e.ctx, host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if e.ipWithin(ip.IP, cidr) {
			return true
		}
	}
	return false
}

func (e *eval) ipInMX(domain string, cidr int) bool {
	mxs, err := e.r.LookupMX(e.ctx, domain)
	if err != nil {
		return false
	}
	for _, mx := range mxs {
		if !e.budget() {
			return false
		}
		if e.ipInAddrs(strings.TrimSuffix(mx.Host, "."), cidr) {
			return true
		}
	}
	// RFC 7208 §5.4: the domain itself is also an implicit MX target.
	if !e.budget() {
		return false
	}
	return e.ipInAddrs(domain, cidr)
}

func (e *eval) ptrMatches(domain string) bool {
	names, err := e.r.LookupAddr(e.ctx, e.ip.String())
	if err != nil {
		return false
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if name != domain && !strings.HasSuffix(name, "."+domain) {
			continue
		}
		if !e.budget() {
			return false
		}
		// Forward confirmation: the name must resolve back to the IP.
		if e.ipInAddrs(name, -1) {
			return true
		}
	}
	return false
}

// ipWithin reports whether ip falls inside the /cidr network around the
// given address (cidr == -1 means exact equality).
func (e *eval) ipWithin(addr net.IP, cidr int) bool {
	ipA, ok := netip.AddrFromSlice(e.ip)
	if !ok {
		return false
	}
	ipB, ok := netip.AddrFromSlice(addr)
	if !ok {
		return false
	}
	if cidr < 0 {
		return ipA == ipB
	}
	if cidr > ipB.BitLen() {
		return false
	}
	return netip.PrefixFrom(ipB.Unmap(), cidr).Contains(ipA.Unmap())
}

func (e *eval) budget() bool {
	e.lookups++
	return e.lookups <= maxDNSLookups
}

func resultForQualifier(q byte) Result {
	switch q {
	case '-':
		return ResultFail
	case '~':
		return ResultSoftFail
	case '?':
		return ResultNeutral
	default:
		return ResultPass
	}
}

func isModifier(term string) bool {
	if i := strings.IndexByte(term, '='); i > 0 {
		switch strings.ToLower(term[:i]) {
		case "redirect", "exp":
			return true
		}
	}
	return false
}

func parsePrefix(s string) (netip.Prefix, error) {
	if _, err := netip.ParsePrefix(s); err == nil {
		return netip.ParsePrefix(s)
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func splitCidr(s string) (host string, cidr int) {
	host, cidrStr, ok := strings.Cut(s, "/")
	if !ok {
		return s, -1
	}
	n, err := strconv.Atoi(cidrStr)
	if err != nil || n < 0 || n > 128 {
		return "", -1
	}
	return host, n
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// expand performs RFC 7208 §7 macro expansion. domain is the current
// "d" macro value; ipOnly requests a domain-spec expansion (used for
// exists/redirect/include), where macros %{p}, %{r}, %{c} are rejected.
func (e *eval) expand(s, domain string, ipOnly bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			return "", false
		}
		next := s[i+1]
		switch next {
		case '%':
			b.WriteByte('%')
			i++
			continue
		case '_':
			b.WriteByte(' ')
			i++
			continue
		case '-':
			b.WriteString("%20")
			i++
			continue
		}
		if next != '{' {
			return "", false
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", false
		}
		body := s[i+2 : i+2+end]
		val, ok := e.expandMacro(body, domain, ipOnly)
		if !ok {
			return "", false
		}
		b.WriteString(val)
		i = i + 2 + end
	}
	return b.String(), true
}

func (e *eval) expandMacro(body, domain string, ipOnly bool) (string, bool) {
	macro := body
	var transform string
	if i := strings.IndexAny(body, "0123456789r-"); i >= 0 {
		macro = body[:i]
		transform = body[i:]
	}
	if len(macro) != 1 {
		return "", false
	}
	var val string
	switch macro {
	case "s":
		val = e.localpart + "@" + e.senderDomain
	case "l":
		val = e.localpart
	case "o":
		val = e.senderDomain
	case "d":
		val = domain
	case "i":
		val = e.ip.String()
	case "h":
		val = e.helo
	case "v":
		if e.ip.To4() != nil {
			val = "in-addr"
		} else {
			val = "ip6"
		}
	case "p", "r":
		if ipOnly {
			return "", false
		}
		names, err := e.r.LookupAddr(e.ctx, e.ip.String())
		if err == nil && len(names) > 0 {
			val = names[0]
		} else {
			val = "unknown"
		}
	case "c":
		val = "unknown" // country code requires geo data; RFC allows unknown
	case "t":
		val = strconv.FormatInt(time.Now().Unix(), 10)
	case "x":
		val = base64.RawURLEncoding.EncodeToString(e.ip)
	default:
		return "", false
	}
	return applyTransform(val, transform), true
}

// applyTransform applies RFC 7208 §7.2 part transforms (N, N-, -N, r).
func applyTransform(s, t string) string {
	if t == "" {
		return s
	}
	parts := strings.Split(s, ".")
	if strings.HasSuffix(t, "r") {
		t = strings.TrimSuffix(t, "r")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
	}
	switch {
	case t == "":
		return strings.Join(parts, ".")
	case strings.HasSuffix(t, "-"):
		n, err := strconv.Atoi(strings.TrimSuffix(t, "-"))
		if err != nil || n <= 0 || n > len(parts) {
			return s
		}
		return strings.Join(parts[n-1:], ".")
	case strings.HasPrefix(t, "-"):
		n, err := strconv.Atoi(strings.TrimPrefix(t, "-"))
		if err != nil || n <= 0 || n > len(parts) {
			return s
		}
		return strings.Join(parts[len(parts)-n:], ".")
	default:
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 || n > len(parts) {
			return s
		}
		return strings.Join(parts[:n], ".")
	}
}
