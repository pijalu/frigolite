// carray table-valued function — Go port of SQLite's ext carray.c.
//
// The function returns the values of a C-language array. In C the array is
// identified by a raw pointer bound with sqlite3_bind_pointer(..., "carray");
// in frigolite the equivalent opaque handle is a *CArrayHandle value produced
// by the inttoptr() scalar (the tabfunc01/carray test-harness analog of
// sqlite3_carray_bind). Any other pointer value (text, integer, NULL)
// resolves to no array and the table is empty, mirroring
// sqlite3_value_pointer() returning 0.
//
// Schema (carrayConnect):
//
//	CREATE TABLE x(value, pointer HIDDEN, count HIDDEN, ctype HIDDEN)
package vtab

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// azCarrayType lists the allowed ctype names (carray.c azCarrayType).
var azCarrayType = []string{"int32", "int64", "double", "char*", "struct iovec"}

// carrayAddrRegistry maps an address token ("intarray_addr 5 7 ...") to the
// single shared array it names. The tabfunc01/carray tests rely on identity:
// inttoptr(PTR) twice yields the SAME storage, so remember(123, inttoptr(PTR))
// is visible through a later carray(inttoptr(PTR), ...) read.
var (
	carrayAddrMu       sync.Mutex
	carrayAddrRegistry = map[string]*CArrayHandle{}
)

// carrayAddrTags maps address-token prefixes to element type names
// (the test harness builds int/int64/double/text arrays).
var carrayAddrTags = []struct {
	tag   string
	ctype string
}{
	{"intarray_addr", "int32"},
	{"int64array_addr", "int64"},
	{"doublearray_addr", "double"},
	{"textarray_addr", "char*"},
}

// CArrayHandle is the opaque analog of a "carray"-typed bound pointer.
type CArrayHandle struct {
	Values []interface{} // array elements in storage order
	Type   string        // one of azCarrayType; empty means int32 (C default)
}

// carray_column numbers (carray.c).
const (
	carrayColumnValue   = 0
	carrayColumnPointer = 1
	carrayColumnCount   = 2
	carrayColumnCType   = 3
)

// CarrayModule implements the carray virtual table. Like series.c it is
// registered with an xCreate (Create == Connect), so CREATE VIRTUAL TABLE
// USING carray succeeds; the bare name is also usable in FROM because the
// engine treats registered modules as eponymous when unconstrained rows are
// served by Connect-time state.
type CarrayModule struct{}

// Create creates a carray instance (xCreate == xConnect in carray.c).
func (m *CarrayModule) Create(args []string) (VirtualTable, error) { return m.Connect(args) }

// Connect creates a carray instance; all bindings arrive per-query through
// hidden-column constraints / function arguments (carrayFilter).
func (m *CarrayModule) Connect(args []string) (VirtualTable, error) {
	return &carrayVTab{count: -1}, nil
}

// CreateWithValues binds function-call arguments by value (ValueModule):
// carray(ptr), carray(ptr, count), carray(ptr, count, ctype).
func (m *CarrayModule) CreateWithValues(args []interface{}) (VirtualTable, error) {
	return m.ConnectWithValues(args)
}

// Eponymous reports the module name is directly usable in FROM (carray.c
// registers xCreate == xConnect, making it an eponymous virtual table).
func (m *CarrayModule) Eponymous() bool { return true }

// ConnectWithValues binds function-call arguments by value.
func (m *CarrayModule) ConnectWithValues(args []interface{}) (VirtualTable, error) {
	v := &carrayVTab{count: -1}
	if err := v.bindArgs(args); err != nil {
		return nil, err
	}
	return v, nil
}

// carrayVTab is one virtual-table instance.
type carrayVTab struct {
	values []interface{} // resolved array elements; nil slice = unbound
	ctype  string        // active element type name ("" until resolved)
	count  int64         // constrained count (-1 = follow the handle length)
}

// Columns reports the declared schema; pointer/count/ctype are HIDDEN.
func (v *carrayVTab) Columns() []string {
	return []string{"value", "pointer", "count", "ctype"}
}

// HiddenColumns marks pointer/count/ctype hidden (carray.c schema).
func (v *carrayVTab) HiddenColumns() map[int]bool {
	return map[int]bool{carrayColumnPointer: true, carrayColumnCount: true, carrayColumnCType: true}
}

