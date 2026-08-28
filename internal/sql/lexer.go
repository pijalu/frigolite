package sql

import "strings"

// TokenType represents the type of a SQL token.
type TokenType int

const (
	TokenEOF TokenType = iota
	TokenError
	TokenIdentifier
	TokenString
	TokenNumber
	TokenBlob
	TokenKeyword

	// Operators
	TokenEq          // =
	TokenNeq         // != or <>
	TokenLt          // <
	TokenGt          // >
	TokenArrow       // ->
	TokenDoubleArrow // ->>
	TokenLe          // <=
	TokenGe          // >=
	TokenPlus        // +
	TokenMinus       // -
	TokenStar        // *
	TokenSlash       // /
	TokenMod         // %
	TokenBitAnd      // &
	TokenBitOr       // |
	TokenLShift      // <<
	TokenRShift      // >>
	TokenTilde       // ~
	TokenLParen      // (
	TokenRParen      // )
	TokenComma       // ,
	TokenSemicolon   // ;
	TokenDot         // .
	TokenConcat      // ||

	// Special
	TokenParam // ?

	// TokenUnrecognized marks a lexer error that SQLite reports as
	// "unrecognized token: %q" (e.g. 0x0MATCH, a malformed hex literal
	// followed by identifier characters). TokenError reports "near %q:
	// syntax error" instead.
	TokenUnrecognized
)

// Token represents a single token from the SQL input.
type Token struct {
	Type  TokenType
	Value string
	Pos   int
	// QuotedIdent is true when the token came from a double-quoted
	// identifier ("name"). SQLite's DQS behavior treats double-quoted
	// strings as string literals when they are not column references;
	// an empty double-quoted token ("") is always a string literal.
	QuotedIdent bool
}

// Tokenizer splits SQL text into tokens.
type Tokenizer struct {
	input string
	pos   int
	last  Token
}

// NewTokenizer creates a new tokenizer for the given input.
func NewTokenizer(input string) *Tokenizer {
	return &Tokenizer{input: input}
}

