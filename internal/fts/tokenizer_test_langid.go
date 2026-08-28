package fts

import (
	"fmt"
	"sync"
)

// LangidAware is implemented by tokenizers whose token output depends on the
// FTS4 languageid=<col> value (fts3.c passes iLangid to xLanguageid before
// each tokenization: sqlite3Fts3OpenTokenizer). WithLangid returns a
// tokenizer bound to the given language id; the receiver is not modified.
type LangidAware interface {
	WithLangid(langid int64) Tokenizer
}

// LangidValidator is implemented by language-aware tokenizers that reject
// certain language ids at write time (the fts3_test.c test tokenizer fails
// xLanguageid with SQLITE_ERROR for langid >= 100, surfacing as "SQL logic
// error" — fts4langid 4.1.5).
type LangidValidator interface {
	ValidateLangid(langid int64) error
}

// TestTokenizer mirrors SQLite's ext/fts3/fts3_test.c "test" tokenizer used
// by fts4langid section 4 (registered via fts3_tokenizer('testtokenizer',
// $ptr)): tokens are runs of ASCII letters; every byte is lowercased when the
// cursor's language id is EVEN and preserved verbatim when it is ODD.
type TestTokenizer struct {
	langid int64
}

// NewTestTokenizer creates a test tokenizer at language id 0.
func NewTestTokenizer() *TestTokenizer { return &TestTokenizer{} }

// WithLangid implements LangidAware.
func (t *TestTokenizer) WithLangid(langid int64) Tokenizer {
	return &TestTokenizer{langid: langid}
}

// ValidateLangid implements LangidValidator (fts3_test.c
// testTokenizerLanguage: langid >= 100 is SQLITE_ERROR).
func (t *TestTokenizer) ValidateLangid(langid int64) error {
	if langid >= 100 {
		return fmt.Errorf("SQL logic error")
	}
	return nil
}

// Tokenize implements Tokenizer.
func (t *TestTokenizer) Tokenize(text string) []Token {
	lower := t.langid%2 == 0
	var toks []Token
	start := -1
	var b []byte
	flush := func(end int) {
		if start >= 0 {
			toks = append(toks, Token{Term: string(b), Position: len(toks)})
			start = -1
			b = b[:0]
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !isAlpha {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		if lower && c >= 'A' && c <= 'Z' {
			c = c - 'A' + 'a'
		}
		b = append(b, c)
	}
	flush(len(text))
	return toks
}

// TokenizeOffsets implements Tokenizer.
func (t *TestTokenizer) TokenizeOffsets(text string) []OffsetToken {
	lower := t.langid%2 == 0
	var toks []OffsetToken
	start := -1
	var b []byte
	flush := func(end int) {
		if start >= 0 {
			toks = append(toks, OffsetToken{Term: string(b), Start: start, End: end})
			start = -1
			b = b[:0]
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !isAlpha {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		if lower && c >= 'A' && c <= 'Z' {
			c = c - 'A' + 'a'
		}
		b = append(b, c)
	}
	flush(len(text))
	return toks
}

// customTokenizers registry: names registered through the
// fts3_tokenizer(name, module) SQL function (fts3_tokenizer.c
// sqlite3Fts3InitTokenizer stores the module under its name). A pure-Go
// engine cannot hold C pointers; a registered name maps to a factory that
// must produce a language-aware-capable tokenizer. Only the fts3_test.c
// "test" tokenizer semantics are provided (fts4langid 4.x).
var (
	customTokMu      sync.Mutex
	customTokenizers = map[string]func() Tokenizer{}
)

// RegisterCustomTokenizer stores a tokenizer factory under name (the
// fts3_tokenizer(name, ptr) registration form).
func RegisterCustomTokenizer(name string, factory func() Tokenizer) {
	customTokMu.Lock()
	defer customTokMu.Unlock()
	customTokenizers[name] = factory
}

// LookupCustomTokenizer resolves a registered custom tokenizer factory.
func LookupCustomTokenizer(name string) (func() Tokenizer, bool) {
	customTokMu.Lock()
	defer customTokMu.Unlock()
	f, ok := customTokenizers[name]
	return f, ok
}

// UnregisterCustomTokenizer removes a registered tokenizer factory (the
// fts3_tokenizer(name, NULL) deletion form).
func UnregisterCustomTokenizer(name string) {
	customTokMu.Lock()
	defer customTokMu.Unlock()
	delete(customTokenizers, name)
}

// HasTokenizer reports whether name names any known tokenizer (built-in or
// registered): the one-argument fts3_tokenizer(name) form returns non-NULL
// for known names and errors otherwise (fts4noti/fts4langid usage).
func HasTokenizer(name string) bool {
	switch name {
	case "simple", "unicode61", "porter":
		return true
	}
	_, ok := LookupCustomTokenizer(name)
	return ok
}
