package vtab

import (
	"fmt"
	"sync"
)

// intarrayStore holds the integer arrays bound to named intarray virtual
// tables (src/test_intarray.c). Each array is associated with a table name;
// sqlite3_intarray_bind replaces the array, and the vtab cursor snapshots it
// at Open() time. The arrays are populated through the test-only
// sqlite3_intarray_create / sqlite3_intarray_bind C-API, emulated by the
// tcl2go harness command handlers.
var intarrayStore = struct {
	sync.Mutex
	tables map[string][]int64 // vtab name -> bound integer array
}{tables: map[string][]int64{}}

// IntarrayBind replaces the array bound to a named intarray vtab. Called by
// the harness command handler for `sqlite3_intarray_bind`.
func IntarrayBind(name string, vals []int64) {
	intarrayStore.Lock()
	defer intarrayStore.Unlock()
	cp := make([]int64, len(vals))
	copy(cp, vals)
	intarrayStore.tables[name] = cp
}

// IntarrayHandleRegistry maps the opaque handle returned by
// sqlite3_intarray_create to the intarray table name it binds. The handle is a
// text token (uppercase hex) so it matches the test's `[0-9A-Z]+` regex.
var intarrayHandles = struct {
	sync.Mutex
	next  uint64
	names map[string]string // handle -> table name
}{names: map[string]string{}}

// IntarrayRegisterHandle records a new handle for a table name and returns the
// handle token. Called by the harness command handler for
// `sqlite3_intarray_create`.
func IntarrayRegisterHandle(name string) string {
	intarrayHandles.Lock()
	defer intarrayHandles.Unlock()
	intarrayHandles.next++
	h := fmt.Sprintf("0X%X", intarrayHandles.next)
	intarrayHandles.names[h] = name
	return h
}

// IntarrayResolveHandle returns the table name for a handle token, or "".
func IntarrayResolveHandle(h string) string {
	intarrayHandles.Lock()
	defer intarrayHandles.Unlock()
	return intarrayHandles.names[h]
}

// IntarrayModule implements the intarray virtual table (src/test_intarray.c):
// a read-only vtab exposing the integer array bound via sqlite3_intarray_bind
// as a single "value" column, with the array index as the rowid. The bound
// table name is passed as the single module argument at Connect time
// (USING intarray('name')) so the instance can locate its array.
type IntarrayModule struct{}

// NewIntarrayModule builds the intarray module.
func NewIntarrayModule() *IntarrayModule { return &IntarrayModule{} }

// Eponymous reports that intarray is created explicitly (xCreate == xConnect).
func (m *IntarrayModule) Eponymous() bool { return false }

// Create implements Module.
func (m *IntarrayModule) Create(args []string) (VirtualTable, error) {
	return m.Connect(args)
}

// Connect implements Module: the single argv is the bound table name.
func (m *IntarrayModule) Connect(args []string) (VirtualTable, error) {
	name := ""
	if len(args) > 0 {
		name = unquoteVtabArg(args[0])
	}
	return &intarrayVTab{name: name}, nil
}

// Columns reports the intarray schema (value INTEGER PRIMARY KEY).
func (v *intarrayVTab) Columns() []string { return []string{"value"} }

// PrimaryKeyColumns reports value as the INTEGER PRIMARY KEY (rowid) so that
// `col IN intarray_table` membership tests resolve against the array values.
func (v *intarrayVTab) PrimaryKeyColumns() map[int]bool { return map[int]bool{0: true} }

// BestIndex accepts the default full-scan plan.
func (v *intarrayVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

type intarrayVTab struct {
	name   string
	array  []int64
	idx    int
	opened bool
}

// Open snapshots the currently bound array so later binds don't disturb an
// in-flight scan (SQLite binds before the query and the array is stable for
// the query's duration).
func (v *intarrayVTab) Open() (Cursor, error) {
	intarrayStore.Lock()
	arr := intarrayStore.tables[v.name]
	intarrayStore.Unlock()
	cp := make([]int64, len(arr))
	copy(cp, arr)
	return &intarrayCursor{array: cp, idx: -1}, nil
}

// intarrayCursor walks the bound array; the rowid equals the array index.
type intarrayCursor struct {
	array []int64
	idx   int // -1 before first row
	done  bool
}

// Next advances; first call positions at index 0.
func (c *intarrayCursor) Next() bool {
	if c.done {
		return false
	}
	c.idx++
	if c.idx >= len(c.array) {
		c.done = true
		return false
	}
	return true
}

// Column returns the integer at the current index.
func (c *intarrayCursor) Column(idx int) (interface{}, error) {
	if idx == 0 && c.idx >= 0 && c.idx < len(c.array) {
		return c.array[c.idx], nil
	}
	return nil, fmt.Errorf("intarray: invalid column access")
}

// Close implements Cursor.
func (c *intarrayCursor) Close() error { return nil }

// Rowid implements RowidCursor: the array index.
func (c *intarrayCursor) Rowid() int64 { return int64(c.idx) }
