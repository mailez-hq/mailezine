// conformance drives real client libraries (go-imap/v2 imapclient and
// go-smtp) against a live mailezine deployment and checks the IMAP/SMTP
// behaviour third-party mail clients depend on. This is the automated half
// of the client-conformance plan; the manual GUI matrix lives in
// docs/conformance/README.md.
//
// Unlike the hermetic unit tests, this tool speaks the real wire protocols
// to a running server (dev: docker compose ports, e.g.
// -imap 127.0.0.1:11143 -smtp 127.0.0.1:11587).
//
// Usage:
//
//	go run ./cmd/conformance -imap 127.0.0.1:11143 -smtp 127.0.0.1:11587 \
//	    -user conformance@example.com -pass 'secret'
//
// Provision the account with cmd/e2e-mailez or the admin console first.
// Exit status is 0 only when every check passes.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

const tmpMailbox = "ConformanceTMP"
const tmpMailbox2 = "ConformanceTMP2"

type namedCheck struct {
	name string
	fn   func() error
}

func main() {
	imapAddr := flag.String("imap", "127.0.0.1:11143", "mailezine IMAP address")
	smtpAddr := flag.String("smtp", "127.0.0.1:11587", "mailezine submission address")
	user := flag.String("user", "", "account e-mail (required)")
	pass := flag.String("pass", os.Getenv("MAILEZINE_CONFORMANCE_PASS"), "account password")
	skipIMAP := flag.Bool("skip-imap", false, "skip IMAP checks")
	skipSMTP := flag.Bool("skip-smtp", false, "skip SMTP checks")
	flag.Parse()
	if *user == "" || *pass == "" {
		fmt.Fprintln(os.Stderr, "conformance: -user and -pass are required")
		os.Exit(2)
	}

	var checks []namedCheck
	if !*skipIMAP {
		checks = append(checks, imapChecks(*imapAddr, *user, *pass)...)
	}
	if !*skipSMTP {
		checks = append(checks, smtpChecks(*smtpAddr, *imapAddr, *user, *pass, *skipIMAP)...)
	}

	failed := 0
	for _, c := range checks {
		err := c.fn()
		if err == nil {
			fmt.Printf("[PASS] %s\n", c.name)
			continue
		}
		failed++
		fmt.Printf("[FAIL] %s — %v\n", c.name, err)
	}
	fmt.Printf("\n%d checks, %d failed\n", len(checks), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// dialLogin opens a fresh IMAP connection and logs in.
func dialLogin(addr, user, pass string) (*imapclient.Client, error) {
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		return nil, err
	}
	if err := c.Login(user, pass).Wait(); err != nil {
		c.Logout()
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

func imapChecks(addr, user, pass string) []namedCheck {
	var savedUID imap.UID
	var savedModSeq uint64
	var c *imapclient.Client
	// The shared client is replaced by each connection-local check; the
	// final cleanup check logs it out.
	logout := func() error {
		if c != nil {
			c.Logout()
			c = nil
		}
		return nil
	}

	return []namedCheck{
		{"imap/preauth-capability", func() error {
			cc, err := dialLogin(addr, user, pass)
			if err != nil {
				return err
			}
			defer cc.Logout()
			caps, err := cc.Capability().Wait()
			if err != nil {
				return err
			}
			for _, need := range []imap.Cap{imap.CapIMAP4rev1, imap.Cap("AUTH=PLAIN")} {
				if !caps.Has(need) {
					return fmt.Errorf("missing capability %s", need)
				}
			}
			return nil
		}},
		{"imap/login", func() error {
			cc, err := dialLogin(addr, user, pass)
			if err != nil {
				return err
			}
			c = cc
			return nil
		}},
		{"imap/list-inbox", func() error {
			data, err := c.List("", "*", nil).Collect()
			if err != nil {
				return err
			}
			for _, d := range data {
				if d.Mailbox == "INBOX" {
					return nil
				}
			}
			return fmt.Errorf("INBOX not listed")
		}},
		{"imap/status-inbox", func() error {
			data, err := c.Status("INBOX", &imap.StatusOptions{
				NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true,
			}).Wait()
			if err != nil {
				return err
			}
			if data.UIDValidity == 0 || data.UIDNext == 0 {
				return fmt.Errorf("uidvalidity=%d uidnext=%d", data.UIDValidity, data.UIDNext)
			}
			return nil
		}},
		{"imap/select-highestmodseq", func() error {
			data, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				return err
			}
			if data.HighestModSeq == 0 {
				return fmt.Errorf("no HIGHESTMODSEQ (CONDSTORE broken)")
			}
			return nil
		}},
		{"imap/enable-condstore-qresync", func() error {
			data, err := c.Enable(imap.CapCondStore, imap.CapQResync).Wait()
			if err != nil {
				return err
			}
			for _, want := range []imap.Cap{imap.CapCondStore, imap.CapQResync} {
				if !data.Caps.Has(want) {
					return fmt.Errorf("ENABLE did not confirm %s", want)
				}
			}
			return nil
		}},
		{"imap/mailbox-crud", func() error {
			if err := c.Create(tmpMailbox, nil).Wait(); err != nil {
				return fmt.Errorf("create: %w", err)
			}
			if err := c.Create(tmpMailbox2, nil).Wait(); err != nil {
				return fmt.Errorf("create2: %w", err)
			}
			if err := c.Rename(tmpMailbox, tmpMailbox+"R", nil).Wait(); err != nil {
				return fmt.Errorf("rename: %w", err)
			}
			if err := c.Rename(tmpMailbox+"R", tmpMailbox, nil).Wait(); err != nil {
				return fmt.Errorf("rename back: %w", err)
			}
			if err := c.Subscribe(tmpMailbox).Wait(); err != nil {
				return fmt.Errorf("subscribe: %w", err)
			}
			return nil
		}},
		{"imap/append-uidplus", func() error {
			msg := "From: " + user + "\r\nSubject: conformance-append\r\n\r\nbody\r\n"
			cmd := c.Append(tmpMailbox, int64(len(msg)), &imap.AppendOptions{
				Flags: []imap.Flag{imap.FlagSeen},
				Time:  time.Now(),
			})
			if _, err := cmd.Write([]byte(msg)); err != nil {
				return fmt.Errorf("append write: %w", err)
			}
			if err := cmd.Close(); err != nil {
				return fmt.Errorf("append close: %w", err)
			}
			data, err := cmd.Wait()
			if err != nil {
				return fmt.Errorf("append: %w", err)
			}
			savedUID = data.UID
			if savedUID == 0 {
				return fmt.Errorf("no UID returned (UIDPLUS broken)")
			}
			return nil
		}},
		{"imap/fetch-modseq-body", func() error {
			bufs, err := c.Fetch(imap.UIDSetNum(savedUID), &imap.FetchOptions{
				UID:         true,
				Flags:       true,
				ModSeq:      true,
				BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierHeader}},
			}).Collect()
			if err != nil {
				return err
			}
			if len(bufs) != 1 {
				return fmt.Errorf("fetched %d messages, want 1", len(bufs))
			}
			if bufs[0].ModSeq == 0 {
				return fmt.Errorf("no MODSEQ in FETCH (CONDSTORE broken)")
			}
			savedModSeq = bufs[0].ModSeq
			return nil
		}},
		{"imap/store-unchangedsince", func() error {
			bufs, err := c.Store(imap.UIDSetNum(savedUID), &imap.StoreFlags{
				Op:    imap.StoreFlagsAdd,
				Flags: []imap.Flag{imap.FlagFlagged},
			}, &imap.StoreOptions{UnchangedSince: savedModSeq}).Collect()
			if err != nil {
				return err
			}
			if len(bufs) != 1 || bufs[0].ModSeq <= savedModSeq {
				return fmt.Errorf("store did not advance MODSEQ (n=%d)", len(bufs))
			}
			savedModSeq = bufs[0].ModSeq
			return nil
		}},
		{"imap/search", func() error {
			if _, err := c.Select(tmpMailbox, nil).Wait(); err != nil {
				return err
			}
			data, err := c.UIDSearch(&imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "conformance-append"}},
			}, nil).Wait()
			if err != nil {
				return err
			}
			if data.All == nil || data.All.String() == "" {
				return fmt.Errorf("SEARCH found nothing")
			}
			return nil
		}},
		{"imap/copy-move", func() error {
			if _, err := c.Select(tmpMailbox, nil).Wait(); err != nil {
				return err
			}
			copyData, err := c.Copy(imap.UIDSetNum(savedUID), tmpMailbox2).Wait()
			if err != nil {
				return fmt.Errorf("copy: %w", err)
			}
			if copyData.UIDValidity == 0 {
				return fmt.Errorf("copy: no COPYUID response")
			}
			moveData, err := c.Move(imap.UIDSetNum(savedUID), tmpMailbox2).Wait()
			if err != nil {
				return fmt.Errorf("move: %w", err)
			}
			if moveData.DestUIDs == nil {
				return fmt.Errorf("move: no COPYUID response")
			}
			return nil
		}},
		{"imap/idle-push", func() error {
			// Connection A idles; connection B appends; A must observe the
			// EXISTS bump while idling — what every push-capable client
			// (mainstream desktop clients) relies on.
			exists := make(chan uint32, 4)
			a, err := imapclient.DialInsecure(addr, &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Mailbox: func(d *imapclient.UnilateralDataMailbox) {
						if d.NumMessages != nil {
							exists <- *d.NumMessages
						}
					},
				},
			})
			if err != nil {
				return err
			}
			defer a.Logout()
			if err := a.Login(user, pass).Wait(); err != nil {
				return err
			}
			if _, err := a.Select(tmpMailbox2, nil).Wait(); err != nil {
				return err
			}
			ic, err := a.Idle()
			if err != nil {
				return fmt.Errorf("idle start: %w", err)
			}
			// Connection B: append one message.
			b, err := dialLogin(addr, user, pass)
			if err != nil {
				ic.Close()
				ic.Wait()
				return err
			}
			defer b.Logout()
			msg := "From: " + user + "\r\nSubject: conformance-idle\r\n\r\nbody\r\n"
			cmd := b.Append(tmpMailbox2, int64(len(msg)), nil)
			cmd.Write([]byte(msg))
			cmd.Close()
			if _, err := cmd.Wait(); err != nil {
				ic.Close()
				ic.Wait()
				return fmt.Errorf("idle append: %w", err)
			}
			select {
			case n := <-exists:
				if n < 1 {
					return fmt.Errorf("idle EXISTS = %d", n)
				}
			case <-time.After(20 * time.Second):
				ic.Close()
				ic.Wait()
				return fmt.Errorf("no EXISTS within 20s of append during IDLE")
			}
			ic.Close()
			return ic.Wait()
		}},
		{"imap/expunge-unselect", func() error {
			if _, err := c.Select(tmpMailbox2, nil).Wait(); err != nil {
				return err
			}
			if _, err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{
				Op:    imap.StoreFlagsAdd,
				Flags: []imap.Flag{imap.FlagDeleted},
			}, nil).Collect(); err != nil {
				return fmt.Errorf("store deleted: %w", err)
			}
			if _, err := c.Expunge().Collect(); err != nil {
				return fmt.Errorf("expunge: %w", err)
			}
			if err := c.Unselect().Wait(); err != nil {
				return fmt.Errorf("unselect: %w", err)
			}
			return nil
		}},
		{"imap/cleanup-and-logout", func() error {
			for _, mb := range []string{tmpMailbox, tmpMailbox2} {
				_ = c.Unsubscribe(mb).Wait() // best-effort
				if err := c.Delete(mb).Wait(); err != nil {
					return fmt.Errorf("delete %s: %w", mb, err)
				}
			}
			return logout()
		}},
	}
}

