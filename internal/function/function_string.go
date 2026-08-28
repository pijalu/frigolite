package function

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/value"
)

func fnUPPER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite's built-in upper() folds ASCII only; non-ASCII characters are
	// left unchanged (no ICU Unicode case mapping).
	return toUpperASCII(toString(args[0])), nil
}

func fnLOWER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return toLowerASCII(toString(args[0])), nil
}

// toUpperASCII uppercases only the ASCII letters a-z, leaving every other
// byte (including multi-byte UTF-8 sequences) untouched.
func toUpperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// toLowerASCII lowercases only the ASCII letters A-Z.
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

func fnLENGTH(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite length(): character count for text, byte count for blobs, and
	// the length of the text form for numbers.
	switch v := args[0].(type) {
	case []byte:
		return int64(len(v)), nil
	case value.ZeroBlob:
		return int64(v.N), nil // blob byte count without materializing
	default:
		return int64(utf8CharLen(toString(args[0]))), nil
	}
}

// fnOCTETLENGTH returns the number of bytes in the argument. For text values
// this is the UTF-8 byte length; for numbers the byte length of their text
// representation (SQLite octet_length semantics). NULL returns NULL.
func fnOCTETLENGTH(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case []byte:
		return int64(len(v)), nil
	default:
		return int64(len(toString(args[0]))), nil
	}
}

func fnTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return sqliteTrim(toString(args[0]), toString(args[1]), "both"), nil
	}
	return strings.TrimSpace(toString(args[0])), nil
}

func fnLTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return sqliteTrim(toString(args[0]), toString(args[1]), "left"), nil
	}
	return strings.TrimLeft(toString(args[0]), " \t\n\r"), nil
}

func fnRTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return sqliteTrim(toString(args[0]), toString(args[1]), "right"), nil
	}
	return strings.TrimRight(toString(args[0]), " \t\n\r"), nil
}

// sqliteTrim trims characters from the trim set off the input string,
// matching SQLite's trim semantics for possibly-invalid UTF-8 (badutf.test).
// The set is split into character byte-sequences (lead byte + continuation
// run); the input is trimmed by repeatedly removing a set character's byte
// sequence from the start (leading) or end (trailing). This is byte-level
// comparison — invalid UTF-8 bytes are preserved and matched as-is.
func sqliteTrim(s, set, mode string) string {
	if set == "" {
		return s
	}
	setChars := splitUTF8Chars(set)
	start := 0
	end := len(s)
	if mode == "both" || mode == "left" {
		for start < end {
			matched := false
			for _, c := range setChars {
				if strings.HasPrefix(s[start:end], string(c)) {
					start += len(c)
					matched = true
					break
				}
			}
			if !matched {
				break
			}
		}
	}
	if mode == "both" || mode == "right" {
		for end > start {
			matched := false
			for _, c := range setChars {
				if strings.HasSuffix(s[start:end], string(c)) {
					end -= len(c)
					matched = true
					break
				}
			}
			if !matched {
				break
			}
		}
	}
	return s[start:end]
}

// splitUTF8Chars splits a string into its character byte-sequences (lead byte
// plus continuation run), preserving raw bytes for invalid UTF-8.
func splitUTF8Chars(s string) [][]byte {
	var chars [][]byte
	for i := 0; i < len(s); {
		c := nextUTF8Char(s, i)
		chars = append(chars, []byte(c))
		i += len(c)
	}
	return chars
}

// nextUTF8Char returns the byte-sequence of the character starting at i.
func nextUTF8Char(s string, i int) string {
	if i >= len(s) {
		return ""
	}
	j := i + 1
	if s[i]&0xC0 == 0xC0 {
		for j < len(s) && s[j]&0xC0 == 0x80 {
			j++
		}
	}
	return s[i:j]
}

