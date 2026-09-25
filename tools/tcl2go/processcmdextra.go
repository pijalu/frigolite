// Package main implements the tcl2go tool.
//
// This file contains the individual TCL command emitters dispatched from
// processCommand. Each handler has a single responsibility.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// runSubBody parses a braced body at args[idx], transpiles it in a fresh
// sub-transpiler (sharing the output buffer and state), and copies the
// sub-transpiler's state back into tp. Returns true when a body was run.
func (tp *transpiler) runSubBody(args []tcl.RawWord, idx int) bool {
	bodyCmds := tp.parseBracedBody(args, idx)
	if bodyCmds == nil {
		return false
	}
	bodyTP := &transpiler{
		sb:            tp.sb,
		indent:        tp.indent,
		dbVar:         tp.dbVar,
		t:             tp.t,
		varCount:      tp.varCount,
		vars:          tp.vars,
		arrayKeys:     tp.arrayKeys,
		arrayMapVars:  tp.arrayMapVars,
		forIncrs:      tp.forIncrs,
		testPrefix:    tp.testPrefix,
		preparedState: tp.preparedState,
		unsetVars:     tp.unsetVars,
		dbVarFuncs:    tp.dbVarFuncs,
		constFuncs:    tp.constFuncs, quotaCallbacks: tp.quotaCallbacks,
		identityFuncs: tp.identityFuncs,
		predFuncs:     tp.predFuncs,
		queryFuncs:    tp.queryFuncs,
		specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
		autovacCallbacks:    tp.autovacCallbacks,
		rangeListFuncs:      tp.rangeListFuncs,
		collateDtorVars:     tp.collateDtorVars,
		collateGoFuncs:      tp.collateGoFuncs,
		collateEmittedProcs: tp.collateEmittedProcs,
		procBodies:          tp.procBodies,
		queryVars:           tp.queryVars,
		dbAliases:           tp.dbAliases,
		dbClosed:            tp.dbClosed,
		fixtureVar:          tp.fixtureVar,
		dqsDDL:              tp.dqsDDL,
		dqsDML:              tp.dqsDML,
		authTypeName:        tp.authTypeName,
		authProcCount:       tp.authProcCount,
		authProcGo:          tp.authProcGo,
		authPreamble:        tp.authPreamble,
		authCurrentDeclared: tp.authCurrentDeclared,
		testDir:             tp.testDir,
		genesisPreamble:     tp.genesisPreamble,
		varConstValues:      tp.varConstValues,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.vars = bodyTP.vars
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.predFuncs = bodyTP.predFuncs
	tp.queryFuncs = bodyTP.queryFuncs
	tp.specialFuncs = bodyTP.specialFuncs
	tp.rangeListFuncs = bodyTP.rangeListFuncs
	tp.collateDtorVars = bodyTP.collateDtorVars
	tp.collateGoFuncs = bodyTP.collateGoFuncs
	tp.queryVars = bodyTP.queryVars
	tp.dbAliases = bodyTP.dbAliases
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.authTypeName = bodyTP.authTypeName
	tp.authProcCount = bodyTP.authProcCount
	tp.authProcGo = bodyTP.authProcGo
	tp.authPreamble = bodyTP.authPreamble
	tp.authCurrentDeclared = bodyTP.authCurrentDeclared
	tp.genesisPreamble = bodyTP.genesisPreamble
	tp.varConstValues = bodyTP.varConstValues
	return true
}

// processStepsql handles `stepsql $DB {SQL}` — the test harness helper that
// executes a batch of statements on the main connection.
func (tp *transpiler) processStepsql(args []tcl.RawWord) {
	if len(args) >= 2 {
		tp.processExecSQL(append([]tcl.RawWord{args[1]}, args[2:]...), "exec")
	} else if len(args) == 1 {
		tp.processExecSQL(args, "exec")
	}
}

// processSQLVar handles `sql $VAR` — the test-harness helper that runs a SQL
// string held in a variable (e.g. savepoint6's DATABASE_SCHEMA).
func (tp *transpiler) processSQLVar(args []tcl.RawWord) {
	if len(args) >= 1 && strings.HasPrefix(args[0].Text, "$") {
		gv := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
		if isValidGoIdent(gv) {
			tp.emitLine("_res = db.Exec(%s)", gv)
			tp.emitLine("if _res.Error != nil {")
			tp.emitLine("\tt.Errorf(\"exec error: %%v\", _res.Error)")
			tp.emitLine("}")
			return
		}
	}
	tp.emitLine("// sql %s (unsupported command, not transpiled)", sanitizeTCLComment(describeArgsShort(args)))
}

// processOptimizationControl handles `optimization_control DB OPT BOOLEAN` —
// SQLite's test-harness toggle for query-planner optimizations. We map the
// most common flags (skip-scan, query-flattener) to engine PRAGMAs and emit a
// no-op comment for the rest. The full optimization_control mask includes
// ~15 flags; only the ones we implement are honored.
func (tp *transpiler) processOptimizationControl(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitLine("// optimization_control (insufficient args)")
		return
	}
	dbVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	if dbVar == "" {
		dbVar = tp.dbVar
	}
	opt := strings.ToLower(strings.TrimSpace(args[1].Text))
	onOff := strings.ToLower(strings.TrimSpace(args[2].Text))
	on := onOff == "on" || onOff == "1" || onOff == "true"

	switch opt {
	case "skip-scan", "skipscan":
		val := "0"
		if on {
			val = "1"
		}
		tp.emitLine("_res = %s.Exec(\"PRAGMA skip_scan = %s\")", dbVar, val)
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"optimization_control skip-scan error: %%v\", _res.Error)")
		tp.emitLine("}")
	case "all":
		// Restore every optimization we map. When "all" is OFF we emit a
		// commented-out marker because we don't have per-flag setters for
		// query-flattener / push-down / etc., but turning "all" back ON must
		// re-enable skip-scan so downstream tests (skipscan1-2.2eqp etc.)
		// see the optimizer in its default state.
		val := "0"
		if on {
			val = "1"
		}
		tp.emitLine("_res = %s.Exec(\"PRAGMA skip_scan = %s\")", dbVar, val)
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"optimization_control all skip-scan error: %%v\", _res.Error)")
		tp.emitLine("}")
	default:
		// Unimplemented optimization_control flag (query-flattener, distinct-opt,
		// transitive, push-down, etc.). Emit a no-op so the surrounding tests run.
		tp.emitLine("// optimization_control %s %s (no PRAGMA equivalent; ignored)",
			sanitizeTCLComment(opt), sanitizeTCLComment(onOff))
	}
}

