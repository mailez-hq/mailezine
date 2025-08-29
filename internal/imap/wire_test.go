// Wire-level regression tests for CONDSTORE STORE UNCHANGEDSINCE
// (MODIFIED response code), RFC 3501 "n:*" last-message semantics, and
// UID SORT — behaviors the typed go-imap client cannot express.
package imap

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// rawIMAP is a minimal scripted client speaking IMAP over a raw connection.
type rawIMAP struct {
	t   *testing.T
	c   net.Conn
	br  *bufio.Reader
	tag int
}

func dialRawTestServer(t *testing.T) *rawIMAP {
	t.Helper()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:           ms,
		Auth:            auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory:       dir,
		MaxMessageBytes: 1 << 20,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r := &rawIMAP{t: t, c: conn, br: bufio.NewReader(conn)}
	r.readLine() // server greeting
	if resp := r.cmd("LOGIN alice@example.com s3cret"); !strings.HasPrefix(resp, "t1 OK") {
		t.Fatalf("login failed: %s", resp)
	}
	if resp := r.cmd("SELECT INBOX"); !strings.HasPrefix(resp, "t2 OK") {
		t.Fatalf("select failed: %s", resp)
	}
	return r
}

// readLine returns one untagged/continuation line.
func (r *rawIMAP) readLine() string {
	r.t.Helper()
	_ = r.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := r.br.ReadString('\n')
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// cmd sends a command and returns the tagged completion line.
func (r *rawIMAP) cmd(format string, args ...any) string {
	r.t.Helper()
	tagged, _ := r.cmdCollect(format, args...)
	return tagged
}

// cmdCollect also returns every untagged line seen before the tagged one.
func (r *rawIMAP) cmdCollect(format string, args ...any) (string, []string) {
	r.t.Helper()
	r.tag++
	id := fmt.Sprintf("t%d", r.tag)
	if _, err := fmt.Fprintf(r.c, "%s %s\r\n", id, fmt.Sprintf(format, args...)); err != nil {
		r.t.Fatalf("write: %v", err)
	}
	var untagged []string
	for {
		line := r.readLine()
		if strings.HasPrefix(line, id+" ") {
			return line, untagged
		}
		untagged = append(untagged, line)
	}
}

// okStatus reports whether a tagged completion line is OK.
func okStatus(line string) bool {
	f := strings.Fields(line)
	return len(f) >= 2 && f[1] == "OK"
}

// appendRaw appends a message via the literal protocol.
func (r *rawIMAP) appendRaw(body string) string {
	r.t.Helper()
	r.tag++
	id := fmt.Sprintf("t%d", r.tag)
	if _, err := fmt.Fprintf(r.c, "%s APPEND INBOX {%d}\r\n", id, len(body)); err != nil {
		r.t.Fatalf("write: %v", err)
	}
	if cont := r.readLine(); !strings.HasPrefix(cont, "+") {
		r.t.Fatalf("expected continuation, got %q", cont)
	}
	if _, err := r.c.Write([]byte(body + "\r\n")); err != nil {
		r.t.Fatalf("write literal: %v", err)
	}
	for {
		line := r.readLine()
		if strings.HasPrefix(line, id+" ") {
			return line
		}
	}
}

// RFC 7162 §3.3: STORE UNCHANGEDSINCE skips messages with a higher modseq
// and reports them via the [MODIFIED <set>] response code.
func TestSTOREUnchangedSinceReturnsModified(t *testing.T) {
	r := dialRawTestServer(t)
	r.appendRaw("From: a@x.test\r\nSubject: one\r\n\r\n1\r\n")
	r.appendRaw("From: b@x.test\r\nSubject: two\r\n\r\n2\r\n")
	// uid 1 has modseq 1, uid 2 has modseq 2: UNCHANGEDSINCE=1 lets uid 1
	// through and must report uid 2 as MODIFIED.
	resp := r.cmd("UID STORE 1:2 (UNCHANGEDSINCE 1) +FLAGS (\\Seen)")
	if !okStatus(resp) {
		t.Fatalf("store failed: %s", resp)
	}
	if !strings.Contains(resp, "[MODIFIED 2]") {
		t.Fatalf("tagged OK missing [MODIFIED 2]: %s", resp)
	}
}

// RFC 3501 §6.4.8: "5:*" with three messages matches exactly message 3.
func TestStarRangeIncludesLastMessage(t *testing.T) {
	r := dialRawTestServer(t)
	r.appendRaw("From: a@x.test\r\nSubject: one\r\n\r\n1\r\n")
	r.appendRaw("From: b@x.test\r\nSubject: two\r\n\r\n2\r\n")
	r.appendRaw("From: c@x.test\r\nSubject: three\r\n\r\n3\r\n")

	resp, untagged := r.cmdCollect("FETCH 5:* (UID)")
	if !okStatus(resp) {
		t.Fatalf("fetch failed: %s", resp)
	}
	found := false
	for _, l := range untagged {
		if strings.Contains(l, "* 3 FETCH 3 (UID 3)") || strings.Contains(l, "* 3 FETCH (UID 3)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("5:* did not match the final message: %v", untagged)
	}
	resp, untagged = r.cmdCollect("UID SEARCH UID 5:*")
	if !okStatus(resp) {
		t.Fatalf("search failed: %s", resp)
	}
	found = false
	for _, l := range untagged {
		if strings.Contains(l, "SEARCH 3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("UID 5:* should match the highest UID: %v", untagged)
	}
}

// RFC 5256 §3: UID SORT orders identically to SORT but answers with UIDs.
func TestUIDSortBySubject(t *testing.T) {
	r := dialRawTestServer(t)
	r.appendRaw("From: a@x.test\r\nSubject: beta\r\n\r\n1\r\n")
	r.appendRaw("From: b@x.test\r\nSubject: alpha\r\n\r\n2\r\n")
	r.appendRaw("From: c@x.test\r\nSubject: gamma\r\n\r\n3\r\n")

	resp, untagged := r.cmdCollect("UID SORT (SUBJECT) UTF-8 ALL")
	if !okStatus(resp) {
		t.Fatalf("UID SORT failed: %s", resp)
	}
	sortLine := ""
	for _, l := range untagged {
		if strings.HasPrefix(l, "* SORT") {
			sortLine = l
		}
	}
	// Subjects alpha(uid2) beta(uid1) gamma(uid3).
	if strings.TrimSpace(sortLine) != "* SORT 2 1 3" {
		t.Fatalf("UID SORT order = %q, want \"* SORT 2 1 3\"", sortLine)
	}
}

// RFC 5256 subject ordering strips only LEADING re:/fwd: prefixes.
func TestSortSubjectLeadingPrefixOnly(t *testing.T) {
	r := dialRawTestServer(t)
	r.appendRaw("From: a@x.test\r\nSubject: Re: alpha\r\n\r\n1\r\n")
	r.appendRaw("From: b@x.test\r\nSubject: fire: safety\r\n\r\n2\r\n")

	resp, untagged := r.cmdCollect("SORT (SUBJECT) UTF-8 ALL")
	if !okStatus(resp) {
		t.Fatalf("SORT failed: %s", resp)
	}
	sortLine := ""
	for _, l := range untagged {
		if strings.HasPrefix(l, "* SORT") {
			sortLine = l
		}
	}
	// "Re: alpha" → base "alpha" sorts before "fire: safety" (which must
	// NOT lose its mid-string "re:" via naive prefix stripping).
	if strings.TrimSpace(sortLine) != "* SORT 1 2" {
		t.Fatalf("SORT subject order = %q, want \"* SORT 1 2\"", sortLine)
	}
}
