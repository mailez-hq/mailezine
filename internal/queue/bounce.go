// Bounce/DSN generation (RFC 3464) for terminal failures. The queue invokes
// the bounce handler once when a message enters the bounced state; the
// composition root turns it into a delivery status notification sent with a
// null envelope sender (RFC 5321: no DSN for DSNs).
package queue

import (
	"context"
	"fmt"
	"strings"

	"mailezine/internal/maildsn"
)

// BounceFailure describes one failed recipient for the DSN.
type BounceFailure struct {
	To      string
	Status  string
	Comment string
}

// BounceHandler is called when a message enters the terminal bounced state.
// body is the original message (still available at that point).
type BounceHandler func(ctx context.Context, from string, msg *Message, body []byte, failures []BounceFailure)

// SetBounceHandler installs the bounce callback.
func (m *Manager) SetBounceHandler(fn BounceHandler) {
	m.bounce = fn
}

// ComposeBounceDSN builds a multipart/report DSN for the failed recipients.
func ComposeBounceDSN(from string, msg *Message, failures []BounceFailure, hostname string) ([]byte, error) {
	if hostname == "" {
		hostname = "localhost"
	}
	var recipients []maildsn.Recipient
	var lines []string
	for _, f := range failures {
		if !validAddress(f.To) {
			continue
		}
		recipients = append(recipients, maildsn.Recipient{
			FinalRecipient:     f.To,
			Action:             maildsn.ActionFailed,
			Status:             f.Status,
			StatusComment:      f.Comment,
			DiagnosticCodeSMTP: f.Comment,
			LastAttemptDate:    msg.UpdatedAt,
		})
		lines = append(lines, fmt.Sprintf("%s: %s", f.To, f.Comment))
	}
	if len(recipients) == 0 {
		return nil, fmt.Errorf("queue: no parseable failed recipients")
	}
	text := "Your message could not be delivered to one or more recipients.\n\n" +
		strings.Join(lines, "\n") + "\n"
	d := &maildsn.Message{
		From:         "postmaster@" + hostname,
		To:           from,
		Subject:      "Delivery Status Notification (Failure)",
		ReportingMTA: hostname,
		ArrivalDate:  msg.CreatedAt,
		TextBody:     text,
		Recipients:   recipients,
		MessageID:    fmt.Sprintf("<mailezine-dsn-%d@%s>", msg.ID, hostname),
	}
	return d.Compose()
}

func validAddress(addr string) bool {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	return true
}
