package vtab

// JsonEachModule implements the json_each table-valued function (SQLite
// json.c jsonEachModule): given a JSON argument it yields one row per child
// element — for an object, one row per member; for an array, one row per
// element; a scalar argument yields a single row.
//
// The implementation walks the JSONB-encoded document exactly like SQLite:
// element ids are BYTE OFFSETS into the JSONB blob (jsonEachColumn JEACH_ID
// returns p->i), so ids are stable and match the oracle ({"a":1,"b":2} →
// ids 1 and 5).
//
// Column layout matches SQLite's json_each:
//
//	key TEXT, value ANY, type TEXT, atom ANY,
//	id INTEGER, parent INTEGER, fullkey TEXT, path TEXT, json HIDDEN
//
// JsonTreeModule implements json_tree: the same column layout, but rows for
// EVERY node including the root ('$') and all descendants, emitted
// depth-first with container rows before their children; parent is the id of
// the enclosing element.

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/function"
)

// JSONB element-type nibbles (src/json.c).
const (
	jbArray  = 11
	jbObject = 12
)

// JsonEachModule implements json_each(J[,P]).
type JsonEachModule struct{}

// JsonTreeModule implements json_tree(J[,P]).
type JsonTreeModule struct{}

type jsonEachVTab struct {
	rows [][]interface{}
	cols []string
}

func (m *JsonEachModule) Create(args []string) (VirtualTable, error) {
	return buildJSONEachRows(stringsToValues(args), false)
}

func (m *JsonEachModule) Connect(args []string) (VirtualTable, error) {
	return buildJSONEachRows(stringsToValues(args), false)
}

// CreateWithValues implements ValueModule: json_each accepts BLOB (JSONB)
// arguments that string rendering would destroy.
func (m *JsonEachModule) CreateWithValues(args []interface{}) (VirtualTable, error) {
	return buildJSONEachRows(args, false)
}

// ConnectWithValues implements ValueModule.
func (m *JsonEachModule) ConnectWithValues(args []interface{}) (VirtualTable, error) {
	return buildJSONEachRows(args, false)
}

func (m *JsonTreeModule) Create(args []string) (VirtualTable, error) {
	return buildJSONEachRows(stringsToValues(args), true)
}

func (m *JsonTreeModule) Connect(args []string) (VirtualTable, error) {
	return buildJSONEachRows(stringsToValues(args), true)
}

// CreateWithValues implements ValueModule.
func (m *JsonTreeModule) CreateWithValues(args []interface{}) (VirtualTable, error) {
	return buildJSONEachRows(args, true)
}

// ConnectWithValues implements ValueModule.
func (m *JsonTreeModule) ConnectWithValues(args []interface{}) (VirtualTable, error) {
	return buildJSONEachRows(args, true)
}

// stringsToValues adapts plain string module args to SQL values.
func stringsToValues(args []string) []interface{} {
	vals := make([]interface{}, len(args))
	for i, a := range args {
		if a == "" {
			vals[i] = nil
		} else {
			vals[i] = a
		}
	}
	return vals
}

// jeParent mirrors sqlite's JsonParent struct: one entry per enclosing
// container on the traversal stack.
type jeParent struct {
	iKey   int64 // next array index (objects use labels instead)
	iHead  int   // id reported as parent for children (the label elem for object members)
	iEnd   int   // blob offset just past the container payload
	iValue int   // blob offset of the container's value element
	nPath  int   // path length when descended (restored on pop)
}

// jeWalk mirrors sqlite's JsonEachCursor: the traversal state over the blob.
type jeWalk struct {
	doc       *function.JSONBlob
	i, iEnd   int
	eType     byte // 0 = not positioned in a container; else container type
	parents   []jeParent
	path      []byte
	recursive bool
	rowid     int64
	nRoot     int // length of the root-path prefix of path
	basePath  int // root path with its final accessor stripped (0 = n/a)
	src       string
}

// buildJSONEachRows materializes the json_each/json_tree row set for the
// given evaluated arguments (args[0]=J, args[1]=optional root path P).
// A NULL J yields zero rows (SQLite treats NULL input as empty).
func buildJSONEachRows(args []interface{}, tree bool) (*jsonEachVTab, error) {
	v := &jsonEachVTab{cols: []string{"key", "value", "type", "atom", "id", "parent", "fullkey", "path", "json"}}
	name := "json_each"
	if tree {
		name = "json_tree"
	}
	if len(args) < 1 || args[0] == nil || args[0] == "" {
		return v, nil
	}
	doc, ok, err := function.AsJSONBlob(args[0])
	if err != nil {
		return nil, fmt.Errorf("%s: malformed JSON", name)
	}
	if !ok {
		return v, nil
	}
	w := &jeWalk{doc: doc, recursive: tree, src: fmt.Sprint(args[0]), path: []byte("$"), nRoot: 1}
	root := ""
	if len(args) > 1 {
		if args[1] == nil {
			// sqlite jsonEachFilter: a NULL root returns zero rows
			// (SQLITE_OK with an empty cursor).
			return v, nil
		}
		root, _ = args[1].(string)
	}
	if err := w.filter(root, name); err != nil {
		return nil, err
	}
	for !w.eof() {
		v.rows = append(v.rows, w.row())
		w.next()
	}
	return v, nil
}

