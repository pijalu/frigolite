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
	TokenEq        // =
	TokenNeq       // != or <>
	TokenLt        // <
	TokenGt        // >
	TokenArrow     // ->
	TokenDoubleArrow // ->>
	TokenLe        // <=
	TokenGe        // >=
	TokenPlus      // +
	TokenMinus     // -
	TokenStar      // *
	TokenSlash     // /
	TokenMod       // %
	TokenLParen    // (
	TokenRParen    // )
	TokenComma     // ,
	TokenSemicolon // ;
	TokenDot       // .
	TokenConcat    // ||

	// Special
	TokenParam // ?
)

// Token represents a single token from the SQL input.
type Token struct {
	Type  TokenType
	Value string
	Pos   int
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
	"OR": TokenKeyword, "ORDER": TokenKeyword, "OUTER": TokenKeyword,
	"OVER": TokenKeyword, "PARTITION": TokenKeyword, "PLAN": TokenKeyword,
	"PRAGMA": TokenKeyword, "PRECEDING": TokenKeyword, "PRIMARY": TokenKeyword,
	"QUERY": TokenKeyword, "RAISE": TokenKeyword, "RANGE": TokenKeyword,
	"RECURSIVE": TokenKeyword, "REFERENCES": TokenKeyword, "REGEXP": TokenKeyword,
	"REINDEX": TokenKeyword, "RELEASE": TokenKeyword, "RENAME": TokenKeyword,
	"REPLACE": TokenKeyword, "RESTRICT": TokenKeyword, "RETURNING": TokenKeyword,
	"RIGHT": TokenKeyword,
	"ROLLBACK": TokenKeyword, "ROW": TokenKeyword, "ROWS": TokenKeyword,
	"SAVEPOINT": TokenKeyword, "SELECT": TokenKeyword, "SET": TokenKeyword,
	"STORE": TokenKeyword, "STORED": TokenKeyword, "STRICT": TokenKeyword, "TABLE": TokenKeyword, "TEMP": TokenKeyword, "TEMPORARY": TokenKeyword,
	"THEN": TokenKeyword, "TO": TokenKeyword, "TRANSACTION": TokenKeyword,
	"TRIGGER": TokenKeyword, "UNBOUNDED": TokenKeyword, "UNION": TokenKeyword,
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

	switch {
	case ch == '\'':
		return t.readString()
	case ch == '"':
		return t.readQuotedIdent(pos)
	case ch == '`':
		return t.readBacktickIdent(pos)
	case ch == '.' || (ch >= '0' && ch <= '9'):
		return t.readNumber()
	case isIdentStart(ch):
		return t.readIdent()
	case ch == '?':
		t.pos++
		t.last = Token{Type: TokenParam, Value: "?", Pos: pos}
		return t.last
	case ch == '$':
		return t.readDollarParam(pos)
	case ch == '@':
		return t.readAtParam(pos)
	case ch == ':':
		return t.readColonParam(pos)
	case ch == '%':
		t.pos++
		t.last = Token{Type: TokenMod, Value: "%", Pos: pos}
		return t.last
	case ch == '{' || ch == '}':
		// TCL-specific characters: skip them (used in compat test framework)
		t.pos++
		return t.Next()
	case ch == '[':
		return t.readBracketIdent(pos)
	default:
		t.pos++
		t.last = Token{Type: TokenError, Value: string(ch), Pos: pos}
	}
	return t.last
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
		t.pos++
		if t.pos < len(t.input) && t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
			t.last = t.readNumber()
			return &t.last
		}
		t.last = Token{Type: TokenDot, Value: ".", Pos: pos}
		return &t.last
	case '-':
		// Check for -> and ->> operators
		if t.pos+1 < len(t.input) && t.input[t.pos+1] == '>' {
			t.pos += 2
			if t.pos < len(t.input) && t.input[t.pos] == '>' {
				t.pos++
				return &Token{Type: TokenDoubleArrow, Value: "->>", Pos: pos}
			}
			return &Token{Type: TokenArrow, Value: "->", Pos: pos}
		}
		return t.simpleSingleCharToken(ch, pos)
	case '+', '*', '/', '(', ')', ',', ';':
		return t.simpleSingleCharToken(ch, pos)
	default:
		return nil
	}
}

