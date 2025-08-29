package fts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/registry"
)

func mustIndex(t *testing.T, body string) (*Indexer, string) {
	t.Helper()
	dir := t.TempDir()
	ix, err := Open(filepath.Join(dir, "fts"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	msg := "From: 张三 <zhang@example.com>\r\nTo: admin@example.com\r\nSubject: 会议纪要\r\n" +
		"Date: Sat, 29 Aug 2026 10:00:00 +0800\r\n\r\n" + body
	if err := ix.IndexMessage(context.Background(), "admin@example.com", "INBOX", 7, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	return ix, dir
}

// TestSearchChineseBigram: a two-character query matches its bigram exactly,
// and a bigram that never occurs adjacently (票据) is not matched even though
// both characters are present separately in the text.
func TestSearchChineseBigram(t *testing.T) {
	ix, _ := mustIndex(t, "会议发票已开出，请查收附件。Quarterly report attached.\r\n")
	for _, q := range []string{"发票", "会议", "开出", "查收"} {
		got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{q})
		if err != nil {
			t.Fatalf("search %s: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("search %s: got %d hits, want 1", q, len(got))
		}
	}
	// 票 and 据 both appear (票据 never adjacent — 据 does not appear at all),
	// so the bigram must miss.
	got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"票据"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("non-adjacent bigram matched: %v", got)
	}
}

// TestSearchChineseSingleCharPrefix: a one-character query falls back to a
// prefix over the indexed bigrams (发* matches 发票).
func TestSearchChineseSingleCharPrefix(t *testing.T) {
	ix, _ := mustIndex(t, "会议发票已开出\r\n")
	got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"票"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("single-char prefix: got %d hits, want 1", len(got))
	}
	got, err = ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"钱"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("absent char matched: %v", got)
	}
}

// TestSearchLatinPrefixUnchanged: latin word/prefix semantics must survive
// the analyzer switch (quart* still matches quarterly).
func TestSearchLatinPrefixUnchanged(t *testing.T) {
	ix, _ := mustIndex(t, "Quarterly report attached.\r\n")
	got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"quart"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("latin prefix: got %d hits, want 1", len(got))
	}
	got, err = ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"quarterly", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("AND across terms broken: %v", got)
	}
}

// TestSearchChinesePhraseAND: multi-character Chinese query requires every
// bigram of the query to be present.
func TestSearchChinesePhraseAND(t *testing.T) {
	ix, _ := mustIndex(t, "会议发票已开出\r\n")
	got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"发票已开"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("phrase AND: got %d hits, want 1", len(got))
	}
}

// TestOpenRecreatesStaleAnalyzerIndex: an index built with the old standard
// analyzer is detected and rebuilt with mailez_cjk instead of silently
// answering with incompatible tokens.
func TestOpenRecreatesStaleAnalyzerIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fts")
	old := bleve.NewIndexMapping()
	old.DefaultAnalyzer = "standard"
	idx, err := bleve.New(path, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Index("admin@example.com\x1fINBOX\x1f1", map[string]string{
		"id":      "x",
		"account": "admin@example.com",
		"mailbox": "INBOX",
		"text":    "会议发票已开出",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	ix, err := Open(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	// The recreated index starts empty: the stale doc is gone and the new
	// analyzer is in charge.
	got, err := ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"发票"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale doc survived recreate: %v", got)
	}
	if err := ix.IndexMessage(context.Background(), "admin@example.com", "INBOX", 2,
		[]byte("From: a@b.c\r\nSubject: s\r\n\r\n会议发票已开出\r\n")); err != nil {
		t.Fatal(err)
	}
	got, err = ix.SearchText(context.Background(), "admin@example.com", "INBOX", []string{"发票"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recreated index cannot search CJK: %v", got)
	}
}

// TestAnalyzerBigramTokens pins the analyzer output shape: a contiguous CJK
// run becomes overlapping bigrams, latin words pass through lowercased, and
// non-CJK tokens break bigram formation (no cross-language bigrams).
func TestAnalyzerBigramTokens(t *testing.T) {
	an, err := registry.NewCache().AnalyzerNamed(CJKAnalyzerName)
	if err != nil {
		t.Fatal(err)
	}
	toks := an.Analyze([]byte("会议发票Invoice"))
	var got []string
	for _, tk := range toks {
		got = append(got, string(tk.Term))
	}
	// 会议发票 is one Han run → overlapping bigrams plus a run-final
	// unigram (票), then the latin word.
	want := []string{"会议", "议发", "发票", "票", "invoice"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	toks = an.Analyze([]byte("会Invoice议"))
	got = got[:0]
	for _, tk := range toks {
		got = append(got, string(tk.Term))
	}
	// Separated single CJK characters stay unigrams; no 会议 bigram forms
	// across the latin word.
	want = []string{"会", "invoice", "议"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("separated tokens = %v, want %v", got, want)
	}
}

// TestOpenFreshDirectoryStillWorks guards the recreate path against eating
// healthy indexes: same version → no wipe.
func TestOpenFreshDirectoryStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fts")
	ix1, err := Open(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix1.IndexMessage(context.Background(), "a@b.c", "INBOX", 1,
		[]byte("From: a@b.c\r\nSubject: s\r\n\r\n发票内容\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := ix1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("index dir vanished: %v", err)
	}
	ix2, err := Open(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix2.Close() })
	got, err := ix2.SearchText(context.Background(), "a@b.c", "INBOX", []string{"发票"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("healthy index was wiped on reopen: %v", got)
	}
}
