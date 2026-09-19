// JSON1 path machinery: "$.key[4]" path parsing (SQLite jsonParsePath) and
// json_extract / the -> and ->> SQL operators.

package function

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

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
