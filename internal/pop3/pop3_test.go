package pop3

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func startPOP3(t *testing.T, ms mailstore.MailboxStore) (string, func() error) {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	srv := &Server{
		Store:     ms,
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = srv.ServeConn(context.Background(), conn) }()
		}
	}()
	return ln.Addr().String(), dir.Close
}

type popClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dialPOP(t *testing.T, addr string) *popClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &popClient{t: t, conn: conn, r: bufio.NewReader(conn)}
	if line := c.readline(); !strings.HasPrefix(line, "+OK") {
		t.Fatalf("greeting: %q", line)
	}
	return c
}

func (c *popClient) cmd(line string) string {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.conn, "%s\r\n", line); err != nil {
		c.t.Fatal(err)
	}
	return c.readline()
}

func (c *popClient) readline() string {
	c.t.Helper()
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	return strings.TrimRight(line, "\r\n")
}

func (c *popClient) login() {
	c.t.Helper()
	if got := c.cmd("USER alice@example.com"); !strings.HasPrefix(got, "+OK") {
		c.t.Fatalf("USER: %q", got)
	}
	if got := c.cmd("PASS s3cret"); !strings.HasPrefix(got, "+OK") {
		c.t.Fatalf("PASS: %q", got)
	}
}

func seedMailbox(t *testing.T, ms mailstore.MailboxStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte(bodyOne),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{
		Data: []byte(bodyTwo),
	}); err != nil {
		t.Fatal(err)
	}
}

const (
	bodyOne = "From: a@x.test\r\nSubject: one\r\n\r\nbody one\r\n"
	bodyTwo = "From: b@x.test\r\nSubject: two\r\n\r\nbody two\r\n"
)

func TestPOP3Lifecycle(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	seedMailbox(t, ms)
	runPOP3Lifecycle(t, ms)
}

