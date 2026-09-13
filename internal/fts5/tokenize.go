package fts5

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/fts"
)

// This file ports the fts5 tokenizers (ext/fts5/fts5_tokenize.c): unicode61
// (with the categories= option), ascii, porter (a wrapper around a base
// tokenizer, default unicode61) and trigram. Unlike FTS3/4, fts5 registers
// exactly these four — "simple" is not an fts5 tokenizer.

// Token is one tokenizer output: the folded term plus its byte span in the
// source text (fts5's xToken reports iStart/iEnd offsets; slice 5's
// highlight()/snippet() aux functions need them).
type Token struct {
	Term  string
	Start int
	End   int
}

// Tokenizer tokenizes text into tokens (fts5_tokenizer xTokenize).
type Tokenizer interface {
	Tokenize(text string) []Token
}

// NewTokenizer builds the tokenizer named by TokSpec[0] with the remaining
// words as constructor arguments (fts5_config.c sqlite3Fts5LoadTokenizer +
// fts5_tokenize.c xCreate entries). An unknown name fails with C's text.
func NewTokenizer(spec []string) (Tokenizer, error) {
	if len(spec) == 0 {
		return nil, fmt.Errorf("no such tokenizer: ")
	}
	name := strings.ToLower(spec[0])
	args := spec[1:]
	// Constructor arguments arrive as option/value pairs.
	switch name {
	case "unicode61":
		return newUnicode61(args)
	case "ascii":
		return newASCIITokenizer(args)
	case "porter":
		return newPorterTokenizer(args)
	case "trigram":
		return newTrigramTokenizer(args)
	}
	return nil, fmt.Errorf("no such tokenizer: %s", spec[0])
}

// tokenizerArgError is xCreate's failure text (fts5_tokenize.c: every
// constructor returns "error in tokenizer constructor" via
// sqlite3Fts5LoadTokenizer).
func tokenizerArgError() error { return fmt.Errorf("error in tokenizer constructor") }

// --- unicode61 ---

// unicode61Tokenizer ports fts5's unicode61 (fts5_tokenize.c
// unicode61Create/unicode61Next): a token is a run of characters whose Unicode
// category matches the configured set (default "L* N* Co"), with per-codepoint
// exceptions (tokenchars=/separators=); diacritics continue a token; each
// character is case-folded and optionally diacritic-stripped.
type unicode61Tokenizer struct {
	eRemoveDiacritic int
	categories       []*unicode.RangeTable
	exceptions       map[rune]bool // inverted is-token-char (tokenchars/separators)
}

// newUnicode61 builds a unicode61 tokenizer from option/value pairs
// (unicode61Create). Any unrecognized option or value fails with C's
// constructor error.
func newUnicode61(args []string) (Tokenizer, error) {
	t := &unicode61Tokenizer{
		eRemoveDiacritic: 1, // FTS5_REMOVE_DIACRITICS_SIMPLE
		exceptions:       make(map[rune]bool),
		categories:       []*unicode.RangeTable{unicode.L, unicode.N, unicode.Co},
	}
	cats := ""
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return nil, tokenizerArgError()
		}
		switch strings.ToLower(args[i]) {
		case "categories":
			cats = args[i+1]
		case "remove_diacritics":
			switch args[i+1] {
			case "0", "1", "2":
				t.eRemoveDiacritic = int(args[i+1][0] - '0')
			default:
				return nil, tokenizerArgError()
			}
		case "tokenchars":
			if err := t.addExceptions(args[i+1], true); err != nil {
				return nil, err
			}
		case "separators":
			if err := t.addExceptions(args[i+1], false); err != nil {
				return nil, err
			}
		default:
			return nil, tokenizerArgError()
		}
	}
	if cats != "" {
		tables, err := parseCategories(cats)
		if err != nil {
			return nil, err
		}
		t.categories = tables
	}
	return t, nil
}

// parseCategories parses a category specification like "L* N* Co"
// (unicodeSetCategories): whitespace-separated two-letter codes, a trailing
// '*' meaning the whole one-letter family. Unknown codes fail.
func parseCategories(spec string) ([]*unicode.RangeTable, error) {
	var tables []*unicode.RangeTable
	for _, word := range strings.Fields(spec) {
		if len(word) < 1 || len(word) > 2 {
			return nil, tokenizerArgError()
		}
		if len(word) == 2 && word[1] == '*' {
			word = word[:1]
		}
		tbl, ok := unicode.Categories[word]
		if !ok {
			return nil, tokenizerArgError()
		}
		tables = append(tables, tbl)
	}
	if len(tables) == 0 {
		return nil, tokenizerArgError()
	}
	return tables, nil
}