func fnSUBSTR(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	// A NULL length argument yields NULL (SQLite substrFunc checks the
	// argument TYPE, not just the numeric value).
	if len(args) > 2 && args[2] == nil {
		return nil, nil
	}
	p1 := toInt64(args[1])
	if b, ok := args[0].([]byte); ok {
		// Blob: byte-based offsets, result stays a blob. As with text, the
		// two-argument default length is SQLite's huge LIMIT_LENGTH so that
		// a start pushed before the beginning returns the whole blob.
		n := int64(len(b))
		p2 := int64(1000000000) // SQLITE_LIMIT_LENGTH default
		if len(args) > 2 {
			p2 = toInt64(args[2])
		}
		start, length := sqliteSubstrBounds(n, p1, p2)
		if length <= 0 || start >= n {
			return []byte{}, nil
		}
		if start+length > n {
			length = n - start
		}
		return b[start : start+length], nil
	}
	// Text: character-based offsets. With two arguments SQLite uses a huge
	// default length (SQLITE_LIMIT_LENGTH), which matters when a negative
	// start pushes p1 before the beginning: substr('abcdefg',-100) returns
	// the whole string.
	s := toString(args[0])
	nChars := int64(utf8CharLen(s))
	p2 := int64(1000000000) // SQLITE_LIMIT_LENGTH default
	if len(args) > 2 {
		p2 = toInt64(args[2])
	}
	start, length := sqliteSubstrBounds(nChars, p1, p2)
	byteStart := charOffsetToByte(s, start)
	byteEnd := charOffsetToByte(s, start+length)
	if byteEnd < byteStart {
		byteEnd = byteStart
	}
	return s[byteStart:byteEnd], nil
}

// sqliteSubstrBounds computes the 0-based (start, length) pair for SQLite's
// 1-based substr(X, p1[, p2]) over a sequence of total units (bytes for
// blobs, characters for text). It mirrors src/func.c substrFunc: a negative
// p1 counts back from the end, p1==0 with p2>0 consumes one unit of length,
// and a negative p2 returns the |p2| units preceding p1.
func sqliteSubstrBounds(total, p1, p2 int64) (int64, int64) {
	if p1 < 0 {
		p1 += total
		if p1 < 0 {
			if p2 < 0 {
				p2 = 0
			} else {
				p2 += p1
			}
			p1 = 0
		}
	} else if p1 > 0 {
		p1--
	} else if p2 > 0 {
		p2--
	}
	if p2 < 0 {
		if p2 < -p1 {
			p2 = p1
		} else {
			p2 = -p2
		}
		p1 -= p2
	}
	if p1 < 0 {
		p1 = 0
	}
	if p2 < 0 {
		p2 = 0
	}
	return p1, p2
}

// utf8CharLen counts the characters in s using SQLite's length() character
// counting rule (the SQLITE_SKIP_UTF8 macro): a byte with bit 7 set
// (0x80-0xFF) begins a character that swallows all following continuation
// bytes (0x80-0xBF); all other bytes are single characters. This matches the
// TCL test-suite expectations for invalid UTF-8 (badutf.test: 10×0x80 is one
// character, %7f%80%81 is two).
func utf8CharLen(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n++
		if s[i]&0x80 != 0 {
			// Non-ASCII byte: consume the whole continuation run.
			i++
			for i < len(s) && s[i]&0xC0 == 0x80 {
				i++
			}
			i--
		}
	}
	return n
}

// charOffsetToByte returns the byte offset in s after n characters, walking
// whole UTF-8 characters (clamped to the end of s). A non-ASCII byte
// (0x80-0xFF) swallows its following continuation run, matching utf8CharLen.
func charOffsetToByte(s string, n int64) int {
	i := 0
	for c := int64(0); c < n && i < len(s); c++ {
		if s[i]&0x80 != 0 {
			i++
			for i < len(s) && s[i]&0xC0 == 0x80 {
				i++
			}
		} else {
			i++
		}
	}
	return i
}

func fnREPLACE(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil || args[2] == nil {
		return nil, nil
	}
	s := toString(args[0])
	old := toString(args[1])
	new := toString(args[2])
	if old == "" {
		// SQLite: an empty find string returns the original unchanged.
		return s, nil
	}
	return strings.ReplaceAll(s, old, new), nil
}

