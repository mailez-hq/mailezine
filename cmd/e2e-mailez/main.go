// e2e-mailez drives the full mailez control-plane chain against mailezine:
// backend API (login, domain/user provisioning) -> authenticated SMTP
// submission -> IMAP delivery, verifying the directory/auth contract the
// gateway depends on. This is the mailezine-side replacement for the mailez
// repo's cmd/e2e (which lags the paginated API).
//
// Usage:
//
//	go run ./cmd/e2e-mailez -api http://127.0.0.1:18081 -smtp 127.0.0.1:11587 -imap 127.0.0.1:11143
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

func main() {
	api := flag.String("api", "http://127.0.0.1:18081", "backend API base URL")
	smtpAddr := flag.String("smtp", "127.0.0.1:11587", "mailezine submission address")
	imapAddr := flag.String("imap", "127.0.0.1:11143", "mailezine IMAP address")
	adminEmail := flag.String("admin", "admin@example.com", "global admin email")
	adminPassword := flag.String("admin-password", "MailezDemo2026!", "global admin password")
	domain := flag.String("domain", "e2e.example.com", "test domain")
	user := flag.String("user", "e2e", "test user localpart")
	flag.Parse()

	checks, passed := 0, 0
	check := func(name string, ok bool, detail string) {
		checks++
		if ok {
			passed++
		}
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		if detail != "" {
			fmt.Printf("[%s] %s - %s\n", mark, name, detail)
		} else {
			fmt.Printf("[%s] %s\n", mark, name)
		}
	}

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	userEmail := *user + "@" + *domain
	userPassword := "E2e-Passw0rd!"

	// 1. Backend health.
	if code, err := apiJSON(hc, *api, "GET", "/api/v1/health", nil, nil); err != nil || code != 200 {
		check("backend health", false, fmt.Sprintf("status %d: %v", code, err))
		return
	}
	check("backend health", true, "")

	// 2. Admin login (session cookie).
	if code, err := apiJSON(hc, *api, "POST", "/api/v1/sso/login",
		map[string]string{"email": *adminEmail, "pw": *adminPassword}, nil); err != nil || code != 200 {
		check("admin login", false, fmt.Sprintf("status %d: %v", code, err))
		return
	}
	check("admin login", true, *adminEmail)

	// 3. Provision domain (paginated, idempotent).
	var page domainPage
	if code, err := apiJSON(hc, *api, "GET", "/api/v1/domains?limit=100", nil, &page); err != nil || code != 200 {
		check("list domains", false, fmt.Sprintf("status %d: %v", code, err))
		return
	}
	found := false
	for _, d := range page.Data {
		if d.Name == *domain {
			found = true
			break
		}
	}
	if !found {
		code, err := apiJSON(hc, *api, "POST", "/api/v1/domains",
			map[string]any{"name": *domain, "max_users": -1, "max_aliases": -1}, nil)
		check("create domain", err == nil && (code == 200 || code == 201), fmt.Sprintf("status %d", code))
	} else {
		check("create domain", true, "already exists")
	}

	// 4. Provision user (paginated, idempotent).
	var users userPage
	if code, err := apiJSON(hc, *api, "GET", "/api/v1/users?limit=100&domain="+*domain, nil, &users); err != nil || code != 200 {
		check("list users", false, fmt.Sprintf("status %d: %v", code, err))
		return
	}
	userFound := false
	for _, u := range users.Data {
		if u.Email == userEmail {
			userFound = true
			break
		}
	}
	if !userFound {
		code, err := apiJSON(hc, *api, "POST", "/api/v1/users",
			map[string]any{"email": userEmail, "password": userPassword, "enabled": true}, nil)
		check("create user", err == nil && code == 201, fmt.Sprintf("status %d", code))
	} else {
		check("create user", true, "already exists")
	}

	// 5. Authenticated SMTP submission through mailezine.
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: mailez e2e\r\n\r\nhello from e2e-mailez\r\n", userEmail, userEmail)
	if err := smtpSubmit(*smtpAddr, userEmail, userPassword, userEmail, body); err != nil {
		check("smtp submission", false, err.Error())
		return
	}
	check("smtp submission", true, userEmail)

	// 6. IMAP login + delivery check.
	imapErr := imapVerify(*imapAddr, userEmail, userPassword, "mailez e2e")
	check("imap delivery", imapErr == nil, func() string {
		if imapErr == nil {
			return ""
		}
		return imapErr.Error()
	}())

	fmt.Printf("\n%d/%d checks passed\n", passed, checks)
	if passed != checks {
		os.Exit(1)
	}
}

type domainPage struct {
	Data []struct {
		Name string `json:"name"`
	} `json:"data"`
}

type userPage struct {
	Data []struct {
		Email string `json:"email"`
	} `json:"data"`
}

func apiJSON(hc *http.Client, base, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

func smtpSubmit(addr, user, password, to, body string) error {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	c := gosmtp.NewClient(conn)
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", user, password)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := c.Mail(user, nil); err != nil {
		return err
	}
	if err := c.Rcpt(to, nil); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

func imapVerify(addr, user, password, wantSubject string) error {
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		return err
	}
	defer c.Logout()
	if err := c.Login(user, password).Wait(); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return err
	}
	// UID SEARCH SUBJECT <wantSubject> must find the delivered message.
	cmd := c.UIDSearch(&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{
		{Key: "Subject", Value: wantSubject},
	}}, nil)
	data, err := cmd.Wait()
	if err != nil {
		return err
	}
	if data.All == nil || data.All.String() == "" {
		return fmt.Errorf("message with subject %q not found", wantSubject)
	}
	return nil
}
