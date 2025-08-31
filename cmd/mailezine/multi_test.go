//go:build mailez_ee

// Multi-active drill: two engine PROCESSES over one TiDB KV and one shared
// blob directory — the production multi-active shape, compressed onto one
// host. Pins the distributed semantics end to end:
//
//  1. both nodes report role=active (no leader/standby);
//  2. mail delivered through node A is readable through node B (shared
//     mailbox state, cross-node IMAP);
//  3. an outbound message claimed by node A and killed mid-delivery is
//     stolen and delivered by node B once the claim lease lapses —
//     exactly once at the sink, with the queue reaching "delivered".
//
// Requires a live MySQL-protocol server (MAILEZINE_TEST_TIDB_DSN, same
// gate as the store TiDB contract tests); skips otherwise. The engine's
// fixed "mailezine_kv" table is dropped before and after for isolation.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	_ "github.com/go-sql-driver/mysql"

	"mailezine/internal/store"
)

// gatedSink is a hand-rolled MX that stalls the FIRST delivery after
// end-of-DATA (upstream hangs) and completes every later one. It models
// the crash window the claim lease exists for.
type gatedSink struct {
	addr string

	mu         sync.Mutex
	deliveries []string
	gotData    chan struct{}
	release    chan struct{}
	dataOnce   sync.Once
}

func newGatedSink(t *testing.T) *gatedSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &gatedSink{
		addr:    ln.Addr().String(),
		gotData: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *gatedSink) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}
	write("220 sink.test")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250 sink.test")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			write("250 ok")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 go ahead")
			var body []string
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				l = strings.TrimRight(l, "\r\n")
				if l == "." {
					break
				}
				body = append(body, l)
			}
			s.dataOnce.Do(func() { close(s.gotData) })
			<-s.release // first delivery hangs here while its node is killed
			s.record(strings.Join(body, "\n"))
			write("250 ok queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (s *gatedSink) record(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, body)
}

func (s *gatedSink) countContaining(needle string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.deliveries {
		if strings.Contains(d, needle) {
			n++
		}
	}
	return n
}

func multiEnv(t *testing.T, dirFile, authFile, rocksPath, dsn, health, smtp, sub, imap, mgmt, msieve, pop3, sinkHost, sinkPort string) []string {
	t.Helper()
	return []string{
		"MAILEZINE_CLUSTER_MODE=multi",
		"MAILEZINE_STORAGE_BACKEND=tidb",
		"MAILEZINE_STORAGE_DSN=" + dsn,
		// Shared RocksPath ⇒ shared "<path>.blobs" blob directory: node A
		// writes the spool, node B delivers it.
		"MAILEZINE_ROCKS_PATH=" + rocksPath,
		"MAILEZINE_DIRECTORY_MODE=dev",
		"MAILEZINE_DIRECTORY_FILE=" + dirFile,
		"MAILEZINE_AUTH_MODE=dev",
		"MAILEZINE_AUTH_DEV_FILE=" + authFile,
		"MAILEZINE_HEALTH_ADDR=" + health,
		"MAILEZINE_SMTP_ADDR=" + smtp,
		"MAILEZINE_SUBMISSION_ADDR=" + sub,
		"MAILEZINE_IMAP_ADDR=" + imap,
		"MAILEZINE_MANAGESIEVE_ADDR=" + msieve,
		"MAILEZINE_POP3_ADDR=" + pop3,
		"MAILEZINE_MANAGEMENT_ADDR=" + mgmt,
		"MAILEZINE_MANAGEMENT_SECRET=drill-secret",
		"MAILEZINE_OUTBOUND_ENABLED=true",
		"MAILEZINE_OUTBOUND_FIXED_HOST=" + sinkHost,
		"MAILEZINE_OUTBOUND_FIXED_PORT=" + sinkPort,
		// Fast claim cycling for the drill: B steals within ~3s of the kill.
		"MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS=1",
		"MAILEZINE_QUEUE_CLAIM_LEASE_SECONDS=2",
		"MAILEZINE_QUEUE_MAX_ATTEMPTS=6",
		"MAILEZINE_FTS_ENABLED=false",
		"MAILEZINE_LOG_LEVEL=warn",
	}
}

