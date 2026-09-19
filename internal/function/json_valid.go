// JSON1 validation and typing: json_valid, json_error_position, the strict
// (RFC 8259) scanner used by json_valid, and json_type.

package function

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

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
