package function

// JSONBlob is a JSONB-encoded document (byte-exact port of SQLite's binary
// JSON format, src/json.c) supporting the offset-based element traversal
// that json_each/json_tree use: element ids are byte offsets into the blob.

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// appendJSONEscaped appends s as a JSON string literal (bytes-based form of
// jsonEscapeString).
func appendJSONEscaped(sb []byte, s string) []byte {
	var b strings.Builder
	jsonEscapeString(&b, s)
	return append(sb, b.String()...)
}

// JSONEscapeString renders s as a JSON string literal (with the enclosing
// quotes), escaping quotes, backslashes and control characters.
func JSONEscapeString(s string) string { return jsonQuoteString(s) }

// jsonbTypeNames mirrors sqlite's jsonbType[] table (src/json.c).
var jsonbTypeNames = [16]string{
	"null", "true", "false", "integer", "integer",
	"real", "real", "text", "text", "text",
	"text", "array", "object", "", "", "",
}

// JSONBlob wraps an encoded JSONB document.
type JSONBlob struct {
	b []byte
}

// NewJSONBlob parses src (strict JSON or the relaxed/JSON5 dialect) and
// returns its JSONB encoding.
func NewJSONBlob(src string) (*JSONBlob, error) {
	root, err := parseJSON(src)
	if err != nil {
		return nil, err
	}
	return &JSONBlob{b: jsonbEncodeElement(nil, root)}, nil
}

// AsJSONBlob converts a SQL value to a JSONBlob: TEXT (with or without the
// returned-JSON subtype) is parsed and encoded; a BLOB whose first byte is a
// valid JSONB header is used as-is (jsonArgIsJsonb parity). ok=false when v
// is SQL NULL.
func AsJSONBlob(v interface{}) (*JSONBlob, bool, error) {
	v = util.UnwrapColumnValue(v)
	switch x := v.(type) {
	case nil:
		return nil, false, nil
	case JSONText:
		b, err := NewJSONBlob(string(x))
		return b, true, err
	case string:
		b, err := NewJSONBlob(x)
		return b, true, err
	case []byte:
		if !jsonbHeaderCheck(x) {
			// Non-JSONB blobs are interpreted as their TEXT bytes
			// (src/json.c tag-20240123-a fall-through).
			tb, terr := NewJSONBlob(string(x))
			return tb, true, terr
		}
		return &JSONBlob{b: x}, true, nil
	default:
		return nil, true, fmt.Errorf("malformed JSON")
	}
}

// jsonArgText converts a JSON function's leading argument to JSON text:
// BLOB inputs carrying a valid JSONB header are decoded to text — with
// structural corruption reported as an error (src/json.c
// jsonTranslateBlobToText sets JSTRING_MALFORMED and json()/->/->>/
// json_extract() fail with "malformed JSON", jsonb01-2.0); other BLOBs are
// interpreted as their raw TEXT bytes (src/json.c tag-20240123-a);
// remaining values render via toString.
func jsonArgText(v interface{}) (string, error) {
	v = util.UnwrapColumnValue(v)
	if b, ok := v.([]byte); ok {
		if jsonbHeaderCheck(b) {
			return (&JSONBlob{b: b}).TranslateText(0)
		}
		return string(b), nil
	}
	return toString(v), nil
}

// parseJSONArg parses a JSON function's leading argument, accepting TEXT,
// JSONText and BLOB/JSONB inputs.
func parseJSONArg(v interface{}) (*jsonNode, error) {
	src, err := jsonArgText(v)
	if err != nil {
		return nil, err
	}
	return parseJSON(src)
}