// processCapturePragma handles `capture_pragma DB TABNAME {SQL}` — runs the
// pragma, builds a TEMP table from the result columns, and inserts the rows.
func (tp *transpiler) processCapturePragma(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	dbVar := tclVarToGo(strings.TrimPrefix(args[0].Text, "$"))
	if dbVar == "" {
		dbVar = tp.dbVar
	}
	tabName := strings.TrimSpace(args[1].Text)
	sqlExpr := tp.collectSQLExpression(args[2:3])
	capVar := fmt.Sprintf("capPragma%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := %s.Query(%s)", capVar, dbVar, sqlExpr)
	tp.emitLine("if %s.Error != nil { t.Errorf(\"capture_pragma error: %%v\", %s.Error) }", capVar, capVar)
	tp.emitLine("%s.Exec(\"DROP TABLE IF EXISTS temp.%s\")", dbVar, tabName)
	tp.emitLine("{ // capture_pragma %s", tabName)
	tp.indent++
	tp.emitLine("if len(%s.Columns) > 0 {", capVar)
	tp.indent++
	tp.emitLine("var colList []string")
	tp.emitLine("for _, c := range %s.Columns { colList = append(colList, %q + c + %q) }", capVar, "\"", "\"")
	tp.emitLine("%s.Exec(\"CREATE TEMP TABLE %s (\" + strings.Join(colList, \",\") + \")\")", dbVar, tabName)
	tp.emitLine("for _, row := range %s.Rows {", capVar)
	tp.indent++
	tp.emitLine("var vals []string")
	tp.emitLine("for _, v := range row { vals = append(vals, strconv.Quote(tclStr(v))) }")
	tp.emitLine("%s.Exec(\"INSERT INTO %s VALUES (\" + strings.Join(vals, \",\") + \")\")", dbVar, tabName)
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processSqlite3TestControl handles sqlite3_test_control; only the localtime
// fault control is transpiled (SQLite's TCL harness installs an alternative
// localtime that the date tests rely on).
func (tp *transpiler) processSqlite3TestControl(args []tcl.RawWord) {
	if len(args) >= 2 && args[0].Text == "SQLITE_TESTCTRL_LOCALTIME_FAULT" {
		mode := strings.TrimSpace(args[1].Text)
		switch mode {
		case "1":
			// main.c:4469: bLocaltimeFault=1, xAltLocaltime cleared —
			// osLocaltime always fails ("local time unavailable",
			// tkt-bd484a090c 2.1/2.2).
			tp.emitLine("function.SetLocaltimeFault(true)")
		case "2":
			// main.c:4471: bLocaltimeFault=2 installs the alternate
			// localtime implementation (date.test's even/odd day hook).
			tp.emitLine("function.SetLocaltimeHook(tclTestLocaltime)")
		case "0":
			// main.c:4475: fault cleared and xAltLocaltime reset to nil.
			tp.emitLine("function.SetLocaltimeFault(false)")
			tp.emitLine("function.SetLocaltimeHook(nil)")
		default:
			tp.emitLine("// sqlite3_test_control SQLITE_TESTCTRL_LOCALTIME_FAULT %s (unsupported mode)", mode)
		}
		return
	}
	// `sqlite3_test_control SQLITE_TESTCTRL_PENDING_BYTE 0x0010000`:
	// update the global ::sqlite_pending_byte to the parsed value. The
	// transpiler materialises the value as a Go literal so file-size
	// checks (autovacuum-9.3 / 9.5) compare against the same number the
	// SQLite C harness would have computed (tester.tcl:102 pins
	// 0x10000=65536).
	if len(args) >= 1 && args[0].Text == "SQLITE_TESTCTRL_PENDING_BYTE" {
		tp.emitPendingByteControl(args)
	}
}

// emitPendingByteControl updates the global ::sqlite_pending_byte shadow
// variable from the parsed numeric argument of sqlite3_test_control
// SQLITE_TESTCTRL_PENDING_BYTE.
func (tp *transpiler) emitPendingByteControl(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	val := strings.TrimSpace(args[1].Text)
	var n int64
	var err error
	if strings.HasPrefix(val, "0x") || strings.HasPrefix(val, "0X") {
		n, err = strconv.ParseInt(val[2:], 16, 64)
	} else {
		n, err = strconv.ParseInt(val, 0, 64)
	}
	if err != nil {
		tp.emitLine("// sqlite3_test_control SQLITE_TESTCTRL_PENDING_BYTE %s (parse error: %v)", val, err)
		return
	}
	tp.emitLine("sqlite_pending_byte = %q // sqlite3_test_control SQLITE_TESTCTRL_PENDING_BYTE %s", strconv.FormatInt(n, 10), val)
}

// processSqlite3TestControlPendingByte handles the standalone TCL command
// `sqlite3_test_control_pending_byte 0x0010000` (tester.tcl:102, trans.tcl,
// etc.) — the C-defined wrapper around sqlite3_test_control
// SQLITE_TESTCTRL_PENDING_BYTE. The call MOVES the engine's pending byte
// (db.SetPendingByte, the src/test2.c testPendingByte override) and
// records the value in the Go shadow variable so file-size comparisons in
// the test see the harness's expected value rather than the production
// default. A `$var` argument dispatches at runtime (pager1.test 42.x
// restores the byte with `sqlite3_test_control_pending_byte $pending_prev`).
func (tp *transpiler) processSqlite3TestControlPendingByte(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	val := strings.TrimSpace(args[0].Text)
	if strings.HasPrefix(val, "$") {
		tp.emitPendingByteRuntime(val)
		return
	}
	var n int64
	var err error
	if strings.HasPrefix(val, "0x") || strings.HasPrefix(val, "0X") {
		n, err = strconv.ParseInt(val[2:], 16, 64)
	} else {
		n, err = strconv.ParseInt(val, 0, 64)
	}
	if err != nil {
		tp.emitLine("// sqlite3_test_control_pending_byte %s (parse error: %v)", val, err)
		return
	}
	tp.emitLine("%s.SetPendingByte(%d)", tp.dbVar, n)
	tp.emitLine("sqlite_pending_byte = %q // sqlite3_test_control_pending_byte %s", strconv.FormatInt(n, 10), val)
}

// emitPendingByteRuntime handles the `$var` argument form of
// sqlite3_test_control_pending_byte: the byte moves at runtime from the
// declared variable's current value.
func (tp *transpiler) emitPendingByteRuntime(val string) {
	goVar := tclVarToGo(strings.TrimPrefix(val, "$"))
	if isValidGoIdent(goVar) && tp.isVarDeclared(goVar) {
		tp.emitLine("%s.SetPendingByte(uint32(tclAtoi(%s)))", tp.dbVar, goVar)
		tp.emitLine("sqlite_pending_byte = %s", goVar)
		return
	}
	tp.emitLine("// sqlite3_test_control_pending_byte %s (undeclared var)", val)
}

// processSqlite3Limit handles sqlite3_limit; the expression/trigger depth,
// column-count, and length limits are transpiled as db.Set*Limit calls.
func (tp *transpiler) processSqlite3Limit(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	limitName := args[1].Text
	period := strings.TrimSpace(args[2].Text)
	switch limitName {
	case "SQLITE_LIMIT_EXPR_DEPTH":
		varName := strings.TrimPrefix(period, "$")
		if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && tp.isVarDeclared(varName)) {
			tp.emitLine("db.SetExprDepthLimit(toInt(%s))", replaceVarRefsRaw(period))
		}
	case "SQLITE_LIMIT_TRIGGER_DEPTH":
		tp.emitTriggerDepthLimit(args[2].Text)
	case "SQLITE_LIMIT_COLUMN", "SQLITE_LIMIT_LENGTH", "SQLITE_LIMIT_SQL_LENGTH",
		"SQLITE_LIMIT_COMPOUND_SELECT", "SQLITE_LIMIT_FUNCTION_ARG",
		"SQLITE_LIMIT_LIKE_PATTERN_LENGTH", "SQLITE_LIMIT_VARIABLE_NUMBER":
		tp.emitNamedLimit(limitName, period)
	}
}