func TestPOP3AuthSASL(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	seedMailbox(t, ms)
	addr, _ := startPOP3(t, ms)

	t.Run("CAPA advertises SASL", func(t *testing.T) {
		c := dialPOP(t, addr)
		if got := c.cmd("CAPA"); !strings.HasPrefix(got, "+OK") {
			t.Fatalf("CAPA: %q", got)
		}
		var caps []string
		for {
			line := c.readline()
			if line == "." {
				break
			}
			caps = append(caps, line)
		}
		if !containsStr(caps, "SASL PLAIN LOGIN") {
			t.Fatalf("CAPA missing SASL: %v", caps)
		}
	})

	t.Run("PLAIN inline", func(t *testing.T) {
		c := dialPOP(t, addr)
		token := base64.StdEncoding.EncodeToString([]byte("\x00alice@example.com\x00s3cret"))
		if got := c.cmd("AUTH PLAIN " + token); !strings.HasPrefix(got, "+OK") {
			t.Fatalf("AUTH PLAIN inline: %q", got)
		}
		if got := c.cmd("STAT"); !strings.HasPrefix(got, "+OK 2 ") {
			t.Fatalf("STAT after AUTH: %q", got)
		}
	})

	t.Run("PLAIN challenge", func(t *testing.T) {
		c := dialPOP(t, addr)
		if got := c.cmd("AUTH PLAIN"); got != "+ " {
			t.Fatalf("PLAIN challenge: %q", got)
		}
		token := base64.StdEncoding.EncodeToString([]byte("\x00alice@example.com\x00s3cret"))
		if got := c.cmd(token); !strings.HasPrefix(got, "+OK") {
			t.Fatalf("PLAIN response: %q", got)
		}
	})

	t.Run("LOGIN challenge", func(t *testing.T) {
		c := dialPOP(t, addr)
		if got := c.cmd("AUTH LOGIN"); got != "+ VXNlcm5hbWU6" {
			t.Fatalf("LOGIN user challenge: %q", got)
		}
		user := base64.StdEncoding.EncodeToString([]byte("alice@example.com"))
		if got := c.cmd(user); got != "+ UGFzc3dvcmQ6" {
			t.Fatalf("LOGIN pass challenge: %q", got)
		}
		pass := base64.StdEncoding.EncodeToString([]byte("s3cret"))
		if got := c.cmd(pass); !strings.HasPrefix(got, "+OK") {
			t.Fatalf("LOGIN response: %q", got)
		}
	})

	t.Run("LOGIN inline", func(t *testing.T) {
		c := dialPOP(t, addr)
		user := base64.StdEncoding.EncodeToString([]byte("alice@example.com"))
		pass := base64.StdEncoding.EncodeToString([]byte("s3cret"))
		if got := c.cmd("AUTH LOGIN " + user); got != "+ UGFzc3dvcmQ6" {
			t.Fatalf("LOGIN inline user: %q", got)
		}
		if got := c.cmd(pass); !strings.HasPrefix(got, "+OK") {
			t.Fatalf("LOGIN inline pass: %q", got)
		}
	})

	t.Run("bad credentials", func(t *testing.T) {
		c := dialPOP(t, addr)
		token := base64.StdEncoding.EncodeToString([]byte("\x00alice@example.com\x00wrong"))
		if got := c.cmd("AUTH PLAIN " + token); !strings.HasPrefix(got, "-ERR") {
			t.Fatalf("bad password accepted: %q", got)
		}
	})

	t.Run("unsupported mechanism", func(t *testing.T) {
		c := dialPOP(t, addr)
		if got := c.cmd("AUTH CRAM-MD5"); !strings.HasPrefix(got, "-ERR") {
			t.Fatalf("unsupported mechanism: %q", got)
		}
	})
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func runPOP3Lifecycle(t *testing.T, ms mailstore.MailboxStore) {
	t.Helper()
	addr, _ := startPOP3(t, ms)
	c := dialPOP(t, addr)
	c.login()

	wantStat := fmt.Sprintf("+OK 2 %d", len(bodyOne)+len(bodyTwo))
	if got := c.cmd("STAT"); got != wantStat {
		t.Fatalf("STAT: %q", got)
	}
	if got := c.cmd("LIST"); !strings.HasPrefix(got, "+OK 2 messages") {
		t.Fatalf("LIST: %q", got)
	}
	for i := 0; i < 2; i++ {
		line := c.readline()
		if !strings.HasPrefix(line, "1 ") && !strings.HasPrefix(line, "2 ") {
			t.Fatalf("LIST item: %q", line)
		}
	}
	if got := c.readline(); got != "." {
		t.Fatalf("LIST terminator: %q", got)
	}

	// RETR with dot-stuffed body.
	if got := c.cmd("RETR 1"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("RETR: %q", got)
	}
	var retr []string
	for {
		line := c.readline()
		if line == "." {
			break
		}
		retr = append(retr, line)
	}
	joined := strings.Join(retr, "\n")
	if !strings.Contains(joined, "Subject: one") || !strings.Contains(joined, "body one") {
		t.Fatalf("RETR content: %q", joined)
	}

	if got := c.cmd("UIDL"); !strings.HasPrefix(got, "+OK 2 messages") {
		t.Fatalf("UIDL: %q", got)
	}
	c.readline()
	c.readline()
	if got := c.readline(); got != "." {
		t.Fatalf("UIDL terminator: %q", got)
	}

	if got := c.cmd("DELE 1"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("DELE: %q", got)
	}
	if got := c.cmd("STAT"); got != fmt.Sprintf("+OK 1 %d", len(bodyTwo)) {
		t.Fatalf("STAT after DELE: %q", got)
	}
	if got := c.cmd("RSET"); got != "+OK" {
		t.Fatalf("RSET: %q", got)
	}
	if got := c.cmd("STAT"); got != wantStat {
		t.Fatalf("STAT after RSET: %q", got)
	}
	if got := c.cmd("DELE 2"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("DELE 2: %q", got)
	}
	if got := c.cmd("QUIT"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("QUIT: %q", got)
	}

	// Only message 2 was deleted at QUIT.
	left, err := ms.ListMessages(context.Background(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].UID != 1 {
		t.Fatalf("remaining after QUIT: %+v", left)
	}
}

func TestPOP3AuthFailure(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	addr, _ := startPOP3(t, mailstore.NewKV(s))
	c := dialPOP(t, addr)
	c.cmd("USER alice@example.com")
	if got := c.cmd("PASS wrong"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("PASS: %q", got)
	}
	if got := c.cmd("STAT"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("STAT before auth: %q", got)
	}
}

func TestPOP3TopAndUnknown(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	seedMailbox(t, ms)
	runPOP3TopAndUnknown(t, ms)
}

func runPOP3TopAndUnknown(t *testing.T, ms mailstore.MailboxStore) {
	t.Helper()
	addr, _ := startPOP3(t, ms)
	c := dialPOP(t, addr)
	c.login()
	if got := c.cmd("TOP 1 1"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("TOP: %q", got)
	}
	for {
		line := c.readline()
		if line == "." {
			break
		}
	}
	if got := c.cmd("BOGUS"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("unknown command: %q", got)
	}
	if got := c.cmd("CAPA"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("CAPA: %q", got)
	}
}
