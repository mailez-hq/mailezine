//go:build unix

package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Maildir++ layout (ARCHITECTURE.md §5.3):
//
//	INBOX          → <root>/
//	Sent           → <root>/.Sent/
//	Foo/Bar        → <root>/.Foo.Bar/
//
// This matches the mailez deployment layout for maildir:/mail/%u (no
// :LAYOUT=fs).

// maxMailboxNameLen bounds names defensively.
const maxMailboxNameLen = 200

// ValidateName checks a mailbox name and returns an error with a reason.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("maildir: empty mailbox name")
	}
	if len(name) > maxMailboxNameLen {
		return fmt.Errorf("maildir: mailbox name too long")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("maildir: mailbox name %q has empty path segment", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("maildir: mailbox name %q must not start with '.'", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("maildir: mailbox name %q has invalid segment %q", name, seg)
		}
		for _, c := range seg {
			if c < 0x21 || c > 0x7e || c == '\\' {
				return fmt.Errorf("maildir: mailbox name %q has invalid character %q", name, c)
			}
		}
	}
	return nil
}

// dirName maps a mailbox name to its directory name under the account root.
func dirName(name string) string {
	if name == "INBOX" {
		return ""
	}
	return "." + strings.ReplaceAll(name, "/", ".")
}

// nameFromDir maps a directory name back to the mailbox name.
func nameFromDir(dir string) string {
	if dir == "" {
		return "INBOX"
	}
	return strings.ReplaceAll(strings.TrimPrefix(dir, "."), ".", "/")
}

// mailboxDir resolves the absolute directory of a mailbox.
func mailboxDir(root, name string) string {
	return filepath.Join(root, dirName(name))
}

// ensureMaildir creates cur/new/tmp for a mailbox directory.
func ensureMaildir(dir string) error {
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// isMaildirDir reports whether dir is a Maildir++ mailbox directory.
func isMaildirDir(dir string) bool {
	for _, sub := range []string{"cur", "new", "tmp"} {
		fi, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}
