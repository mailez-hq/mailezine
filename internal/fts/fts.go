// Package fts provides full-text search indexing (bleve, embedded) for
// message bodies and headers. Semantics are word/prefix based, not
// substring: IMAP SEARCH TEXT first consults the index for candidate UIDs,
// then verifies against the raw bytes, so results stay exact for the
// indexed terms. Index failures degrade to a full scan, never to mail loss.
package fts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/emersion/go-message"
)

// doc is the bleve document for one message copy.
type doc struct {
	ID      string `json:"id"`
	Account string `json:"account"`
	Mailbox string `json:"mailbox"`
	Text    string `json:"text"`
}

// Indexer owns one bleve index per data directory.
type Indexer struct {
	idx     bleve.Index
	logger  *slog.Logger
	tikaURL string
	hc      *http.Client
}

// Open opens (or creates) the index at path. A corrupt/unusable index is
// recreated on open; callers treat an error as "index unavailable".
// tikaURL optionally enables attachment text extraction (Apache Tika
// /tika endpoint); extraction failures degrade to indexing without
// attachment text.
func Open(path, tikaURL string, logger *slog.Logger) (*Indexer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	idx, err := bleve.Open(path)
	if err != nil {
		// Recreate a corrupt index rather than fail startup.
		m := buildMapping()
		idx, err = bleve.New(path, m)
		if err != nil {
			return nil, err
		}
	}
	return &Indexer{
		idx:     idx,
		logger:  logger,
		tikaURL: tikaURL,
		hc:      &http.Client{Timeout: 5 * time.Second},
	}, nil
}

// Close closes the index.
func (ix *Indexer) Close() error {
	if ix == nil || ix.idx == nil {
		return nil
	}
	return ix.idx.Close()
}

func buildMapping() mapping.IndexMapping {
	m := bleve.NewIndexMapping()
	dm := m.DefaultMapping
	mb := bleve.NewTextFieldMapping()
	mb.Analyzer = "keyword"
	dm.AddFieldMappingsAt("account", mb)
	dm.AddFieldMappingsAt("mailbox", mb)
	tf := bleve.NewTextFieldMapping()
	dm.AddFieldMappingsAt("text", tf)
	m.DefaultAnalyzer = "standard"
	return m
}

// docID is the stable document key for one message copy.
func docID(account, mailbox string, uid uint32) string {
	return account + "\x1f" + mailbox + "\x1f" + fmt.Sprint(uid)
}

// IndexMessage adds (or replaces) one message copy in the index. text is
// the full raw message; headers and body are indexed together.
func (ix *Indexer) IndexMessage(ctx context.Context, account, mailbox string, uid uint32, data []byte) error {
	if ix == nil || ix.idx == nil {
		return nil
	}
	d := doc{
		ID:      docID(account, mailbox, uid),
		Account: account,
		Mailbox: mailbox,
		Text:    ix.searchableText(ctx, data),
	}
	return ix.idx.Index(d.ID, d)
}

// searchableText builds the indexed text: headers + text parts + optional
// Tika-extracted attachment text.
func (ix *Indexer) searchableText(ctx context.Context, data []byte) string {
	var out strings.Builder
	msg, err := message.Read(strings.NewReader(string(data)))
	if err != nil {
		return string(data)
	}
	for _, key := range []string{"From", "To", "Subject"} {
		if v := msg.Header.Get(key); v != "" {
			out.WriteString(key)
			out.WriteString(": ")
			out.WriteString(v)
			out.WriteByte('\n')
		}
	}
	var attachments [][]byte
	walkParts(msg, &out, &attachments)
	if ix.tikaURL != "" {
		for _, att := range attachments {
			if text, err := ix.tikaExtract(ctx, att); err == nil && text != "" {
				out.WriteString("\n")
				out.WriteString(text)
			}
		}
	}
	return out.String()
}

// tikaExtract sends one attachment to the Tika /tika endpoint.
func (ix *Indexer) tikaExtract(ctx context.Context, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ix.tikaURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := ix.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tika status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

var htmlTagRe = regexp.MustCompile(`(?s)<[^>]+>`)
var whitespaceRe = regexp.MustCompile(`\s+`)

// walkParts collects visible text into out and non-text attachment bodies
// into attachments (bounded per part).
func walkParts(msg *message.Entity, out *strings.Builder, attachments *[][]byte) {
	mt, _, _ := msg.Header.ContentType()
	if strings.HasPrefix(mt, "text/plain") || strings.HasPrefix(mt, "text/html") {
		body, err := io.ReadAll(io.LimitReader(msg.Body, 4<<20))
		if err == nil {
			text := string(body)
			if strings.HasPrefix(mt, "text/html") {
				text = htmlTagRe.ReplaceAllString(text, " ")
			}
			text = whitespaceRe.ReplaceAllString(text, " ")
			out.WriteString(text)
			out.WriteByte('\n')
		}
		return
	}
	mr := msg.MultipartReader()
	if mr != nil {
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			walkParts(p, out, attachments)
		}
		return
	}
	// Leaf, non-text part: candidate attachment for Tika.
	if mt != "" {
		body, err := io.ReadAll(io.LimitReader(msg.Body, 8<<20))
		if err == nil && len(body) > 0 {
			*attachments = append(*attachments, body)
		}
	}
}

// DeleteMessage removes one message copy from the index.
func (ix *Indexer) DeleteMessage(ctx context.Context, account, mailbox string, uid uint32) error {
	if ix == nil || ix.idx == nil {
		return nil
	}
	return ix.idx.Delete(docID(account, mailbox, uid))
}

// SearchText returns the UIDs in the mailbox whose indexed text matches any
// of the terms (word/prefix semantics, AND across terms per IMAP SEARCH).
// A nil result means the index is unavailable — the caller falls back to a
// full scan.
func (ix *Indexer) SearchText(ctx context.Context, account, mailbox string, terms []string) (map[uint32]struct{}, error) {
	if ix == nil || ix.idx == nil {
		return nil, fmt.Errorf("fts: index unavailable")
	}
	if len(terms) == 0 {
		return map[uint32]struct{}{}, nil
	}
	// Mailbox filter + AND of terms. Each term matches the word or a
	// prefix of a word.
	var qs []query.Query
	acq := bleve.NewTermQuery(account)
	acq.SetField("account")
	qs = append(qs, acq)
	mbq := bleve.NewTermQuery(mailboxName(account, mailbox))
	mbq.SetField("mailbox")
	qs = append(qs, mbq)
	terms = append([]string(nil), terms...)
	filtered := terms[:0]
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		filtered = append(filtered, t)
		pq := bleve.NewPrefixQuery(t)
		pq.SetField("text")
		qs = append(qs, pq)
	}
	if len(qs) == 1 {
		return map[uint32]struct{}{}, nil
	}
	q := bleve.NewConjunctionQuery(qs...)
	req := bleve.NewSearchRequest(q)
	req.Size = 100000
	res, err := ix.idx.Search(req)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]struct{}, len(res.Hits))
	for _, hit := range res.Hits {
		fields := strings.Split(hit.ID, "\x1f")
		if len(fields) != 3 {
			continue
		}
		var uid uint32
		if _, err := fmt.Sscanf(fields[2], "%d", &uid); err != nil {
			continue
		}
		out[uid] = struct{}{}
	}
	return out, nil
}

// mailboxName is the keyword field value.
func mailboxName(_, mailbox string) string {
	return mailbox
}
