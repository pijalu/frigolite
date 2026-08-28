// Package main implements the tcl2go tool.
//
// This file contains do_test body classification helpers and the
// inlineVarFuncs transformer.
package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// (imports managed by goimports)

// bodyEndsWithIndexExpr reports whether a do_test body's last command is an
// `expr` whose result is a boolean comparison of a variable (e.g.
// `expr {$idx>=0}` after `set idx [lsearch $prg OpenEphemeral]`). Such
// bodies produce a boolean result the do_test compares, not an error.
func bodyEndsWithIndexExpr(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 2 || last[0].Text != "expr" {
		return false
	}
	text := last[1].Text
	if !strings.Contains(text, ">=") && !strings.Contains(text, ">") {
		return false
	}
	// The compared operand must be a plain variable reference (e.g. $idx); a
	// command substitution (e.g. [sqlite3_stmt_status ...]>0) is not a variable
	// and must not be mangled into a Go identifier.
	cmpIdx := strings.Index(text, ">=")
	if cmpIdx < 0 {
		cmpIdx = strings.Index(text, ">")
	}
	lhs := strings.TrimSpace(text[:cmpIdx])
	return strings.HasPrefix(lhs, "$") && !strings.Contains(lhs, "[")
}

// bodyEndsWithExprCompare reports whether a do_test body's last command is an
// `expr {$a==$b}` (or `$a!=$b`) comparison of two TCL variables — the
// dataversion1.test idiom `expr {$::dv1==$dv2}`. The expr evaluates to a
// boolean ("1"/"0") that the do_test compares.
func bodyEndsWithExprCompare(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 2 || last[0].Text != "expr" {
		return false
	}
	text := last[1].Text
	if !strings.Contains(text, "==") && !strings.Contains(text, "!=") {
		return false
	}
	// Both operands must be plain variable references.
	op := "=="
	if strings.Contains(text, "!=") {
		op = "!="
	}
	parts := strings.SplitN(text, op, 2)
	if len(parts) != 2 {
		return false
	}
	lhs := strings.TrimSpace(parts[0])
	rhs := strings.TrimSpace(parts[1])
	return strings.HasPrefix(lhs, "$") && strings.HasPrefix(rhs, "$") &&
		!strings.Contains(lhs, "[") && !strings.Contains(rhs, "[")
}

// bodyEndsWithExprResult reports whether a do_test body's last command is
// `expr [cmd ...] OP N` (a command-substitution comparison whose result was
// left in _r by processExpr, e.g. dbstatus.test 5.5.x).
func bodyEndsWithExprResult(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 2 || last[0].Text != "expr" {
		return false
	}
	text := last[1].Text
	return strings.Contains(text, "[") && strings.Contains(text, "]") &&
		(strings.Contains(text, ">") || strings.Contains(text, "<") || strings.Contains(text, "==") || strings.Contains(text, "!="))
}

// bodyEndsWithStringResult reports whether a do_test body's last command is a
// `string` subcommand (e.g. `string map {- {}} [string tolower $x]`) whose
// RESULT is the value the do_test compares — not an error expectation.
func bodyEndsWithStringResult(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	switch last[0].Text {
	case "string":
		return len(last) >= 2
	}
	return false
}

// bodyEndsWithStringMatch reports whether a do_test body's last command is
// `string match PATTERN STR` (its "1"/"0" result is left in `_r` by
// processStringMatch).
func bodyEndsWithStringMatch(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 3 {
		return false
	}
	return last[0].Text == "string" && last[1].Text == "match"
}

