package delivery

import (
	"bufio"
	"io"
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
	var buf strings.Builder
	if err := OutcleanTo(strings.NewReader(string(data)), &buf); err != nil {
		return data
	}
	return []byte(buf.String())
}

// OutcleanTo streams the outbound privacy filter: it copies r to w, dropping
// Received/client-fingerprint headers (with folded continuations) in the
// header block and normalising Mime-Version, while leaving the body verbatim.
// Line endings are preserved.
func OutcleanTo(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	inHeaders := true
	var pending string
	var pendingErr error
	for {
		var line string
		var err error
		if pending != "" || pendingErr != nil {
			line, err = pending, pendingErr
			pending, pendingErr = "", nil
		} else {
			line, err = br.ReadString('\n')
		}
		if err != nil && err != io.EOF {
			return err
		}
		last := err == io.EOF
		if inHeaders && strings.TrimRight(line, "\r\n") == "" {
			// Blank line ends the header block.
			inHeaders = false
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if last {
				return nil
			}
			continue
		}
		if inHeaders {
			trimmed := strings.TrimLeft(strings.ToLower(line), " \t")
			if matchIgnore(trimmed) {
				// Drop this header and its folded continuation lines.
				for {
					next, nerr := br.ReadString('\n')
					if nerr != nil && nerr != io.EOF {
						return nerr
					}
					if next == "" || !isFolded(next) {
						// Keep the first non-folded line for the main loop
						// (it may be the header/body separator or the next
						// header).
						pending, pendingErr = next, nerr
						break
					}
				}
				if pending == "" && pendingErr == io.EOF {
					return nil
				}
				continue
			}
			if isMimeVersion(line) {
				if _, werr := io.WriteString(w, cleanMimeVersion(line)); werr != nil {
					return werr
				}
				if last {
					return nil
				}
				continue
			}
		}
		if _, werr := io.WriteString(w, line); werr != nil {
			return werr
		}
		if last {
			return nil
		}
	}
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
	if strings.HasSuffix(line, "\n") {
		line = line[:len(line)-1]
		if strings.HasSuffix(line, "\r") {
			eol = "\r\n"
			line = line[:len(line)-1]
		} else {
			eol = "\n"
		}
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