// keywords is a map of SQL keywords.
var keywords = map[string]TokenType{
	"ABORT": TokenKeyword, "ACTION": TokenKeyword, "ADD": TokenKeyword,
	"AFTER": TokenKeyword, "ALL": TokenKeyword, "ALTER": TokenKeyword,
	"ANALYZE": TokenKeyword, "AND": TokenKeyword, "AS": TokenKeyword,
	"ASC": TokenKeyword, "ATTACH": TokenKeyword, "AUTOINCREMENT": TokenKeyword,
	"BEFORE": TokenKeyword, "BEGIN": TokenKeyword, "BETWEEN": TokenKeyword,
	"BY": TokenKeyword, "CASCADE": TokenKeyword, "CASE": TokenKeyword,
	"CAST": TokenKeyword, "CHECK": TokenKeyword, "COLLATE": TokenKeyword,
	"COLUMN": TokenKeyword, "COMMIT": TokenKeyword, "CONFLICT": TokenKeyword,
	"CONSTRAINT": TokenKeyword, "CREATE": TokenKeyword, "CROSS": TokenKeyword,
	"CURRENT": TokenKeyword, "DATABASE": TokenKeyword, "DEFAULT": TokenKeyword,
	"DEFERRABLE": TokenKeyword, "DEFERRED": TokenKeyword, "DELETE": TokenKeyword, "DESC": TokenKeyword,
	"DETACH": TokenKeyword, "DISTINCT": TokenKeyword, "DO": TokenKeyword,
	"DROP": TokenKeyword, "EACH": TokenKeyword, "ELSE": TokenKeyword,
	"END": TokenKeyword, "ESCAPE": TokenKeyword, "EXCEPT": TokenKeyword,
	"EXCLUSIVE": TokenKeyword, "EXISTS": TokenKeyword, "EXPLAIN": TokenKeyword,
	"FAIL": TokenKeyword, "FILTER": TokenKeyword, "FIRST": TokenKeyword, "FOLLOWING": TokenKeyword,
	"FOR": TokenKeyword, "FOREIGN": TokenKeyword, "FROM": TokenKeyword,
	"FULL": TokenKeyword, "GLOB": TokenKeyword, "GROUP": TokenKeyword,
	"GROUPS": TokenKeyword, "EXCLUDE": TokenKeyword,
	"HAVING": TokenKeyword, "IF": TokenKeyword, "IGNORE": TokenKeyword,
	"IMMEDIATE": TokenKeyword, "IN": TokenKeyword, "INDEX": TokenKeyword,
	"INDEXED": TokenKeyword, "INITIALLY": TokenKeyword, "INNER": TokenKeyword,
	"INSERT": TokenKeyword, "INSTEAD": TokenKeyword, "INTERSECT": TokenKeyword,
	"INTO": TokenKeyword, "IS": TokenKeyword, "ISNULL": TokenKeyword,
	"JOIN": TokenKeyword, "KEY": TokenKeyword, "LAST": TokenKeyword, "LEFT": TokenKeyword,
	"LIKE": TokenKeyword, "LIMIT": TokenKeyword, "MATCH": TokenKeyword, "MATERIALIZED": TokenKeyword,
	"NATURAL": TokenKeyword, "NO": TokenKeyword, "NOT": TokenKeyword,
	"NOTHING": TokenKeyword, "NOTNULL": TokenKeyword, "NULL": TokenKeyword,
	"NULLS": TokenKeyword, "OF": TokenKeyword, "OFFSET": TokenKeyword, "ON": TokenKeyword,
	"OTHERS": TokenKeyword,
	"OR":     TokenKeyword, "ORDER": TokenKeyword, "OUTER": TokenKeyword,
	"OVER": TokenKeyword, "PARTITION": TokenKeyword, "PLAN": TokenKeyword,
	"PRAGMA": TokenKeyword, "PRECEDING": TokenKeyword, "PRIMARY": TokenKeyword,
	"QUERY": TokenKeyword, "RAISE": TokenKeyword, "RANGE": TokenKeyword,
	"RECURSIVE": TokenKeyword, "REFERENCES": TokenKeyword, "REGEXP": TokenKeyword,
	"REINDEX": TokenKeyword, "RELEASE": TokenKeyword, "RENAME": TokenKeyword,
	"REPLACE": TokenKeyword, "RESTRICT": TokenKeyword, "RETURNING": TokenKeyword,
	"RIGHT":    TokenKeyword,
	"ROLLBACK": TokenKeyword, "ROW": TokenKeyword, "ROWS": TokenKeyword,
	"SAVEPOINT": TokenKeyword, "SELECT": TokenKeyword, "SET": TokenKeyword,
	"STORE": TokenKeyword, "STORED": TokenKeyword, "STRICT": TokenKeyword, "TABLE": TokenKeyword, "TEMP": TokenKeyword, "TEMPORARY": TokenKeyword,
	"THEN": TokenKeyword, "TIES": TokenKeyword, "TO": TokenKeyword, "TRANSACTION": TokenKeyword,
	"TRIGGER": TokenKeyword, "TRUE": TokenKeyword, "FALSE": TokenKeyword,
	"UNBOUNDED": TokenKeyword, "UNION": TokenKeyword,
	"UNIQUE": TokenKeyword, "UPDATE": TokenKeyword, "USING": TokenKeyword,
	"VACUUM": TokenKeyword, "VALUES": TokenKeyword, "VIEW": TokenKeyword,
	"VIRTUAL": TokenKeyword, "WHEN": TokenKeyword, "WHERE": TokenKeyword,
	"WINDOW": TokenKeyword, "WITH": TokenKeyword, "WITHOUT": TokenKeyword,
}

func (t *Tokenizer) Next() Token {
	t.skipWhitespace()
	if t.pos >= len(t.input) {
		t.last = Token{Type: TokenEOF, Pos: t.pos}
		return t.last
	}

	ch := t.input[t.pos]
	pos := t.pos

	if tok := t.tryComment(); tok != nil {
		return *tok
	}
	if tok := t.trySingleCharToken(ch, pos); tok != nil {
		return *tok
	}
	if tok := t.readParamOrOp(ch, pos); tok != nil {
		return *tok
	}

	switch {
	case isQuoteChar(ch):
		return t.readQuoted(ch, pos)
	case isNumberStart(ch):
		return t.readNumber()
	case isIdentStart(ch):
		return t.readIdent()
	case ch == '[':
		return t.readBracketIdent(pos)
	case ch == '{' || ch == '}':
		// TCL-specific characters: skip them (used in compat test framework)
		t.pos++
		return t.Next()
	}
	t.pos++
	t.last = Token{Type: TokenError, Value: string(ch), Pos: pos}
	return t.last
}

