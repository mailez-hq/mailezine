// Package verify implements the inbound authentication stage
// (ARCHITECTURE.md §4): SPF, DKIM and DMARC checks summarized in an
// Authentication-Results header (RFC 8601). SPF is the engine's own RFC
// 7208 implementation (internal/spf), DKIM uses go-msgauth, DMARC policy
// evaluation is engine-local, and the header is emitted with
// go-msgauth/authres.
package verify

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	gdkim "github.com/emersion/go-msgauth/dkim"

	"mailezine/internal/maildns"
	"mailezine/internal/spf"
)

// Verifier runs SPF + DKIM + DMARC on an inbound message.
type Verifier struct {
	Logger   *slog.Logger
	Resolver maildns.Resolver
	Hostname string // our hostname for the Authentication-Results header
	LocalIP  net.IP
}

// Verify returns the Authentication-Results header (ending in CRLF) for the
// message, or "" when the envelope is unusable.
func (v *Verifier) Verify(ctx context.Context, peer net.IP, from string, data []byte) (string, error) {
	if v.Logger == nil {
		v.Logger = slog.Default()
	}
	_, domain, ok := splitFrom(from)
	if !ok {
		return "", nil
	}
	var results []authres.Result

	// SPF (mail-from identity; the HELO identity is skipped because go-smtp
	// does not surface the EHLO name to the session — documented limitation).
	spfResult := spf.Check(ctx, v.Resolver, peer, domain, "", "")
	results = append(results, &authres.SPFResult{Value: authres.ResultValue(spfResult), From: domain})

	// DKIM.
	verifs, dkimErr := gdkim.VerifyWithOptions(bytes.NewReader(data), &gdkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			return v.Resolver.LookupTXT(ctx, name)
		},
		MaxVerifications: 20,
	})
	dkimPassDomain := ""
	if dkimErr != nil {
		results = append(results, &authres.DKIMResult{Value: "none", Reason: dkimErr.Error()})
	} else if len(verifs) == 0 {
		results = append(results, &authres.DKIMResult{Value: "none", Reason: "no dkim signatures"})
	}
	for _, ver := range verifs {
		r := &authres.DKIMResult{Domain: ver.Domain, Identifier: ver.Identifier}
		switch {
		case ver.Err == nil:
			r.Value = "pass"
			dkimPassDomain = ver.Domain
		case gdkim.IsTempFail(ver.Err):
			r.Value = "temperror"
			r.Reason = ver.Err.Error()
		case gdkim.IsPermFail(ver.Err):
			r.Value = "permerror"
			r.Reason = ver.Err.Error()
		default:
			r.Value = "fail"
			r.Reason = ver.Err.Error()
		}
		results = append(results, r)
	}

	// DMARC (RFC 7489 policy evaluation against the RFC5322.From domain).
	dmarcUse, dmarcValue := evaluateDMARC(ctx, v.Resolver, domain, spfResult, dkimPassDomain, data)
	if dmarcUse {
		results = append(results, &authres.DMARCResult{Value: dmarcValue, From: fromDomain(data)})
	}

	return "Authentication-Results: " + authres.Format(v.Hostname, results) + "\r\n", nil
}

func splitFrom(addr string) (local, domain string, ok bool) {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", "", false
	}
	return addr[:at], addr[at+1:], true
}

// fromDomain extracts the domain of the first RFC5322.From address.
func fromDomain(data []byte) string {
	var hdr []byte
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		hdr = data[:i]
	} else if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		hdr = data[:i]
	} else {
		hdr = data
	}
	from := ""
	for _, line := range strings.Split(string(hdr), "\n") {
		line = strings.TrimRight(line, "\r")
		if from == "" {
			if strings.HasPrefix(strings.ToLower(line), "from:") {
				from = strings.TrimSpace(line[len("from:"):])
				continue
			}
			continue
		}
		// Folded continuation lines belong to the From field.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			from += " " + strings.TrimSpace(line)
			continue
		}
		break
	}
	if from == "" {
		return ""
	}
	if i := strings.Index(from, "<"); i >= 0 {
		from = from[i+1:]
		if j := strings.Index(from, ">"); j >= 0 {
			from = from[:j]
		}
	} else {
		fields := strings.Fields(from)
		if len(fields) > 0 {
			from = strings.Trim(fields[0], "\"'")
		}
	}
	_, domain, ok := splitFrom(from)
	if !ok {
		return ""
	}
	return strings.ToLower(domain)
}