// jsonbHeaderCheck reports whether b carries a plausible JSONB header —
// sqlite jsonArgIsJsonb parity: the first element's type nibble is valid,
// its payload size spans EXACTLY the blob, and TEXT-lookalike tiny payloads
// pass the strict validity check. Unlike isJSONBBlob this does NOT validate
// the children: sqlite json_each/json() accept structurally corrupt tails
// (e.g. a final label without a value, Bug 2026-07-04) and surface them
// lazily through jsonSkipLabel.
func jsonbHeaderCheck(b []byte) bool {
	if len(b) == 0 || b[0]&0x0f > 12 {
		return false
	}
	n, sz, err := jsonbPayloadSize(b, 0)
	if err != nil || n == 0 || n+sz != len(b) {
		return false
	}
	c := b[0]
	if c&0x0f <= 2 && sz != 0 { // null/true/false must be empty
		return false
	}
	if sz > 7 || (c != 0x7b && c != 0x5b && !(c >= '0' && c <= '9')) {
		return true
	}
	// Tiny payload that could be plain TEXT ("{..."/"[..."/digits): full
	// validity check decides (sqlite jsonbValidityCheck(p,0,nBlob,1)==0).
	ok, _ := jsonbElementValid(b, 0)
	return ok && jsonbElementEnd(b, 0) == len(b)
}

// isJSONBBlob reports whether b is a fully valid JSONB document: the first
// element header must be well-formed and every nested element must consume
// exactly the whole blob. This guards against TEXT-as-BLOB inputs whose
// first byte merely mimics a JSONB header.
func isJSONBBlob(b []byte) bool {
	if len(b) == 0 || b[0]&0x0f > 12 {
		return false
	}
	ok, _ := jsonbElementValid(b, 0)
	return ok && jsonbElementEnd(b, 0) == len(b)
}

// jsonbElementEnd returns the exclusive end offset of the element at i.
func jsonbElementEnd(b []byte, i int) int {
	n, sz, err := jsonbPayloadSize(b, i)
	if err != nil {
		return -1
	}
	return i + n + sz
}

// jsonbElementValid structurally validates the element at offset i.
func jsonbElementValid(b []byte, i int) (bool, int) {
	t := b[i] & 0x0f
	end := jsonbElementEnd(b, i)
	if end < 0 || end > len(b) {
		return false, 0
	}
	switch t {
	case 11: // array
		off := i + 1
		if b[i]>>4 > 11 {
			off = i + jsonbHeaderLen(b, i)
		}
		for off < end {
			if off >= len(b) || b[off]&0x0f > 12 {
				return false, 0
			}
			v, next := jsonbElementValid(b, off)
			if !v || next <= off {
				return false, 0
			}
			off = next
		}
		return off == end, end
	case 12: // object: alternating label/value elements
		off := i + jsonbHeaderLen(b, i)
		for off < end {
			if off >= len(b) || b[off]&0x0f < 7 || b[off]&0x0f > 10 {
				return false, 0 // label must be a text element
			}
			v, labelEnd := jsonbElementValid(b, off)
			if !v || labelEnd >= end {
				return false, 0
			}
			off = labelEnd
			if off >= len(b) || b[off]&0x0f > 12 {
				return false, 0
			}
			v, valEnd := jsonbElementValid(b, off)
			if !v {
				return false, 0
			}
			off = valEnd
		}
		return true, end
	default:
		return true, end
	}
}

// jsonbHeaderLen returns the header length of the element at i.
func jsonbHeaderLen(b []byte, i int) int {
	n, _, err := jsonbPayloadSize(b, i)
	if err != nil {
		return 0
	}
	return n
}

