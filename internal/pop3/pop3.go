// Package pop3 implements a minimal POP3 server (RFC 1939 + CAPA/UIDL/TOP).
// Deletion follows the classic model: DELE marks a message; only QUIT
// commits the expunge. TLS is terminated by the gateway, like SMTP/IMAP.
package pop3

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// Server is the POP3 listener backend.
type Server struct {
	Store     mailstore.MailboxStore
	Auth      auth.Service
	Directory directory.Service
	TLSConfig *tls.Config // optional; enables STLS for direct deploys
	Logger    *slog.Logger
}

// ServeConn handles one POP3 session (server.Listener Handler).
func (s *Server) ServeConn(ctx context.Context, conn net.Conn) error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	ses := &session{srv: s, conn: conn, r: r, w: w}
	if _, ok := conn.(*tls.Conn); ok {
		ses.tlsUp = true // implicit TLS (pop3s): no STLS needed
	}
	return ses.run(ctx)
}

type message struct {
	uid  uint32
	size int64
}

type session struct {
	srv      *Server
	conn     net.Conn
	r        *bufio.Reader
	w        *bufio.Writer
	user     string
	authUser string // decoded username while a LOGIN exchange is in flight
	authed   bool
	tlsUp    bool
	msgs     []message // INBOX snapshot at login, ordered by UID
	deleted  map[uint32]bool
}

func (s *session) run(ctx context.Context) error {
	if err := s.reply("+OK mailezine POP3 ready"); err != nil {
		return err
	}
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return nil // EOF or deadline: no QUIT means nothing is committed
		}
		cmd := strings.TrimRight(line, "\r\n")
		if cmd == "" {
			continue
		}
		quit, err := s.handle(ctx, cmd)
		if err != nil {
			return err
		}
		if quit {
			return nil
		}
	}
}

func (s *session) handle(ctx context.Context, cmd string) (bool, error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false, s.reply("-ERR empty command")
	}
	verb := strings.ToUpper(fields[0])
	switch verb {
	case "QUIT":
		if err := s.commit(ctx); err != nil {
			return true, err
		}
		return true, s.reply("+OK bye")
	case "USER":
		if len(fields) != 2 {
			return false, s.reply("-ERR usage: USER <name>")
		}
		s.user = fields[1]
		return false, s.reply("+OK send PASS")
	case "AUTH":
		return false, s.handleAuth(ctx, fields)
	case "PASS":
		if len(fields) != 2 || s.user == "" {
			return false, s.reply("-ERR usage: PASS <password>")
		}
		if err := s.login(ctx, fields[1]); err != nil {
			return false, s.reply("-ERR authentication failed")
		}
		return false, s.reply("+OK mailbox locked and ready")
	case "STAT":
		if !s.authed {
			return false, s.reply("-ERR authenticate first")
		}
		count, size := s.stats()
		return false, s.reply(fmt.Sprintf("+OK %d %d", count, size))
	case "LIST":
		return false, s.handleList(fields)
	case "UIDL":
		return false, s.handleUIDL(fields)
	case "RETR":
		return false, s.handleRetr(ctx, fields)
	case "TOP":
		return false, s.handleTop(ctx, fields)
	case "DELE":
		return false, s.handleDele(fields)
	case "NOOP":
		return false, s.reply("+OK")
	case "RSET":
		s.deleted = map[uint32]bool{}
		return false, s.reply("+OK")
	case "CAPA":
		return false, s.handleCapa()
	case "STLS":
		return false, s.handleSTLS()
	default:
		return false, s.reply("-ERR unknown command")
	}
}

func (s *session) login(ctx context.Context, password string) error {
	if s.srv.TLSConfig != nil && !s.tlsUp {
		return errors.New("TLS required before authentication")
	}
	ok, err := s.srv.Auth.Authenticate(ctx, s.user, password, auth.Options{Protocol: "pop3"})
	if err != nil || !ok {
		return errors.New("bad credentials")
	}
	u, err := s.srv.Directory.User(ctx, s.user)
	if err != nil || !u.Enabled {
		return errors.New("account disabled")
	}
	msgs, err := s.srv.Store.ListMessages(ctx, s.user, "INBOX")
	if err != nil {
		return err
	}
	for _, m := range msgs {
		s.msgs = append(s.msgs, message{uid: m.UID, size: m.Size})
	}
	sort.Slice(s.msgs, func(i, j int) bool { return s.msgs[i].uid < s.msgs[j].uid })
	s.deleted = map[uint32]bool{}
	s.authed = true
	return nil
}

