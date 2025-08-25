// Package smtp implements the inbound and submission SMTP server
// (ARCHITECTURE.md §6.1), built on emersion/go-smtp (MIT) with RFC 5321
// session semantics.
//
// Authentication model (DECISIONS.md ADR-002): external clients are
// authenticated by the gateway (nginx auth_http → mailez backend); mailezine
// trusts the gateway subnet and skips its own auth. Direct deployments can
// authenticate with AUTH PLAIN through the auth.Service. Recipients are
// validated against the directory contract; trusted sessions may relay
// external recipients to the outbound queue.
package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/auth"
	"mailezine/internal/delivery"
	"mailezine/internal/directory"
)

// Backend wires the SMTP server to the mailezine services.
type Backend struct {
	Hostname           string
	Directory          directory.Service
	Auth               auth.Service
	TrustedNets        []*net.IPNet
	RequireAuth        bool   // true for the submission listener
	AllowRelay         bool   // trusted sessions may submit external recipients
	RecipientDelimiter string // extended-address separator ("" disables)
	MaxRecipients      int
	MaxMessageBytes    int64
	MaxLineLength      int
	TLSConfig          *tls.Config // optional; enables STARTTLS for direct deploys
	Logger             *slog.Logger

	// Submit enqueues a validated message. Called once per DATA with the
	// peer address, authenticated user ("" for anonymous/trusted-peer
	// sessions), envelope and raw message bytes (RFC 5322).
	Submit func(ctx context.Context, peer net.IP, user, from string, to []string, data []byte) error
}

// NewServer builds a go-smtp server from the backend.
func NewServer(b *Backend) *gosmtp.Server {
	if b.Logger == nil {
		b.Logger = slog.Default()
	}
	if b.MaxRecipients <= 0 {
		b.MaxRecipients = 100
	}
	if b.MaxMessageBytes <= 0 {
		b.MaxMessageBytes = 50 << 20
	}
	if b.MaxLineLength <= 0 {
		b.MaxLineLength = 1000
	}
	s := gosmtp.NewServer(gosmtp.BackendFunc(func(c *gosmtp.Conn) (gosmtp.Session, error) {
		return &session{backend: b, conn: c, trusted: b.isTrusted(remoteIP(c.Conn().RemoteAddr()))}, nil
	}))
	s.Domain = b.Hostname
	s.MaxRecipients = b.MaxRecipients
	s.MaxMessageBytes = b.MaxMessageBytes
	s.MaxLineLength = b.MaxLineLength
	s.TLSConfig = b.TLSConfig
	// With TLS configured the gateway no longer terminates it, so
	// credentials must only travel over an encrypted channel. Without TLS
	// (gateway model) plaintext AUTH on the internal link stays allowed.
	s.AllowInsecureAuth = b.TLSConfig == nil
	// Advertise the same extended capabilities as the mailez gateway
	// (8BITMIME/SIZE/PIPELINING are already default; DSN and SMTPUTF8 are
	// accepted and pass through — full DSN generation is not implemented).
	s.EnableDSN = true
	s.EnableSMTPUTF8 = true
	s.ErrorLog = slogAdapter{b.Logger}
	return s
}

type session struct {
	backend *Backend
	conn    *gosmtp.Conn
	trusted bool
	user    string
	from    string
	to      []string
}

func (s *session) AuthMechanisms() []string { return []string{"PLAIN"} }