// readQuoted dispatches quoted tokens: single-quoted strings, double-quoted
// identifiers, and backtick identifiers.
func (t *Tokenizer) readQuoted(ch byte, pos int) Token {
	switch ch {
	case '\'':
		return t.readString()
	case '"':
		return t.readQuotedIdent(pos)
	default:
		return t.readBacktickIdent(pos)
	}
}

// isQuoteChar reports whether ch opens a quoted token.
func isQuoteChar(ch byte) bool {
	return ch == '\'' || ch == '"' || ch == '`'
}

// isNumberStart reports whether ch can begin a numeric literal.
func isNumberStart(ch byte) bool {
	return ch == '.' || (ch >= '0' && ch <= '9')
}

// readParamOrOp reads parameter tokens (?, $, @, :, #) and simple single-char
// operators (%, &). Returns nil if ch is none of these.
func (t *Tokenizer) readParamOrOp(ch byte, pos int) *Token {
	switch ch {
	case '?':
		return t.readQuestionParam(pos)
	case '$':
		tok := t.readDollarParam(pos)
		return &tok
	case '@':
		tok := t.readAtParam(pos)
		return &tok
	case ':':
		tok := t.readColonParam(pos)
		return &tok
	case '#':
		tok := t.readHashParam(pos)
		return &tok
	case '%':
		t.pos++
		t.last = Token{Type: TokenMod, Value: "%", Pos: pos}
		return &t.last
	case '&':
		t.pos++
		t.last = Token{Type: TokenBitAnd, Value: "&", Pos: pos}
		return &t.last
	}
	return nil
}

// readQuestionParam reads a ? or ?NNN numbered parameter.
func (t *Tokenizer) readQuestionParam(pos int) *Token {
	t.pos++
	if t.pos < len(t.input) && t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
		paramStart := t.pos
		for t.pos < len(t.input) && t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
			t.pos++
		}
		t.last = Token{Type: TokenParam, Value: "?" + t.input[paramStart:t.pos], Pos: pos}
		return &t.last
	}
	t.last = Token{Type: TokenParam, Value: "?", Pos: pos}
	return &t.last
}

func (t *Tokenizer) trySingleCharToken(ch byte, pos int) *Token {
	switch ch {
	case '=':
		return t.readEqualsOp(pos)
	case '<':
		t.pos++
		t.last = t.readLtOp(pos)
		return &t.last
	case '>':
		t.pos++
		t.last = t.readGtOp(pos)
		return &t.last
	case '!':
		t.pos++
		t.last = t.readBangOp(pos)
		return &t.last
	case '|':
		t.pos++
		t.last = t.readPipeOp(pos)
		return &t.last
	case '.':
		return t.readDotOp(pos)
	case '-':
		return t.readMinusOp(ch, pos)
	case '+', '*', '/', '(', ')', ',', ';', '~':
		return t.simpleSingleCharToken(ch, pos)
	default:
		return nil
	}
}

// readDotOp reads a '.' token, or a number if a digit follows.
func (t *Tokenizer) readDotOp(pos int) *Token {
	t.pos++
	if t.pos < len(t.input) && t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
		t.last = t.readNumber()
		return &t.last
	}
	t.last = Token{Type: TokenDot, Value: ".", Pos: pos}
	return &t.last
}

// readMinusOp reads a '-' token, or the -> / ->> JSON operators.
func (t *Tokenizer) readMinusOp(ch byte, pos int) *Token {
	if t.pos+1 < len(t.input) && t.input[t.pos+1] == '>' {
		t.pos += 2
		if t.pos < len(t.input) && t.input[t.pos] == '>' {
			t.pos++
			return &Token{Type: TokenDoubleArrow, Value: "->>", Pos: pos}
		}
		return &Token{Type: TokenArrow, Value: "->", Pos: pos}
	}
	return t.simpleSingleCharToken(ch, pos)
}