// emitNamedLimit sets a numeric sqlite3_limit directly, or resolves
// [expr ...] constants at transpile time (e.g. [expr $::SQLITE_MAX_COLUMN+1]).
func (tp *transpiler) emitNamedLimit(limitName, period string) {
	n := ""
	varName := strings.TrimPrefix(period, "$")
	if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && (tp.isVarDeclared(varName) || knownGlobalVars()[varName])) {
		n = replaceVarRefsRaw(period)
	} else if strings.HasPrefix(period, "[expr ") {
		n = limitExprValue(strings.TrimSuffix(strings.TrimPrefix(period, "[expr "), "]"))
	}
	if n != "" {
		tp.emitLine("db.SetLimit(%q, toInt(%s))", limitName, n)
	}
}

// limitExprValue resolves a sqlite3_limit [expr ...] argument at transpile
// time: substitutes known SQLITE_MAX_* compile-time constants (with and
// without the :: namespace prefix), evaluates the arithmetic, and falls back
// to a runtime tclExpr call when unresolvable.
func limitExprValue(exprBody string) string {
	subst := exprBody
	for k, val := range map[string]string{
		"SQLITE_MAX_COLUMN":       "2000",
		"::SQLITE_MAX_COLUMN":     "2000",
		"SQLITE_MAX_LENGTH":       "1000000000",
		"::SQLITE_MAX_LENGTH":     "1000000000",
		"SQLITE_MAX_SQL_LENGTH":   "1000000000",
		"::SQLITE_MAX_SQL_LENGTH": "1000000000",
	} {
		subst = strings.ReplaceAll(subst, "$"+k, val)
	}
	if v, err := tcl.EvalExpr(subst, &tcl.Interp{}, nil); err == nil {
		return v
	}
	return fmt.Sprintf("tclExpr(%q)", exprBody)
}