// jsonbPayloadSize returns the header length n and payload size sz of the
// element at offset i (src/json.c jsonbPayloadSize). err is non-nil when the
// size header is truncated OR the element does not fit within the blob —
// sqlite's containment tail check, which is what surfaces corrupt JSONB
// (e.g. a child whose declared payload runs past the end) as "malformed
// JSON" during translation and lookup rather than as an out-of-bounds read.
// (sqlite's pParse->delta is 0 for the immutable blobs frigolite renders.)
func jsonbPayloadSize(b []byte, i int) (n, sz int, err error) {
	if i < 0 || i >= len(b) {
		return 0, 0, fmt.Errorf("malformed JSON")
	}
	h := b[i] >> 4
	switch {
	case h <= 11:
		n, sz = 1, int(h)
	case h == 12:
		if i+1 >= len(b) {
			return 0, 0, fmt.Errorf("malformed JSON")
		}
		n, sz = 2, int(b[i+1])
	case h == 13:
		if i+2 >= len(b) {
			return 0, 0, fmt.Errorf("malformed JSON")
		}
		n, sz = 3, int(binary.BigEndian.Uint16(b[i+1:i+3]))
	case h == 14:
		if i+4 >= len(b) {
			return 0, 0, fmt.Errorf("malformed JSON")
		}
		n, sz = 5, int(binary.BigEndian.Uint32(b[i+1:i+5]))
	default:
		if i+8 >= len(b) {
			return 0, 0, fmt.Errorf("malformed JSON")
		}
		s := binary.BigEndian.Uint64(b[i+1 : i+9])
		if s > uint64(math.MaxInt32) {
			return 0, 0, fmt.Errorf("malformed JSON")
		}
		n, sz = 9, int(s)
	}
	// Containment: the element (header plus payload) must fit in the blob.
	if i+n+sz > len(b) {
		return 0, 0, fmt.Errorf("malformed JSON")
	}
	return n, sz, nil
}

// ElemType returns the element-type nibble at offset i.
func (jb *JSONBlob) ElemType(i int) byte { return jb.b[i] & 0x0f }

// HeaderSize returns the header length and payload size of the element at i.
func (jb *JSONBlob) HeaderSize(i int) (n, sz int) {
	n, sz, _ = jsonbPayloadSize(jb.b, i)
	return n, sz
}

// TypeName returns the json_each "type" column value for the element at i.
func (jb *JSONBlob) TypeName(i int) string { return jsonbTypeNames[jb.ElemType(i)] }

// Key returns the raw label text stored in the label element at i (the
// current element of an object walk).
func (jb *JSONBlob) Key(i int) string {
	n, sz := jb.HeaderSize(i)
	return string(jb.b[i+n : i+n+sz])
}

// Blob returns the underlying encoded bytes.
func (jb *JSONBlob) Blob() []byte { return jb.b }

// SQLValue renders the element at i as a SQL value (jsonReturnFromBlob
// parity): scalars become their SQL equivalents; containers render as JSON
// text carrying the JSON subtype so downstream functions embed them raw.
func (jb *JSONBlob) SQLValue(i int) interface{} {
	t := jb.ElemType(i)
	n, sz := jb.HeaderSize(i)
	payload := string(jb.b[i+n : i+n+sz])
	switch t {
	case 0: // null
		return nil
	case 1: // true
		return int64(1)
	case 2: // false
		return int64(0)
	case 3, 4: // integer
		if v, err := strconv.ParseInt(payload, 10, 64); err == nil {
			return v
		}
		return payload
	case 5, 6: // real
		if payload == "null" {
			// NaN serializes as "null" and surfaces as SQL NULL.
			return nil
		}
		if f, err := strconv.ParseFloat(payload, 64); err == nil {
			return f
		} else if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f // strtod saturation: ±Inf / 0
		}
		return payload
	case 7, 8, 9, 10: // text forms
		return payload
	default: // array/object → JSON text with subtype
		return JSONText(jb.JSONText(i))
	}
}

// IsContainer reports whether the element at i is an array or object.
func (jb *JSONBlob) IsContainer(i int) bool { return jb.ElemType(i) >= 11 }

// ElementComplete reports whether a structurally complete element starts
// at byte offset i (sqlite jsonSkipLabel uses this implicitly: when the
// element after a label is missing or truncated, the label itself becomes
// the value — Bug 2026-07-04).
func (jb *JSONBlob) ElementComplete(i int) bool {
	if i < 0 || i >= len(jb.b) {
		return false
	}
	ok, _ := jsonbElementValid(jb.b, i)
	return ok && jsonbElementEnd(jb.b, i) <= len(jb.b)
}

