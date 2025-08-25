// Outbound TLS policy selection: MTA-STS first (enforce + MX match → TLS
// required; enforce + mismatch → permanent failure), then DANE (TLSA records
// present → TLS required with those records). Resolver/policy lookup
// failures fail open to opportunistic TLS (documented: a policy outage must
// not block mail). The policy seam uses the engine's own maildns.Resolver
// and a pluggable MTA-STS source, so tests exercise the full decision tree
// without network access.
package queue

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"mailezine/internal/maildns"
	"mailezine/internal/mailmtasts"
	"mailezine/internal/mailsmtp"
)

// mtastsSource is the MTA-STS policy lookup seam.
type mtastsSource interface {
	Lookup(ctx context.Context, domain string) (*mailmtasts.Policy, error)
}

func outboundTLSPolicy(ctx context.Context, resolver maildns.Resolver, msts mtastsSource, logger *slog.Logger, domain, host string) (mailsmtp.TLSMode, []maildns.TLSA, error) {
	if resolver == nil {
		return mailsmtp.TLSModeOpportunistic, nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	host = maildns.NormalizeDomain(host)

	// MTA-STS applies to the original recipient domain, not the MX host.
	if domain != "" && msts != nil {
		policy, err := msts.Lookup(ctx, domain)
		if err != nil {
			logger.Debug("queue: mta-sts lookup", "domain", domain, "err", err)
		} else if policy != nil && policy.Mode == mailmtasts.ModeEnforce {
			if !policy.Matches(host) {
				return mailsmtp.TLSModeOpportunistic, nil,
					fmt.Errorf("queue: mx %s does not match enforced mta-sts policy", host)
			}
			return mailsmtp.TLSModeRequired, nil, nil
		}
	}

	// DANE: TLSA records exist → require TLS and verify them.
	if net.ParseIP(host) == nil {
		tlsas, err := resolver.LookupTLSA(ctx, 25, "tcp", host)
		if err == nil && len(tlsas) > 0 {
			return mailsmtp.TLSModeRequired, tlsas, nil
		}
	}
	return mailsmtp.TLSModeOpportunistic, nil, nil
}
