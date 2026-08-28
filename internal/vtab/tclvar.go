package vtab

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pijalu/frigolite/internal/util"
)

// tclvarRegistry mirrors the TCL interpreter's variable table for the tclvar
// virtual table (src/test_tclvar.c): generated tests register every array
// element assignment (set arr(key) value) so `CREATE VIRTUAL TABLE ... USING
// tclvar` scans can expose (name, arrayname, value) rows.
var tclvarRegistry = struct {
	sync.Mutex
	rows map[string]string // "name(key)" -> value
}{rows: map[string]string{}}

var activeTclvarBases = map[string]bool{}

// TclVarSet records one TCL variable/array-element assignment for the tclvar
// virtual table. Generated tests call this from transpiled `set` statements:
// array elements pass a non-empty key; scalars pass an empty key.
func TclVarSet(name, key, value string) {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	if key == "" {
		tclvarRegistry.rows[name] = value
	} else {
		tclvarRegistry.rows[name+"("+key+")"] = value
	}
}

// tclvarFullname splits a fullname into (base, key): "arr(key)" -> arr,key;
// "scalar" -> scalar,"".
func tclvarFullname(fullname string) (base, key string) {
	open := strings.IndexByte(fullname, '(')
	if open < 0 || !strings.HasSuffix(fullname, ")") {
		return fullname, ""
	}
	return fullname[:open], strings.TrimSuffix(fullname[open+1:], ")")
}

// TclVarGet returns the value of a TCL variable/array element.
func TclVarGet(name, key string) string {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	if key == "" {
		return tclvarRegistry.rows[name]
	}
	return tclvarRegistry.rows[name+"("+key+")"]
}

// TclVarExists reports whether a registry entry (scalar or array element)
// has been set.
func TclVarExists(name, key string) bool {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	if key == "" {
		_, ok := tclvarRegistry.rows[name]
		return ok
	}
	_, ok := tclvarRegistry.rows[name+"("+key+")"]
	return ok
}

// MarkTclvarBase records base as an array backed by the tclvar registry.
func MarkTclvarBase(base string) { activeTclvarBases[base] = true }

// IsTclvarBase reports whether base is registry-backed.
func IsTclvarBase(base string) bool { return activeTclvarBases[base] }

// TclVarDelete removes one variable entry.
func TclVarDelete(name, key string) {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	if key == "" {
		delete(tclvarRegistry.rows, name)
	} else {
		delete(tclvarRegistry.rows, name+"("+key+")")
	}
}

// TclVarReset clears the registry (called when a fresh test file starts).
func TclVarReset() {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	tclvarRegistry.rows = map[string]string{}
}

// vtabOmitMode reads the ::tclvar_set_omit interpreter variable mirrored in
// the registry by generated tests.
func vtabOmitMode() string {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	return tclvarRegistry.rows["tclvar_set_omit"]
}

// tclvarModule implements the tclvar eponymous-only virtual table.
// Schema: name TEXT, arrayname TEXT, value TEXT.
type TclVarModule struct{}

// NewTclVarModule builds the tclvar module.
func NewTclVarModule() *TclVarModule { return &TclVarModule{} }

// Eponymous implements EponymousModule: test_tclvar.c registers
// xCreate != 0, so the table can also be created explicitly.
func (m *TclVarModule) Eponymous() bool { return true }

// tclvarVTab serves the interpreter variable table.
type tclvarVTab struct {
	matchCol string // column a MATCH constraint consumed ("" = none)
	matchPat string // TCL string-match pattern (glob syntax)
}

// SetMatchConstraint implements MatchConstraintSetter: absorbs
// `col MATCH 'pattern'` (test_tclvar.c style name probes).
func (v *tclvarVTab) SetMatchConstraint(column, target string) {
	switch strings.ToLower(column) {
	case "name", "f", "fullname":
		v.matchCol = strings.ToLower(column)
		v.matchPat = target
	}
}

// tclStrMatch reports whether str matches TCL's string match pattern
// (glob syntax: * ? [chars] [^chars]; backslash escapes are ignored because
// the corpus patterns never rely on them).
func tclStrMatch(pattern, str string) bool {
	return matchHere(pattern, str)
}

func matchHere(pat, s string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			// collapse consecutive stars
			for len(pat) > 0 && pat[0] == '*' {
				pat = pat[1:]
			}
			if pat == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchHere(pat, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			pat, s = pat[1:], s[1:]
		case '[':
			if s == "" {
				return false
			}
			end := strings.IndexByte(pat, ']')
			if end < 0 {
				return false
			}
			set := pat[1:end]
			negated := strings.HasPrefix(set, "^")
			set = strings.TrimPrefix(set, "^")
			matched := matchCharSet(set, s[0])
			if negated {
				matched = !matched
			}
			if !matched {
				return false
			}
			pat, s = pat[end+1:], s[1:]
		default:
			if s == "" || pat[0] != s[0] {
				return false
			}
			pat, s = pat[1:], s[1:]
		}
	}
	return s == ""
}

// matchCharSet reports whether c belongs to one TCL [...] character class
// body (ranges like a-z included).
func matchCharSet(set string, c byte) bool {
	for i := 0; i < len(set); i++ {
		if i+2 < len(set) && set[i+1] == '-' {
			if c >= set[i] && c <= set[i+2] {
				return true
			}
			i += 2
			continue
		}
		if set[i] == c {
			return true
		}
	}
	return false
}

// CountOperatorOverloads implements OperatorOverloadCounter: tclvar mirrors
// test_tclvar.c reading the ::tclvar_set_omit interpreter variable — when set
// truthy the instance consumes operator constraints without the core probing
// the overridden like()/glob()/regexp() functions.
func (v *tclvarVTab) CountOperatorOverloads() bool {
	return vtabOmitMode() != "1"
}