// BestIndex satisfies the Module plumbing; constraint handling is done by the
// engine's hidden-column pushdown (SetHiddenConstraint below).
func (v *carrayVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// Open returns a cursor over the resolved values (carrayOpen).
func (v *carrayVTab) Open() (Cursor, error) {
	return &carrayCursor{v: v, rowid: 0}, nil
}

// SetHiddenConstraint absorbs one WHERE equality on pointer/count/ctype
// (carrayFilter argv parity). The order of arrivals mirrors idxNum:
//   - pointer alone            → bind form: the handle must carry its own data
//   - pointer + count          → int32 semantics over the handle
//   - pointer + count + ctype  → named element type over the handle
//
// A pointer value that is not a *CArrayHandle yields an empty table
// (sqlite3_value_pointer == 0), never an error.
func (v *carrayVTab) SetHiddenConstraint(col string, val interface{}) error {
	switch strings.ToLower(col) {
	case "pointer":
		if h, ok := val.(*CArrayHandle); ok && h != nil {
			v.values = h.Values
			v.ctype = h.Type
		}
	case "count":
		// The count only matters together with a usable pointer; the cursor
		// clamps to len(values) like carray.c (iCnt from the same argv).
		if n, ok := asInt64(val); ok && n >= 0 {
			v.count = n
		}
	case "ctype":
		name, ok := val.(string)
		if !ok {
			return fmt.Errorf("unknown datatype: '%s'", fmt.Sprint(val))
		}
		if err := v.validateCType(name); err != nil {
			return err
		}
	}
	return nil
}

func (v *carrayVTab) validateCType(name string) error {
	for _, t := range azCarrayType {
		if strings.EqualFold(name, t) {
			v.ctype = name
			return nil
		}
	}
	// SQLite formats this with %Q (single-quoted).
	return fmt.Errorf("unknown datatype: '%s'", name)
}

// bindArgs applies function-call arguments: ptr[,count[,ctype]] — the SQL
// argument list maps onto carrayFilter's argv for idxNum 1..3.
func (v *carrayVTab) bindArgs(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil // unconstrained: empty table
	}
	h, ok := args[0].(*CArrayHandle)
	if !ok || h == nil {
		return nil // non-pointer argument: sqlite3_value_pointer == 0
	}
	if len(args) >= 2 {
		// Explicit count: serve min(count, handle length) elements with
		// int32 semantics by default (idxNum 2), honoring a trailing ctype
		// (idxNum 3).
		n, _ := asInt64(args[1])
		if n < 0 || n > int64(len(h.Values)) {
			n = int64(len(h.Values))
		}
		typ := "int32"
		if len(args) >= 3 {
			name, ok := args[2].(string)
			if !ok {
				return fmt.Errorf("unknown datatype: %q", fmt.Sprint(args[2]))
			}
			if err := v.validateCType(name); err != nil {
				return err
			}
			typ = name
		}
		v.values = convertCArray(h.Values[:n], typ)
		v.ctype = typ
		return nil
	}
	// Single argument: bind-form parity — the handle supplies its own type.
	v.values = convertCArray(h.Values, handleType(h))
	v.ctype = handleType(h)
	return nil
}

func handleType(h *CArrayHandle) string {
	if h.Type == "" {
		return "int32"
	}
	return h.Type
}

// convertCArray reinterprets storage values under the named element type.
// Values arrive as Go scalars already decoded from the handle's storage;
// numeric reinterpretation is lossless for int32/int64 and widens to float64
// for double. char* renders scalars as text; struct iovec renders blobs
// verbatim (both only occur for handles explicitly built that way).
func convertCArray(vals []interface{}, typ string) []interface{} {
	out := make([]interface{}, len(vals))
	for i, e := range vals {
		switch strings.ToLower(typ) {
		case "double":
			if n, ok := asInt64(e); ok {
				out[i] = float64(n)
			} else if f, ok := e.(float64); ok {
				out[i] = f
			} else {
				out[i] = e
			}
		case "char*":
			out[i] = fmt.Sprint(e)
		default:
			out[i] = e
		}
	}
	return out
}

// carrayCursor scans the resolved values; rowid is 1-based (carrayRowid).
type carrayCursor struct {
	v     *carrayVTab
	rowid int64
}

// Next advances; EOF when rowid exceeds the usable element count
// (carrayEof: pCur->iRowid > pCur->iCnt).
func (c *carrayCursor) Next() bool {
	limit := int64(len(c.v.values))
	if c.v.count >= 0 && c.v.count < limit {
		limit = c.v.count
	}
	if c.rowid+1 > limit {
		return false
	}
	c.rowid++
	return true
}