// addExceptions inverts the token-char decision for each codepoint of s
// (fts5UnicodeAddExceptions). Diacritical marks cannot be exceptions.
func (t *unicode61Tokenizer) addExceptions(s string, bAlnum bool) error {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if !fts.Unicode61IsDiacritic(int(r)) && t.isCategory(r) != bAlnum {
			t.exceptions[r] = true
		}
		i += size
	}
	return nil
}

// isCategory reports whether r matches the configured category set
// (fts5UnicodeIsAlnum's category half).
func (t *unicode61Tokenizer) isCategory(r rune) bool {
	for _, tbl := range t.categories {
		if unicode.Is(tbl, r) {
			return true
		}
	}
	return false
}

// isAlnum reports whether r is a token character (fts5UnicodeIsAlnum):
// category match XOR exception.
func (t *unicode61Tokenizer) isAlnum(r rune) bool {
	alarm := t.isCategory(r)
	if t.exceptions[r] {
		return !alarm
	}
	return alarm
}

// fold folds one codepoint (unicode61Fold via the shared fts3 tables).
func (t *unicode61Tokenizer) fold(r rune) rune {
	out := fts.Unicode61Fold(int(r), t.eRemoveDiacritic)
	if out == 0 {
		return 0
	}
	return rune(out)
}

// decodeRune decodes one rune, treating an invalid byte as the raw byte value
// (SQLite's READ_UTF8 behavior for malformed sequences).
func decodeRune(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return rune(s[0]), 1
	}
	return r, size
}

// Tokenize splits text into tokens (unicode61Next): skip separators, consume
// category chars (plus diacritics), folding each.
func (t *unicode61Tokenizer) Tokenize(text string) []Token {
	var tokens []Token
	i, n := 0, len(text)
	for i < n {
		for i < n {
			r, size := decodeRune(text[i:])
			if t.isAlnum(r) {
				break
			}
			i += size
		}
		if i >= n {
			break
		}
		start := i
		var sb strings.Builder
		for i < n {
			r, size := decodeRune(text[i:])
			cc := int(r)
			if t.isAlnum(r) || fts.Unicode61IsDiacritic(cc) {
				if out := t.fold(r); out != 0 {
					sb.WriteRune(out)
				}
				i += size
			} else {
				break
			}
		}
		tokens = append(tokens, Token{Term: sb.String(), Start: start, End: i})
	}
	return tokens
}

// --- ascii ---

// asciiTokenizer ports fts5's ascii tokenizer (fts5_tokenize.c
// fts5AsciiTokenize): a token is a run of ASCII alphanumeric or underscore
// bytes; A-Z are lowercased, all other bytes are separators.
type asciiTokenizer struct{}

func newASCIITokenizer(args []string) (Tokenizer, error) {
	if len(args) != 0 {
		return nil, tokenizerArgError()
	}
	return asciiTokenizer{}, nil
}

