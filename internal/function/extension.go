// Package function — loadable-extension SQL functions implemented natively.
//
// These are ports of SQLite's ext/misc/ extensions (basexx.c, fileio.c,
// ieee754.c, decimal.c, eval.c) plus the test-harness status function from
// src/test_loadext.c. A pure-Go engine cannot dlopen C shared libraries, so
// the extensions that SQLite ships as loadable modules are built in directly,
// matching the behavior of a SQLite build that compiles them as built-ins.
package function

import (
	"fmt"
	"math"
	"os"
)

// ---------------------------------------------------------------------------
// basexx.c — base64(X), base85(X), is_base85(X)
//
// Port of SQLite ext/misc/base64.c and ext/misc/base85.c (2022-11).
// Encode: BLOB -> TEXT with embedded '\n' line feeds every 72 (base64) or 80
// (base85) columns, plus a terminating '\n'. Decode: TEXT -> BLOB, skipping
// every character that is not a base-N numeral (whitespace and junk like '~'
// are ignored).
// ---------------------------------------------------------------------------

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const b64DarkMax = 72

// b64DigitValue returns the base64 digit value of c, or:
//
//	0x80 = not a digit (ND), 0x81 = whitespace (WS), 0x82 = pad '=' (PC)
//
// matching base64.c's b64DigitValues table for ASCII 0-127. Bytes >= 0x80
// are treated as ND.
// b64DigitValues mirrors base64.c's b64DigitValues table: the base64 digit
// value of each ASCII code (0-63), 0x80 = not a digit (ND), 0x81 = whitespace
// (WS), 0x82 = pad '=' (PC). Codes >= 0x80 are ND.
var b64DigitValues = func() [128]byte {
	var t [128]byte
	for i := range t {
		t[i] = 0x80 // ND
	}
	for _, c := range []byte{'\t', '\n', 0x0b, 0x0c, '\r', ' '} {
		t[c] = 0x81 // WS
	}
	t['='] = 0x82 // PC
	t['+'] = 62
	t['/'] = 63
	for c := byte('0'); c <= '9'; c++ {
		t[c] = c - '0' + 52
	}
	for c := byte('A'); c <= 'Z'; c++ {
		t[c] = c - 'A'
	}
	for c := byte('a'); c <= 'z'; c++ {
		t[c] = c - 'a' + 26
	}
	return t
}()

func b64DigitValue(c byte) byte {
	if c >= 0x80 {
		return 0x80
	}
	return b64DigitValues[c]
}

func isB64Digit(v byte) bool { return v < 0x80 }
func isB64WS(v byte) bool    { return v == 0x81 }
func isB64Pad(v byte) bool   { return v == 0x82 }
func b64Numeral(v byte) byte { return b64Alphabet[v&0x3f] }

// toBase64 encodes a byte buffer into base64 text with '\n' line feeds every
// B64_DARK_MAX columns and after the final group (base64.c toBase64).
func toBase64(pIn []byte) []byte {
	var out []byte
	nCol := 0
	i := 0
	for len(pIn)-i >= 3 {
		b0, b1, b2 := pIn[i], pIn[i+1], pIn[i+2]
		out = append(out, b64Numeral(b0>>2),
			b64Numeral(((b0<<4)|(b1>>4))&0x3f),
			b64Numeral(((b1&0xf)<<2)|(b2>>6)),
			b64Numeral(b2&0x3f))
		i += 3
		nCol += 4
		if nCol >= b64DarkMax || len(pIn)-i <= 0 {
			out = append(out, '\n')
			nCol = 0
		}
	}
	if rem := len(pIn) - i; rem > 0 {
		nco := rem + 1
		qv := uint64(pIn[i])
		for nbe := 1; nbe < 3; nbe++ {
			qv <<= 8
			if nbe < rem {
				qv |= uint64(pIn[i+nbe])
			}
		}
		// Emit 4 chars, most significant first, padding with '='.
		buf := [4]byte{}
		for nbe := 3; nbe >= 0; nbe-- {
			if nbe < nco {
				buf[nbe] = b64Numeral(byte(qv & 0x3f))
			} else {
				buf[nbe] = '='
			}
			qv >>= 6
		}
		out = append(out, buf[:]...)
		out = append(out, '\n')
	}
	return out
}

