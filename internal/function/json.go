// JSON1 shared core: the jsonNode value model and the relaxed-mode parser.
//
// This is a pure-Go implementation of the core of SQLite's JSON1 extension
// (ext/misc/json.c) with SQLite's "relaxed" JSON mode: object keys may be
// unquoted, strings may use single quotes, numbers accept a leading '+', a
// leading '.' (".5") or trailing '.' ("1.") and hexadecimal ("0x10") forms,
// and objects/arrays accept trailing commas. NaN parses to SQL NULL and
// +/-Infinity parse to a value that serializes as "9.0e+999"/"-9.0e+999" (SQLite's
// JSON overflow representation).
package function

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// jsonKind is the JSON value kind.
type jsonKind int

const (
	jsonNull jsonKind = iota
	jsonObject
	jsonArray
	jsonString
	jsonNumber
	jsonTrue
	jsonFalse
)

// jsonPair is one ordered object member.
type jsonPair struct {
	key   string
	value *jsonNode
}

// jsonNode is a parsed JSON value. Object members and array elements keep
// their source order. Numbers store both their typed value (for json_extract
// typing) and their serialized text (SQLite re-emits the original number text
// verbatim, with only hex/plus/leading-dot normalizations).
type jsonNode struct {
	kind  jsonKind
	str   string // string value (unescaped)
	i64   int64  // integer number value (isInt)
	num   float64
	isInt bool
	text  string // serialized number text (jsonNumber only)
	arr   []*jsonNode
	obj   []jsonPair
}

// jsonParser is a relaxed-mode JSON parser over a byte string.
type jsonParser struct {
	src    string
	pos    int
	errPos int // byte offset of the most recent parse failure
	depth  int // current container nesting depth (SQLite JSON_MAX_DEPTH)
}

// jsonMaxDepth mirrors SQLite's JSON_MAX_DEPTH (src/json.c): documents
// nested deeper than this are malformed.
const jsonMaxDepth = 1000

// jsonParseError is the error raised for malformed JSON input.
type jsonParseError struct{ msg string }

func (e *jsonParseError) Error() string { return e.msg }

func jsonParseErr() error { return &jsonParseError{msg: "malformed JSON"} }

// fail records the current offset as the parse-failure position and returns
// the standard malformed-JSON error (used by json_error_position).
func (p *jsonParser) fail() error {
	p.errPos = p.pos
	return jsonParseErr()
}

// parseJSON parses src as relaxed-mode JSON.
func parseJSON(src string) (*jsonNode, error) {
	p := &jsonParser{src: src}
	p.skipWS()
	n, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.src) {
		return nil, p.fail()
	}
	return n, nil
}

func (p *jsonParser) skipWS() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '/' {
			if !p.skipJSON5Comment() {
				return
			}
			continue
		}
		w := jsonSpaceWidth(p.src, p.pos)
		if w == 0 {
			return
		}
		p.pos += w
	}
}

// skipJSON5Comment consumes a // or /* */ comment at p.pos (the '/' byte).
// It reports whether a comment was consumed; a lone '/' is not a comment.
func (p *jsonParser) skipJSON5Comment() bool {
	if p.pos+1 >= len(p.src) {
		return false
	}
	switch p.src[p.pos+1] {
	case '/':
		for p.pos < len(p.src) && p.src[p.pos] != '\n' {
			p.pos++
		}
		return true
	case '*':
		end := strings.Index(p.src[p.pos+2:], "*/")
		if end < 0 {
			p.pos = len(p.src)
		} else {
			p.pos += 2 + end + 2
		}
		return true
	}
	return false
}

// jsonASCIISpace reports the single-byte JSON whitespace characters.
func jsonASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', 0x0b, 0x0c:
		return true
	}
	return false
}