func (asciiTokenizer) isTok(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// Tokenize splits on non-ASCII-token bytes, lowercasing A-Z only
// (fts5AsciiNext? the C tokenizer writes lowercase for A-Z and copies other
// bytes verbatim).
func (t asciiTokenizer) Tokenize(text string) []Token {
	var tokens []Token
	i, n := 0, len(text)
	for i < n {
		for i < n && !t.isTok(text[i]) {
			i++
		}
		if i >= n {
			break
		}
		start := i
		var sb strings.Builder
		for i < n && t.isTok(text[i]) {
			b := text[i]
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			sb.WriteByte(b)
			i++
		}
		tokens = append(tokens, Token{Term: sb.String(), Start: start, End: i})
	}
	return tokens
}

// --- porter ---

// porterTokenizer wraps a base tokenizer and Porter-stems every token
// (fts5_porter.c fts5PorterCreate: the base defaults to unicode61).
type porterTokenizer struct {
	base Tokenizer
}

func newPorterTokenizer(args []string) (Tokenizer, error) {
	if len(args) == 0 {
		base, err := newUnicode61(nil)
		if err != nil {
			return nil, err
		}
		return porterTokenizer{base: base}, nil
	}
	if len(args) != 1 {
		return nil, tokenizerArgError()
	}
	base, err := NewTokenizer([]string{args[0]})
	if err != nil {
		return nil, err
	}
	return porterTokenizer{base: base}, nil
}

// Tokenize stems each base token (fts5PorterTokenize).
func (t porterTokenizer) Tokenize(text string) []Token {
	tokens := t.base.Tokenize(text)
	for i := range tokens {
		tokens[i].Term = fts.PorterStem(tokens[i].Term)
	}
	return tokens
}

// --- trigram ---

// trigramTokenizer ports fts5's trigram tokenizer (fts5_trigram.c
// fts5TriCreate/fts5TriTokenize): every three-character substring of the text
// becomes a token, so MATCH/LIKE/GLOB can locate arbitrary substrings. By
// default text is case-folded; case_sensitive 1 keeps original case, and
// remove_diacritics folds diacritics (mutually exclusive with
// case_sensitive 1).
type trigramTokenizer struct {
	bFold      bool
	iFoldParam int // 0 none, 2 complex diacritic folding
}

func newTrigramTokenizer(args []string) (Tokenizer, error) {
	t := &trigramTokenizer{bFold: true}
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return nil, tokenizerArgError()
		}
		switch strings.ToLower(args[i]) {
		case "case_sensitive":
			switch args[i+1] {
			case "0", "1":
				t.bFold = args[i+1] == "0"
			default:
				return nil, tokenizerArgError()
			}
		case "remove_diacritics":
			switch args[i+1] {
			case "0":
				t.iFoldParam = 0
			case "1", "2":
				t.iFoldParam = 2
			default:
				return nil, tokenizerArgError()
			}
		default:
			return nil, tokenizerArgError()
		}
	}
	if t.iFoldParam != 0 && !t.bFold {
		return nil, tokenizerArgError()
	}
	return t, nil
}

// foldText case-folds (and optionally diacritic-folds) the whole input
// (fts5TriTokenize's zFold buffer).
func (t *trigramTokenizer) foldText(text string) string {
	if !t.bFold && t.iFoldParam == 0 {
		return text
	}
	var sb strings.Builder
	for i := 0; i < len(text); {
		r, size := decodeRune(text[i:])
		if t.bFold {
			r = unicode.ToLower(r)
		}
		if t.iFoldParam != 0 {
			if out := fts.Unicode61Fold(int(r), t.iFoldParam); out != 0 {
				r = rune(out)
			}
		}
		sb.WriteRune(r)
		i += size
	}
	return sb.String()
}

// Tokenize emits one token per 3-rune window (fts5TriTokenize). The token
// spans are byte offsets into the ORIGINAL text (C reports offsets into the
// folded buffer, which coincide only for ASCII; the engine's snippets compare
// against the original column text).
func (t *trigramTokenizer) Tokenize(text string) []Token {
	folded := t.foldText(text)
	var tokens []Token
	// Map folded rune index -> original byte offsets so spans stay usable.
	origStart := make([]int, 0, utf8.RuneCountInString(folded)+1)
	origEnd := make([]int, 0, utf8.RuneCountInString(folded)+1)
	fi, oi := 0, 0
	for fi < len(folded) {
		_, fsize := decodeRune(folded[fi:])
		_, osize := decodeRune(text[oi:])
		origStart = append(origStart, oi)
		oi += osize
		origEnd = append(origEnd, oi)
		fi += fsize
	}
	nRunes := len(origStart)
	for i := 0; i+3 <= nRunes; i++ {
		tokens = append(tokens, Token{
			Term:  runeSlice(folded, i, i+3),
			Start: origStart[i],
			End:   origEnd[i+2],
		})
	}
	return tokens
}

// runeSlice returns the substring spanned by runes [a, b) of s.
func runeSlice(s string, a, b int) string {
	ra := 0
	i := 0
	for i < len(s) && ra < a {
		_, size := decodeRune(s[i:])
		i += size
		ra++
	}
	start := i
	for i < len(s) && ra < b {
		_, size := decodeRune(s[i:])
		i += size
		ra++
	}
	return s[start:i]
}
