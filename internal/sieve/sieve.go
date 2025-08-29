// Package sieve compiles and executes RFC 5228 Sieve scripts (foxcpp/go-sieve,
// MIT) and serves ManageSieve (RFC 5804) for the webmail filter editor.
//
// Execution is a delivery-pipeline stage: the active script (from the
// directory contract) decides the target mailboxes, flags and whether the
// message is discarded. A script error never loses mail: the caller falls
// back to INBOX delivery.
package sieve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-message/textproto"
	gosieve "mailezine/internal/gosieve"
	interp "mailezine/internal/gosieve/interp"
)

// Result is the outcome of one script execution for one message.
type Result struct {
	Mailboxes []string // mailboxes to store one copy each (INBOX = keep)
	Discard   bool     // discard the message (no store, no bounce)
	Flags     []string // imap4flags to apply to stored copies
	Redirects []string // addresses to forward a copy to (sieve redirect)
	Reject    string   // reject/ereject reason: refuse the message (RFC 5429)
	// editheader (RFC 5293): header edits to apply to the stored copy, in
	// script order (deletes before adds preserve "replace" semantics).
	DeleteHeaders []HeaderEdit
	AddHeaders    []HeaderEdit
	// Vacation (RFC 5230): automatic reply requested by the script.
	Vacation *Vacation
}

// HeaderEdit is one editheader operation.
type HeaderEdit struct {
	Name   string
	Value  string   // addheader value; for deletes, the joined match list
	Index  int      // deleteheader :index (0 = all); unused for add
	Values []string // deleteheader :value list (nil = delete every instance)
	Delete bool
}

// Vacation is a requested automatic reply (RFC 5230 subset).
type Vacation struct {
	Days    int
	From    string
	Subject string
	Body    string
}

// Engine compiles and caches scripts.
type Engine struct {
	mu     sync.Mutex
	cache  map[[32]byte]*cacheEntry
	logger *slog.Logger
}

type cacheEntry struct {
	script *gosieve.Script
	when   time.Time
}

const scriptTTL = 5 * time.Minute

// NewEngine builds a script engine.
func NewEngine(logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{cache: map[[32]byte]*cacheEntry{}, logger: logger}
}

// Route executes src against the message and returns the routing result.
// envTo is the ORIGINAL envelope recipient (pre alias-expansion, pre
// +tag-stripping): RFC 5228 §5.4 requires envelope tests to evaluate against
// it, so subaddress routing (envelope :detail) and alias matching work.
func (e *Engine) Route(ctx context.Context, src, from, envTo string, data []byte) (Result, error) {
	script, err := e.compile(src)
	if err != nil {
		return Result{}, fmt.Errorf("sieve: compile: %w", err)
	}
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(data)))
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("sieve: parse header: %w", err)
	}
	runtime := gosieve.NewRuntimeData(script, allowRedirect{}, interp.EnvelopeStatic{
		From: from,
		To:   envTo,
	}, interp.MessageStatic{
		Size:       len(data),
		Header:     &header,
		RawMessage: data,
	})
	if err := script.Execute(ctx, runtime); err != nil {
		return Result{}, fmt.Errorf("sieve: execute: %w", err)
	}
	return actionsToResult(runtime.AppliedActions), nil
}

// Check reports whether src compiles (used by ManageSieve CHECKSCRIPT and
// PUTSCRIPT validation; RFC 5804 §2.6: a script that does not compile must
// not be stored).
func (e *Engine) Check(src string) error {
	_, err := e.compile(src)
	return err
}

func (e *Engine) compile(src string) (*gosieve.Script, error) {
	key := sha256.Sum256([]byte(src))
	e.mu.Lock()
	if entry, ok := e.cache[key]; ok && time.Since(entry.when) < scriptTTL {
		e.mu.Unlock()
		return entry.script, nil
	}
	e.mu.Unlock()

	script, err := gosieve.Load(strings.NewReader(src), gosieve.DefaultOptions())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.cache[key] = &cacheEntry{script: script, when: time.Now()}
	e.mu.Unlock()
	return script, nil
}

// actionsToResult maps interpreter actions to delivery decisions. The
// interpreter appends ActionKeep{Implicit:true} when the script does not
// cancel the implicit keep (RFC 5228 §2.10.3).
func actionsToResult(actions []interp.AppliedAction) Result {
	var res Result
	for _, a := range actions {
		switch act := a.(type) {
		case interp.ActionKeep:
			res.Mailboxes = append(res.Mailboxes, "INBOX")
			res.Flags = append(res.Flags, normalizeFlags(act.Flags)...)
		case interp.ActionFileInto:
			res.Mailboxes = append(res.Mailboxes, act.Mailbox)
			res.Flags = append(res.Flags, normalizeFlags(act.Flags)...)
		case interp.ActionDiscard:
			res.Discard = true
		case interp.ActionRedirect:
			res.Redirects = append(res.Redirects, act.Address)
		case interp.ActionReject:
			res.Reject = act.Reason
		case interp.ActionEReject:
			if res.Reject == "" {
				res.Reject = act.Reason
			}
		case interp.ActionAddHeader:
			res.AddHeaders = append(res.AddHeaders, HeaderEdit{Name: act.Name, Value: act.Value})
		case interp.ActionDeleteHeader:
			res.DeleteHeaders = append(res.DeleteHeaders, HeaderEdit{Name: act.Name, Value: strings.Join(act.Values, ", "), Values: act.Values, Index: act.Index, Delete: true})
		case interp.ActionVacation:
			res.Vacation = &Vacation{Days: act.Days, From: act.From, Subject: act.Subject, Body: act.Body}
		}
	}
	res.Mailboxes = dedupeFold(res.Mailboxes)
	res.Flags = dedupe(res.Flags)
	res.Redirects = dedupe(res.Redirects)
	return res
}

// normalizeFlags folds Sieve flag spellings (setflag "\seen", parser output
// "seen") into the canonical IMAP system flags used by the mailstore.
var systemFlagNames = map[string]string{
	"seen":     "\\Seen",
	"answered": "\\Answered",
	"flagged":  "\\Flagged",
	"deleted":  "\\Deleted",
	"draft":    "\\Draft",
}

func normalizeFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		lower := strings.ToLower(strings.TrimPrefix(f, `\`))
		if canonical, ok := systemFlagNames[lower]; ok {
			out = append(out, canonical)
			continue
		}
		out = append(out, f)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// dedupeFold removes duplicates case-insensitively. Mailbox names differ only
// by spelling ("Inbox" vs "INBOX") yet denote the same mailbox; a
// case-sensitive pass would let fileinto :copy "Inbox" plus the implicit
// keep store two copies of the message.
func dedupeFold(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// allowRedirect permits sieve redirects; the delivery pipeline enqueues
// the forwarded copy into the outbound queue.
type allowRedirect struct{}

func (allowRedirect) RedirectAllowed(context.Context, *interp.RuntimeData, string) (bool, error) {
	return true, nil
}
