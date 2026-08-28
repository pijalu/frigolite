package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processIntarrayCreate emits Go for the test-only C-API command
// `sqlite3_intarray_create DB NAME`: it registers an intarray virtual table
// named NAME (bound to an initially-empty integer array) and returns an opaque
// handle string (the result of vtab.IntarrayRegisterHandle). The handle is
// assigned to the enclosing `set VAR [...]' or `catch {...} VAR' variable by
// the caller via the `_r` result convention.
func (tp *transpiler) processIntarrayCreate(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitLine("// sqlite3_intarray_create (too few args)")
		return
	}
	name := strings.TrimSpace(args[1].Text)
	name = strings.Trim(name, "'\"")
	if name == "" {
		tp.emitLine("// sqlite3_intarray_create (empty name)")
		return
	}
	// Register the handle so later sqlite3_intarray_bind calls can resolve it
	// back to the table name.
	tp.emitLine("_r = vtab.IntarrayRegisterHandle(%q)", name)
	// SQLite creates the vtab in the temp schema; bind the table name as the
	// single module argument so the instance can locate its array at Open().
	tp.emitLine("_res = %s.Exec(\"CREATE VIRTUAL TABLE temp.%s USING intarray('%s')\")", tp.dbVar, name, name)
	tp.emitLine("if _res.Error != nil { t.Errorf(\"intarray create: %%v\", _res.Error) }")
}

// processIntarrayBind emits Go for `sqlite3_intarray_bind HANDLE V1 V2 ...`:
// it resolves the opaque HANDLE (a `$var` holding the sqlite3_intarray_create
// result) to the table name and replaces that table's bound integer array.
func (tp *transpiler) processIntarrayBind(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitLine("// sqlite3_intarray_bind (too few args)")
		return
	}
	handle := strings.TrimSpace(args[0].Text)
	handle = strings.TrimPrefix(handle, "$")
	handleGo := tclVarToGo(handle)
	if handleGo == "" {
		handleGo = strconv.Quote(handle)
	}
	var parts []string
	for _, a := range args[1:] {
		parts = append(parts, tp.intArgExpr(strings.TrimSpace(a.Text)))
	}
	tp.emitLine("_iaName := vtab.IntarrayResolveHandle(%s)", handleGo)
	tp.emitLine("vtab.IntarrayBind(_iaName, []int64{%s})", strings.Join(parts, ", "))
}

// intarrayCreateSetValue wires `set VAR [sqlite3_intarray_create DB NAME]` to
// assign the returned handle to VAR. Returns true when the bracket command is
// an intarray_create (handled).
func (tp *transpiler) intarrayCreateSetValue(goName, cmdText string) bool {
	fields := strings.Fields(cmdText)
	if len(fields) < 2 || fields[0] != "sqlite3_intarray_create" {
		return false
	}
	words := make([]tcl.RawWord, 0, len(fields))
	for _, f := range fields {
		words = append(words, tcl.RawWord{Text: f})
	}
	tp.processIntarrayCreate(words[1:])
	tp.assignSetValue(goName, "_r")
	return true
}
