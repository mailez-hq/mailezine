// Package delivery implements the inbound pipeline (ARCHITECTURE.md §4):
// resolve recipients through the directory, enforce quotas and store one
// copy per local target. Verification (SPF/DKIM/DMARC) and spam filtering
// run before storage: verify → classify → deliver.
package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"mailezine/internal/directory"
	"mailezine/internal/fts"
	"mailezine/internal/mailstore"
	"mailezine/internal/notify"
	"mailezine/internal/sieve"
)

// ErrQuota is returned when storing a copy would exceed the account quota.
var ErrQuota = errors.New("delivery: quota exceeded")

// SMTP rejection outcomes produced by the spam stage. The SMTP layer maps
// these to 554 (reject), 451 (greylist/soft reject) responses so the
// message is never stored.
var (
	ErrReject     = errors.New("delivery: message rejected by spam filter")
	ErrSoftReject = errors.New("delivery: message temporarily rejected by spam filter")
	ErrGreylist   = errors.New("delivery: message greylisted, retry later")
	// ErrSieveReject is returned when a Sieve reject/ereject action refuses
	// the message (RFC 5429); the SMTP layer maps it to 550.
	ErrSieveReject = errors.New("delivery: message rejected by sieve script")
)

// Verifier adds authentication results (SPF/DKIM/DMARC) to inbound messages.
type Verifier interface {
	Verify(ctx context.Context, peer net.IP, from string, data []byte) (header string, err error)
}

// Result is the classification of one message: the contract the pipeline
// consumes. Classifier implementations (e.g. the rspamd client) produce
// these.
type Result struct {
	Action        string // "no action", "greylist", "add header", "rewrite subject", "soft reject", "reject"
	Score         float64
	RequiredScore float64
	Headers       []string // "Name: value" lines to prepend, in insertion order
}

// Classifier scans an inbound message and returns headers to prepend plus an
// action.
type Classifier interface {
	Classify(ctx context.Context, peer net.IP, from string, to []string, data []byte) (Result, error)
}

// authResultsClassifier is implemented by classifiers that can reuse the
// verifier's Authentication-Results header instead of re-running
// SPF/DKIM/DMARC themselves (the junk baseline classifier does; the rspamd
// client re-scans the whole message and does not).
type authResultsClassifier interface {
	Classifier
	ClassifyAuthResults(ctx context.Context, authResults string, peer net.IP, from string, to []string, data []byte) (Result, error)
}

// Pipeline resolves and stores inbound messages.
type Pipeline struct {
	Directory    directory.Service
	Store        mailstore.Store
	Verifier     Verifier           // optional
	Classifier   Classifier         // optional; nil disables scanning
	Sieve        *sieve.Engine      // optional; nil keeps INBOX
	ScriptSource sieve.ScriptSource // optional; defaults to Directory.Sieve
	Hostname     string             // our hostname for the Received header
	// RecipientDelimiter is the extended-address separator: "user+tag@d"
	// resolves to "user@d" when the full address is unknown ("" disables).
	RecipientDelimiter string
	FTS                *fts.Indexer // optional full-text index
	// Notifier receives per-account delivery receipts so the control plane
	// can raise push/webhook/SSE immediately (nil disables).
	Notifier Notifier
	// Redirect forwards a copy to an external address (sieve redirect);
	// implementations spool into the outbound queue. When nil, redirects
	// are logged and skipped (the local copy still applies).
	Redirect func(ctx context.Context, from, to string, data []byte) error
	// Gate optionally serializes per-account writers ACROSS nodes
	// (multi-active): each target's storage section runs under the node's
	// ownership lease for that account, absorbing cross-node write
	// conflicts. Advisory — nil keeps direct writes.
	Gate   AccountGate
	Logger *slog.Logger

	vacationMu   sync.Mutex
	vacationLast map[string]time.Time // "account\x00sender" -> last auto-reply
}

// AccountGate serializes the per-account write section across nodes.
// WithAccount returns once the account is locally owned (after a bounded
// wait on a foreign owner) or proceeds without ownership — implementations
// are advisory and never change write correctness.
type AccountGate interface {
	WithAccount(ctx context.Context, account string, fn func() error) error
}

