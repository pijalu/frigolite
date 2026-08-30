// Package main implements the tcl2go tool.
//
// This file handles the TCL set command.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
	"github.com/pijalu/frigolite/tools/tclconvert/tcl/tclparser"
)

// ---- Variable handlers ----

func (tp *transpiler) processSet(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}

	// A `set var value` gives the variable a value, so it is no longer
	// unset — clear any NULL-marking so $var renders as the value.
	goName := tclVarToGo(args[0].Text)
	if tp.unsetVars != nil {
		delete(tp.unsetVars, goName)
	}
	// Flag a TCL var whose value is the literal command `sqlite3_intarray_bind`
	// (intarray.test builds such a script var and later `eval`s it). The dynamic
	// eval site then dispatches to the runtime intarray-bind handler.
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "sqlite3_intarray_bind") {
		if tp.intarrayEvalVars == nil {
			tp.intarrayEvalVars = make(map[string]bool)
		}
		tp.intarrayEvalVars[goName] = true
	}

	// set ::STMT [sqlite3_prepare db "SQL" -1 TAIL] — record the prepared
	// statement so later sqlite3_bind_* / sqlite3_step / sqlite3_reset /
	// sqlite3_finalize calls can be emulated as plain db.Exec INSERTs (the
	// C API itself has no Go equivalent, but the test state it creates does).
	if len(args) >= 2 {
		prepareText := strings.TrimSpace(args[1].Text)
		if strings.HasPrefix(prepareText, "[sqlite3_prepare") && strings.HasSuffix(prepareText, "]") {
			tp.recordPreparedStatement(goName, prepareText)
			return
		}
		// Bracket delimiters may already be removed when command is nested in
		// catch body; retain same prepare handling for that parser form.
		if strings.HasPrefix(prepareText, "sqlite3_prepare") {
			tp.recordPreparedStatement(goName, "["+prepareText+"]")
			return
		}
		// `set fd [open FILE MODE]` — track the channel's path so subsequent
				// `puts $fd TEXT` writes to the right file (regardless of MODE: wb,
				// r+, etc.; the corrupt*.test suites open test.db r+ and overwrite a
				// byte at a known offset to simulate corruption). Without this,
				// packages that run AFTER another test that opens test.tcl for write
				// would inherit `activeFileChannels["fd"] = "test.tcl"` and write
				// corruption bytes to the wrong file.
				if path, mode, ok := parseOpenChannelWord(args[1].Text); ok {
					if strings.HasPrefix(path, "$") {
						activeFileChannels[goName] = tclVarToGo(strings.TrimPrefix(path, "$"))
						activeFileChannelExprs[goName] = true
						if strings.Contains(mode, "w") {
							tp.emitLine("_ = os.WriteFile(%s, nil, 0644)", activeFileChannels[goName])
						}
					} else {
						activeFileChannels[goName] = path
						if strings.Contains(mode, "w") {
							tp.emitLine("_ = os.WriteFile(%s, nil, 0644)", strconv.Quote(path))
						}
					}
				}
	}

	// Skip set testdir [file dirname $argv0] etc - infrastructure
	if len(args) >= 1 && tp.processNamespaceSet(args) {
		return
	}

	tp.processSetPlain(args)
}

// trackArrayKey records a TCL array literal-key assignment (set arr(K) V) so a
// later $arr($keyvar) reference can be transpiled to a runtime selection.
func (tp *transpiler) trackArrayKey(name string) {
	idx := strings.Index(name, "(")
	if idx <= 0 || !strings.HasSuffix(name, ")") {
		return
	}
	base := name[:idx]
	key := name[idx+1 : len(name)-1]
	if base == "" || key == "" || key == "*" || strings.HasPrefix(key, "$") {
		return
	}
	if tp.arrayKeys == nil {
		tp.arrayKeys = make(map[string][]string)
	}
	tp.arrayKeys[base] = append(tp.arrayKeys[base], key)
}

// dynamicArraySet detects a dynamic-key array assignment `set arr($keyvar) V`
// where arr is registered as a map variable. Returns the base array name, the
// key expression (the $var), and true when this form applies.
func (tp *transpiler) dynamicArraySet(name string) (string, string, bool) {
	idx := strings.Index(name, "(")
	if idx <= 0 || !strings.HasSuffix(name, ")") {
		return "", "", false
	}
	base := name[:idx]
	key := name[idx+1 : len(name)-1]
	if base == "" || !strings.HasPrefix(key, "$") {
		return "", "", false
	}
	if tp.arrayMapVars != nil && tp.arrayMapVars[base] {
		return base, strings.TrimPrefix(key, "$"), true
	}
	return "", "", false
}

// emitDynamicArraySet emits `arrMap[keyExpr] = value` for a dynamic-key array
// assignment. The key expression is the loop/var holding the key; the value is
// the remaining set arguments rendered as a string expression.
func (tp *transpiler) emitDynamicArraySet(base, keyVar string, args []tcl.RawWord) {
	mapVar := tclVarToGo(base) + "Map"
	keyExpr := tclVarToGo(keyVar)
	valExpr := `""`
	if len(args) >= 2 {
		valExpr = tp.goStringLiteral(args[1])
	}
	tp.emitLine("%s[%s] = %s", mapVar, keyExpr, valExpr)
}

