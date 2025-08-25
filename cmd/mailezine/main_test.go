// End-to-end smoke tests of the composition root: boot the engine with real
// backends, drive SMTP over the wire and inspect the storage after
// shutdown. DNS-dependent stages (SPF/DKIM/DMARC) run against the reserved
// .test/.invalid zones and fail fast or fail open.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func TestEndToEndLocalDelivery(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, false, "")

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)

	waitHTTP(t, "http://"+healthAddr+"/health")

	client, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: e2e\r\n\r\nhello from e2e\r\n"
	if err := smtpSend(client, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}

	ms := openMailstore(t, rocksPath)
	email, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	if email.From != "sender@remote.test" || email.Mailbox != "INBOX" {
		t.Fatalf("email: %+v", email)
	}
	var buf bytes.Buffer
	if err := ms.GetBlob(context.Background(), email.BlobID, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hello from e2e") {
		t.Fatalf("stored body: %q", buf.String())
	}
}

func TestEndToEndRelayQueue(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	outboundPort := freePort(t) // nothing listens here: delivery is refused, message stays deferred
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, true, outboundPort)
	mgmtAddr := "127.0.0.1:" + freePort(t)
	t.Setenv("MAILEZINE_MANAGEMENT_ADDR", mgmtAddr)
	t.Setenv("MAILEZINE_MANAGEMENT_SECRET", "sekret")
	msieveAddr := "127.0.0.1:" + freePort(t)
	t.Setenv("MAILEZINE_MANAGESIEVE_ADDR", msieveAddr)

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	// Authenticated submission of an external recipient: must be accepted
	// and spooled, not rejected as relay denied.
	client, err := gosmtp.Dial(subAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	body := "From: alice@example.com\r\nTo: relay@localhost\r\nSubject: out\r\n\r\nrelay me\r\n"
	if err := smtpSend(client, "alice@example.com", []string{"relay@localhost"}, body); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	// A remote sender relayed over the inbound port must be SRS-rewritten
	// at the envelope (bounces route back through us).
	inbound, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(inbound, "sender@remote.test", []string{"relay@localhost"},
		"From: sender@remote.test\r\nSubject: forwarded\r\n\r\nforward me\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = inbound.Close()

	// Sieve redirect: write an active script via ManageSieve, then deliver a
	// message that must be forwarded to the outbound queue.
	msConn, err := net.Dial("tcp", msieveAddr)
	if err != nil {
		t.Fatal(err)
	}
	msr := bufio.NewReader(msConn)
	msWrite := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintf(msConn, "%s\r\n", s); err != nil {
			t.Fatal(err)
		}
	}
	msStatus := func() string {
		t.Helper()
		for {
			line, err := msr.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "OK" || strings.HasPrefix(line, "OK ") {
				return "OK"
			}
			if line == "NO" || strings.HasPrefix(line, "NO ") {
				return "NO"
			}
		}
	}
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve greeting: %s", got)
	}
	msWrite(`AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`)
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve auth: %s", got)
	}
	redirectScript := `redirect "forward@remote.test";`
	msWrite(fmt.Sprintf(`PUTSCRIPT "redirect" {%d}`, len(redirectScript)))
	msWrite(redirectScript)
	if got := msStatus(); got != "OK" {
		t.Fatalf("putscript: %s", got)
	}
	msWrite(`SETACTIVE "redirect"`)
	if got := msStatus(); got != "OK" {
		t.Fatalf("setactive: %s", got)
	}
	_ = msConn.Close()

	redirectInbound, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(redirectInbound, "sender@remote.test", []string{"alice@example.com"},
		"From: sender@remote.test\r\nSubject: please forward\r\n\r\nforward me\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = redirectInbound.Close()

	// Give the queue a moment to spool and maybe attempt delivery, then stop.
	time.Sleep(200 * time.Millisecond)

	// Management API sees the queue over HTTP.
	req, err := http.NewRequest(http.MethodGet, "http://"+mgmtAddr+"/v1/queue", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sekret")
	mgmtResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var queueView struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(mgmtResp.Body).Decode(&queueView); err != nil {
		t.Fatal(err)
	}
	mgmtResp.Body.Close()
	if queueView.Count < 2 {
		t.Fatalf("management queue count = %d, want >= 2", queueView.Count)
	}

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}

	kv, err := store.OpenPebble(rocksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	meta, err := kv.Get(store.QueueKey("meta", 1))
	if err != nil {
		dumpKeys(t, kv)
		t.Fatalf("queue metadata missing: %v", err)
	}
	var msg struct {
		From       string
		Recipients []struct {
			Address string
		}
	}
	if err := json.Unmarshal(meta, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.From != "alice@example.com" || len(msg.Recipients) != 1 || msg.Recipients[0].Address != "relay@localhost" {
		t.Fatalf("queue message: %+v", msg)
	}
	srsMeta, err := kv.Get(store.QueueKey("meta", 2))
	if err != nil {
		t.Fatalf("SRS queue metadata missing: %v", err)
	}
	var srsMsg struct {
		From string
	}
	if err := json.Unmarshal(srsMeta, &srsMsg); err != nil {
		t.Fatal(err)
	}
	if srsMsg.From != "SRS0=dev=sender=remote.test@example.com" {
		t.Fatalf("SRS envelope not rewritten: %q", srsMsg.From)
	}
	redirectMeta, err := kv.Get(store.QueueKey("meta", 3))
	if err != nil {
		t.Fatalf("redirect queue metadata missing: %v", err)
	}
	var redirectMsg struct {
		From       string
		Recipients []struct {
			Address string
		}
	}
	if err := json.Unmarshal(redirectMeta, &redirectMsg); err != nil {
		t.Fatal(err)
	}
	if redirectMsg.From != "SRS0=dev=sender=remote.test@example.com" ||
		len(redirectMsg.Recipients) != 1 || redirectMsg.Recipients[0].Address != "forward@remote.test" {
		t.Fatalf("redirect queue message: %+v", redirectMsg)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}

	// The spooled blob survives shutdown; the DKIM vault is unreachable so
	// the body is delivered unsigned (opportunistic signing).
	blob, err := store.NewFSBlob(rocksPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := blob.Get(context.Background(), "q-0000000000000001", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "relay me") {
		t.Fatalf("spooled body: %q", buf.String())
	}
}

// TestEndToEndIMAP wires the whole read path: SMTP delivery → IMAP login →
// select → fetch, through the real listeners of the running engine.
func TestEndToEndIMAP(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr, imapAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, false, "")
	t.Setenv("MAILEZINE_IMAP_ADDR", imapAddr)
	msieveAddr := "127.0.0.1:" + freePort(t)
	t.Setenv("MAILEZINE_MANAGESIEVE_ADDR", msieveAddr)

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	// Deliver via SMTP first.
	smtpClient, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: imap e2e\r\n\r\nread me via imap\r\n"
	if err := smtpSend(smtpClient, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	_ = smtpClient.Close()

	// Read via IMAP.
	imapClient, err := imapclient.DialInsecure(imapAddr, nil)
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
		!strings.Contains(string(fetched[0].BodySection[0].Bytes), "read me via imap") {
		t.Fatalf("imap fetch: %+v", fetched)
	}
	_ = imapClient.Logout().Wait()

	// ManageSieve: read-only flow against the real listener.
	msConn, err := net.Dial("tcp", msieveAddr)
	if err != nil {
		t.Fatal(err)
	}
	msr := bufio.NewReader(msConn)
	msWrite := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintf(msConn, "%s\r\n", s); err != nil {
			t.Fatal(err)
		}
	}
	msStatus := func() string {
		t.Helper()
		for {
			line, err := msr.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "OK" || strings.HasPrefix(line, "OK ") {
				return "OK"
			}
			if line == "NO" || strings.HasPrefix(line, "NO ") {
				return "NO"
			}
		}
	}
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve greeting: %s", got)
	}
	msWrite(`AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`)
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve auth: %s", got)
	}
	msWrite("LISTSCRIPTS")
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve list: %s", got)
	}
	// Write flow: store and activate a local filter, then deliver a message
	// that must land in Junk (local script wins over the directory default).
	filter := "require \"fileinto\";\nif header :contains \"Subject\" \"news\" { fileinto \"Junk\"; }"
	msWrite(fmt.Sprintf(`PUTSCRIPT "rules" {%d}`, len(filter)))
	msWrite(filter)
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve putscript: %s", got)
	}
	msWrite(`SETACTIVE "rules"`)
	if got := msStatus(); got != "OK" {
		t.Fatalf("managesieve setactive: %s", got)
	}
	_ = msConn.Close()

	// A second message with Subject "news" is filtered into Junk.
	newsBody := "From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: news update\r\n\r\nnews via sieve\r\n"
	smtpClient2, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(smtpClient2, "sender@remote.test", []string{"alice@example.com"}, newsBody); err != nil {
		t.Fatal(err)
	}
	_ = smtpClient2.Close()

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}
	ms := openMailstore(t, rocksPath)
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "Junk", 1); err != nil {
		t.Fatalf("filtered message missing from Junk: %v", err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("first message missing from INBOX: %v", err)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("news message leaked into INBOX: %v", err)
	}
}