// Notifier receives delivery receipts after mail lands in local mailboxes.
type Notifier interface {
	DeliveredAsync(account string, refs []notify.Delivered)
}

// Deliver stores one copy per resolved local target. Aliases expand through
// the directory; external targets are rejected by the SMTP layer until relay
// routing lands. Verification and classification happen before storage;
// Authentication-Results and spam headers are prepended to the stored copy.
func (p *Pipeline) Deliver(ctx context.Context, peer net.IP, from string, to []string, data []byte) error {
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	stored := data
	headers := []string{receivedHeader(p.Hostname, peer)}
	if !hasHeader(data, "Message-ID") {
		host := p.Hostname
		if host == "" {
			host = "mailezine"
		}
		headers = append(headers, fmt.Sprintf("Message-ID: <%s.%s@%s>",
			time.Now().Format("20060102150405"), newMessageID(), host))
	}
	var authResults string
	if p.Verifier != nil {
		if header, err := p.Verifier.Verify(ctx, peer, from, data); err != nil {
			p.Logger.Warn("delivery: verify", "from", from, "err", err)
		} else if header != "" {
			headers = append(headers, header)
			authResults = header
		}
	}
	if !classifierNil(p.Classifier) {
		var res Result
		var err error
		if ac, ok := p.Classifier.(authResultsClassifier); ok {
			res, err = ac.ClassifyAuthResults(ctx, authResults, peer, from, to, data)
		} else {
			res, err = p.Classifier.Classify(ctx, peer, from, to, data)
		}
		if err != nil {
			// Fail-open (decision D5): an unreachable classifier must not
			// stop mail. The missing mark is visible in metrics.
			p.Logger.Warn("delivery: classify", "from", from, "err", err)
		} else {
			switch res.Action {
			case "reject":
				return ErrReject
			case "soft reject":
				return ErrSoftReject
			case "greylist":
				return ErrGreylist
			}
			headers = append(headers, res.Headers...)
			if !headersContain(headers, "X-Spam-Level") {
				// rspamd's header set is configurable; the Sieve spamtest
				// extension reads X-Spam-Level, so synthesize it from the
				// score when the scanner did not provide it.
				level := int(res.Score)
				if level < 0 {
					level = 0
				}
				headers = append(headers, "X-Spam-Level: "+strings.Repeat("*", level))
			}
		}
	}
	// An inbound X-Spam-Level is sender-controlled: the Sieve spamtest
	// extension must never trust it. Strip it unconditionally and rely on
	// the locally prepended verdict (rspamd's header, or the synthesized
	// fallback above; nothing at all when no classifier is configured).
	base := stripHeaderFields(data, "X-Spam-Level")
	if len(headers) > 0 {
		stored = prependHeaders(headers, base)
	} else {
		stored = base
	}
	for _, rcpt := range to {
		if err := p.deliverTo(ctx, rcpt, from, stored, data); err != nil {
			return err
		}
	}
	return nil
}

// hasHeader reports whether the message header block contains the field.
func hasHeader(data []byte, key string) bool {
	return headerValue(data, key) != ""
}

// headerValue returns the first value of a header field ("" when absent).
// Folding continuation lines are joined; the value is returned unfolded.
func headerValue(data []byte, key string) string {
	prefix := strings.ToLower(key) + ":"
	block := string(data)
	if i := strings.Index(block, "\r\n\r\n"); i >= 0 {
		block = block[:i]
	}
	matched := false
	var sb strings.Builder
	for _, line := range strings.Split(block, "\r\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if matched {
				sb.WriteString(" ")
				sb.WriteString(strings.TrimSpace(line))
			}
			continue
		}
		if matched {
			break // the next header field starts: stop folding this one
		}
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			matched = true
			sb.Reset()
			sb.WriteString(strings.TrimSpace(line[len(prefix):]))
		}
	}
	if !matched {
		return ""
	}
	return sb.String()
}

