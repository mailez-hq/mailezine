package imapserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime"
	nmail "net/mail"
	"regexp"
	"strings"

	"github.com/emersion/go-imap/v2"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// ExtractBodySection extracts a section of a message body.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractBodySection(r io.Reader, item *imap.FetchItemBodySection) []byte {
	var (
		header textproto.Header
		body   io.Reader
	)

	br := bufio.NewReader(r)
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil
	}
	body = br

	parentMediaType, header, body := findMessagePart(header, body, item.Part)
	if body == nil {
		return nil
	}

	if len(item.Part) > 0 {
		switch item.Specifier {
		case imap.PartSpecifierHeader, imap.PartSpecifierText:
			header, body = openMessagePart(header, body, parentMediaType)
		}
	}

	// Filter header fields
	if len(item.HeaderFields) > 0 {
		keep := make(map[string]struct{})
		for _, k := range item.HeaderFields {
			keep[strings.ToLower(k)] = struct{}{}
		}
		for field := header.Fields(); field.Next(); {
			if _, ok := keep[strings.ToLower(field.Key())]; !ok {
				field.Del()
			}
		}
	}
	for _, k := range item.HeaderFieldsNot {
		header.Del(k)
	}

	// Write the requested data to a buffer
	var buf bytes.Buffer

	writeHeader := true
	switch item.Specifier {
	case imap.PartSpecifierNone:
		writeHeader = len(item.Part) == 0
	case imap.PartSpecifierText:
		writeHeader = false
	}
	if writeHeader {
		if err := textproto.WriteHeader(&buf, header); err != nil {
			return nil
		}
	}

	switch item.Specifier {
	case imap.PartSpecifierNone, imap.PartSpecifierText:
		if _, err := io.Copy(&buf, body); err != nil {
			return nil
		}
	}

	return extractPartial(buf.Bytes(), item.Partial)
}

func findMessagePart(header textproto.Header, body io.Reader, partPath []int) (string, textproto.Header, io.Reader) {
	// First part of non-multipart message refers to the message itself
	msgHeader := gomessage.Header{Header: header}
	mediaType, _, _ := msgHeader.ContentType()
	if !strings.HasPrefix(mediaType, "multipart/") && len(partPath) > 0 && partPath[0] == 1 {
		partPath = partPath[1:]
	}

	var parentMediaType string
	for i := 0; i < len(partPath); i++ {
		partNum := partPath[i]

		header, body = openMessagePart(header, body, parentMediaType)

		msgHeader := gomessage.Header{Header: header}
		mediaType, typeParams, _ := msgHeader.ContentType()
		if !strings.HasPrefix(mediaType, "multipart/") {
			if partNum != 1 {
				return "", textproto.Header{}, nil
			}
			continue
		}

		mr := textproto.NewMultipartReader(body, typeParams["boundary"])
		found := false
		for j := 1; j <= partNum; j++ {
			p, err := mr.NextPart()
			if err != nil {
				return "", textproto.Header{}, nil
			}

			if j == partNum {
				parentMediaType = mediaType
				header = p.Header
				body = p
				found = true
				break
			}
		}
		if !found {
			return "", textproto.Header{}, nil
		}
	}

	return parentMediaType, header, body
}

func openMessagePart(header textproto.Header, body io.Reader, parentMediaType string) (textproto.Header, io.Reader) {
	msgHeader := gomessage.Header{Header: header}
	mediaType, _, _ := msgHeader.ContentType()
	if !msgHeader.Has("Content-Type") && parentMediaType == "multipart/digest" {
		mediaType = "message/rfc822"
	}
	if mediaType == "message/rfc822" || mediaType == "message/global" {
		br := bufio.NewReader(body)
		header, _ = textproto.ReadHeader(br)
		return header, br
	}
	return header, body
}

func extractPartial(b []byte, partial *imap.SectionPartial) []byte {
	if partial == nil {
		return b
	}

	end := partial.Offset + partial.Size
	if partial.Offset > int64(len(b)) {
		return nil
	}
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return b[partial.Offset:end]
}

func ExtractBinarySection(r io.Reader, item *imap.FetchItemBinarySection) []byte {
	var (
		header textproto.Header
		body   io.Reader
	)

	br := bufio.NewReader(r)
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil
	}
	body = br

	_, header, body = findMessagePart(header, body, item.Part)
	if body == nil {
		return nil
	}

	part, err := gomessage.New(gomessage.Header{Header: header}, body)
	if err != nil {
		return nil
	}

	// Write the requested data to a buffer
	var buf bytes.Buffer

	if len(item.Part) == 0 {
		if err := textproto.WriteHeader(&buf, part.Header.Header); err != nil {
			return nil
		}
	}

	if _, err := io.Copy(&buf, part.Body); err != nil {
		return nil
	}

	return extractPartial(buf.Bytes(), item.Partial)
}

func ExtractBinarySectionSize(r io.Reader, item *imap.FetchItemBinarySectionSize) uint32 {
	// TODO: optimize
	b := ExtractBinarySection(r, &imap.FetchItemBinarySection{Part: item.Part})
	return uint32(len(b))
}