// filter positions the walker at the first row (sqlite jsonEachFilter).
func (w *jeWalk) filter(root, name string) error {
	w.i, w.eType = 0, 0
	if root == "" {
		// Whole document: i stays 0; the common tail below handles
		// container descent for json_each.
	} else {
		if !strings.HasPrefix(root, "$") {
			return fmt.Errorf("%s: bad JSON path: %s", name, root)
		}
		if root == "$" {
			w.i, w.eType = 0, 0
		} else {
			label, val, err := w.doc.Lookup(root)
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					w.i, w.iEnd = 0, 0 // no such element: zero rows
					return nil
				}
				return fmt.Errorf("%s: bad JSON path: %s", name, root)
			}
			w.i = val
			if label >= 0 {
				w.i = label
				w.eType = jbObject
			} else {
				w.eType = jbArray
			}
		}
		w.path = []byte(root)
		w.nRoot = len(root)
		w.basePath = rootBasePath(root)
	}
	// i (valueIdx below) is the VALUE element driving iEnd/descent; when a
	// label was resolved, p->i points at the label but sizing uses the value.
	valueIdx := w.i
	if root != "" && root != "$" && w.eType == jbObject {
		// p->i points at the LABEL element; sizing/descent use the VALUE
		// element that follows it.
		nl, sl := w.doc.HeaderSize(w.i)
		valueIdx = w.i + nl + sl
	}
	n, sz := w.doc.HeaderSize(valueIdx)
	w.iEnd = valueIdx + n + sz
	t := w.doc.ElemType(valueIdx)
	if !w.recursive && t >= jbArray {
		w.i = valueIdx + n
		w.eType = t
		w.parents = append(w.parents, jeParent{iKey: 0, iEnd: w.iEnd, iHead: w.i, iValue: valueIdx})
	}
	return nil
}

func (w *jeWalk) eof() bool { return w.i >= w.iEnd }

// skipLabel returns the value-element index for the current position: for
// object members the label element at p->i is skipped; otherwise the current
// position already is the value (sqlite jsonSkipLabel). When the element
// after the label is missing or truncated (corrupt JSONB tail), the label
// index itself is returned so the value appears to be the label — sqlite
// Bug 2026-07-04T04:58:54Z (json101-26.1: value = key = "eee").
func (w *jeWalk) skipLabel() int {
	if w.eType == jbObject {
		n, sz := w.doc.HeaderSize(w.i)
		next := w.i + n + sz
		if !w.doc.ElementComplete(next) {
			return w.i
		}
		return next
	}
	return w.i
}

// next advances the cursor (sqlite jsonEachNext).
func (w *jeWalk) next() {
	if w.recursive {
		levelChange := false
		i := w.skipLabel()
		x := w.doc.ElemType(i)
		n, sz := w.doc.HeaderSize(i)
		if n == 0 {
			// The element does not fit in the blob (corrupt tail): jump to
			// the scan end so the walk terminates instead of stalling.
			w.i = w.iEnd
		} else if x == jbObject || x == jbArray {
			levelChange = true
			nPath := len(w.path)
			if w.eType != 0 && len(w.parents) > 0 {
				w.appendPathName()
			}
			w.parents = append(w.parents, jeParent{
				iHead:  w.i,
				iValue: i,
				iEnd:   i + n + sz,
				iKey:   -1,
				nPath:  nPath,
			})
			w.i = i + n
		} else {
			w.i = i + n + sz
		}
		for len(w.parents) > 0 && w.i >= w.parents[len(w.parents)-1].iEnd {
			p := w.parents[len(w.parents)-1]
			w.parents = w.parents[:len(w.parents)-1]
			w.path = w.path[:p.nPath]
			levelChange = true
		}
		if levelChange {
			if len(w.parents) > 0 {
				top := w.parents[len(w.parents)-1]
				w.eType = w.doc.ElemType(top.iValue)
			} else {
				w.eType = 0
			}
		}
	} else {
		i := w.skipLabel()
		n, sz := w.doc.HeaderSize(i)
		if n == 0 {
			// Corrupt tail: terminate the scan (see the recursive branch).
			w.i = w.iEnd
		} else {
			w.i = i + n + sz
		}
	}
	if w.eType == jbArray && len(w.parents) > 0 {
		w.parents[len(w.parents)-1].iKey++
	}
	w.rowid++
}