// suppressesAutoReply reports whether the message headers request no
// automatic response. RFC 3834 §5.1: an Auto-Submitted value of "no" is the
// explicit "this is a human message" marker and must NOT suppress replies;
// any other non-empty value (auto-replied, auto-generated, …) does.
func suppressesAutoReply(data []byte) bool {
	if headerValue(data, "X-Auto-Response-Suppress") != "" {
		return true
	}
	v := headerValue(data, "Auto-Submitted")
	if v == "" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(v), "no")
}

// headersContain reports whether the "Name: value" header list has the field.
func headersContain(headers []string, key string) bool {
	key = strings.ToLower(key) + ":"
	for _, h := range headers {
		if strings.HasPrefix(strings.ToLower(h), key) {
			return true
		}
	}
	return false
}

// headerField is one logical header field: its first line, folding
// continuation lines, and the unfolded value.
type headerField struct {
	name  string // as written, case preserved
	value string // unfolded and trimmed
	start int    // first line index
	end   int    // last (continuation) line index
}

// splitHeaderFields groups header lines into logical fields, attaching
// folding continuation lines (leading SP/HTAB) to the field they continue.
func splitHeaderFields(lines []string) []headerField {
	var out []headerField
	cur := -1
	for i, line := range lines {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if cur >= 0 {
				out[cur].value += " " + strings.TrimSpace(line)
				out[cur].end = i
			}
			continue
		}
		if c := strings.IndexByte(line, ':'); c >= 0 {
			out = append(out, headerField{name: line[:c], value: strings.TrimSpace(line[c+1:]), start: i, end: i})
			cur = len(out) - 1
		} else {
			cur = -1
		}
	}
	return out
}

// stripHeaderFields removes every instance of the named header fields
// (folding continuation lines included) from the header block of data.
func stripHeaderFields(data []byte, names ...string) []byte {
	block := string(data)
	body := ""
	if i := strings.Index(block, "\r\n\r\n"); i >= 0 {
		body = block[i:]
		block = block[:i]
	}
	lines := strings.Split(block, "\r\n")
	lower := make(map[string]bool, len(names))
	for _, n := range names {
		lower[strings.ToLower(n)] = true
	}
	remove := make([]bool, len(lines))
	for _, f := range splitHeaderFields(lines) {
		if lower[strings.ToLower(f.name)] {
			for i := f.start; i <= f.end; i++ {
				remove[i] = true
			}
		}
	}
	var kept []string
	for i, l := range lines {
		if !remove[i] {
			kept = append(kept, l)
		}
	}
	return []byte(strings.Join(kept, "\r\n") + body)
}

// applyHeaderEdits applies RFC 5293 editheader actions to the message being
// stored. Deletes run in script order — each action counts :index
// occurrences against the message as mutated by its predecessors — and a
// deleted field takes its folding continuation lines with it; adds append
// after the deletes (a delete+add pair is a "replace").
func applyHeaderEdits(data []byte, res sieve.Result) []byte {
	if len(res.DeleteHeaders) == 0 && len(res.AddHeaders) == 0 {
		return data
	}
	block := string(data)
	body := ""
	if i := strings.Index(block, "\r\n\r\n"); i >= 0 {
		body = block[i:]
		block = block[:i]
	}
	lines := strings.Split(block, "\r\n")

	for _, e := range res.DeleteHeaders {
		lines = deleteHeaderFields(lines, e)
	}
	for _, e := range res.AddHeaders {
		lines = append(lines, e.Name+": "+e.Value)
	}
	return []byte(strings.Join(lines, "\r\n") + body)
}

// deleteHeaderFields applies one deleteheader action to the header lines.
func deleteHeaderFields(lines []string, e sieve.HeaderEdit) []string {
	name := strings.ToLower(e.Name)
	values := e.Values
	if len(values) == 0 && e.Value != "" {
		values = strings.Split(e.Value, ",")
	}
	// An empty match list (not merely nil) deletes every instance: the
	// interpreter routes a valueless deleteheader through the same list
	// machinery, producing an empty non-nil slice.
	if len(values) == 0 {
		values = nil
	}
	fields := splitHeaderFields(lines)
	remove := make([]bool, len(lines))
	occurrence := 0
	for _, f := range fields {
		if strings.ToLower(f.name) != name {
			continue
		}
		occurrence++
		// RFC 5293 §2.5: :index restricts the delete to the Nth occurrence
		// of the field (1-based); without it every matching instance goes.
		if e.Index > 0 && occurrence != e.Index {
			continue
		}
		if values != nil && !matchesAnyValue(values, f.value) {
			continue
		}
		for i := f.start; i <= f.end; i++ {
			remove[i] = true
		}
	}
	var kept []string
	for i, l := range lines {
		if !remove[i] {
			kept = append(kept, l)
		}
	}
	return kept
}

