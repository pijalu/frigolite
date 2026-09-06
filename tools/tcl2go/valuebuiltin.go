// SPDX-License-Identifier: GPL-3.0-or-later
// Value-returning TCL builtins for do_test body dispatch: bodies whose final
// command is one of these compare the command's `_r` result value instead of
// the `_res` error slot (see emitDoTestBodyComparison in dotest.go).

package main

import tcl "github.com/pijalu/frigolite/tools/tclconvert/tcl"

// valueReturningBuiltins is the set of TCL builtins (in the SQLite TCL harness
// test scripts) whose last-command invocation result is what `do_test` body
// comparisons expect — i.e. they return a value via the transpiler's `_r`
// slot rather than via the `_res` error-result slot. Used by
// bodyEndsWithValueBuiltin to dispatch a multi-cmd do_test body whose final
// statement is a value-returning call (pager_cache_size, execsql, etc.).
var valueReturningBuiltins = map[string]bool{
	// SQLite test infrastructure helpers (Tcl-side procs and builtins).
	"execsql":              true,
	"execsql2":             true,
	"pager_cache_size":     true,
	"pager_cache_stats":    true,
	"capicount":            true,
	"hexio_get_int":        true,
	"db_last_insert_rowid": true,
	"db_status":            true,
	"stmt_status":          true,
	"get_rowid":            true,
	"get_state":            true,
	"get_pgresets":         true,
	"pgsz":                 true,
	"pgsz_v2":              true,
	"pagecount":            true,
	"readpages":            true,
	"exec_prepared":        true,
	"q1":                   true,
	"q2":                   true,
	"db_remaining":         true,
	"dbeval":               true,
	"pager_pagecount":      true,
	"integrity_check":      true,
	"explain":              true,
}

// bodyEndsWithValueBuiltin reports whether a do_test body's last command is
// a known value-returning TCL builtin (e.g. `pager_cache_size db`,
// `execsql {SELECT ...}`). The last command's result is what the do_test
// compares against the expected value (and was left in `_r` by the
// transpiled handler).
func bodyEndsWithValueBuiltin(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 1 {
		return false
	}
	return valueReturningBuiltins[last[0].Text]
}
