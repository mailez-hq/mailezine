// Package junk implements the built-in baseline spam classifier: a
// conservative scorer over SPF/DKIM/DMARC authentication results (RFC 8601
// Authentication-Results), DNSBL queries and sender allow/deny lists.
//
// It satisfies delivery.Classifier (plus the ClassifyAuthResults fast path so
// the pipeline can hand over the verifier's header instead of forcing a
// re-verification) and gives the default build usable baseline protection
// with zero external dependencies. When MAILEZINE_RSPAMD_URL is configured,
// the rspamd client takes precedence and this classifier is not wired.
//
// The scoring posture is deliberately lenient — a rejected legitimate message
// costs far more than a delivered spam. Only hard signals (deny-listed
// senders, multi-RBL hits, DMARC p=reject failures) reach the reject
// threshold; everything else marks via X-Spam headers and lets the user's
// Sieve spamtest rules decide.
package junk

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"mailezine/internal/delivery"
)

// Default scoring posture. Kept conservative: the classifier sees only a
// handful of signals, so the reject bar sits well above any single soft hit.
const (
	DefaultHeaderScore = 4.5
	DefaultRejectScore = 12.0

	scoreSPFFail      = 3.0
	scoreSPFSoftFail  = 1.5
	scoreSPFNeutral   = 0.5
	scoreDKIMFail     = 3.0
	scoreDMARCReject  = 4.0
	scoreDMARCQuar    = 2.0
	scoreRBLHit       = 3.0
	denyScore         = 99.0 // deny-listed senders reject outright
	defaultRBLTimeout = 2 * time.Second
	dnsblHitTTL       = 5 * time.Minute
	dnsblCleanTTL     = 10 * time.Minute
	greylistWindow    = time.Hour // retries within this window pass
)

// Config tunes the classifier. Zero fields fall back to the defaults above.
type Config struct {
	// HeaderScore is the score at or above which messages get X-Spam-Flag.
	HeaderScore float64
	// RejectScore is the score at or above which messages are rejected.
	RejectScore float64
	// RBLs lists DNSBL zones queried with the reversed peer IP (e.g.
	// "bl.spamcop.net"). Empty disables DNSBL checks.
	RBLs []string
	// Whitelist entries are full addresses ("a@b.c") or bare domains
	// ("b.c"); whitelisted senders skip every check.
	Whitelist []string
	// Blacklist entries use the same forms; deny-listed senders are
	// rejected outright.
	Blacklist []string
	// Greylist enables greylisting for first-seen senders whose score
	// lands in the ambiguous band (above half the header threshold, below
	// the reject threshold). Off by default: it trades a retry delay for
	// protection, which is an operator decision.
	Greylist bool
}

// Classifier is the baseline community spam classifier.
type Classifier struct {
	resolver Resolver
	cfg      Config
	logger   *slog.Logger

	rblMu    sync.Mutex
	rblCache map[string]dnsblEntry

	greyMu sync.Mutex
	grey   map[string]time.Time
}

// Resolver is the narrow DNS surface the classifier needs.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type dnsblEntry struct {
	hit   bool
	until time.Time
}