// lsearchDBEvalExpr parses a do_test body's final `expr` command of the form
//
//	expr {[lsearch [db eval {SQL}] PATTERN]>=0}
//
// which asserts that PATTERN appears in the rows produced by the db-eval SQL
// (ctime-3.0.1: DIRECT_OVERFLOW_READ is listed in PRAGMA compile_options).
// It returns the SQL text and the searched pattern. ok is false when the body
// does not match this shape (or uses a variable/command that cannot be
// resolved at transpile time).
func lsearchDBEvalExpr(bodyCmds [][]tcl.RawWord) (sqlText, pattern string, ok bool) {
	if len(bodyCmds) == 0 {
		return "", "", false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 2 || last[0].Text != "expr" {
		return "", "", false
	}
	text := strings.TrimSpace(last[1].Text)
	if !strings.Contains(text, ">=") && !strings.Contains(text, ">") {
		return "", "", false
	}
	cmpIdx := strings.Index(text, ">=")
	if cmpIdx < 0 {
		cmpIdx = strings.Index(text, ">")
	}
	lhs := strings.TrimSpace(text[:cmpIdx])
	// Shape: [lsearch [db eval {SQL}] PATTERN]
	inner := strings.TrimSpace(strings.TrimPrefix(lhs, "["))
	inner = strings.TrimSuffix(strings.TrimSpace(inner), "]")
	if !strings.HasPrefix(inner, "lsearch ") {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(inner, "lsearch "))
	if !strings.HasPrefix(rest, "[db eval ") {
		return "", "", false
	}
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "[db eval "))
	// The SQL is a braced or quoted word inside [db eval {...}]; find its end.
	if !strings.HasPrefix(rest, "{") {
		return "", "", false
	}
	close := strings.Index(rest, "}")
	if close < 0 {
		return "", "", false
	}
	sqlText = rest[1:close]
	tail := strings.TrimSpace(rest[close+1:])
	// tail has the form `] PATTERN]` (closing bracket of the db-eval
	// substitution, the pattern, and the closing bracket of the lsearch
	// command). Strip the leading closing bracket and the trailing one.
	tail = strings.TrimPrefix(strings.TrimSpace(tail), "]")
	tail = strings.TrimSuffix(strings.TrimSpace(tail), "]")
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return "", "", false
	}
	return sqlText, tail, true
}

// bodyEndsWithListResult reports whether a do_test body's last command is a
// `list` command whose result the do_test compares against the expected value
// (e.g. `list [catch {sqlite3_blob_write ...} msg] $msg`). The transpiled
// command leaves its value in `_r`.
func bodyEndsWithListResult(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	return last[0].Text == "list"
}

// bodyEndsWithBlobResult reports whether a do_test body's last command is an
// incremental-blob command whose result the do_test compares against the
// expected value (sqlite3_blob_read/bytes/write, read $blob, db incrblob,
// sqlite3_blob_close returns ""). The transpiled command leaves its value in
// `_r`.
func bodyEndsWithBlobResult(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	switch last[0].Text {
	case "sqlite3_blob_read", "sqlite3_blob_bytes", "sqlite3_blob_write",
		"sqlite3_blob_open", "sqlite3_blob_close", "sqlite3_blob_reopen",
		"read", "seek":
		return true
	}
	// db incrblob table column rowid — result is the blob handle name.
	if len(last) >= 4 && last[0].Text == "db" && last[1].Text == "incrblob" {
		return true
	}
	return false
}

// bodyEndsWithBackupResult reports whether a do_test body's last command is a
// backup/errmsg/file-size command whose result the do_test compares against
// the expected value (sqlite3_backup, B step/finish/remaining/pagecount,
// sqlite3_errmsg, sqlite3_errcode, sqlite3_close, dbcksum, file size). The
// transpiled command leaves its value in `_r`.
func bodyEndsWithBackupResult(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	switch last[0].Text {
	case "sqlite3_backup", "sqlite3_errmsg", "sqlite3_errcode", "sqlite3_close", "dbcksum", "file_control_data_version", "sqlite3_exec":
		return true
	}
	// Backup object subcommands: B step / B finish / B remaining / B pagecount.
	if len(last) >= 2 {
		switch last[1].Text {
		case "step", "finish", "remaining", "pagecount":
			if goName := tclVarToGo(last[0].Text); isValidGoIdent(goName) {
				return true
			}
		}
	}
	// `file size PATH` body (backup4's file-size assertions).
	if len(last) >= 3 && last[0].Text == "file" && last[1].Text == "size" {
		return true
	}
	return false
}

