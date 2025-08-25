// Delay warnings: when a message stays in the queue past the threshold, the
// caller is notified so a "delayed" DSN (RFC 3464 action=delayed) can be
// sent to the sender. The warning repeats at the same interval until
// delivery succeeds or the message reaches a terminal state.
package queue

import (
	"context"
	"fmt"
	"time"

	"mailezine/internal/maildsn"
)

// DelayWarningHandler is called each time a queued message crosses the
// delay-warning interval. data is the original message body.
type DelayWarningHandler func(ctx context.Context, from string, msg *Message, data []byte, waited time.Duration)

// SetDelayWarningHandler installs the delay warning callback.
func (m *Manager) SetDelayWarningHandler(fn DelayWarningHandler) {
	m.delayWarn = fn
}

// ComposeDelayDSN builds a multipart/report DSN with action=delayed for
// every still-pending recipient.
func ComposeDelayDSN(from string, msg *Message, waited time.Duration, hostname string) ([]byte, error) {
	if hostname == "" {
		hostname = "localhost"
	}
	var recipients []maildsn.Recipient
	for _, r := range msg.Recipients {
		if r.Status != RecipientPending {
			continue
		}
		if !validAddress(r.Address) {
			continue
		}
		recipients = append(recipients, maildsn.Recipient{
			FinalRecipient: r.Address,
			Action:         maildsn.ActionDelayed,
			Status:         "4.4.1",
			StatusComment:  "cannot connect to remote host",
		})
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	text := fmt.Sprintf(`This is a warning that your message has not yet been
delivered to all recipients after %s.

No action is required on your part; delivery attempts will continue.
`, waited.Round(waited).String())
	d := &maildsn.Message{
		From:         "postmaster@" + hostname,
		To:           from,
		Subject:      "Delayed Mail Notification",
		ReportingMTA: hostname,
		ArrivalDate:  msg.CreatedAt,
		TextBody:     text,
		Recipients:   recipients,
		MessageID:    fmt.Sprintf("<mailezine-dsn-%d@%s>", msg.ID, hostname),
	}
	return d.Compose()
}

// maybeDelayWarning fires the delay warning when the message has been queued
// past the threshold and the previous warning (if any) is also older than
// the threshold. Returns true when a warning was sent.
func (m *Manager) maybeDelayWarning(ctx context.Context, msg *Message, data []byte) bool {
	if m.delayWarn == nil || m.opts.DelayWarning <= 0 {
		return false
	}
	now := m.opts.Now()
	waited := now.Sub(msg.CreatedAt)
	if waited < m.opts.DelayWarning {
		return false
	}
	if !msg.LastWarningAt.IsZero() && now.Sub(msg.LastWarningAt) < m.opts.DelayWarning {
		return false
	}
	msg.LastWarningAt = now
	if err := m.save(*msg, msg.NextAttempt); err != nil {
		m.logger.Error("queue: save delay warning", "message", msg.ID, "err", err)
		return false
	}
	m.delayWarn(ctx, msg.From, msg, data, waited)
	m.event("delayed")
	return true
}