// limitValueExpr resolves a sqlite3_limit do_test set argument to a Go int
// expression: integers/$vars pass through, [expr ...] resolves via
// limitExprValue (SQLITE_MAX_* constants substituted).
func (tp *transpiler) limitValueExpr(setVal string) string {
	setVal = strings.TrimSpace(setVal)
	if strings.HasPrefix(setVal, "[expr ") {
		return limitExprValue(strings.TrimSuffix(strings.TrimPrefix(setVal, "[expr "), "]"))
	}
	if isIntegerLiteral(setVal) {
		return strconv.Quote(setVal)
	}
	// TCL hex literals (sqllimits1-4.x set limits to 0x7fffffff):
	// not decimal literals — pass through as Go hex ints (toInt's
	// string case only handles decimal via Atoi).
	if len(setVal) > 2 && setVal[0] == '0' && (setVal[1] == 'x' || setVal[1] == 'X') {
		return setVal
	}
	return replaceVarRefsRaw(setVal)
}

// emitTriggerDepthLimit emits db.SetTriggerDepthLimit for a sqlite3_limit
// SQLITE_LIMIT_TRIGGER_DEPTH argument, resolving plain integers, declared TCL
// variables, and [expr ...] constants at transpile time.
func (tp *transpiler) emitTriggerDepthLimit(periodArg string) {
	period := strings.TrimSpace(periodArg)
	varName := strings.TrimPrefix(period, "$")
	if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && (tp.isVarDeclared(varName) || knownGlobalVars()[varName])) {
		tp.emitLine("db.SetTriggerDepthLimit(toInt(%s))", replaceVarRefsRaw(period))
	} else if strings.HasPrefix(period, "[expr ") {
		// [expr $SQLITE_MAX_TRIGGER_DEPTH / 10] — resolve known
		// constants at transpile time.
		exprBody := strings.TrimSuffix(strings.TrimPrefix(period, "[expr "), "]")
		if v, err := tcl.EvalExpr(exprBody, &tcl.Interp{}, map[string]string{"SQLITE_MAX_TRIGGER_DEPTH": "1000"}); err == nil {
			tp.emitLine("db.SetTriggerDepthLimit(toInt(%q))", v)
		} else {
			tp.emitLine("db.SetTriggerDepthLimit(toInt(tclExpr(%q)))", exprBody)
		}
	}
}

// processCreateCollation handles sqlite3_create_collation_v2: registers a
// custom collation and records its destructor counter so delete/close fire it.
func (tp *transpiler) processCreateCollation(args []tcl.RawWord) {
	if len(args) < 4 {
		return
	}
	collName := strings.TrimSpace(args[1].Text)
	procArg := strings.TrimSpace(args[2].Text)
	var goFn string
	if f := collationProcGo(procArg); f != "" {
		goFn = f
	} else if fn, ok := tp.collateGoFuncs[procArg]; ok {
		goFn = fn
	}
	if goFn == "" || collName == "" {
		tp.emitLine("// sqlite3_create_collation_v2 %s (not transpiled)", collName)
		return
	}
	tp.emitLine("db.RegisterCollation(%s, %s)", tp.goStringLiteral(args[1]), goFn)
	tp.trackCollationDtor(collName, args[3].Text)
}

// trackCollationDtor records the destructor counter (e.g. `incr ::VAR`) for a
// collation so sqlite_delete_collation and db close fire it, matching SQLite's
// xDestroy callback. The destructor may be inline ({incr ::VAR}) or a $var
// holding a [list incr ::VAR].
func (tp *transpiler) trackCollationDtor(collName, dtorText string) {
	dtor := strings.TrimSpace(dtorText)
	if strings.HasPrefix(dtor, "$") {
		if v, ok := tp.varConstValues[tclVarToGo(strings.TrimPrefix(dtor, "$"))]; ok {
			dtor = v
		}
	}
	if incrVar := counterProcValue(dtor); incrVar != "" {
		if tp.collateDtorVars == nil {
			tp.collateDtorVars = make(map[string]string)
		}
		tp.collateDtorVars[strings.ToUpper(collName)] = incrVar
	}
}

