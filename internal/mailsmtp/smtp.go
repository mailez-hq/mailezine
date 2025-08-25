// Package mailsmtp is the engine's outbound SMTP client. It implements the
// RFC 5321 dialogue (EHLO, MAIL, RCPT, DATA, QUIT), opportunistic and
// required STARTTLS (RFC 3207), SASL PLAIN authentication, and DANE TLSA
// verification (RFC 7672). One connection carries one message to a group of
// recipients and reports a per-recipient response; every network seam is
// injectable for tests.
package mailsmtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"

	"mailezine/internal/maildns"
)

// TLSMode controls STARTTLS behavior.
type TLSMode int

const (
	// TLSModeOpportunistic upgrades to TLS when advertised, falling back to
	// a plaintext retry when the upgrade fails (fail-open policy, D11).
	TLSModeOpportunistic TLSMode = iota
	// TLSModeRequired demands STARTTLS; a server that does not offer it is
	// a delivery failure.
	TLSModeRequired
)

// ErrNoTLSUpgrade is returned when an opportunistic STARTTLS attempt fails;
// the caller may re-dial with NoTLS set.
var ErrNoTLSUpgrade = errors.New("mailsmtp: opportunistic starttls failed")

// Response is the outcome of one SMTP command (usually one RCPT or DATA).
type Response struct {
	Code      int
	Permanent bool // 5xx
	Err       error
}

// ResponseError wraps a non-2xx SMTP response so callers can classify
// permanent vs temporary failures.
type ResponseError struct {
	Response
}

func (e *ResponseError) Error() string {
	msg := fmt.Sprintf("mailsmtp: smtp %d", e.Code)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Capabilities parsed from the EHLO reply.
type Capabilities struct {
	Size         int64 // -1 when not advertised
	Auth         []string
	StartTLS     bool
	EightBitMIME bool
	SMTPUTF8     bool
	Pipelining   bool
}

// AdvertisesAuth reports whether mech is offered (case-insensitive).
func (c Capabilities) AdvertisesAuth(mech string) bool {
	for _, m := range c.Auth {
		if strings.EqualFold(m, mech) {
			return true
		}
	}
	return false
}

// ConnOptions configures one outbound connection.
type ConnOptions struct {
	// Dialer used for the TCP connection; a default 30s dialer is used when
	// nil.
	Dialer *net.Dialer
	// Host is the SMTP server name used for EHLO greeting context, TLS SNI
	// and certificate verification. Defaults to the addr host.
	Host string
	// TLSMode selects opportunistic or required STARTTLS.
	TLSMode TLSMode
	// DANE holds the TLSA records for Host. When non-empty, TLS is required
	// and the presented chain must match one record (RFC 7672).
	DANE []maildns.TLSA
}

// Client is one SMTP session over a network connection.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	host    string
	cap     Capabilities
	greeted bool
	closed  bool
}

const (
	maxReplyLine = 8192
	maxEHLOArgs  = 64
)

// Dial connects to addr and returns a Client before any SMTP commands are
// sent. The caller must call Greet.
func Dial(ctx context.Context, addr string, opts ConnOptions) (*Client, error) {
	dialer := opts.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 30 * time.Second}
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mailsmtp: dial %s: %w", addr, err)
	}
	host := opts.Host
	if host == "" {
		host, _, _ = net.SplitHostPort(addr)
	}
	host = strings.TrimSuffix(host, ".")
	c := &Client{
		conn: conn,
		br:   bufio.NewReader(conn),
		bw:   bufio.NewWriter(conn),
		host: host,
		cap:  Capabilities{Size: -1},
	}

	return c, nil
}

