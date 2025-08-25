// RFC 4314 ACL integration tests: the go-imap client's native commands
// (MYRIGHTS/GETACL/SETACL), capability advertisement, and raw wire checks
// for the commands the client library does not expose (DELETEACL,
// LISTRIGHTS).
package imap

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	v1 "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/v2"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func TestIMAPACLCommands(t *testing.T) {
	c, ms := startTestServer(t)

	if !c.Caps().Has(imap.CapACL) {
		t.Fatal("ACL capability not advertised")
	}

	// MYRIGHTS: the owner holds the full implicit right set.
	my, err := c.MyRights("INBOX").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if my.Mailbox != "INBOX" || my.Rights.String() != "lrswipkxtecda" {
		t.Fatalf("MYRIGHTS = %+v", my)
	}

	// GETACL: empty by default.
	got, err := c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rights) != 0 {
		t.Fatalf("GETACL default = %v", got.Rights)
	}

	// SETACL grants bob a subset; the response round-trips through the
	// real client and the persisted store.
	if err := c.SetACL("INBOX", imap.RightsIdentifier("bob@example.com"),
		imap.RightModificationReplace, imap.RightSet("lrswip")).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.SetACL("INBOX", imap.RightsIdentifier("anyone"),
		imap.RightModificationReplace, imap.RightSet("lrs")).Wait(); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got.Rights[imap.RightsIdentifier("bob@example.com")].String() != "lrswip" ||
		got.Rights[imap.RightsIdentifier("anyone")].String() != "lrs" {
		t.Fatalf("GETACL after SETACL = %v", got.Rights)
	}

	// The mailstore view matches the wire view.
	stored, err := ms.GetACL(t.Context(), "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if stored["bob@example.com"] != "lrswip" || stored["anyone"] != "lrs" {
		t.Fatalf("store ACL = %v", stored)
	}

	// +rights modifies the current set; a bare string replaces it.
	if err := c.SetACL("INBOX", imap.RightsIdentifier("bob@example.com"),
		imap.RightModificationAdd, imap.RightSet("a")).Wait(); err != nil {
		t.Fatal(err)
	}
	got, err = c.GetACL("INBOX").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got.Rights[imap.RightsIdentifier("bob@example.com")].String() != "lrswipa" {
		t.Fatalf("SETACL +a = %v", got.Rights[imap.RightsIdentifier("bob@example.com")])
	}

	// RFC 4314: ACL commands are valid in the Selected state too.
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MyRights("INBOX").Wait(); err != nil {
		t.Fatalf("MYRIGHTS in selected state: %v", err)
	}
	if _, err := c.GetACL("INBOX").Wait(); err != nil {
		t.Fatalf("GETACL in selected state: %v", err)
	}
}

// TestIMAPACLWire exercises DELETEACL and LISTRIGHTS over a raw connection,
// asserting the exact untagged response lines (RFC 4314 §6).
func TestIMAPACLWire(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	ms := mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
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
	defer conn.Close()
	br := bufio.NewReader(conn)
	readLine := func() string {
		t.Helper()
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	send := func(tag, s string) []string {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, s); err != nil {
			t.Fatal(err)
		}
		var untagged []string
		for {
			line := readLine()
			if strings.HasPrefix(line, "* ") {
				untagged = append(untagged, strings.TrimPrefix(line, "* "))
				continue
			}
			if strings.HasPrefix(line, tag+" OK") || strings.HasPrefix(line, tag+" NO") {
				return untagged
			}
			t.Fatalf("unexpected line %q", line)
		}
	}

	readLine() // greeting
	send("a1", `LOGIN "alice@example.com" "s3cret"`)

	// LISTRIGHTS reports the current grant and every grantable right.
	resp := send("a2", `LISTRIGHTS "INBOX" "carol@example.com"`)
	if len(resp) != 1 || resp[0] != `LISTRIGHTS INBOX "carol@example.com" NIL l r s w i p k x t e c d a` {
		t.Fatalf("LISTRIGHTS = %v", resp)
	}

	// SETACL then DELETEACL: the ACL disappears.
	send("a3", `SETACL "INBOX" "carol@example.com" lr`)
	resp = send("a4", `GETACL "INBOX"`)
	if len(resp) != 1 || !strings.Contains(resp[0], `"carol@example.com" lr`) {
		t.Fatalf("GETACL after SETACL = %v", resp)
	}
	send("a5", `DELETEACL "INBOX" "carol@example.com"`)
	resp = send("a6", `GETACL "INBOX"`)
	if len(resp) != 1 || strings.Contains(resp[0], "carol") {
		t.Fatalf("GETACL after DELETEACL = %v", resp)
	}

	// Unknown commands still get BAD (extension hook must not swallow them).
	if _, err := fmt.Fprintf(conn, "a7 BOGUS\r\n"); err != nil {
		t.Fatal(err)
	}
	if line := readLine(); !strings.HasPrefix(line, "a7 BAD") {
		t.Fatalf("unknown command response = %q", line)
	}
}