func (t *Tokenizer) simpleSingleCharToken(ch byte, pos int) *Token {
	typ := TokenError
	var val string
	switch ch {
	case '+':
		typ = TokenPlus
		val = "+"
	case '-':
		typ = TokenMinus
		val = "-"
	case '*':
		typ = TokenStar
		val = "*"
	case '/':
		typ = TokenSlash
		val = "/"
	case '(':
		typ = TokenLParen
		val = "("
	case ')':
		typ = TokenRParen
		val = ")"
	case ',':
		typ = TokenComma
		val = ","
	case ';':
		typ = TokenSemicolon
		val = ";"
	case '~':
		typ = TokenTilde
		val = "~"
	}
	t.pos++
	t.last = Token{Type: typ, Value: val, Pos: pos}
	return &t.last
}

func (t *Tokenizer) readEqualsOp(pos int) *Token {
	t.pos++
	if t.pos < len(t.input) && t.input[t.pos] == '=' {
		t.pos++ // skip second '=' for == operator
	}
	t.last = Token{Type: TokenEq, Value: "=", Pos: pos}
	return &t.last
}

func (t *Tokenizer) tryComment() *Token {
	ch := t.input[t.pos]
	if ch == '-' && t.pos+1 < len(t.input) && t.input[t.pos+1] == '-' {
		t.skipLineComment()
		tok := t.Next()
		return &tok
	}
	if ch == '/' && t.pos+1 < len(t.input) && t.input[t.pos+1] == '*' {
		t.skipBlockComment()
		tok := t.Next()
		return &tok
	}
	return nil
}

func (t *Tokenizer) readLtOp(pos int) Token {
	if t.pos < len(t.input) && t.input[t.pos] == '=' {
		t.pos++
		return Token{Type: TokenLe, Value: "<=", Pos: pos}
	}
	if t.pos < len(t.input) && t.input[t.pos] == '>' {
		t.pos++
		return Token{Type: TokenNeq, Value: "<>", Pos: pos}
	}
	if t.pos < len(t.input) && t.input[t.pos] == '<' {
		t.pos++
		return Token{Type: TokenLShift, Value: "<<", Pos: pos}
	}
	return Token{Type: TokenLt, Value: "<", Pos: pos}
}

func (t *Tokenizer) readGtOp(pos int) Token {
	if t.pos < len(t.input) && t.input[t.pos] == '=' {
		t.pos++
		return Token{Type: TokenGe, Value: ">=", Pos: pos}
	}
	if t.pos < len(t.input) && t.input[t.pos] == '>' {
		t.pos++
		return Token{Type: TokenRShift, Value: ">>", Pos: pos}
	}
	return Token{Type: TokenGt, Value: ">", Pos: pos}
}

func (t *Tokenizer) readBangOp(pos int) Token {
	if t.pos < len(t.input) && t.input[t.pos] == '=' {
		t.pos++
		return Token{Type: TokenNeq, Value: "!=", Pos: pos}
	}
	return Token{Type: TokenError, Value: "!", Pos: pos}
}

func (t *Tokenizer) readPipeOp(pos int) Token {
	if t.pos < len(t.input) && t.input[t.pos] == '|' {
		t.pos++
		return Token{Type: TokenConcat, Value: "||", Pos: pos}
	}
	return Token{Type: TokenBitOr, Value: "|", Pos: pos}
}

// Peek returns the next token without consuming it.
func (t *Tokenizer) Peek() Token {
	pos := t.pos
	tok := t.Next()
	t.pos = pos
	t.last = tok
	return tok
}

// Position returns the current scan position.
func (t *Tokenizer) Position() int {
	return t.pos
}

// SetPosition restores the scan position (used for multi-token lookahead).
func (t *Tokenizer) SetPosition(pos int) {
	t.pos = pos
}

func (t *Tokenizer) skipWhitespace() {
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			t.pos++
		} else {
			break
		}
	}
}

func (t *Tokenizer) skipLineComment() {
	for t.pos < len(t.input) && t.input[t.pos] != '\n' {
		t.pos++
	}
}