// handleAuth implements the POP3 SASL AUTH extension (RFC 1734 + RFC 5034)
// for PLAIN (RFC 4616) and LOGIN. Both mechanisms accept the initial
// response inline or via the classic "+ " challenge exchange.
func (s *session) handleAuth(ctx context.Context, fields []string) error {
	if s.authed {
		return s.reply("-ERR already authenticated")
	}
	if s.srv.TLSConfig != nil && !s.tlsUp {
		return s.reply("-ERR TLS required before authentication")
	}
	if len(fields) < 2 {
		return s.reply("-ERR usage: AUTH <mechanism> [initial-response]")
	}
	switch mech := strings.ToUpper(fields[1]); mech {
	case "PLAIN":
		if len(fields) >= 3 {
			return s.authPlain(ctx, fields[2])
		}
		if err := s.reply("+ "); err != nil {
			return err
		}
		line, err := s.readLine()
		if err != nil {
			return err
		}
		return s.authPlain(ctx, strings.TrimSpace(line))
	case "LOGIN":
		if len(fields) >= 3 {
			u, err := decodeBase64(fields[2])
			if err != nil {
				return s.reply("-ERR invalid base64")
			}
			s.authUser = u
		} else {
			if err := s.reply("+ VXNlcm5hbWU6"); err != nil { // "Username:"
				return err
			}
			line, err := s.readLine()
			if err != nil {
				return err
			}
			u, err := decodeBase64(strings.TrimSpace(line))
			if err != nil {
				return s.reply("-ERR invalid base64")
			}
			s.authUser = u
		}
		if err := s.reply("+ UGFzc3dvcmQ6"); err != nil { // "Password:"
			return err
		}
		line, err := s.readLine()
		if err != nil {
			return err
		}
		p, err := decodeBase64(strings.TrimSpace(line))
		if err != nil {
			return s.reply("-ERR invalid base64")
		}
		s.user = s.authUser
		s.authUser = ""
		if err := s.login(ctx, p); err != nil {
			return s.reply("-ERR authentication failed")
		}
		return s.reply("+OK mailbox locked and ready")
	default:
		return s.reply("-ERR unsupported authentication mechanism")
	}
}

func (s *session) authPlain(ctx context.Context, token string) error {
	raw, err := decodeBase64(token)
	if err != nil {
		return s.reply("-ERR invalid base64")
	}
	// RFC 4616: [authzid] NUL authcid NUL passwd.
	parts := strings.Split(string(raw), "\x00")
	var user, pass string
	switch len(parts) {
	case 3:
		user, pass = parts[1], parts[2]
	case 2:
		user, pass = parts[0], parts[1]
	default:
		return s.reply("-ERR malformed credentials")
	}
	if user == "" {
		return s.reply("-ERR malformed credentials")
	}
	s.user = user
	if err := s.login(ctx, pass); err != nil {
		return s.reply("-ERR authentication failed")
	}
	return s.reply("+OK mailbox locked and ready")
}

func decodeBase64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return string(b), err
}

func (s *session) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (s *session) stats() (count int, size int64) {
	for _, m := range s.msgs {
		if s.deleted[m.uid] {
			continue
		}
		count++
		size += m.size
	}
	return count, size
}

func (s *session) handleList(fields []string) error {
	if !s.authed {
		return s.reply("-ERR authenticate first")
	}
	if len(fields) == 2 {
		idx, err := strconv.Atoi(fields[1])
		m, ok := s.messageAt(idx)
		if err != nil || !ok {
			return s.reply("-ERR no such message")
		}
		return s.reply(fmt.Sprintf("+OK %d %d", idx, m.size))
	}
	count, size := s.stats()
	if err := s.reply(fmt.Sprintf("+OK %d messages (%d octets)", count, size)); err != nil {
		return err
	}
	for i, m := range s.msgs {
		if s.deleted[m.uid] {
			continue
		}
		if err := s.line(fmt.Sprintf("%d %d", i+1, m.size)); err != nil {
			return err
		}
	}
	return s.term()
}

func (s *session) handleUIDL(fields []string) error {
	if !s.authed {
		return s.reply("-ERR authenticate first")
	}
	if len(fields) == 2 {
		idx, err := strconv.Atoi(fields[1])
		m, ok := s.messageAt(idx)
		if err != nil || !ok {
			return s.reply("-ERR no such message")
		}
		return s.reply(fmt.Sprintf("+OK %d %d", idx, m.uid))
	}
	if err := s.reply(fmt.Sprintf("+OK %d messages", len(s.msgs))); err != nil {
		return err
	}
	for i, m := range s.msgs {
		if err := s.line(fmt.Sprintf("%d %d", i+1, m.uid)); err != nil {
			return err
		}
	}
	return s.term()
}

func (s *session) handleRetr(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.reply("-ERR authenticate first")
	}
	_, m, ok := s.message(fields)
	if !ok {
		return s.reply("-ERR no such message")
	}
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, "INBOX", m.uid)
	if err != nil {
		return s.reply("-ERR cannot retrieve")
	}
	defer rc.Close()
	if err := s.reply(fmt.Sprintf("+OK %d octets", m.size)); err != nil {
		return err
	}
	if err := s.dotCopy(rc); err != nil {
		return err
	}
	return s.term()
}