func (s *session) Auth(mech string) (sasl.Server, error) {
	if !strings.EqualFold(mech, "PLAIN") {
		return nil, fmt.Errorf("smtp: unsupported mechanism %q", mech)
	}
	return sasl.NewPlainServer(func(_ string, username, password string) error {
		ok, err := s.backend.Auth.Authenticate(context.Background(), username, password, auth.Options{Protocol: "smtp"})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("smtp: invalid credentials")
		}
		s.user = username
		s.trusted = true
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	if from == "" || !strings.Contains(from, "@") {
		return &gosmtp.SMTPError{Code: 501, Message: "syntax error in MAIL FROM"}
	}
	if s.backend.RequireAuth && !s.trusted {
		return &gosmtp.SMTPError{Code: 530, EnhancedCode: gosmtp.EnhancedCode{5, 7, 0}, Message: "authentication required"}
	}
	// Outbound rate limiting (directory contract): enforced for trusted
	// sessions (gateway or authenticated). Remote inbound mail is not
	// sender-rate-limited here.
	if s.trusted {
		rate, err := s.backend.Directory.SenderRate(context.Background(), from)
		switch {
		case err != nil && !errors.Is(err, directory.ErrNotFound):
			return &gosmtp.SMTPError{Code: 451, Message: "temporary directory failure"}
		case err == nil && !rate.Allowed:
			msg := rate.Reason
			if msg == "" {
				msg = "rate limit exceeded"
			}
			return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: msg}
		}
	}
	// Sender identity (anti-spoofing): authenticated users may only send
	// from addresses the directory permits. Trusted-peer inbound mail
	// (gateway-forwarded external senders) is not restricted here.
	if s.user != "" {
		sender, err := s.backend.Directory.Sender(context.Background(), from)
		switch {
		case err == nil && sender.Allowed:
			if len(sender.Addresses) > 0 && !containsString(sender.Addresses, from) {
				return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "sender address not allowed"}
			}
		case err == nil && !sender.Allowed:
			return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "sender not allowed"}
		case err != nil && !errors.Is(err, directory.ErrNotFound):
			return &gosmtp.SMTPError{Code: 451, Message: "temporary directory failure"}
		default: // ErrNotFound: no sender record for this address
			return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "sender address not allowed"}
		}
	}
	s.from = from
	s.to = nil
	return nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if len(s.to) >= s.backend.MaxRecipients {
		return &gosmtp.SMTPError{Code: 552, Message: "too many recipients"}
	}
	if to == "" || !strings.Contains(to, "@") {
		return &gosmtp.SMTPError{Code: 501, Message: "syntax error in RCPT TO"}
	}
	// SRS: restore the original recipient of an SRS0 address before routing
	// (forwarding loops and redirected bounces).
	rcpt := to
	if strings.HasPrefix(rcpt, "SRS0=") {
		if restored, err := s.backend.Directory.SRSRestore(context.Background(), rcpt); err == nil && restored != "" {
			rcpt = restored
		}
	}
	// Local recipients resolve through the directory. Unknown recipients are
	// accepted as relay targets only when the session is trusted (gateway
	// subnet or authenticated) and relay is enabled; otherwise they are
	// refused to avoid open-relay and address enumeration.
	if _, err := s.backend.Directory.Aliases(context.Background(), rcpt); err != nil {
		// Extended addresses (user+tag@domain) deliver to the base user.
		if errors.Is(err, directory.ErrNotFound) && s.backend.RecipientDelimiter != "" {
			if base, ok := directory.SplitDelimited(rcpt, s.backend.RecipientDelimiter); ok {
				if _, err2 := s.backend.Directory.Aliases(context.Background(), base); err2 == nil {
					rcpt = base
					s.to = append(s.to, rcpt)
					return nil
				}
			}
		}
		if errors.Is(err, directory.ErrNotFound) {
			if s.backend.AllowRelay && s.trusted {
				s.to = append(s.to, rcpt)
				return nil
			}
			return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "relay access denied"}
		}
		return &gosmtp.SMTPError{Code: 451, Message: "temporary directory failure"}
	}
	s.to = append(s.to, rcpt)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if len(s.to) == 0 {
		return &gosmtp.SMTPError{Code: 503, Message: "no valid recipients"}
	}
	if s.backend.Submit == nil {
		return &gosmtp.SMTPError{Code: 451, Message: "delivery not configured"}
	}
	limit := s.backend.MaxMessageBytes
	data, err := io.ReadAll(r)
	if err != nil {
		// go-smtp enforces MaxMessageBytes on the DATA reader.
		if errors.Is(err, gosmtp.ErrDataTooLarge) {
			return &gosmtp.SMTPError{Code: 552, Message: "message exceeds size limit"}
		}
		if errors.Is(err, gosmtp.ErrTooLongLine) {
			return &gosmtp.SMTPError{Code: 554, Message: "line too long"}
		}
		return &gosmtp.SMTPError{Code: 451, Message: "error reading message"}
	}
	if int64(len(data)) > limit {
		return &gosmtp.SMTPError{Code: 552, Message: "message exceeds size limit"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer := remoteIP(s.conn.Conn().RemoteAddr())
	if err := s.backend.Submit(ctx, peer, s.user, s.from, s.to, data); err != nil {
		s.backend.Logger.Error("smtp: submit", "from", s.from, "to", s.to, "err", err)
		switch {
		case errors.Is(err, delivery.ErrReject):
			return &gosmtp.SMTPError{Code: 554, Message: "message rejected by policy"}
		case errors.Is(err, delivery.ErrSieveReject):
			return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 0}, Message: "message rejected by recipient policy"}
		case errors.Is(err, delivery.ErrGreylist), errors.Is(err, delivery.ErrSoftReject):
			return &gosmtp.SMTPError{Code: 451, Message: "try again later"}
		}
		return &gosmtp.SMTPError{Code: 451, Message: "temporary delivery failure"}
	}
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error { return nil }

func (b *Backend) isTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range b.TrustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(addr net.Addr) net.IP {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}
	return nil
}

// slogAdapter adapts slog to go-smtp's Logger interface.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Printf(format string, args ...any) {
	a.l.Error("smtp: "+format, args...)
}

func (a slogAdapter) Println(args ...any) {
	a.l.Error("smtp: " + fmt.Sprintln(args...))
}

var _ gosmtp.Session = (*session)(nil)
var _ gosmtp.AuthSession = (*session)(nil)
var _ gosmtp.Logger = slogAdapter{}
