//go:build unix

package maildir

import "testing"

func TestDirNameMapping(t *testing.T) {
	cases := []struct{ name, dir string }{
		{"INBOX", ""},
		{"Sent", ".Sent"},
		{"Foo", ".Foo"},
		{"Foo/Bar", ".Foo.Bar"},
		{"A/B/C", ".A.B.C"},
	}
	for _, c := range cases {
		if got := dirName(c.name); got != c.dir {
			t.Errorf("dirName(%q) = %q, want %q", c.name, got, c.dir)
		}
		if got := nameFromDir(c.dir); got != c.name {
			t.Errorf("nameFromDir(%q) = %q, want %q", c.dir, got, c.name)
		}
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{"INBOX", "Sent", "Foo/Bar", "A-B_C", "Drafts"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) rejected: %v", name, err)
		}
	}
	invalid := []string{
		"", "/x", "x/", ".hidden", "a/../b", "a//b", `a\b`, "a b",
		"a\tb", "INBOX/x/../y", string(make([]byte, maxMailboxNameLen+1)),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) accepted, want rejection", name)
		}
	}
}
