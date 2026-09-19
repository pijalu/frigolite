// JSON1 modifier functions: json_insert, json_replace, json_set,
// json_array_insert, json_remove and json_patch (src/json.c).

package function

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

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
			if pos, ok := jsonArrayPos(cur, c); ok {
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
		if err := jsonArrayInsertPair(root, toString(args[i]), args[i+1]); err != nil {
			return nil, err
		}
	}
	return JSONText(jsonSerialize(root)), nil
}

// jsonArrayInsertPair applies one P,V pair of json_array_insert: V is
// inserted into the array at path P. The final path component must be an
// array-element specifier ([N] or [#±N]); a parent that resolves to a
// non-array or an out-of-range position is a silent no-op.
func jsonArrayInsertPair(root *jsonNode, path string, rawValue interface{}) error {
	comps, err := jsonArrayInsertPath(path)
	if err != nil {
		return err
	}
	if len(comps) == 0 || !comps[len(comps)-1].isIdx {
		return fmt.Errorf("not an array element: '%s'", path)
	}
	val, verr := jsonInsertValue(rawValue)
	if verr != nil {
		return verr
	}
	cur, ok := jsonInsertParent(root, comps)
	if !ok || cur.kind != jsonArray {
		return nil // silent no-op (e.g. '$[0]' against an object root)
	}
	last := comps[len(comps)-1]
	pos := last.index
	if last.tail {
		pos = int64(len(cur.arr)) + last.index
	}
	if pos < 0 || pos > int64(len(cur.arr)) {
		return nil // out-of-range insert is ignored
	}
	arr := append(cur.arr, nil)
	copy(arr[pos+1:], arr[pos:])
	arr[pos] = val
	cur.arr = arr
	return nil
}

// jsonInsertParent resolves the container holding comps' final component,
// creating missing intermediates (json_insert creation rule). It reports
// false when an intermediate could not be traversed or created.
func jsonInsertParent(root *jsonNode, comps []jsonPathComponent) (*jsonNode, bool) {
	cur := root
	for j := 0; j < len(comps)-1; j++ {
		nxt, ok := jsonInsertStep(cur, comps[j], comps[j+1])
		if !ok {
			return nil, false
		}
		cur = nxt
	}
	return cur, true
}

// jsonArrayInsertPath parses the path of a json_array_insert pair. An
// unterminated final "[.." specifier is reported as a non-array-element
// target rather than a bad path.
func jsonArrayInsertPath(path string) ([]jsonPathComponent, error) {
	comps, perr := parseJSONPath(path)
	if perr != nil {
		if k := strings.LastIndexByte(path, '['); k >= 0 && !strings.Contains(path[k:], "]") {
			return nil, fmt.Errorf("not an array element: '%s'", path)
		}
		return nil, badJSONPath(path)
	}
	return comps, nil
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

// jsonArrayPos resolves an [N] / [#±N] path component against the array
// arr: '#' forms count back from the array end. It reports whether pos is
// an in-range element index.
func jsonArrayPos(arr *jsonNode, c jsonPathComponent) (int, bool) {
	pos := c.index
	if c.tail {
		pos += int64(len(arr.arr))
	}
	return int(pos), pos >= 0 && pos < int64(len(arr.arr))
}

// jsonRemoveStep resolves comps[i] during json_remove traversal (no
// creation; missing steps stop the walk).
func jsonRemoveStep(cur *jsonNode, c jsonPathComponent) (*jsonNode, bool) {
	if !c.isIdx {
		if cur.kind != jsonObject {
			return nil, false
		}
		return jsonObjectGet(cur, c.key)
	}
	if cur.kind != jsonArray {
		return nil, false
	}
	pos, ok := jsonArrayPos(cur, c)
	if !ok {
		return nil, false
	}
	return cur.arr[pos], true
}

func jsonRemoveAt(root *jsonNode, comps []jsonPathComponent) {
	cur := root
	for i, c := range comps {
		if i == len(comps)-1 {
			jsonRemoveLeaf(cur, c)
			return
		}
		nxt, ok := jsonRemoveStep(cur, c)
		if !ok {
			return
		}
		cur = nxt
	}
}

// jsonRemoveLeaf deletes the final path component from cur: an array
// element by resolved index, or an object member by key. Out-of-range
// indexes and missing keys are silently ignored.
func jsonRemoveLeaf(cur *jsonNode, c jsonPathComponent) {
	if c.isIdx {
		if cur.kind != jsonArray {
			return
		}
		if pos, ok := jsonArrayPos(cur, c); ok {
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
		jsonApplyPatchMember(target, pr)
	}
}

// jsonApplyPatchMember applies one patch member onto a target object per
// RFC-7396: null deletes the key, objects merge recursively, everything
// else replaces or appends the member.
func jsonApplyPatchMember(target *jsonNode, pr jsonPair) {
	if pr.value == nil || pr.value.kind == jsonNull {
		jsonObjectRemoveKey(target, pr.key)
		return
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