// jsonSpace3 maps the fixed three-byte whitespace sequences to their width.
var jsonSpace3 = map[[3]byte]int{
	{0xe1, 0x9a, 0x80}: 3, // U+1680 OGHAM SPACE MARK
	{0xe2, 0x81, 0x9f}: 3, // U+205F MEDIUM MATHEMATICAL SPACE
	{0xe3, 0x80, 0x80}: 3, // U+3000 IDEOGRAPHIC SPACE
	{0xef, 0xbb, 0xbf}: 3, // U+FEFF ZERO WIDTH NO-BREAK SPACE (BOM)
}

// jsonSpaceWidth returns the byte width of the Unicode whitespace sequence
// starting at src[i] (the whitespace set accepted by SQLite's relaxed JSON
// reader, src/json.c jsonParseSkipWs), or 0 when src[i] is not whitespace.
func jsonSpaceWidth(src string, i int) int {
	c := src[i]
	if jsonASCIISpace(c) {
		return 1
	}
	if c == 0xc2 && i+1 < len(src) && (src[i+1] == 0xa0 || src[i+1] == 0x85) {
		return 2 // U+00A0 NBSP, U+0085 NEL
	}
	if c < 0xe0 || i+2 >= len(src) {
		return 0
	}
	if _, ok := jsonSpace3[[3]byte{src[i], src[i+1], src[i+2]}]; ok {
		return 3
	}
	return jsonSpaceWidthE2(src, i)
}

// jsonSpaceWidthE2 classifies the U+2xxx whitespace sequences (lead 0xe2).
func jsonSpaceWidthE2(src string, i int) int {
	if i+2 >= len(src) {
		return 0
	}
	if src[i+1] == 0x80 {
		switch c := src[i+2]; {
		case c == 0xa8 || c == 0xa9: // U+2028 LINE SEPARATOR / U+2029 PARAGRAPH SEPARATOR
			return 3
		case c >= 0x80 && c <= 0x8a: // U+2000..U+200A EN QUAD .. HAIR SPACE
			return 3
		case c == 0xaf: // U+202F NARROW NO-BREAK SPACE
			return 3
		}
	}
	if src[i+1] == 0x81 && src[i+2] == 0x9f { // U+205F MEDIUM MATHEMATICAL SPACE
		return 3
	}
	return 0
}