// JSONText serializes the element subtree at i back to compact JSON text.
// It is the LENIENT rendering used by the json_each cursor path (sqlite
// jsonReturnFromBlob/jsonTranslateBlobToText callers that do not check the
// JSTRING_MALFORMED flag): a corrupt subtree renders partially instead of
// erroring. Use TranslateText where sqlite propagates the error.
func (jb *JSONBlob) JSONText(i int) string {
	sb, _, _ := jb.appendText(nil, i)
	return string(sb)
}

// TranslateText serializes the element subtree at i to compact JSON text,
// reporting structural corruption as an error (src/json.c
// jsonTranslateBlobToText + its JSTRING_MALFORMED handling). This is the
// entry point behind json()/->/->>/json_extract() on a BLOB: a corrupt
// JSONB document must fail with "malformed JSON" (jsonb01-2.0) instead of
// silently rendering truncated output.
func (jb *JSONBlob) TranslateText(i int) (string, error) {
	sb, _, err := jb.appendText(nil, i)
	if err != nil {
		return "", err
	}
	return string(sb), nil
}

// appendText appends the JSON text of the element subtree at i to sb,
// returning the extended buffer, the blob index just past the element, and
// an error for malformed input (src/json.c jsonTranslateBlobToText):
//   - an element whose header/payload does not fit in the blob (n==0),
//   - an INT/FLOAT/INT5/FLOAT5 with an empty payload,
//   - an INT5 payload that is not (sign?)0xHEX,
//   - a TEXT5 payload ending in a dangling or truncated escape,
//   - a container whose children overrun the container (j>iEnd) or an
//     object with an odd number of members (label without value).
func (jb *JSONBlob) appendText(sb []byte, i int) ([]byte, int, error) {
	n, sz := jb.HeaderSize(i)
	if n == 0 {
		return sb, len(jb.b) + 1, jsonParseErr()
	}
	var err error
	payload := jb.b[i+n : i+n+sz]
	t := jb.ElemType(i)
	switch t {
	case 0:
		return append(sb, "null"...), i + 1, nil
	case 1:
		return append(sb, "true"...), i + 1, nil
	case 2:
		return append(sb, "false"...), i + 1, nil
	case 3, 5: // INT, FLOAT: payload is already canonical JSON number text
		if sz == 0 {
			return sb, i + n + sz, jsonParseErr()
		}
		return append(sb, payload...), i + n + sz, nil
	case 4: // INT5: 0xHEX integer literal — render as decimal
		sb, err = jb.appendInt5(sb, payload)
		if err != nil {
			return sb, 0, err
		}
		return sb, i + n + sz, nil
	case 6: // FLOAT5: literal missing digits beside "." — insert them
		sb, err = jb.appendFloat5(sb, payload)
		if err != nil {
			return sb, 0, err
		}
		return sb, i + n + sz, nil
	case 7: // TEXT: plain, escape as JSON string
		return appendJSONEscaped(sb, string(payload)), i + n + sz, nil
	case 8: // TEXTJ: already JSON-escaped body
		sb = append(sb, '"')
		sb = append(sb, payload...)
		return append(sb, '"'), i + n + sz, nil
	case 9: // TEXT5: JSON5 escape forms — translate to JSON escapes
		sb, err = jb.appendText5(sb, payload)
		if err != nil {
			return sb, 0, err
		}
		return sb, i + n + sz, nil
	case 10: // TEXTRAW: SQL text that needs escaping
		return appendJSONEscaped(sb, string(payload)), i + n + sz, nil
	case 11: // array
		sb = append(sb, '[')
		end := i + n + sz
		j := i + n
		first := true
		for j < end {
			var err error
			if !first {
				sb = append(sb, ',')
			}
			first = false
			sb, j, err = jb.appendText(sb, j)
			if err != nil {
				return sb, j, err
			}
		}
		if j > end {
			return sb, j, jsonParseErr()
		}
		return append(sb, ']'), j, nil
	default: // object
		sb = append(sb, '{')
		end := i + n + sz
		j := i + n
		cnt := 0
		for j < end {
			var err error
			// Elements alternate label, value: ':' follows a label, ','
			// separates members (sqlite appends after each child and trims
			// the trailing one; emitting between elements is equivalent).
			if cnt > 0 {
				if cnt%2 == 1 {
					sb = append(sb, ':')
				} else {
					sb = append(sb, ',')
				}
			}
			sb, j, err = jb.appendText(sb, j)
			if err != nil {
				return sb, j, err
			}
			cnt++
		}
		if cnt%2 != 0 || j > end {
			return sb, j, jsonParseErr()
		}
		return append(sb, '}'), j, nil
	}
}