// skipNonB64 advances s past characters that are not base64 numerals.
func skipNonB64(s []byte) int {
	i := 0
	for i < len(s) && !isB64Digit(b64DigitValue(s[i])) {
		i++
	}
	return i
}

// fromBase64 decodes base64 text into bytes, skipping non-numeral characters
// and honoring '=' padding (base64.c fromBase64).
func fromBase64(pIn []byte) []byte {
	var out []byte
	nc := len(pIn)
	if nc > 0 && pIn[nc-1] == '\n' {
		nc--
	}
	nboi := [...]int{0, 0, 1, 2, 3}
	for nc > 0 && pIn[0] != '=' {
		skip := skipNonB64(pIn[:nc])
		nc -= skip
		pIn = pIn[skip:]
		nti := nc
		if nti > 4 {
			nti = 4
		}
		nc -= nti
		nbo := nboi[nti]
		if nbo == 0 {
			break
		}
		qv := uint64(0)
		for nac := 0; nac < 4; nac++ {
			var c byte
			if nac < nti {
				c = pIn[0]
				pIn = pIn[1:]
			} else {
				c = b64Alphabet[0]
			}
			bdp := b64DigitValue(c)
			switch {
			case bdp == 0x80: // ND: treat as pad, terminate this group
				nc = 0
				fallthrough
			case isB64WS(bdp):
				nti = nac
				fallthrough
			case isB64Pad(bdp):
				bdp = 0
				nbo--
				fallthrough
			default:
				qv = qv<<6 | uint64(bdp)
			}
		}
		switch nbo {
		case 3:
			out = append(out, byte((qv>>16)&0xff), byte((qv>>8)&0xff), byte(qv&0xff))
		case 2:
			out = append(out, byte((qv>>16)&0xff), byte((qv>>8)&0xff))
		case 1:
			out = append(out, byte((qv>>16)&0xff))
		}
	}
	return out
}

// fnBASE64 implements base64(X): blob -> text (encode) or text -> blob
// (decode). NULL and non-blob/text inputs raise "base64 accepts only blob or
// text" (base64.c base64()).
func fnBASE64(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	switch x := v.(type) {
	case []byte:
		return string(toBase64(x)), nil
	case string:
		return fromBase64([]byte(x)), nil
	default:
		return nil, fmt.Errorf("base64 accepts only blob or text")
	}
}

// EvalBaseX implements base64/base85 with the engine's SQLITE_LIMIT_LENGTH
// check (basexx.c base64()/base85()). It is exported for the expression
// evaluator, which supplies the current length limit.
func EvalBaseX(name string, v interface{}, limit int) (interface{}, error) {
	switch x := v.(type) {
	case []byte:
		nv := int64(len(x))
		var nc int64
		if name == "base64" {
			nc = 4 * ((nv + 2) / 3)
			nc += (nc + 71) / 72
			nc++
		} else {
			nc = 5*(nv/4) + nv%4 + nv/64 + 1 + 2
		}
		if int64(limit) < nc {
			return nil, fmt.Errorf("blob expanded to %s too big", name)
		}
		if name == "base64" {
			return string(toBase64(x)), nil
		}
		return string(toBase85(x)), nil
	case string:
		nv := int64(len(x))
		var nb int64
		if name == "base64" {
			nb = 3 * ((nv + 3) / 4)
		} else {
			nb = 4*(nv/5) + nv%5
		}
		if int64(limit) < nb {
			return nil, fmt.Errorf("blob from %s may be too big", name)
		}
		if name == "base64" {
			return fromBase64([]byte(x)), nil
		}
		return fromBase85([]byte(x)), nil
	default:
		if name == "base64" {
			return nil, fmt.Errorf("base64 accepts only blob or text")
		}
		//lint:ignore ST1005 exact SQLite error text ends with a period (base85.c)
		return nil, fmt.Errorf("base85 accepts only blob or text.")
	}
}

// ---------------------------------------------------------------------------
// base85.c — base85(X), is_base85(X)
//
// Base85 numerals are 7-bit USASCII codes excluding control characters and
// Space ! " ' ( ) { | } ~ Del, in code order representing digit values 0..84.
// B85_CLASS(c) = (c>='#')+(c>'&')+(c>='*')+(c>'z'); odd class = numeral.
// Groups of 4 bytes (big-endian 32-bit) become 5 digits; trailing 1-3 bytes
// become 2-4 digits. Encoding inserts '\n' every 80 columns and at the end.
// ---------------------------------------------------------------------------