func (p *jsonParser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

// parseNested runs fn with the container depth incremented, failing with
// malformed-JSON when nesting exceeds SQLite's JSON_MAX_DEPTH
// (json.c: "if( iDepth>JSON_MAX_DEPTH ) return i+1").
func (p *jsonParser) parseNested(fn func() (*jsonNode, error)) (*jsonNode, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > jsonMaxDepth {
		return nil, p.fail()
	}
	return fn()
}

func (p *jsonParser) parseValue() (*jsonNode, error) {
	if n, ok := p.tryKeywordValue(); ok {
		return n, nil
	}
	switch c := p.peek(); {
	case c == '{':
		return p.parseNested(p.parseObject)
	case c == '[':
		return p.parseNested(p.parseArray)
	case c == '"' || c == '\'':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &jsonNode{kind: jsonString, str: s}, nil
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	}
	return nil, p.fail()
}

type jsonKeyword struct {
	word string
	node func() *jsonNode
}

// jsonKeywords lists the JSON keyword literals accepted in relaxed mode.
// Longer words precede their prefixes ("Infinity" before "Inf", and the
// negative forms first so "-Inf" matches before "-" is treated as a sign).
var jsonKeywords = []jsonKeyword{
	{"-Infinity", func() *jsonNode { return infNode(true) }},
	{"-Inf", func() *jsonNode { return infNode(true) }},
	{"Infinity", func() *jsonNode { return infNode(false) }},
	{"Inf", func() *jsonNode { return infNode(false) }},
	{"NaN", func() *jsonNode {
		// NaN parses to a real whose value is SQL NULL at extraction and
		// "null" at serialization (SQLite json.c jsonParseNumber).
		return &jsonNode{kind: jsonNumber, num: math.NaN(), isInt: false, text: "null"}
	}},
	{"true", func() *jsonNode { return &jsonNode{kind: jsonTrue} }},
	{"false", func() *jsonNode { return &jsonNode{kind: jsonFalse} }},
	{"null", func() *jsonNode { return &jsonNode{kind: jsonNull} }},
}

// tryKeywordValue attempts to parse a JSON keyword literal at the current
// position. It returns ok=false when the next token is not a keyword; the
// parser position is unchanged then.
func (p *jsonParser) tryKeywordValue() (*jsonNode, bool) {
	for _, kw := range jsonKeywords {
		if strings.HasPrefix(p.src[p.pos:], kw.word) {
			p.pos += len(kw.word)
			return kw.node(), true
		}
	}
	return nil, false
}

// parseObject parses { key:value, ... } with optional trailing comma.
func (p *jsonParser) parseObject() (*jsonNode, error) {
	n := &jsonNode{kind: jsonObject}
	p.pos++ // '{'
	p.skipWS()
	if p.peek() == '}' {
		p.pos++
		return n, nil
	}
	for {
		p.skipWS()
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.peek() != ':' {
			return nil, p.fail()
		}
		p.pos++
		p.skipWS()
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		n.obj = append(n.obj, jsonPair{key: key, value: v})
		p.skipWS()
		c := p.peek()
		if c == ',' {
			p.pos++
			p.skipWS()
			if p.peek() == '}' {
				// Trailing comma (relaxed mode).
				p.pos++
				return n, nil
			}
			continue
		}
		if c == '}' {
			p.pos++
			return n, nil
		}
		return nil, p.fail()
	}
}

// parseKey parses an object key: a quoted string (double or single) or an
// unquoted identifier ([A-Za-z0-9_]+, relaxed mode).
func (p *jsonParser) parseKey() (string, error) {
	c := p.peek()
	if c == '"' || c == '\'' {
		return p.parseString()
	}
	start := p.pos
	for p.pos < len(p.src) {
		if !isJSON5IdentChar(p.src[p.pos]) {
			break
		}
		p.pos++
	}
	if p.pos == start {
		return "", p.fail()
	}
	return p.src[start:p.pos], nil
}

// isJSON5IdentChar reports the relaxed-mode (JSON5) unquoted-key characters:
// identifier characters plus '$' and any non-ASCII byte.
func isJSON5IdentChar(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// parseArray parses [ v1, v2, ... ] with optional trailing comma.
func (p *jsonParser) parseArray() (*jsonNode, error) {
	n := &jsonNode{kind: jsonArray}
	p.pos++ // '['
	p.skipWS()
	if p.peek() == ']' {
		p.pos++
		return n, nil
	}
	for {
		p.skipWS()
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		n.arr = append(n.arr, v)
		p.skipWS()
		c := p.peek()
		if c == ',' {
			p.pos++
			p.skipWS()
			if p.peek() == ']' {
				// Trailing comma (relaxed mode).
				p.pos++
				return n, nil
			}
			continue
		}
		if c == ']' {
			p.pos++
			return n, nil
		}
		return nil, p.fail()
	}
}

// parseString parses a double- or single-quoted string with JSON escapes.
func (p *jsonParser) parseString() (string, error) {
	quote := p.src[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == quote {
			p.pos++
			return sb.String(), nil
		}
		if c == '\\' {
			if err := p.parseEscape(&sb); err != nil {
				return "", err
			}
			continue
		}
		if c < 0x20 {
			// Raw control characters inside strings are silently dropped
			// by sqlite's lenient parser (json501 14.x).
			p.pos++
			continue
		}
		sb.WriteByte(c)
		p.pos++
	}
	return "", p.fail()
}

// jsonSimpleEscapes maps a JSON escape character to its byte value.
var jsonSimpleEscapes = map[byte]byte{
	'"':  '"',
	'\'': '\'',
	'\\': '\\',
	'/':  '/',
	'b':  '\b',
	'f':  '\f',
	'n':  '\n',
	'v':  '\v',
	'0':  0,
	'r':  '\r',
	't':  '\t',
}

// parseEscape parses one backslash escape sequence (the backslash has already
// been consumed) and writes its decoded form to sb.
func (p *jsonParser) parseEscape(sb *strings.Builder) error {
	p.pos++
	if p.pos >= len(p.src) {
		return p.fail()
	}
	e := p.src[p.pos]
	p.pos++
	if b, ok := jsonSimpleEscapes[e]; ok {
		sb.WriteByte(b)
		return nil
	}
	if e == 'u' {
		r, err := p.parseUnicodeEscape()
		if err != nil {
			return err
		}
		sb.WriteRune(r)
		return nil
	}
	if e == 'x' {
		// JSON5 \xHH hex escape (relaxed mode only; the strict scanner
		// rejects it separately).
		if p.pos+2 > len(p.src) || !isHexDigit(p.src[p.pos]) || !isHexDigit(p.src[p.pos+1]) {
			return p.fail()
		}
		hi, lo := hexVal(p.src[p.pos]), hexVal(p.src[p.pos+1])
		p.pos += 2
		sb.WriteRune(rune(hi<<4 | lo))
		return nil
	}
	if p.escapeLineContinuation(e) {
		return nil
	}
	return p.fail()
}

// escapeLineContinuation consumes the JSON5 line-continuation escapes that
// may follow a backslash: a line terminator contributes nothing to the
// string value. It reports whether e was one.
func (p *jsonParser) escapeLineContinuation(e byte) bool {
	switch e {
	case '\n':
		// Line continuation.
		return true
	case '\r':
		// \r\n counts as one terminator.
		if p.peek() == '\n' {
			p.pos++
		}
		return true
	case 0xe2:
		// \U+2028 / \U+2029 are JSON5 line terminators too.
		if p.pos+2 < len(p.src) && p.src[p.pos] == 0x80 &&
			(p.src[p.pos+1] == 0xa8 || p.src[p.pos+1] == 0xa9) {
			p.pos += 2
			return true
		}
	}
	return false
}

// parseUnicodeEscape parses \uXXXX, combining surrogate pairs.
func (p *jsonParser) parseUnicodeEscape() (rune, error) {
	if p.pos+4 > len(p.src) {
		return 0, p.fail()
	}
	v, err := strconv.ParseUint(p.src[p.pos:p.pos+4], 16, 32)
	if err != nil {
		return 0, p.fail()
	}
	p.pos += 4
	r := rune(v)
	if lo, ok := p.trySurrogatePair(r); ok {
		return lo, nil
	}
	if r >= 0xD800 && r <= 0xDFFF {
		// Unpaired surrogate: SQLite emits the raw byte (U+FFFD-style
		// replacement is not performed; the code unit round-trips).
		return utf8.RuneError, nil
	}
	return r, nil
}

// trySurrogatePair combines a high surrogate \uD800-\uDBFF immediately
// followed by a low surrogate \uDC00-\uDFFF into one rune. Returns the
// decoded rune and ok=true when the pair is present.
func (p *jsonParser) trySurrogatePair(r rune) (rune, bool) {
	if r < 0xD800 || r > 0xDBFF {
		return 0, false
	}
	if p.pos+6 > len(p.src) || p.src[p.pos] != '\\' || p.src[p.pos+1] != 'u' {
		return 0, false
	}
	lo, err := strconv.ParseUint(p.src[p.pos+2:p.pos+6], 16, 32)
	if err != nil {
		return 0, false
	}
	lr := rune(lo)
	if lr < 0xDC00 || lr > 0xDFFF {
		return 0, false
	}
	p.pos += 6
	return utf16.DecodeRune(r, lr), true
}

// parseNumber parses a JSON number in SQLite relaxed mode.
func (p *jsonParser) parseNumber() (*jsonNode, error) {
	start := p.pos
	sign := p.numberSign()
	// Signed Infinity (JSON5): +/-Infinity, +/-Inf.
	if p.peek() == 'I' {
		for _, w := range []string{"Infinity", "Inf"} {
			if strings.HasPrefix(p.src[p.pos:], w) {
				p.pos += len(w)
				neg := sign < 0
				return infNode(neg), nil
			}
		}
	}
	if p.peek() == '0' && p.pos+1 < len(p.src) && (p.src[p.pos+1] == 'x' || p.src[p.pos+1] == 'X') {
		return p.parseHexNumber(sign)
	}
	return p.parseDecimalNumber(start, sign)
}

// numberSign consumes an optional leading '-' (sign=-1) or '+' (sign=1) and
// returns the sign.
func (p *jsonParser) numberSign() int {
	switch p.peek() {
	case '-':
		p.pos++
		return -1
	case '+':
		p.pos++
		return 1
	}
	return 1
}

// parseHexNumber parses the relaxed-mode hexadecimal form 0x[0-9a-fA-F]+.
func (p *jsonParser) parseHexNumber(sign int) (*jsonNode, error) {
	p.pos += 2 // "0x"
	digits := p.pos
	for p.pos < len(p.src) && isHexDigit(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == digits {
		return nil, p.fail()
	}
	u, err := strconv.ParseUint(p.src[digits:p.pos], 16, 64)
	if err != nil {
		return nil, p.fail()
	}
	v := int64(u)
	if sign < 0 {
		v = -v
	}
	return &jsonNode{kind: jsonNumber, i64: v, isInt: true, text: strconv.FormatInt(v, 10)}, nil
}

// parseDecimalNumber parses a decimal/exponent JSON number: an optional
// integer part, optional fraction, optional exponent. The serialized text is
// normalized (leading '+' stripped, ".5"→"0.5", "1."→"1.0", sign reapplied).
func (p *jsonParser) parseDecimalNumber(start, sign int) (*jsonNode, error) {
	isReal := false
	if err := p.scanIntegerPart(&isReal); err != nil {
		return nil, err
	}
	p.scanFractionPart(&isReal)
	if err := p.scanExponentPart(&isReal); err != nil {
		return nil, err
	}
	text := normalizeNumberText(p.src[start:p.pos], sign)
	return numberNode(text, isReal)
}

// scanIntegerPart consumes the integer digits. SQLite relaxed mode rejects a
// leading zero unless the number is exactly "0".
func (p *jsonParser) scanIntegerPart(isReal *bool) error {
	intStart := p.pos
	digits := 0
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		if digits == 1 && p.src[intStart] == '0' {
			return p.fail()
		}
		digits++
		p.pos++
	}
	if digits == 0 {
		if p.peek() == '.' {
			// ".5" form (relaxed mode).
			*isReal = true
			return nil
		}
		return p.fail()
	}
	return nil
}

// scanFractionPart consumes an optional ".digits" fraction.
func (p *jsonParser) scanFractionPart(isReal *bool) {
	if p.peek() != '.' {
		return
	}
	*isReal = true
	p.pos++
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
}

// scanExponentPart consumes an optional "e[+-]digits" exponent.
func (p *jsonParser) scanExponentPart(isReal *bool) error {
	if p.peek() != 'e' && p.peek() != 'E' {
		return nil
	}
	*isReal = true
	p.pos++
	if p.peek() == '+' || p.peek() == '-' {
		p.pos++
	}
	ed := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == ed {
		return p.fail()
	}
	return nil
}

// normalizeNumberText builds the serialized form of a parsed number: leading
// '+' stripped, ".5"→"0.5", "1."→"1.0", sign reapplied when negative.
func normalizeNumberText(raw string, sign int) string {
	text := strings.TrimPrefix(raw, "+")
	neg := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	if strings.HasPrefix(text, ".") {
		text = "0" + text
	}
	if strings.HasSuffix(text, ".") {
		text = text + "0"
	}
	if (neg || sign < 0) && !strings.HasPrefix(text, "-") {
		text = "-" + text
	}
	// JSON5 allows "4.e2" (empty fraction before the exponent); Go's
	// ParseFloat rejects it, so materialize the missing zero digit.
	if i := strings.IndexAny(text, "eE"); i > 0 && strings.HasSuffix(text[:i], ".") {
		text = text[:i] + "0" + text[i:]
	}
	return text
}

// numberNode builds a jsonNumber node from its serialized text. Integers
// (no fraction/exponent) are exact int64 when they fit, else fall back to
// real; reals are float64.
func numberNode(text string, isReal bool) (*jsonNode, error) {
	if !isReal {
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return &jsonNode{kind: jsonNumber, i64: i, isInt: true, text: text}, nil
		}
		isReal = true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Out-of-range literals (e.g. 9.0e+999) saturate to ±Inf/0 exactly
		// like SQLite's atof (strtod semantics); only true syntax failures
		// are malformed.
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return nil, jsonParseErr()
		}
	}
	return &jsonNode{kind: jsonNumber, num: f, isInt: false, text: text}, nil
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// hexVal converts one hex digit to its nibble value.
func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// jsonKeySimpleEscapes maps the single-character escape letters of a
// double-quoted path key to their decoded byte.
var jsonKeySimpleEscapes = map[byte]byte{
	'n': '\n', 't': '\t', 'r': '\r', 'b': '\b', 'f': '\f',
}

// decodeJSONKeyHex decodes a fixed-width hex escape (\uXXXX or \xHH) whose
// letter is at s[i]. It returns the decoded rune, the digit count consumed,
// and whether the escape was well-formed (enough digits remain).
func decodeJSONKeyHex(s string, i, ndigits int) (r rune, n int, ok bool) {
	if i+ndigits >= len(s) {
		return rune(s[i]), 0, false
	}
	v := 0
	for k := 1; k <= ndigits; k++ {
		v = v<<4 | hexVal(s[i+k])
	}
	return rune(v), ndigits, true
}

// decodeJSONKeyEscapes resolves backslash escapes inside a double-quoted
// JSON path key (\" \\ \/ \b \f \n \r \t and \uXXXX).
func decodeJSONKeyEscapes(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i += decodeOneJSONKeyEscape(s, i+1, &sb)
	}
	return sb.String()
}