func startMultiNode(t *testing.T, env []string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(buildEngine(t))
	cmd.Env = append(os.Environ(), env...)
	// Engine diagnostics straight into the test log: a node that exits or
	// 451s otherwise fails the drill without a reason.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

func waitMultiRole(t *testing.T, addr, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if haRole(t, addr) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("engine at %s never reached role=%s", addr, want)
}

func TestMultiActiveFailover(t *testing.T) {
	dsn := os.Getenv("MAILEZINE_TEST_TIDB_DSN")
	if dsn == "" {
		t.Skip("set MAILEZINE_TEST_TIDB_DSN to run the multi-active drill")
	}
	if store.LookupKVOpener("tidb") == nil {
		t.Skip("TiDB backend not registered (enterprise build only)")
	}

	// The engine opens the fixed table "mailezine_kv": drop it around the
	// run so repeated drills start clean.
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() { _, _ = db.Exec("DROP TABLE IF EXISTS `mailezine_kv`") }
	drop()
	t.Cleanup(func() { drop(); _ = db.Close() })

	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks-shared")
	writeDevFiles(t, dirFile, authFile)

	aHealth := "127.0.0.1:" + freePort(t)
	aSMTP := "127.0.0.1:" + freePort(t)
	aSub := "127.0.0.1:" + freePort(t)
	aIMAP := "127.0.0.1:" + freePort(t)
	aMgmt := "127.0.0.1:" + freePort(t)
	aSieve := "127.0.0.1:" + freePort(t)
	aPOP3 := "127.0.0.1:" + freePort(t)
	bHealth := "127.0.0.1:" + freePort(t)
	bSMTP := "127.0.0.1:" + freePort(t)
	bSub := "127.0.0.1:" + freePort(t)
	bIMAP := "127.0.0.1:" + freePort(t)
	bMgmt := "127.0.0.1:" + freePort(t)
	bSieve := "127.0.0.1:" + freePort(t)
	bPOP3 := "127.0.0.1:" + freePort(t)

	sink := newGatedSink(t)
	sinkHost, sinkPort, _ := net.SplitHostPort(sink.addr)

	nodeA := startMultiNode(t, multiEnv(t, dirFile, authFile, rocksPath, dsn,
		aHealth, aSMTP, aSub, aIMAP, aMgmt, aSieve, aPOP3, sinkHost, sinkPort))
	startMultiNode(t, multiEnv(t, dirFile, authFile, rocksPath, dsn,
		bHealth, bSMTP, bSub, bIMAP, bMgmt, bSieve, bPOP3, sinkHost, sinkPort))

	// (1) Both nodes are active peers.
	waitMultiRole(t, aHealth, "active")
	waitMultiRole(t, bHealth, "active")

	// (2) Deliver through node A, read through node B: shared mailbox
	// state, no affinity anywhere.
	inbound, err := gosmtp.Dial(aSMTP)
	if err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(inbound, "sender@remote.test", []string{"alice@example.com"},
		"From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: cross node\r\n\r\ncross-node body\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = inbound.Close()

	imapClient, err := imapclient.DialInsecure(bIMAP, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := imapClient.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := imapClient.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetched, err := imapClient.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || len(fetched[0].BodySection) != 1 ||
		!strings.Contains(string(fetched[0].BodySection[0].Bytes), "cross-node body") {
		t.Fatalf("cross-node IMAP fetch: %d results", len(fetched))
	}
	_ = imapClient.Logout().Wait()

	// (3) Claim takeover: node A submits an outbound message, claims it,
	// and is killed mid-DATA. Node B must steal the claim after the lease
	// lapses and complete the delivery exactly once.
	sub, err := gosmtp.Dial(aSub)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(sub, "alice@example.com", []string{"external@sink.test"},
		"From: alice@example.com\r\nTo: external@sink.test\r\nSubject: drill\r\n\r\nfailover drill body\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = sub.Close()

	// Wait until node A's worker is inside DATA (claim held), then kill it.
	select {
	case <-sink.gotData:
	case <-time.After(15 * time.Second):
		t.Fatal("node A never reached the sink's DATA phase")
	}
	if err := nodeA.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = nodeA.Process.Wait()
	close(sink.release) // unblock the stalled session; B's retry completes

	deadline := time.Now().Add(20 * time.Second)
	for {
		if n := sink.countContaining("failover drill body"); n >= 1 {
			if n != 1 {
				t.Fatalf("sink saw %d completed deliveries, want exactly 1", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node B never delivered the stolen message")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The queue row reached its terminal state through node B.
	mgmtDeadline := time.Now().Add(10 * time.Second)
	for {
		state := queueStateFor(t, bMgmt, "external@sink.test")
		if state == "delivered" {
			break
		}
		if state != "" && state != "active" && state != "queued" && state != "deferred" {
			t.Fatalf("queue state = %q, want delivered", state)
		}
		if time.Now().After(mgmtDeadline) {
			t.Fatalf("queue never reached delivered (last state %q)", state)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// queueStateFor reads node B's management API and returns the state of the
// first queued message addressed to the given recipient ("" while absent).
func queueStateFor(t *testing.T, mgmtAddr, rcpt string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+mgmtAddr+"/v1/queue", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer drill-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var view struct {
		Messages []struct {
			State      string   `json:"state"`
			Recipients []string `json:"recipients"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &view) != nil {
		return ""
	}
	for _, m := range view.Messages {
		for _, r := range m.Recipients {
			if r == rcpt {
				return m.State
			}
		}
	}
	return ""
}
