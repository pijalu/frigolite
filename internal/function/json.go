// JSON1 core functions: json_extract() and json_insert().
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
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/util"
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
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == 0x0b || c == 0x0c:
			p.pos++
		case c == 0xc2 && p.pos+1 < len(p.src) && p.src[p.pos+1] == 0xa0:
			// U+00A0 NBSP
			p.pos += 2
		case c == 0xc2 && p.pos+1 < len(p.src) && p.src[p.pos+1] == 0x85:
			// U+0085 NEL
			p.pos += 2
		case c == 0xe2 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x80 &&
			(p.src[p.pos+2] == 0xa8 || p.src[p.pos+2] == 0xa9):
			// U+2028 LINE SEPARATOR / U+2029 PARAGRAPH SEPARATOR
			p.pos += 3
		case c == 0xe1 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x9a && p.src[p.pos+2] == 0x80:
			// U+1680 OGHAM SPACE MARK
			p.pos += 3
		case c == 0xe2 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x80 &&
			p.src[p.pos+2] >= 0x80 && p.src[p.pos+2] <= 0x8a:
			// U+2000..U+200A EN QUAD .. HAIR SPACE
			p.pos += 3
		case c == 0xe2 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x80 &&
			(p.src[p.pos+2] == 0xaf):
			// U+202F NARROW NO-BREAK SPACE
			p.pos += 3
		case c == 0xe2 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x81 && p.src[p.pos+2] == 0x9f:
			// U+205F MEDIUM MATHEMATICAL SPACE
			p.pos += 3
		case c == 0xe3 && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0x80 && p.src[p.pos+2] == 0x80:
			// U+3000 IDEOGRAPHIC SPACE
			p.pos += 3
		case c == 0xef && p.pos+2 < len(p.src) && p.src[p.pos+1] == 0xbb && p.src[p.pos+2] == 0xbf:
			// U+FEFF ZERO WIDTH NO-BREAK SPACE (BOM)
			p.pos += 3
		case c == '/':
			// JSON5 comments: // to end of line, /* block */.
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == '/' {
				for p.pos < len(p.src) && p.src[p.pos] != '\n' {
					p.pos++
				}
				continue
			}
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == '*' {
				end := strings.Index(p.src[p.pos+2:], "*/")
				if end < 0 {
					p.pos = len(p.src)
				} else {
					p.pos += 2 + end + 2
				}
				continue
			}
			return
		default:
			return
		}
	}
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
		c = p.src[p.pos]
		if c == '_' || c == '$' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", p.fail()
	}
	return p.src[start:p.pos], nil
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
	if e == '\n' {
		// JSON5 line continuation: backslash followed by a line terminator
		// contributes nothing to the string value.
		return nil
	}
	if e == '\r' {
		// \r\n counts as one terminator.
		if p.peek() == '\n' {
			p.pos++
		}
		return nil
	}
	if e == 0xe2 && p.pos+2 < len(p.src) && p.src[p.pos] == 0x80 && (p.src[p.pos+1] == 0xa8 || p.src[p.pos+1] == 0xa9) {
		// \U+2028 / \U+2029 are JSON5 line terminators too.
		p.pos += 2
		return nil
	}
	return p.fail()
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

