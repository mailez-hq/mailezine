package junk

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"mailezine/internal/delivery"
)

// fakeResolver answers DNSBL queries from a map; anything absent is NXDOMAIN.
type fakeResolver struct {
	answers map[string][]net.IPAddr
	err     error
	queries []string
}

func (f *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	f.queries = append(f.queries, host)
	if f.err != nil {
		return nil, f.err
	}
	if addrs, ok := f.answers[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func newTest(t *testing.T, cfg Config, r Resolver) *Classifier {
	t.Helper()
	c := New(r, cfg, nil)
	if c == nil {
		t.Fatal("nil classifier")
	}
	return c
}

const arPass = "Authentication-Results: mx.example.com; spf=pass smtp.mailfrom=corp.example; dkim=pass header.d=corp.example; dmarc=pass (p=none; dis=none)"

func TestClassifyAuthResults_CleanMailPasses(t *testing.T) {
	c := newTest(t, Config{}, &fakeResolver{})
	res, err := c.ClassifyAuthResults(context.Background(), arPass, net.ParseIP("203.0.113.5"), "peer@corp.example", []string{"user@example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "no action" {
		t.Fatalf("action = %q, want no action (%+v)", res.Action, res)
	}
	if len(res.Headers) != 0 {
		t.Fatalf("unexpected headers %v", res.Headers)
	}
}

func TestClassifyAuthResults_FailuresAccumulateToHeader(t *testing.T) {
	c := newTest(t, Config{}, &fakeResolver{})
	ar := "Authentication-Results: mx; spf=fail smtp.mailfrom=spam.example; dkim=fail header.d=spam.example; dmarc=fail (p=quarantine)"
	res, _ := c.ClassifyAuthResults(context.Background(), ar, net.ParseIP("203.0.113.9"), "x@spam.example", []string{"u@example.com"}, nil)
	// 3 + 3 + 2 = 8 >= 4.5 -> add header, below 12 -> no reject.
	if res.Action != "add header" {
		t.Fatalf("action = %q, want add header (%+v)", res.Action, res)
	}
	if !headersContain(res.Headers, "X-Spam-Flag: YES") {
		t.Fatalf("missing X-Spam-Flag in %v", res.Headers)
	}
}

func TestClassifyAuthResults_DMARCRejectPolicyIsHard(t *testing.T) {
	c := newTest(t, Config{}, &fakeResolver{})
	ar := "Authentication-Results: mx; spf=fail smtp.mailfrom=evil.example; dmarc=fail (p=reject)"
	res, _ := c.ClassifyAuthResults(context.Background(), ar, net.ParseIP("203.0.113.9"), "x@evil.example", []string{"u@example.com"}, nil)
	// 3 + 4 = 7 < 12: still only a header. DMARC p=reject failure alone must
	// not reject at the MTA without corroboration, but must flag clearly.
	if res.Action != "add header" {
		t.Fatalf("action = %q (%+v)", res.Action, res)
	}
	if !headersContain(res.Headers, "dmarc=fail (p=reject) (4.0)") {
		t.Fatalf("report missing dmarc line: %v", res.Headers)
	}
}

func TestClassifyAuthResults_WorstResultMerges(t *testing.T) {
	// Two dkim segments: fail + pass must merge to a single dkim=fail (+3),
	// not stack; spf pass adds nothing. Total stays below the header bar.
	c := newTest(t, Config{}, &fakeResolver{})
	ar := "Authentication-Results: mx; dkim=fail header.d=one.example; dkim=pass header.d=two.example; spf=pass smtp.mailfrom=a.example"
	res, _ := c.ClassifyAuthResults(context.Background(), ar, net.ParseIP("203.0.113.5"), "x@a.example", []string{"u@example.com"}, nil)
	if res.Action != "no action" {
		t.Fatalf("action = %q, want no action (%+v)", res.Action, res)
	}
}

func TestDenyListRejects_WhitelistWins(t *testing.T) {
	r := &fakeResolver{}
	c := newTest(t, Config{
		Blacklist: []string{"bad.example", "@evil.example", "user@spam.example"},
		Whitelist: []string{"partner.example"},
	}, r)
	for _, from := range []string{"a@bad.example", "x@evil.example", "user@spam.example"} {
		res, _ := c.ClassifyAuthResults(context.Background(), "", net.ParseIP("203.0.113.9"), from, []string{"u@example.com"}, nil)
		if res.Action != "reject" {
			t.Fatalf("%s: action = %q, want reject", from, res.Action)
		}
	}
	for _, from := range []string{"boss@partner.example", "a@sub.partner.example"} {
		res, _ := c.ClassifyAuthResults(context.Background(), "", net.ParseIP("203.0.113.9"), from, []string{"u@example.com"}, nil)
		if res.Action != "no action" {
			t.Fatalf("%s: whitelisted but action = %q", from, res.Action)
		}
	}
}

func TestRBLHitScoresAndCaches(t *testing.T) {
	r := &fakeResolver{answers: map[string][]net.IPAddr{
		"9.113.0.203.zen.spamhaus.org": {{IP: net.ParseIP("127.0.0.4")}},
	}}
	c := newTest(t, Config{RBLs: []string{"zen.spamhaus.org"}}, r)
	peer := net.ParseIP("203.0.113.9")
	res, _ := c.Classify(context.Background(), peer, "x@unknown.example", []string{"u@example.com"}, nil)
	// 1 RBL hit = 3 < 4.5: no header yet, but the query must have happened.
	if res.Action != "no action" || res.Score != scoreRBLHit {
		t.Fatalf("action = %q score = %v, want no action/3", res.Action, res.Score)
	}
	c.Classify(context.Background(), peer, "x@unknown.example", []string{"u@example.com"}, nil)
	if len(r.queries) != 1 {
		t.Fatalf("expected 1 DNS query (cached on repeat), got %d", len(r.queries))
	}
	// Enough hits to reject: configure two zones that both list the peer.
	r2 := &fakeResolver{answers: map[string][]net.IPAddr{
		"9.113.0.203.a.dnsbl": {{IP: net.ParseIP("127.0.0.2")}},
		"9.113.0.203.b.dnsbl": {{IP: net.ParseIP("127.0.0.2")}},
	}}
	c2 := newTest(t, Config{RBLs: []string{"a.dnsbl", "b.dnsbl"}, RejectScore: 6}, r2)
	res, _ = c2.Classify(context.Background(), peer, "x@unknown.example", []string{"u@example.com"}, nil)
	if res.Action != "reject" {
		t.Fatalf("action = %q, want reject from double RBL (%+v)", res.Action, res)
	}
}

func TestRBLDNSFailureFailsOpen(t *testing.T) {
	r := &fakeResolver{err: fmt.Errorf("timeout")}
	c := newTest(t, Config{RBLs: []string{"a.dnsbl"}}, r)
	res, _ := c.Classify(context.Background(), net.ParseIP("203.0.113.9"), "x@x.example", []string{"u@example.com"}, nil)
	if res.Action != "no action" {
		t.Fatalf("DNS failure must fail open, got %q", res.Action)
	}
}

func TestGreylistFirstSeenThenPass(t *testing.T) {
	c := newTest(t, Config{Greylist: true, HeaderScore: 2}, &fakeResolver{})
	peer := net.ParseIP("203.0.113.9")
	ar := "Authentication-Results: mx; spf=softfail smtp.mailfrom=slow.example" // 1.5: ambiguous band
	res, _ := c.ClassifyAuthResults(context.Background(), ar, peer, "x@slow.example", []string{"u@example.com"}, nil)
	if res.Action != "greylist" {
		t.Fatalf("first attempt: action = %q, want greylist", res.Action)
	}
	res, _ = c.ClassifyAuthResults(context.Background(), ar, peer, "x@slow.example", []string{"u@example.com"}, nil)
	if res.Action != "no action" {
		t.Fatalf("retry: action = %q, want no action", res.Action)
	}
	// Clean senders are never greylisted.
	c2 := newTest(t, Config{Greylist: true}, &fakeResolver{})
	res, _ = c2.ClassifyAuthResults(context.Background(), arPass, peer, "x@corp.example", []string{"u@example.com"}, nil)
	if res.Action != "no action" {
		t.Fatalf("clean mail greylisted: %q", res.Action)
	}
}

func TestPlainClassifyWithoutAuthResults(t *testing.T) {
	c := newTest(t, Config{}, &fakeResolver{})
	res, err := c.Classify(context.Background(), net.ParseIP("203.0.113.5"), "x@x.example", []string{"u@example.com"}, []byte("data"))
	if err != nil || res.Action != "no action" {
		t.Fatalf("plain classify: %q %v", res.Action, err)
	}
}

func TestSatisfiesDeliveryClassifier(t *testing.T) {
	var c delivery.Classifier = New(nil, Config{}, nil)
	if _, err := c.Classify(context.Background(), net.ParseIP("203.0.113.5"), "x@x.example", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerdictHeadersShape(t *testing.T) {
	c := newTest(t, Config{}, &fakeResolver{})
	res := c.verdict(6, []hit{{"spf=fail", 3}, {"DNSBL:z", 3}})
	joined := strings.Join(res.Headers, "\n")
	for _, want := range []string{"X-Spam-Flag: YES", "X-Spam-Score: 6.0", "X-Spam-Level: ******", "spf=fail (3.0)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("headers missing %q:\n%s", want, joined)
		}
	}
}

func headersContain(headers []string, want string) bool {
	for _, h := range headers {
		if strings.Contains(h, want) {
			return true
		}
	}
	return false
}