// TestEndToEndPOP3 wires the read/delete path: SMTP delivery → POP3
// USER/PASS → STAT → RETR → DELE → QUIT, then verifies the expunge landed
// in storage.
func TestEndToEndPOP3(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr, pop3Addr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, false, "")
	t.Setenv("MAILEZINE_POP3_ADDR", pop3Addr)
	t.Setenv("MAILEZINE_POP3_ENABLED", "true")

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	smtpClient, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: sender@remote.test\r\nSubject: pop me\r\n\r\npop3 body\r\n"
	if err := smtpSend(smtpClient, "sender@remote.test", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	_ = smtpClient.Close()

	conn, err := net.Dial("tcp", pop3Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	cmd := func(line string) string {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\r\n", line); err != nil {
			t.Fatal(err)
		}
		got, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(got, "\r\n")
	}
	greeting, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(greeting, "+OK") {
		t.Fatalf("POP3 greeting: %q", greeting)
	}
	if got := cmd("USER alice@example.com"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("USER: %q", got)
	}
	if got := cmd("PASS s3cret"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("PASS: %q", got)
	}
	if got := cmd("STAT"); !strings.HasPrefix(got, "+OK 1") {
		t.Fatalf("STAT: %q", got)
	}
	if got := cmd("RETR 1"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("RETR: %q", got)
	}
	var retr []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "." {
			break
		}
		retr = append(retr, line)
	}
	if !strings.Contains(strings.Join(retr, "\n"), "pop3 body") {
		t.Fatalf("POP3 body mismatch: %v", retr)
	}
	if got := cmd("DELE 1"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("DELE: %q", got)
	}
	if got := cmd("QUIT"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("QUIT: %q", got)
	}

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}

	ms := openMailstore(t, rocksPath)
	left, err := ms.ListMessages(context.Background(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("expunged message still present: %+v", left)
	}
}