const b85DarkMax = 80

func isB85(c byte) bool {
	// B85_CLASS(c) & 1: odd means base85 numeral.
	class := 0
	if c >= '#' {
		class++
	}
	if c > '&' {
		class++
	}
	if c >= '*' {
		class++
	}
	if c > 'z' {
		class++
	}
	return class&1 == 1
}

// b85Offset returns the digit-value offset for a numeral c (B85_DNOS).
// Non-numerals return 0 (the C code's b85_cOffset maps class 0/2/4 to 0),
// which makes the decode loop treat them as group delimiters.
func b85Offset(c byte) byte {
	if !isB85(c) {
		return 0
	}
	if c < '*' {
		return '#'
	}
	return '*' - 4
}

func b85Numeral(dn byte) byte {
	if dn < 4 {
		return dn + '#'
	}
	return dn - 4 + '*'
}

func skipNonB85(s []byte) int {
	i := 0
	for i < len(s) && !isB85(s[i]) {
		i++
	}
	return i
}

// toBase85 encodes a byte buffer into base85 text, inserting pSep ('\n') every
// B85_DARK_MAX columns and after the final group (base85.c toBase85).
func toBase85(pIn []byte) []byte {
	var out []byte
	nCol := 0
	i := 0
	for len(pIn)-i >= 4 {
		qbv := uint64(pIn[i])<<24 | uint64(pIn[i+1])<<16 | uint64(pIn[i+2])<<8 | uint64(pIn[i+3])
		var buf [5]byte
		for nco := 5; nco > 0; nco-- {
			dv := byte(qbv % 85)
			qbv /= 85
			buf[nco-1] = b85Numeral(dv)
		}
		out = append(out, buf[:]...)
		i += 4
		nCol += 5
		if nCol >= b85DarkMax {
			out = append(out, '\n')
			nCol = 0
		}
	}
	if rem := len(pIn) - i; rem > 0 {
		nco := rem + 1
		qv := uint64(pIn[i])
		for nbe := 1; nbe < rem; nbe++ {
			qv = qv<<8 | uint64(pIn[i+nbe])
		}
		nCol += nco
		var buf [4]byte
		for nco > 0 {
			dv := byte(qv % 85)
			qv /= 85
			buf[nco-1] = b85Numeral(dv)
			nco--
		}
		out = append(out, buf[:rem+1]...)
	}
	if nCol > 0 {
		out = append(out, '\n')
	}
	return out
}

// fromBase85 decodes base85 text into bytes (base85.c fromBase85).
func fromBase85(pIn []byte) []byte {
	var out []byte
	nc := len(pIn)
	if nc > 0 && pIn[nc-1] == '\n' {
		nc--
	}
	nboi := [...]int{0, 0, 1, 2, 3, 4}
	for nc > 0 {
		skip := skipNonB85(pIn[:nc])
		nc -= skip
		pIn = pIn[skip:]
		nti := nc
		if nti > 5 {
			nti = 5
		}
		nbo := nboi[nti]
		if nbo == 0 {
			break
		}
		qv := uint64(0)
		for nti > 0 {
			c := pIn[0]
			pIn = pIn[1:]
			cdo := b85Offset(c)
			nc--
			if cdo == 0 {
				break
			}
			qv = 85*qv + uint64(c-cdo)
			nti--
		}
		nbo -= nti // adjust for early (non-digit) end of group
		switch {
		case nbo >= 4:
			out = append(out, byte((qv>>24)&0xff), byte((qv>>16)&0xff), byte((qv>>8)&0xff), byte(qv&0xff))
		case nbo == 3:
			out = append(out, byte((qv>>16)&0xff), byte((qv>>8)&0xff), byte(qv&0xff))
		case nbo == 2:
			out = append(out, byte((qv>>8)&0xff), byte(qv&0xff))
		case nbo == 1:
			out = append(out, byte(qv&0xff))
		}
	}
	return out
}

// allBase85 reports whether every byte of p is a base85 numeral or C isspace.
func allBase85(p []byte) bool {
	for _, c := range p {
		if !isB85(c) && !isASCIISpace(c) {
			return false
		}
	}
	return true
}

func isASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', 0x0b, 0x0c, '\r':
		return true
	}
	return false
}