func fnINSTR(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	// Mirror sqlite src/func.c instrFunc: the result is one more than the
	// number of characters (or bytes, when BOTH arguments are blobs) in the
	// haystack prior to the first occurrence of the needle, or 0 if absent.
	// An empty needle matches at position 1 (N stays 1); an empty haystack
	// gives 0.
	hayIsBlob, needleIsBlob := isBlob(args[0]), isBlob(args[1])
	var hay, needle []byte
	if hayIsBlob && needleIsBlob {
		// Byte search.
		hay = args[0].([]byte)
		needle = args[1].([]byte)
	} else {
		// Text search: any non-blob argument is used as text; a blob
		// argument is decoded as UTF-8 text.
		hay = []byte(toString(args[0]))
		needle = []byte(toString(args[1]))
	}
	return instrSearch(hay, needle, !(hayIsBlob && needleIsBlob)), nil
}

// instrSearch finds the 1-based position of needle in hay, counting whole
// UTF-8 characters in charMode (bytes otherwise). Returns 0 when absent.
func instrSearch(hay, needle []byte, charMode bool) int64 {
	if len(needle) == 0 {
		return 1
	}
	n := 1 // characters/bytes consumed (1-based)
	for len(needle) <= len(hay) && !bytes.HasPrefix(hay, needle) {
		n++
		// Advance one unit: skip a whole UTF-8 character in text mode.
		hay = hay[1:]
		if charMode {
			for len(hay) > 0 && hay[0]&0xC0 == 0x80 {
				hay = hay[1:]
			}
		}
	}
	if len(needle) > len(hay) {
		return 0
	}
	return int64(n)
}

// isBlob reports whether v is a raw []byte (SQLite BLOB) value.
func isBlob(v interface{}) bool {
	_, ok := v.([]byte)
	return ok
}

func fnHEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite hex(): uppercase hex of the raw bytes, or of the TEXT form for
	// numbers (hex(65) = hex of '65' = 3635).
	return fmt.Sprintf("%X", toString(args[0])), nil
}

func fnQUOTE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		// Quote(NULL) returns the 4-character text string 'NULL' (not SQL
		// NULL), matching SQLite: SELECT quote(NULL) -> 'NULL' (text).
		return "NULL", nil
	}
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%d", v), nil
	case float64:
		// SQLite renders infinities and NaN in quote() as fixed text.
		if math.IsInf(v, 1) {
			return "9.0e+999", nil
		}
		if math.IsInf(v, -1) {
			return "-9.0e+999", nil
		}
		if math.IsNaN(v) {
			return "NaN", nil
		}
		// Format float like SQLite: use %g but ensure .0 for whole numbers
		s := fmt.Sprintf("%g", v)
		// Handle negative zero: SQLite shows -0.0
		if s == "-0" {
			s = "0"
		}
		// If no decimal point and no exponent, add .0
		if !strings.Contains(s, ".") && !strings.ContainsAny(s, "eE") {
			s += ".0"
		}
		return s, nil
	case string:
		// Escape single quotes by doubling them, wrap in single quotes
		escaped := strings.ReplaceAll(v, "'", "''")
		return "'" + escaped + "'", nil
	case []byte:
		// Blob: X'HEX' with uppercase hex digits
		return fmt.Sprintf("X'%X'", v), nil
	case value.ZeroBlob:
		// zeroblob expands at use: quote(zeroblob(2)) is X'0000'
		return "X'" + strings.Repeat("00", v.N) + "'", nil
	default:
		// For bool and other types
		return fmt.Sprintf("'%v'", v), nil
	}
}

func fnUNICODE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	if len(s) == 0 {
		// unicode('') returns NULL.
		return nil, nil
	}
	r, _ := utf8.DecodeRuneInString(s)
	return int64(r), nil
}