// processDeleteCollation handles sqlite_delete_collation: unregisters a
// collation and fires its destructor (increments the tracked counter var).
func (tp *transpiler) processDeleteCollation(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	collName := strings.TrimSpace(args[1].Text)
	tp.emitLine("db.UnregisterCollation(%s)", tp.goStringLiteral(args[1]))
	if tp.collateDtorVars != nil {
		if incrVar, ok := tp.collateDtorVars[strings.ToUpper(collName)]; ok {
			tp.emitIncrCounter(incrVar)
			delete(tp.collateDtorVars, strings.ToUpper(collName))
		}
	}
}

// processDeleteFile handles `delete_file PATH [PATH...]` — the tester.tcl
// helper that removes files (like forcedelete without the reset semantics).
func (tp *transpiler) processDeleteFile(args []tcl.RawWord) {
	for _, a := range args {
		tp.emitLine("os.Remove(%s)", tp.goStringLiteral(tcl.RawWord{Text: a.Text}))
		// The next sqlite3 db <file> open of this file starts from a fresh
		// database (same as forcedelete: the TCL delete_file + sqlite3 db
		// pattern resets the connection).
		if tp.pendingFileReset == nil {
			tp.pendingFileReset = make(map[string]bool)
		}
		tp.pendingFileReset[a.Text] = true
		// This body remove supersedes one hoisted pre-Open delete of the
		// same path (pragma2: the file-top `delete_file test.db` hoists an
		// entry; the mid-file 4.1 reset emits its own os.Remove; without
		// decrementing, the later 5.1 `forcedelete test.db` would be
		// silently swallowed by the stale entry and the reopen would see
		// the old database).
		if genPreDeleted[a.Text] > 0 {
			genPreDeleted[a.Text]--
		}
	}
}

// processResetDB handles reset_db: close, delete test.db, reopen on ./test.db.
// Reopening on the same filename matters because a later "sqlite3 db test.db"
// reopens that file and must find the writes made after reset. The TCL
// driver's nullvalue setting is per-connection; the fresh connection created
// here starts with the default (empty-string) rendering, so reset the harness
// nullvalue to "{}".
func (tp *transpiler) processResetDB() {
	tp.emitLine("db.Close()")
	// tester.tcl reset_db:551 forcedeletes test.db, test.db-journal AND
	// test.db-wal — a leftover -wal from a previous WAL-mode section would
	// replay into the freshly created database and resurrect dropped
	// objects (pragma3-5.1x "table t1 already exists").
	tp.emitLine("os.Remove(\"test.db\")")
	tp.emitLine("os.Remove(\"test.db-journal\")")
	tp.emitLine("os.Remove(\"test.db-wal\")")
	tp.emitLine("db, err = frigolite.Open(\"test.db\")")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("tcl_nullvalue = \"{}\" // fresh connection resets nullvalue")
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
}

// processDBSaveAndClose handles the TCL framework's db_save_and_close:
// snapshot the database files (test.db and its journal/sidecars) under the
// sv_ prefix and close the connection. The engine keeps the file-backed
// state on disk, so the snapshot is a file copy; the next
// db_restore_and_reopen restores it.
func (tp *transpiler) processDBSaveAndClose() {
	tp.emitLine("// db_save_and_close: snapshot test.db* under sv_ prefix")
	tp.emitLine("for _, _sf := range tclSplitList(tclGlob(\"test.db*\")) {")
	tp.emitLine("\ttclFileCopy(_sf, \"sv_\"+_sf)")
	tp.emitLine("}")
	tp.emitLine("db.Close()")
	tp.emitLine("db, err = frigolite.Open(\"test.db\")")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("tcl_nullvalue = \"{}\" // fresh connection resets nullvalue")
	tp.dqsDDL = true
	tp.dqsDML = true
}

// processDBRestoreAndReopen handles db_restore_and_reopen: restore the
// snapshot files (sv_test.db*) over test.db* and reopen the connection.
func (tp *transpiler) processDBRestoreAndReopen() {
	tp.emitLine("// db_restore_and_reopen: restore sv_test.db* snapshot")
	tp.emitLine("db.Close()")
	tp.emitLine("for _, _sf := range tclSplitList(tclGlob(\"test.db*\")) { os.Remove(_sf) }")
	tp.emitLine("for _, _sv := range tclSplitList(tclGlob(\"sv_test.db*\")) {")
	tp.emitLine("\ttclFileCopy(_sv, strings.TrimPrefix(_sv, \"sv_\"))")
	tp.emitLine("}")
	tp.emitLine("db, err = frigolite.Open(\"test.db\")")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("tcl_nullvalue = \"{}\" // fresh connection resets nullvalue")
	tp.dqsDDL = true
	tp.dqsDML = true
}