// appendPathName extends path with the current element's accessor
// (sqlite jsonAppendPathName): "[N]" for arrays, ".name" or
// ".\"quoted\"" for object members.
func (w *jeWalk) appendPathName() {
	top := &w.parents[len(w.parents)-1]
	if w.eType == jbArray {
		w.path = append(w.path, fmt.Sprintf("[%d]", top.iKey)...)
		return
	}
	key := w.doc.Key(w.i)
	needQuote := len(key) == 0 || !isAlpha(key[0])
	for i := 0; i < len(key) && !needQuote; i++ {
		if !isAlnum(key[i]) {
			needQuote = true
		}
	}
	if needQuote {
		// Quoted keys are JSON-escaped (" and control characters) so the
		// resulting path stays parseable.
		w.path = append(w.path, '.')
		w.path = append(w.path, function.JSONEscapeString(key)...)
	} else {
		w.path = append(w.path, '.')
		w.path = append(w.path, key...)
	}
}

func isAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isAlnum(c byte) bool {
	return isAlpha(c) || c >= '0' && c <= '9'
}

// row builds the output row for the current position
// (sqlite jsonEachColumn).
func (w *jeWalk) row() []interface{} {
	row := make([]interface{}, 9)
	vi := w.skipLabel()
	isScalarRoot := len(w.parents) == 0

	// key
	switch {
	case isScalarRoot && w.nRoot == 1:
		row[0] = nil
	case isScalarRoot:
		row[0] = rootPathKey(string(w.path))
	case w.eType == jbObject:
		row[0] = w.doc.Key(w.i)
	default: // array
		row[0] = w.parents[len(w.parents)-1].iKey
	}

	val := w.doc.SQLValue(vi)
	row[1] = val
	row[2] = w.doc.TypeName(vi)
	if w.doc.IsContainer(vi) {
		row[3] = nil
	} else {
		row[3] = val
	}
	row[4] = int64(w.i)
	if w.recursive && len(w.parents) > 0 {
		row[5] = int64(w.parents[len(w.parents)-1].iHead)
	}
	fullkey := string(w.path)
	if len(w.parents) > 0 {
		// fullkey = base path + this element's accessor; compute without
		// mutating w.path (save/restore mirrors the C code's nUsed save).
		save := len(w.path)
		w.appendPathName()
		fullkey = string(w.path)
		w.path = w.path[:save]
	}
	row[6] = fullkey
	row[7] = string(w.path[:w.pathLength()])
	row[8] = w.src
	return row
}

// pathLength reports the path-column length (sqlite jsonEachPathLength).
// The rowid==0 recursive trim only matters for deep root arguments; the
// plain buffer length covers every other case.
func (w *jeWalk) pathLength() int {
	if len(w.parents) == 0 && w.basePath > 0 {
		// Top-level row under a deep root argument: sqlite reports the
		// PARENT container's path ($."\x17" root → path "$").
		return w.basePath
	}
	return len(w.path)
}

// rootBasePath returns the length of the root path with its final accessor
// removed ("$.a" → 2 for "$", "$[3]" → 1, quoted forms included). This is
// the path-column value for rows whose parent chain is empty under a deep
// root.
func rootBasePath(root string) int {
	if strings.HasSuffix(root, "]") {
		if i := strings.LastIndexByte(root, '['); i > 0 {
			return i
		}
	}
	if strings.HasSuffix(root, "\"") {
		for i := len(root) - 2; i > 0; i-- {
			if root[i] == '"' {
				if i > 0 && root[i-1] == '.' {
					return i - 1
				}
				return i
			}
		}
	}
	if i := strings.LastIndexByte(root, '.'); i > 0 {
		return i
	}
	return 1
}

// rootPathKey derives the key column for a scalar root targeted through a
// deeper root path (sqlite reads it back out of the path buffer): "$[3]"
// yields integer 3; "$.a"/$."a b" yield the final label text.
func rootPathKey(root string) interface{} {
	if i := strings.LastIndexByte(root, '['); i >= 0 && strings.HasSuffix(root, "]") {
		var n int64
		if _, err := fmt.Sscanf(root[i+1:len(root)-1], "%d", &n); err == nil {
			return n
		}
	}
	s := root[strings.LastIndexByte(root, '.')+1:]
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return s
}

func (v *jsonEachVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// HiddenColumns marks the trailing source-text column ('json', index 8) as
// HIDDEN so it never appears in `SELECT *` expansion (SQLite declares it
// HIDDEN in jsonEachModule's column list).
func (v *jsonEachVTab) HiddenColumns() map[int]bool { return map[int]bool{8: true} }

func (v *jsonEachVTab) Columns() []string { return v.cols }

func (v *jsonEachVTab) Open() (Cursor, error) {
	return &jsonEachCursor{rows: v.rows, pos: -1}, nil
}

type jsonEachCursor struct {
	rows [][]interface{}
	pos  int
}

func (c *jsonEachCursor) Next() bool {
	c.pos++
	return c.pos < len(c.rows)
}

func (c *jsonEachCursor) Column(idx int) (interface{}, error) {
	// Out-of-range idx MUST return an error: readVtabRows probes columns
	// until the first error to detect row width.
	if c.pos >= len(c.rows) {
		return nil, fmt.Errorf("json_each: no current row")
	}
	if idx >= len(c.rows[c.pos]) {
		return nil, fmt.Errorf("json_each: invalid column index %d", idx)
	}
	return c.rows[c.pos][idx], nil
}

func (c *jsonEachCursor) Close() error { return nil }
