// CJK analysis: the bleve v2 core ships no CJK analyzer, and its standard
// analyzer emits each Han character as its own token, so a two-character
// Chinese query (发票) can never match — term 发票 does not exist in the
// index. The mailez_cjk analyzer composes the unicode tokenizer with a
// bigram filter: consecutive CJK characters become overlapping bigrams
// (会议 → 会议; 会议发票 → 会议/议发/发票), non-CJK text keeps word
// semantics (lowercased). Query and index sides share it, so searching 发票
// matches the 发票 bigram exactly — mainstream-class Chinese search without a
// dictionary dependency for 政企内网 deployments.
package fts

import (
	"unicode"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	bleveunicode "github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/registry"
)

// CJKAnalyzerName is the registered analyzer used for all indexed text and
// CJK query terms.
const CJKAnalyzerName = "mailez_cjk"

func init() {
	registry.RegisterAnalyzer(CJKAnalyzerName, cjkAnalyzerConstructor)
}

func cjkAnalyzerConstructor(_ map[string]any, _ *registry.Cache) (analysis.Analyzer, error) {
	return &analysis.DefaultAnalyzer{
		Tokenizer: bleveunicode.NewUnicodeTokenizer(),
		TokenFilters: []analysis.TokenFilter{
			lowercase.NewLowerCaseFilter(),
			cjkBigramFilter{indexMode: true},
		},
	}, nil
}

// cjkBigramFilter rewrites the token stream: runs of consecutive CJK
// characters (arriving as one token each from the unicode tokenizer) become
// overlapping bigrams — a run of n characters yields n-1 bigrams. In index
// mode the run's final character is also emitted as a unigram so a
// single-character query for it can prefix-match (no bigram starts with
// it); in query mode it is skipped, because the conjunction requires every
// query token and a spurious unigram would over-restrict results. A lone
// character always passes through as a unigram. Non-CJK tokens are
// untouched.
type cjkBigramFilter struct {
	indexMode bool
}

func (f cjkBigramFilter) Filter(in analysis.TokenStream) analysis.TokenStream {
	atoms := atomize(in)
	var out analysis.TokenStream
	i := 0
	for i < len(atoms) {
		if !atoms[i].cjk {
			out = append(out, atoms[i].token(len(out)+1))
			i++
			continue
		}
		j := i
		for j < len(atoms) && atoms[j].cjk {
			j++
		}
		run := atoms[i:j]
		for k := 0; k+1 < len(run); k++ {
			out = append(out, run[k].bigram(run[k+1], len(out)+1))
		}
		if len(run) == 1 {
			out = append(out, run[0].token(len(out)+1))
		} else if f.indexMode {
			out = append(out, run[len(run)-1].token(len(out)+1))
		}
		i = j
	}
	return out
}

// queryAnalyzer is the query-side twin of the registered index analyzer:
// bigrams only (no run-final unigrams), so one analyzed query maps exactly
// onto the bigrams a document run produces.
var queryAnalyzer = &analysis.DefaultAnalyzer{
	Tokenizer: bleveunicode.NewUnicodeTokenizer(),
	TokenFilters: []analysis.TokenFilter{
		lowercase.NewLowerCaseFilter(),
		cjkBigramFilter{indexMode: false},
	},
}

// analyzeCJKQuery segments a CJK query term into the tokens the index side
// would have produced (bigrams plus lowercased latin words).
func analyzeCJKQuery(term string) []string {
	var out []string
	for _, tk := range queryAnalyzer.Analyze([]byte(term)) {
		out = append(out, string(tk.Term))
	}
	return out
}

// atom is one indivisible output candidate: a single CJK character or a
// non-CJK token carried over from the input stream.
type atom struct {
	term  string // full term text (single CJK rune, or a non-CJK token)
	cjk   bool
	start int // byte offsets in the original field
	end   int
}

func (a atom) token(pos int) *analysis.Token {
	return &analysis.Token{
		Term:     []byte(a.term),
		Position: pos,
		Start:    a.start,
		End:      a.end,
	}
}

func (a atom) bigram(next atom, pos int) *analysis.Token {
	return &analysis.Token{
		Term:     []byte(a.term + next.term),
		Position: pos,
		Start:    a.start,
		End:      next.end,
	}
}

// atomize splits every input token into CJK single-character atoms and
// non-CJK segments. The unicode tokenizer already emits Han characters one
// per token; splitting defensively also handles tokenizers that keep whole
// CJK runs together.
func atomize(in analysis.TokenStream) []atom {
	var out []atom
	for _, t := range in {
		term := t.Term
		var runes []rune
		var offs []int
		for i, w := 0, 0; i < len(term); i += w {
			r, width := utf8.DecodeRune(term[i:])
			w = width
			runes = append(runes, r)
			offs = append(offs, i)
		}
		i := 0
		for i < len(runes) {
			if isCJK(runes[i]) {
				out = append(out, atom{
					term:  string(runes[i]),
					cjk:   true,
					start: t.Start + offs[i],
					end:   t.Start + offs[i] + utf8.RuneLen(runes[i]),
				})
				i++
				continue
			}
			j := i
			for j < len(runes) && !isCJK(runes[j]) {
				j++
			}
			out = append(out, atom{
				term:  string(runes[i:j]),
				cjk:   false,
				start: t.Start + offs[i],
				end:   t.Start + offs[j-1] + utf8.RuneLen(runes[j-1]),
			})
			i = j
		}
	}
	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// containsCJK reports whether s has any CJK rune (Han, kana, hangul).
func containsCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// singleCJKRune reports whether the term is exactly one CJK character; such
// queries fall back to prefix matching over the indexed bigrams.
func singleCJKRune(s string) bool {
	runes := []rune(s)
	return len(runes) == 1 && isCJK(runes[0])
}