// appendInt5 renders an INT5 payload (sign?0xHEX) as a decimal integer
// (src/json.c jsonTranslateBlobToText JSONB_INT5 case). Overflow of the
// 64-bit hex value renders sqlite's 9.0e999 out-of-range sentinel.
func (jb *JSONBlob) appendInt5(sb []byte, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return sb, jsonParseErr()
	}
	k := 0
	var u uint64
	overflow := false
	if payload[0] == '-' {
		sb = append(sb, '-')
		k++
	} else if payload[0] == '+' {
		k++
	}
	// k=2 in sqlite skips the "0x" prefix; non-hex digits are malformed.
	for ; k < len(payload); k++ {
		c := payload[k]
		v, ok := hexDigit(c)
		if !ok {
			return sb, jsonParseErr()
		}
		if u>>60 != 0 {
			overflow = true
		} else {
			u = u*16 + uint64(v)
		}
	}
	if overflow {
		return append(sb, "9.0e999"...), nil
	}
	return strconv.AppendUint(sb, u, 10), nil
}

// appendFloat5 renders a FLOAT5 payload (digits missing beside ".") as a
// canonical JSON number (src/json.c jsonTranslateBlobToText JSONB_FLOAT5
// case): a leading "." gains a '0' before it, a "." at the end or followed
// by a non-digit gains a '0' after it.
func (jb *JSONBlob) appendFloat5(sb []byte, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return sb, jsonParseErr()
	}
	k := 0
	if payload[0] == '-' {
		sb = append(sb, '-')
		k++
	}
	if k < len(payload) && payload[k] == '.' {
		sb = append(sb, '0')
	}
	for ; k < len(payload); k++ {
		sb = append(sb, payload[k])
		if payload[k] == '.' && (k+1 == len(payload) || payload[k+1] < '0' || payload[k+1] > '9') {
			sb = append(sb, '0')
		}
	}
	return sb, nil
}

// appendText5 renders a TEXT5 payload (JSON5 escapes) as a JSON string
// (src/json.c jsonTranslateBlobToText JSONB_TEXT5 case): \' → ', \v →
// \u000b, \xHH → \u00HH, \0 → \u0000, \<CR><LF> and \ + U+2028/2029 →
// nothing (line continuations). A dangling backslash or truncated \xHH or
// U+2028 sequence is malformed.
func (jb *JSONBlob) appendText5(sb []byte, payload []byte) ([]byte, error) {
	sb = append(sb, '"')
	for k := 0; k < len(payload); {
		c := payload[k]
		if c != '\\' && c != '"' && c > 0x1f {
			// Ordinary run: copy until the next special byte.
			start := k
			for k < len(payload) {
				c = payload[k]
				if c == '\\' || c == '"' || c <= 0x1f {
					break
				}
				k++
			}
			sb = append(sb, payload[start:k]...)
			continue
		}
		switch {
		case c == '"':
			sb = append(sb, "\\\""...)
			k++
		case c <= 0x1f:
			// Control byte: render as \u00XX (sqlite jsonAppendControlChar).
			sb = append(sb, "\\u00"...)
			sb = append(sb, hexDigits[c>>4], hexDigits[c&0xf])
			k++
		default: // backslash escape
			if k+1 >= len(payload) {
				return sb, jsonParseErr()
			}
			switch e := payload[k+1]; e {
			case '\'':
				sb = append(sb, '\'')
			case 'v':
				sb = append(sb, "\\u000b"...)
			case 'x':
				if k+3 >= len(payload) {
					return sb, jsonParseErr()
				}
				sb = append(sb, "\\u00"...)
				sb = append(sb, payload[k+2], payload[k+3])
				k += 2
			case '0':
				sb = append(sb, "\\u0000"...)
			case '\r':
				if k+2 < len(payload) && payload[k+2] == '\n' {
					k++ // \<CR><LF> is one line continuation
				}
			case '\n':
				// Line continuation: emit nothing.
			case 0xe2:
				if k+3 >= len(payload) || payload[k+2] != 0x80 ||
					(payload[k+3] != 0xa8 && payload[k+3] != 0xa9) {
					return sb, jsonParseErr()
				}
				k += 2
			default:
				sb = append(sb, payload[k], payload[k+1])
			}
			k += 2
		}
	}
	return append(sb, '"'), nil
}