// decodeOneJSONKeyEscape decodes the escape whose letter is at s[i] and
// appends it to sb. It returns the number of source bytes consumed,
// including the escape letter itself.
func decodeOneJSONKeyEscape(s string, i int, sb *strings.Builder) int {
	switch c := s[i]; c {
	case 'u':
		if r, n, ok := decodeJSONKeyHex(s, i, 4); ok {
			sb.WriteRune(r)
			return 1 + n
		}
		sb.WriteByte('u')
	case 'x':
		// JSON5 \xHH escape.
		if r, n, ok := decodeJSONKeyHex(s, i, 2); ok {
			sb.WriteRune(r)
			return 1 + n
		}
		sb.WriteByte('x')
	default:
		if b, ok := jsonKeySimpleEscapes[c]; ok {
			sb.WriteByte(b)
		} else {
			sb.WriteByte(c)
		}
	}
	return 1
}

// infNode builds a signed Infinity number node from parsed Infinity
// keywords; sqlite normalizes these to "±9e999" on serialization.
func infNode(neg bool) *jsonNode {
	if neg {
		return &jsonNode{kind: jsonNumber, num: math.Inf(-1), isInt: false, text: "-9e999"}
	}
	return &jsonNode{kind: jsonNumber, num: math.Inf(1), isInt: false, text: "9e999"}
}

// --- Path parsing and walking ---

// jsonPathComponent is one step of a JSON path: an object key or an array
// index.
type jsonPathComponent struct {
	key   string
	index int64
	isIdx bool
	// tail marks a '#' index ('#', '#-N', '#+N' or the negative abbreviated
	// operator form): index is an offset relative to the array length.
	tail bool
}