// processDBRestore handles db_restore: restore the snapshot files over
// test.db* without reopening (the connection stays open; SQLite's version
// restores the file under the open connection).
func (tp *transpiler) processDBRestore() {
	tp.emitLine("// db_restore: restore sv_test.db* snapshot")
	tp.emitLine("for _, _sf := range tclSplitList(tclGlob(\"test.db*\")) { os.Remove(_sf) }")
	tp.emitLine("for _, _sv := range tclSplitList(tclGlob(\"sv_test.db*\")) {")
	tp.emitLine("\ttclFileCopy(_sv, strings.TrimPrefix(_sv, \"sv_\"))")
	tp.emitLine("}")
}

// processDBDeleteAndReopen handles db_delete_and_reopen: delete all test.db*
// files and reopen the connection on test.db.
func (tp *transpiler) processDBDeleteAndReopen() {
	tp.emitLine("// db_delete_and_reopen: delete test.db* and reopen")
	tp.emitLine("db.Close()")
	tp.emitLine("for _, _sf := range tclSplitList(tclGlob(\"test.db*\")) { os.Remove(_sf) }")
	tp.emitLine("db, err = frigolite.Open(\"test.db\")")
	tp.emitLine("if err != nil { t.Fatal(err) }")
	tp.emitLine("tcl_nullvalue = \"{}\" // fresh connection resets nullvalue")
	tp.dqsDDL = true
	tp.dqsDML = true
}

// processNamespace handles `namespace eval ::NS { BODY }` — transpile the
// namespace body's top-level `variable NAME VALUE` declarations as Go
// assignments to the qualified variable (NS::NAME → NS_NAME), matching TCL
// namespace-variable semantics (main.test's testnamespace::xyz).
func (tp *transpiler) processNamespace(args []tcl.RawWord) {
	if len(args) < 3 || args[0].Text != "eval" {
		tp.emitLine("// namespace (unsupported form, not transpiled)")
		return
	}
	nsName := strings.TrimPrefix(strings.TrimSpace(args[1].Text), "::")
	body := args[2]
	if !body.Braced {
		tp.emitLine("// namespace eval %s (dynamic body, not transpiled)", nsName)
		return
	}
	parsed := parseCommands(body.Text)
	for _, cmd := range parsed {
		if len(cmd) < 2 || cmd[0].Text != "variable" {
			tp.emitLine("// namespace %s: %s (not transpiled)", nsName, sanitizeTCLComment(cmd[0].Text))
			continue
		}
		name := cmd[1].Text
		if strings.Contains(name, "::") {
			continue // already qualified — a reference, not a declaration
		}
		goName := tclVarToGo(nsName + "::" + name)
		if !isValidGoIdent(goName) {
			continue
		}
		if len(cmd) >= 3 {
			// `variable NAME VALUE` — declare and initialize.
			tp.emitLine("%s = %s", goName, tp.goStringLiteral(cmd[2]))
		} else {
			tp.emitLine("_ = %s // namespace %s variable %s", goName, nsName, name)
		}
	}
}

// processArray handles `array set NAME LIST` (P8.INCRVACUUM.phase9
// transpiler support). LIST is a TCL-format key-value list
// (e.g. `{207 1 412 1}` for `array set unusable_page {207 1 412 1}`).
// The transpiler emits `nameMap[k] = v` for each pair, matching
// the runtime's `unusable_pageMap` (a `map[string]string`
// declared at the top of the generated test).
//
// `array get` / `array unset` / `array exists` / `array names` /
// `array size` / `array startsearch` / `array nextelement` /
// `array anymore` / `array donesearch` are emitted as comments
// (the autovacuum/incrvacuum/... tests only use `array set`).
func (tp *transpiler) processArray(args []tcl.RawWord) {
	if len(args) < 2 {
		tp.emitLine("// array (too few args)")
		return
	}
	subCmd := args[0].Text
	switch subCmd {
	case "set":
		tp.emitArraySet(args)
	default:
		tp.emitLine("// array %s (not transpiled)", subCmd)
	}
}

// emitArraySet transpiles `array set NAME LIST`: registers the array in the
// map-var registries and emits a map assignment per key-value pair. Only the
// braced-literal list form is transpilable; runtime values need a map
// literal that the runtime helper can populate.
func (tp *transpiler) emitArraySet(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitLine("// array set (too few args)")
		return
	}
	name := args[1].Text
	if !isValidGoIdent(name) {
		tp.emitLine("// array set %s (non-identifier name, not transpiled)", name)
		return
	}
	// Register the array name in arrayMapVars so the
	// [info exists NAME($key)] dynamic-key handler in
	// processloop.go can emit a Go-side map lookup
	// (the registry path via TclVarExists only fires when
	// the literal-name form is used; the dynamic-key form
	// needs the Map to look up by runtime key).
	if tp.arrayMapVars == nil {
		tp.arrayMapVars = make(map[string]bool)
	}
	tp.arrayMapVars[name] = true
	globalArrayMapVars[name] = true
	// The list arg may be a braced body (e.g. `{207 1 412 1}`)
	// or a non-braced single token (e.g. `$var`). Only the
	// braced-literal form is transpilable; runtime values need
	// a map literal that the runtime helper can populate.
	list := args[2]
	if !list.Braced {
		tp.emitLine("// array set %s (dynamic list, not transpiled)", name)
		return
	}
	items := tclSplitList(list.Text)
	if len(items)%2 != 0 {
		tp.emitLine("// array set %s (odd list length, not transpiled)", name)
		return
	}
	tp.emitArraySetPairs(name, items)
}

