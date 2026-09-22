package fts

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token represents a single token from tokenization.
type Token struct {
	Term     string
	Position int
}

// OffsetToken is a token with its byte span in the source text (used by the
// offsets() and snippet() auxiliary functions).
type OffsetToken struct {
	Term  string
	Start int // byte offset of the token's first byte
	End   int // byte offset one past the token's last byte
}

// TokenizeOffsets tokenizes text, returning tokens with their byte offsets in
// the source (SQLite's tokenizer xNext reports byte offsets, fts3_snippet.c
// sqlite3Fts3Offsets).
func (t *SimpleTokenizer) TokenizeOffsets(text string) []OffsetToken {
	var tokens []OffsetToken
	start := -1
	for i := 0; i < len(text); {
		_, size := decodeRune(text[i:])
		isDelim := size == 1 && t.isDelim(text[i])
		if !isDelim {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 {
				tokens = append(tokens, OffsetToken{Term: asciiLowerBytes(text[start:i]), Start: start, End: i})
				start = -1
			}
		}
		i += size
	}
	if start >= 0 {
		tokens = append(tokens, OffsetToken{Term: asciiLowerBytes(text[start:]), Start: start, End: len(text)})
	}
	return tokens
}

// asciiLowerBytes lowercases only ASCII A-Z bytes, leaving all other bytes
// (including invalid UTF-8 and high bytes) intact (fts3_tokenizer1.c
// simpleNext: `ch>='A' && ch<='Z' ? ch-'A'+'a' : ch`). strings.ToLower would
// corrupt invalid UTF-8 (0xf1 → U+FFFD) — fts3snippet2.test 3.1.
func asciiLowerBytes(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// TokenizeOffsets tokenizes text for the porter tokenizer with byte spans
// (the stem is not needed for offset reporting; the span of the raw token is).
func (t *PorterTokenizer) TokenizeOffsets(text string) []OffsetToken {
	return tokenizeOffsetsGeneric(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// tokenizeOffsetsGeneric is the shared byte-offset tokenizer: it emits a
// token for each maximal run of isTok runes.
func tokenizeOffsetsGeneric(text string, isTok func(rune) bool) []OffsetToken {
	var tokens []OffsetToken
	start := -1
	for i := 0; i < len(text); {
		r, size := decodeRune(text[i:])
		if isTok(r) {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 {
				tokens = append(tokens, OffsetToken{Term: strings.ToLower(text[start:i]), Start: start, End: i})
				start = -1
			}
		}
		i += size
	}
	if start >= 0 {
		tokens = append(tokens, OffsetToken{Term: strings.ToLower(text[start:]), Start: start, End: len(text)})
	}
	return tokens
}

// decodeRune decodes one rune from a UTF-8 string, returning the rune and its
// byte size (replacement char + 1 byte for invalid sequences).
func decodeRune(s string) (rune, int) {
	if len(s) == 0 {
		return 0, 0
	}
	r := rune(s[0])
	size := 1
	if r >= 0x80 {
		r, size = utf8.DecodeRuneInString(s)
	}
	return r, size
}

// Tokenizer tokenizes text into tokens for the inverted index.
type Tokenizer interface {
	Tokenize(text string) []Token
	// TokenizeOffsets tokenizes text, returning tokens with their byte spans
	// in the source (fts3_snippet.c sqlite3Fts3Offsets; also used by the
	// fts3tokenize virtual table module's start/end columns).
	TokenizeOffsets(text string) []OffsetToken
}

// NewTokenizer creates a tokenizer by name (simple, unicode61, porter).
func NewTokenizer(name string) Tokenizer {
	switch strings.ToLower(name) {
	case "unicode61":
		// Default eRemoveDiacritic is 1 (unicodeCreate, fts3_unicode.c).
		return &Unicode61Tokenizer{eRemoveDiacritic: 1}
	case "porter":
		// Porter tokenizer chains simple + porter stemming
		return &PorterTokenizer{}
	case "simple":
		fallthrough
	default:
		return &SimpleTokenizer{}
	}
}

// NewTokenizerFromSpec creates a tokenizer from a SQLite tokenizer
// specification: a name optionally followed by quoted arguments
// (fts3_tokenizer.c sqlite3Fts3InitTokenizer: `simple"delims"` or
// `simple "x" "y"`). The name must be a known tokenizer or the function
// returns an error (fts3.c fts3InitVtab reports "unknown tokenizer" when the
// tokenizer xCreate fails, e.g. unicode61 with an empty arg — fts3tok1 2.3).
// For the simple tokenizer the SECOND argument is the delimiter string
// (NewSimpleTokenizer).
func NewTokenizerFromSpec(spec string, extraArgs []string) (Tokenizer, error) {
	name, args := splitTokenizerSpec(spec)
	args = append(args, extraArgs...)
	switch strings.ToLower(name) {
	case "simple":
		return NewSimpleTokenizer(args), nil
	case "unicode61":
		return newUnicode61Tokenizer(args)
	case "porter":
		return &PorterTokenizer{}, nil
	}
	// A name registered through fts3_tokenizer(name, module) resolves to its
	// stored factory (fts3_tokenizer.c keeps a hash of registered modules).
	if f, ok := LookupCustomTokenizer(strings.ToLower(name)); ok {
		return f(), nil
	}
	return nil, fmt.Errorf("unknown tokenizer: %s", name)
}

// splitTokenizerSpec splits a tokenizer specification into its name and quoted
// arguments (fts3_tokenizer.c sqlite3Fts3NextToken semantics: the name is the
// first token; each following token is an argument). An unquoted spec like
// "simple" yields just the name.
func splitTokenizerSpec(spec string) (string, []string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	name, rest := splitTokenizerName(spec)
	var args []string
	for len(rest) > 0 {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		arg, tail, consumed := splitTokenizerArg(rest)
		if !consumed {
			break
		}
		args = append(args, arg)
		rest = tail
	}
	return name, args
}

// splitTokenizerName splits the leading name token off a tokenizer spec: the
// name is a quoted string or the leading run of identifier characters.
func splitTokenizerName(spec string) (name, rest string) {
	if spec[0] == '\'' || spec[0] == '"' || spec[0] == '`' {
		q := spec[0]
		if end := strings.IndexByte(spec[1:], q); end >= 0 {
			return spec[1 : 1+end], spec[2+end:]
		}
		return "", spec
	}
	i := 0
	for i < len(spec) && (isFTSIdChar(spec[i]) && spec[i] != '\'' && spec[i] != '"') {
		i++
	}
	return spec[:i], spec[i:]
}

// splitTokenizerArg consumes one argument token from the head of rest,
// returning the argument, the remaining text and whether a token was
// consumed. A quoted token with no closing quote falls through to the
// unquoted rule (the opening quote becomes part of the bare token).
func splitTokenizerArg(rest string) (arg, tail string, consumed bool) {
	if a, t, ok := splitQuotedArg(rest); ok {
		return a, t, true
	}
	// SQLite bracket-quoted identifier: [tokenchars= .] is the argument
	// "tokenchars= ." (fts4unicode.test section 9: a CREATE VIRTUAL
	// TABLE argument written as [tokenchars= .] reaches the tokenizer
	// constructor as `tokenchars= .`).
	if rest[0] == '[' {
		if end := strings.IndexByte(rest[1:], ']'); end >= 0 {
			return rest[1 : 1+end], rest[2+end:], true
		}
	}
	// Unquoted token (e.g. a bare arg): take the next whitespace-run.
	i := 0
	for i < len(rest) && !isSpaceByte(rest[i]) {
		i++
	}
	if i == 0 {
		return "", rest, false
	}
	return rest[:i], rest[i:], true
}

// splitQuotedArg consumes a quote-delimited argument. SQLite quotes: a
// doubled quote inside the string is an escaped quote
// (`"tokenchars=[=""]"` → tokenchars=[="]). Scan with the doubling rule so
// the closing quote is the last one.
func splitQuotedArg(rest string) (arg, tail string, ok bool) {
	if rest[0] != '\'' && rest[0] != '"' && rest[0] != '`' {
		return "", rest, false
	}
	q := rest[0]
	end := -1
	i := 1
	for i < len(rest) {
		if rest[i] == q {
			if i+1 < len(rest) && rest[i+1] == q {
				i += 2
				continue
			}
			end = i
			break
		}
		i++
	}
	if end < 0 {
		return "", rest, false
	}
	inner := rest[1:end]
	inner = strings.ReplaceAll(inner, string(q)+string(q), string(q))
	return inner, rest[end+1:], true
}

// isSpaceByte reports whether b is an ASCII whitespace byte.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// --- Simple tokenizer ---

// SimpleTokenizer splits on delimiter characters and lowercases. With no
// custom delimiters (NewSimpleTokenizer with nil), the delimiters are the
// non-alphanumeric ASCII bytes (fts3_tokenizer1.c simpleCreate default). With
// a custom delimiter string, ONLY the listed bytes are delimiters (the C
// simple tokenizer's delim[] table — fts3tok1.test 1.4: delims "xyz " make x
// a separator but keep the space in tokens unless listed).
type SimpleTokenizer struct {
	// delims maps an ASCII byte to "is a delimiter". nil means the default
	// non-alphanumeric-ASCII delimiter set.
	delims map[byte]bool
}

// NewSimpleTokenizer creates a simple tokenizer. args follow the C simple
// tokenizer's xCreate convention: the SECOND argument (args[1]) is the
// delimiter string; with fewer than two args the default ASCII delimiters
// apply (fts3_tokenizer1.c simpleCreate: `if( argc>1 )` uses argv[1]).
func NewSimpleTokenizer(args []string) *SimpleTokenizer {
	if len(args) < 2 {
		return &SimpleTokenizer{}
	}
	delims := make(map[byte]bool)
	for i := 0; i < len(args[1]); i++ {
		delims[args[1][i]] = true
	}
	return &SimpleTokenizer{delims: delims}
}

// isDelim reports whether b is a delimiter for this tokenizer. In default mode
// a non-alphanumeric ASCII byte is a delimiter; in custom mode only the listed
// bytes are (fts3_tokenizer1.c simpleDelim).
func (t *SimpleTokenizer) isDelim(b byte) bool {
	if t.delims != nil {
		return t.delims[b]
	}
	if b >= 0x80 {
		return false
	}
	return !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9'))
}

func (t *SimpleTokenizer) Tokenize(text string) []Token {
	var tokens []Token
	pos := 0
	current := strings.Builder{}
	for i := 0; i < len(text); {
		r, size := decodeRune(text[i:])
		if size == 1 && t.isDelim(text[i]) {
			if current.Len() > 0 {
				tokens = append(tokens, Token{Term: current.String(), Position: pos})
				pos++
				current.Reset()
			}
			i += size
			continue
		}
		// Non-delimiter rune: include it in the current token. The simple
		// tokenizer lowercases only ASCII A-Z (fts3_tokenizer1.c simpleNext:
		// `ch>='A' && ch<='Z' ? ch-'A'+'a' : ch`); high bytes and non-ASCII
		// runes pass through byte-for-byte (strings.ToLower would corrupt
		// invalid UTF-8 like 0xf1 into U+FFFD — fts3snippet2.test 3.1).
		if r >= 'A' && r <= 'Z' && size == 1 {
			current.WriteByte(byte(r) + ('a' - 'A'))
		} else {
			current.WriteString(text[i : i+size])
		}
		i += size
	}
	if current.Len() > 0 {
		tokens = append(tokens, Token{Term: current.String(), Position: pos})
	}
	return tokens
}

// --- Porter tokenizer ---

// PorterTokenizer applies SimpleTokenizer then Porter stemming.
type PorterTokenizer struct {
	simple SimpleTokenizer
}

func (t *PorterTokenizer) Tokenize(text string) []Token {
	tokens := t.simple.Tokenize(text)
	for i := range tokens {
		tokens[i].Term = stemPorter(tokens[i].Term)
	}
	return tokens
}

// PorterStem exposes the Porter stemming algorithm for the fts5 package's
// porter tokenizer (fts5_porter.c wraps a base tokenizer and stems each of
// its tokens with the same classic algorithm).
func PorterStem(word string) string {
	return stemPorter(word)
}

// stemPorter stems one token with SQLite's FTS3/4 Porter stemmer — a
// faithful port of fts3_porter.c porter_stemmer, including its copy_stemmer
// fallback for tokens that are too short, too long, or contain non-letter
// bytes (see porter_stem.go).
func stemPorter(word string) string {
	return porterStem(word)
}