func fnCHAR(args []interface{}) (interface{}, error) {
	// SQLite char(): each argument is a Unicode codepoint encoded as UTF-8;
	// NULL arguments are skipped; values above U+10FFFF become U+FFFD.
	var sb strings.Builder
	for _, a := range args {
		if a == nil {
			continue
		}
		c := toInt64(a)
		if c > 0x10FFFF {
			c = 0xFFFD
		}
		sb.WriteRune(rune(c))
	}
	return sb.String(), nil
}

func fnCONCATWS(args []interface{}) (interface{}, error) {
	// SQLite concat_ws(sep, ...): a NULL separator yields the empty string
	// (func9-140: concat_ws(NULL,1,2,...) → {}). NULL values are skipped.
	if len(args) == 0 || args[0] == nil {
		return "", nil
	}
	sep := fmt.Sprintf("%v", args[0])
	var parts []string
	for i := 1; i < len(args); i++ {
		if args[i] != nil {
			parts = append(parts, fmt.Sprintf("%v", args[i]))
		}
	}
	return strings.Join(parts, sep), nil
}

func fnEDITDIST3(args []interface{}) (interface{}, error) {
	// Stub: return 0 for edit distance
	return int64(0), nil
}

func fnUNHEX(args []interface{}) (interface{}, error) {
	// SQLite unhex(X, S): parse the hex string X into a BLOB. Any character
	// in the optional second argument S is ignored (used as a separator, e.g.
	// unhex('FFFF ABCD', ' -') -> X'FFFFABCD'). Returns NULL if the input
	// (after removing separators) contains anything other than 0-9A-Fa-f or
	// has odd length, or if either argument is NULL.
	if args[0] == nil || (len(args) > 1 && args[1] == nil) {
		return nil, nil
	}
	s := toString(args[0])
	if len(args) > 1 {
		s = stripHexSeparators(s, toString(args[1]))
	}
	out, ok := decodeHexBytes(s)
	if !ok {
		return nil, nil
	}
	return out, nil
}