func (t *Tokenizer) skipBlockComment() {
	t.pos += 2 // skip /*
	for t.pos+1 < len(t.input) {
		if t.input[t.pos] == '*' && t.input[t.pos+1] == '/' {
			t.pos += 2
			return
		}
		t.pos++
	}
	// Unterminated comment - just return
}

func (t *Tokenizer) readString() Token {
	pos := t.pos
	t.pos++ // skip opening quote
	strStart := t.pos
	// Fast path: scan for closing quote without escaped quotes
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '\'' {
			// Check for escaped quote ''
			if t.pos+1 < len(t.input) && t.input[t.pos+1] == '\'' {
				// Slow path: has escaped quotes, fall back to byte buffer
				return t.readEscapedString(pos, strStart)
			}
			// Simple string — use direct slice (no allocation)
			result := t.input[strStart:t.pos]
			t.pos++ // skip closing quote
			t.last = Token{Type: TokenString, Value: result, Pos: pos}
			return t.last
		}
		t.pos++
	}
	// Unterminated string with no escaped quotes: SQLite reports the whole
	// run (opening quote through end of input) as one illegal token
	// ("unrecognized token: \"'abc\"").
	t.last = Token{Type: TokenUnrecognized, Value: t.input[pos:], Pos: pos}
	return t.last
}

// readEscapedString scans a string literal from strStart handling doubled
// ” escapes. pos is the token start position.
func (t *Tokenizer) readEscapedString(pos, strStart int) Token {
	t.pos = strStart
	var buf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '\'' {
			if t.pos+1 < len(t.input) && t.input[t.pos+1] == '\'' {
				buf = append(buf, '\'')
				t.pos += 2
				continue
			}
			t.pos++
			t.last = Token{Type: TokenString, Value: string(buf), Pos: pos}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	// Unterminated string (with escaped quotes): report the whole run as an
	// illegal token, matching SQLite's tokenizer.
	t.last = Token{Type: TokenUnrecognized, Value: t.input[pos:], Pos: pos}
	return t.last
}

func (t *Tokenizer) readNumber() Token {
	pos := t.pos

	// Fast path: read consecutive digits first
	digitStart := t.scanDigits()

	// Check if this is a simple integer (no hex, fraction, exponent, underscore)
	if t.pos == len(t.input) || isSimpleIntegerEnd(t.input[t.pos]) {
		// A number immediately followed by an identifier character is a
		// single illegal token in SQLite (tokenize.c: while(IdChar) i++ →
		// TK_ILLEGAL), e.g. "456ሴ" or "123abc".
		if t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
			tail, _ := t.consumeIdentTail([]byte(t.input[digitStart:t.pos]))
			t.last = Token{Type: TokenUnrecognized, Value: string(tail), Pos: pos}
			return t.last
		}
		// Simple integer — use direct string slice (no allocation for the digit scan)
		t.last = Token{Type: TokenNumber, Value: t.input[digitStart:t.pos], Pos: pos}
		return t.last
	}

	// Slow path: complex number (hex, float, exponent, or underscore separator)
	// Reset position and use the full readDigits path
	t.pos = pos
	buf := t.readDigits(nil)

	// Hex literal: 0x... or 0X...
	if t.isHexPrefix(buf) {
		if tok, handled := t.tryReadHexNumber(pos, buf); handled {
			return tok
		}
	}

	// Fractional part
	buf = t.readFraction(buf)
	// Exponent
	buf = t.readExponent(buf)

	// A number immediately followed by an identifier character is a single
	// illegal token in SQLite (e.g. "1.5x", "1e5x").
	if tail, hasTail := t.consumeIdentTail(buf); hasTail {
		t.last = Token{Type: TokenUnrecognized, Value: string(tail), Pos: pos}
		return t.last
	}

	t.last = Token{Type: TokenNumber, Value: string(buf), Pos: pos}
	return t.last
}

// scanDigits consumes consecutive decimal digits, returning the start
// position of the scanned run.
func (t *Tokenizer) scanDigits() int {
	start := t.pos
	for t.pos < len(t.input) {
		if t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
			t.pos++
		} else {
			break
		}
	}
	return start
}