// TestEndToEndTLS verifies the direct-deployment STARTTLS wiring end to end:
// SMTP requires TLS before AUTH, IMAP authenticates over STARTTLS, and the
// message round-trips.
func TestEndToEndTLS(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)
	certFile, keyFile := writeSelfSigned(t, dir)

	healthAddr, smtpAddr, subAddr, imapAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, false, "")
	t.Setenv("MAILEZINE_IMAP_ADDR", imapAddr)
	t.Setenv("MAILEZINE_TLS_CERT_FILE", certFile)
	t.Setenv("MAILEZINE_TLS_KEY_FILE", keyFile)

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	// Plaintext AUTH must be refused.
	plain, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err == nil {
		t.Fatal("plaintext AUTH allowed with TLS configured")
	}
	_ = plain.Close()

	// STARTTLS then authenticate and deliver.
	secure, err := gosmtp.DialStartTLS(smtpAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := secure.Auth(sasl.NewPlainClient("", "alice@example.com", "s3cret")); err != nil {
		t.Fatal(err)
	}
	body := "From: alice@example.com\r\nTo: alice@example.com\r\nSubject: over tls\r\n\r\nsecure body\r\n"
	if err := smtpSend(secure, "alice@example.com", []string{"alice@example.com"}, body); err != nil {
		t.Fatal(err)
	}
	_ = secure.Close()

	// IMAP over STARTTLS reads it back.
	imapClient, err := imapclient.DialStartTLS(imapAddr, &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
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
	if len(fetched) != 1 || !strings.Contains(string(fetched[0].BodySection[0].Bytes), "secure body") {
		t.Fatalf("imap fetch over TLS: %+v", fetched)
	}
	_ = imapClient.Logout().Wait()

	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}
}

