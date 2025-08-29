package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"mailezine/internal/maildns"
	"mailezine/internal/mailmtasts"
	"mailezine/internal/mailsmtp"
	"mailezine/internal/testdns"
)

func TestOutboundTLSPolicy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	t.Run("no resolver is opportunistic", func(t *testing.T) {
		mode, dane, err := outboundTLSPolicy(ctx, nil, nil, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeOpportunistic || dane != nil {
			t.Fatalf("mode=%v dane=%v err=%v", mode, dane, err)
		}
	})

	t.Run("no records is opportunistic", func(t *testing.T) {
		r := &testdns.Resolver{}
		mode, dane, err := outboundTLSPolicy(ctx, r, nil, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeOpportunistic || len(dane) != 0 {
			t.Fatalf("mode=%v dane=%v err=%v", mode, dane, err)
		}
	})

	t.Run("tlsa records require tls", func(t *testing.T) {
		r := &testdns.Resolver{
			TLSA: map[string][]maildns.TLSA{
				"mx.example.com": {{Usage: 3, Selector: 1, MatchingType: 1, Cert: make([]byte, 32)}},
			},
		}
		mode, dane, err := outboundTLSPolicy(ctx, r, nil, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeRequired || len(dane) != 1 {
			t.Fatalf("mode=%v dane=%v err=%v", mode, dane, err)
		}
	})

	t.Run("unvalidated tlsa falls back to opportunistic", func(t *testing.T) {
		// RFC 7672 §5: TLSA records from an answer without the DNSSEC
		// authenticated-data bit are not actionable — enforcing them would
		// let an on-path spoofer pin (or break) transport.
		r := &testdns.Resolver{
			TLSA: map[string][]maildns.TLSA{
				"mx.example.com": {{Usage: 3, Selector: 1, MatchingType: 1, Cert: make([]byte, 32)}},
			},
			TLSAValidated: map[string]bool{"mx.example.com": false},
		}
		mode, dane, err := outboundTLSPolicy(ctx, r, nil, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeOpportunistic || len(dane) != 0 {
			t.Fatalf("mode=%v dane=%v err=%v", mode, dane, err)
		}
	})

	t.Run("ip host skips dane", func(t *testing.T) {
		r := &testdns.Resolver{}
		mode, _, err := outboundTLSPolicy(ctx, r, nil, logger, "example.com", "192.0.2.10")
		if err != nil || mode != mailsmtp.TLSModeOpportunistic {
			t.Fatalf("mode=%v err=%v", mode, err)
		}
	})

	t.Run("mtasts enforce match requires tls", func(t *testing.T) {
		r := &testdns.Resolver{}
		msts := &fakeMTSTS{policy: &mailmtasts.Policy{Mode: mailmtasts.ModeEnforce, MX: []string{"mx.example.com"}}}
		mode, dane, err := outboundTLSPolicy(ctx, r, msts, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeRequired || len(dane) != 0 {
			t.Fatalf("mode=%v dane=%v err=%v", mode, dane, err)
		}
	})

	t.Run("mtasts enforce mismatch is permanent", func(t *testing.T) {
		r := &testdns.Resolver{}
		msts := &fakeMTSTS{policy: &mailmtasts.Policy{Mode: mailmtasts.ModeEnforce, MX: []string{"mx.other.com"}}}
		mode, _, err := outboundTLSPolicy(ctx, r, msts, logger, "example.com", "mx.example.com")
		if err == nil || mode != mailsmtp.TLSModeOpportunistic {
			t.Fatalf("mode=%v err=%v", mode, err)
		}
	})

	t.Run("mtasts testing is opportunistic", func(t *testing.T) {
		r := &testdns.Resolver{}
		msts := &fakeMTSTS{policy: &mailmtasts.Policy{Mode: mailmtasts.ModeTesting, MX: []string{"mx.example.com"}}}
		mode, _, err := outboundTLSPolicy(ctx, r, msts, logger, "example.com", "mx.example.com")
		if err != nil || mode != mailsmtp.TLSModeOpportunistic {
			t.Fatalf("mode=%v err=%v", mode, err)
		}
	})
}

// fakeMTSTS is a scripted MTA-STS source.
type fakeMTSTS struct {
	policy *mailmtasts.Policy
	err    error
}

func (f *fakeMTSTS) Lookup(_ context.Context, _ string) (*mailmtasts.Policy, error) {
	return f.policy, f.err
}