// isSimpleIntegerEnd reports whether ch cannot continue a number, meaning the
// digits scanned so far form a complete simple integer.
func isSimpleIntegerEnd(ch byte) bool {
	return ch != 'x' && ch != 'X' && ch != '.' && ch != 'e' && ch != 'E' && ch != '_'
}

// isHexPrefix reports whether the digits read so far are the "0" of a
// 0x/0X hex literal with the position on the x/X.
func (t *Tokenizer) isHexPrefix(buf []byte) bool {
	return len(buf) == 1 && buf[0] == '0' && t.pos < len(t.input) && (t.input[t.pos] == 'x' || t.input[t.pos] == 'X')
}

// tryReadHexNumber attempts to parse a 0x/0X hex literal after readDigits has
// consumed the leading "0". Returns the token and true if a hex literal was
// parsed; otherwise restores the position and returns handled=false so the
// caller continues with decimal (fraction/exponent) parsing.
func (t *Tokenizer) tryReadHexNumber(pos int, buf []byte) (Token, bool) {
	savePos := t.pos
	t.pos++ // skip x
	if t.pos < len(t.input) && isHexDigit(t.input[t.pos]) {
		buf = append(buf, t.input[savePos]) // add the x/X
		buf = t.scanHexDigits(buf)
		// A hex literal immediately followed by an identifier character is
		// an unrecognized token in SQLite (e.g. 0x0MATCH, 0x0G, 0x0z).
		if buf, hasTail := t.consumeIdentTail(buf); hasTail {
			t.last = Token{Type: TokenUnrecognized, Value: string(buf), Pos: pos}
			return t.last, true
		}
		t.last = Token{Type: TokenNumber, Value: string(buf), Pos: pos}
		return t.last, true
	}
	// No hex digits after 0x - fall through to normal number parsing
	t.pos = savePos
	return Token{}, false
}

// scanHexDigits consumes hex digits (with underscore separators) into buf.
func (t *Tokenizer) scanHexDigits(buf []byte) []byte {
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if isHexDigit(ch) {
			buf = append(buf, ch)
			t.pos++
		} else if ch == '_' && t.pos+1 < len(t.input) && isHexDigit(t.input[t.pos+1]) {
			t.pos++ // skip underscore separator
		} else {
			break
		}
	}
	return buf
}

// consumeIdentTail appends any trailing identifier characters to buf and
// reports whether any were found.
func (t *Tokenizer) consumeIdentTail(buf []byte) ([]byte, bool) {
	if t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
		for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
			buf = append(buf, t.input[t.pos])
			t.pos++
		}
		return buf, true
	}
	return buf, false
}

// readFraction appends a fractional part (.digits) to buf if present.
func (t *Tokenizer) readFraction(buf []byte) []byte {
	if t.pos < len(t.input) && t.input[t.pos] == '.' {
		buf = append(buf, '.')
		t.pos++
		buf = t.readDigits(buf)
	}
	return buf
}

// readExponent appends an exponent part (e/E[+-]digits) to buf if present.
func (t *Tokenizer) readExponent(buf []byte) []byte {
	if t.pos < len(t.input) && (t.input[t.pos] == 'e' || t.input[t.pos] == 'E') {
		buf = append(buf, t.input[t.pos])
		t.pos++
		if t.pos < len(t.input) && (t.input[t.pos] == '+' || t.input[t.pos] == '-') {
			buf = append(buf, t.input[t.pos])
			t.pos++
		}
		buf = t.readDigits(buf)
	}
	return buf
}

func (t *Tokenizer) readDigits(buf []byte) []byte {
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch >= '0' && ch <= '9' {
			buf = append(buf, ch)
			t.pos++
		} else if ch == '_' && t.pos+1 < len(t.input) && t.input[t.pos+1] >= '0' && t.input[t.pos+1] <= '9' {
			// SQL2017 underscore digit separator: skip the underscore
			// (only valid between digits, not at start/end of number)
			t.pos++
		} else {
			break
		}
	}
	return buf
}

