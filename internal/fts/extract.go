package fts

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"io"
	"mime"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// extractAttachmentText returns searchable text for common attachment types
// without external services (政企内网零依赖): plain text, HTML, PDF,
// OOXML (docx/xlsx/pptx), ODF and RFC 822. Unknown types return "" and the
// optional Tika endpoint (when configured) covers the rest.
func extractAttachmentText(filename, contentType string, data []byte) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if base, _, err := mime.ParseMediaType(ct); err == nil {
		ct = strings.ToLower(base)
	}
	ext := strings.ToLower(path.Ext(filename))
	switch {
	case strings.HasPrefix(ct, "text/"), ct == "application/json", ct == "application/xml",
		ct == "application/x-javascript", ct == "application/javascript",
		ct == "application/csv", ct == "application/x-csv", ct == "application/sql",
		ct == "application/rtf", ct == "application/x-httpd-php", ct == "application/x-sh":
		return normalizeText(string(data))
	case ct == "application/pdf" || ext == ".pdf":
		return pdfText(data)
	case ct == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || ext == ".docx":
		return officeText(data, "word/document.xml", "t")
	case ct == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" || ext == ".xlsx":
		return officeText(data, "xl/sharedStrings.xml", "t")
	case ct == "application/vnd.openxmlformats-officedocument.presentationml.presentation" || ext == ".pptx":
		return officeZipText(data, "ppt/slides/slide", "t")
	case ct == "application/vnd.oasis.opendocument.text" || ext == ".odt" ||
		ct == "application/vnd.oasis.opendocument.spreadsheet" || ext == ".ods" ||
		ct == "application/vnd.oasis.opendocument.presentation" || ext == ".odp":
		return officeZipText(data, "content.xml", "p")
	case ct == "message/rfc822" || ext == ".eml" || ext == ".msg":
		return normalizeText(string(data))
	}
	return ""
}

func normalizeText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	return whitespaceRe.ReplaceAllString(s, " ")
}

// ---------------------------------------------------------------------------
// PDF text extraction (minimal, pure Go): finds page content streams,
// inflates Flate streams and pulls the strings from Tj / TJ operators.
// Encrypted or image-only PDFs yield no text (Tika covers those when
// configured).
// ---------------------------------------------------------------------------

var (
	streamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	tjRe     = regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\)\s*Tj`)
	tjArrRe  = regexp.MustCompile(`\[((?:[^\[\]]|\\.)*)\]\s*TJ`)
	strRe    = regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\)`)
)

func pdfText(data []byte) string {
	var out []string
	seen := map[string]bool{}
	for _, m := range streamRe.FindAllSubmatch(data, -1) {
		raw := m[1]
		decoded := raw
		if r, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			if b, err := io.ReadAll(io.LimitReader(r, 16<<20)); err == nil {
				decoded = b
			}
		}
		// Page content streams contain BT/ET text objects; font/image
		// streams rarely do, so this keeps the text signal clean.
		if !bytes.Contains(decoded, []byte("BT")) {
			continue
		}
		text := pdfContentText(decoded)
		if text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return normalizeText(strings.Join(out, "\n"))
}

func pdfContentText(stream []byte) string {
	var parts []string
	for _, m := range tjRe.FindAllSubmatch(stream, -1) {
		parts = append(parts, pdfDecodeString(m[1]))
	}
	for _, m := range tjArrRe.FindAllSubmatch(stream, -1) {
		var cells []string
		for _, s := range strRe.FindAllSubmatch(m[1], -1) {
			cells = append(cells, pdfDecodeString(s[1]))
		}
		parts = append(parts, strings.Join(cells, ""))
	}
	return strings.Join(parts, "")
}

func pdfDecodeString(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' {
			sb.WriteByte(c)
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		switch b[i] {
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case '(', ')', '\\':
			sb.WriteByte(b[i])
		default:
			if b[i] >= '0' && b[i] <= '7' {
				oct := []byte{b[i]}
				for j := 1; j < 3 && i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '7'; j++ {
					i++
					oct = append(oct, b[i])
				}
				if n, err := strconv.ParseUint(string(oct), 8, 8); err == nil {
					sb.WriteByte(byte(n))
				}
			} else {
				sb.WriteByte(b[i])
			}
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// OOXML / ODF text extraction via zip + XML tag text.
// ---------------------------------------------------------------------------

// officeText extracts <tag> text from the first matching zip entry.
func officeText(data []byte, entry, tag string) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	for _, f := range zr.File {
		if f.Name == entry {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			b, err := io.ReadAll(io.LimitReader(rc, 32<<20))
			rc.Close()
			if err != nil {
				return ""
			}
			return xmlTagText(b, tag)
		}
	}
	return ""
}

// officeZipText aggregates <tag> text from every entry whose name has the
// given prefix (pptx slides, odt content).
func officeZipText(data []byte, prefix, tag string) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var out []string
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			continue
		}
		if s := xmlTagText(b, tag); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// xmlTagText returns the concatenated inner text of every <tag ...>...</tag>
// element (namespace prefixes ignored).
func xmlTagText(b []byte, tag string) string {
	re := regexp.MustCompile(`(?s)<(?:[a-zA-Z0-9_]*:)?` + regexp.QuoteMeta(tag) + `[^>]*>(.*?)</(?:[a-zA-Z0-9_]*:)?` + regexp.QuoteMeta(tag) + `>`)
	var out []string
	for _, m := range re.FindAllSubmatch(b, -1) {
		out = append(out, string(m[1]))
	}
	return normalizeText(strings.Join(out, " "))
}
