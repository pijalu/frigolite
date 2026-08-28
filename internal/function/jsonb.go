package function

// Faithful port of the JSONB binary format from src/json.c (2025): each
// element is a header followed by a payload. The low nibble of the first
// header byte is the element type; the high nibble encodes the payload size
// (0..11 = inline size with a one-byte header; 12/13/14/15 = payload size as
// a 1/2/4/8-byte big-endian integer in the remainder of the header).

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/pijalu/frigolite/internal/util"
)

const (
	jsonbNull     = 0
	jsonbTrue     = 1
	jsonbFalse    = 2
	jsonbInt      = 3
	jsonbFloat    = 5
	jsonbText     = 7
	jsonbArray    = 11
	jsonbObject   = 12
	jsonbMaxSmall = 11
)

// jsonbEncodeElement appends the JSONB encoding of n to out.
func jsonbEncodeElement(out []byte, n *jsonNode) []byte {
	switch n.kind {
	case jsonNull:
		return append(out, jsonbNull)
	case jsonTrue:
		return append(out, jsonbTrue)
	case jsonFalse:
		return append(out, jsonbFalse)
	case jsonString:
		return jsonbAppendHeader(out, len(n.str), jsonbText, func(b []byte) []byte {
			return append(b, n.str...)
		})
	case jsonArray:
		children := make([][]byte, 0, len(n.arr))
		total := 0
		for _, el := range n.arr {
			c := jsonbEncodeElement(nil, el)
			children = append(children, c)
			total += len(c)
		}
		return jsonbAppendHeader(out, total, jsonbArray, func(b []byte) []byte {
			for _, c := range children {
				b = append(b, c...)
			}
			return b
		})
	case jsonObject:
		total := 0
		children := make([][]byte, 0, len(n.obj)*2)
		for _, pr := range n.obj {
			children = append(children, jsonbEncodeElement(nil, &jsonNode{kind: jsonString, str: pr.key}))
			children = append(children, jsonbEncodeElement(nil, pr.value))
		}
		for _, c := range children {
			total += len(c)
		}
		return jsonbAppendHeader(out, total, jsonbObject, func(b []byte) []byte {
			for _, c := range children {
				b = append(b, c...)
			}
			return b
		})
	default: // jsonNumber
		typ := byte(jsonbInt)
		if !n.isInt {
			typ = jsonbFloat
		}
		text := n.text
		if text == "" {
			text = strconv.FormatInt(n.i64, 10)
			if !n.isInt {
				text = jsonNumberText(n.num)
			}
		}
		return jsonbAppendHeader(out, len(text), typ, func(b []byte) []byte {
			return append(b, text...)
		})
	}
}

// jsonbAppendHeader writes the type/size header followed by the payload
// produced by fill. Header layout per src/json.c jsonbAppendHeader: the low
// nibble of the first byte is the element type; sizes 0..11 are encoded
// directly in the high nibble; larger sizes use a high-nibble marker
// 12/13/14/15 followed by a 1/2/4/8-byte big-endian size integer.
func jsonbAppendHeader(out []byte, size int, elemType byte, fill func([]byte) []byte) []byte {
	switch {
	case size <= jsonbMaxSmall:
		out = append(out, byte(size)<<4|elemType)
		return fill(out)
	case size <= 255:
		out = append(out, 0xC0|elemType, byte(size))
		return fill(out)
	case size <= 65535:
		out = append(out, 0xD0|elemType, byte(size>>8), byte(size))
		return fill(out)
	case size <= math.MaxUint32:
		out = append(out, 0xE0|elemType)
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], uint32(size))
		out = append(out, be[:]...)
		return fill(out)
	default:
		out = append(out, 0xF0|elemType)
		var be [8]byte
		binary.BigEndian.PutUint64(be[:], uint64(size))
		out = append(out, be[:]...)
		return fill(out)
	}
}

// fnJSON_B returns the JSONB binary blob of its argument.
func fnJSON_B(args []interface{}) (interface{}, error) {
	v := util.UnwrapColumnValue(args[0])
	if b, ok := v.([]byte); ok {
		// jsonb(JSONB) passes the document through unchanged (sqlite
		// jsonArgIsJsonb header check).
		if !jsonbHeaderCheck(b) {
			return nil, jsonParseErr()
		}
		return b, nil
	}
	root, err := parseJSONArg(v)
	if err != nil {
		return nil, err
	}
	return jsonbEncodeElement(nil, root), nil
}

// jsonbWrap converts a text-JSON scalar function into its JSONB-returning
// jsonb_ variant: TEXT/JSONText results are re-encoded as a JSONB BLOB;
// scalars and errors pass through unchanged.
func jsonbWrap(fn func([]interface{}) (interface{}, error)) func([]interface{}) (interface{}, error) {
	return func(args []interface{}) (interface{}, error) {
		v, err := fn(args)
		if err != nil {
			return nil, err
		}
		switch t := v.(type) {
		case JSONText:
			return jsonbEncodeText(string(t))
		case string:
			return jsonbEncodeText(t)
		default:
			return v, nil
		}
	}
}

// jsonbEncodeText parses s and returns its JSONB encoding.
func jsonbEncodeText(s string) ([]byte, error) {
	n, err := parseJSON(s)
	if err != nil {
		return nil, err
	}
	return jsonbEncodeElement(nil, n), nil
}

// jsonbAgg adapts a text-JSON aggregate into its JSONB-returning jsonb_
// variant (jsonb_group_array / jsonb_group_object).
type jsonbAgg struct {
	inner Aggregator
}

func (a *jsonbAgg) Step(args []interface{}) error { return a.inner.Step(args) }

func (a *jsonbAgg) Final() (interface{}, error) {
	v, err := a.inner.Final()
	if err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case JSONText:
		return jsonbEncodeText(string(t))
	case string:
		return jsonbEncodeText(t)
	default:
		return v, nil
	}
}
