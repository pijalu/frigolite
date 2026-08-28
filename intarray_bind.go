package frigolite

import "github.com/pijalu/frigolite/internal/vtab"

// TclIntarrayBind resolves an opaque intarray handle (returned by the
// sqlite3_intarray_create C-API emulation) to its bound table name and replaces
// that table's integer array. It backs the `sqlite3_intarray_bind` command used
// by the tcl2go-generated test harness for the intarray virtual table; the
// harness cannot statically expand the dynamic `eval`-built bind command, so it
// dispatches to this helper at runtime.
func TclIntarrayBind(handle string, vals []int64) {
	vtab.IntarrayBind(vtab.IntarrayResolveHandle(handle), vals)
}
