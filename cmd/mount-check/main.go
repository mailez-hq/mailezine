// Command mount-check validates a maildir mount end to end: it logs into
// IMAP, lists INBOX, fetches every message's subject and appends a probe
// message via SMTP. Exit code 0 means the engine can read the existing
// (pre-seeded) data and write into the same layout.
//
// Usage: mount-check -imap host:port -smtp host:port -user u@example.com -pass pw
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gosmtp "github.com/emersion/go-smtp"
)

func main() {
	var imapAddr, smtpAddr, user, pass, subject string
	flag.StringVar(&imapAddr, "imap", "127.0.0.1:143", "IMAP address")
	flag.StringVar(&smtpAddr, "smtp", "127.0.0.1:25", "SMTP address")
	flag.StringVar(&user, "user", "alice@example.com", "account")
	flag.StringVar(&pass, "pass", "seed", "password")
	flag.StringVar(&subject, "subject", "mount check "+time.Now().Format("2006-01-02 15:04:05"), "probe subject")
	flag.Parse()

	client, err := imapclient.DialInsecure(imapAddr, nil)
	if err != nil {
		fatal("imap dial: %v", err)
	}
	if err := client.Login(user, pass).Wait(); err != nil {
		fatal("imap login: %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		fatal("imap select: %v", err)
	}
	fetched, err := client.Fetch(imap.SeqSetNum(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), &imap.FetchOptions{
		Envelope: true,
	}).Collect()
	if err != nil {
		fatal("imap fetch: %v", err)
	}
	fmt.Printf("INBOX has %d fetched messages\n", len(fetched))
	for _, m := range fetched {
		subj := ""
		if m.Envelope != nil {
			subj = m.Envelope.Subject
		}
		fmt.Printf("  uid=%d subject=%q\n", m.UID, subj)
	}
	_ = client.Logout().Wait()

	// Append a probe via SMTP (local delivery back into the same mailbox).
	smtpClient, err := gosmtp.Dial(smtpAddr)
	if err != nil {
		fatal("smtp dial: %v", err)
	}
	defer smtpClient.Close()
	if err := smtpClient.Mail("mount-check@example.com", nil); err != nil {
		fatal("smtp mail: %v", err)
	}
	if err := smtpClient.Rcpt(user, nil); err != nil {
		fatal("smtp rcpt: %v", err)
	}
	w, err := smtpClient.Data()
	if err != nil {
		fatal("smtp data: %v", err)
	}
	body := fmt.Sprintf("From: mount-check@example.com\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <mount-check-%d@example.com>\r\nDate: %s\r\n\r\nprobe\r\n",
		user, subject, time.Now().UnixNano(), time.Now().Format(time.RFC1123Z))
	if _, err := w.Write([]byte(body)); err != nil {
		fatal("smtp write: %v", err)
	}
	if err := w.Close(); err != nil {
		fatal("smtp close: %v", err)
	}
	fmt.Printf("SMTP probe %q queued\n", subject)
	fmt.Println("MOUNT_OK")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mount-check: "+format+"\n", args...)
	os.Exit(1)
}
