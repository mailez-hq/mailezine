package fts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestIndex(t *testing.T) *Indexer {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "fts"), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func TestIndexAndSearch(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	msg := "From: alice@example.com\r\nSubject: weekly report\r\n\r\ninvoice attached for q3\n"
	if err := ix.IndexMessage(ctx, "alice@example.com", "INBOX", 1, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if n, err := ix.idx.DocCount(); err == nil {
		t.Logf("doccount after index = %d", n)
	} else {
		t.Logf("doccount err = %v", err)
	}
	res, serr := ix.SearchText(ctx, "alice@example.com", "INBOX", []string{"invoice"})
	t.Logf("direct search = %v err=%v", res, serr)
	if err := ix.IndexMessage(ctx, "alice@example.com", "INBOX", 2, []byte("Subject: other\r\n\r\nnothing here")); err != nil {
		t.Fatal(err)
	}
	if err := ix.IndexMessage(ctx, "alice@example.com", "Junk", 3, []byte("Subject: spam\r\n\r\ninvoice discount")); err != nil {
		t.Fatal(err)
	}

	// Word/prefix hit in INBOX.
	got := searchEventually(t, ix, "alice@example.com", "INBOX", []string{"invoice"}, 1)
	if _, ok := got[1]; !ok {
		t.Fatalf("uid 1 missing: %v", got)
	}

	// AND across terms.
	_ = searchEventually(t, ix, "alice@example.com", "INBOX", []string{"weekly", "report"}, 1)

	// Mailbox isolation: Junk copy is not found in INBOX.
	got, err := ix.SearchText(ctx, "alice@example.com", "INBOX", []string{"discount"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("junk leaked into inbox search: %v", got)
	}
	got = searchEventually(t, ix, "alice@example.com", "Junk", []string{"discount"}, 1)
	if !mapContains(got, 3) {
		t.Fatalf("junk search = %v", got)
	}
}

func TestDeleteAndMove(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	msg := "Subject: hello world\r\n\r\nneedle in haystack"
	if err := ix.IndexMessage(ctx, "alice@example.com", "INBOX", 7, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := ix.DeleteMessage(ctx, "alice@example.com", "INBOX", 7); err != nil {
		t.Fatal(err)
	}
	got, err := ix.SearchText(ctx, "alice@example.com", "INBOX", []string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted message still found: %v", got)
	}

	// Move: index the copy in the destination mailbox.
	if err := ix.IndexMessage(ctx, "alice@example.com", "Archive", 7, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	got = searchEventually(t, ix, "alice@example.com", "Archive", []string{"needle"}, 1)
	if !mapContains(got, 7) {
		t.Fatalf("moved message not found in Archive: %v", got)
	}
}

func TestPrefixSemantics(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	if err := ix.IndexMessage(ctx, "a@x.test", "INBOX", 1, []byte("Subject: quarterly\r\n\r\nq3 numbers")); err != nil {
		t.Fatal(err)
	}
	got := searchEventually(t, ix, "a@x.test", "INBOX", []string{"quar"}, 1)
	if !mapContains(got, 1) {
		t.Fatalf("prefix search missed: %v", got)
	}
}

// TestExtractHTMLAndMultipart: HTML markup and attachments must not pollute
// the index; the visible text is searchable.
func TestExtractHTMLAndMultipart(t *testing.T) {
	ix := newTestIndex(t)
	ctx := context.Background()
	html := "Content-Type: text/html\r\n\r\n<html><body><p>quarterly <b>revenue</b> report</p></body></html>"
	if err := ix.IndexMessage(ctx, "a@x.test", "INBOX", 1, []byte(html)); err != nil {
		t.Fatal(err)
	}
	got := searchEventually(t, ix, "a@x.test", "INBOX", []string{"revenue"}, 1)
	if !mapContains(got, 1) {
		t.Fatalf("html body text not indexed: %v", got)
	}

	// Multipart: text part indexed; base64 attachment content ignored.
	multi := "Content-Type: multipart/mixed; boundary=xx\r\n\r\n" +
		"--xx\r\nContent-Type: text/plain\r\n\r\nneedle in the haystack\r\n" +
		"--xx\r\nContent-Type: application/pdf; name=q.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQKJcOk\r\n" +
		"--xx--\r\n"
	if err := ix.IndexMessage(ctx, "a@x.test", "INBOX", 2, []byte(multi)); err != nil {
		t.Fatal(err)
	}
	got = searchEventually(t, ix, "a@x.test", "INBOX", []string{"needle"}, 1)
	if !mapContains(got, 2) {
		t.Fatalf("multipart text not indexed: %v", got)
	}
	// The base64 PDF payload must not create searchable tokens.
	got, err := ix.SearchText(ctx, "a@x.test", "INBOX", []string{"JVBERi0xLjQ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("attachment payload leaked into index: %v", got)
	}
}

// TestTikaAttachmentExtraction: with a Tika endpoint configured, attachment
// text is extracted and searchable; without one the same message indexes
// only its text part.
func TestTikaAttachmentExtraction(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		// "Extract" the attachment payload into searchable text.
		_, _ = w.Write([]byte("extracted " + string(body)))
	}))
	defer srv.Close()

	ix, err := Open(filepath.Join(t.TempDir(), "fts"), srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	ctx := context.Background()
	// A multipart message with a PDF-ish attachment carrying a unique
	// payload marker; Tika "extracts" it into the index.
	multi := "Content-Type: multipart/mixed; boundary=yy\r\n\r\n" +
		"--yy\r\nContent-Type: application/pdf; name=doc.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nbWFya2VyMTIz\r\n" +
		"--yy--\r\n"
	if err := ix.IndexMessage(ctx, "a@x.test", "INBOX", 1, []byte(multi)); err != nil {
		t.Fatal(err)
	}
	t.Logf("tika calls = %d", calls)
	got := searchEventually(t, ix, "a@x.test", "INBOX", []string{"marker123"}, 1)
	if !mapContains(got, 1) {
		t.Fatalf("tika attachment text not indexed: %v", got)
	}
}

func mapContains(m map[uint32]struct{}, uid uint32) bool {
	_, ok := m[uid]
	return ok
}

// searchEventually retries until the index batch is flushed (bleve commits
// asynchronously).
func searchEventually(t *testing.T, ix *Indexer, account, mailbox string, terms []string, want int) map[uint32]struct{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := ix.SearchText(context.Background(), account, mailbox, terms)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("search %v = %v, want %d hits", terms, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