// decodeJSONKeyEscapes resolves backslash escapes inside a double-quoted
// JSON path key (\" \\ \/ \b \f \n \r \t and \uXXXX).
func decodeJSONKeyEscapes(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'u':
			if i+4 < len(s) {
				r := rune(hexVal(s[i+1])<<12 | hexVal(s[i+2])<<8 | hexVal(s[i+3])<<4 | hexVal(s[i+4]))
				sb.WriteRune(r)
				i += 4
				continue
			}
			sb.WriteByte('u')
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'x':
			// JSON5 \xHH escape.
			if i+2 < len(s) {
				r := rune(hexVal(s[i+1])<<4 | hexVal(s[i+2]))
				sb.WriteRune(r)
				i += 2
				continue
			}
			sb.WriteByte('x')
		default:
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
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

// parseJSONPath parses a JSON path: $, then .key / ."key" / [N] steps.
func parseJSONPath(path string) ([]jsonPathComponent, error) {
	if path == "" {
		return nil, badJSONPath(path)
	}
	if path[0] != '$' {
		return nil, badJSONPath(path)
	}
	var comps []jsonPathComponent
	i := 1
	for i < len(path) {
		switch path[i] {
		case '.':
			key, next, ok := parsePathKey(path, i+1)
			if !ok {
				return nil, badJSONPath(path)
			}
			comps = append(comps, jsonPathComponent{key: key})
			i = next
		case '[':
			idx, tail, next, ok := parsePathIndex(path, i+1)
			if !ok {
				return nil, badJSONPath(path)
			}
			comps = append(comps, jsonPathComponent{index: idx, isIdx: true, tail: tail})
			i = next
		default:
			return nil, badJSONPath(path)
		}
	}
	return comps, nil
}

// parsePathKey parses a .key or ."key" path step starting at i (after the
// dot). Returns the key, the index after the step, and ok=false on a
// malformed step.
func parsePathKey(path string, i int) (string, int, bool) {
	if i >= len(path) {
		return "", 0, false
	}
	if path[i] == '"' || path[i] == '\'' {
		// Quoted key: "..." or '...' (relaxed). Double-quoted keys decode
		// backslash escapes (\" \\ \uXXXX ...) so the decoded text compares
		// against raw stored labels.
		q := path[i]
		i++
		start := i
		for i < len(path) && path[i] != q {
			if path[i] == '\\' && q == '"' && i+1 < len(path) {
				i++
			}
			i++
		}
		if i >= len(path) {
			return "", 0, false
		}
		raw := path[start:i]
		if q == '\'' || !strings.ContainsRune(raw, '\\') {
			return raw, i + 1, true
		}
		return decodeJSONKeyEscapes(raw), i + 1, true
	}
	start := i
	for i < len(path) && path[i] != '.' && path[i] != '[' {
		i++
	}
	if i == start {
		return "", 0, false
	}
	// Bare keys compare RAW against stored labels: backslashes are literal.
	return path[start:i], i, true
}

// parsePathIndex parses a [N] or [#] / [#-N] / [#+N] path step starting at
// i (after the '['). Returns the index (for '#...' forms, the signed offset
// relative to the array length) with tail=true, the position after the ']',
// and ok=false on a malformed step.
func parsePathIndex(path string, i int) (idx int64, tail bool, next int, ok bool) {
	start := i
	for i < len(path) && path[i] != ']' {
		i++
	}
	if i >= len(path) {
		return 0, false, 0, false
	}
	body := path[start:i]
	if strings.HasPrefix(body, "#") {
		// Tail form: '#' (= 0) or '#-N'. SQLite rejects '#+N' (bad JSON path).
		off := int64(0)
		if len(body) > 1 {
			n, err := strconv.ParseInt(body[2:], 10, 64)
			if err != nil || body[1] != '-' {
				return 0, false, 0, false
			}
			off = -n
		}
		return off, true, i + 1, true
	}
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil || n < 0 {
		return 0, false, 0, false
	}
	return n, false, i + 1, true
}

// badJSONPath builds the SQLite "bad JSON path" error for a path string.
func badJSONPath(path string) error {
	return fmt.Errorf("bad JSON path: '%s'", path)
}

// jsonLookup walks comps from node, returning the node and whether it was
// found. A missing intermediate or terminal step returns (nil, false).
func jsonLookup(node *jsonNode, comps []jsonPathComponent) (*jsonNode, bool) {
	cur := node
	for _, c := range comps {
		if cur == nil {
			return nil, false
		}
		var ok bool
		if c.isIdx {
			cur, ok = jsonLookupArray(cur, c)
		} else {
			cur, ok = jsonLookupObject(cur, c)
		}
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// jsonLookupArray resolves one array-index path step.
func jsonLookupArray(cur *jsonNode, c jsonPathComponent) (*jsonNode, bool) {
	if cur.kind != jsonArray {
		return nil, false
	}
	idx := c.index
	if c.tail {
		// '#' index: relative to the array length ('#-1' = last element).
		idx += int64(len(cur.arr))
	}
	if idx < 0 || idx >= int64(len(cur.arr)) {
		return nil, false
	}
	return cur.arr[idx], true
}

// jsonLookupObject resolves one object-key path step.
func jsonLookupObject(cur *jsonNode, c jsonPathComponent) (*jsonNode, bool) {
	if cur.kind != jsonObject {
		return nil, false
	}
	for _, pr := range cur.obj {
		if pr.key == c.key {
			return pr.value, true
		}
	}
	return nil, false
}

// --- json_extract ---

func fnJSON_EXTRACT(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	// A NULL path argument makes json_extract return NULL (sqlite
	// jsonExtractFunc NULL-path rule).
	for _, a := range args[1:] {
		if a == nil {
			return nil, nil
		}
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	// A single argument validates the JSON and returns NULL (SQLite
	// json_extract(X) with no path returns NULL after parsing).
	if len(args) == 1 {
		return nil, nil
	}
	if len(args) == 2 {
		comps, err := parseJSONPath(toString(args[1]))
		if err != nil {
			return nil, err
		}
		node, ok := jsonLookup(root, comps)
		if !ok {
			return nil, nil
		}
		return jsonNodeToValue(node), nil
	}
	// Multiple paths: return a JSON array with one element per path
	// (missing paths become null), matching SQLite json_extract(X,P1,P2,...).
	nodes := make([]*jsonNode, 0, len(args)-1)
	for i := 1; i < len(args); i++ {
		comps, perr := parseJSONPath(toString(args[i]))
		if perr != nil {
			return nil, perr
		}
		if node, ok := jsonLookup(root, comps); ok {
			nodes = append(nodes, node)
		} else {
			nodes = append(nodes, &jsonNode{kind: jsonNull})
		}
	}
	return jsonSerialize(&jsonNode{kind: jsonArray, arr: nodes}), nil
}

// jsonNodeToValue maps a JSON node to a SQL value the way json_extract does:
// null → NULL, true/false → 1/0 (integers), numbers → INTEGER/REAL, strings →
// TEXT, objects/arrays → their serialized JSON text.
func jsonNodeToValue(n *jsonNode) interface{} {
	switch n.kind {
	case jsonNull:
		return nil
	case jsonTrue:
		return int64(1)
	case jsonFalse:
		return int64(0)
	case jsonNumber:
		if math.IsNaN(n.num) {
			return nil
		}
		if n.isInt {
			return n.i64
		}
		return n.num
	case jsonString:
		return n.str
	default:
		return JSONText(jsonSerialize(n))
	}
}

// --- JSON arrow operators ('->' and '->>') ---

// JSONArrowLookup resolves the abbreviated PATH accepted by the '->' and
// '->>' operators (SQLite jsonExtractFunc with the JSON_ABPATH flag):
// a full '$...' path, an INTEGER array index (negative counts from the
// array end, PostgreSQL-compatible), a bare alphanumeric label, an
// "[N]" index, or any other text as a quoted object member. Returns the
// located node; ok=false when the path does not match (SQL NULL result).
func JSONArrowLookup(j, path interface{}) (node *jsonNode, ok bool, err error) {
	if j == nil || path == nil {
		return nil, false, nil
	}
	root, err := parseJSONArg(j)
	if err != nil {
		return nil, false, err
	}
	comps, err := jsonArrowPath(path)
	if err != nil {
		return nil, false, err
	}
	node, ok = jsonLookup(root, comps)
	return node, ok, nil
}

// JSONArrowExtract implements J -> PATH: the subvalue serialized as JSON
// text (SQLite's JSON_JSON result), carrying the JSON subtype.
func JSONArrowExtract(j, path interface{}) (interface{}, error) {
	node, ok, err := JSONArrowLookup(j, path)
	if err != nil || !ok {
		return nil, err
	}
	return JSONText(jsonSerialize(node)), nil
}

// JSONArrowExtractSQL implements J ->> PATH: the subvalue converted to a
// plain SQL value — TEXT for strings, INTEGER/REAL for numbers, 1/0 for
// booleans, NULL for null (SQLite's JSON_SQL result).
func JSONArrowExtractSQL(j, path interface{}) (interface{}, error) {
	node, ok, err := JSONArrowLookup(j, path)
	if err != nil || !ok {
		return nil, err
	}
	v := jsonNodeToValue(node)
	// The JSON_SQL (->>) result carries NO subtype, even for containers:
	// arrays/objects come back as plain TEXT.
	if jt, isJT := v.(JSONText); isJT {
		return string(jt), nil
	}
	return v, nil
}

// jsonArrowPath converts one abbreviated operator path argument into lookup
// components, mirroring the path rewrite SQLite performs before
// jsonLookupStep (INTEGER → "$[N]" / "$[#N]", alphanum → "$.LABEL",
// "[N]" verbatim, anything else → "$.\"text\"").
func jsonArrowPath(path interface{}) ([]jsonPathComponent, error) {
	path = util.UnwrapColumnValue(path)
	text := toString(path)
	if strings.HasPrefix(text, "$") {
		return parseJSONPath(text)
	}
	if n, ok := path.(int64); ok {
		// INTEGER argument: an explicit array index (negative: from end).
		return []jsonPathComponent{{index: n, isIdx: true, tail: n < 0}}, nil
	}
	switch {
	case isAllAlnumOrUnderscore(text):
		return []jsonPathComponent{{key: text}}, nil
	case len(text) >= 3 && text[0] == '[' && text[len(text)-1] == ']':
		idx, err := strconv.ParseInt(text[1:len(text)-1], 10, 64)
		if err != nil {
			return nil, badJSONPath(text)
		}
		return []jsonPathComponent{{index: idx, isIdx: true}}, nil
	default:
		// Escape sequences (\xHH, \uXXXX, ...) resolve during comparison.
		if strings.ContainsRune(text, '\\') {
			return []jsonPathComponent{{key: decodeJSONKeyEscapes(text)}}, nil
		}
		return []jsonPathComponent{{key: text}}, nil
	}
}

// isAllAlnumOrUnderscore reports whether every byte is alphanumeric or '_'
// (SQLite's jsonAllAlphanum).
func isAllAlnumOrUnderscore(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'A' && c <= 'Z') &&
			!(c >= 'a' && c <= 'z') && c != '_' {
			return false
		}
	}
	return true
}

// --- json_insert ---

func fnJSON_INSERT(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	if len(args)%2 == 0 {
		return nil, fmt.Errorf("json_insert() needs an odd number of arguments")
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	// A single argument re-serializes the parsed JSON unchanged.
	if len(args) == 1 {
		return JSONText(jsonSerialize(root)), nil
	}
	for i := 1; i < len(args); i += 2 {
		if args[i] == nil {
			continue // sqlite jsonInsertFunc: a NULL path pair is skipped
		}
		comps, perr := parseJSONPath(toString(args[i]))
		if perr != nil {
			return nil, perr
		}
		val, verr := jsonInsertValue(args[i+1])
		if verr != nil {
			return nil, verr
		}
		jsonInsertAt(root, comps, val)
	}
	return JSONText(jsonSerialize(root)), nil
}

// jsonInsertValue converts a SQL value argument into a JSON node the way
// json_insert does: NULL → null, INTEGER → integer, REAL → real, TEXT → JSON
// string, BLOB → error.
func jsonInsertValue(v interface{}) (*jsonNode, error) {
	v = util.UnwrapColumnValue(v)
	if jt, ok := v.(JSONText); ok {
		// Returned-JSON subtype: embed as raw JSON (parse to normalize).
		if n, err := parseJSON(string(jt)); err == nil {
			return n, nil
		}
	}
	switch x := v.(type) {
	case nil:
		return &jsonNode{kind: jsonNull}, nil
	case int64:
		return &jsonNode{kind: jsonNumber, i64: x, isInt: true, text: strconv.FormatInt(x, 10)}, nil
	case float64:
		return &jsonNode{kind: jsonNumber, num: x, isInt: false, text: jsonNumberText(x)}, nil
	case string:
		return &jsonNode{kind: jsonString, str: x}, nil
	case []byte:
		// A BLOB carrying a JSONB document (sqlite jsonArgIsJsonb header
		// check) embeds as that JSON value; other blobs are rejected
		// (sqlite jsonParseFuncArg default case).
		if jsonbHeaderCheck(x) {
			return parseJSONArg(x)
		}
		return nil, fmt.Errorf("JSON cannot hold BLOB values")
	default:
		return &jsonNode{kind: jsonString, str: fmt.Sprintf("%v", x)}, nil
	}
}

// jsonNumberText renders a float64 the way current SQLite's json.c
// serializes a REAL value: NaN → "null", ±Infinity → ±"9.0e+999"
// (sqlite3's out-of-range sentinel rendering), else the SHORTEST decimal
// text that round-trips to the same double ("0.12345678901234568",
// "1e+99", "2.0"). Round-trip fidelity matters because json_extract/->>
// re-parse the serialized text: a %!.15g truncation would make
// json_array(0.1234567890123456789)->>0 differ from the original literal
// (json101-25.1).
func jsonNumberText(f float64) string {
	if math.IsNaN(f) {
		return "null"
	}
	if math.IsInf(f, 1) {
		return "9.0e+999"
	}
	if math.IsInf(f, -1) {
		return "-9.0e+999"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		mant := s[:e]
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + s[e:]
	}
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// jsonInsertAt inserts value at the path comps under root using json_insert
// semantics (insert only where the path does not already exist). The root
// node is mutated in place.
func jsonInsertAt(root *jsonNode, comps []jsonPathComponent, value *jsonNode) {
	cur := root
	for i, c := range comps {
		if i == len(comps)-1 {
			jsonInsertLeaf(cur, c, value)
			return
		}
		nxt, ok := jsonInsertStep(cur, c, comps[i+1])
		if !ok {
			return
		}
		cur = nxt
	}
}

// jsonInsertLeaf applies the terminal path step: append to an array at the
// exact next index, or append a missing object key. Existing values are left
// untouched (json_insert never overwrites).
func jsonInsertLeaf(cur *jsonNode, c jsonPathComponent, value *jsonNode) {
	if c.isIdx {
		if cur.kind != jsonArray {
			return
		}
		pos := c.index
		if c.tail {
			// '#' offsets may land anywhere inside the array.
			pos += int64(len(cur.arr))
			if pos >= 0 && pos <= int64(len(cur.arr)) {
				arr := append(cur.arr, nil)
				copy(arr[pos+1:], arr[pos:])
				arr[pos] = value
				cur.arr = arr
			}
			return
		}
		// Plain numeric indexes only ever APPEND (json_insert never moves
		// existing elements).
		if pos == int64(len(cur.arr)) {
			cur.arr = append(cur.arr, value)
		}
		return
	}
	if cur.kind == jsonObject && !jsonObjectHasKey(cur, c.key) {
		cur.obj = append(cur.obj, jsonPair{key: c.key, value: value})
	}
}

// jsonInsertStep advances through one intermediate path step, creating an
// empty container when the step is missing. Returns ok=false to abort the
// insert (the current node is not an object/array, or an array index is out
// of range).
func jsonInsertStep(cur *jsonNode, c jsonPathComponent, next jsonPathComponent) (*jsonNode, bool) {
	if c.isIdx {
		if cur.kind != jsonArray {
			return nil, false
		}
		if c.index < int64(len(cur.arr)) {
			return cur.arr[c.index], true
		}
		if c.index == int64(len(cur.arr)) {
			child := jsonNewContainer(next)
			cur.arr = append(cur.arr, child)
			return child, true
		}
		return nil, false
	}
	if cur.kind != jsonObject {
		return nil, false
	}
	if child, found := jsonObjectGet(cur, c.key); found {
		return child, true
	}
	child := jsonNewContainer(next)
	cur.obj = append(cur.obj, jsonPair{key: c.key, value: child})
	return child, true
}

// jsonNewContainer builds an empty object or array depending on the next path
// step (a [N] step creates an array, a .key step creates an object).
func jsonNewContainer(next jsonPathComponent) *jsonNode {
	if next.isIdx {
		return &jsonNode{kind: jsonArray}
	}
	return &jsonNode{kind: jsonObject}
}

func jsonObjectHasKey(o *jsonNode, key string) bool {
	for _, pr := range o.obj {
		if pr.key == key {
			return true
		}
	}
	return false
}

func jsonObjectGet(o *jsonNode, key string) (*jsonNode, bool) {
	for _, pr := range o.obj {
		if pr.key == key {
			return pr.value, true
		}
	}
	return nil, false
}

// JSONText marks a Go string as already-valid JSON (SQLite's returned-JSON
// subtype): when such a value is used as the VALUE argument of
// json_insert/set/replace/array/object, it is embedded as raw JSON rather
// than quoted as a text string (sqlite3_value_subtype SQLITE_RETURNED_JSON).
type JSONText string

func (j JSONText) String() string { return string(j) }

// CarrierText exposes the underlying TEXT payload to lower-layer comparison
// code (internal/value) without an import: JSON values carry the JSON
// subtype as Go type metadata but compare as ordinary TEXT.
func (j JSONText) CarrierText() string { return string(j) }

// --- Serialization ---

// jsonSerialize renders a JSON node tree as compact JSON text, matching
// SQLite's jsonReturning: keys and strings JSON-escaped, numbers re-emitted
// from their stored text.
func jsonSerialize(n *jsonNode) string {
	var sb strings.Builder
	jsonSerializeInto(&sb, n)
	return sb.String()
}

func jsonSerializeInto(sb *strings.Builder, n *jsonNode) {
	switch n.kind {
	case jsonNull:
		sb.WriteString("null")
	case jsonTrue:
		sb.WriteString("true")
	case jsonFalse:
		sb.WriteString("false")
	case jsonNumber:
		jsonSerializeNumber(sb, n)
	case jsonString:
		jsonEscapeString(sb, n.str)
	case jsonObject:
		sb.WriteByte('{')
		for i, pr := range n.obj {
			if i > 0 {
				sb.WriteByte(',')
			}
			jsonEscapeString(sb, pr.key)
			sb.WriteByte(':')
			jsonSerializeInto(sb, pr.value)
		}
		sb.WriteByte('}')
	case jsonArray:
		sb.WriteByte('[')
		for i, v := range n.arr {
			if i > 0 {
				sb.WriteByte(',')
			}
			jsonSerializeInto(sb, v)
		}
		sb.WriteByte(']')
	}
}

// jsonSerializeNumber writes a number node: its stored serialized text when
// present (parsed numbers keep their normalized original text), NaN as null,
// integers as decimal, and reals via SQLite's %.15g rendering.
func jsonSerializeNumber(sb *strings.Builder, n *jsonNode) {
	if n.text != "" {
		sb.WriteString(n.text)
		return
	}
	if math.IsNaN(n.num) {
		sb.WriteString("null")
		return
	}
	if n.isInt {
		sb.WriteString(strconv.FormatInt(n.i64, 10))
		return
	}
	sb.WriteString(jsonNumberText(n.num))
}

// jsonEscapeString writes s as a JSON string literal.
func jsonEscapeString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

// --- json_set / json_replace / json_remove / json_valid / json_type /
//     json_quote / json_array_length / json_group_array / json_group_object ---

// fnJSON_SET implements json_set(X,P,V,...): existing values are overwritten,
// missing intermediate containers are created (json_insert's overwrite
// sibling — sqlite3_json_set in json.c).
func fnJSON_SET(args []interface{}) (interface{}, error) {
	return jsonEditPaths(args, jsonEditSet)
}

// fnJSON_REPLACE implements json_replace(X,P,V,...): only paths that already
// resolve are overwritten; missing paths are ignored.
func fnJSON_REPLACE(args []interface{}) (interface{}, error) {
	return jsonEditPaths(args, jsonEditReplace)
}

type jsonEditMode int

const (
	jsonEditSet jsonEditMode = iota
	jsonEditReplace
)

// jsonEditPaths is the shared body of json_set/json_replace.
func jsonEditPaths(args []interface{}, mode jsonEditMode) (interface{}, error) {
	name := "json_set"
	if mode == jsonEditReplace {
		name = "json_replace"
	}
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	if len(args)%2 == 0 {
		return nil, fmt.Errorf("%s() needs an odd number of arguments", name)
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	// sqlite json.c jsonInsertIntoBlob: a NULL path argument SKIPS its pair
	// only; later pairs still apply (oracle: json_set('{"a":1}',NULL,9,'$.b',2)
	// → {"a":1,"b":2}). Only json_remove/json_extract return NULL for NULL
	// paths; set/replace/insert ignore the pair.
	for i := 1; i < len(args); i += 2 {
		if args[i] == nil {
			continue
		}
		comps, perr := parseJSONPath(toString(args[i]))
		if perr != nil {
			return nil, perr
		}
		val, verr := jsonInsertValue(args[i+1])
		if verr != nil {
			return nil, verr
		}
		if mode == jsonEditSet {
			jsonSetAt(root, comps, val)
		} else {
			jsonReplaceAt(root, comps, val)
		}
	}
	return JSONText(jsonSerialize(root)), nil
}

// jsonSetAt applies the terminal path step with overwrite semantics.
func jsonSetAt(root *jsonNode, comps []jsonPathComponent, value *jsonNode) {
	cur := root
	for i, c := range comps {
		if i == len(comps)-1 {
			jsonSetLeaf(cur, c, value)
			return
		}
		nxt, ok := jsonInsertStep(cur, c, comps[i+1])
		if !ok {
			return
		}
		cur = nxt
	}
}

func jsonSetLeaf(cur *jsonNode, c jsonPathComponent, value *jsonNode) {
	if c.isIdx {
		if cur.kind != jsonArray {
			return
		}
		pos := c.index
		if c.tail {
			pos += int64(len(cur.arr))
		}
		switch {
		case pos >= 0 && pos < int64(len(cur.arr)):
			cur.arr[pos] = value
		case pos == int64(len(cur.arr)):
			cur.arr = append(cur.arr, value)
		}
		return
	}
	if cur.kind == jsonObject {
		for i := range cur.obj {
			if cur.obj[i].key == c.key {
				cur.obj[i].value = value
				return
			}
		}
		cur.obj = append(cur.obj, jsonPair{key: c.key, value: value})
	}
}

// jsonReplaceAt overwrites the terminal path step only when it already
// exists; otherwise the path is ignored.
func jsonReplaceAt(root *jsonNode, comps []jsonPathComponent, value *jsonNode) {
	cur := root
	for i, c := range comps {
		if i == len(comps)-1 {
			jsonReplaceLeaf(cur, c, value)
			return
		}
		nxt, ok := jsonReplaceStep(cur, c)
		if !ok {
			return
		}
		cur = nxt
	}
}

func jsonReplaceStep(cur *jsonNode, c jsonPathComponent) (*jsonNode, bool) {
	if c.isIdx {
		if cur.kind != jsonArray {
			return nil, false
		}
		pos := c.index
		if c.tail {
			pos += int64(len(cur.arr))
		}
		if pos < 0 || pos >= int64(len(cur.arr)) {
			return nil, false
		}
		return cur.arr[pos], true
	}
	if cur.kind != jsonObject {
		return nil, false
	}
	if child, found := jsonObjectGet(cur, c.key); found {
		return child, true
	}
	return nil, false
}

func jsonReplaceLeaf(cur *jsonNode, c jsonPathComponent, value *jsonNode) {
	if c.isIdx {
		if cur.kind == jsonArray {
			pos := c.index
			if c.tail {
				pos += int64(len(cur.arr))
			}
			if pos >= 0 && pos < int64(len(cur.arr)) {
				cur.arr[pos] = value
			}
		}
		return
	}
	if cur.kind == jsonObject {
		for i := range cur.obj {
			if cur.obj[i].key == c.key {
				cur.obj[i].value = value
				return
			}
		}
	}
}

// fnJSON_ARRAY_INSERT implements json_array_insert(J,P,V,...): inserts V
// into the array at path P (2026 sqlite). The final path component must be
// an array-element specifier ([N] or [#±N]); '#' is the array end and '#-N'
// counts back from it. Missing intermediate containers are created like
// json_insert; a resolved parent that is not an array makes the pair a
// silent no-op, while a non-array-specifier final component raises
// "not an array element".
func fnJSON_ARRAY_INSERT(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	if len(args)%2 != 1 {
		return nil, fmt.Errorf("json_array_insert() needs an odd number of arguments")
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(args); i += 2 {
		path := toString(args[i])
		comps, perr := parseJSONPath(path)
		if perr != nil {
			// An unterminated final "[.." specifier is reported as a
			// non-array-element target rather than a bad path.
			if k := strings.LastIndexByte(path, '['); k >= 0 && !strings.Contains(path[k:], "]") {
				return nil, fmt.Errorf("not an array element: '%s'", path)
			}
			return nil, badJSONPath(path)
		}
		if len(comps) == 0 || !comps[len(comps)-1].isIdx {
			return nil, fmt.Errorf("not an array element: '%s'", path)
		}
		val, verr := jsonInsertValue(args[i+1])
		if verr != nil {
			return nil, verr
		}
		// Resolve the parent container, creating missing intermediates.
		cur := root
		alive := true
		for j := 0; j < len(comps)-1 && alive; j++ {
			next := comps[j+1]
			nxt, ok := jsonInsertStep(cur, comps[j], next)
			if !ok {
				alive = false
				break
			}
			cur = nxt
		}
		if !alive || cur.kind != jsonArray {
			continue // silent no-op (e.g. '$[0]' against an object root)
		}
		last := comps[len(comps)-1]
		pos := last.index
		if last.tail {
			pos = int64(len(cur.arr)) + last.index
		}
		if pos < 0 || pos > int64(len(cur.arr)) {
			continue // out-of-range insert is ignored
		}
		arr := append(cur.arr, nil)
		copy(arr[pos+1:], arr[pos:])
		arr[pos] = val
		cur.arr = arr
	}
	return JSONText(jsonSerialize(root)), nil
}

// fnJSON_REMOVE implements json_remove(X,P,...): each path whose full chain
// resolves has its final component deleted.
func fnJSON_REMOVE(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	// A NULL path argument makes json_remove return NULL (sqlite
	// jsonRemoveFunc NULL-path rule).
	for _, a := range args[1:] {
		if a == nil {
			return nil, nil
		}
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(args); i++ {
		comps, perr := parseJSONPath(toString(args[i]))
		if perr != nil {
			return nil, perr
		}
		if len(comps) == 0 {
			// Removing the root ('$') deletes the entire document.
			return nil, nil
		}
		jsonRemoveAt(root, comps)
	}
	return JSONText(jsonSerialize(root)), nil
}

func jsonRemoveAt(root *jsonNode, comps []jsonPathComponent) {
	cur := root
	for i, c := range comps {
		if i == len(comps)-1 {
			if c.isIdx {
				if cur.kind != jsonArray {
					return
				}
				pos := c.index
				if c.tail {
					pos += int64(len(cur.arr))
				}
				if pos >= 0 && pos < int64(len(cur.arr)) {
					cur.arr = append(cur.arr[:pos], cur.arr[pos+1:]...)
				}
				return
			}
			if cur.kind == jsonObject {
				for j := range cur.obj {
					if cur.obj[j].key == c.key {
						cur.obj = append(cur.obj[:j], cur.obj[j+1:]...)
						return
					}
				}
			}
			return
		}
		var nxt *jsonNode
		var ok bool
		if c.isIdx {
			if cur.kind == jsonArray {
				pos := c.index
				if c.tail {
					pos += int64(len(cur.arr))
				}
				if pos >= 0 && pos < int64(len(cur.arr)) {
					nxt, ok = cur.arr[pos], true
				}
			}
		} else if cur.kind == jsonObject {
			nxt, ok = jsonObjectGet(cur, c.key)
		}
		if !ok {
			return
		}
		cur = nxt
	}
}

// fnJSON_VALID implements json_valid(X[,F]): 1 when X parses as well-formed
// JSON. Without a flag (or F != 5) the check is strict RFC-8259: JSON5
// extensions (trailing commas, unquoted/single-quoted keys, hex numbers,
// leading '+', bare '.', Infinity/NaN keywords) are rejected even though
// the lenient parser accepts them — matching SQLite, where other functions
// accept those forms but json_valid reports them invalid. With F=5 (the
// SQLite JSON5 flag) the lenient parse result is authoritative.
func fnJSON_VALID(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	// Flag bitmask (src/json.c jsonValidFunc): 0x01 accepts strict
	// RFC-8259 JSON, 0x02 accepts JSON5 extensions, 0x04/0x08 control BLOB
	// (JSONB) checking depth.
	flags := 1
	if len(args) > 1 {
		f, ok := args[1].(int64)
		if !ok || f < 1 || f > 15 {
			return nil, fmt.Errorf("FLAGS parameter to json_valid() must be between 1 and 15")
		}
		flags = int(f)
	}
	v := util.UnwrapColumnValue(args[0])
	if b, ok := v.([]byte); ok && (flags&0x0c) != 0 {
		// BLOB checking flags (src/json.c jsonValidFunc): 0x04 = superficial
		// header check (sqlite jsonArgIsJsonb), 0x08 = full structural
		// validity — a corrupt tail (label without value, json101-26.2)
		// fails 0x08 but may pass 0x04. Either way the blob is NOT
		// re-interpreted as TEXT when a BLOB flag is set.
		if flags&0x04 != 0 {
			return boolToInt64(jsonbHeaderCheck(b)), nil
		}
		return boolToInt64(isJSONBBlob(b)), nil
	}
	src, err := jsonArgText(v)
	if err != nil {
		return int64(0), nil
	}
	if flags&0x03 == 0 {
		return int64(0), nil
	}
	if _, err := parseJSON(src); err != nil {
		return int64(0), nil
	}
	if flags&0x02 != 0 {
		// JSON5 extensions accepted: any successful lenient parse is valid.
		return int64(1), nil
	}
	return boolToInt64(isStrictJSON(src)), nil
}

// fnJSON_ERROR_POSITION implements json_error_position(X): 0 when X parses
// (with the lenient parser, matching SQLite), otherwise the 1-based byte
// offset where parsing stopped — SQLite's jsonParse position semantics.
func fnJSON_ERROR_POSITION(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("json_error_position() needs exactly one argument")
	}
	if args[0] == nil {
		return nil, nil // sqlite: json_error_position(NULL) is SQL NULL
	}
	p := &jsonParser{src: toString(util.UnwrapColumnValue(args[0]))}
	p.skipWS()
	if _, err := p.parseValue(); err != nil {
		return int64(p.errPos + 1), nil
	}
	p.skipWS()
	if p.pos != len(p.src) {
		return int64(p.pos + 1), nil
	}
	return int64(0), nil
}

// isStrictJSON reports whether src is exactly RFC-8259 JSON (no JSON5
// extensions): whitespace limited to space/tab/LF/CR, double-quoted strings,
// unadorned numbers, no trailing commas.
func isStrictJSON(src string) bool {
	s := &strictScanner{src: src}
	s.skipWS()
	if err := s.value(); err != nil {
		return false
	}
	s.skipWS()
	return s.pos == len(s.src)
}

// strictScanner checks one JSON document against the strict grammar.
type strictScanner struct {
	src   string
	pos   int
	depth int
}

func (s *strictScanner) peek() byte {
	if s.pos < len(s.src) {
		return s.src[s.pos]
	}
	return 0
}

func (s *strictScanner) skipWS() {
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *strictScanner) value() error {
	switch c := s.peek(); {
	case c == '{':
		return s.nested(s.object)
	case c == '[':
		return s.nested(s.array)
	case c == '"':
		return s.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return s.number()
	default:
		for kw, n := range map[string]int{"true": 4, "false": 5, "null": 4} {
			if strings.HasPrefix(s.src[s.pos:], kw) {
				s.pos += n
				return nil
			}
		}
		return fmt.Errorf("unexpected character %q", c)
	}
}

// nested runs fn with the container depth incremented, rejecting documents
// nested deeper than SQLite's JSON_MAX_DEPTH (json_valid parity).
func (s *strictScanner) nested(fn func() error) error {
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > jsonMaxDepth {
		return fmt.Errorf("too deep")
	}
	return fn()
}

func (s *strictScanner) object() error {
	s.pos++ // '{'
	s.skipWS()
	if s.peek() == '}' {
		s.pos++
		return nil
	}
	for {
		s.skipWS()
		if s.peek() != '"' {
			return fmt.Errorf("object key must be a double-quoted string")
		}
		if err := s.str(); err != nil {
			return err
		}
		s.skipWS()
		if s.peek() != ':' {
			return fmt.Errorf("expected ':'")
		}
		s.pos++
		s.skipWS()
		if err := s.value(); err != nil {
			return err
		}
		s.skipWS()
		switch s.peek() {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return nil
		default:
			return fmt.Errorf("expected ',' or '}'")
		}
	}
}

func (s *strictScanner) array() error {
	s.pos++ // '['
	s.skipWS()
	if s.peek() == ']' {
		s.pos++
		return nil
	}
	for {
		s.skipWS()
		if err := s.value(); err != nil {
			return err
		}
		s.skipWS()
		switch s.peek() {
		case ',':
			s.pos++
		case ']':
			s.pos++
			return nil
		default:
			return fmt.Errorf("expected ',' or ']'")
		}
	}
}

func (s *strictScanner) str() error {
	s.pos++ // opening '"'
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch c {
		case '\\':
			s.pos++
			if s.pos >= len(s.src) {
				return fmt.Errorf("unterminated escape")
			}
			// RFC-8259 allows only these escapes (plus \uXXXX).
			switch e := s.src[s.pos]; e {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if s.pos+4 >= len(s.src) {
					return fmt.Errorf("malformed \\u escape")
				}
				for k := 1; k <= 4; k++ {
					h := s.src[s.pos+k]
					if !isHexDigitByte(h) {
						return fmt.Errorf("malformed \\u escape")
					}
				}
				s.pos += 4
			default:
				return fmt.Errorf("invalid escape character %q", e)
			}
			s.pos++
		case '"':
			s.pos++
			return nil
		default:
			if c < 0x20 {
				return fmt.Errorf("unescaped control character in string")
			}
			s.pos++
		}
	}
	return fmt.Errorf("unterminated string")
}

func isHexDigitByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (s *strictScanner) number() error {
	if s.peek() == '-' {
		s.pos++
	}
	digitsStart := s.pos
	for isJSONDigit(s.peek()) {
		s.pos++
	}
	if s.pos == digitsStart {
		return fmt.Errorf("malformed number")
	}
	// Leading zeros are not allowed ("00", "01").
	if s.src[digitsStart] == '0' && s.pos-digitsStart > 1 {
		return fmt.Errorf("leading zero in number")
	}
	if s.peek() == '.' {
		s.pos++
		frac := s.pos
		for isJSONDigit(s.peek()) {
			s.pos++
		}
		if s.pos == frac {
			return fmt.Errorf("malformed fraction")
		}
	}
	if c := s.peek(); c == 'e' || c == 'E' {
		s.pos++
		if c := s.peek(); c == '+' || c == '-' {
			s.pos++
		}
		exp := s.pos
		for isJSONDigit(s.peek()) {
			s.pos++
		}
		if s.pos == exp {
			return fmt.Errorf("malformed exponent")
		}
	}
	return nil
}

func isJSONDigit(c byte) bool { return c >= '0' && c <= '9' }

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// fnJSON_TYPE implements json_type(X[,P]): the JSON type name of the value,
// or of X itself when P is omitted.
func fnJSON_TYPE(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	node := root
	if len(args) > 1 {
		// sqlite jsonTypeFunc: a NULL path yields SQL NULL (no result is set).
		if args[1] == nil {
			return nil, nil
		}
		comps, perr := parseJSONPath(toString(args[1]))
		if perr != nil {
			return nil, perr
		}
		found, ok := jsonLookup(root, comps)
		if !ok {
			return nil, nil
		}
		node = found
	}
	switch node.kind {
	case jsonObject:
		return "object", nil
	case jsonArray:
		return "array", nil
	case jsonTrue:
		return "true", nil
	case jsonFalse:
		return "false", nil
	case jsonNull:
		return "null", nil
	case jsonString:
		return "text", nil
	case jsonNumber:
		if node.isInt {
			return "integer", nil
		}
		return "real", nil
	default:
		return nil, nil
	}
}

// fnJSON_QUOTE implements json_quote(V): the JSON representation of an SQL
// value (numbers verbatim, text quoted and escaped, NULL → null).
func fnJSON_QUOTE(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return "null", nil
	}
	v, err := jsonInsertValue(args[0])
	if err != nil {
		return nil, err
	}
	return jsonSerialize(v), nil
}

// fnJSON_ARRAY_LENGTH implements json_array_length(X[,P]): the number of
// elements of the array at P (or of X); a non-array value counts as 0.
func fnJSON_ARRAY_LENGTH(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	node := root
	if len(args) > 1 {
		// sqlite jsonArrayLengthFunc: a NULL path yields SQL NULL.
		if args[1] == nil {
			return nil, nil
		}
		comps, perr := parseJSONPath(toString(args[1]))
		if perr != nil {
			return nil, perr
		}
		found, ok := jsonLookup(root, comps)
		if !ok {
			return nil, nil
		}
		node = found
	}
	if node.kind != jsonArray {
		return int64(0), nil
	}
	return int64(len(node.arr)), nil
}

// jsonGroupArrayAgg implements json_group_array(V): a JSON array of every
// stepped value.
type jsonGroupArrayAgg struct {
	items []*jsonNode
}

func (a *jsonGroupArrayAgg) Step(args []interface{}) error {
	v, err := jsonInsertValue(args[0])
	if err != nil {
		return err
	}
	a.items = append(a.items, v)
	return nil
}

func (a *jsonGroupArrayAgg) Final() (interface{}, error) {
	n := &jsonNode{kind: jsonArray, arr: a.items}
	return JSONText(jsonSerialize(n)), nil
}

// jsonGroupObjectAgg implements json_group_object(K,V): a JSON object built
// from key/value argument pairs.
type jsonGroupObjectAgg struct {
	pairs []jsonPair
}

func (a *jsonGroupObjectAgg) Step(args []interface{}) error {
	if len(args) < 2 {
		return fmt.Errorf("json_group_object() needs two arguments")
	}
	key := fmt.Sprint(args[0])
	v, err := jsonInsertValue(args[1])
	if err != nil {
		return err
	}
	a.pairs = append(a.pairs, jsonPair{key: key, value: v})
	return nil
}

func (a *jsonGroupObjectAgg) Final() (interface{}, error) {
	n := &jsonNode{kind: jsonObject, obj: a.pairs}
	return JSONText(jsonSerialize(n)), nil
}

// fnJSON_PRETTY implements json_pretty(JSON[,INDENT]) (json.c jsonPrettyFunc):
// pretty-printed rendering of the input JSON; invalid JSON returns NULL.
// INDENT defaults to four spaces.
func fnJSON_PRETTY(args []interface{}) (interface{}, error) {
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
	root, err := parseJSONArg(args[0])
	if err != nil {
		return nil, nil // json_pretty returns NULL for invalid JSON
	}
	indent := "    "
	if len(args) > 1 && args[1] != nil {
		if s, ok := util.UnwrapColumnValue(args[1]).(string); ok {
			indent = s
		} else {
			indent = toString(args[1])
		}
	}
	var sb strings.Builder
	jsonPrettyNode(&sb, root, indent, 0)
	return JSONText(sb.String()), nil
}

func jsonPrettyIndent(sb *strings.Builder, indent string, depth int) {
	sb.WriteString("\n")
	for i := 0; i < depth; i++ {
		sb.WriteString(indent)
	}
}

func jsonPrettyNode(sb *strings.Builder, n *jsonNode, indent string, depth int) {
	switch n.kind {
	case jsonObject:
		if len(n.obj) == 0 {
			sb.WriteString("{}")
			return
		}
		sb.WriteString("{")
		for i, pr := range n.obj {
			if i > 0 {
				sb.WriteString(",")
			}
			jsonPrettyIndent(sb, indent, depth+1)
			sb.WriteString(jsonQuoteString(pr.key))
			sb.WriteString(" : ")
			jsonPrettyNode(sb, pr.value, indent, depth+1)
		}
		jsonPrettyIndent(sb, indent, depth)
		sb.WriteString("}")
	case jsonArray:
		if len(n.arr) == 0 {
			sb.WriteString("[]")
			return
		}
		sb.WriteString("[")
		for i, el := range n.arr {
			if i > 0 {
				sb.WriteString(",")
			}
			jsonPrettyIndent(sb, indent, depth+1)
			jsonPrettyNode(sb, el, indent, depth+1)
		}
		jsonPrettyIndent(sb, indent, depth)
		sb.WriteString("]")
	default:
		sb.WriteString(jsonSerialize(n))
	}
}

// jsonQuoteString renders a Go string as a quoted JSON string literal.
func jsonQuoteString(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// fnJSON_PATCH implements json_patch(T,P): RFC-7396 merge-patch applied
// recursively (json.c jsonPatch; json106/json104 suites).
func fnJSON_PATCH(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("json_patch() needs two arguments")
	}
	if args[0] == nil {
		return nil, nil // patching a NULL target yields SQL NULL
	}
	target, err := parseJSON(toString(args[0]))
	if err != nil {
		return nil, err
	}
	if args[1] == nil {
		return nil, nil // a NULL patch yields SQL NULL
	}
	patch, err := parseJSON(toString(args[1]))
	if err != nil {
		return nil, err
	}
	// RFC-7396 via src/json.c jsonPatchFunc: a non-object patch replaces
	// the result wholesale; an object patch applied to a non-object target
	// behaves as if the target were an empty object.
	if patch.kind != jsonObject {
		return JSONText(jsonSerialize(patch)), nil
	}
	if target.kind != jsonObject {
		target = &jsonNode{kind: jsonObject}
	}
	jsonApplyPatch(target, patch)
	return JSONText(jsonSerialize(target)), nil
}

// jsonApplyPatch applies one merge-patch object level onto target.
func jsonApplyPatch(target, patch *jsonNode) {
	if patch.kind != jsonObject {
		return // non-object patch replaces wholesale (handled by caller)
	}
	if target.kind != jsonObject {
		*target = *patch
		return
	}
	for _, pr := range patch.obj {
		if pr.value == nil || pr.value.kind == jsonNull {
			jsonObjectRemoveKey(target, pr.key)
			continue
		}
		existing, found := jsonObjectGet(target, pr.key)
		switch {
		case found && existing.kind == jsonObject && pr.value.kind == jsonObject:
			// RFC-7396: object members merge recursively.
			jsonApplyPatch(existing, pr.value)
		case pr.value.kind == jsonObject:
			// Merge into a fresh object so null leaves disappear
			// (json104-220: {"a":{"bb":{"ccc":null}}} -> {"a":{"bb":{}}}).
			newObj := &jsonNode{kind: jsonObject}
			jsonApplyPatch(newObj, pr.value)
			jsonUpsertMember(target, pr.key, newObj)
		case found:
			// Replace the existing member in place (no duplicates).
			for i := range target.obj {
				if target.obj[i].key == pr.key {
					target.obj[i].value = pr.value
					break
				}
			}
		default:
			target.obj = append(target.obj, jsonPair{key: pr.key, value: pr.value})
		}
	}
}

// jsonUpsertMember sets the value of key, replacing in place when present.
func jsonUpsertMember(o *jsonNode, key string, value *jsonNode) {
	for i := range o.obj {
		if o.obj[i].key == key {
			o.obj[i].value = value
			return
		}
	}
	o.obj = append(o.obj, jsonPair{key: key, value: value})
}

func jsonObjectRemoveKey(o *jsonNode, key string) {
	for i := range o.obj {
		if o.obj[i].key == key {
			o.obj = append(o.obj[:i], o.obj[i+1:]...)
			return
		}
	}
}
