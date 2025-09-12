// Package burn implements the engine-side burn-after-read sweeper.
//
// Burn is flag-based: the control plane writes $BurnRead + $BurnReadUntil-<unix>
// when a reader reveals the message, and withholds the body from API clients
// outside that window (internal/mailbox gateBurn). Withholding is not the same
// as destroying, though: the text still sits in the store. The sweeper closes
// that gap — once the window has passed it replaces the message with a stub
// ("destroyed" notice, no body, no attachments) and deletes the original, so
// the blob garbage collector can reclaim the content.
//
// Replacing rather than rewriting is deliberate: stored messages are
// immutable, and the recall path already uses the same append-stub + delete
// shape. The stub carries no burn keywords, which also makes the sweep
// idempotent — a second pass finds nothing to do.
package burn

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"mailezine/internal/mailstore"
	"mailezine/internal/notify"
)

const (
	// ReadFlag marks a revealed burn-after-read message.
	ReadFlag = "$BurnRead"
	// UntilPrefix encodes the reveal deadline as $BurnReadUntil-<unix>.
	UntilPrefix = "$BurnReadUntil-"
)

// AccountLister enumerates the local accounts (the KV store's directory).
type AccountLister func(ctx context.Context) ([]string, error)

// receiptNotifier is the same receipt channel delivery/snooze use; it lets the
// control plane raise push/webhooks/SSE immediately after a sweep.
type receiptNotifier interface {
	DeliveredAsync(account string, refs []notify.Delivered)
}

// Sweeper destroys due burn-after-read messages on a fixed interval.
type Sweeper struct {
	Accounts AccountLister
	Store    mailstore.MailboxStore
	Notify   receiptNotifier // optional
	Logger   *slog.Logger
}

// Run sweeps every interval until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce destroys every due burn-after-read message across all accounts and
// returns how many were replaced. Errors are per-account/mailbox and never
// abort the sweep — the next tick retries whatever failed.
func (s *Sweeper) SweepOnce(ctx context.Context) int {
	if s.Accounts == nil || s.Store == nil {
		return 0
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	accounts, err := s.Accounts(ctx)
	if err != nil {
		s.Logger.Warn("burn: list accounts", "err", err)
		return 0
	}
	now := time.Now()
	burned := 0
	for _, account := range accounts {
		boxes, err := s.Store.ListMailboxes(ctx, account)
		if err != nil {
			s.Logger.Warn("burn: list mailboxes", "account", account, "err", err)
			continue
		}
		var receipts []notify.Delivered
		for _, box := range boxes {
			msgs, err := s.Store.ListMessages(ctx, account, box.Name)
			if err != nil {
				s.Logger.Warn("burn: list messages", "account", account, "mailbox", box.Name, "err", err)
				continue
			}
			for _, msg := range msgs {
				until, ok := UntilFromKeywords(msg.Keywords)
				if !ok || until.After(now) {
					continue
				}
				stub := burnStub(msg)
				if _, err := s.Store.Append(ctx, account, box.Name, stub); err != nil {
					s.Logger.Warn("burn: append stub", "account", account, "mailbox", box.Name, "uid", msg.UID, "err", err)
					continue
				}
				// Only delete once the replacement is safely stored.
				if _, err := s.Store.DeleteUIDs(ctx, account, box.Name, []uint32{msg.UID}); err != nil {
					s.Logger.Warn("burn: delete original", "account", account, "mailbox", box.Name, "uid", msg.UID, "err", err)
					continue
				}
				burned++
				receipts = append(receipts, notify.Delivered{Mailbox: box.Name, UID: msg.UID})
			}
		}
		if len(receipts) > 0 && s.Notify != nil {
			s.Notify.DeliveredAsync(account, receipts)
		}
	}
	if burned > 0 {
		s.Logger.Info("burn: destroyed", "count", burned)
	}
	return burned
}

// UntilFromKeywords extracts the reveal deadline from a message's keywords.
func UntilFromKeywords(keywords []string) (time.Time, bool) {
	prefix := strings.ToLower(UntilPrefix)
	for _, kw := range keywords {
		lower := strings.ToLower(kw)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		if n, err := strconv.ParseInt(lower[len(prefix):], 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// burnStub builds the replacement message: the burned notice keeps the thread
// subject readable, carries no content and no burn keywords (so the sweep is
// idempotent and the control plane stops treating it as burn mail).
func burnStub(msg *mailstore.Message) *mailstore.Message {
	subject := headerValue(msg.Data, "Subject")
	if subject == "" {
		subject = "(无主题)"
	}
	raw := "Subject: 已阅后即焚：" + subject + "\r\n" +
		"X-Mailez-Burned: 1\r\n" +
		"Auto-Submitted: auto-replied\r\n" +
		"\r\n" +
		"该邮件为阅后即焚邮件，阅读窗口已结束，正文已被销毁。\r\n"
	return &mailstore.Message{
		Data:         []byte(raw),
		InternalDate: msg.InternalDate,
		Flags:        append(keepFlags(msg.Flags), "\\Seen"),
		Size:         int64(len(raw)),
	}
}

// keepFlags copies every flag except the burn keywords.
func keepFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	read := strings.ToLower(ReadFlag)
	prefix := strings.ToLower(UntilPrefix)
	for _, f := range flags {
		lower := strings.ToLower(f)
		if lower == read || strings.HasPrefix(lower, prefix) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// headerValue reads one header from raw RFC822 bytes (headers only, first
// occurrence, no folding beyond the first continuation line).
func headerValue(raw []byte, key string) string {
	head := raw
	if i := strings.Index(string(raw), "\r\n\r\n"); i >= 0 {
		head = raw[:i]
	}
	want := strings.ToLower(key) + ":"
	for _, line := range strings.Split(string(head), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), want) {
			return strings.TrimSpace(line[len(want):])
		}
	}
	return ""
}