// Create implements Module.
func (m *TclVarModule) Create(args []string) (VirtualTable, error) {
	return &tclvarVTab{}, nil
}

// Connect implements Module.
func (m *TclVarModule) Connect(args []string) (VirtualTable, error) {
	return &tclvarVTab{}, nil
}

// Columns implements ColumnInfo (test_tclvar.c schema).
func (v *tclvarVTab) Columns() []string {
	return []string{"name", "arrayname", "value", "fullname"}
}

// BestIndex accepts the default plan.
func (v *tclvarVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open snapshots the registry rows sorted by "name(key)" so scans are
// deterministic.
func (v *tclvarVTab) Open() (Cursor, error) {
	tclvarRegistry.Lock()
	keys := make([]string, 0, len(tclvarRegistry.rows))
	for k := range tclvarRegistry.rows {
		keys = append(keys, k)
	}
	vals := make(map[string]string, len(keys))
	for _, k := range keys {
		vals[k] = tclvarRegistry.rows[k]
	}
	tclvarRegistry.Unlock()
	sort.Strings(keys)
	// MATCH constraint absorption: keep only rows whose matched field
	// satisfies the TCL string-match pattern.
	if v.matchCol != "" && v.matchPat != "" {
		kept := make([]string, 0, len(keys))
		for _, k := range keys {
			base, key := tclvarFullname(k)
			cand := base
			switch v.matchCol {
			case "fullname":
				cand = k
			case "f":
				cand = k
			case "name":
				cand = base
			}
			if key == "" || v.matchCol == "fullname" {
				if tclStrMatch(v.matchPat, cand) {
					kept = append(kept, k)
				}
			}
		}
		keys = kept
	}
	c := &tclvarCursor{keys: keys, vals: vals}
	return c, nil
}

// tclvarCursor walks the registered variable rows.
type tclvarCursor struct {
	keys    []string
	vals    map[string]string
	idx     int
	started bool
	done    bool
}

// Next advances; the first call serves row 0.
func (c *tclvarCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		return len(c.keys) > 0
	}
	c.idx++
	if c.idx >= len(c.keys) {
		c.done = true
		return false
	}
	return true
}

// Column serves name / arrayname / value / fullname parsed from the registry
// key ("name(key)" for array elements, plain "name" for scalars).
func (c *tclvarCursor) Column(idx int) (interface{}, error) {
	k := c.keys[c.idx]
	base, key := tclvarFullname(k)
	switch idx {
	case 0:
		return base, nil
	case 1:
		if key == "" {
			return nil, nil // scalars have no array index
		}
		return key, nil
	case 2:
		return c.vals[k], nil
	case 3:
		return k, nil
	}
	return nil, fmt.Errorf("tclvar: invalid column index %d", idx)
}

// Close implements Cursor.
func (c *tclvarCursor) Close() error { return nil }

// UpdateRow implements RowUpdater: applies value/fullname changes, including
// renames (delete the old key when fullname changed).
func (v *tclvarVTab) UpdateRow(oldValues, newValues []interface{}) error {
	getFull := func(vals []interface{}) (string, bool) {
		if len(vals) > 3 && vals[3] != nil {
			if str, isStr := vals[3].(string); isStr {
				return str, true
			}
		}
		return "", false
	}
	oldFull, hasOld := getFull(oldValues)
	newFull, hasNew := getFull(newValues)
	if !hasOld || !hasNew {
		return fmt.Errorf("tclvar: fullname is required")
	}
	var val interface{}
	if len(newValues) > 2 {
		val = newValues[2]
	}
	oldBase, oldKey := tclvarFullname(oldFull)
	newBase, newKey := tclvarFullname(newFull)
	if val == nil {
		TclVarDelete(oldBase, oldKey)
		return nil
	}
	text := fmt.Sprintf("%v", util.UnwrapColumnValue(val))
	if oldFull != newFull {
		TclVarDelete(oldBase, oldKey)
	}
	TclVarSet(newBase, newKey, text)
	return nil
}

// DeleteRow implements RowUpdater: removes the variable named by the old
// row's fullname.
func (v *tclvarVTab) DeleteRow(oldValues []interface{}) error {
	if len(oldValues) > 3 && oldValues[3] != nil {
		if full, isStr := oldValues[3].(string); isStr {
			base, key := tclvarFullname(full)
			TclVarDelete(base, key)
			return nil
		}
	}
	return fmt.Errorf("tclvar: fullname is required")
}

// InsertRow implements RowUpdater: INSERT INTO tclvar(fullname, value)
// sets (or clears, when value is NULL) the variable named by fullname
// (test_tclvar.c xUpdate parity).
func (v *tclvarVTab) InsertRow(values []interface{}) (int64, error) {
	if len(values) <= 3 || values[3] == nil {
		return 0, fmt.Errorf("tclvar: fullname is required")
	}
	fullname, _ := values[3].(string)
	var val interface{}
	if len(values) > 2 {
		val = values[2]
	}
	base, key := tclvarFullname(fullname)
	if val == nil {
		TclVarDelete(base, key)
	} else {
		TclVarSet(base, key, fmt.Sprintf("%v", util.UnwrapColumnValue(val)))
	}
	return 0, nil
}

// TclArrayNames returns the sorted key list of an array variable
// (`array names arr` parity): elements registered via TclVarSet with a
// non-empty key under base.
func TclArrayNames(base string) []string {
	tclvarRegistry.Lock()
	defer tclvarRegistry.Unlock()
	prefix := base + "("
	var out []string
	for k := range tclvarRegistry.rows {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, ")") {
			out = append(out, k[len(prefix):len(k)-1])
		}
	}
	sort.Strings(out)
	return out
}