func matchesAnyValue(patterns []string, value string) bool {
	for _, p := range patterns {
		if strings.TrimSpace(p) == value {
			return true
		}
	}
	return false
}

// sendVacation generates an RFC 5230 auto-reply and hands it to the outbound
// path with a null envelope sender (RFC 5321: no DSN for auto-replies, and
// the null sender prevents loops). A per-recipient :days throttle is kept in
// memory; the "Auto-Submitted" presence check stops reply storms.
func (p *Pipeline) sendVacation(ctx context.Context, account, from string, to []string, data []byte, v *sieve.Vacation) error {
	if p.Redirect == nil {
		p.Logger.Warn("delivery: vacation skipped (no outbound queue)", "account", account, "to", from)
		return nil
	}
	if suppressesAutoReply(data) {
		return nil
	}
	if from == "" {
		return nil // never reply to the null sender
	}
	days := v.Days
	if days <= 0 {
		days = 7
	}
	// Throttle key and persisted state are case-normalized: envelope
	// addresses vary in case between senders' retries, and each variant
	// would otherwise get an independent reply budget.
	sender := strings.ToLower(from)
	key := account + "\x00" + sender
	// Persisted throttle when the store supports it; otherwise fall back
	// to the in-memory map (still prevents same-process storms).
	var last time.Time
	if vs, ok := p.Store.(mailstore.VacationStateStore); ok {
		var err error
		last, err = vs.VacationLastSent(ctx, account, sender)
		if err != nil {
			p.Logger.Warn("delivery: vacation state read", "account", account, "err", err)
		}
	} else {
		p.vacationMu.Lock()
		last = p.vacationLast[key]
		p.vacationMu.Unlock()
	}
	if !last.IsZero() && time.Since(last) < time.Duration(days)*24*time.Hour {
		return nil
	}
	now := time.Now()
	if vs, ok := p.Store.(mailstore.VacationStateStore); ok {
		if err := vs.SetVacationLastSent(ctx, account, sender, now); err != nil {
			p.Logger.Warn("delivery: vacation state write", "account", account, "err", err)
		}
	} else {
		p.vacationMu.Lock()
		if p.vacationLast == nil {
			p.vacationLast = map[string]time.Time{}
		}
		p.vacationLast[key] = now
		p.vacationMu.Unlock()
	}

	replyFrom := v.From
	if replyFrom == "" {
		replyFrom = account
	}
	subject := v.Subject
	if subject == "" {
		subject = "Re: your message"
	}
	// RFC 5230 §5.2: replies carry their own Message-ID and thread into the
	// original via In-Reply-To/References when the source has one.
	var hdrs strings.Builder
	fmt.Fprintf(&hdrs, "Auto-Submitted: auto-replied\r\n"+
		"X-Auto-Response-Suppress: All\r\n"+
		"From: %s\r\n"+
		"To: %s\r\n"+
		"Subject: %s\r\n"+
		"Date: %s\r\n"+
		"Message-ID: <vac-%s@mailezine>\r\n",
		replyFrom, from, subject, time.Now().Format(time.RFC1123Z), newMessageID())
	if orig := headerValue(data, "Message-Id"); orig != "" {
		hdrs.WriteString("In-Reply-To: " + orig + "\r\n")
		hdrs.WriteString("References: " + orig + "\r\n")
	}
	reply := hdrs.String() + "\r\n" + v.Body + "\r\n"
	p.Logger.Debug("delivery: vacation reply", "account", account, "to", from, "days", days)
	return p.Redirect(ctx, "", from, []byte(reply))
}

