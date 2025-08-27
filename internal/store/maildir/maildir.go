//go:build unix

// Package maildir implements the Maildir++ backend (ARCHITECTURE.md §5.3).
//
// Compatibility targets, verified against the mailez deployment template:
//
//   - mail_location = maildir:/mail/%u, so an account root holds cur/new/tmp;
//   - Maildir++ layout (no :LAYOUT=fs): INBOX is the root, submailboxes are
//     ".Name" directories with dots for hierarchy separators;
//   - UIDs live in dovecot-uidlist (version 3), guarded by
//     dovecot-uidlist.lock; system flags are the maildir ":2," info section;
//   - keywords and derived metadata live in a rebuildable sidecar
//     (.mailezine/keywords.json) — losing it loses keywords, never messages.
//
// Concurrency contract: single writer per account (in-process mutex + file
// lock); multi-instance writes are not supported (D7 in DECISIONS.md).
package maildir

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Sentinel errors of the maildir backend.
var (
	ErrNotFound      = errors.New("maildir: not found")
	ErrMailboxExists = errors.New("maildir: mailbox exists")
)

// Flag is a maildir system flag (RFC-style \Seen etc. map to these letters).
type Flag byte

// System flags, matching the maildir ":2," info section.
const (
	FlagDraft   Flag = 'D'
	FlagFlagged Flag = 'F'
	FlagPassed  Flag = 'P'
	FlagReplied Flag = 'R'
	FlagSeen    Flag = 'S'
	FlagTrashed Flag = 'T'
)

// imapFromMaildir maps maildir ":2," system flags to IMAP flag strings.
var imapFromMaildir = map[Flag]string{
	FlagDraft:   "\\Draft",
	FlagFlagged: "\\Flagged",
	FlagPassed:  "\\Passed",
	FlagReplied: "\\Answered",
	FlagSeen:    "\\Seen",
	FlagTrashed: "\\Trashed",
}

// MaildirToIMAPFlags converts maildir ":2," system flags to IMAP flag
// strings (used by the migrate command; \Deleted has no maildir letter and
// lives in the sidecar keywords).
func MaildirToIMAPFlags(flags []Flag) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if s, ok := imapFromMaildir[f]; ok {
			out = append(out, s)
		}
	}
	return out
}

// InfoSep is the separator between the base filename and the ":2," info.
const InfoSep = ":2,"

// Message is one stored message.
type Message struct {
	UID          uint32
	Subdir       string // "cur" or "new"
	Filename     string // complete filename including the info section (":2," or legacy IMAP ",S=,W=")
	Flags        []Flag
	Size         int64
	InternalDate time.Time
}

// Has reports whether a system flag is set.
func (m Message) Has(f Flag) bool {
	for _, g := range m.Flags {
		if g == f {
			return true
		}
	}
	return false
}

// ParseFlags decodes the ":2," info section (the part after the last ':').
// Unknown letters are ignored, matching maildir's extensibility.
func ParseFlags(info string) []Flag {
	var out []Flag
	for _, c := range info {
		if f := flagFromByte(byte(c)); f != 0 {
			out = append(out, f)
		}
	}
	return out
}

// FormatFlags encodes flags into the canonical ":2," suffix (letters sorted
// alphabetically, as the maildir specification requires).
func FormatFlags(flags []Flag) string {
	set := map[byte]bool{}
	for _, f := range flags {
		if f != 0 {
			set[byte(f)] = true
		}
	}
	letters := make([]byte, 0, len(set))
	for c := range set {
		letters = append(letters, c)
	}
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })
	return "2," + string(letters)
}

// SplitInfo separates a full filename into base and info section.
func SplitInfo(filename string) (base, info string) {
	i := strings.LastIndex(filename, ":")
	if i < 0 {
		return filename, ""
	}
	return filename[:i], filename[i+1:]
}

// newFilename builds a unique filename:
// <sec>.<usec>M<pid>V<seq>.<host>. Callers append the ":2,<flags>" info
// section. The host is fixed so files are portable across machines;
// uniqueness comes from pid + monotonic seq.
func newFilename(now time.Time, pid int, seq uint64, flags []Flag) string {
	sec := now.Unix()
	usec := now.UnixMicro() % 1_000_000
	return fmt.Sprintf("%d.%06dM%dV%d.mailezine",
		sec, usec, pid, seq)
}

func flagFromByte(c byte) Flag {
	switch Flag(c) {
	case FlagDraft, FlagFlagged, FlagPassed, FlagReplied, FlagSeen, FlagTrashed:
		return Flag(c)
	default:
		return 0
	}
}

// hostnameForFilename returns a stable, safe host token for filenames.
func hostnameForFilename() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return sanitizeHost(h)
	}
	return "mailezine"
}

func sanitizeHost(h string) string {
	var b strings.Builder
	for _, c := range h {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "mailezine"
	}
	return b.String()
}