// stripHexSeparators removes every byte of sep from s (byte-wise, matching
// SQLite's unhex: a multi-byte separator character contributes each of its
// UTF-8 bytes to the skip set).
func stripHexSeparators(s, sep string) string {
	if sep == "" {
		return s
	}
	sepSet := make(map[byte]bool, len(sep))
	for i := 0; i < len(sep); i++ {
		sepSet[sep[i]] = true
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if !sepSet[s[i]] {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// decodeHexBytes decodes a hex string of even length into bytes, reporting
// false if any character is not a hexadecimal digit.
func decodeHexBytes(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := unhexDigit(s[i])
		lo, ok2 := unhexDigit(s[i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out[i/2] = hi<<4 | lo
	}
	return out, true
}

// unhexDigit decodes a single hexadecimal digit byte to 0-15.
func unhexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func fnCONCAT(args []interface{}) (interface{}, error) {
	var parts []string
	for _, a := range args {
		if a != nil {
			parts = append(parts, fmt.Sprintf("%v", a))
		}
	}
	return strings.Join(parts, ""), nil
}

func fnUNISTR(args []interface{}) (interface{}, error) {
	// SQLite unistr(X): decode \uXXXX (4 hex) and \UXXXXXXXX (8 hex) unicode
	// escapes into UTF-8. Even invalid codepoints are encoded as their raw
	// UTF-8 bytes (func9-300: \UFFFFFFFF → bytes F7 BF BF BF).
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	var b strings.Builder
	for i := 0; i < len(s); {
		if dec, adv, ok := unistrEscape(s, i); ok {
			b.WriteString(dec)
			i += adv
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), nil
}

// unistrEscape decodes a \uXXXX / \UXXXXXXXX escape at s[i]; returns the
// encoded UTF-8 string, the advance in bytes, and whether it matched.
func unistrEscape(s string, i int) (string, int, bool) {
	if s[i] != '\\' || i+1 >= len(s) || (s[i+1] != 'u' && s[i+1] != 'U') {
		return "", 0, false
	}
	n := 4
	if s[i+1] == 'U' {
		n = 8
	}
	if i+2+n > len(s) {
		return "", 0, false
	}
	cp, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32)
	if err != nil {
		return "", 0, false
	}
	return utf8EncodeCP(uint32(cp)), 2 + n, true
}

// utf8EncodeCP encodes a code point as UTF-8 bytes, matching SQLite's unistr
// which writes the raw bytes even for out-of-range values (\UFFFFFFFF →
// F7 BF BF BF) rather than replacing them.
func utf8EncodeCP(cp uint32) string {
	switch {
	case cp <= 0x7F:
		return string([]byte{byte(cp)})
	case cp <= 0x7FF:
		return string([]byte{0xC0 | byte(cp>>6), 0x80 | byte(cp&0x3F)})
	case cp <= 0xFFFF:
		return string([]byte{0xE0 | byte(cp>>12), 0x80 | byte((cp>>6)&0x3F), 0x80 | byte(cp&0x3F)})
	case cp <= 0x1FFFFF:
		return string([]byte{0xF0 | byte(cp>>18), 0x80 | byte((cp>>12)&0x3F), 0x80 | byte((cp>>6)&0x3F), 0x80 | byte(cp&0x3F)})
	default:
		// Out of UTF-8 range (>
		// 0x1FFFFF): SQLite writes the 4-byte form with the top bits
		// masked; U+FFFFFFFF → F7 BF BF BF.
		return string([]byte{0xF0 | byte(cp>>18)&0x07, 0x80 | byte((cp>>12)&0x3F), 0x80 | byte((cp>>6)&0x3F), 0x80 | byte(cp&0x3F)})
	}
}

// fnUNISTRQUOTE implements unistr_quote(X): unistr() the argument then wrap it
// in single quotes with ” escaping (SQLite's unistr_quote, func9-210).
func fnUNISTRQUOTE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s, err := fnUNISTR(args)
	if err != nil || s == nil {
		return nil, err
	}
	str := toString(s)
	return "'" + strings.ReplaceAll(str, "'", "''") + "'", nil
}

func fnNEXTCHAR(args []interface{}) (interface{}, error) {
	// Stub: return input character + 1
	if args[0] == nil {
		return nil, nil
	}
	s := fmt.Sprintf("%v", args[0])
	if len(s) > 0 {
		return string([]byte{byte(s[0] + 1)}), nil
	}
	return "", nil
}

func fnINT2HEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%x", v), nil
	case float64:
		return fmt.Sprintf("%x", int64(v)), nil
	default:
		return fmt.Sprintf("%x", v), nil
	}
}

func fnPREFIXLENGTH(args []interface{}) (interface{}, error) {
	// SQLite's test-harness prefix_length: the length of the common prefix of
	// the two string arguments (NULL gives 0, per the test suite's expected
	// values: SELECT prefix_length(null,'abc') returns 0).
	if len(args) < 2 || args[0] == nil || args[1] == nil {
		return int64(0), nil
	}
	// The test-harness prefix_length operates on TEXT; blob arguments (e.g.
	// zeroblob) yield 0 (SELECT prefix_length(zeroblob(15000),zeroblob(5000))
	// returns 0 per prefixes.test 3.3).
	if _, isBlob := args[0].([]byte); isBlob {
		return int64(0), nil
	}
	if _, isBlob := args[1].([]byte); isBlob {
		return int64(0), nil
	}
	a := toString(args[0])
	b := toString(args[1])
	// Count the common prefix in characters (Unicode code points), matching
	// the test-harness prefix_length which operates on TCL string characters.
	ar := []rune(a)
	br := []rune(b)
	n := 0
	for n < len(ar) && n < len(br) && ar[n] == br[n] {
		n++
	}
	return int64(n), nil
}

func fnREPEAT(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		if b, ok := args[0].([]byte); ok {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", args[0])
		}
	}
	var n int
	switch v := args[1].(type) {
	case int64:
		n = int(v)
	case int:
		n = v
	case float64:
		n = int(v)
	case string:
		fmt.Sscanf(v, "%d", &n)
	default:
		return nil, nil
	}
	if n <= 0 {
		return "", nil
	}
	return strings.Repeat(s, n), nil
}