// processSetPlain handles `set var ...` for plain (non-namespace) variable
// names: identifier checks, the err/db redirects, and bracket-command value
// dispatch.
func (tp *transpiler) processSetPlain(args []tcl.RawWord) {
	// Dynamic-key array assignment `set arr($keyvar) V`: emit a Go map store
	// arrMap[keyvar] = V (the array is declared as map[string]string in the
	// preamble because its keys are runtime values).
	if base, key, isDyn := tp.dynamicArraySet(args[0].Text); isDyn {
		tp.emitDynamicArraySet(base, key, args)
		return
	}
	// `set arr(key) value` with a literal key also registers into the tclvar
	// virtual-table registry so USING tclvar scans see it (test_tclvar.c).
	if base, key, isElem := splitArrayElement(args[0].Text); isElem && !strings.Contains(key, "$") {
		markTclvarBase(base)
		if len(args) >= 2 {
			// Write form.
			valExpr := tp.varValueExpr(args[1:])
			tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, key, valExpr)
		} else {
			// Read form (`set arr(key)` with no value): fetch the element.
			goRead := tclVarToGo(args[0].Text)
			tp.emitLine("%s = vtab.TclVarGet(%q, %q)", goRead, base, key)
		}
	} else if !isElem && len(args) >= 2 && isValidGoIdent(tclVarToGo(base)) {
		// Scalars are registered too: tclvar exposes the whole interpreter
		// namespace (vtabH-2.0: set xyz 10 then WHERE name='xyz').
		valExpr := tp.varValueExpr(args[1:])
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, "", valExpr)
	} else if !isElem && len(args) >= 2 && isValidGoIdent(tclVarToGo(base)) {
		// Scalars are registered too: tclvar exposes the whole interpreter
		// namespace (vtabH-2.0: set xyz 10 then SELECT ... WHERE name='xyz').
		valExpr := tp.varValueExpr(args[1:])
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", base, "", valExpr)
	}
	goName := tclVarToGo(args[0].Text)
	tp.trackArrayKey(args[0].Text)
	if goName == "" || !isValidGoIdent(goName) {
		// Variable name is not a valid Go identifier — skip
		tp.emitLine("// set %s (invalid identifier, skipped)", args[0].Text)
		return
	}
	// Avoid type conflicts: 'err' is Go error type in preamble, 'db' is *frigolite.DB.
	// Redirect TCL string assignments to separate variables.
	goName = tp.redirectErrVar(goName)
	// Skip assignments to DB connection variables (db, db1-db9) from sqlite3_open
	// or other commands that return non-DB values — these would cause type conflicts.
	if tp.skipDBConnectionSet(goName, args) {
		return
	}
	rest := args[1:]

	if tp.setHarnessPinnedVar(goName, rest) {
		return
	}

	if len(rest) == 0 {
		return
	}

	// Scalar set commands mirror into the tclvar registry: generated tests
	// seed module-visible interpreter state through plain sets
	// (`set x1 aback` feeding a tclvar scan), which otherwise never reach
	// the virtual table.
	if !strings.Contains(args[0].Text, "(") && len(rest) > 0 && !strings.HasPrefix(strings.TrimSpace(rest[0].Text), "[") {
		valExpr := tp.varValueExpr(rest)
		tp.emitLine(`vtab.TclVarSet(%q, "", %s)`, args[0].Text, valExpr)
	}

	// set var [cmd ...] — dispatch the bracket-command special cases.
	// A command-substitution word may be represented with a leading space
	// inside the brackets (TCL `set var [ expr {..} ]`); isBracketWord keys
	// off !Braced, so also accept any single word whose trimmed text starts
	// with "[" as a command substitution.
	if len(rest) == 1 && (isBracketWord(rest[0]) || strings.HasPrefix(strings.TrimSpace(rest[0].Text), "[")) {
		cmdText := strings.TrimSuffix(strings.TrimPrefix(rest[0].Text, "["), "]")
		// Dynamic-key array assignment (`set ARR($key) [cmd]`): store into
		// the XxxMap Go map instead of a scalar variable.
		if base, key, isDyn := tp.dynamicArraySet(args[0].Text); isDyn {
			valExpr := tp.cmdExpr(cmdText)
			mapVar := tclVarToGo(base) + "Map"
			if !tp.isVarDeclared(mapVar) {
				tp.emitLine("%s := map[string]string{}", mapVar)
				tp.vars = append(tp.vars, mapVar)
			}
			tp.emitLine("%s[%s] = %s", mapVar, tclVarToGo(key), valExpr)
			return
		}
		if tp.processSetBracketValue(goName, cmdText) {
			return
		}
		// [time { SCRIPT }] and [lindex [time { SCRIPT }] N]: transpile the
		// inner script; timing is not measured, so the variable is bound to
		// "" (time) or "0" (lindex-time, for $microsec<10000000-style
		// comparisons).
		if tp.processSetTimedValue(goName, rest[0].Text) {
			return
		}
	}

	// set VAR "concat $tests {LIST}" (or [concat $tests {LIST}]) — the TCL
	// test-suite idiom that appends a literal TCL list to a variable holding
	// another list (colmeta.test's $tests accumulation). Evaluate the concat
	// at transpile time so the runtime variable holds the combined list.
	if tp.processSetConcatList(goName, rest) {
		return
	}

	tp.processSetGeneric(goName, args, rest)
}