// New builds a classifier. resolver may be nil only when cfg.RBLs is empty.
func New(resolver Resolver, cfg Config, logger *slog.Logger) *Classifier {
	if cfg.HeaderScore <= 0 {
		cfg.HeaderScore = DefaultHeaderScore
	}
	if cfg.RejectScore <= 0 {
		cfg.RejectScore = DefaultRejectScore
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Classifier{
		resolver: resolver,
		cfg:      cfg,
		logger:   logger,
		rblCache: map[string]dnsblEntry{},
		grey:     map[string]time.Time{},
	}
}

// hit is one scoring signal, kept for the X-Spam-Report line.
type hit struct {
	name   string
	points float64
}

// ClassifyAuthResults implements the pipeline's auth-results fast path: the
// verifier's Authentication-Results header (already computed upstream) is
// scored instead of re-running SPF/DKIM/DMARC. Satisfies
// delivery.Classifier via the plain Classify path (which scores from DNSBL
// and lists only).
func (c *Classifier) ClassifyAuthResults(ctx context.Context, authResults string, peer net.IP, from string, to []string, data []byte) (delivery.Result, error) {
	if c.denyListed(from) {
		return c.verdict(denyScore, []hit{{"deny-listed sender", denyScore}}), nil
	}
	if c.allowListed(from) {
		return delivery.Result{Action: "no action"}, nil
	}
	var hits []hit
	hits = append(hits, scoreAuthResults(authResults)...)
	rblHits, err := c.checkRBLs(ctx, peer)
	if err != nil {
		// DNS trouble must not block mail: keep the partial picture.
		c.logger.Warn("junk: rbl", "peer", peer, "err", err)
	}
	for _, zone := range rblHits {
		hits = append(hits, hit{"DNSBL:" + zone, scoreRBLHit})
	}
	score := total(hits)
	if c.cfg.Greylist && score > c.cfg.HeaderScore/2 && score < c.cfg.RejectScore && !c.greySeen(peer, from, to) {
		return delivery.Result{Action: "greylist", Score: score, RequiredScore: c.cfg.RejectScore}, nil
	}
	return c.verdict(score, hits), nil
}

// Classify implements delivery.Classifier without an auth-results header
// (the verifier may be disabled); DNSBL and lists still apply.
func (c *Classifier) Classify(ctx context.Context, peer net.IP, from string, to []string, data []byte) (delivery.Result, error) {
	if c.denyListed(from) {
		return c.verdict(denyScore, []hit{{"deny-listed sender", denyScore}}), nil
	}
	if c.allowListed(from) {
		return delivery.Result{Action: "no action"}, nil
	}
	rblHits, err := c.checkRBLs(ctx, peer)
	if err != nil {
		c.logger.Warn("junk: rbl", "peer", peer, "err", err)
	}
	var hits []hit
	for _, zone := range rblHits {
		hits = append(hits, hit{"DNSBL:" + zone, scoreRBLHit})
	}
	return c.verdict(total(hits), hits), nil
}

// total sums the hit points.
func total(hits []hit) float64 {
	var s float64
	for _, h := range hits {
		s += h.points
	}
	return s
}

// verdict maps a score to the pipeline action and builds the X-Spam headers.
func (c *Classifier) verdict(score float64, hits []hit) delivery.Result {
	res := delivery.Result{Score: score, RequiredScore: c.cfg.RejectScore}
	switch {
	case score >= c.cfg.RejectScore:
		res.Action = "reject"
	case score >= c.cfg.HeaderScore:
		res.Action = "add header"
	default:
		res.Action = "no action"
		return res
	}
	level := int(score)
	if level < 1 {
		level = 1
	}
	res.Headers = append(res.Headers,
		"X-Spam-Flag: YES",
		fmt.Sprintf("X-Spam-Score: %.1f (threshold %.1f)", score, c.cfg.RejectScore),
		"X-Spam-Level: "+strings.Repeat("*", level),
	)
	var names []string
	for _, h := range hits {
		names = append(names, fmt.Sprintf("%s (%.1f)", h.name, h.points))
	}
	if len(names) > 0 {
		res.Headers = append(res.Headers, "X-Spam-Report: "+strings.Join(names, ", "))
	}
	return res
}

// denyListed reports whether the sender address or its domain is blacklisted.
func (c *Classifier) denyListed(from string) bool { return listed(c.cfg.Blacklist, from) }

// allowListed reports whether the sender address or its domain is whitelisted.
func (c *Classifier) allowListed(from string) bool { return listed(c.cfg.Whitelist, from) }

// listed matches the sender against entries that are either a full address
// (case-insensitive) or a bare domain matching the @-suffix.
func listed(entries []string, from string) bool {
	if len(entries) == 0 || from == "" {
		return false
	}
	addr := strings.ToLower(from)
	domain := ""
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		domain = addr[at+1:]
	}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		switch {
		case strings.HasPrefix(e, "@"):
			// "@domain" is a bare-domain entry with an explicit marker.
			if domain != "" && (domain == e[1:] || strings.HasSuffix(domain, e)) {
				return true
			}
		case strings.Contains(e, "@"):
			if e == addr {
				return true
			}
		default:
			if domain != "" && (e == domain || strings.HasSuffix(domain, "."+e)) {
				return true
			}
		}
	}
	return false
}

// greySeen records a (peer, sender, recipient) tuple and reports whether it
// was retried within the window. State is in-memory: a restart re-greylists
// first-seen senders, which is safe (legitimate MTAs retry).
func (c *Classifier) greySeen(peer net.IP, from string, to []string) bool {
	now := time.Now()
	rcpt := ""
	if len(to) > 0 {
		rcpt = strings.ToLower(to[0])
	}
	key := peer.String() + "\x00" + strings.ToLower(from) + "\x00" + rcpt
	c.greyMu.Lock()
	defer c.greyMu.Unlock()
	for k, t := range c.grey {
		if now.Sub(t) > greylistWindow {
			delete(c.grey, k)
		}
	}
	if t, ok := c.grey[key]; ok && now.Sub(t) <= greylistWindow {
		return true
	}
	c.grey[key] = now
	return false
}

// LearnWithFuzzy satisfies the app-level spamClassifier surface (IMAP
// Junk-boundary feedback). The baseline classifier has no statistical
// learner to feed, so this is an explicit no-op: marking mail as Junk in
// the IMAP client still works (it is a mailbox move), it just does not
// retrain anything. The rspamd classifier implements real supervised
// learning when a learning-capable backend is wired.
func (c *Classifier) LearnWithFuzzy(ctx context.Context, isSpam bool, data []byte) error {
	return nil
}
