// ManageSieve (RFC 5804) server. v1 is read-only for scripts: scripts are
// rendered by the mailez control plane from the directory contract; write
// commands (PUTSCRIPT/SETACTIVE/DELETESCRIPT) answer NO with a clear reason
// instead of silently diverging from the control plane.
package sieve

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
	"strconv"
	"strings"
	"time"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
)

// Server is the ManageSieve listener backend.
type Server struct {
	Auth      auth.Service
	Directory directory.Service
	Scripts   mailstore.SieveStore
	Port      string      // listening port, passed to auth so the control plane recognizes webmail ports
	TLSConfig *tls.Config // optional; enables STARTTLS for direct deploys
	// Engine validates scripts on PUTSCRIPT/CHECKSCRIPT (RFC 5804 §2.5/2.6:
	// a script that does not compile must not be stored). Nil disables
	// validation.
	Engine *Engine
	Logger *slog.Logger
}

// ManageSieveSession handles one connection (used as the server.Listener
// Handler).
func (s *Server) ManageSieveSession(ctx context.Context, conn net.Conn) error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Minute))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	ses := &mSession{srv: s, conn: conn, r: r, w: w}
	if _, ok := conn.(*tls.Conn); ok {
		ses.tlsUp = true // implicit TLS: no STARTTLS needed
	}
	if err := ses.writeCapabilities(); err != nil {
		return err
	}
	return ses.loop(ctx)
}

type mSession struct {
	srv    *Server
	conn   net.Conn
	r      *bufio.Reader
	w      *bufio.Writer
	user   string
	authed bool
	tlsUp  bool
}

func (s *mSession) loop(ctx context.Context) error {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		cmd := strings.TrimRight(line, "\r\n")
		if cmd == "" {
			continue
		}
		done, err := s.handle(ctx, cmd)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// handle executes one command; done reports whether the connection must
// close (LOGOUT).
func (s *mSession) handle(ctx context.Context, cmd string) (bool, error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false, nil
	}
	verb := strings.ToUpper(fields[0])
	switch verb {
	case "LOGOUT":
		return true, s.status("OK", "bye")
	case "NOOP":
		return false, s.status("OK", "")
	case "CAPABILITY":
		return false, s.writeCapabilities()
	case "LOGIN":
		return false, s.handleLogin(cmd)
	case "AUTHENTICATE":
		return false, s.handleAuthenticate(cmd)
	case "LISTSCRIPTS":
		return false, s.handleListScripts(ctx)
	case "GETSCRIPT":
		return false, s.handleGetScript(ctx, fields)
	case "PUTSCRIPT":
		return false, s.handlePutScript(ctx, fields)
	case "CHECKSCRIPT":
		return false, s.handleCheckScript(fields)
	case "SETACTIVE":
		return false, s.handleSetActive(ctx, fields)
	case "DELETESCRIPT":
		return false, s.handleDeleteScript(ctx, fields)
	case "STARTTLS":
		return false, s.handleStartTLS()
	default:
		return false, s.status("NO", "unknown command")
	}
}

func (s *mSession) handleLogin(cmd string) error {
	args, err := parseQuotedArgs(cmd)
	if err != nil || len(args) != 2 {
		return s.status("NO", "LOGIN requires two quoted arguments")
	}
	return s.authenticate(args[0], args[1])
}

func (s *mSession) handleAuthenticate(cmd string) error {
	fields := strings.Fields(cmd)
	if len(fields) < 3 || !strings.EqualFold(strings.Trim(fields[1], `"`), "PLAIN") {
		return s.status("NO", "only PLAIN is supported")
	}
	raw := strings.Trim(fields[2], `"`)
	payload, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return s.status("NO", "invalid base64")
	}
	parts := strings.Split(string(payload), "\x00")
	if len(parts) != 3 {
		return s.status("NO", "invalid PLAIN payload")
	}
	return s.authenticate(parts[1], parts[2])
}