// processSetConcatList handles `set VAR "concat $OTHER {LIST}"` and
// `set VAR [concat $OTHER {LIST}]` — the TCL test-suite idiom that appends a
// literal braced TCL list to a tracked variable holding another list
// (colmeta.test's `set tests "concat $tests {100 ...}"`). The combined list is
// stored in varConstValues so a later `foreach ... $tests` iterates the real
// entries. Returns true when handled.
func (tp *transpiler) processSetConcatList(goName string, rest []tcl.RawWord) bool {
	if len(rest) < 1 {
		return false
	}
	text := strings.TrimSpace(rest[0].Text)
	// Unwrap a bracket wrapper: [concat ...]
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if !strings.HasPrefix(text, "concat ") {
		return false
	}
	restStr := strings.TrimSpace(strings.TrimPrefix(text, "concat "))
	// Find the variable reference ($tests) and the trailing braced list.
	varName := ""
	braced := ""
	if i := strings.Index(restStr, "{"); i >= 0 {
		varName = strings.TrimSpace(restStr[:i])
		braced = strings.TrimSpace(restStr[i:])
	}
	if varName == "" {
		return false
	}
	baseVar := strings.TrimPrefix(varName, "$")
	baseGo := tclVarToGo(baseVar)
	base := tp.varConstValues[baseGo]
	if base == "" && !tp.isVarDeclared(baseGo) {
		return false
	}
	// Strip outer braces of the appended list.
	appended := strings.TrimSpace(braced)
	appended = strings.TrimPrefix(appended, "{")
	appended = strings.TrimSuffix(appended, "}")
	combined := strings.TrimSpace(base)
	if combined != "" && strings.TrimSpace(appended) != "" {
		combined += " "
	}
	combined += strings.TrimSpace(appended)
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = combined
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, tp.goStringLiteral(tcl.RawWord{Text: combined}))
	} else {
		tp.emitLine("var %s = %s", goName, tp.goStringLiteral(tcl.RawWord{Text: combined}))
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setHarnessPinnedVar handles the harness-pinned variable assignments:
// sqlite_current_time (fixed clock) and ::sqlite_interrupt_count (arm the
// vdbe.c per-opcode interrupt countdown). Returns true when handled.
func (tp *transpiler) setHarnessPinnedVar(goName string, rest []tcl.RawWord) bool {
	if len(rest) < 1 {
		return false
	}
	switch goName {
	case "sqlite_current_time":
		val := strings.TrimSpace(rest[0].Text)
		if _, err := strconv.ParseInt(val, 10, 64); err == nil {
			tp.emitLine("function.SetNowFunc(func() time.Time { return time.Unix(%s, 0) })", val)
			return true
		}
	case "sqlite_interrupt_count":
		valExpr := tp.buildStringExpr(rest[0].Text)
		tp.emitLine("sqlite_interrupt_count = %s", valExpr)
		tp.emitLine("db.SetInterruptCount(tclInt(sqlite_interrupt_count))")
		tp.emitLine("_ = sqlite_interrupt_count // suppress unused warning")
		return true
	}
	return false
}

// maybeArmInterruptCount arms the engine countdown when a TCL-namespace-form
// assignment targets ::sqlite_interrupt_count.
func (tp *transpiler) maybeArmInterruptCount(goName string) {
	if goName == "sqlite_interrupt_count" {
		tp.emitLine("db.SetInterruptCount(tclInt(sqlite_interrupt_count))")
	}
}

// skipDBConnectionSet reports whether a set assignment targets a DB connection
// variable (db, db1-db9) from sqlite3_open or another non-DB-returning command,
// which would cause a type conflict.
func (tp *transpiler) skipDBConnectionSet(goName string, args []tcl.RawWord) bool {
	if len(args) < 2 {
		return false
	}
	openText := ""
	for _, word := range args[1:] {
		if strings.Contains(word.Text, "sqlite3_open") {
			openText = strings.Join(func() []string {
				out := make([]string, 0, len(args)-1)
				for _, w := range args[1:] {
					out = append(out, w.Text)
				}
				return out
			}(), " ")
			break
		}
	}
	if openText == "" {
		return false
	}
	// Legacy `set ::dbx [sqlite3_open FILE]` assigns a real connection
	// handle, not TCL text.  Open it directly so later sqlite3_close calls
	// receive *frigolite.DB (tableapi.test uses this form).
	if !isPreDeclaredDB(goName) && goName != "db" {
		openArg := sqlite3OpenArg(openText)
		if openArg == "" {
			return false
		}
		filename := tp.goStringLiteral(tcl.RawWord{Text: openArg})
		if tp.isVarDeclared(goName) {
			tp.emitLine("%s, err = frigolite.Open(%s)", goName, filename)
		} else {
			tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
			tp.vars = append(tp.vars, goName)
		}
		tp.emitLine("if err != nil { t.Fatal(err) }")
		tp.emitLine("defer %s.Close()", goName)
		if tp.dbConnVars == nil {
			tp.dbConnVars = make(map[string]bool)
		}
		tp.dbConnVars[goName] = true
		return true
	}
	// A failed-path sqlite3_open (e.g. /bogus/path/test.db) leaves the
	// connection in the "unable to open" state: sqlite3_errmsg reports the
	// message, sqlite3_errcode reports SQLITE_CANTOPEN, and sqlite3_close
	// succeeds (capi3-3.3/3.4/3.5). Record it so the errmsg/errcode/close
	// handlers emit the C-API values. A reopen also clears any prior
	// closed/failed state for this connection.
	openArg := sqlite3OpenArg(args[1].Text)
	if openArg != "" && strings.Contains(openArg, "/") && !isMainTestFile(openArg) && !strings.HasPrefix(openArg, "test.db") {
		if tp.connFailedOpen == nil {
			tp.connFailedOpen = make(map[string]string)
		}
		tp.connFailedOpen[goName] = "unable to open database file"
	} else {
		// A successful (or empty-path) open clears any prior failed-open state.
		delete(tp.connFailedOpen, goName)
	}
	delete(tp.connClosed, goName)
	tp.emitLine("// set %s [sqlite3_open ...] (skipped, DB connection)", goName)
	return true
}

// sqlite3OpenArg extracts the filename argument of a `sqlite3_open PATH`
// command embedded in a set bracket expression (e.g. `[sqlite3_open
// /bogus/path/test.db {}]`), returning "" when no path is present.
func sqlite3OpenArg(text string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		f = strings.TrimPrefix(f, "[")
		if f == "sqlite3_open" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// redirectErrVar redirects assignments to 'err' (a Go error type in the
// preamble) to a separate TCL string variable, declaring it if needed.
func (tp *transpiler) redirectErrVar(goName string) string {
	if goName != "err" {
		return goName
	}
	goName = "_err_tcl"
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	return goName
}

// processSetTimedValue handles `[time { SCRIPT }]` and `[lindex [time
// { SCRIPT }] N]` set values. Returns true when the value was a timing form.
func (tp *transpiler) processSetTimedValue(goName, bracketText string) bool {
	if strings.HasPrefix(bracketText, "[time ") {
		tp.processSetTimeValue(goName, bracketText)
		return true
	}
	if strings.HasPrefix(bracketText, "[lindex [time ") {
		tp.processSetLindexTimeValue(goName, bracketText)
		return true
	}
	return false
}

// prepareSQLExpr renders prepared SQL text as a Go string expression. Braced
// SQL passes verbatim (TCL performs no $var substitution inside braces);
// otherwise known TCL variables (e.g. bind.test's ?$iMaxVar) become runtime
// concatenations and unknown text stays literal.
func (tp *transpiler) prepareSQLExpr(sqlText string, braced bool) string {
	if braced {
		return fmt.Sprintf("%q", sqlText)
	}
	return tp.buildStringExpr(sqlText)
}

// recordPreparedStatement handles `set ::STMT [sqlite3_prepare db "SQL" -1
// TAIL]` (and the sqlite3_prepare_v2 form), recording the prepared statement
// for bind/step emulation.
func (tp *transpiler) recordPreparedStatement(goName, bracketText string) {
	inner := strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")
	parts := tclCmdWords(inner)
	if len(parts) < 3 || (parts[0] != "sqlite3_prepare" && parts[0] != "sqlite3_prepare_v2") {
		tp.declareStmtHandle(goName)
		return
	}
	sqlText := strings.TrimSpace(parts[2])
	sqlText = strings.Trim(sqlText, `"`)
	ps := tp.preparedStateRef()
	ps.stmts[goName] = sqlText
	conn := "db"
	if len(parts) > 1 {
		conn = tp.dbArgGo(parts[1])
	}
	ps.conns[goName] = conn
	// TCL substitution rules: a braced SQL word passes through verbatim; a
	// quoted/bare word has $var references substituted at runtime.
	braced := false
	if innerCmds := tclparser.ParseCommands(strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")); len(innerCmds) > 0 && len(innerCmds[0]) > 2 {
		braced = innerCmds[0][2].Braced
	}
	if !stmtVMEnabled() {
		// Legacy emulation: only queries run at prepare time (so compile
		// errors reach the connection); INSERT/DDL prepares stay inert.
		tp.emitLine("// prepared %s: %s (bind/step emulation)", goName, sanitizeCommentLine(sqlText))
		if isQueryStmt(lastStatementSQL(sqlText)) {
			tp.emitLine("tclPrepareStep(%s, %q, %q)", conn, sqlText, goName)
		} else if strings.HasPrefix(sqlText, "$") {
			tp.emitLine("tclPrepareStep(%s, %s, %q)", conn, tclVarToGo(strings.TrimPrefix(sqlText, "$")), goName)
		}
		tp.emitPrepareTail(parts, sqlText)
		tp.declareStmtHandle(goName)
		return
	}
	// Runtime prepare (sqlite3_prepare_v2): compile errors set the
	// connection's last-error state; the statement handle is kept for
	// bind/step/reset/finalize emulation. Preparing has no SQL side effects,
	// so INSERT/DDL prepares are safe here too.
	nByte := -1
	if len(parts) > 3 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[3])); err == nil {
			nByte = n
		}
	}
	tp.emitLine("_r = tclPrepareStmt(%s, %q, %s, %d)", conn, goName, tp.prepareSQLExpr(sqlText, braced), nByte)
	tp.emitLine("// prepared %s: %s (bind/step emulation)", goName, sanitizeCommentLine(sqlText))

	// sqlite3_prepare's TAIL argument (parts[4], e.g. `-1 TAIL`) names
	// the variable that receives the SQL text after the first statement.
	// capi2-2.x asserts `set SQL` after a multi-statement prepare returns
	// the tail; assign it (statistically for a literal SQL, at runtime
	// for a $var SQL).
	tp.emitPrepareTail(parts, sqlText)
	tp.declareStmtHandle(goName)
}