// bodyEndsWithStmtMetadata reports whether a do_test body's last command is a
// prepared-statement metadata query (bind_parameter_*/column_*/data_count)
// whose value the runtime Stmt helpers leave in _r.
func bodyEndsWithStmtMetadata(bodyCmds [][]tcl.RawWord) bool {
	if !stmtVMEnabled() {
		return false
	}
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	switch last[0].Text {
	case "sqlite3_bind_parameter_count", "sqlite3_bind_parameter_name",
		"sqlite3_bind_parameter_index", "sqlite3_column_count",
		"sqlite3_column_name", "sqlite3_column_text", "sqlite3_column_int",
		"sqlite3_data_count", "sqlite3_step", "sqlite3_finalize", "sqlite3_reset":
		return true
	}
	return false
}

// bodyEndsWithSqlite3Exec reports whether a do_test body's last command is a
// `sqlite3_exec db {SQL}` harness command (its "{code {headers values}}"
// result is left in _r; badutf/badutf2.test).
func bodyEndsWithSqlite3Exec(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	return len(last) >= 1 && last[0].Text == "sqlite3_exec"
}

// bodyEndsWithLindex reports whether a do_test body's last command is a
// `lindex` whose result (left in _r by processListOp) the do_test compares
// against the expected value (badutf2.test's `lindex [lindex $res 1] 1`).
func bodyEndsWithLindex(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	return len(last) >= 1 && last[0].Text == "lindex"
}

// bodyEndsWithQueryFunc reports whether a do_test body's last command is a
// bare query-proc call (e.g. `signature` where proc signature returns a
// db-eval result). The last command's query result is what the do_test
// compares against the expected value.
func bodyEndsWithQueryFunc(bodyCmds [][]tcl.RawWord, queryFuncs map[string]string) bool {
	if len(bodyCmds) == 0 || len(queryFuncs) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) == 0 {
		return false
	}
	_, ok := queryFuncs[last[0].Text]
	return ok
}

// bodyEndsWithEQP reports whether a do_test body's last command is `eqp
// "SQL"` (the EXPLAIN QUERY PLAN detail collector). Its result is the detail
// list the do_test compares against the expected value.
func bodyEndsWithEQP(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) == 0 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	return len(last) >= 2 && last[0].Text == "eqp"
}

// bodyEndsWithSetVar reports whether a do_test body's last command is a
// `set VAR` (a variable read) or `set VAR <value>` (an assignment; the set
// command's VALUE is the assigned variable). The do_test's VALUE is that
// variable, so the assertion compares the Go variable against the expected
// value rather than treating the body as an error expectation
// (attach4-1.2.1 ends with `set L`; round1's `set x [db one ...]` bodies
// hold the rounded value in x). It returns the Go variable name.
func bodyEndsWithSetVar(tp *transpiler, bodyCmds [][]tcl.RawWord) (string, bool) {
	if len(bodyCmds) == 0 {
		return "", false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) >= 2 && last[0].Text == "set" {
		name := strings.TrimPrefix(last[1].Text, "$")
		// Dynamic-key array read (`set ARR($key)`): the value lives in the
		// corresponding XxxMap Go map — but ONLY when ARR is a registered
		// dynamic array (collectArrayMapVars), because those are the only
		// arrays the preamble declares as maps. Anything else (literal keys,
		// the (*) column-list pseudo-key, unregistered arrays) falls back to
		// the flat tclVarToGo name so the output stays compilable
		// (with1 17.2's `set A(*)` → _A_arr; mutex1 1.5's
		// `set counters(total)` → counters_total).
		if idx := strings.Index(name, "("); idx > 0 && strings.HasSuffix(name, ")") {
			base := strings.TrimPrefix(name[:idx], "::")
			key := name[idx+1 : len(name)-1]
			if key != "" && key != "*" && tp.arrayMapVars[base] {
				mapVar := tclVarToGo(base) + "Map"
				keyExpr := strings.TrimPrefix(key, "$")
				if keyExpr == key {
					// Literal key: set ARR(3) → ARRMap["3"]
					return fmt.Sprintf("%s[%q]", mapVar, key), true
				}
				return mapVar + "[" + tclVarToGo(keyExpr) + "]", true
			}
		}
		goVar := tclVarToGo(strings.TrimPrefix(last[1].Text, "$"))
		if isValidGoIdent(goVar) && goVar != "" {
			return goVar, true
		}
	}
	return "", false
}

