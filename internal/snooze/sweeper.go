// Package snooze implements the engine-side wake-up sweeper for snoozed
// mail. Snooze is flag-based (the control plane sets $Snoozed +
// $SnoozedUntil-<unix> + \Seen over IMAP); the sweeper periodically walks
// the store, and for every message whose wake-up time has passed it strips
// the snooze keywords and \Seen — the message reappears in the inbox as
// unread — and fires a delivery receipt so the control plane raises push,
// webhooks and the webmail SSE stream immediately. Without the sweeper a
// due message only resurfaces when its owner happens to open the snoozed
// view (lazy wake), with no notification at all.
package snooze

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
	// Flag marks a message as snoozed (hidden until its wake-up time).
	Flag = "$Snoozed"
	// UntilPrefix encodes the wake-up time as $SnoozedUntil-<unix>.
	UntilPrefix = "$SnoozedUntil-"
)

// AccountLister enumerates the local accounts (the KV store's directory).
type AccountLister func(ctx context.Context) ([]string, error)

// receiptNotifier is the wake-up receipt channel; *notify.Client (the same
// one delivery receipts use) satisfies it.
type receiptNotifier interface {
	DeliveredAsync(account string, refs []notify.Delivered)
}

// Sweeper wakes due snoozed messages on a fixed interval.
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

// SweepOnce wakes every due snoozed message across all accounts. Returns
// the number of messages woken. Errors are per-account/mailbox and never
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
		s.Logger.Warn("snooze: list accounts", "err", err)
		return 0
	}
	now := time.Now()
	woken := 0
	for _, account := range accounts {
		boxes, err := s.Store.ListMailboxes(ctx, account)
		if err != nil {
			s.Logger.Warn("snooze: list mailboxes", "account", account, "err", err)
			continue
		}
		var receipts []notify.Delivered
		for _, box := range boxes {
			msgs, err := s.Store.ListMessages(ctx, account, box.Name)
			if err != nil {
				s.Logger.Warn("snooze: list messages", "account", account, "mailbox", box.Name, "err", err)
				continue
			}
			for _, msg := range msgs {
				until, ok := UntilFromKeywords(msg.Keywords)
				if !ok || until.After(now) {
					continue
				}
				if err := s.Store.SetFlags(ctx, account, box.Name, msg.UID, wakeFlags(msg.Flags, msg.Keywords)); err != nil {
					s.Logger.Warn("snooze: wake", "account", account, "mailbox", box.Name, "uid", msg.UID, "err", err)
					continue
				}
				woken++
				receipts = append(receipts, notify.Delivered{Mailbox: box.Name, UID: msg.UID})
			}
		}
		if woken > 0 && s.Notify != nil && len(receipts) > 0 {
			// Same channel as delivery receipts: the control plane kicks
			// push/webhooks/SSE for the account immediately.
			s.Notify.DeliveredAsync(account, receipts)
		}
	}
	if woken > 0 {
		s.Logger.Info("snooze: woken", "count", woken)
	}
	return woken
}

// UntilFromKeywords extracts the wake-up time from the message keywords.
func UntilFromKeywords(keywords []string) (time.Time, bool) {
	for _, kw := range keywords {
		lower := strings.ToLower(kw)
		if strings.HasPrefix(lower, strings.ToLower(UntilPrefix)) {
			if n, err := strconv.ParseInt(strings.TrimPrefix(lower, strings.ToLower(UntilPrefix)), 10, 64); err == nil {
				return time.Unix(n, 0), true
			}
		}
	}
	return time.Time{}, false
}

// wakeFlags builds the post-wake flag set: the snooze keywords and \Seen are
// stripped (the message returns unread so unseen counts grow and clients
// refresh), everything else survives.
func wakeFlags(flags, keywords []string) []string {
	out := make([]string, 0, len(flags))
	snoozeFlag := strings.ToLower(Flag)
	snoozePrefix := strings.ToLower(UntilPrefix)
	for _, f := range flags {
		lower := strings.ToLower(f)
		if lower == snoozeFlag || strings.HasPrefix(lower, snoozePrefix) || lower == "\\seen" {
			continue
		}
		out = append(out, f)
	}
	for _, kw := range keywords {
		lower := strings.ToLower(kw)
		if lower == snoozeFlag || strings.HasPrefix(lower, snoozePrefix) {
			continue
		}
		if !containsFlag(out, kw) {
			out = append(out, kw)
		}
	}
	return out
}

func containsFlag(flags []string, f string) bool {
	for _, x := range flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}