// declareStmtHandle emits the prepared-statement handle variable declaration
// (a plain string in the emulation) when it is not already in scope.
func (tp *transpiler) declareStmtHandle(goName string) {
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // prepared statement handle", goName)
}

// emitPrepareQueryCheck emits a db.Query run for a prepared query so a
// compile-time error (bad column/table name) sets the connection's last-error
// state, matching the C-API prepare-error tests. Queries have no side effects;
// INSERT/DDL prepares are NOT run (their side effects happen at step).
//
//lint:ignore U1000 retained for generated prepared-query paths.
//lint:ignore U1000 retained for generated prepared-query paths.
func (tp *transpiler) emitPrepareQueryCheck(sqlText string) {
	// A $var SQL whose constant value is known (set earlier in the file) can
	// be classified statically.
	sqlForCheck := sqlText
	if strings.HasPrefix(strings.TrimSpace(sqlText), "$") {
		if v, ok := tp.sqlVarValues[tclVarToGo(strings.TrimPrefix(strings.TrimSpace(sqlText), "$"))]; ok {
			sqlForCheck = v
		}
	}
	if !isQueryStmt(lastStatementSQL(sqlForCheck)) || strings.HasPrefix(strings.TrimSpace(sqlForCheck), "$") {
		return
	}
	// A prepared multi-statement body (capi3-1.4: "SELECT name FROM
	// sqlite_master;SELECT 10") runs only its first statement at prepare;
	// SQLite compiles the whole text but the tail is returned, not executed.
	// db.Query on the full text would run both; use the first statement only
	// for error detection.
	firstStmt := splitSQLStatements(sqlForCheck)[0]
	tp.emitLine("r = db.Query(%q)", firstStmt)
	tp.emitLine("_ = r.Error // prepare error state is read via db.LastErr/LastErrCode")
}