func (s *mSession) authenticate(email, password string) error {
	if s.srv.TLSConfig != nil && !s.tlsUp {
		return s.status("NO", "STARTTLS required before authentication")
	}
	ok, err := s.srv.Auth.Authenticate(context.Background(), email, password, auth.Options{
		Protocol: "sieve",
		Port:     s.srv.Port,
	})
	if err != nil || !ok {
		return s.status("NO", "authentication failed")
	}
	u, err := s.srv.Directory.User(context.Background(), email)
	if err != nil || !u.Enabled {
		return s.status("NO", "authentication failed")
	}
	s.user = email
	s.authed = true
	return s.status("OK", "")
}

func (s *mSession) handleListScripts(ctx context.Context) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if s.srv.Scripts != nil {
		scripts, err := s.srv.Scripts.ListSieveScripts(ctx, s.user)
		if err == nil && len(scripts) > 0 {
			for _, sc := range scripts {
				line := fmt.Sprintf(`"%s"`, escapeSieve(sc.Name))
				if sc.Active {
					line += " ACTIVE"
				}
				if err := s.line(line); err != nil {
					return err
				}
			}
			return s.status("OK", "")
		}
	}
	script, err := s.srv.Directory.Sieve(ctx, s.user)
	if err != nil {
		return s.status("NO", "directory lookup failed")
	}
	name := script.Name
	if name == "" {
		name = "default"
	}
	if err := s.line(fmt.Sprintf(`"%s" ACTIVE`, escapeSieve(name))); err != nil {
		return err
	}
	return s.status("OK", "")
}

func (s *mSession) handleGetScript(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if len(fields) != 2 {
		return s.status("NO", "GETSCRIPT requires a name")
	}
	name := strings.Trim(fields[1], `"`)
	if s.srv.Scripts != nil {
		if content, err := s.srv.Scripts.GetSieveScript(ctx, s.user, name); err == nil {
			if err := s.literal(content); err != nil {
				return err
			}
			return s.status("OK", "")
		} else if !errors.Is(err, mailstore.ErrNotFound) {
			return s.status("NO", "script storage error")
		}
	}
	if name != "" && name != "default" {
		return s.status("NO", "script not found")
	}
	script, err := s.srv.Directory.Sieve(ctx, s.user)
	if err != nil {
		return s.status("NO", "directory lookup failed")
	}
	body := script.Script
	if err := s.literal(body); err != nil {
		return err
	}
	return s.status("OK", "")
}

// handlePutScript consumes the literal body and stores the script.
func (s *mSession) handlePutScript(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if s.srv.Scripts == nil {
		return s.status("NO", "script storage not available")
	}
	if len(fields) != 3 {
		return s.status("NO", "PUTSCRIPT requires a name and literal size")
	}
	name := strings.Trim(fields[1], `"`)
	sizeSpec := strings.TrimSuffix(strings.Trim(fields[2], "{}"), "+")
	n, err := strconv.Atoi(sizeSpec)
	if err != nil || n < 0 {
		return s.status("NO", "invalid literal size")
	}
	content := make([]byte, n)
	if _, err := io.ReadFull(s.r, content); err != nil {
		return err
	}
	if _, err := s.r.ReadString('\n'); err != nil {
		return err
	}
	// RFC 5804 §2.6: reject scripts that do not compile instead of
	// storing a script that silently falls back to INBOX at delivery.
	if s.srv.Engine != nil {
		if err := s.srv.Engine.Check(string(content)); err != nil {
			return s.status("NO", "script does not compile: "+err.Error())
		}
	}
	if err := s.srv.Scripts.PutSieveScript(ctx, s.user, name, string(content), false); err != nil {
		return s.status("NO", "script storage error")
	}
	return s.status("OK", "")
}

// handleCheckScript validates a literal script without storing it
// (RFC 5804 §2.5).
func (s *mSession) handleCheckScript(fields []string) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if s.srv.Engine == nil {
		return s.status("NO", "script validation not available")
	}
	if len(fields) != 2 {
		return s.status("NO", "CHECKSCRIPT requires a literal size")
	}
	sizeSpec := strings.TrimSuffix(strings.Trim(fields[1], "{}"), "+")
	n, err := strconv.Atoi(sizeSpec)
	if err != nil || n < 0 {
		return s.status("NO", "invalid literal size")
	}
	content := make([]byte, n)
	if _, err := io.ReadFull(s.r, content); err != nil {
		return err
	}
	if _, err := s.r.ReadString('\n'); err != nil {
		return err
	}
	if err := s.srv.Engine.Check(string(content)); err != nil {
		return s.status("NO", "script does not compile: "+err.Error())
	}
	return s.status("OK", "")
}