// isTestCommand reports whether cmdName is a TCL test command whose first
// argument is the test name (after an optional "-db NAME" prefix).
func isTestCommand(cmdName string) bool {
	switch cmdName {
	case "do_execsql_test", "do_timed_execsql_test", "do_execsql2_test",
		"do_catchsql_test", "do_test", "do_eqp_test", "do_preupdate_test":
		return true
	}
	return false
}

// testCommandName returns the test name from a test command's arguments,
// skipping an optional "-db NAME" prefix.
func testCommandName(args []tcl.RawWord) string {
	if len(args) == 0 {
		return ""
	}
	if args[0].Text == "-db" && len(args) >= 2 {
		return args[1].Text
	}
	return args[0].Text
}

// emitSkippedTest emits a no-op block for a test that exercises an unsupported
// engine feature.
func (tp *transpiler) emitSkippedTest(name, reason string) {
	nameExpr := tp.goStringLiteral(tcl.RawWord{Text: name})
	tp.emitLine("{ // %s — skipped: %s", nameExpr, reason)
	tp.emitLine("}")
}

// emitSkippedTestSideEffects emits a skipped test, but for do_execsql_test /
// do_timed_execsql_test bodies it also runs the SQL batch for its SIDE EFFECTS
// (CREATE/INSERT/ANALYZE setup) so later tests that depend on the schema still
// see it. Only the assertion is dropped (the test is N-A for reasons recorded
// in skipTests). do_test / do_eqp_test bodies are not executed (their side
// effects are entangled with C-ABI state or the assertion itself).
func (tp *transpiler) emitSkippedTestSideEffects(cmdName string, args []tcl.RawWord, name, reason string) {
	nameExpr := tp.goStringLiteral(tcl.RawWord{Text: name})
	isExecsql := cmdName == "do_execsql_test" || cmdName == "do_timed_execsql_test" || cmdName == "do_execsql2_test" || cmdName == "do_preupdate_test"
	// A "(no-side-effects)" marker in the reason suppresses the SQL side
	// effects too: some skipped statements (e.g. FTS5 UPDATE, which corrupts
	// the pager) are harmful to run and later tests must not observe them.
	if strings.Contains(reason, "(no-side-effects)") {
		tp.emitSkippedTest(name, reason)
		return
	}
	if !isExecsql || len(args) < 2 {
		// A do_test whose body is a single db eval {SQL} (covering-index
		// scan-order tests) still needs the SQL's side effects (CREATE/INSERT)
		// for later tests; only the assertion is dropped.
		if tp.emitSkippedDoTestSideEffects(name, reason, args) {
			return
		}
		tp.emitSkippedTest(name, reason)
		return
	}
	tp.emitLine("{ // %s — skipped: %s (SQL side effects only)", nameExpr, reason)
	tp.indent++
	// The do_execsql_test body may have an optional "-db NAME" prefix.
	sqlArgs := args
	if len(sqlArgs) >= 3 && sqlArgs[0].Text == "-db" {
		sqlArgs = sqlArgs[2:]
	}
	sqlExpr := tp.collectSQLExpression(sqlArgs[1:2])
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("_ = _res.Error // tolerate unsupported-feature errors in skipped tests")
	tp.indent--
	tp.emitLine("}")
}