// emitPrepareTail emits the assignment of sqlite3_prepare's TAIL argument
// (the variable that receives the SQL text after the first statement).
func (tp *transpiler) emitPrepareTail(parts []string, sqlText string) {
	if len(parts) < 5 {
		return
	}
	tailVar := strings.TrimSpace(parts[4])
	if tailVar == "" || strings.HasPrefix(tailVar, "-") || tailVar == "notused" || tailVar == "dummy" {
		return
	}
	goTail := tclVarToGo(strings.TrimPrefix(tailVar, "$"))
	if !isValidGoIdent(goTail) {
		return
	}
	if !tp.isVarDeclared(goTail) {
		tp.emitLine("var %s string", goTail)
		tp.vars = append(tp.vars, goTail)
	}
	if strings.HasPrefix(strings.TrimSpace(sqlText), "$") {
		sqlGo := tclVarToGo(strings.TrimPrefix(strings.TrimSpace(sqlText), "$"))
		tp.emitLine("%s = tclSqlTail(%s)", goTail, sqlGo)
	} else {
		tp.emitLine("%s = tclSqlTail(%q)", goTail, sqlText)
	}
	tp.emitLine("_ = %s // suppress unused warning", goTail)
}

// sanitizeCommentLine collapses whitespace (newlines, tabs, runs of spaces) in
// a text so it can be embedded in a single-line Go comment.
func sanitizeCommentLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// processNamespaceSet handles `set ::var ...` (TCL namespace variables) and the
// testdir infrastructure skip. Returns true when the set was fully handled.
func (tp *transpiler) processNamespaceSet(args []tcl.RawWord) bool {
	varName := args[0].Text
	if varName == "testdir" {
		tp.emitLine("// set testdir: test directory (not used in Go test context)")
		return true
	}
	// set ::arr($key) V — a dynamic-key array assignment with a namespace
	// prefix. Route through the same map-store path as plain arrays
	// (fts4aa.test: set ::fts4aa_res($q) [db eval ...]).
	if strings.HasPrefix(varName, "::") {
		if base, key, isDyn := tp.dynamicArraySet(varName); isDyn {
			tp.emitDynamicArraySet(base, key, args)
			return true
		}
	}
	if !strings.HasPrefix(varName, "::") {
		return false
	}
	// set ::sqlite_current_time N — the TCL test harness pins 'now' for
	// CURRENT_TIME/DATE/TIMESTAMP and date()/time()/datetime('now').
	// Install a fixed clock so the generated test is deterministic.
	if varName == "::sqlite_current_time" && len(args) >= 2 {
		val := strings.TrimSpace(args[1].Text)
		if _, err := strconv.ParseInt(val, 10, 64); err == nil {
			tp.emitLine("function.SetNowFunc(func() time.Time { return time.Unix(%s, 0) })", val)
			return true
		}
	}
	// set ::sqlite3_max_blobsize N — the TCL harness links SQLite's
	// test-only global (test1.c Tcl_LinkVar of src/vdbe.c
	// sqlite3_max_blobsize). Writes go to the engine tracker; reads are
	// re-materialized below so do_test bodies can compare the value.
	if varName == "::sqlite3_max_blobsize" {
		goName := tclVarToGo(varName)
		decl := ""
		if !tp.isVarDeclared(goName) {
			decl = "var "
			tp.vars = append(tp.vars, goName)
		}
		if len(args) >= 2 {
			val := strings.TrimSpace(args[1].Text)
			if n, err := strconv.Atoi(val); err == nil {
				tp.emitLine("%s%s = %q // linked sqlite3_max_blobsize", decl, goName, val)
				tp.emitLine("storage.SetMaxBlobsize(%d)", n)
				return true
			}
		}
		// Query form: refresh the shadow variable from the tracker.
		tp.emitLine("%s = strconv.Itoa(storage.MaxBlobsize()) // linked sqlite3_max_blobsize", goName)
		tp.emitLine("_ = %s", goName)
		return true
	}
	goName := tclVarToGo(varName)
	// Skip invalid identifiers
	if !isValidGoIdent(goName) {
		tp.emitLine("// set %s (invalid identifier, skipped)", varName)
		return true
	}
	// Legacy prepared statement assignment in namespace form.
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "[sqlite3_prepare") {
		tp.recordPreparedStatement(goName, strings.TrimSpace(args[1].Text))
		return true
	}
	// Legacy `set ::dbx [sqlite3_open FILE]` creates a connection handle.
	// Preserve its type so later sqlite3_close receives *frigolite.DB.
	if len(args) >= 2 && strings.Contains(args[1].Text, "sqlite3_open") {
		openArg := sqlite3OpenArg(args[1].Text)
		if openArg != "" {
			filename := tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSuffix(openArg, "]")})
			tp.emitLine("%s, err := frigolite.Open(%s)", goName, filename)
			tp.emitLine("if err != nil { t.Fatal(err) }")
			tp.emitLine("defer %s.Close()", goName)
			if tp.dbConnVars == nil {
				tp.dbConnVars = make(map[string]bool)
			}
			tp.dbConnVars[goName] = true
			return true
		}
	}
	// Skip assignments to DB connection variables (type conflict)
	if isPreDeclaredDB(goName) || goName == "db" {
		if len(args) >= 2 {
			tp.emitLine("// set %s (skipped, DB connection)", varName)
		}
		return true
	}
	if len(args) < 2 {
		// set ::var without value -> query or unset, don't redeclare
		tp.emitLine("_ = %s // TCL namespace variable (query)", goName)
		return true
	}
	// set ::var [queryProc] — inline the query result (e.g.
	// `set ::sig [signature]` where signature returns a db-eval result).
	if tp.inlineNamespaceQuery(goName, args[1]) {
		return true
	}
	// set ::var [expr ...] — evaluate constant/runtime expressions through
	// setExprValue (file-size arithmetic, string ops), matching how plain
	// `set var [expr ...]` is handled. Without this, `set ::size [expr
	// [file size $::cmdlinearg(INFO_SCRIPT)]]` would be emitted as a raw
	// tclExprWith call referencing an undeclared array-map variable.
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "[expr ") {
		if tp.setExprValue(goName, strings.TrimSuffix(strings.TrimSpace(args[1].Text)[1:], "]")) {
			return true
		}
	}
	// set ::data [read $fd2] — read a file channel (fd2 holds a path).
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "[read $") {
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "[read $"), "]")
		// `read $fd N` includes a byte count after the channel var; we want
		// only the channel name. `read $fd` (whole file) has no N.
		parts := strings.Fields(inner)
		if len(parts) == 0 {
			return false
		}
		chanVar := parts[0]
		goChan := tclVarToGo(chanVar)
		if isValidGoIdent(goChan) && tp.isVarDeclared(goChan) {
			if len(parts) >= 2 {
				tp.assignSetValue(goName, fmt.Sprintf("tclReadFileWithLen(%s, %s)", goChan, parts[1]))
			} else {
				tp.assignSetValue(goName, "tclReadFile("+goChan+")")
			}
			return true
		}
	}
	// set ::var [db one {SQL}] — execute the db-onecolumn query and assign.
	if len(args) >= 2 && strings.HasPrefix(strings.TrimSpace(args[1].Text), "[db one") {
		cmdText := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "["), "]")
		if tp.setDBOneValue(goName, cmdText, strings.Fields(cmdText)) {
			return true
		}
	}
	// set ::blob [<conn> incrblob ...] — assign the *frigolite.Blob to the
	// namespace var and register it as a blob channel.
	if len(args) >= 2 && strings.Contains(strings.TrimSpace(args[1].Text), " incrblob ") {
		cmdText := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(args[1].Text), "["), "]")
		cmdParts := strings.Fields(cmdText)
		if len(cmdParts) >= 2 && isDBIncrblobCmd(cmdParts) {
			connName := cmdParts[0]
			restText := strings.TrimSpace(strings.TrimPrefix(cmdText, connName))
			restText = strings.TrimSpace(strings.TrimPrefix(restText, "incrblob"))
			rest := strings.Fields(restText)
			restWords := make([]tcl.RawWord, 0, len(rest))
			for _, f := range rest {
				restWords = append(restWords, tcl.RawWord{Text: f})
			}
			tp.processDBIncrblobTo(goName, connName, restWords)
			return true
		}
		// set ::b [eval db incrblob $arg t1 d 1] — the eval form with a
		// dynamic option variable ($arg is "" or "-readonly").
		if len(cmdParts) >= 4 && cmdParts[0] == "eval" && cmdParts[1] == "db" && cmdParts[2] == "incrblob" {
			rest := cmdParts[3:]
			restWords := make([]tcl.RawWord, 0, len(rest))
			for _, f := range rest {
				restWords = append(restWords, tcl.RawWord{Text: f})
			}
			// $arg (if present) is the first word: "" or "-readonly".
			tp.processDBIncrblobEvalTo(goName, "db", restWords)
			return true
		}
	}
	valExpr := tp.varValueExpr(args[1:])
	// Namespace variables are TCL globals: register into the tclvar registry
	// so USING tclvar scans see them (vtabH 2.0: set ::xyz 10).
	nm := strings.TrimPrefix(varName, "::")
	_, _, isElem := splitArrayElement(nm)
	if !isElem && isValidGoIdent(tclVarToGo(nm)) {
		tp.emitLine("vtab.TclVarSet(%q, %q, %s)", nm, "", valExpr)
		tp.emitTclProcAliasRegistrations(nm, valExpr)
	}
	tp.resolveNamespacePrefix(varName, valExpr)
	// Namespace variables whose names appear in knownGlobalVars (e.g.
	// `oplog` — the journal2 testvfs sink) are package-level helpers-
	// template variables; emit a plain assignment, not a `var` redeclaration.
	if tp.isVarDeclared(goName) || knownGlobalVars()[goName] {
		tp.emitLine("%s = %s // TCL namespace variable", goName, valExpr)
	} else {
		tp.emitLine("var %s = %s // TCL namespace variable", goName, valExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	tp.maybeArmInterruptCount(goName)
	// Track simple string-literal assignments so later commands
	// (e.g. sqlite3_create_collation_v2's $cmd destructor) can
	// resolve the variable's constant value.
	tp.trackVarConstValue(goName, args)
	return true
}

// resolveNamespacePrefix updates tp.testPrefix when a set ::testprefix or
// set testprefix value is assigned. valExpr is a Go expression (usually a
// quoted string literal); resolve it to the plain name for the skip lookup,
// stripping the surrounding quotes.
func (tp *transpiler) resolveNamespacePrefix(varName, valExpr string) {
	if varName != "::testprefix" && varName != "testprefix" {
		return
	}
	prefix := strings.TrimSpace(valExpr)
	if len(prefix) >= 2 && prefix[0] == '"' && prefix[len(prefix)-1] == '"' {
		prefix = prefix[1 : len(prefix)-1]
	}
	tp.testPrefix = prefix
}

// inlineNamespaceQuery inlines a query-proc result assigned to a TCL
// namespace variable (`set ::sig [signature]`). Returns true when the value
// was a recognized query proc.
func (tp *transpiler) inlineNamespaceQuery(goName string, valWord tcl.RawWord) bool {
	if len(valWord.Text) < 2 || !strings.HasPrefix(valWord.Text, "[") || !strings.HasSuffix(valWord.Text, "]") || len(tp.queryFuncs) == 0 {
		return false
	}
	innerCmd := strings.TrimSuffix(strings.TrimPrefix(valWord.Text, "["), "]")
	cmdParts := strings.Fields(innerCmd)
	if sql, ok := tp.queryFuncs[cmdParts[0]]; ok {
		tp.emitQueryVarAssign(goName, sql)
		return true
	}
	return false
}

// trackVarConstValue records a simple string-literal assignment ("lit" or
// {lit}) in varConstValues so later commands can resolve the constant.
func (tp *transpiler) trackVarConstValue(goName string, args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	lit := args[1].Text
	if len(lit) < 2 {
		return
	}
	if !((lit[0] == '"' && lit[len(lit)-1] == '"') || (lit[0] == '{' && lit[len(lit)-1] == '}')) {
		return
	}
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = lit[1 : len(lit)-1]
}

// emitQueryVarAssign emits an assignment of a query-proc result to goName.
func (tp *transpiler) emitQueryVarAssign(goName, sql string) {
	sqlExpr := tp.buildSQLStringExpr(sql)
	dbEvalVar := fmt.Sprintf("_dbeval%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclExecSQL(db, %s)", dbEvalVar, sqlExpr)
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, dbEvalVar)
	} else {
		tp.emitLine("var %s = %s", goName, dbEvalVar)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}