// TestEndToEndBounceDSN verifies terminal queue failure generates an RFC
// 3464 DSN delivered to the local sender's INBOX (null envelope sender, so
// it can never bounce again).
func TestEndToEndBounceDSN(t *testing.T) {
	dir := t.TempDir()
	dirFile := filepath.Join(dir, "directory.json")
	authFile := filepath.Join(dir, "passwords.json")
	rocksPath := filepath.Join(dir, "rocks")
	writeDevFiles(t, dirFile, authFile)

	healthAddr, smtpAddr, subAddr := "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t), "127.0.0.1:"+freePort(t)
	outboundPort := freePort(t) // nothing listens: delivery fails permanently after 1 attempt
	setTestEnv(t, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr, true, outboundPort)
	t.Setenv("MAILEZINE_QUEUE_MAX_ATTEMPTS", "1")
	t.Setenv("MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS", "1")

	ctx, cancel := context.WithCancel(context.Background())
	exit := startEngine(t, ctx)
	waitHTTP(t, "http://"+healthAddr+"/health")

	client, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := smtpSend(client, "alice@example.com", []string{"missing@nowhere.invalid"},
		"From: alice@example.com\r\nTo: missing@nowhere.invalid\r\nSubject: will bounce\r\n\r\nbody\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	// Let the queue attempt (once), bounce, and deliver the DSN locally.
	time.Sleep(3 * time.Second)
	cancel()
	if code := waitExit(t, exit); code != 0 {
		t.Fatalf("engine exit code = %d, want 0", code)
	}

	// The queue message is terminal (bounced).
	kv, err := store.OpenPebble(rocksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	meta, err := kv.Get(store.QueueKey("meta", 1))
	if err != nil {
		t.Fatalf("queue meta missing: %v", err)
	}
	var qm struct {
		State string
	}
	if err := json.Unmarshal(meta, &qm); err != nil {
		t.Fatal(err)
	}
	if qm.State != "bounced" {
		t.Fatalf("queue state = %q, want bounced", qm.State)
	}
	_ = kv.Close()

	// The DSN landed in the local sender's INBOX.
	ms := openMailstore(t, rocksPath)
	msgs, err := ms.ListMessages(context.Background(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range msgs {
		rc, err := ms.OpenMessage(context.Background(), "alice@example.com", "INBOX", m.UID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(body), "Delivery Status Notification") &&
			strings.Contains(string(body), "message/delivery-status") &&
			strings.Contains(string(body), "missing@nowhere.invalid") {
			found = true
		}
	}
	if !found {
		t.Fatalf("DSN not delivered to sender INBOX")
	}
}

func writeSelfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mail.mailez.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func dumpKeys(t *testing.T, kv store.KV) {
	t.Helper()
	t.Log("kv keys:")
	_ = kv.Scan(nil, func(k, v []byte) error {
		t.Logf("  %x = %q", k, v)
		return nil
	})
}

func writeDevFiles(t *testing.T, dirFile, authFile string) {
	t.Helper()
	dir := map[string]any{
		"users": map[string]any{
			"alice@example.com": map[string]any{
				"email": "alice@example.com", "enabled": true, "quotaBytes": 1 << 20,
			},
		},
		"domains": []string{"example.com"},
		"aliases": map[string]any{},
		"relays":  map[string]any{},
		"senders": map[string]any{"alice@example.com": []string{"alice@example.com"}},
		"sieve":   map[string]any{},
	}
	writeJSON(t, dirFile, dir)
	writeJSON(t, authFile, map[string]string{"alice@example.com": "s3cret"})
}

func setTestEnv(t *testing.T, dirFile, authFile, rocksPath, healthAddr, smtpAddr, subAddr string, outbound bool, outboundPort string) {
	t.Helper()
	t.Setenv("MAILEZINE_STORAGE_BACKEND", "pebble")
	t.Setenv("MAILEZINE_ROCKS_PATH", rocksPath)
	t.Setenv("MAILEZINE_DIRECTORY_MODE", "dev")
	t.Setenv("MAILEZINE_DIRECTORY_FILE", dirFile)
	t.Setenv("MAILEZINE_AUTH_MODE", "dev")
	t.Setenv("MAILEZINE_AUTH_DEV_FILE", authFile)
	t.Setenv("MAILEZINE_HEALTH_ADDR", healthAddr)
	t.Setenv("MAILEZINE_SMTP_ADDR", smtpAddr)
	t.Setenv("MAILEZINE_SUBMISSION_ADDR", subAddr)
	t.Setenv("MAILEZINE_OUTBOUND_ENABLED", fmt.Sprintf("%v", outbound))
	if outboundPort != "" {
		t.Setenv("MAILEZINE_OUTBOUND_PORT", outboundPort)
	}
	t.Setenv("MAILEZINE_LOG_LEVEL", "error")
}

func startEngine(t *testing.T, ctx context.Context) chan int {
	t.Helper()
	exit := make(chan int, 1)
	go func() { exit <- runCtx(ctx, nil) }()
	return exit
}

func waitExit(t *testing.T, exit chan int) int {
	t.Helper()
	select {
	case code := <-exit:
		return code
	case <-time.After(15 * time.Second):
		t.Fatal("engine did not exit")
		return -1
	}
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("engine did not become ready at %s", url)
}

func openMailstore(t *testing.T, rocksPath string) *mailstore.KV {
	t.Helper()
	kv, err := store.OpenPebble(rocksPath)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.NewFSBlob(rocksPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	return mailstore.NewKV(store.New(kv, blob))
}

func smtpSend(client *gosmtp.Client, from string, to []string, body string) error {
	if err := client.Mail(from, nil); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt, nil); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return fmt.Sprintf("%d", port)
}