// ExtractEnvelope returns a message envelope from its header.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractEnvelope(h textproto.Header) *imap.Envelope {
	mh := mail.Header{Header: gomessage.Header{Header: h}}
	date, _ := mh.Date()
	subject := decodeHeaderText(mh.Get("Subject"))
	inReplyTo, _ := mh.MsgIDList("In-Reply-To")
	messageID, _ := mh.MessageID()
	return &imap.Envelope{
		Date:      date,
		Subject:   subject,
		From:      parseAddressList(mh, "From"),
		Sender:    parseAddressList(mh, "Sender"),
		ReplyTo:   parseAddressList(mh, "Reply-To"),
		To:        parseAddressList(mh, "To"),
		Cc:        parseAddressList(mh, "Cc"),
		Bcc:       parseAddressList(mh, "Bcc"),
		InReplyTo: inReplyTo,
		MessageID: messageID,
	}
}

// addrEmailRe is the last-resort extraction used when the RFC 5322 address
// parser rejects a header: losing the display name is cosmetic, losing the
// whole From (blank sender in the client) is not.
var addrEmailRe = regexp.MustCompile(`[^\s<>,;"']+@[^\s<>,;"']+`)

// charsetReader resolves charset labels through both registries that matter:
// IANA (what mail senders quote, via ianaindex) and WHATWG (the alias set
// browsers accept, via htmlindex), so "=?GBK?B?...?=", "=?Big5?B?...?=",
// "=?Shift_JIS?B?...?=", "=?ks_c_5601-1987?B?...?=" and
// "=?windows-1251?B?...?=" all decode. The stdlib decoder behind
// mail.Header.Subject() knows only UTF-8 and ISO-8859-1, which is how one
// encoded word reached the list row and the reading-pane title untouched; a
// hand-written switch would just move the same gap to the next charset.
func CharsetReader(charset string, input io.Reader) (io.Reader, error) {
	if enc, err := ianaindex.MIME.Encoding(charset); err == nil && enc != nil {
		return enc.NewDecoder().Reader(input), nil
	}
	if enc, err := htmlindex.Get(charset); err == nil {
		return enc.NewDecoder().Reader(input), nil
	}
	return nil, fmt.Errorf("unknown charset %q", charset)
}

// message.CharsetReader is go-message's process-wide hook for MIME parts: with
// it set, every parse (IMAP body sections, the searchable text below) decodes
// declared charsets instead of failing them.
func init() {
	gomessage.CharsetReader = CharsetReader
}

// addressDecoder decodes RFC 2047 display names, so a =?GBK?B?...?= From
// does not blank the address list.
var addressDecoder = &mime.WordDecoder{CharsetReader: CharsetReader}

// headerTextDecoder decodes RFC 2047 words in header values. The stdlib
// decoder only knows UTF-8/ISO-8859-1, so a GBK subject (every mail from
// 126.com/163.com) came through as the raw "=?GBK?B?...?=" — in the list row,
// the reading pane title and every thread key derived from it.
var headerTextDecoder = &mime.WordDecoder{CharsetReader: CharsetReader}

// decodeHeaderText decodes an RFC 2047 header value, falling back to the raw
// value when a word cannot be decoded: showing the encoded form beats showing
// nothing.
func decodeHeaderText(raw string) string {
	if raw == "" {
		return ""
	}
	decoded, err := headerTextDecoder.DecodeHeader(raw)
	if err != nil {
		return raw
	}
	return decoded
}

// maxMessageTextPart bounds how much of one text part goes into the searchable
// rendering; a message with a multi-megabyte text body is indexed only up to
// here, which is far past anything a query can distinguish.
const maxMessageTextPart = 1 << 20

// MessageText renders a message as UTF-8 text for searching and indexing:
// every header value (RFC 2047 words and their charsets decoded) followed by
// each text part, transfer- and charset-decoded.
//
// SEARCH and the full-text index match against this instead of the raw bytes,
// which is what lets a UTF-8 query find a message sent in GBK, Big5 or
// Shift_JIS — and what makes the text of a base64-encoded part searchable at
// all, since the raw bytes hold only the base64.
func MessageText(raw []byte) string {
	var sb strings.Builder
	if h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw))); err == nil {
		appendHeaderText(&sb, &h)
	}
	sb.WriteString(MessageBodyText(raw))
	return sb.String()
}

// MessageBodyText is MessageText without the headers: what BODY criteria match.
func MessageBodyText(raw []byte) string {
	var sb strings.Builder
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return sb.String()
	}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		// An absent Content-Type means text/plain (RFC 2045 §5.2), which is
		// also how a single-part message reaches this loop: go-message wraps
		// it as a one-part multipart.
		if ct != "" && !strings.HasPrefix(ct, "text/") {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(p.Body, maxMessageTextPart))
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// appendHeaderText adds the header values a query can meaningfully match,
// decoding encoded words. Header names are included too: SEARCH matches them
// the same way a raw-byte scan used to.
func appendHeaderText(sb *strings.Builder, h *textproto.Header) {
	for it := h.Fields(); it.Next(); {
		value := it.Value()
		if len(value) > 8<<10 {
			value = value[:8<<10]
		}
		sb.WriteString(it.Key())
		sb.WriteString(": ")
		sb.WriteString(decodeHeaderText(value))
		sb.WriteByte('\n')
	}
}