// emitArraySetPairs emits one map-literal assignment plus tclvar-registry
// entry per key-value pair. The map literal is for the Go-side iteration;
// the registry is for the TCL-style existence check (the `info exists
// unusable_page($i)` runtime check, generated as
// `tclBool01(vtab.TclVarExists(...))`, must return true for these keys).
func (tp *transpiler) emitArraySetPairs(name string, items []string) {
	for i := 0; i < len(items); i += 2 {
		k := items[i]
		v := items[i+1]
		tp.emitLine("%sMap[%q] = %q", name, k, v)
		tp.emitLine("vtab.TclVarSet(%q, %q, %q)", name, k, v)
	}
}

// processIfcapable handles `ifcapable NAME { BODY }` / `ifcapable !NAME {...}`.
// The body runs at TCL time only when the target build LACKS the named
// capability for `!NAME` (or HAS it for `NAME`). The transpiler mirrors the
// runtime decision against frigolite's own capability table: a guard whose
// condition would be false under frigolite is dropped entirely; one that
// fires transpiles its body — typically `finish_test ; return`, aborting the
// generated test (loadext-style whole-file skip).
func (tp *transpiler) processIfcapable(args []tcl.RawWord) {
	if !ifcapableGuardFires(args[0].Text) {
		return
	}
	tp.runSubBody(args, 1)
}

// processIfnotcapable handles `ifnotcapable NAME { BODY }` (== ifcapable !NAME).
func (tp *transpiler) processIfnotcapable(args []tcl.RawWord) {
	if ifcapableGuardFires("!" + args[0].Text) {
		tp.runSubBody(args, 1)
	}
}

// processTime handles `time { SCRIPT } [count]` — transpile the inner script
// as regular code, ignoring the timing measurement.
func (tp *transpiler) processTime(args []tcl.RawWord) {
	tp.runSubBody(args, 0)
}

// processUnset handles `unset var` — in TCL an unset variable referenced via
// $var in a db eval binds as SQL NULL. Track it so $var renders as
// sqlLiteral(nil), and so a later `set var value` un-marks it.
func (tp *transpiler) processUnset(args []tcl.RawWord) {
	for _, a := range args {
		flag := strings.TrimSpace(a.Text)
		if flag == "-nocomplain" || flag == "--" {
			continue
		}
		if !isValidGoIdent(tclVarToGo(flag)) {
			continue
		}
		// Quota flag mirroring: `unset ::quota_request_ok` clears the
		// runtime registry entry so the quota callback's exists-check
		// returns false (quota.test 3.2.x).
		if tclVarToGo(flag) == "quota_request_ok" {
			tp.emitLine(`vtab.TclVarDelete("quota_request_ok", "")`)
		}
		if tp.unsetVars == nil {
			tp.unsetVars = make(map[string]bool)
		}
		tp.unsetVars[tclVarToGo(flag)] = true
	}
}

// processCount handles `count {SQL}` — execute SQL, return result + search
// count (always 0).
func (tp *transpiler) processCount(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	sqlExpr := tp.collectSQLExpression(args)
	tp.emitLine("_ = db.Exec(%s) // count (search count always 0)", sqlExpr)
}

// processCksort handles `cksort {SQL}` — execute SQL, sort info not available.
func (tp *transpiler) processCksort(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	sqlExpr := tp.collectSQLExpression(args)
	tp.emitLine("_ = db.Exec(%s) // cksort", sqlExpr)
}

// processInfraComment emits a comment for test-infrastructure procs that are
// not transpiled (queryplan, optimization, etc.).
func (tp *transpiler) processInfraComment(cmdName string, args []tcl.RawWord) {
	if len(args) > 0 {
		tp.emitLine("// %s %s (test infra, not transpiled)", cmdName, describeArgsShort(args))
	} else {
		tp.emitLine("// %s (test infra, not transpiled)", cmdName)
	}
}

// processQueryPlan handles like.test's queryplan proc: it executes the SQL
// (the engine's LIKE/GLOB call counter observes that execution - like-3.x's
// count assertions are only meaningful when the query actually runs) and
// derives plan info via EXPLAIN QUERY PLAN plus sqlite_sort_count. The
// plan/sorter introspection half is test infrastructure and stays dropped;
// the SQL side effect is emitted.
func (tp *transpiler) processQueryPlan(args []tcl.RawWord) {
	for _, w := range args {
		inner := bracedBody(w)
		if inner != "" {
			tp.emitLine("_ = db.Query(%s)", strconv.Quote(inner))
			return
		}
	}
	tp.processInfraComment("queryplan", args)
}