// hexDigit maps one ASCII hex digit to its value.
func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// Lookup resolves path (e.g. "$.a[2]") against the blob, returning the label
// element index (-1 when the final step was an array index or the root) and
// the value element index (jsonLookupStep parity for the cases json_each's
// ROOT argument exercises).
func (jb *JSONBlob) Lookup(path string) (label, value int, err error) {
	comps, perr := parseJSONPath(path)
	if perr != nil {
		return -1, -1, badJSONPath(path)
	}
	cur := 0
	label = -1
	for _, c := range comps {
		if jb.ElemType(cur) != 11 && jb.ElemType(cur) != 12 {
			return -1, -1, badJSONPath(path)
		}
		next, lab, ok := jb.lookupStep(cur, c)
		if !ok {
			return -1, -1, fmt.Errorf("not found")
		}
		label = lab
		cur = next
	}
	return label, cur, nil
}

// lookupStep resolves one path component against the container at cur,
// returning the child value element index and its label index (-1 for arrays).
// A child whose header/payload does not fit in the blob ends the walk
// (jsonbPayloadSize containment): the path is reported as not found.
func (jb *JSONBlob) lookupStep(cur int, c jsonPathComponent) (value, label int, ok bool) {
	n, sz := jb.HeaderSize(cur)
	if n == 0 {
		return 0, -1, false
	}
	end := cur + n + sz
	off := cur + n
	if jb.ElemType(cur) == 12 { // object: alternating label/value elements
		for off < end {
			li := off
			ln, lsz := jb.HeaderSize(li)
			if ln == 0 {
				break
			}
			vi := li + ln + lsz
			vn, vsz := jb.HeaderSize(vi)
			if vn == 0 {
				break
			}
			if c.isIdx {
				// Object members are not addressable by index; no match here.
				off = vi + vn + vsz
				continue
			}
			if jb.Key(li) == c.key {
				return vi, li, true
			}
			off = vi + vn + vsz
		}
		return 0, -1, false
	}
	// array
	if !c.isIdx {
		return 0, -1, false
	}
	idx := 0
	for off < end {
		vi := off
		vn, vsz := jb.HeaderSize(vi)
		if vn == 0 {
			break
		}
		if int64(idx) == c.index {
			return vi, -1, true
		}
		off = vi + vn + vsz
		idx++
	}
	return 0, -1, false
}

// fnSUBTYPE implements the test-suite subtype(X) helper: 1 when X carries
// the returned-JSON subtype (JSONText) or is a JSONB blob, else 0.
func fnSUBTYPE(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("subtype() needs exactly one argument")
	}
	switch util.UnwrapColumnValue(args[0]).(type) {
	case JSONText:
		return int64(1), nil
	case []byte:
		return int64(1), nil
	default:
		return int64(0), nil
	}
}
