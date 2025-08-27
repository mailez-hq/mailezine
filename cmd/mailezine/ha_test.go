// HA failover e2e: two engine processes share one KV volume and one lease
// file. A is leader and accepts mail; B stays on standby (no listeners).
// When A exits, B takes over and serves the same data.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

var (
	engineBinOnce sync.Once
	engineBin     string
	engineBinErr  error
)

func buildEngine(t *testing.T) string {
	t.Helper()
	engineBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mailezine-ha-bin-")
		if err != nil {
			engineBinErr = err
			return
		}
		exe := "mailezine"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		engineBin = filepath.Join(dir, exe)
		cmd := exec.Command("go", "build", "-o", engineBin, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			engineBinErr = fmt.Errorf("build engine: %v: %s", err, out)
		}
	})
	if engineBinErr != nil {
		t.Fatal(engineBinErr)
	}
	return engineBin
}

func haEnv(t *testing.T, dir, rocksPath, leasePath, host, health, smtp, sub, imap string) []string {
	t.Helper()
	return []string{
		"MAILEZINE_STORAGE_BACKEND=pebble",
		"MAILEZINE_ROCKS_PATH=" + rocksPath,
		"MAILEZINE_DIRECTORY_MODE=dev",
		"MAILEZINE_DIRECTORY_FILE=" + filepath.Join(dir, "directory.json"),
		"MAILEZINE_AUTH_MODE=dev",
		"MAILEZINE_AUTH_DEV_FILE=" + filepath.Join(dir, "passwords.json"),
		"MAILEZINE_HEALTH_ADDR=" + health,
		"MAILEZINE_SMTP_ADDR=" + smtp,
		"MAILEZINE_SUBMISSION_ADDR=" + sub,
		"MAILEZINE_IMAP_ADDR=" + imap,
		"MAILEZINE_OUTBOUND_ENABLED=false",
		"MAILEZINE_HA_ENABLED=true",
		"MAILEZINE_HA_LEASE_PATH=" + leasePath,
		"MAILEZINE_HA_TTL_SECONDS=2",
		"MAILEZINE_HOSTNAME=" + host,
	}
}

func haReady(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// haRole fetches the /health role ("leader"/"standby"); "" when unreachable.
func haRole(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.Role
}

func haSubmit(t *testing.T, addr, subject string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := gosmtp.NewClient(conn)
	defer c.Close()
	if err := c.Auth(plainAuth{u: "alice@example.com", p: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("alice@example.com", nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "From: alice@example.com\r\nTo: alice@example.com\r\nSubject: %s\r\n\r\nha body\r\n", subject)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func haImapCheck(t *testing.T, addr, subject string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	line := func() string {
		l, _ := br.ReadString('\n')
		return strings.TrimRight(l, "\r\n")
	}
	line() // greeting
	write := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }
	write(`a1 LOGIN alice@example.com s3cret`)
	for {
		if strings.HasPrefix(line(), "a1 OK") {
			break
		}
	}
	write(`a2 SELECT INBOX`)
	for {
		l := line()
		if strings.HasPrefix(l, "a2 OK") {
			break
		}
	}
	write(fmt.Sprintf(`a3 UID SEARCH SUBJECT "%s"`, subject))
	found := false
	for {
		l := line()
		if strings.HasPrefix(l, "* SEARCH") && l != "* SEARCH" {
			found = true
		}
		if strings.HasPrefix(l, "a3 OK") {
			break
		}
	}
	write("a4 LOGOUT")
	if !found {
		t.Fatalf("subject %q not found after failover", subject)
	}
}

func TestHAFailover(t *testing.T) {
	bin := buildEngine(t)
	dir := t.TempDir()
	writeDevFiles(t, filepath.Join(dir, "directory.json"), filepath.Join(dir, "passwords.json"))
	rocks := filepath.Join(dir, "rocks")
	lease := filepath.Join(dir, "lease.json")

	aHealth := "127.0.0.1:" + freePort(t)
	aSMTP := "127.0.0.1:" + freePort(t)
	aSub := "127.0.0.1:" + freePort(t)
	bHealth := "127.0.0.1:" + freePort(t)
	bIMAP := "127.0.0.1:" + freePort(t)

	start := func(env []string) *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	// A becomes leader.
	a := start(haEnv(t, dir, rocks, lease, "ha-a", aHealth, aSMTP, aSub, ""))
	t.Cleanup(func() {
		if a.Process != nil {
			_ = a.Process.Kill()
			_, _ = a.Process.Wait()
		}
	})
	// Wait for actual leadership (health answers from process start; only
	// the leader role guarantees the mail listeners are bound).
	deadline := time.Now().Add(15 * time.Second)
	for haRole(t, aHealth) != "leader" {
		if time.Now().After(deadline) {
			t.Fatal("engine A did not become leader")
		}
		time.Sleep(100 * time.Millisecond)
	}
	haSubmit(t, aSub, "ha-message")

	// B starts and stays on standby: no listeners.
	b := start(haEnv(t, dir, rocks, lease, "ha-b", bHealth, "", "", bIMAP))
	t.Cleanup(func() {
		if b.Process != nil {
			_ = b.Process.Kill()
			_, _ = b.Process.Wait()
		}
	})
	// B starts and stays on standby: its health endpoint answers (an
	// orchestrator must not mistake a follower for a dead pod) but reports
	// the standby role and never binds mail listeners.
	deadline = time.Now().Add(15 * time.Second)
	for haRole(t, bHealth) != "standby" {
		if time.Now().After(deadline) {
			t.Fatal("engine B did not report role=standby while A leads")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if haReady(bIMAP) {
		t.Fatal("engine B must not bind mail listeners while A leads")
	}

	// A dies; B takes over within TTL + retry.
	_ = a.Process.Kill()
	_, _ = a.Process.Wait()
	// The health endpoint is up the whole time (standbys answer probes);
	// takeover completes only when the role flips to leader, which happens
	// after the mail listeners are already bound.
	deadline = time.Now().Add(20 * time.Second)
	for haRole(t, bHealth) != "leader" {
		if time.Now().After(deadline) {
			t.Fatal("engine B did not take over after A failed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	haImapCheck(t, bIMAP, "ha-message")
}

// plainAuth is a minimal SASL PLAIN for go-smtp.
type plainAuth struct{ u, p string }

func (a plainAuth) Start() (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.u + "\x00" + a.p), nil
}

func (a plainAuth) Next(_ []byte) ([]byte, error) { return nil, nil }