func (t *Tokenizer) readIdent() Token {
	pos := t.pos
	// Fast path: scan identifier characters directly
	identStart := t.pos
	for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
		t.pos++
	}
	word := t.input[identStart:t.pos]

	// Hex blob literal: X'...' or x'...'
	if len(word) == 1 && (word == "x" || word == "X") && t.pos < len(t.input) && t.input[t.pos] == '\'' {
		content := t.readHexBlobContent()
		if len(content)%2 != 0 || !allHexDigits(content) {
			badContent := strings.TrimSpace(content)
			terminated := true
			if i := strings.IndexAny(badContent, "\r\n"); i >= 0 {
				badContent = badContent[:i]
				terminated = false
			}
			value := word + "'" + badContent
			if terminated {
				value += "'"
			}
			t.last = Token{Type: TokenUnrecognized, Value: value, Pos: pos}
			return t.last
		}
		t.last = Token{Type: TokenBlob, Value: content, Pos: pos}
		return t.last
	}

	upper := strings.ToUpper(word)
	if _, ok := keywords[upper]; ok {
		// Store the ORIGINAL word (not uppercased) as the Value so that
		// keywords used as identifiers (e.g. CREATE TABLE savepoint(...))
		// preserve their original case, matching SQLite.
		t.last = Token{Type: TokenKeyword, Value: word, Pos: pos}
	} else {
		t.last = Token{Type: TokenIdentifier, Value: word, Pos: pos}
	}
	return t.last
}

func allHexDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

// readHexBlobContent reads the content between the quotes of a hex blob
// literal (the opening quote has been confirmed and the position is on it).
func (t *Tokenizer) readHexBlobContent() string {
	t.pos++ // skip '
	var hexBuf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '\'' {
			t.pos++
			// Check for doubled '' (escaped quote inside blob)
			if t.pos < len(t.input) && t.input[t.pos] == '\'' {
				hexBuf = append(hexBuf, '\'')
				t.pos++
				continue
			}
			break
		}
		hexBuf = append(hexBuf, ch)
		t.pos++
	}
	// Store the hex content between the quotes as the token value.
	return string(hexBuf)
}

func isIdentStart(ch byte) bool {
	// Multi-byte UTF-8 (ch >= 0x80) is allowed in identifiers (SQLite treats
	// any byte >= 0x80 as an identifier character).
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' || ch >= 0x80
}

// readDollarParam ports tokenize.c's CC_DOLLAR case (TCL variable syntax):
// the name is a run of IdChar/':' characters; a '(' after a non-empty name
// opens TCL-array syntax $name(key) — the key runs to ')' (whitespace aborts,
// and an unterminated key makes the whole run an illegal token). A name with
// zero identifier characters ($ alone) is also illegal, matching SQLite's
// "unrecognized token: \"$\"" for `select $(abc)`.
func (t *Tokenizer) readDollarParam(pos int) Token {
	t.pos++ // skip $
	n := t.scanDollarName()
	if n == 0 {
		// "$" with no name characters at all (e.g. `select $(abc)`).
		return t.tokenAt(TokenUnrecognized, pos)
	}
	if t.pos < len(t.input) && t.input[t.pos] == '(' {
		return t.readTCLArrayKey(pos)
	}
	return t.tokenAt(TokenParam, pos)
}

// scanDollarName ports tokenize.c's CC_DOLLAR name scan: identifier
// characters plus '$', and a ':' only when it starts a TCL namespace
// separator '::' (so "$abc:123" splits into "$abc" followed by ":123",
// matching SQLite). Returns how many characters were consumed.
func (t *Tokenizer) scanDollarName() int {
	n := 0
	for t.pos < len(t.input) {
		c := t.input[t.pos]
		if isIdentPart(c) || c == '$' {
			n++
			t.pos++
			continue
		}
		if c == ':' && t.pos+1 < len(t.input) && t.input[t.pos+1] == ':' {
			t.pos += 2 // namespace separator; not part of the counted name
			continue
		}
		break
	}
	return n
}