// receivedHeader builds the RFC 5321 §4.4 Received trace of this MTA. The
// HELO identity is not surfaced by go-smtp, so the peer IP stands in.
func receivedHeader(hostname string, peer net.IP) string {
	if hostname == "" {
		hostname = "unknown"
	}
	ip := "unknown"
	if peer != nil {
		ip = peer.String()
	}
	return fmt.Sprintf("Received: from %s by %s (mailezine) with SMTP id %s; %s",
		ip, hostname, newMessageID(), time.Now().Format(time.RFC1123Z))
}

func newMessageID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}

// classifierNil reports whether c is nil or a typed nil pointer. Storing a
// nil *spam.Client in the interface field would otherwise panic at call time
// instead of degrading to fail-open.
func classifierNil(c Classifier) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func (p *Pipeline) deliverTo(ctx context.Context, rcpt, from string, stored, raw []byte) error {
	// envTo is the original envelope recipient: alias expansion and
	// +tag-stripping below narrow the DELIVERY targets, but Sieve envelope
	// tests must still see the address the sender actually used.
	envTo := rcpt
	targets, err := p.Directory.Aliases(ctx, rcpt)
	if errors.Is(err, directory.ErrNotFound) && p.RecipientDelimiter != "" {
		if base, ok := directory.SplitDelimited(rcpt, p.RecipientDelimiter); ok {
			if targets, err = p.Directory.Aliases(ctx, base); err == nil {
				rcpt = base
			}
		}
	}
	if err != nil {
		return fmt.Errorf("delivery: resolve %s: %w", rcpt, err)
	}
	for _, target := range targets {
		if p.Gate == nil {
			if err := p.deliverToTarget(ctx, target, envTo, from, targets, stored, raw); err != nil {
				return err
			}
			continue
		}
		// The whole storage section for one account runs under the node's
		// ownership lease: cross-node writers queue here instead of
		// colliding on the account's hot keys.
		if err := p.Gate.WithAccount(ctx, target, func() error {
			return p.deliverToTarget(ctx, target, envTo, from, targets, stored, raw)
		}); err != nil {
			return err
		}
	}
	return nil
}

// deliverToTarget runs the per-account section of the pipeline: quota
// checks, Sieve routing and one stored copy per destination mailbox.
func (p *Pipeline) deliverToTarget(ctx context.Context, target, envTo, from string, targets []string, stored, raw []byte) error {
	{
		size := int64(len(raw))
		if err := p.checkQuota(ctx, target, size); err != nil {
			return err
		}
		mailboxes := []string{"INBOX"}
		var sieveRes sieve.Result
		if p.Sieve != nil {
			script, ok, serr := p.activeScript(ctx, target)
			if serr != nil {
				p.Logger.Warn("delivery: sieve fetch", "account", target, "err", serr)
			} else if ok {
				res, rerr := p.Sieve.Route(ctx, script, from, envTo, stored)
				sieveRes = res
				if rerr != nil {
					// A broken script must never lose mail: keep INBOX.
					p.Logger.Warn("delivery: sieve run", "account", target, "err", rerr)
				} else if res.Discard {
					p.Logger.Debug("delivery: sieve discard", "account", target)
					return nil
				} else if res.Reject != "" {
					p.Logger.Info("delivery: sieve reject", "account", target, "reason", res.Reject)
					return fmt.Errorf("%w: %s", ErrSieveReject, res.Reject)
				} else if len(res.Mailboxes) > 0 {
					mailboxes = res.Mailboxes
				}
				for _, addr := range res.Redirects {
					if p.Redirect == nil {
						p.Logger.Warn("delivery: sieve redirect skipped (no queue)",
							"account", target, "to", addr)
						continue
					}
					if err := p.Redirect(ctx, from, addr, stored); err != nil {
						p.Logger.Error("delivery: sieve redirect", "account", target, "to", addr, "err", err)
					}
				}
				if res.Vacation != nil {
					if err := p.sendVacation(ctx, target, from, targets, stored, res.Vacation); err != nil {
						p.Logger.Error("delivery: vacation", "account", target, "to", from, "err", err)
					}
				}
			}
		}
		finalData := applyHeaderEdits(stored, sieveRes)
		// INV-QUOTA (conservative pre-check): every fileinto copy is stored
		// and charged separately, so the pre-check must budget all of them —
		// checking one raw copy while writing N final copies lets accounts
		// grow past their limit.
		if err := p.checkQuota(ctx, target, int64(len(finalData))*int64(len(mailboxes))); err != nil {
			return err
		}
		var delivered []notify.Delivered
		for _, mailbox := range mailboxes {
			// Sieve scripts are authored with the display spelling ("Inbox/Sub").
			// Only the exact "INBOX" name is the special mailbox on the wire,
			// so rewrite the prefix before storing — otherwise the message
			// lands in a literal "Inbox" folder that IMAP clients selecting
			// "INBOX/Sub" can never reach.
			mailbox = normalizeMailboxName(mailbox)
			msg := &mailstore.Message{
				From:         from,
				To:           targets,
				Data:         finalData,
				Flags:        sieveRes.Flags, // imap4flags: setflag/keep :flags land here
				InternalDate: time.Now(),
			}
			uid, err := p.Store.Deliver(ctx, target, mailbox, msg)
			if err != nil {
				return fmt.Errorf("delivery: store to %s/%s: %w", target, mailbox, err)
			}
			if p.FTS != nil {
				if err := p.FTS.IndexMessage(ctx, target, mailbox, uid, finalData); err != nil {
					// Indexing must never lose mail.
					p.Logger.Warn("delivery: fts index", "account", target, "mailbox", mailbox, "err", err)
				}
			}
			delivered = append(delivered, notify.Delivered{Mailbox: mailbox, UID: uid})
		}
		if p.Notifier != nil && len(delivered) > 0 {
			// Immediate receipt: web push/webhooks/SSE fire now instead of
			// at the poller's next tick. Fire-and-forget by contract.
			p.Notifier.DeliveredAsync(target, delivered)
		}
		p.reportQuota(ctx, target)
		p.Logger.Debug("delivered", "to", target, "mailboxes", mailboxes, "bytes", size)
	}
	return nil
}