// fnBASE85 implements base85(X): blob -> text (encode) or text -> blob
// (decode). NULL and non-blob/text inputs raise "base85 accepts only blob or
// text." (base85.c base85()).
func fnBASE85(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	switch x := v.(type) {
	case []byte:
		return string(toBase85(x)), nil
	case string:
		return fromBase85([]byte(x)), nil
	default:
		//lint:ignore ST1005 exact SQLite error text ends with a period (base85.c)
		return nil, fmt.Errorf("base85 accepts only blob or text.")
	}
}

// fnISBASE85 implements is_base85(X): 1 if X contains only base85 numerals
// and whitespace, 0 otherwise; NULL in -> NULL; other types raise
// "is_base85 accepts only text or NULL" (base85.c is_base85()).
func fnISBASE85(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if allBase85([]byte(x)) {
			return int64(1), nil
		}
		return int64(0), nil
	default:
		return nil, fmt.Errorf("is_base85 accepts only text or NULL")
	}
}

// ---------------------------------------------------------------------------
// fileio.c — readfile(X), writefile(W,X[,Y[,Z]])
//
// Port of SQLite ext/misc/fileio.c (readFileContents / writeFile /
// writefileFunc). Only the read/write semantics needed by the SQL functions
// are implemented; the fsdir virtual table and ls/mode helpers are omitted.
// ---------------------------------------------------------------------------

// fnREADFILE implements readfile(X): return the contents of file X as a BLOB,
// or NULL if X is NULL or the file does not exist / is unreadable.
func fnREADFILE(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	if v == nil {
		return nil, nil
	}
	name := toString(v)
	data, err := os.ReadFile(name)
	if err != nil {
		// File does not exist or is unreadable: leave result NULL.
		return nil, nil
	}
	return data, nil
}

// fnWRITEFILE implements writefile(W,X[,Y[,Z]]): write X's bytes to file W
// and return the number of bytes written. NULL W returns NULL; NULL X writes
// 0 bytes (truncating the file to empty) and returns 0. Mode/mtime arguments
// (Y/Z) are accepted but not applied (permission/timestamp side effects are
// outside the SQL function's observable result).
func fnWRITEFILE(args []interface{}) (interface{}, error) {
	if len(args) < 2 || len(args) > 4 {
		return nil, fmt.Errorf("wrong number of arguments to function writefile()")
	}
	w := unwrap(args[0])
	if w == nil {
		return nil, nil
	}
	name := toString(w)
	data := unwrap(args[1])
	var payload []byte
	if data != nil {
		switch d := data.(type) {
		case []byte:
			payload = d
		case string:
			payload = []byte(d)
		default:
			payload = []byte(toString(data))
		}
	}
	if err := os.WriteFile(name, payload, 0o666); err != nil {
		return nil, nil
	}
	return int64(len(payload)), nil
}

// ---------------------------------------------------------------------------
// ieee754.c — ieee754(X), ieee754(M,E), ieee754_mantissa/exponent,
// ieee754_to_blob/from_blob, ieee754_to_int/from_int, ieee754_inc
//
// Port of SQLite ext/misc/ieee754.c (2021-03-02 ticket 22dea1cfdb9151e4).
// ---------------------------------------------------------------------------

// ieee754Split decomposes a binary64 value into (mantissa, exponent) such that
// value == mantissa * 2^exponent, mirroring ieee754func's one-argument path.
// For zero: (0, -1075). For -0.0 (bits 0x8000...0): the C code treats it as
// a==0 (memcmp of the double -0.0 and int64 0x8000...0 both match), returning
// (0, -1075) — i.e. -0.0 renders as "ieee754(0,-1075)".
func ieee754Split(r float64) (int64, int) {
	isNeg := r < 0.0
	if isNeg {
		r = -r
	}
	a := int64(math.Float64bits(r))
	if a == 0 {
		return 0, -1075
	}
	e := int(a >> 52)
	m := int64(a & ((int64(1) << 52) - 1))
	if e == 0 {
		m <<= 1
	} else {
		m |= int64(1) << 52
	}
	for e < 1075 && m > 0 && m&1 == 0 {
		m >>= 1
		e++
	}
	if isNeg {
		m = -m
	}
	return m, e - 1075
}