func (t *Tokenizer) simpleSingleCharToken(ch byte, pos int) *Token {
	typ := TokenError
	switch ch {
	case '+':
		typ = TokenPlus
	case '-':
		typ = TokenMinus
	case '*':
		typ = TokenStar
	case '/':
		typ = TokenSlash
	case '(':
		typ = TokenLParen
	case ')':
		typ = TokenRParen
	case ',':
		typ = TokenComma
	case ';':
		typ = TokenSemicolon
	}
	t.pos++
	t.last = Token{Type: typ, Value: string(ch), Pos: pos}
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
	if ch == '#' || (ch == '-' && t.pos+1 < len(t.input) && t.input[t.pos+1] == '-') {
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
	return Token{Type: TokenLt, Value: "<", Pos: pos}
}

func (t *Tokenizer) readGtOp(pos int) Token {
	if t.pos < len(t.input) && t.input[t.pos] == '=' {
		t.pos++
		return Token{Type: TokenGe, Value: ">=", Pos: pos}
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
	return Token{Type: TokenError, Value: "|", Pos: pos}
}

// Peek returns the next token without consuming it.
func (t *Tokenizer) Peek() Token {
	pos := t.pos
	tok := t.Next()
	t.pos = pos
	t.last = tok
	return tok
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
	var buf []byte
	for t.pos < len(t.input) {
		ch := t.input[t.pos]
		if ch == '\'' {
			// Check for escaped quote ''
			if t.pos+1 < len(t.input) && t.input[t.pos+1] == '\'' {
				buf = append(buf, '\'')
				t.pos += 2
				continue
			}
			t.pos++ // skip closing quote
			t.last = Token{Type: TokenString, Value: string(buf), Pos: pos}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	t.last = Token{Type: TokenError, Value: string(buf), Pos: pos}
	return t.last
}

func (t *Tokenizer) readNumber() Token {
	pos := t.pos
	var buf []byte

	// Integer part
	buf = t.readDigits(buf)

	// Hex literal: 0x... or 0X...
	if len(buf) == 1 && buf[0] == '0' && t.pos < len(t.input) && (t.input[t.pos] == 'x' || t.input[t.pos] == 'X') {
		// Check if there's at least one hex digit after 0x
		savePos := t.pos
		t.pos++ // skip x
		if t.pos < len(t.input) && isHexDigit(t.input[t.pos]) {
			buf = append(buf, t.input[savePos]) // add the x/X
			for t.pos < len(t.input) && isHexDigit(t.input[t.pos]) {
				buf = append(buf, t.input[t.pos])
				t.pos++
			}
			t.last = Token{Type: TokenNumber, Value: string(buf), Pos: pos}
			return t.last
		}
		// No hex digits after 0x - fall through to normal number parsing
		t.pos = savePos
	}

	// Fractional part
	if t.pos < len(t.input) && t.input[t.pos] == '.' {
		buf = append(buf, '.')
		t.pos++
		buf = t.readDigits(buf)
	}

	// Exponent
	if t.pos < len(t.input) && (t.input[t.pos] == 'e' || t.input[t.pos] == 'E') {
		buf = append(buf, t.input[t.pos])
		t.pos++
		if t.pos < len(t.input) && (t.input[t.pos] == '+' || t.input[t.pos] == '-') {
			buf = append(buf, t.input[t.pos])
			t.pos++
		}
		buf = t.readDigits(buf)
	}

	t.last = Token{Type: TokenNumber, Value: string(buf), Pos: pos}
	return t.last
}

func (t *Tokenizer) readDigits(buf []byte) []byte {
	for t.pos < len(t.input) && t.input[t.pos] >= '0' && t.input[t.pos] <= '9' {
		buf = append(buf, t.input[t.pos])
		t.pos++
	}
	return buf
}

func (t *Tokenizer) readIdent() Token {
	pos := t.pos
	var buf []byte
	for t.pos < len(t.input) && isIdentPart(t.input[t.pos]) {
		buf = append(buf, t.input[t.pos])
		t.pos++
	}
	word := string(buf)

	// Hex blob literal: X'...' or x'...'
	if len(word) == 1 && (word == "x" || word == "X") && t.pos < len(t.input) && t.input[t.pos] == '\'' {
		t.pos++ // skip '
		for t.pos < len(t.input) {
			ch := t.input[t.pos]
			if ch == '\'' {
				t.pos++
				// Check for doubled '' (escaped quote inside blob)
				if t.pos < len(t.input) && t.input[t.pos] == '\'' {
					t.pos++
					continue
				}
				break
			}
			t.pos++
		}
		t.last = Token{Type: TokenBlob, Value: word, Pos: pos}
		return t.last
	}

	upper := strings.ToUpper(word)
	if _, ok := keywords[upper]; ok {
		t.last = Token{Type: TokenKeyword, Value: upper, Pos: pos}
	} else {
		t.last = Token{Type: TokenIdentifier, Value: word, Pos: pos}
	}
	return t.last
}

func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func (t *Tokenizer) readDollarParam(pos int) Token {
	t.pos++ // skip $
	start := t.pos
	for t.pos < len(t.input) && (isIdentPart(t.input[t.pos]) || t.input[t.pos] == ':') {
		t.pos++
	}
	value := t.input[start:t.pos]
	t.last = Token{Type: TokenParam, Value: "$" + value, Pos: pos}
	return t.last
}

func (t *Tokenizer) readAtParam(pos int) Token {
	t.pos++ // skip @
	start := t.pos
	for t.pos < len(t.input) && (isIdentPart(t.input[t.pos]) || t.input[t.pos] == ':') {
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
			t.last = Token{Type: TokenIdentifier, Value: string(buf), Pos: pos}
			return t.last
		}
		buf = append(buf, ch)
		t.pos++
	}
	t.last = Token{Type: TokenError, Value: string(buf), Pos: pos}
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
	t.last = Token{Type: TokenError, Value: string(buf), Pos: pos}
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
	t.last = Token{Type: TokenError, Value: string(buf), Pos: pos}
	return t.last
}

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}

func isHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}