// bracedBody returns a braced RawWord's inner text (braces stripped, outer
// whitespace trimmed), or "" when the word is not a braced block. Handles
// both word representations: Text including the braces and the Braced flag
// with the braces already stripped.
func bracedBody(w tcl.RawWord) string {
	if w.Braced {
		return strings.TrimSpace(w.Text)
	}
	text := w.Text
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return ""
	}
	return strings.TrimSpace(text[1 : len(text)-1])
}

// processExecHex handles test1.c's sqlite3_exec_hex command: it decodes percent-H-H
// sequences to raw bytes, executes the SQL, and returns "<rc> <column names
// and row values>" (exec_printf_cb prepends the column names to the values).
// The statement-position form discards the result; the expression-position
// form ([sqlite3_exec_hex db SQL], like-9.3.1) captures it via cmdExpr.
func (tp *transpiler) processExecHex(args []tcl.RawWord) {
	if len(args) >= 2 {
		if inner := bracedBody(args[len(args)-1]); inner != "" {
			tp.emitLine("_ = tclExecHex(%s, %s)", tp.dbVar, strconv.Quote(inner))
			return
		}
	}
	tp.processInfraComment("sqlite3_exec_hex", args)
}

// processExprTest handles expression testing procs (test_expr, do_like_test,
// do_realnum_test, ...). These need table setup, so they emit a comment —
// EXCEPT do_realnum_test bodies that exercise prepared-statement binds or a
// db eval side-effect (CREATE TABLE setup), which must emit their SQL side
// effects so later tests see the inserted rows.
func (tp *transpiler) processExprTest(cmdName string, args []tcl.RawWord) {
	if cmdName == "do_realnum_test" && len(args) >= 2 {
		if tp.emitRealnumTestSideEffects(args) {
			return
		}
	}
	if len(args) > 0 {
		tp.emitLine("// %s %s (expr test, not transpiled)", cmdName, describeArgsShort(args))
	} else {
		tp.emitLine("// %s (expr test, not transpiled)", cmdName)
	}
}

// emitRealnumTestSideEffects emits the SQL side effects of a do_realnum_test
// body that exercises prepared-statement binds or a db eval side-effect
// (CREATE TABLE setup), so later tests see the inserted rows. Returns true
// when the body qualified and was emitted.
func (tp *transpiler) emitRealnumTestSideEffects(args []tcl.RawWord) bool {
	bodyCmds := tp.parseBracedBody(args, 1)
	if bodyCmds == nil || !(containsBindStep(bodyCmds) ||
		(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 3 &&
			bodyCmds[0][0].Text == "db" && bodyCmds[0][1].Text == "eval")) {
		return false
	}
	tp.emitLine("{ // %s (do_realnum_test; SQL side effects only)", tp.goStringLiteral(args[0]))
	tp.indent++
	tp.runSubBody(args, 1)
	tp.indent--
	tp.emitLine("}")
	return true
}

// processDropAllTables handles drop_all_tables: drop every user table in every
// database (main, temp, and attached) so later CREATE TABLE statements start
// fresh (matches the TCL helper, which turns foreign_keys OFF and iterates
// PRAGMA database_list).
func (tp *transpiler) processDropAllTables() {
	tp.emitLine("_res = db.Exec(\"PRAGMA foreign_keys = OFF\")")
	tp.emitLine("for _, _t := range db.Query(\"SELECT name, type FROM sqlite_master WHERE type IN('table','view')\").Rows {")
	tp.emitLine("\tdb.Exec(\"DROP \" + fmt.Sprint(_t[1]) + \" \" + tclQuoteIdent(fmt.Sprint(_t[0])))")
	tp.emitLine("}")
	tp.emitLine("for _, _t := range db.Query(\"SELECT name, type FROM temp.sqlite_master WHERE type IN('table','view')\").Rows {")
	tp.emitLine("\tdb.Exec(\"DROP \" + fmt.Sprint(_t[1]) + \" temp.\" + tclQuoteIdent(fmt.Sprint(_t[0])))")
	tp.emitLine("}")
	tp.emitLine("for _, _t := range db.Query(\"PRAGMA database_list\").Rows {")
	tp.emitLine("\tif len(_t) > 1 {")
	tp.emitLine("\t\tdbname := fmt.Sprint(_t[1])")
	tp.emitLine("\t\tif dbname != \"main\" && dbname != \"temp\" {")
	tp.emitLine("\t\t\tfor _, _u := range db.Query(\"SELECT name, type FROM \" + dbname + \".sqlite_master WHERE type IN('table','view')\").Rows {")
	tp.emitLine("\t\t\t\tdb.Exec(\"DROP \" + fmt.Sprint(_u[1]) + \" \" + dbname + \".\" + tclQuoteIdent(fmt.Sprint(_u[0])))")
	tp.emitLine("\t\t\t}")
	tp.emitLine("\t\t}")
	tp.emitLine("\t}")
	tp.emitLine("}")
	tp.emitLine("_res = db.Exec(\"PRAGMA foreign_keys = ON\")")
}
