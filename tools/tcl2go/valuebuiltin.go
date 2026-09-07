// SPDX-License-Identifier: GPL-3.0-or-later
// do_test body dispatch helpers: value-returning builtin detection (bodies
// whose final command compares the command's `_r` result value instead of the
// `_res` error slot — see emitDoTestBodyComparison in dotest.go) and the
// trailing lappend-var shape.

package main

import (
	"strings"

	tcl "github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

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
	// exclusive2.test change-counter procs (transpiled to harness helpers).
	"readPagerChangeCounter": true,
	"pagerChangeCounter":     true,
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
	if valueReturningBuiltins[last[0].Text] {
		return true
	}
	// A bare user-proc call whose body is a table fingerprint
	// (exclusive2.test's t1sig) leaves its "COUNT MD5HEX" value in _r.
	if body, ok := globalProcBodies[last[0].Text]; ok && userProcEmitterFor(last[0].Text, body) == "table_sig" {
		return true
	}
	return false
}

// bodyEndsWithLappendVar reports whether a do_test body's last command is
// `lappend VAR $X`, whose value is the appended list variable itself
// (sqllimits1-6.3's `set rc [catch {sqlite3_prepare ...} STMT];
// lappend rc $STMT` — the do_test compares rc's final list value). Returns
// the variable to compare.
func bodyEndsWithLappendVar(bodyCmds [][]tcl.RawWord) (string, bool) {
	if len(bodyCmds) == 0 {
		return "", false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 3 && last[0].Text == "lappend" {
		return tclVarToGo(strings.TrimPrefix(last[1].Text, "$")), true
	}
	return "", false
}