// emitSkippedDoTestSideEffects handles a do_test whose body contains
// `dbN eval {SQL}` commands: emit the SQL side effects (CREATE/INSERT/DROP)
// for later tests while dropping the assertions (catchsql, lappend, etc.).
// Returns true when at least one dbN eval command was found.
func (tp *transpiler) emitSkippedDoTestSideEffects(name, reason string, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	bodyCmds := tp.parseBracedBody(args, 1)

	// Collect all dbN eval {SQL} commands (the DDL/DML side effects).
	type sideEffect struct{ connVar, sqlExpr string }
	var effects []sideEffect
	for _, cmd := range bodyCmds {
		if len(cmd) < 3 || !strings.HasPrefix(cmd[0].Text, "db") || cmd[1].Text != "eval" {
			continue
		}
		effects = append(effects, sideEffect{
			connVar: cmd[0].Text,
			sqlExpr: tp.collectSQLExpression(cmd[2:3]),
		})
	}
	if len(effects) == 0 {
		return false
	}
	nameExpr := tp.goStringLiteral(tcl.RawWord{Text: name})
	tp.emitLine("{ // %s — skipped: %s (SQL side effects only)", nameExpr, reason)
	tp.indent++
	for _, eff := range effects {
		tp.emitLine("_res = %s.Exec(%s)", eff.connVar, eff.sqlExpr)
		tp.emitLine("_ = _res.Error // tolerate unsupported-feature errors in skipped tests")
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// unsupportedSQL reports a reason string when sql uses a construct the
// engine does not support (window functions), or "" when the SQL is
// transpilable. Tests using these constructs are emitted as no-op skips so
// the package still compiles and runs.
func unsupportedSQL(sql string) string {
	// Window functions (OVER clauses) are implemented by the engine (P4.WINDOW)
	// and no longer skipped.
	// VACUUM (including VACUUM INTO and VACUUM aux) rebuilds the database
	// file, reclaiming free pages and renumbering rowids. Frigolite has no
	// file-level VACUUM (P8.VACUUM), so statements that run it or assert its
	// effects (file size, freelist, rowid renumbering, fragment counts) are
	// skipped.
	if reVACUUM.MatchString(sql) {
		return "VACUUM not implemented (P8.VACUUM)"
	}
	// PRAGMA freelist_count reports free pages left after deletes, which only
	// has meaning relative to VACUUM/auto_vacuum page management.
	if strings.Contains(strings.ToUpper(sql), "PRAGMA FREELIST_COUNT") {
		return "PRAGMA freelist_count is VACUUM-dependent (P8.VACUUM)"
	}
	// Corruption tests that rename index entries inside sqlite_schema via
	// writable_schema rely on SQLite storing every WITHOUT ROWID autoindex as
	// a schema entry; Frigolite synthesizes PK/UNIQUE autoindexes instead, so
	// the duplicate-name corruption they create never forms and REINDEX
	// cannot report it.
	if strings.Contains(strings.ToUpper(sql), "UPDATE SQLITE_SCHEMA SET") &&
		strings.Contains(strings.ToUpper(sql), "SQLITE_AUTOINDEX") {
		return "writable_schema autoindex-rename corruption not supported"
	}
	// decimal / ieee754 loadable-extension functions (ext/misc/decimal.c,
	// ext/misc/ieee754.c) are not part of the default SQLite build; the
	// fpconv1 stress tests that call them (dtostr, decimal_sub, decimal_exp,
	// ieee754_from_int, ieee754_to_int) cannot run.
	upper := strings.ToUpper(sql)
	for _, fn := range []string{"dtostr", "decimal_sub", "decimal_exp", "decimal_mul", "decimal_add", "ieee754_from_int", "ieee754_to_int", "ieee754_from_blob", "ieee754_to_blob", "ieee754_mantissa", "ieee754_exponent", "ieee754_inc"} {
		if fnCallIn(upper, strings.ToUpper(fn)) {
			return "decimal/ieee754 loadable extension not implemented N-A"
		}
	}
	// misc8-2.1: eval(printf('DELETE FROM t2 WHERE c=%d AND %d>5', a+c, a+c))
	// inside a cross-join SELECT. SQLite streams the join row-by-row, so the
	// DELETE executed by eval() during output evaluation removes rows from the
	// still-streaming t2 scan (later join rows for deleted c values show NULL).
	// Frigolite materializes all join rows before evaluating output
	// expressions, so the deletions cannot affect the already-built join set
	// (and the planner's loop order differs). The eval() core (misc8-1.x) is
	// fully implemented; this single test requires streaming-join-with-side-
	// effects, a materialized-join architectural gap.
	if strings.Contains(upper, "EVAL(") && strings.Contains(upper, "PRINTF('DELETE FROM") {
		return "delete-during-join-iteration requires streaming join (materialized-join gap)"
	}
	return ""
}

// fnCallIn reports whether sqlUpper (uppercased SQL) contains a bare
// function-call token NAME( with word boundaries, case-folded (both args are
// uppercase).
func fnCallIn(sqlUpper, name string) bool {
	for i := 0; ; {
		j := strings.Index(sqlUpper[i:], name)
		if j < 0 {
			return false
		}
		pos := i + j
		// Word boundary before the name: not preceded by an identifier
		// char or underscore (so "xdecimal_sub" doesn't match).
		if pos > 0 && (isIdentChar(sqlUpper[pos-1])) {
			i = pos + len(name)
			continue
		}
		// Followed by '(' (function call) — the boundary after the name.
		k := pos + len(name)
		for k < len(sqlUpper) && (sqlUpper[k] == ' ' || sqlUpper[k] == '\t' || sqlUpper[k] == '\n' || sqlUpper[k] == '\r') {
			k++
		}
		if k < len(sqlUpper) && sqlUpper[k] == '(' {
			return true
		}
		i = pos + len(name)
	}
}

// isIdentChar reports whether c is an SQL identifier character.
func isIdentChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// reVACUUM matches a VACUUM statement (plain, VACUUM INTO, or VACUUM schema).
var reVACUUM = regexp.MustCompile(`(?i)\bVACUUM\b`)

// sanitizeSQL rewrites constructs the transpiler understands so they run on
// the engine. In SQLite's VALUES-coroutine execution, row_number() OVER ()
// inside an INSERT ... VALUES clause evaluates to the constant 1 (each VALUES
// row is a separate constant row), so the generated tests replace it with the
// literal 1 there. The folding is scoped to the VALUES clause of INSERT/
// REPLACE statements so SELECT / CREATE VIEW window functions (now executed
// by the engine) keep the real row_number() OVER () form — including in
// multi-statement strings that begin with INSERT but contain a trailing
// SELECT (window1 26.2).
var reRowNumberConst = regexp.MustCompile(`(?i)row_number\s*\(\s*\)\s*OVER\s*\(\s*\)`)

func sanitizeSQL(sql string) string {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, "VALUES")
	if idx < 0 {
		return sql
	}
	// Only fold within the VALUES clause (between VALUES and the first
	// semicolon / end of the INSERT statement).
	vals := sql[idx+len("VALUES"):]
	end := strings.IndexByte(vals, ';')
	if end < 0 {
		end = len(vals)
	}
	clause := vals[:end]
	folded := reRowNumberConst.ReplaceAllString(clause, "1")
	return sql[:idx+len("VALUES")] + folded + vals[end:]
}

func (tp *transpiler) collectSQLExpression(args []tcl.RawWord) string {
	if len(args) == 0 {
		return `""`
	}
	if args[0].Braced {
		// execsql args like { INSERT ... VALUES($i, $x) } are re-evaluated by
		// TCL's uplevel, so $var references ARE substituted with the current
		// loop/test variable values. TCL `db eval` binds $var as a VALUE (not
		// SQL text), so build a Go string expression that renders each $var
		// as a SQL literal via sqlLiteral(); otherwise keep the literal
		// braced text. Bracketed SQL identifiers ([4], [t.1]) are literal in
		// a braced word and must be preserved verbatim, never treated as TCL
		// command substitutions.
		// IMPORTANT: an undeclared $var (like $abc in a literal-token test such
		// as `select $abc(`) is NOT a TCL substitution — it is literal SQL that
		// exercises SQLite's tokenizer. Only substitute vars that were actually
		// declared via `set`/`foreach`/`for`.
		text := sanitizeSQL(args[0].Text)
		// dbconfig_maindbname_<alias> test hook: rewrite alias. → main.
		text = tp.rewriteMainDBAlias(text)
		if hasDeclaredDollarVarRef(text, tp) || hasColonVarRef(text, tp) {
			return tp.buildSQLStringExprNoCmd(text)
		}
		// A registered variable-reader function (e.g. tclvar('v1')) is
		// inlined as the Go variable's current value.
		if inlined := tp.inlineVarFuncs(text); inlined != "" {
			return inlined
		}
		return fmt.Sprintf("%q", text)
	}
	// A bare (non-braced) TCL variable reference such as `db eval $::schema`
	// means "execute the SQL stored in that variable". Emit the Go variable
	// (e.g. `db.Exec(schema)`) rather than the literal text "$::schema".
	if !args[0].Braced && strings.HasPrefix(args[0].Text, "$") && len(args) == 1 {
		bare := strings.TrimPrefix(args[0].Text, "$")
		bare = strings.TrimPrefix(bare, "::")
		if isPlainTCLVarName(bare) {
			v := tclVarToGo(args[0].Text)
			if tp.isVarDeclared(v) || v == "schema" || v == "sql" {
				return v
			}
		}
	}
	return tp.goStringLiteral(tcl.RawWord{Text: sanitizeSQL(args[0].Text), Quoted: args[0].Quoted})
}

// inlineVarFuncs rewrites calls to registered variable-reader SQL functions
// (e.g. `db function tclvar` → `tclvar('v1')`) into a Go string concatenation
// that injects the current value of the named TCL variable as a SQL literal:
//
//	`SELECT ... WHERE tclvar('v1')`  →  `"SELECT ... WHERE " + sqlLiteral(v1) + ";..."`
//
// Returns "" when no such call is present.
// isPlainTCLVarName reports whether s is a plain TCL variable name
// (letters, digits, underscores; may not start with a digit).
func isPlainTCLVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func (tp *transpiler) inlineVarFuncs(text string) string {
	if len(tp.dbVarFuncs) == 0 {
		return ""
	}
	var parts []string // alternating literals and sqlLiteral(...) fragments
	rest := text
	for {
		// Find the earliest registered-function call fname('X') in rest.
		bestPos, bestFName, bestName, bestVar := tp.findVarFuncCall(rest)
		if bestPos < 0 {
			parts = append(parts, fmt.Sprintf("%q", rest))
			break
		}
		if bestPos > 0 {
			parts = append(parts, fmt.Sprintf("%q", rest[:bestPos]))
		}
		parts = append(parts, "sqlLiteral("+bestVar+")")
		// skip the full call: fname + "('" + name + "')"
		rest = rest[bestPos+len(bestFName)+len(bestName)+4:]
	}
	return strings.Join(parts, " + ")
}

// findVarFuncCall scans rest for the earliest registered variable-reader
// function call fname('VAR'), returning its position, the function name, the
// TCL variable name, and the Go variable name (or position -1 when none).
func (tp *transpiler) findVarFuncCall(rest string) (int, string, string, string) {
	bestPos := -1
	bestFName := ""
	bestName := ""
	bestVar := ""
	for fname := range tp.dbVarFuncs {
		pos := strings.Index(rest, fname+"('")
		if pos < 0 {
			continue
		}
		closing := strings.Index(rest[pos+len(fname)+2:], "')")
		if closing < 0 {
			continue
		}
		vname := rest[pos+len(fname)+2 : pos+len(fname)+2+closing]
		// Only a plain TCL variable name (letters/digits/underscore) is a
		// variable-reader target. A function whose string argument is
		// arbitrary SQL text (tkt3080: execsql('CREATE TABLE t1(x)')) is a
		// real SQL function — the arg is a literal, not a variable name.
		// The variable must also be DECLARED (set) in the test; a literal
		// string arg like my_changes('trigger') is data, not a variable
		// reference (e_changes.test 5.1.x).
		if !isPlainTCLVarName(vname) {
			continue
		}
		goVar := tclVarToGo(vname)
		if !tp.isVarDeclared(goVar) {
			continue
		}
		if !isValidGoIdent(goVar) {
			continue
		}
		if bestPos < 0 || pos < bestPos {
			bestPos = pos
			bestFName = fname
			bestName = vname
			bestVar = goVar
		}
	}
	return bestPos, bestFName, bestName, bestVar
}

// isBareGoIdent reports whether s is a single Go identifier (a variable
// reference emitted by buildStringExpr, not a quoted literal or expression).
func isBareGoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			continue
		}
		if i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return false
	}
	return true
}