// isBracketWord reports whether w is an unbraced word starting with "[".
func isBracketWord(w tcl.RawWord) bool {
	return !w.Braced && strings.HasPrefix(w.Text, "[")
}

// isLsearchCmd reports whether cmdParts is `lsearch ...` with >= 3 words.
func isLsearchCmd(cmdParts []string) bool {
	return len(cmdParts) >= 3 && cmdParts[0] == "lsearch"
}

// isMakeExprCmd reports whether cmdParts starts with make_expr1/2/3.
func isMakeExprCmd(cmdParts []string) bool {
	if len(cmdParts) < 1 {
		return false
	}
	return cmdParts[0] == "make_expr1" || cmdParts[0] == "make_expr2" || cmdParts[0] == "make_expr3"
}

// isRegexpCmd reports whether cmdParts is `regexp ...` with >= 3 words.
func isRegexpCmd(cmdParts []string) bool {
	return len(cmdParts) >= 3 && cmdParts[0] == "regexp"
}

// isDBEvalCmd reports whether cmdParts is `db eval ...`.
func isDBEvalCmd(cmdParts []string) bool {
	if len(cmdParts) < 2 || cmdParts[1] != "eval" {
		return false
	}
	conn := cmdParts[0]
	return conn == "db" || isPreDeclaredDB(conn) || strings.HasPrefix(conn, "db")
}

