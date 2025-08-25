// DMARC policy evaluation (RFC 7489): record lookup/parse via
// go-msgauth/dmarc, alignment and result computation engine-local.
package verify

import (
	"context"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dmarc"

	"mailezine/internal/maildns"
	"mailezine/internal/spf"
)

// evaluateDMARC looks up the _dmarc record for mailFromDomain and computes
// the DMARC result from the SPF result, the d= domain of a passing DKIM
// signature, and the RFC5322.From domain parsed from data. use is false
// when no usable record exists (the dmarc method is omitted from the
// Authentication-Results header).
func evaluateDMARC(ctx context.Context, r maildns.Resolver, mailFromDomain string, spfResult spf.Result, dkimDomain string, data []byte) (use bool, value authres.ResultValue) {
	fromDomain := fromDomain(data)
	if fromDomain == "" {
		return false, ""
	}
	txts, err := r.LookupTXT(ctx, "_dmarc."+mailFromDomain)
	if err != nil {
		return false, ""
	}
	var recordText string
	for _, txt := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(txt)), "v=dmarc1") {
			recordText = txt
			break
		}
	}
	if recordText == "" {
		return false, ""
	}
	rec, err := dmarc.Parse(recordText)
	if err != nil {
		return false, "" // malformed record is treated as no policy (fail-open)
	}

	spfAligned := spfResult == spf.ResultPass &&
		aligned(mailFromDomain, fromDomain, string(rec.SPFAlignment))
	dkimAligned := dkimDomain != "" &&
		aligned(dkimDomain, fromDomain, string(rec.DKIMAlignment))
	if spfAligned || dkimAligned {
		return true, "pass"
	}
	return true, "fail"
}

// aligned reports whether identity matches the RFC5322.From domain under
// the record's alignment mode: "s" (strict) requires equality, "r"
// (relaxed) also accepts a subdomain of fromDomain.
func aligned(identity, fromDomain, mode string) bool {
	identity = strings.ToLower(strings.TrimSuffix(identity, "."))
	fromDomain = strings.ToLower(strings.TrimSuffix(fromDomain, "."))
	if identity == fromDomain {
		return true
	}
	if mode == "s" {
		return false
	}
	return strings.HasSuffix(identity, "."+fromDomain)
}