var addressParser = nmail.AddressParser{}

func parseAddressList(mh mail.Header, k string) []imap.Address {
	// TODO: handle groups
	raw := strings.TrimSpace(mh.Get(k))
	if raw == "" {
		return nil
	}
	// Decode RFC 2047 words first (GBK/Big5-aware): DecodeHeader keeps
	// unknown words verbatim, then the stdlib address parser sees plain
	// UTF-8 display names instead of encoded tokens it would reject.
	decoded, derr := addressDecoder.DecodeHeader(raw)
	if derr != nil || decoded == "" {
		decoded = raw
	}
	addrs, err := addressParser.ParseList(decoded)
	if err != nil || len(addrs) == 0 {
		// Fall back to plain address extraction: the strict parser rejects
		// odd-but-real-world headers (encoded names it cannot decode,
		// stray tokens), and an email-only entry beats an empty From.
		out := make([]imap.Address, 0, 1)
		for _, m := range addrEmailRe.FindAllString(raw, -1) {
			if mailbox, host, ok := strings.Cut(m, "@"); ok {
				out = append(out, imap.Address{Mailbox: mailbox, Host: host})
			}
		}
		return out
	}
	var l []imap.Address
	for _, addr := range addrs {
		mailbox, host, ok := strings.Cut(addr.Address, "@")
		if !ok {
			continue
		}
		l = append(l, imap.Address{
			Name:    addr.Name,
			Mailbox: mailbox,
			Host:    host,
		})
	}
	return l
}

// ExtractBodyStructure extracts the structure of a message body.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractBodyStructure(r io.Reader) imap.BodyStructure {
	br := bufio.NewReader(r)
	header, _ := textproto.ReadHeader(br)
	return extractBodyStructure(header, br)
}

func extractBodyStructure(rawHeader textproto.Header, r io.Reader) imap.BodyStructure {
	header := gomessage.Header{Header: rawHeader}

	mediaType, typeParams, _ := header.ContentType()
	primaryType, subType, _ := strings.Cut(mediaType, "/")

	if primaryType == "multipart" {
		bs := &imap.BodyStructureMultiPart{Subtype: subType}
		mr := textproto.NewMultipartReader(r, typeParams["boundary"])
		for {
			part, _ := mr.NextPart()
			if part == nil {
				break
			}
			bs.Children = append(bs.Children, extractBodyStructure(part.Header, part))
		}
		bs.Extended = &imap.BodyStructureMultiPartExt{
			Params:      typeParams,
			Disposition: getContentDisposition(header),
			Language:    getContentLanguage(header),
			Location:    header.Get("Content-Location"),
		}
		return bs
	} else {
		body, _ := io.ReadAll(r) // TODO: optimize
		bs := &imap.BodyStructureSinglePart{
			Type:        primaryType,
			Subtype:     subType,
			Params:      typeParams,
			ID:          header.Get("Content-Id"),
			Description: header.Get("Content-Description"),
			Encoding:    header.Get("Content-Transfer-Encoding"),
			Size:        uint32(len(body)),
		}
		if mediaType == "message/rfc822" || mediaType == "message/global" {
			br := bufio.NewReader(bytes.NewReader(body))
			childHeader, _ := textproto.ReadHeader(br)
			bs.MessageRFC822 = &imap.BodyStructureMessageRFC822{
				Envelope:      ExtractEnvelope(childHeader),
				BodyStructure: extractBodyStructure(childHeader, br),
				NumLines:      int64(bytes.Count(body, []byte("\n"))),
			}
		}
		if primaryType == "text" {
			bs.Text = &imap.BodyStructureText{
				NumLines: int64(bytes.Count(body, []byte("\n"))),
			}
		}
		bs.Extended = &imap.BodyStructureSinglePartExt{
			Disposition: getContentDisposition(header),
			Language:    getContentLanguage(header),
			Location:    header.Get("Content-Location"),
		}
		return bs
	}
}

func getContentDisposition(header gomessage.Header) *imap.BodyStructureDisposition {
	disp, dispParams, _ := header.ContentDisposition()
	if disp == "" {
		return nil
	}
	return &imap.BodyStructureDisposition{
		Value:  disp,
		Params: dispParams,
	}
}

func getContentLanguage(header gomessage.Header) []string {
	v := header.Get("Content-Language")
	if v == "" {
		return nil
	}
	// TODO: handle CFWS
	l := strings.Split(v, ",")
	for i, lang := range l {
		l[i] = strings.TrimSpace(lang)
	}
	return l
}