// isDBOneCmd reports whether cmdParts is `db one ...` or `db onecolumn ...`.
func isDBOneCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "db" && len(cmdParts) >= 2 && (cmdParts[1] == "one" || cmdParts[1] == "onecolumn")
}

// isDBIncrblobCmd reports whether cmdParts is `<conn> incrblob ...` where
// <conn> is a database connection (db, db2, ...).
func isDBIncrblobCmd(cmdParts []string) bool {
	if len(cmdParts) < 2 || cmdParts[1] != "incrblob" {
		return false
	}
	conn := cmdParts[0]
	return conn == "db" || isPreDeclaredDB(conn) || strings.HasPrefix(conn, "db")
}

// isSqlite3OpenCmd reports whether cmdParts is `sqlite3 ...` with >= 3 words.
func isSqlite3OpenCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "sqlite3" && len(cmdParts) >= 3
}

// isCatchCmd reports whether cmdParts is `catch ...` with >= 2 words.
func isCatchCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "catch" && len(cmdParts) >= 2
}

// isListCmd reports whether cmdParts starts with "list".
func isListCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "list"
}

// isExprCmd reports whether cmdParts starts with "expr".
func isExprCmd(cmdParts []string) bool {
	return len(cmdParts) > 0 && cmdParts[0] == "expr"
}

