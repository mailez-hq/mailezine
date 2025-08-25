// Package maildsn generates RFC 3464 delivery status notifications
// (multipart/report with a message/delivery-status part). The structure
// follows RFC 3464 §5-§6; the original message part is omitted (v1 keeps
// the notification compact).
package maildsn

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Action is the per-recipient delivery action.
type Action string

// DSN actions (RFC 3464 §2.3).
const (
	ActionFailed    Action = "failed"
	ActionDelayed   Action = "delayed"
	ActionDelivered Action = "delivered"
	ActionRelayed   Action = "relayed"
)

// Recipient is one delivery-status block.
type Recipient struct {
	FinalRecipient     string // rfc822 address
	Action             Action
	Status             string // e.g. 5.1.1
	StatusComment      string
	DiagnosticCodeSMTP string
	LastAttemptDate    time.Time
}

// Message is a full DSN.
type Message struct {
	From         string // postmaster address
	To           string
	Subject      string
	ReportingMTA string
	ArrivalDate  time.Time
	TextBody     string
	Recipients   []Recipient
	MessageID    string
}

// Compose renders the DSN as a complete RFC 5322 message.
func (m *Message) Compose() ([]byte, error) {
	boundary := newBoundary()
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", m.ArrivalDate.Format(time.RFC1123Z))
	if m.MessageID != "" {
		fmt.Fprintf(&b, "Message-ID: %s\r\n", m.MessageID)
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=delivery-status; boundary=%s\r\n", boundary)
	fmt.Fprintf(&b, "Auto-Submitted: auto-replied\r\n\r\n")

	// Human-readable part.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(m.TextBody)
	b.WriteString("\r\n")

	// Machine-readable delivery-status part.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Type: message/delivery-status\r\n\r\n")
	fmt.Fprintf(&b, "Reporting-MTA: dns; %s\r\n", m.ReportingMTA)
	fmt.Fprintf(&b, "Arrival-Date: %s\r\n", m.ArrivalDate.Format(time.RFC1123Z))
	if len(m.Recipients) > 0 {
		b.WriteString("\r\n")
	}
	for _, r := range m.Recipients {
		fmt.Fprintf(&b, "Final-Recipient: rfc822; %s\r\n", r.FinalRecipient)
		fmt.Fprintf(&b, "Action: %s\r\n", r.Action)
		status := r.Status
		if r.StatusComment != "" {
			status += " (" + r.StatusComment + ")"
		}
		fmt.Fprintf(&b, "Status: %s\r\n", status)
		if r.DiagnosticCodeSMTP != "" {
			fmt.Fprintf(&b, "Diagnostic-Code: smtp; %s\r\n", r.DiagnosticCodeSMTP)
		}
		if !r.LastAttemptDate.IsZero() {
			fmt.Fprintf(&b, "Last-Attempt-Date: %s\r\n", r.LastAttemptDate.Format(time.RFC1123Z))
		}
		b.WriteString("\r\n")
	}

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String()), nil
}

func newBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("mailezine-%d", time.Now().UnixNano())
	}
	return "mailezine-" + hex.EncodeToString(buf[:])
}
