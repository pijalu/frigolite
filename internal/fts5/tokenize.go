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
// '*' meaning the whole one-letter family. Unknown codes fail. A
// whitespace-only specification parses zero words and leaves the category
// set unchanged (nil return).
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
// category match XOR exception. Codepoint 0x00 is always a separator
// (fts5_unicode2.c sqlite3Fts5UnicodeAscii: "0x00 is never a token
// character"), even when Cc joins the category set.
func (t *unicode61Tokenizer) isAlnum(r rune) bool {
	if r == 0 {
		return false
	}
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
// bytes; A-Z are lowercased, all other bytes are separators. The
// tokenchars=/separators= options invert the token-byte decision per byte
// (fts5AsciiAddExceptions).
type asciiTokenizer struct {
	// exceptions inverts the default token-byte decision per ASCII byte.
	exceptions map[byte]bool
}

func newASCIITokenizer(args []string) (Tokenizer, error) {
	t := &asciiTokenizer{}
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return nil, tokenizerArgError()
		}
		bTokenChars := false
		switch strings.ToLower(args[i]) {
		case "tokenchars":
			bTokenChars = true
		case "separators":
		default:
			return nil, tokenizerArgError()
		}
		if t.exceptions == nil {
			t.exceptions = make(map[byte]bool)
		}
		for j := 0; j < len(args[i+1]); j++ {
			b := args[i+1][j]
			if b < 128 {
				t.exceptions[b] = bTokenChars
			}
		}
	}
	return t, nil
}

// isTok reports whether b is a token byte, honoring the per-byte exceptions
// (fts5AsciiAddExceptions's aTokenChar inversions).
func (t *asciiTokenizer) isTok(b byte) bool {
	if t.exceptions != nil {
		if invert, set := t.exceptions[b]; set {
			return invert
		}
	}
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
	// fts5_porter.c fts5PorterCreate: the first argument names the base
	// tokenizer (default unicode61) and the REMAINING arguments are passed
	// to the base tokenizer's constructor ("porter unicode61
	// remove_diacritics 1").
	base, err := NewTokenizer(args)
	if err != nil {
		return nil, err
	}
	return porterTokenizer{base: base}, nil
}

// Tokenize stems each base token (fts5PorterTokenize) with the fts5 porter
// variant (internal porterStem — it deviates from the classic FTS3/4
// algorithm; see porter.go).
func (t porterTokenizer) Tokenize(text string) []Token {
	tokens := t.base.Tokenize(text)
	for i := range tokens {
		tokens[i].Term = porterStem(tokens[i].Term)
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

// foldChar folds one codepoint the way fts5TriTokenize does: case-fold when
// bFold, then diacritic-fold; a folded result of 0 means the character is
// dropped entirely (removed diacritic).
func (t *trigramTokenizer) foldChar(r rune) rune {
	iCode := r
	if t.bFold {
		iCode = unicode.ToLower(iCode)
	}
	if t.iFoldParam != 0 {
		iCode = rune(fts.Unicode61Fold(int(iCode), t.iFoldParam))
	}
	return iCode
}

// nextFolded reads the next retained character starting at input offset zIn,
// folding and skipping characters that fold to nothing (removed diacritics).
// It returns the folded codepoint (0 at end of input), the new input offset,
// and — via *off — the original offset recorded just before the final read
// (fts5TriTokenize's iNext / aStart semantics).
func (t *trigramTokenizer) nextFolded(text string, zIn int, off *int) (rune, int) {
	for {
		*off = zIn
		if zIn >= len(text) {
			return 0, zIn
		}
		r, size := decodeRune(text[zIn:])
		zIn += size
		iCode := t.foldChar(r)
		if iCode != 0 {
			return iCode, zIn
		}
	}
}

// Tokenize ports fts5TriTokenize: a sliding window of three characters over
// the input. Characters that fold to nothing (removed diacritics) are
// skipped — they never enter a trigram — but the reported token span runs in
// ORIGINAL text bytes from the window's first character to the start of the
// character following the window (or EOF), so a diacritic trailing the
// window's last character IS inside the span (fts5trigram2 3.2:
// '\u0303(abc\u0303)' for text '\u0303abc\u0303').
func (t *trigramTokenizer) Tokenize(text string) []Token {
	var tokens []Token
	var aBuf []byte   // folded characters of the current trigram window
	var aStart [3]int // original byte offset of each window character
	zIn := 0

	// Populate aBuf with the characters for the first trigram.
	for ii := 0; ii < 3; ii++ {
		var off int
		iCode, nz := t.nextFolded(text, zIn, &off)
		zIn = nz
		if iCode == 0 {
			return tokens
		}
		aStart[ii] = off
		aBuf = appendRune(aBuf, iCode)
	}

	for {
		// Read characters up to the next retained one, then pass the
		// current trigram back to fts5.
		var off int
		iCode, nz := t.nextFolded(text, zIn, &off)
		tokens = append(tokens, Token{Term: string(aBuf), Start: aStart[0], End: off})
		if iCode == 0 {
			return tokens
		}
		zIn = nz

		// Remove the first character from aBuf, append iCode, and slide
		// the aStart window.
		_, sz := decodeRune(string(aBuf))
		aBuf = append(aBuf[:0], aBuf[sz:]...)
		aBuf = appendRune(aBuf, iCode)
		aStart[0], aStart[1], aStart[2] = aStart[1], aStart[2], off
	}
}

// appendRune appends r to b as UTF-8.
func appendRune(b []byte, r rune) []byte {
	return utf8.AppendRune(b, r)
}