// TestIMAPACLBackendCompat validates that the untagged responses parse with
// the exact logic used by the mailez backend mail client (imap.ParseNamedResp
// over the raw response lines), so the webmail sharing UI works unchanged.
func TestIMAPACLBackendCompat(t *testing.T) {
	c, _ := startTestServer(t)
	if err := c.SetACL("INBOX", imap.RightsIdentifier("bob@example.com"),
		imap.RightModificationReplace, imap.RightSet("lrswip")).Wait(); err != nil {
		t.Fatal(err)
	}
	_ = c.Logout().Wait()

	// Drive the same commands the backend sends and parse the lines the
	// way mailez/backend/internal/mail/acl.go does.
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	srv := New(&Server{
		Store:           mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob())),
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
	defer conn.Close()
	br := bufio.NewReader(conn)
	readLine := func() string {
		t.Helper()
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	send := func(tag, s string) []*v1.DataResp {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, s); err != nil {
			t.Fatal(err)
		}
		var resps []*v1.DataResp
		for {
			line := readLine()
			if strings.HasPrefix(line, tag) {
				return resps
			}
			if strings.HasPrefix(line, "* ") {
				resps = append(resps, &v1.DataResp{Tag: "*", Fields: splitResponseFields(strings.TrimPrefix(line, "* "))})
			}
		}
	}

	readLine() // greeting
	send("b1", `LOGIN "alice@example.com" "s3cret"`)
	send("b1a", `SETACL "INBOX" "bob@example.com" lrswip`)

	// GETACL, exactly as FolderACL consumes it.
	resps := send("b2", `GETACL "INBOX"`)
	var entries []struct {
		Identifier string
		Rights     string
	}
	for _, resp := range resps {
		name, fields, ok := v1.ParseNamedResp(resp)
		if !ok || name != "ACL" || len(fields) < 2 {
			continue
		}
		for i := 1; i+1 < len(fields); i += 2 {
			id, _ := fields[i].(string)
			rights, _ := fields[i+1].(string)
			if id != "" {
				entries = append(entries, struct {
					Identifier string
					Rights     string
				}{id, rights})
			}
		}
	}
	if len(entries) != 1 || entries[0].Identifier != "bob@example.com" || entries[0].Rights != "lrswip" {
		t.Fatalf("backend GETACL parse = %+v", entries)
	}

	// MYRIGHTS, exactly as MyRights consumes it.
	resps = send("b3", `MYRIGHTS "INBOX"`)
	my := ""
	for _, resp := range resps {
		name, fields, ok := v1.ParseNamedResp(resp)
		if !ok || name != "MYRIGHTS" || len(fields) < 2 {
			continue
		}
		my, _ = fields[len(fields)-1].(string)
	}
	if my != "lrswipkxtecda" {
		t.Fatalf("backend MYRIGHTS parse = %q", my)
	}

	// LISTRIGHTS, exactly as ListRights consumes it.
	resps = send("b4", `LISTRIGHTS "INBOX" "bob@example.com"`)
	granted, available := "", ""
	for _, resp := range resps {
		name, fields, ok := v1.ParseNamedResp(resp)
		if !ok || name != "LISTRIGHTS" || len(fields) < 3 {
			continue
		}
		granted, _ = fields[2].(string)
		for _, f := range fields[3:] {
			if s, ok := f.(string); ok {
				available += s
			}
		}
	}
	if granted != "lrswip" || available != "lrswipkxtecda" {
		t.Fatalf("backend LISTRIGHTS parse = granted %q available %q", granted, available)
	}
}

// splitResponseFields is a minimal tokenizer for untagged response lines:
// quoted strings become strings, bare tokens stay atoms, NIL becomes nil.
// The go-imap v1 client reader produces the same shape (DataResp.Fields).
func splitResponseFields(line string) []interface{} {
	var fields []interface{}
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}
		if line[i] == '"' {
			j := i + 1
			for j < len(line) && line[j] != '"' {
				j++
			}
			fields = append(fields, line[i+1:j])
			i = j + 1
			continue
		}
		j := i
		for j < len(line) && line[j] != ' ' {
			j++
		}
		tok := line[i:j]
		if tok == "NIL" {
			fields = append(fields, nil)
		} else {
			fields = append(fields, tok) // v1 ReadAtom returns plain string
		}
		i = j
	}
	return fields
}