func smtpChecks(smtpAddr, imapAddr, user, pass string, skipIMAP bool) []namedCheck {
	subject := fmt.Sprintf("conformance-smtp-%d", time.Now().UnixNano())

	// submit performs an authenticated submission from user to to.
	submit := func(from, to, body string) error {
		conn, err := net.DialTimeout("tcp", smtpAddr, 15*time.Second)
		if err != nil {
			return err
		}
		client := gosmtp.NewClient(conn)
		defer client.Close()
		if err := client.Auth(sasl.NewPlainClient("", user, pass)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		if err := client.Mail(from, nil); err != nil {
			return fmt.Errorf("mail from: %w", err)
		}
		if err := client.Rcpt(to, nil); err != nil {
			return fmt.Errorf("rcpt to: %w", err)
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, strings.NewReader(body)); err != nil {
			return err
		}
		return w.Close()
	}
	// newAuthedClient opens EHLO+AUTH for protocol-level checks.
	newAuthedClient := func(password string) (*gosmtp.Client, error) {
		conn, err := net.DialTimeout("tcp", smtpAddr, 15*time.Second)
		if err != nil {
			return nil, err
		}
		client := gosmtp.NewClient(conn)
		if password != "" {
			if err := client.Auth(sasl.NewPlainClient("", user, password)); err != nil {
				client.Close()
				return nil, err
			}
		}
		return client, nil
	}

	return []namedCheck{
		{"smtp/ehlo-extensions", func() error {
			client, err := newAuthedClient("")
			if err != nil {
				return err
			}
			defer client.Close()
			for _, ext := range []string{"PIPELINING", "SIZE", "8BITMIME", "ENHANCEDSTATUSCODES"} {
				if ok, _ := client.Extension(ext); !ok {
					return fmt.Errorf("%s not advertised", ext)
				}
			}
			return nil
		}},
		{"smtp/auth-bad-credentials", func() error {
			_, err := newAuthedClient("definitely-wrong")
			if err == nil {
				return fmt.Errorf("server accepted a wrong password")
			}
			return nil
		}},
		{"smtp/submit-8bit", func() error {
			body := "From: " + user + "\r\n" +
				"To: " + user + "\r\n" +
				"Subject: " + subject + "\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" +
				"一致性与回归 8-bit body ✓\r\n"
			return submit(user, user, body)
		}},
		{"smtp/recipient-unknown-rejected", func() error {
			client, err := newAuthedClient(pass)
			if err != nil {
				return err
			}
			defer client.Close()
			if err := client.Mail(user, nil); err != nil {
				return err
			}
			domain := strings.SplitN(user, "@", 2)[1]
			err = client.Rcpt("no-such-user-9f83k1@"+domain, nil)
			if err == nil {
				return fmt.Errorf("server accepted mail for a nonexistent local part")
			}
			return nil
		}},
		{"smtp/delivered-to-inbox", func() error {
			if skipIMAP {
				return nil // skipped: needs the IMAP side
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				c, err := dialLogin(imapAddr, user, pass)
				if err != nil {
					return err
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					c.Logout()
					return err
				}
				data, err := c.UIDSearch(&imap.SearchCriteria{
					Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: subject}},
				}, nil).Wait()
				found := err == nil && data.All != nil && data.All.String() != ""
				c.Logout()
				if found {
					return nil
				}
				if !time.Now().Before(deadline) {
					return fmt.Errorf("message %q not delivered within 30s", subject)
				}
				time.Sleep(2 * time.Second)
			}
		}},
	}
}