func (s *mSession) handleSetActive(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if s.srv.Scripts == nil {
		return s.status("NO", "script storage not available")
	}
	if len(fields) != 2 {
		return s.status("NO", "SETACTIVE requires a name")
	}
	name := strings.Trim(fields[1], `"`)
	if err := s.srv.Scripts.SetSieveActive(ctx, s.user, name); err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return s.status("NO", "no such script")
		}
		return s.status("NO", "script storage error")
	}
	return s.status("OK", "")
}

func (s *mSession) handleDeleteScript(ctx context.Context, fields []string) error {
	if !s.authed {
		return s.status("NO", "authenticate first")
	}
	if s.srv.Scripts == nil {
		return s.status("NO", "script storage not available")
	}
	if len(fields) != 2 {
		return s.status("NO", "DELETESCRIPT requires a name")
	}
	name := strings.Trim(fields[1], `"`)
	if err := s.srv.Scripts.DeleteSieveScript(ctx, s.user, name); err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return s.status("NO", "no such script")
		}
		return s.status("NO", "script storage error")
	}
	return s.status("OK", "")
}

// sieveCapabilities is the exact extension set the interpreter accepts
// (internal/gosieve/interp load registry); the advertised SIEVE capability
// must match it, or clients gate features the server actually supports.
const sieveCapabilities = "fileinto reject ereject encoded-character envelope subaddress " +
	"environment body variables relational imap4flags copy " +
	"comparator-i;octet comparator-i;ascii-casemap comparator-i;ascii-numeric comparator-i;unicode-casemap " +
	"index editheader vacation spamtest spamtestplus regex date mailbox"

func (s *mSession) writeCapabilities() error {
	for _, line := range []string{
		`"IMPLEMENTATION" "mailezine"`,
		`"SIEVE" "` + sieveCapabilities + `"`,
		`"VERSION" "1.0"`,
	} {
		if err := s.line(line); err != nil {
			return err
		}
	}
	if s.srv.TLSConfig != nil && !s.tlsUp {
		if err := s.line(`"STARTTLS"`); err != nil {
			return err
		}
	}
	return s.status("OK", "mailezine ManageSieve ready")
}

// handleStartTLS upgrades the connection to TLS (RFC 5804 §2.2). The
// session resets to the unauthenticated state after the upgrade.
func (s *mSession) handleStartTLS() error {
	if s.srv.TLSConfig == nil {
		return s.status("NO", "STARTTLS not available")
	}
	if s.tlsUp {
		return s.status("NO", "TLS already active")
	}
	if s.authed {
		return s.status("NO", "STARTTLS must precede authentication")
	}
	if err := s.status("OK", "begin TLS"); err != nil {
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

func (s *mSession) line(l string) error {
	if _, err := s.w.WriteString(l + "\r\n"); err != nil {
		return err
	}
	return s.w.Flush()
}

// literal writes a {n+} literal followed by the payload.
func (s *mSession) literal(payload string) error {
	if _, err := s.w.WriteString("{" + strconv.Itoa(len(payload)) + "+}\r\n"); err != nil {
		return err
	}
	if _, err := s.w.WriteString(payload + "\r\n"); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *mSession) status(code, msg string) error {
	if msg == "" {
		return s.line(code)
	}
	return s.line(code + " " + msg)
}

// parseQuotedArgs splits a command into quoted string arguments.
func parseQuotedArgs(cmd string) ([]string, error) {
	var args []string
	rest := cmd
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return args, nil
		}
		if !strings.HasPrefix(rest, `"`) {
			return nil, errors.New("expected quoted string")
		}
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return nil, errors.New("unterminated string")
		}
		args = append(args, rest[1:1+end])
		rest = rest[2+end:]
	}
}

func escapeSieve(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