// Greet sends EHLO and parses capabilities, then applies the TLS policy:
// required mode without STARTTLS is an error; opportunistic mode attempts
// STARTTLS when advertised. TLSMode/DANE/NoTLS are passed explicitly so the
// caller's intent is visible at the call site.
func (c *Client) Greet(ctx context.Context, ehloName string, mode TLSMode, dane []maildns.TLSA, noTLS bool) error {
	if ehloName == "" {
		ehloName = "localhost"
	}
	// Consume the server greeting (RFC 5321 §3.1: "220 <domain> ...").
	if lines, code, err := c.readReply(); err != nil {
		return err
	} else if code/100 != 2 {
		return &ResponseError{Response{Code: code, Permanent: code/100 == 5, Err: errors.New(strings.Join(lines, "; "))}}
	}
	if err := c.ehlo(ctx, ehloName); err != nil {
		return err
	}

	if noTLS {
		return nil
	}
	if mode == TLSModeRequired && !c.cap.StartTLS {
		return fmt.Errorf("mailsmtp: %s does not offer STARTTLS (required)", c.host)
	}
	if c.cap.StartTLS {
		if err := c.startTLS(ctx, ehloName, mode, dane); err != nil {
			if mode == TLSModeOpportunistic {
				_ = c.conn.Close()
				return ErrNoTLSUpgrade
			}
			return err
		}
	}
	return nil
}

func (c *Client) ehlo(ctx context.Context, ehloName string) error {
	if err := c.writef("EHLO %s\r\n", ehloName); err != nil {
		return err
	}
	lines, code, err := c.readReply()
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return &ResponseError{Response{Code: code, Permanent: code/100 == 5, Err: errors.New(strings.Join(lines, "; "))}}
	}
	c.greeted = true
	c.cap = parseEHLO(lines)
	return nil
}

// Capabilities returns the parsed EHLO capabilities.
func (c *Client) Capabilities() Capabilities { return c.cap }

// AuthPlain performs SASL PLAIN authentication.
func (c *Client) AuthPlain(ctx context.Context, username, password string) error {
	if !c.cap.AdvertisesAuth("PLAIN") {
		return fmt.Errorf("mailsmtp: %s does not advertise AUTH PLAIN", c.host)
	}
	client := sasl.NewPlainClient("", username, password)
	_, resp, err := client.Start()
	if err != nil {
		return err
	}
	if err := c.writef("AUTH PLAIN %s\r\n", base64.StdEncoding.EncodeToString(resp)); err != nil {
		return err
	}
	lines, code, err := c.readReply()
	if err != nil {
		return err
	}
	if code != 235 {
		return &ResponseError{Response{Code: code, Permanent: code/100 == 5, Err: errors.New(strings.Join(lines, "; "))}}
	}
	return nil
}

// Deliver sends one message to every recipient on a fresh transaction and
// returns a response per recipient (same order as to). A nil slice with a
// non-nil error means the transaction failed before per-recipient results
// existed (all recipients inherit the error). A non-nil slice means the
// transaction reached the per-recipient stage; each entry carries its own
// outcome, and Err is set for recipients that were rejected or whose DATA
// phase failed.
func (c *Client) Deliver(ctx context.Context, from string, to []string, body []byte) ([]Response, error) {
	if !c.greeted {
		return nil, errors.New("mailsmtp: Deliver before Greet")
	}
	if len(to) == 0 {
		return nil, nil
	}
	if err := c.writef("MAIL FROM:<%s>\r\n", sanitizePath(from)); err != nil {
		return nil, err
	}
	lines, code, err := c.readReply()
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, &ResponseError{Response{Code: code, Permanent: code/100 == 5, Err: errors.New(strings.Join(lines, "; "))}}
	}

	responses := make([]Response, len(to))
	accepted := make([]int, 0, len(to))
	for i, rcpt := range to {
		if err := c.writef("RCPT TO:<%s>\r\n", sanitizePath(rcpt)); err != nil {
			return nil, err
		}
		lines, code, err := c.readReply()
		if err != nil {
			return nil, err
		}
		r := Response{Code: code, Permanent: code/100 == 5}
		if code/100 != 2 {
			r.Err = errors.New(strings.Join(lines, "; "))
		} else {
			accepted = append(accepted, i)
		}
		responses[i] = r
	}

	if len(accepted) == 0 {
		_ = c.reset()
		return responses, nil
	}

	if err := c.writef("DATA\r\n"); err != nil {
		return nil, err
	}
	lines, code, err = c.readReply()
	if err != nil {
		return nil, err
	}
	if code != 354 {
		_ = c.reset()
		for _, i := range accepted {
			responses[i].Code = code
			responses[i].Permanent = code/100 == 5
			responses[i].Err = errors.New(strings.Join(lines, "; "))
		}
		return responses, nil
	}

	if err := c.writeData(body); err != nil {
		return nil, err
	}
	lines, code, err = c.readReply()
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		for _, i := range accepted {
			responses[i].Code = code
			responses[i].Permanent = code/100 == 5
			responses[i].Err = errors.New(strings.Join(lines, "; "))
		}
	}
	return responses, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	c.closed = true
	return c.conn.Close()
}

