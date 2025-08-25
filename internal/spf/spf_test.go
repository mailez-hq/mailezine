package spf

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"mailezine/internal/testdns"
)

func check(t *testing.T, rec string, ip net.IP, dns map[string][]string) Result {
	t.Helper()
	r := &testdns.Resolver{TXT: map[string][]string{"example.com.": {rec}}}
	for k, v := range dns {
		r.TXT[k] = v
	}
	return Check(context.Background(), r, ip, "example.com", "alice", "mail.example.com")
}

func TestBasicResults(t *testing.T) {
	ip := net.ParseIP("192.0.2.10")
	if got := check(t, "v=spf1 -all", ip, nil); got != ResultFail {
		t.Fatalf("-all = %s", got)
	}
	if got := check(t, "v=spf1 +all", ip, nil); got != ResultPass {
		t.Fatalf("+all = %s", got)
	}
	if got := check(t, "v=spf1 ~all", ip, nil); got != ResultSoftFail {
		t.Fatalf("~all = %s", got)
	}
	if got := check(t, "v=spf1 ?all", ip, nil); got != ResultNeutral {
		t.Fatalf("?all = %s", got)
	}
}

func TestIPMechanisms(t *testing.T) {
	ip := net.ParseIP("192.0.2.10")
	if got := check(t, "v=spf1 ip4:192.0.2.0/24 -all", ip, nil); got != ResultPass {
		t.Fatalf("ip4 cidr = %s", got)
	}
	if got := check(t, "v=spf1 ip4:198.51.100.1 -all", ip, nil); got != ResultFail {
		t.Fatalf("ip4 exact miss = %s", got)
	}
	if got := check(t, "v=spf1 ip6:2001:db8::/32 -all", net.ParseIP("2001:db8::1"), nil); got != ResultPass {
		t.Fatalf("ip6 = %s", got)
	}
}

func TestAMechanism(t *testing.T) {
	ip := net.ParseIP("203.0.113.7")
	r := &testdns.Resolver{
		TXT: map[string][]string{"example.com.": {"v=spf1 a -all"}},
		IPs: map[string][]net.IPAddr{
			"example.com.": {{IP: net.ParseIP("203.0.113.7")}},
		},
	}
	if got := Check(context.Background(), r, ip, "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("a = %s", got)
	}
	// The resolver host keys here use the trailing-dot form; lookup uses
	// the raw target, so also verify the no-dot form works.
	r.IPs["example.com"] = r.IPs["example.com."]
	delete(r.IPs, "example.com.")
	if got := Check(context.Background(), r, ip, "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("a (no dot) = %s", got)
	}
}

func TestMXMechanism(t *testing.T) {
	r := &testdns.Resolver{
		TXT: map[string][]string{"example.com.": {"v=spf1 mx -all"}},
		MX:  map[string][]*net.MX{"example.com.": {{Host: "mx.example.com."}}},
		IPs: map[string][]net.IPAddr{
			"mx.example.com": {{IP: net.ParseIP("203.0.113.9")}},
		},
	}
	if got := Check(context.Background(), r, net.ParseIP("203.0.113.9"), "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("mx = %s", got)
	}
}

func TestIncludeAndRedirect(t *testing.T) {
	r := &testdns.Resolver{
		TXT: map[string][]string{
			"example.com.":      {"v=spf1 include:_spf.example.net -all"},
			"_spf.example.net.": {"v=spf1 ip4:192.0.2.10 -all"},
		},
	}
	if got := Check(context.Background(), r, net.ParseIP("192.0.2.10"), "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("include pass = %s", got)
	}
	if got := Check(context.Background(), r, net.ParseIP("192.0.2.99"), "example.com", "a", "h"); got != ResultFail {
		t.Fatalf("include fail = %s", got)
	}

	r2 := &testdns.Resolver{
		TXT: map[string][]string{
			"example.com.": {"v=spf1 redirect=example.net"},
			"example.net.": {"v=spf1 ip4:192.0.2.10 -all"},
		},
	}
	if got := Check(context.Background(), r2, net.ParseIP("192.0.2.10"), "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("redirect = %s", got)
	}
}

func TestExistsAndMacro(t *testing.T) {
	r := &testdns.Resolver{
		TXT: map[string][]string{
			"example.com.": {"v=spf1 exists:%{i}.%{d} -all"},
		},
		IPs: map[string][]net.IPAddr{
			"192.0.2.10.example.com": {{IP: net.ParseIP("192.0.2.1")}},
		},
	}
	if got := Check(context.Background(), r, net.ParseIP("192.0.2.10"), "example.com", "alice", "h"); got != ResultPass {
		t.Fatalf("exists macro = %s", got)
	}
}

func TestPTRMechanism(t *testing.T) {
	r := &testdns.Resolver{
		TXT: map[string][]string{"example.com.": {"v=spf1 ptr -all"}},
		PTR: map[string][]string{"192.0.2.10": {"host.example.com."}},
		IPs: map[string][]net.IPAddr{
			"host.example.com": {{IP: net.ParseIP("192.0.2.10")}},
		},
	}
	if got := Check(context.Background(), r, net.ParseIP("192.0.2.10"), "example.com", "a", "h"); got != ResultPass {
		t.Fatalf("ptr = %s", got)
	}
}

func TestErrorsAndLimits(t *testing.T) {
	ip := net.ParseIP("192.0.2.10")
	if got := check(t, "v=spf1 bogus -all", ip, nil); got != ResultPermError {
		t.Fatalf("unknown mech = %s", got)
	}
	if got := check(t, "v=spf1 ip4:not-an-ip -all", ip, nil); got != ResultPermError {
		t.Fatalf("bad ip4 = %s", got)
	}
	if got := check(t, "not spf", ip, nil); got != ResultNone {
		t.Fatalf("no record = %s", got)
	}
	// Multiple SPF records → permerror.
	r := &testdns.Resolver{TXT: map[string][]string{
		"example.com.": {"v=spf1 -all", "v=spf1 +all"},
	}}
	if got := Check(context.Background(), r, ip, "example.com", "a", "h"); got != ResultPermError {
		t.Fatalf("multi record = %s", got)
	}
	// DNS failure → temperror.
	if got := Check(context.Background(), &testdns.Resolver{}, ip, "example.com", "a", "h"); got != ResultTempError {
		t.Fatalf("dns failure = %s", got)
	}
}

func TestLookupBudget(t *testing.T) {
	// 12 includes force the budget over the limit.
	var rec strings.Builder
	rec.WriteString("v=spf1")
	for i := 0; i < 12; i++ {
		rec.WriteString(" include:spf" + strconv.Itoa(i) + ".example.net")
	}
	rec.WriteString(" -all")
	dns := map[string][]string{"example.com.": {rec.String()}}
	for i := 0; i < 12; i++ {
		dns["spf"+strconv.Itoa(i)+".example.net."] = []string{"v=spf1 ?all"}
	}
	if got := check(t, rec.String(), net.ParseIP("192.0.2.10"), dns); got != ResultPermError {
		t.Fatalf("budget = %s", got)
	}
}