// inlineQueryFuncValue inlines a query-proc result (`set var [queryProc]`)
// when the command is a registered query proc. Returns true when inlined.
func (tp *transpiler) inlineQueryFuncValue(goName string, cmdParts []string) bool {
	if len(cmdParts) == 0 || len(tp.queryFuncs) == 0 {
		return false
	}
	sql, ok := tp.queryFuncs[cmdParts[0]]
	if !ok {
		return false
	}
	// set var [queryProc] — the proc returns a db-eval result
	// (e.g. `proc signature {} { return [db eval {SELECT ...}] }`);
	// inline the query and assign the flattened result.
	tp.emitQueryVarAssign(goName, sql)
	return true
}

// globalUserProcs records test-local procs with faithful Go runtime
// implementations. It is package-level because do_test/db-eval/for/foreach
// bodies transpile through cloned sub-transpilers that would otherwise drop
// per-instance registration state; gen.go clears it before each file.
var globalUserProcs = map[string]bool{}

// globalProcBodies mirrors tp.procBodies across sub-transpiler scopes so the
// dispatch layer can fingerprint a file-local definition at its CALL site
// (rtree8/rtreeA fixture procs). gen.go clears it before each file.
var globalProcBodies = map[string]string{}

// markUserProcGlobal registers a proc name as registry-backed for this file.
func markUserProcGlobal(name string) { globalUserProcs[name] = true }

func init() {
	// keep package-level helpers together; no-op initializer
}

// processSetBracketValue dispatches `set var [cmd ...]` to the special-case
// emitters. Returns true when the value was fully handled.

// activeFileChannels tracks TCL file channels opened in write mode
// (`set fd [open FILE wb]`): var name -> path. `puts $fd text` appends to
// the file; `close $fd` unregisters (csv01 5.x setup parity).
var activeFileChannels = map[string]string{}

// activeFileChannelExprs marks channels whose stored destination is a Go
// EXPRESSION (variable TCL path) rather than a quoted literal.
var activeFileChannelExprs = map[string]bool{}

// fileChannelSeek tracks the current byte position of each write-mode file
// channel so that `seek $fd N start` followed by `puts -nonewline $fd DATA`
// writes to the right offset (TCL fconfigure -translation binary + seek +
// puts is the canonical pattern for hex-corrupting a database file at a
// known offset, used by every corrupt*.test suite). The seek offset is
// applied via tclChannelAppendAt on the next puts. corrupt2.test 1.4/1.5
// relies on this to write "\xFF\xFF" at byte 101 — without it the bytes
// land at end-of-file and the corruption detection never fires.
var fileChannelSeek = map[string]int64{}

// channelDestExpr renders a channel's destination: quoted literal, or the
// stored Go expression verbatim for variable TCL paths.
func channelDestExpr(chName, path string) string {
	if activeFileChannelExprs[chName] && isValidGoIdent(path) {
		return path
	}
	return strconv.Quote(path)
}

// parseOpenChannelWord recognizes a bracketed `[open PATH MODE]`
// command-substitution word used as a set RHS. Returns the path and mode.
func parseOpenChannelWord(word string) (path, mode string, ok bool) {
	w := strings.TrimSpace(word)
	if !strings.HasPrefix(w, "[") || !strings.HasSuffix(w, "]") {
		return "", "", false
	}
	inner := strings.TrimSpace(w[1 : len(w)-1])
	fields := strings.Fields(inner)
	if len(fields) < 2 || fields[0] != "open" {
		return "", "", false
	}
	ppath := strings.Trim(fields[1], "\"'")
	pmode := ""
	if len(fields) >= 3 {
		pmode = fields[2]
	}
	return ppath, pmode, true
}

// splitArrayElement splits a TCL variable reference "arr(key)" into
// (arr, key, true); plain names return false.
func splitArrayElement(ref string) (base, key string, ok bool) {
	idx := strings.Index(ref, "(")
	if idx <= 0 || !strings.HasSuffix(ref, ")") {
		return "", "", false
	}
	base = strings.TrimSpace(ref[:idx])
	key = strings.TrimSpace(ref[idx+1 : len(ref)-1])
	return base, key, true
}

// activeTclvarBases tracks array bases whose elements are registered in the
// tclvar registry (package-level so nested body transpilers see it).
var activeTclvarBases = map[string]bool{}

// tclProcVarAliases maps proc names to the TCL global their body returns
// (`proc p {} { return $::g }` → p→g). Registration sites for g also
// register p so the `tcl` vtab module can resolve its argument.
var tclProcVarAliases = map[string]string{}

// markTclProcAlias records that proc name returns global target.
func markTclProcAlias(name, target string) {
	tclProcVarAliases[name] = target
}

// procReturnGlobalAlias extracts the global from a body whose only effect is
// `return $::name`; returns "" otherwise.
func procReturnGlobalAlias(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "return ") {
			continue
		}
		ref := strings.TrimSpace(strings.TrimPrefix(ln, "return "))
		if strings.HasPrefix(ref, "$::") {
			return strings.TrimPrefix(ref, "$::")
		}
	}
	return ""
}

// emitTclProcAliasRegistrations emits extra registry writes so proc aliases
// of var nm carry the same value at runtime.
func (tp *transpiler) emitTclProcAliasRegistrations(nm, valExpr string) {
	for procName, target := range tclProcVarAliases {
		if target == nm && isValidGoIdent(tclVarToGo(procName)) {
			tp.emitLine("vtab.TclVarSet(%q, %q, %s)", procName, "", valExpr)
		}
	}
}

func markTclvarBase(base string) {
	if base != "" {
		activeTclvarBases[base] = true
	}
}