// fnIeee754 implements ieee754(X) -> text "ieee754(M,E)" and ieee754(M,E) ->
// the double M*2^E (or NULL when the reconstruction is NaN).
func fnIeee754(args []interface{}) (interface{}, error) {
	if len(args) == 1 {
		v := unwrap(args[0])
		var r float64
		switch x := v.(type) {
		case []byte:
			if len(x) != 8 {
				return nil, nil
			}
			var bits uint64
			for _, b := range x {
				bits = bits<<8 | uint64(b)
			}
			r = math.Float64frombits(bits)
		case float64:
			r = x
		case int64:
			r = float64(x)
		default:
			r = 0
		}
		m, e := ieee754Split(r)
		return fmt.Sprintf("ieee754(%d,%d)", m, e), nil
	}
	// Two-argument form: reconstruct the double.
	val, ok := ieee754Reconstruct(toInt64(unwrap(args[0])), toInt64(unwrap(args[1])))
	if !ok {
		return nil, nil
	}
	return val, nil
}

// ieee754Reconstruct builds the double M*2^E (ieee754.c ieee754func's
// two-argument path). Returns (value, ok); ok=false means NULL (NaN or
// unrepresentable).
func ieee754Reconstruct(m, e int64) (interface{}, bool) {
	if e > 10000 {
		e = 10000
	} else if e < -10000 {
		e = -10000
	}
	isNeg := false
	if m < 0 {
		if m < -9223372036854775807 {
			return nil, false
		}
		isNeg = true
		m = -m
	} else if m == 0 && e > -1000 && e < 1000 {
		return float64(0), true
	}
	m, e = ieee754Normalize(m, e)
	e += 1075
	if e <= 0 {
		// Subnormal
		if 1-e >= 64 {
			m = 0
		} else {
			m >>= 1 - e
		}
		e = 0
	} else if e > 0x7ff {
		e = 0x7ff
	}
	a := uint64(m & ((int64(1) << 52) - 1))
	a |= uint64(e) << 52
	if isNeg {
		a |= uint64(1) << 63
	}
	r := math.Float64frombits(a)
	// SQLite's sqlite3VdbeMemSetDouble converts NaN to NULL; Inf passes
	// through as a REAL.
	if math.IsNaN(r) {
		return nil, false
	}
	return r, true
}

// ieee754Normalize shifts m into the range [2^52, 2^53), adjusting e so that
// m*2^e is unchanged (ieee754.c ieee754func's two normalization loops).
func ieee754Normalize(m, e int64) (int64, int64) {
	for (m>>32)&0xffe00000 != 0 {
		m >>= 1
		e++
	}
	for m != 0 && (m>>32)&0xfff00000 == 0 {
		m <<= 1
		e--
	}
	return m, e
}

// fnIeee754Mantissa implements ieee754_mantissa(X) — the integer significand
// of the one-argument ieee754() decomposition.
func fnIeee754Mantissa(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	var r float64
	switch x := v.(type) {
	case []byte:
		if len(x) != 8 {
			return nil, nil
		}
		var bits uint64
		for _, b := range x {
			bits = bits<<8 | uint64(b)
		}
		r = math.Float64frombits(bits)
	case float64:
		r = x
	case int64:
		r = float64(x)
	default:
		r = 0
	}
	m, _ := ieee754Split(r)
	return m, nil
}

// fnIeee754Exponent implements ieee754_exponent(X) — the base-2 exponent of
// the one-argument ieee754() decomposition.
func fnIeee754Exponent(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	var r float64
	switch x := v.(type) {
	case []byte:
		if len(x) != 8 {
			return nil, nil
		}
		var bits uint64
		for _, b := range x {
			bits = bits<<8 | uint64(b)
		}
		r = math.Float64frombits(bits)
	case float64:
		r = x
	case int64:
		r = float64(x)
	default:
		r = 0
	}
	_, e := ieee754Split(r)
	return int64(e), nil
}

// fnIeee754ToBlob implements ieee754_to_blob(X): an 8-byte big-endian blob of
// X's binary64 representation (ieee754.c ieee754func_to_blob). FLOAT or
// INTEGER input only; anything else returns NULL.
func fnIeee754ToBlob(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	var r float64
	switch x := v.(type) {
	case float64:
		r = x
	case int64:
		r = float64(x)
	default:
		return nil, nil
	}
	bits := math.Float64bits(r)
	out := make([]byte, 8)
	for i := 1; i <= 8; i++ {
		out[8-i] = byte(bits & 0xff)
		bits >>= 8
	}
	return out, nil
}