// readTCLArrayKey consumes the "(key)" part of $name(key). The key runs to
// ')'; whitespace aborts, and an unterminated key makes the whole run one
// illegal token (SQLite tokenize.c CC_DOLLAR handling).
func (t *Tokenizer) readTCLArrayKey(pos int) Token {
	t.pos++
	for t.pos < len(t.input) {
		k := t.input[t.pos]
		if k == ' ' || k == '\t' || k == '\n' || k == '\r' || k == '\f' {
			break // whitespace aborts the key scan (unterminated)
		}
		t.pos++
		if k == ')' {
			return t.tokenAt(TokenParam, pos)
		}
	}
	// Unterminated $name(key — the whole run is one illegal token.
	return t.tokenAt(TokenUnrecognized, pos)
}

// tokenAt sets and returns the last token of the given type spanning pos..pos.
func (t *Tokenizer) tokenAt(typ TokenType, pos int) Token {
	t.last = Token{Type: typ, Value: t.input[pos:t.pos], Pos: pos}
	return t.last
}

func (t *Tokenizer) readAtParam(pos int) Token {
	t.pos++ // skip @
	start := t.pos
	// tokenize.c CC_VARALPHA: '@' takes a plain IdChar run — no ':'.
	for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
		t.pos++
	}
	value := t.input[start:t.pos]
	t.last = Token{Type: TokenParam, Value: "@" + value, Pos: pos}
	return t.last
}

func (t *Tokenizer) readColonParam(pos int) Token {
	t.pos++ // skip :
	start := t.pos
	for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
		t.pos++
	}
	value := t.input[start:t.pos]
	t.last = Token{Type: TokenParam, Value: ":" + value, Pos: pos}
	return t.last
}

// readHashParam handles '#' following SQLite's tokenizer rules: '#name' is a
// variable (like :name), but '#' followed by a digit or anything else is an
// illegal token whose text includes the following identifier characters so the
// error message can quote it (e.g. `near "#1": syntax error`).
func (t *Tokenizer) readHashParam(pos int) Token {
	t.pos++ // skip #
	if t.pos < len(t.input) && isIdentStart(t.input[t.pos]) {
		start := t.pos
		for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
			t.pos++
		}
		t.last = Token{Type: TokenParam, Value: "#" + t.input[start:t.pos], Pos: pos}
		return t.last
	}
	end := t.pos
	for end < len(t.input) && isIdentPart(t.input[end]) {
		end++
	}
	value := t.input[t.pos:end]
	t.pos = end
	t.last = Token{Type: TokenError, Value: "#" + value, Pos: pos}
	return t.last
}

func (t *Tokenizer) readQuotedIdent(pos int) Token {
	t.pos++ // skip opening "
	var buf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '"' {
			// Check for escaped quote ""
			if t.pos+1 < len(t.input) && t.input[t.pos+1] == '"' {
				buf = append(buf, '"')
				t.pos += 2
				continue
			}
			t.pos++ // skip closing "
			t.last = Token{Type: TokenIdentifier, Value: string(buf), Pos: pos, QuotedIdent: true}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	// Unterminated double-quoted identifier: SQLite reports the whole run
	// (opening quote through end of input) as an illegal token.
	t.last = Token{Type: TokenUnrecognized, Value: t.input[pos:], Pos: pos}
	return t.last
}

func (t *Tokenizer) readBacktickIdent(pos int) Token {
	t.pos++ // skip opening `
	var buf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '`' {
			// Check for escaped backtick ``
			if t.pos+1 < len(t.input) && t.input[t.pos+1] == '`' {
				buf = append(buf, '`')
				t.pos += 2
				continue
			}
			t.pos++ // skip closing `
			t.last = Token{Type: TokenIdentifier, Value: string(buf), Pos: pos}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	// Unterminated backtick identifier: illegal token, matching SQLite.
	t.last = Token{Type: TokenUnrecognized, Value: t.input[pos:], Pos: pos}
	return t.last
}

func (t *Tokenizer) readBracketIdent(pos int) Token {
	t.pos++ // skip opening [
	var buf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == ']' {
			t.pos++ // skip closing ]
			t.last = Token{Type: TokenIdentifier, Value: string(buf), Pos: pos}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	// Unterminated bracket identifier: SQLite reports the whole run (opening
	// bracket through end of input) as an illegal token.
	t.last = Token{Type: TokenUnrecognized, Value: t.input[pos:], Pos: pos}
	return t.last
}

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}

func isHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}
