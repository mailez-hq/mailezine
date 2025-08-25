package delivery

import (
	"strings"
)

// outcleanIgnore lists header names stripped from outbound messages. They
// leak client or internal-topology details (Received chains, mailer/user
// agent fingerprints, originating IPs). Matches the filter classic MTAs
// apply to user-submitted mail.
var outcleanIgnore = []string{
	"received:",
	"user-agent:",
	"x-enigmail:",
	"x-mailer:",
	"x-originating-ip:",
	"x-pgp-agent:",
}

// Outclean strips privacy-leaking headers from an outbound message before it
// is spooled: Received and client-fingerprint headers are removed together
// with their folded continuation lines, and Mime-Version is reduced to the
// bare version token (e.g. "1.0 (Mac OS X Mail 8.1 ...)" → "1.0"). The body
// is never touched. The function preserves the original line endings.
func Outclean(data []byte) []byte {
	raw := string(data)
	if !strings.Contains(raw, "\r\n") && !strings.Contains(raw, "\n") {
		return data
	}
	lines := strings.Split(raw, "\n")
	var out []string
	inHeaders := true
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if inHeaders {
			if line == "" || line == "\r" {
				// End of headers: copy the separator and the rest verbatim.
				out = append(out, line)
				inHeaders = false
				out = append(out, lines[i+1:]...)
				break
			}
			lower := strings.ToLower(line)
			trimmed := strings.TrimLeft(lower, " \t")
			if stripped := matchIgnore(trimmed); stripped {
				// Drop this header and any folded continuation lines.
				for i+1 < len(lines) && isFolded(lines[i+1]) {
					i++
				}
				continue
			}
			if isMimeVersion(line) {
				out = append(out, cleanMimeVersion(line))
				continue
			}
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

func matchIgnore(trimmedLower string) bool {
	for _, name := range outcleanIgnore {
		if strings.HasPrefix(trimmedLower, name) {
			return true
		}
	}
	return false
}

// isFolded reports whether line continues the previous header (RFC 5322
// obs-fold: leading whitespace).
func isFolded(line string) bool {
	if line == "" {
		return false
	}
	return line[0] == ' ' || line[0] == '\t'
}

func isMimeVersion(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "mime-version:")
}

// cleanMimeVersion keeps "Mime-Version: <version>" and drops any trailing
// comment (which can fingerprint the generating client).
func cleanMimeVersion(line string) string {
	// Preserve the original line ending (the header is rebuilt).
	eol := ""
	if strings.HasSuffix(line, "\r") {
		eol = "\r"
		line = line[:len(line)-1]
	}
	trimmed := strings.TrimLeft(line, " \t")
	lead := line[:len(line)-len(trimmed)]
	rest := trimmed
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[i+1:]
		rest = strings.TrimLeft(rest, " \t")
	}
	version := rest
	if i := strings.IndexAny(version, " \t"); i >= 0 {
		version = version[:i]
	}
	if version == "" {
		return line + eol
	}
	return lead + "Mime-Version: " + version + eol
}
