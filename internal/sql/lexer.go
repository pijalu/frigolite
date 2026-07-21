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
	TokenLe        // <=
	TokenGe        // >=
	TokenPlus      // +
	TokenMinus     // -
	TokenStar      // *
	TokenSlash     // /
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
	"DEFERRED": TokenKeyword, "DELETE": TokenKeyword, "DESC": TokenKeyword,
	"DETACH": TokenKeyword, "DISTINCT": TokenKeyword, "DO": TokenKeyword,
	"DROP": TokenKeyword, "EACH": TokenKeyword, "ELSE": TokenKeyword,
	"END": TokenKeyword, "ESCAPE": TokenKeyword, "EXCEPT": TokenKeyword,
	"EXCLUSIVE": TokenKeyword, "EXISTS": TokenKeyword, "EXPLAIN": TokenKeyword,
	"FAIL": TokenKeyword, "FILTER": TokenKeyword, "FOLLOWING": TokenKeyword,
	"FOR": TokenKeyword, "FOREIGN": TokenKeyword, "FROM": TokenKeyword,
	"FULL": TokenKeyword, "GLOB": TokenKeyword, "GROUP": TokenKeyword,
	"HAVING": TokenKeyword, "IF": TokenKeyword, "IGNORE": TokenKeyword,
	"IMMEDIATE": TokenKeyword, "IN": TokenKeyword, "INDEX": TokenKeyword,
	"INDEXED": TokenKeyword, "INITIALLY": TokenKeyword, "INNER": TokenKeyword,
	"INSERT": TokenKeyword, "INSTEAD": TokenKeyword, "INTERSECT": TokenKeyword,
	"INTO": TokenKeyword, "IS": TokenKeyword, "ISNULL": TokenKeyword,
	"JOIN": TokenKeyword, "KEY": TokenKeyword, "LEFT": TokenKeyword,
	"LIKE": TokenKeyword, "LIMIT": TokenKeyword, "MATCH": TokenKeyword,
	"NATURAL": TokenKeyword, "NO": TokenKeyword, "NOT": TokenKeyword,
	"NOTHING": TokenKeyword, "NOTNULL": TokenKeyword, "NULL": TokenKeyword,
	"OF": TokenKeyword, "OFFSET": TokenKeyword, "ON": TokenKeyword,
	"OR": TokenKeyword, "ORDER": TokenKeyword, "OUTER": TokenKeyword,
	"OVER": TokenKeyword, "PARTITION": TokenKeyword, "PLAN": TokenKeyword,
	"PRAGMA": TokenKeyword, "PRECEDING": TokenKeyword, "PRIMARY": TokenKeyword,
	"QUERY": TokenKeyword, "RAISE": TokenKeyword, "RANGE": TokenKeyword,
	"RECURSIVE": TokenKeyword, "REFERENCES": TokenKeyword, "REGEXP": TokenKeyword,
	"REINDEX": TokenKeyword, "RELEASE": TokenKeyword, "RENAME": TokenKeyword,
	"REPLACE": TokenKeyword, "RESTRICT": TokenKeyword, "RIGHT": TokenKeyword,
	"ROLLBACK": TokenKeyword, "ROW": TokenKeyword, "ROWS": TokenKeyword,
	"SAVEPOINT": TokenKeyword, "SELECT": TokenKeyword, "SET": TokenKeyword,
	"TABLE": TokenKeyword, "TEMP": TokenKeyword, "TEMPORARY": TokenKeyword,
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
	case ch == '.' || (ch >= '0' && ch <= '9'):
		return t.readNumber()
	case isIdentStart(ch):
		return t.readIdent()
	case ch == '?':
		t.pos++
		t.last = Token{Type: TokenParam, Value: "?", Pos: pos}
		return t.last
	default:
		t.pos++
		t.last = Token{Type: TokenError, Value: string(ch), Pos: pos}
	}
	return t.last
}

func (t *Tokenizer) trySingleCharToken(ch byte, pos int) *Token {
	switch ch {
	case '=':
		t.pos++
		t.last = Token{Type: TokenEq, Value: "=", Pos: pos}
	case '<':
		t.pos++
		t.last = t.readLtOp(pos)
	case '>':
		t.pos++
		t.last = t.readGtOp(pos)
	case '!':
		t.pos++
		t.last = t.readBangOp(pos)
	case '+':
		t.pos++
		t.last = Token{Type: TokenPlus, Value: "+", Pos: pos}
	case '-':
		t.pos++
		t.last = Token{Type: TokenMinus, Value: "-", Pos: pos}
	case '*':
		t.pos++
		t.last = Token{Type: TokenStar, Value: "*", Pos: pos}
	case '/':
		t.pos++
		t.last = Token{Type: TokenSlash, Value: "/", Pos: pos}
	case '(':
		t.pos++
		t.last = Token{Type: TokenLParen, Value: "(", Pos: pos}
	case ')':
		t.pos++
		t.last = Token{Type: TokenRParen, Value: ")", Pos: pos}
	case ',':
		t.pos++
		t.last = Token{Type: TokenComma, Value: ",", Pos: pos}
	case ';':
		t.pos++
		t.last = Token{Type: TokenSemicolon, Value: ";", Pos: pos}
	case '.':
		t.pos++
		t.last = Token{Type: TokenDot, Value: ".", Pos: pos}
	case '|':
		t.pos++
		t.last = t.readPipeOp(pos)
	default:
		return nil
	}
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

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}