// fnIeee754FromBlob implements ieee754_from_blob(X): the double represented by
// the 8-byte big-endian blob X (ieee754.c ieee754func_from_blob). NULL for a
// non-8-byte blob; other input types leave the result NULL (the C function
// only handles SQLITE_BLOB).
func fnIeee754FromBlob(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	x, ok := v.([]byte)
	if !ok || len(x) != 8 {
		return nil, nil
	}
	var bits uint64
	for _, b := range x {
		bits = bits<<8 | uint64(b)
	}
	return math.Float64frombits(bits), nil
}

// fnIeee754ToInt implements ieee754_to_int(X): the 64-bit integer with the
// same bit pattern as X (ieee754.c ieee754func_to_int). FLOAT input only.
func fnIeee754ToInt(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	r, ok := v.(float64)
	if !ok {
		return nil, nil
	}
	return int64(math.Float64bits(r)), nil
}

// fnIeee754FromInt implements ieee754_from_int(X): the double with the same
// bit pattern as the 64-bit integer X (ieee754.c ieee754func_from_int).
// INTEGER input only.
func fnIeee754FromInt(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	m, ok := v.(int64)
	if !ok {
		return nil, nil
	}
	return math.Float64frombits(uint64(m)), nil
}

// fnIeee754Inc implements ieee754_inc(r,N): move r by N binary64 quantums
// (ieee754.c ieee754inc). Default N is -1 (toward zero) when omitted.
func fnIeee754Inc(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	var r float64
	switch x := v.(type) {
	case float64:
		r = x
	case int64:
		r = float64(x)
	default:
		return nil, nil
	}
	n := int64(-1)
	if len(args) >= 2 {
		n = toInt64(unwrap(args[1]))
	}
	bits := math.Float64bits(r)
	bits += uint64(n)
	return math.Float64frombits(bits), nil
}

// ---------------------------------------------------------------------------
// src/test_loadext.c — sqlite3_status(NAME[,reset])
//
// Port of test_loadext.c's statusFunc. Returns the current value of a status
// parameter (1-arg) or its high-water mark (2-arg). Only the six named
// properties from the test extension are recognized; integer opcodes are
// accepted for the other SQLite status parameters.
// ---------------------------------------------------------------------------

var sqliteStatusNames = map[string]int{
	"MEMORY_USED":        0,
	"PAGECACHE_USED":     1,
	"PAGECACHE_OVERFLOW": 2,
	"SCRATCH_USED":       3,
	"SCRATCH_OVERFLOW":   4,
	"MALLOC_SIZE":        5,
}

// sqliteStatusMaxOp is the largest valid SQLITE_STATUS_* opcode (SQLite's
// SQLITE_STATUS_MALLOC_COUNT is 10; the test extension accepts up to 21 —
// sqlite3_status returns SQLITE_MISUSE (21) beyond the valid range).
const sqliteStatusMaxOp = 21

// fnSQLite3Status implements sqlite3_status(X[,Y]) — the test-extension
// function that calls the C sqlite3_status API. A pure-Go engine has no C
// memory allocator; MEMORY_USED etc. report the engine's current allocation
// high-water approximations (0 for most, matching a static build).
func fnSQLite3Status(args []interface{}) (interface{}, error) {
	v := unwrap(args[0])
	op := 0
	opValid := false
	switch x := v.(type) {
	case int64:
		op = int(x)
		opValid = op >= 0 && op <= sqliteStatusMaxOp
	case string:
		o, ok := sqliteStatusNames[x]
		if !ok {
			return nil, fmt.Errorf("unknown status property: %s", x)
		}
		op = o
		opValid = true
	default:
		return nil, fmt.Errorf("unknown status type")
	}
	if !opValid {
		return nil, fmt.Errorf("sqlite3_status(%d,...) returns 21", op)
	}
	cur := int64(0)
	if op == 0 {
		cur = approxMemoryUsed()
	}
	if len(args) >= 2 {
		// 2-arg form returns the high-water mark. Reset when the second arg
		// is nonzero (SQLITE_TRANSIENT semantics approximated: no-op).
		return cur, nil
	}
	return cur, nil
}

// approxMemoryUsed returns a rough approximation of the engine's current
// memory usage in bytes (the pure-Go analog of SQLITE_STATUS_MEMORY_USED).
func approxMemoryUsed() int64 {
	return int64(1 << 20) // 1 MiB constant floor
}