func (c *Client) startTLS(ctx context.Context, ehloName string, mode TLSMode, dane []maildns.TLSA) error {
	if err := c.writef("STARTTLS\r\n"); err != nil {
		return err
	}
	lines, code, err := c.readReply()
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return &ResponseError{Response{Code: code, Permanent: code/100 == 5, Err: errors.New(strings.Join(lines, "; "))}}
	}

	tlsConf := &tls.Config{
		ServerName: c.host,
		MinVersion: tls.VersionTLS12,
	}
	verifyDANE := false
	if len(dane) > 0 {
		// DANE replaces/augments PKIX trust (RFC 7672): skip the system
		// root check in the handshake and verify against the TLSA records
		// ourselves.
		verifyDANE = true
		tlsConf.InsecureSkipVerify = true
	}
	tconn := tls.Client(c.conn, tlsConf)
	if err := tconn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("mailsmtp: starttls with %s: %w", c.host, err)
	}
	if verifyDANE {
		if err := VerifyDANE(tconn.ConnectionState(), dane, c.host); err != nil {
			return err
		}
	}
	c.conn = tconn
	c.br = bufio.NewReader(tconn)
	c.bw = bufio.NewWriter(tconn)
	// RFC 3207: after the TLS handshake the client must discard all
	// capabilities and issue a fresh EHLO.
	return c.ehlo(ctx, ehloName)
}

// writeData sends the message body with dot-stuffing and the terminating
// CRLF.CRLF.
func (c *Client) writeData(body []byte) error {
	if len(body) == 0 {
		body = []byte("\r\n")
	} else if !bytes.HasSuffix(body, []byte("\r\n")) {
		if !bytes.HasSuffix(body, []byte("\n")) {
			body = append(body, '\r')
		}
		body = append(body, '\n')
	}
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] != '\n' {
			continue
		}
		content := body[start:i]
		if len(content) > 0 && content[len(content)-1] == '\r' {
			content = content[:len(content)-1]
		}
		if len(content) > 0 && content[0] == '.' {
			if err := c.bw.WriteByte('.'); err != nil {
				return err
			}
		}
		if _, err := c.bw.Write(body[start : i+1]); err != nil {
			return err
		}
		start = i + 1
	}
	if _, err := c.bw.WriteString(".\r\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

// reset issues RSET to abandon a failed transaction.
func (c *Client) reset() error {
	if err := c.writef("RSET\r\n"); err != nil {
		return err
	}
	_, _, err := c.readReply()
	return err
}

func (c *Client) writef(format string, args ...any) error {
	if _, err := fmt.Fprintf(c.bw, format, args...); err != nil {
		return err
	}
	return c.bw.Flush()
}

// readReply reads one SMTP reply, folding multiline responses into lines.
func (c *Client) readReply() ([]string, int, error) {
	var lines []string
	for {
		line, err := readLine(c.br)
		if err != nil {
			return nil, 0, err
		}
		if len(line) < 3 {
			return nil, 0, fmt.Errorf("mailsmtp: malformed reply %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return nil, 0, fmt.Errorf("mailsmtp: malformed reply code %q", line)
		}
		var text string
		if len(line) > 3 && line[3] == '-' {
			text = line[4:]
		} else {
			text = strings.TrimPrefix(line[3:], " ")
		}
		lines = append(lines, text)
		if len(line) > 3 && line[3] == '-' {
			continue
		}
		return lines, code, nil
	}
}

// readLine reads one CRLF- or LF-terminated line with a size cap.
func readLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			if len(buf) > maxReplyLine {
				return "", errors.New("mailsmtp: reply line too long")
			}
			continue
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		break
	}
	s := strings.TrimRight(string(buf), "\r\n")
	if len(s) > maxReplyLine {
		return "", errors.New("mailsmtp: reply line too long")
	}
	return s, nil
}