// normalizeMailboxName rewrites the display spelling of the special inbox and
// its children ("Inbox", "Inbox/Sub") to the protocol form ("INBOX",
// "INBOX/Sub") used on the IMAP wire.
func normalizeMailboxName(name string) string {
	if strings.EqualFold(name, "inbox") {
		return "INBOX"
	}
	if idx := strings.IndexByte(name, '/'); idx >= 0 && strings.EqualFold(name[:idx], "inbox") {
		return "INBOX" + name[idx:]
	}
	return name
}

// activeScript resolves the script to run for one account.
func (p *Pipeline) activeScript(ctx context.Context, target string) (string, bool, error) {
	if p.ScriptSource != nil {
		return p.ScriptSource.ActiveSieveScript(ctx, target)
	}
	script, err := p.Directory.Sieve(ctx, target)
	if err != nil {
		return "", false, err
	}
	return script.Script, script.Script != "", nil
}

// prependHeaders joins header lines (ending in CRLF) and places them before
// the message body. rspamd header values are already unfolded.
func prependHeaders(headers []string, data []byte) []byte {
	var sb strings.Builder
	for _, h := range headers {
		sb.WriteString(h)
		if !strings.HasSuffix(h, "\r\n") {
			sb.WriteString("\r\n")
		}
	}
	out := make([]byte, 0, sb.Len()+len(data))
	out = append(out, sb.String()...)
	return append(out, data...)
}

func (p *Pipeline) checkQuota(ctx context.Context, target string, size int64) error {
	q, err := p.Directory.Quota(ctx, target)
	if err != nil {
		return nil // no quota rule: not enforced
	}
	used, err := p.Store.QuotaUsedBytes(ctx, target)
	if err != nil {
		return err
	}
	if q.Limit > 0 && used+size > q.Limit {
		p.Logger.Warn("quota exceeded", "account", target, "used", used, "limit", q.Limit)
		return ErrQuota
	}
	return nil
}

// reportQuota writes the used quota back to the control plane (best effort).
func (p *Pipeline) reportQuota(ctx context.Context, target string) {
	used, err := p.Store.QuotaUsedBytes(ctx, target)
	if err != nil {
		return
	}
	_ = p.Directory.UpdateQuotaUsed(ctx, target, used)
}