func (s *session) handleTop(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.reply("-ERR authenticate first")
	}
	if len(fields) != 3 {
		return s.reply("-ERR usage: TOP <msg> <n>")
	}
	idx, err := strconv.Atoi(fields[1])
	if err != nil {
		return s.reply("-ERR no such message")
	}
	m, ok := s.messageAt(idx)
	if !ok {
		return s.reply("-ERR no such message")
	}
	n, err := strconv.Atoi(fields[2])
	if err != nil || n < 0 {
		return s.reply("-ERR invalid line count")
	}
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, "INBOX", m.uid)
	if err != nil {
		return s.reply("-ERR cannot retrieve")
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return s.reply("-ERR cannot retrieve")
	}
	if err := s.reply(fmt.Sprintf("+OK top of message %d", s.messageIndex(m.uid))); err != nil {
		return err
	}
	// Headers plus up to n body lines.
	parts := strings.SplitN(string(data), "\r\n\r\n", 2)
	if err := s.line(dotStuff(parts[0])); err != nil {
		return err
	}
	if err := s.line(""); err != nil {
		return err
	}
	if len(parts) == 2 {
		bodyLines := strings.Split(parts[1], "\r\n")
		if n > len(bodyLines) {
			n = len(bodyLines)
		}
		for _, l := range bodyLines[:n] {
			if err := s.line(dotStuff(l)); err != nil {
				return err
			}
		}
	}
	return s.term()
}

func (s *session) handleDele(fields []string) error {
	if !s.authed {
		return s.reply("-ERR authenticate first")
	}
	_, m, ok := s.message(fields)
	if !ok {
		return s.reply("-ERR no such message")
	}
	if s.deleted[m.uid] {
		return s.reply("-ERR already deleted")
	}
	s.deleted[m.uid] = true
	return s.reply("+OK marked deleted")
}

func (s *session) handleCapa() error {
	caps := []string{"+OK Capability list follows", "USER", "UIDL", "TOP", "RESP-CODES", "SASL PLAIN LOGIN"}
	if s.srv.TLSConfig != nil && !s.tlsUp {
		caps = append(caps, "STLS")
	}
	caps = append(caps, ".")
	for _, line := range caps {
		if err := s.line(line); err != nil {
			return err
		}
	}
	return nil
}

// handleSTLS upgrades the connection to TLS (RFC 2595 STLS). After the
// upgrade the session resets to the unauthenticated state, as required.
func (s *session) handleSTLS() error {
	if s.srv.TLSConfig == nil {
		return s.reply("-ERR STLS not available")
	}
	if s.tlsUp {
		return s.reply("-ERR TLS already active")
	}
	if s.authed {
		return s.reply("-ERR TLS must be negotiated before authentication")
	}
	if err := s.reply("+OK Begin TLS negotiation"); err != nil {
		return err
	}
	tlsConn := tls.Server(s.conn, s.srv.TLSConfig)
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	s.conn = tlsConn
	s.r = bufio.NewReader(tlsConn)
	s.w = bufio.NewWriter(tlsConn)
	s.tlsUp = true
	s.user = ""
	return nil
}

// commit expunges the messages marked deleted during the session.
func (s *session) commit(ctx context.Context) error {
	if !s.authed || len(s.deleted) == 0 {
		return nil
	}
	var uids []uint32
	for uid := range s.deleted {
		uids = append(uids, uid)
	}
	if _, err := s.srv.Store.Expunge(ctx, s.user, "INBOX", uids); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.srv.Logger.Error("pop3: expunge on quit", "user", s.user, "err", err)
	}
	return nil
}

// message resolves a command argument to a message by 1-based index.
func (s *session) message(fields []string) (int, message, bool) {
	if len(fields) != 2 {
		return 0, message{}, false
	}
	idx, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, message{}, false
	}
	m, ok := s.messageAt(idx)
	return idx, m, ok
}

func (s *session) messageAt(idx int) (message, bool) {
	if idx < 1 || idx > len(s.msgs) {
		return message{}, false
	}
	m := s.msgs[idx-1]
	if s.deleted[m.uid] {
		return message{}, false
	}
	return m, true
}

func (s *session) messageIndex(uid uint32) int {
	for i, m := range s.msgs {
		if m.uid == uid {
			return i + 1
		}
	}
	return 0
}

func (s *session) reply(line string) error {
	return s.line(line)
}

func (s *session) line(line string) error {
	if _, err := s.w.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return s.w.Flush()
}

// term ends a multi-line response.
func (s *session) term() error {
	return s.line(".")
}

// dotCopy streams a message with dot-stuffing.
func (s *session) dotCopy(r io.Reader) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if err := s.line(dotStuff(trimmed)); err != nil {
				return err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// dotStuff prefixes a lone "." with another "." (RFC 1939 §3).
func dotStuff(line string) string {
	if strings.HasPrefix(line, ".") {
		return "." + line
	}
	return line
}
