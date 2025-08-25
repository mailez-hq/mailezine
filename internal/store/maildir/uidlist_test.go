//go:build unix

package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUIDListDovecotFormat(t *testing.T) {
	data := "3\n1700000000\n1 cur/123.M1:2,S\n2 new/456.M2:2,\n3 cur/789.M3:2,FR\n"
	ul, err := parseUIDList([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if ul.validity != 1700000000 {
		t.Fatalf("validity = %d, want 1700000000", ul.validity)
	}
	if len(ul.entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(ul.entries))
	}
	if ul.entries[1] != "cur/123.M1:2,S" || ul.entries[3] != "cur/789.M3:2,FR" {
		t.Fatalf("entries mismatch: %v", ul.entries)
	}
}

func TestUIDListSerializeRoundTrip(t *testing.T) {
	ul := &uidList{validity: 42, entries: map[uint32]string{
		2: "new/b:2,",
		1: "cur/a:2,S",
	}}
	parsed, err := parseUIDList(serializeUIDList(ul))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.validity != 42 || len(parsed.entries) != 2 || parsed.entries[1] != "cur/a:2,S" {
		t.Fatalf("round trip mismatch: %+v", parsed)
	}
	// UIDs must be serialized in ascending order.
	text := string(serializeUIDList(ul))
	if strings.Index(text, "1 a:2,S") > strings.Index(text, "2 :b:2,") {
		t.Fatalf("UIDs not sorted:\n%s", text)
	}
	// The metadata line keeps the legacy IMAP V/N/G shape.
	if !strings.Contains(text, "V42 N3 G") {
		t.Fatalf("metadata line missing V/N/G:\n%s", text)
	}
}

// TestParseDovecotV3RealFormat pins compatibility with a uidlist actually
// written by legacy IMAP 2.3.21 (the seeding container): version and V/N/G
// metadata on one line, ":" prefix for new/ files, and ",S=,W=" info on
// flag-less filenames.
func TestParseDovecotV3RealFormat(t *testing.T) {
	data := "3 V1787599608 N1 Gd82b6613f89a8c6a12000000ef8c9014\n" +
		"1 :1787599608.M325463P18.d386b03ffafa,S=431,W=445\n" +
		"2 cur/2.example,S=100,W=120:2,S\n"
	ul, err := parseUIDList([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if ul.validity != 1787599608 {
		t.Fatalf("validity = %d", ul.validity)
	}
	if ul.next != 1 || ul.guid != "d82b6613f89a8c6a12000000ef8c9014" {
		t.Fatalf("metadata: next=%d guid=%q", ul.next, ul.guid)
	}
	if got := ul.entries[1]; got != "new/1787599608.M325463P18.d386b03ffafa,S=431,W=445" {
		t.Fatalf("entry 1 = %q", got)
	}
	if got := ul.entries[2]; got != "cur/2.example,S=100,W=120:2,S" {
		t.Fatalf("entry 2 = %q", got)
	}
	round := string(serializeUIDList(ul))
	for _, want := range []string{
		"V1787599608 N3 Gd82b6613f89a8c6a12000000ef8c9014",
		"1 :1787599608.M325463P18.d386b03ffafa,S=431,W=445",
		"2 2.example,S=100,W=120:2,S",
	} {
		if !strings.Contains(round, want) {
			t.Fatalf("serialized uidlist missing %q:\n%s", want, round)
		}
	}
}

func TestParseUIDListErrors(t *testing.T) {
	for _, data := range []string{"", "2\n1\n", "3\nnotanumber\n", "3\n1\n1\n2\n"} {
		if _, err := parseUIDList([]byte(data)); err == nil {
			t.Fatalf("expected error for %q", data)
		}
	}
}

func TestLoadUIDListMissing(t *testing.T) {
	dir := t.TempDir()
	ul, err := loadUIDList(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ul.validity == 0 || len(ul.entries) != 0 {
		t.Fatalf("fresh uidlist: validity=%d entries=%d", ul.validity, len(ul.entries))
	}
}

// TestDovecotCompatibility mounts a maildir exactly as legacy IMAP would have
// left it and verifies UIDs and flags come through.
func TestDovecotCompatibility(t *testing.T) {
	root := t.TempDir()
	acct, err := OpenAccount(root)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := acct.OpenMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mb.dir, "cur", "1234567890.M1.abc:2,S"), []byte("seen"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mb.dir, "new", "1234567891.M2.def:2,"), []byte("unseen"), 0o600); err != nil {
		t.Fatal(err)
	}
	uidlist := "3\n1700000000\n1 cur/1234567890.M1.abc:2,S\n2 new/1234567891.M2.def:2,\n"
	if err := os.WriteFile(filepath.Join(mb.dir, "dovecot-uidlist"), []byte(uidlist), 0o600); err != nil {
		t.Fatal(err)
	}

	msgs, err := mb.Messages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].UID != 1 || !msgs[0].Has(FlagSeen) || msgs[0].Subdir != "cur" || msgs[0].Size != 4 {
		t.Fatalf("msg1: %+v", msgs[0])
	}
	if msgs[1].UID != 2 || msgs[1].Has(FlagSeen) || msgs[1].Subdir != "new" {
		t.Fatalf("msg2: %+v", msgs[1])
	}
}
