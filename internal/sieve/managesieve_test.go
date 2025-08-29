package sieve

import (
	"bufio"
	"context"
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

func startManageSieve(t *testing.T) (string, mailstore.SieveStore) {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
		Sieve: map[string]string{
			"alice@example.com": `require "fileinto";
if header :contains "Subject" "spam" { fileinto "Junk"; }`,
		},
	})
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &Server{
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		Scripts:   ms,
		Engine:    NewEngine(logger),
		Logger:    logger,
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
			go func() { _ = srv.ManageSieveSession(context.Background(), conn) }()
		}
	}()
	return ln.Addr().String(), ms
}

func TestManageSieveReadFlow(t *testing.T) {
	addr, ms := startManageSieve(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\r\n", s); err != nil {
			t.Fatal(err)
		}
	}
	readStatus := func() string {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
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

	// Greeting.
	if got := readStatus(); got != "OK" {
		t.Fatalf("greeting: %s", got)
	}
	write(`AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`)
	if got := readStatus(); got != "OK" {
		t.Fatalf("auth: %s", got)
	}
	write("LISTSCRIPTS")
	if got := readStatus(); got != "OK" {
		t.Fatalf("listscripts: %s", got)
	}
	write(`GETSCRIPT "default"`)
	if got := readStatus(); got != "OK" {
		t.Fatalf("getscript: %s", got)
	}
	// Write flow: PUTSCRIPT stores, SETACTIVE activates, GETSCRIPT reads the
	// stored copy, LISTSCRIPTS shows it, DELETESCRIPT removes it.
	script := `require "fileinto";
if header :contains "Subject" "news" { fileinto "Lists"; }`
	write(fmt.Sprintf(`PUTSCRIPT "custom" {%d}`+"\r\n%s", len(script), script))
	if got := readStatus(); got != "OK" {
		t.Fatalf("putscript: %s", got)
	}
	if _, err := ms.GetSieveScript(context.Background(), "alice@example.com", "custom"); err != nil {
		t.Fatalf("script not stored: %v", err)
	}
	write(`SETACTIVE "custom"`)
	if got := readStatus(); got != "OK" {
		t.Fatalf("setactive: %s", got)
	}
	scripts, err := ms.ListSieveScripts(context.Background(), "alice@example.com")
	if err != nil || len(scripts) != 1 || !scripts[0].Active {
		t.Fatalf("list after setactive: %+v err=%v", scripts, err)
	}
	write(`GETSCRIPT "custom"`)
	if got := readStatus(); got != "OK" {
		t.Fatalf("getscript custom: %s", got)
	}
	write("LISTSCRIPTS")
	if got := readStatus(); got != "OK" {
		t.Fatalf("listscripts: %s", got)
	}
	write(`DELETESCRIPT "custom"`)
	if got := readStatus(); got != "OK" {
		t.Fatalf("deletescript: %s", got)
	}
	write("LOGOUT")
	if got := readStatus(); got != "OK" {
		t.Fatalf("logout: %s", got)
	}
}

func TestManageSieveBadAuth(t *testing.T) {
	addr, _ := startManageSieve(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\r\n", s); err != nil {
			t.Fatal(err)
		}
	}
	// consume greeting
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "OK") {
			break
		}
	}
	write(`LOGIN "alice@example.com" "wrong"`)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimRight(line, "\r\n"), "NO") {
		t.Fatalf("expected NO, got %q", line)
	}
}

// TestManageSieveCheckScriptAndValidation pins RFC 5804 §2.5/§2.6:
// CHECKSCRIPT validates without storing, PUTSCRIPT refuses scripts that do
// not compile, and the SIEVE capability advertises the extensions the
// interpreter actually accepts.
func TestManageSieveCheckScriptAndValidation(t *testing.T) {
	addr, ms := startManageSieve(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s\r\n", s); err != nil {
			t.Fatal(err)
		}
	}
	readLine := func() string {
		t.Helper()
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	readStatus := func() string {
		t.Helper()
		for {
			line := readLine()
			if strings.HasPrefix(line, "OK") || strings.HasPrefix(line, "NO") {
				return line
			}
		}
	}

	// Greeting doubles as the capability list.
	var sieveCap string
	for {
		line := readLine()
		if strings.HasPrefix(line, `"SIEVE"`) {
			sieveCap = line
		}
		if strings.HasPrefix(line, "OK") {
			break
		}
	}
	padded := " " + strings.Trim(sieveCap, `"`) + " "
	for _, ext := range []string{"editheader", "vacation", "spamtest", "spamtestplus", "date", "mailbox", "regex", "index", "copy", "ereject"} {
		if !strings.Contains(padded, " "+ext+" ") {
			t.Fatalf("capability %q missing from SIEVE list: %s", ext, sieveCap)
		}
	}

	write(`AUTHENTICATE "PLAIN" "AGFsaWNlQGV4YW1wbGUuY29tAHMzY3JldA=="`)
	if line := readStatus(); !strings.HasPrefix(line, "OK") {
		t.Fatalf("auth: %s", line)
	}

	good := "require \"editheader\";\ndeleteheader \"X-Nope\";\n"
	write(fmt.Sprintf(`CHECKSCRIPT {%d}`+"\r\n%s", len(good), good))
	if line := readStatus(); !strings.HasPrefix(line, "OK") {
		t.Fatalf("checkscript valid: %s", line)
	}

	bad := "if header :contains \"Subject\" \"x\" { fileinto \"Junk\"; }" // missing require "fileinto"
	write(fmt.Sprintf(`CHECKSCRIPT {%d}`+"\r\n%s", len(bad), bad))
	if line := readStatus(); !strings.HasPrefix(line, "NO") {
		t.Fatalf("checkscript invalid must be NO: %s", line)
	}

	write(fmt.Sprintf(`PUTSCRIPT "broken" {%d}`+"\r\n%s", len(bad), bad))
	if line := readStatus(); !strings.HasPrefix(line, "NO") {
		t.Fatalf("putscript invalid must be NO: %s", line)
	}
	if _, err := ms.GetSieveScript(context.Background(), "alice@example.com", "broken"); err == nil {
		t.Fatal("invalid script was stored")
	}

	// PUTSCRIPT still accepts compiling scripts.
	ok2 := "require \"fileinto\";\nif header :contains \"Subject\" \"x\" { fileinto \"Junk\"; }\n"
	write(fmt.Sprintf(`PUTSCRIPT "good" {%d}`+"\r\n%s", len(ok2), ok2))
	if line := readStatus(); !strings.HasPrefix(line, "OK") {
		t.Fatalf("putscript valid: %s", line)
	}
}
