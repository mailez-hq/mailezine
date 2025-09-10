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
	"os"
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
	if err == nil && staleMapping(idx) {
		// An index built with an older analyzer (standard, no CJK bigrams)
		// cannot answer the new queries; recreate it from scratch. Delivery
		// re-indexes incrementally, and `mailezine reindex` backfills the
		// history in one pass.
		logger.Info("fts: analyzer changed; recreating index")
		_ = idx.Close()
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return nil, rmErr
		}
		idx = nil
	}
	if idx == nil || err != nil {
		// Recreate a corrupt (or analyzer-stale) index rather than fail
		// startup.
		idx, err = bleve.New(path, buildMapping())
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

// staleMapping reports whether the opened index was built with a different
// default analyzer than the current code expects.
func staleMapping(idx bleve.Index) bool {
	im, ok := idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok {
		return false
	}
	return im.DefaultAnalyzer != CJKAnalyzerName
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
	tf.Analyzer = CJKAnalyzerName
	dm.AddFieldMappingsAt("text", tf)
	m.DefaultAnalyzer = CJKAnalyzerName
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
	var attachments []attachmentPart
	walkParts(msg, &out, &attachments)
	for _, att := range attachments {
		// Built-in extraction first (政企内网零依赖); Tika covers the long
		// tail when configured.
		if text := extractAttachmentText(att.Name, att.ContentType, att.Data); text != "" {
			out.WriteString("\n")
			out.WriteString(text)
		} else if ix.tikaURL != "" {
			if text, err := ix.tikaExtract(ctx, att.Data); err == nil && text != "" {
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

// attachmentPart is one non-text leaf with its MIME metadata.
type attachmentPart struct {
	Name        string
	ContentType string
	Data        []byte
}

// walkParts collects visible text into out and non-text attachment bodies
// into attachments (bounded per part).
func walkParts(msg *message.Entity, out *strings.Builder, attachments *[]attachmentPart) {
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
			name := ""
			if _, params, derr := msg.Header.ContentDisposition(); derr == nil && params != nil {
				name = params["filename"]
			}
			if name == "" {
				if _, params, cerr := msg.Header.ContentType(); cerr == nil && params != nil {
					name = params["name"]
				}
			}
			*attachments = append(*attachments, attachmentPart{
				Name:        name,
				ContentType: mt,
				Data:        body,
			})
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
		if containsCJK(t) && !singleCJKRune(t) {
			// CJK input: segment with the query-side twin of the index
			// analyzer (bigrams only) and require every token, mirroring
			// IMAP SEARCH AND semantics. Single characters take the prefix
			// branch below.
			for _, tok := range analyzeCJKQuery(t) {
				tq := bleve.NewTermQuery(tok)
				tq.SetField("text")
				qs = append(qs, tq)
			}
			continue
		}
		// Latin words keep word/prefix semantics; a single CJK character
		// prefixes its bigrams (发* matches 发票).
		//
		// The term has to be split into index tokens by hand: the unicode
		// tokenizer the analyzer composes cuts on every non-alphanumeric rune,
		// and a PrefixQuery built from a term that itself analyzes into
		// several tokens resolves to no postings at all (silently matching
		// nothing — "QA-attach" never found "QA-attach-57880"). One prefix
		// query per token keeps the index a superset of the raw-byte check
		// the IMAP layer runs afterwards, so a word still matches on a prefix
		// (attach → attachment) without dropping punctuation-bearing queries.
		tokens := analyzeCJKQuery(t)
		if len(tokens) == 0 {
			// Punctuation-only term: the index holds no token to narrow it
			// down, so report "unavailable" and let the caller exact-scan
			// instead of claiming the mailbox has no match.
			return nil, nil
		}
		for _, tok := range tokens {
			pq := bleve.NewPrefixQuery(tok)
			pq.SetField("text")
			qs = append(qs, pq)
		}
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
