// JSON1 output side: the JSONText returned-JSON subtype, serialization of
// jsonNode trees, json_quote and json_pretty.

package function

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

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
		jsonPrettyObject(sb, n, indent, depth)
	case jsonArray:
		jsonPrettyArray(sb, n, indent, depth)
	default:
		sb.WriteString(jsonSerialize(n))
	}
}

// jsonPrettyObject renders an object, one member per line ("key" : value).
func jsonPrettyObject(sb *strings.Builder, n *jsonNode, indent string, depth int) {
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
}

// jsonPrettyArray renders an array, one element per line.
func jsonPrettyArray(sb *strings.Builder, n *jsonNode, indent string, depth int) {
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
