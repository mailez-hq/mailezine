// storage-smoke exercises one running mailezine engine over its protocol
// surface (dev stubs, no backend): authenticated SMTP submission, IMAP read,
// POP3 read. It is the driver for storage-backend e2e runs (pebble/rocksdb):
//   - --send  submits one message and verifies it via IMAP and POP3;
//   - --check verifies a previously sent subject is still readable via IMAP
//     and POP3 (used after an engine restart to prove persistence).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	user     = "alice@example.com"
	password = "s3cret"
)

func main() {
	mode := flag.String("mode", "send", "send (submit+verify) or check (verify only)")
	smtpAddr := flag.String("smtp", "127.0.0.1:11587", "submission address")
	imapAddr := flag.String("imap", "127.0.0.1:11143", "IMAP address")
	pop3Addr := flag.String("pop3", "127.0.0.1:10110", "POP3 address")
	subject := flag.String("subject", "", "subject to verify (send mode auto-generates)")
	s3Endpoint := flag.String("s3-endpoint", "", "MinIO endpoint to verify blob objects")
	s3Access := flag.String("s3-access", "", "MinIO access key")
	s3Secret := flag.String("s3-secret", "", "MinIO secret key")
	s3Bucket := flag.String("s3-bucket", "", "MinIO bucket")
	flag.Parse()

	if *subject == "" {
		*subject = fmt.Sprintf("storage-smoke %d", time.Now().Unix())
	}

	if *mode == "send" {
		if err := submit(*smtpAddr, *subject); err != nil {
			fatal("smtp submit", err)
		}
		fmt.Printf("[PASS] smtp submit - %s\n", *subject)
	}
	if err := imapCheck(*imapAddr, *subject); err != nil {
		fatal("imap read", err)
	}
	fmt.Println("[PASS] imap read")
	if err := pop3Check(*pop3Addr, *subject); err != nil {
		fatal("pop3 read", err)
	}
	fmt.Println("[PASS] pop3 read")
	if *s3Endpoint != "" {
		if err := s3Check(*s3Endpoint, *s3Access, *s3Secret, *s3Bucket); err != nil {
			fatal("s3 blob check", err)
		}
		fmt.Println("[PASS] s3 blob present")
	}
}

func fatal(what string, err error) {
	fmt.Printf("[FAIL] %s - %v\n", what, err)
	panic(err)
}

func submit(addr, subject string) error {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	c := gosmtp.NewClient(conn)
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", user, password)); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(user, nil); err != nil {
		return err
	}
	if err := c.Rcpt(user, nil); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\nstorage backend smoke\r\n", user, user, subject)
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

func imapCheck(addr, subject string) error {
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		return err
	}
	defer c.Logout()
	if err := c.Login(user, password).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return err
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{
		{Key: "Subject", Value: subject},
	}}, nil).Wait()
	if err != nil {
		return err
	}
	if data.All == nil || data.All.String() == "" {
		return fmt.Errorf("subject %q not found in INBOX", subject)
	}
	return nil
}

func pop3Check(addr, subject string) error {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	line := func() (string, error) {
		l, err := br.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err
	}
	if _, err := line(); err != nil {
		return err
	}
	write := func(s string) error {
		_, err := fmt.Fprintf(conn, "%s\r\n", s)
		return err
	}
	if err := write("USER " + user); err != nil {
		return err
	}
	if _, err := line(); err != nil {
		return err
	}
	if err := write("PASS " + password); err != nil {
		return err
	}
	if _, err := line(); err != nil {
		return err
	}
	if err := write("STAT"); err != nil {
		return err
	}
	stat, err := line()
	if err != nil {
		return err
	}
	var count int
	if _, err := fmt.Sscanf(strings.TrimPrefix(stat, "+OK "), "%d", &count); err != nil {
		return fmt.Errorf("stat parse %q: %w", stat, err)
	}
	for i := 1; i <= count; i++ {
		if err := write(fmt.Sprintf("RETR %d", i)); err != nil {
			return err
		}
		if _, err := line(); err != nil {
			return err
		}
		var msg strings.Builder
		for {
			l, err := line()
			if err != nil {
				return err
			}
			if l == "." {
				break
			}
			msg.WriteString(l)
			msg.WriteString("\n")
		}
		if strings.Contains(msg.String(), "Subject: "+subject) {
			return nil
		}
	}
	return fmt.Errorf("subject %q not found via POP3", subject)
}

// s3Check lists the bucket and verifies at least one email blob object is
// present — proving message bodies actually landed in the object store.
func s3Check(endpoint, access, secret, bucket string) error {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: false,
		Region: "us-east-1",
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	objects := mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{})
	first, ok := <-objects
	if !ok {
		return fmt.Errorf("bucket %q is empty", bucket)
	}
	if first.Err != nil {
		return first.Err
	}
	// Blob keys are content-addressed in production (mailstore/kv.go keys
	// bodies as "sha256-<hex>"); "email-" only appears in test fixtures.
	if !strings.HasPrefix(first.Key, "email-") && !strings.HasPrefix(first.Key, "sha256-") {
		return fmt.Errorf("expected email blob, got %q", first.Key)
	}
	return nil
}