// Column serves one row: value at idx 0; pointer NULL; count/ctype echo the
// binding (carrayColumn).
func (c *carrayCursor) Column(idx int) (interface{}, error) {
	limit := int64(len(c.v.values))
	if c.v.count >= 0 && c.v.count < limit {
		limit = c.v.count
	}
	switch idx {
	case carrayColumnValue:
		if c.rowid-1 < 0 || c.rowid-1 >= limit {
			return nil, fmt.Errorf("carray: row %d out of range", c.rowid)
		}
		return c.v.values[c.rowid-1], nil
	case carrayColumnPointer:
		return nil, nil
	case carrayColumnCount:
		return limit, nil
	case carrayColumnCType:
		if c.v.ctype == "" {
			return nil, nil
		}
		return c.v.ctype, nil
	}
	return nil, fmt.Errorf("carray: invalid column index %d", idx)
}

// Close releases the cursor (nothing to free).
func (c *carrayCursor) Close() error { return nil }

// Rowid reports the 1-based position (carrayRowid).
func (c *carrayCursor) Rowid() int64 { return c.rowid }

// IntToptrFunc implements the test-harness inttoptr() scalar (tabfunc01 /
// carray tests): the text '<tag> V1 V2 ...' names a C array whose elements
// follow the tag; the return value is an opaque *CArrayHandle, the analog of
// a pointer bound with sqlite3_bind_pointer(..., "carray"). Handles are
// interned per address string so all references share storage.
func IntToptrFunc(args []interface{}) (interface{}, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("inttoptr() expects exactly 1 argument")
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("inttoptr() expects a text address")
	}
	var typ string
	for _, t := range carrayAddrTags {
		if strings.HasPrefix(s, t.tag) {
			typ = t.ctype
			break
		}
	}
	if typ == "" {
		// Not an array address: pass through as an unresolvable pointer.
		return s, nil
	}
	carrayAddrMu.Lock()
	defer carrayAddrMu.Unlock()
	if h, ok := carrayAddrRegistry[s]; ok {
		return h, nil
	}
	fields := strings.Fields(s[strings.IndexAny(s, " ")+1:])
	vals := make([]interface{}, 0, len(fields))
	for _, f := range fields {
		v, err := parseAddrElement(f, typ)
		if err != nil {
			return nil, fmt.Errorf("inttoptr(): bad array element %q", f)
		}
		vals = append(vals, v)
	}
	h := &CArrayHandle{Values: vals, Type: typ}
	carrayAddrRegistry[s] = h
	return h, nil
}

// RememberFunc implements the test-harness remember(X, PTR) scalar
// (ext/misc/remember.c analog): stores X into the first element of the
// array addressed by PTR and returns X.
func RememberFunc(args []interface{}) (interface{}, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("remember() expects exactly 2 arguments")
	}
	h, ok := args[1].(*CArrayHandle)
	if !ok || h == nil || len(h.Values) == 0 {
		return args[0], nil
	}
	switch strings.ToLower(h.Type) {
	case "double":
		if n, ok := asInt64(args[0]); ok {
			h.Values[0] = float64(n)
		} else if f, ok := args[0].(float64); ok {
			h.Values[0] = f
		} else {
			h.Values[0] = args[0]
		}
	default:
		h.Values[0] = args[0]
	}
	return args[0], nil
}

// parseAddrElement decodes one address-array element per element type.
// Numeric fields are parsed from their longest valid prefix: the transpiled
// tests synthesize addresses like "<base>+16" whose trailing offset junk
// must not reject the array.
func parseAddrElement(field, typ string) (interface{}, error) {
	switch typ {
	case "double":
		return strconv.ParseFloat(field, 64)
	case "char*", "struct iovec":
		return field, nil
	default:
		end := len(field)
		for end > 0 {
			if n, err := strconv.ParseInt(field[:end], 10, 64); err == nil {
				return n, nil
			}
			end--
		}
		return nil, fmt.Errorf("not an integer: %q", field)
	}
}

// asInt64 coerces common numeric shapes.
// AsVtabInt64 exposes the shared numeric coercion for other packages
// (engine-side scalar wrappers).
func AsVtabInt64(v interface{}) (int64, bool) { return asInt64(v) }

func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		if p, err := strconv.ParseInt(n, 0, 64); err == nil {
			return p, true
		}
	}
	return 0, false
}