// parseEHLO turns a multiline 250 reply into capabilities.
func parseEHLO(lines []string) Capabilities {
	c := Capabilities{Size: -1}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := strings.ToUpper(fields[0])
		switch key {
		case "STARTTLS":
			c.StartTLS = true
		case "SIZE":
			c.Size = -1
			if len(fields) > 1 {
				if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					c.Size = n
				}
			}
		case "AUTH":
			c.Auth = append(c.Auth, fields[1:]...)
		case "8BITMIME":
			c.EightBitMIME = true
		case "SMTPUTF8":
			c.SMTPUTF8 = true
		case "PIPELINING":
			c.Pipelining = true
		}
	}
	return c
}

// sanitizePath strips angle brackets and CR/LF from an address so a hostile
// envelope value cannot inject SMTP commands.
func sanitizePath(addr string) string {
	addr = strings.ReplaceAll(addr, "\r", "")
	addr = strings.ReplaceAll(addr, "\n", "")
	return strings.Trim(addr, "<> ")
}

// VerifyDANE validates the TLS connection state against RFC 6698 TLSA
// records (RFC 7672). Any single matching record is a pass; when records
// exist but none match, the connection is rejected.
func VerifyDANE(state tls.ConnectionState, records []maildns.TLSA, serverName string) error {
	if len(records) == 0 {
		return nil
	}
	if len(state.PeerCertificates) == 0 {
		return errors.New("mailsmtp: dane: no peer certificates")
	}

	var verifiedChains [][]*x509.Certificate
	leaf := state.PeerCertificates[0]
	// PKIX usages need a chain that verifies against the system roots;
	// DANE usages rely on the presented chain only.
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	inter := x509.NewCertPool()
	for _, c := range state.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		DNSName:       serverName,
	}); err == nil {
		verifiedChains = chains
	}

	for _, rec := range records {
		var candidates []*x509.Certificate
		switch rec.Usage {
		case 0: // PKIX-TA
			for _, chain := range verifiedChains {
				if n := len(chain); n > 0 {
					candidates = append(candidates, chain[n-1])
				}
			}
		case 1: // PKIX-EE
			for _, chain := range verifiedChains {
				if len(chain) > 0 {
					candidates = append(candidates, chain[0])
				}
			}
		case 2: // DANE-TA
			candidates = append(candidates, state.PeerCertificates[len(state.PeerCertificates)-1])
		case 3: // DANE-EE
			candidates = append(candidates, leaf)
		default:
			continue
		}
		for _, cert := range candidates {
			if tlsaMatches(cert, rec) {
				return nil
			}
		}
	}
	return fmt.Errorf("mailsmtp: dane: no TLSA record matches %s", serverName)
}

// tlsaMatches compares one certificate against one TLSA record.
func tlsaMatches(cert *x509.Certificate, rec maildns.TLSA) bool {
	var data []byte
	switch rec.Selector {
	case 0: // full certificate
		data = cert.Raw
	case 1: // subject public key info
		data = cert.RawSubjectPublicKeyInfo
	default:
		return false
	}
	switch rec.MatchingType {
	case 0: // exact match
		return string(rec.Cert) == string(data)
	case 1: // sha256
		sum := sha256.Sum256(data)
		return string(rec.Cert) == string(sum[:])
	case 2: // sha512
		sum := sha512.Sum512(data)
		return string(rec.Cert) == string(sum[:])
	default:
		return false
	}
}