func hasVarRef(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && (isVarStartChar(s[i+1]) || s[i+1] == '{') {
			return true
		}
		if s[i] == '[' {
			return true
		}
	}
	return false
}

// hasDeclaredDollarVarRef reports whether s contains a $var reference whose
// variable was actually declared (set/foreach/for). Undeclared vars like
// $abc in `select $abc(` are literal SQL tokens exercising the tokenizer,
// not TCL substitutions.
func hasDeclaredDollarVarRef(s string, tp *transpiler) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			continue
		}
		if s[i+1] == '{' {
			end := strings.Index(s[i+2:], "}")
			if end < 0 {
				continue
			}
			name := s[i+2 : i+2+end]
			// ${ns::var} — qualify the base name via tclVarToGo mapping
			goName := tclVarToGo(name)
			if goName != "" && (isAssignedTCLVar(goName) || goName == "schema" || goName == "sql") {
				return true
			}
			i += 2 + end
			continue
		}
		if !isVarStartChar(s[i+1]) {
			continue
		}
		j := i + 1
		for j < len(s) && isVarChar(s[j]) {
			j++
		}
		// Handle :: qualification and trailing (...)
		name := s[i+1 : j]
		// $ns::var — collect the full ::-qualified name
		for j+1 < len(s) && s[j] == ':' && s[j+1] == ':' {
			j += 2
			k := j
			for k < len(s) && isVarChar(s[k]) {
				k++
			}
			if k == j {
				break
			}
			name += "::" + s[j:k]
			j = k
		}
		goName := tclVarToGo(name)
		if goName != "" && (isAssignedTCLVar(goName) || name == "schema" || name == "sql") {
			return true
		}
		i = j - 1
	}
	return false
}

// hasColonVarRef reports whether s contains a :varname SQL bind-parameter
// reference that corresponds to a declared TCL variable. TCL `db eval
// {SELECT ... LIMIT :limit}` binds the TCL var `limit` to the `:limit`
// parameter, so the transpiler must substitute it as a SQL literal.
func hasColonVarRef(s string, tp *transpiler) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		j := i + 1
		if j >= len(s) || !isVarStartChar(s[j]) {
			continue
		}
		for j < len(s) && isVarChar(s[j]) {
			j++
		}
		name := s[i+1 : j]
		if tp.isVarDeclared(tclVarToGo(name)) {
			return true
		}
	}
	return false
}
