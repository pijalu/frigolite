// Package main implements the tcl2go tool.
//
// This file contains the individual TCL command emitters dispatched from
// processCommand. Each handler has a single responsibility.
package main

import (
	"fmt"
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
		constFuncs:    tp.constFuncs,
		identityFuncs: tp.identityFuncs,
		predFuncs:     tp.predFuncs,
		queryFuncs:    tp.queryFuncs,
		specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
		autovacCallbacks: tp.autovacCallbacks,
		rangeListFuncs:      tp.rangeListFuncs,
		collateDtorVars:     tp.collateDtorVars,
		collateGoFuncs:      tp.collateGoFuncs,
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
		if mode == "2" {
			tp.emitLine("function.SetLocaltimeHook(tclTestLocaltime)")
		} else if mode == "0" {
			tp.emitLine("function.SetLocaltimeHook(nil)")
		} else {
			tp.emitLine("// sqlite3_test_control SQLITE_TESTCTRL_LOCALTIME_FAULT %s (unsupported mode)", mode)
		}
	}
}

// processSqlite3Limit handles sqlite3_limit; the expression/trigger depth,
// column-count, and length limits are transpiled as db.Set*Limit calls.
func (tp *transpiler) processSqlite3Limit(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	limitName := args[1].Text
	period := strings.TrimSpace(args[2].Text)
	varName := strings.TrimPrefix(period, "$")
	switch limitName {
	case "SQLITE_LIMIT_EXPR_DEPTH":
		if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && tp.isVarDeclared(varName)) {
			tp.emitLine("db.SetExprDepthLimit(toInt(%s))", replaceVarRefsRaw(period))
		}
	case "SQLITE_LIMIT_TRIGGER_DEPTH":
		tp.emitTriggerDepthLimit(args[2].Text)
	case "SQLITE_LIMIT_COLUMN", "SQLITE_LIMIT_LENGTH":
		// Set a numeric limit directly, or resolve [expr ...] constants at
		// transpile time (e.g. [expr $::SQLITE_MAX_COLUMN+1]).
		n := ""
		if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && (tp.isVarDeclared(varName) || knownGlobalVars()[varName])) {
			n = replaceVarRefsRaw(period)
		} else if strings.HasPrefix(period, "[expr ") {
			n = limitExprValue(strings.TrimSuffix(strings.TrimPrefix(period, "[expr "), "]"))
		}
		if n != "" {
			tp.emitLine("db.SetLimit(%q, toInt(%s))", limitName, n)
		}
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
	tp.emitLine("os.Remove(\"test.db\")")
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

// userProcHelperFor returns the generated-helper identifier that faithfully
// implements a known test-local proc (fingerprinted by distinctive body
// substrings), or "" when the body has no registered implementation.
func userProcHelperFor(name, body string) string {
	switch name {
	case "rand":
		switch {
		case strings.Contains(body, "1024.0"):
			return "tclRtree4RandFloat" // float variant: int((rand()-0.5)*1024.0*$X)/512.0
		case strings.Contains(body, "(rand()-0.5)*2*"):
			return "tclRtree4RandInt" // rtree_int_only: int((rand()-0.5)*2*$X)
		}
	case "randincr":
		switch {
		case strings.Contains(body, "32.0"):
			return "tclRtree4RandIncrFloat" // int(rand()*$X*32.0)/32.0 until >0
		case strings.Contains(body, "int(rand()*$X)+1"):
			return "tclRtree4RandIncrInt" // int(rand()*$X)+1 (always >0 for X>0)
		}
	case "scramble":
		if strings.Contains(body, "lsort") {
			return "tclUserScramble"
		}
	}
	return ""
}

// emitFsUserProcs registers vtabH.test's filesystem fixture procs
// (src/test_fs.c corpus support): sort_files/list_root_files/list_files/
// contents evaluate against the real filesystem at runtime through the
// harness helpers. Returns true when the proc was one of them and its
// runtime registration was emitted.
func (tp *transpiler) emitFsUserProcs(name, body string) bool {
	switch name {
	case "sort_files":
		if !strings.Contains(body, "lsort") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { nc := \"\"; if len(a) > 1 { nc = a[1] }; return tclSortFiles(a[0], nc) })", name)
		return true
	case "list_root_files":
		if !strings.Contains(body, "-nocomplain") && !strings.Contains(body, "glob") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { return tclListRootFiles() })", name)
		return true
	case "list_files":
		if !strings.Contains(body, "-nocomplain") && !strings.Contains(body, "glob") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return tclListFiles(a[0]) })", name)
		return true
	case "contents":
		if !strings.Contains(body, "list_files") {
			return false
		}
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return tclContents(a[0]) })", name)
		return true
	}
	return false
}

// processProc recognizes simple test-harness procs (constant-returning,
// counter, predicate, join, collation) and registers them for later `db func`
// / `db collate` handlers.
func (tp *transpiler) processProc(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitLine("// proc definition (not transpiled)")
		return
	}
	name := strings.TrimSpace(args[0].Text)
	body := strings.TrimSpace(args[2].Text)
	body = strings.TrimPrefix(body, "{")
	body = strings.TrimSuffix(body, "}")
	body = strings.TrimSpace(body)

	// A proc of the shape used by the `tcl` vtab module
	// (vtabL.test: proc vtab_command {method args} { ... return $::var })
	// acts as an alias returning a TCL global. Record it so every
	// registration site for that global also registers the proc name —
	// the module resolves its schema argument through the same registry.
	if tgt := procReturnGlobalAlias(body); tgt != "" {
		markTclProcAlias(name, tgt)
	}

	// Track proc definitions so a redefinition with a DIFFERENT body emits a
	// fresh SQL-function registration (TCL's `proc` redefines the proc the
	// db-func binding points to; fts4intck1.test redefines slang from the
	// th→d/e→eh map to identity at 2.3).
	if prev, ok := tp.seenProcs[name]; ok && prev != body {
		if tp.emitProcBodyRegistration(name, body) {
			tp.seenProcs[name] = body
			return
		}
	}
	if tp.seenProcs == nil {
		tp.seenProcs = make(map[string]string)
	}
	tp.seenProcs[name] = body
	if tp.emitFsUserProcs(name, body) {
		return
	}
	// Procs with a known faithful Go implementation (rtree4.test's
	// rand/randincr/scramble and variants) register into the generated
	// test's runtime proc registry so every later [name arg...] bracket —
	// plain set RHS, nested expr, interpolated SQL — evaluates through it.
	if fnIdent := userProcHelperFor(name, body); fnIdent != "" {
		markUserProcGlobal(name)
		tp.emitLine("registerTclUserProc(%q, func(a []string) string { if len(a) == 0 { return \"\" }; return %s(a[0]) })", name, fnIdent)
	}
	// Procs transpilable INLINE at call sites: either zero parameters ("{}")
	// or a single parameter with a default value ("{{module rtree}}" — TCL's
	// optional-argument form). At a zero-arg call the default binds and the
	// body sequences supported commands (setup_simple_db et al).
	paramsInner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimSpace(args[1].Text), "{"), "}"))
	if inlineableParams(paramsInner) {
		if tp.inlineProcs == nil {
			tp.inlineProcs = make(map[string]string)
			tp.inlineProcParams = make(map[string]string)
		}
		tp.inlineProcs[name] = body
		tp.inlineProcParams[name] = args[1].Text
	}
	// Keep every proc body available for later registration sites
	// (e.g. `db preupdate hook preup` transpiling the named proc).
	if tp.procBodies == nil {
		tp.procBodies = make(map[string]string)
	}
	globalProcBodies[name] = body
	if prev, ok := tp.procBodies[name]; !ok || prev != body {
		tp.procBodies[name] = body
	}

	// `proc preupdate_hook {args} { ... }` (hook2.test) — the connection's
	// preupdate-hook callback. Its body is emitted as a Go closure when the
	// test registers it via `db preupdate hook preupdate_hook`.
	if name == "preupdate_hook" {
		tp.preupdateHookBody = body
		return
	}

	// Hook callback procs (hook.test): commit_hook, rollback_hook, and the
	// update/preupdate callback procs. Their bodies are emitted as Go
	// closures when the test registers them via db commit_hook / db
	// rollback_hook / db update_hook / db preupdate hook.
	switch name {
	case "commit_hook", "rollback_hook", "update_cb", "preupdate_cb", "commit_hook_cb", "rollback_cb":
		if tp.commitHookBodies == nil {
			tp.commitHookBodies = make(map[string]string)
		}
		tp.commitHookBodies[name] = body
		return
	}

	// `proc int2str {i} { string range [string repeat "$i." 450] 0 899 }`
	// — the test-harness int2str builds a 900-char string. Register a
	// deterministic Go function for it (the transpiler emits a stub
	// otherwise, breaking comparisons with int2str(...) results).
	tp.registerInt2str(name, body)

	// `proc zip {x} { incr ::next_x; set ::strings($::next_x) $x; return
	// $::next_x }` and `proc unzip {x} { return $::strings($x) }` — the
	// fts3comp1 compression harness. Register them so `db func $zip zip`
	// emits a stateful Go function (counter + map) instead of a no-op stub.
	if tp.registerZipUnzipProcs(name, body) {
		return
	}

	// `proc autovac_page_callback {schema filesize freesize pagesize} { ... }`
	// (autovacuum2.test) — the sqlite3_autovacuum_pages callback. Emits a Go
	// closure that the testgen passes to db.SetAutovacuumPagesCallback so
	// the COMMIT-time autovacuum hook fires the callback and appends its
	// args to ::autovac_callback_data.
	if tp.registerAutovacPageCallbackProc(name, body) {
		return
	}

	// `proc blob {a} { binary decode hex $a }` (fts3corrupt4) — the
	// corruption tests build modified root blobs through this hex decoder.
	if tp.registerBlobProc(name, body) {
		return
	}

	// `proc tx x {return [string map [list ( \173 ) \175 ' \042 < \133 > \135] $x]}`
	// (json101) — a single-argument character-mapping proc. Register the
	// pairs so a later [tx $var] command substitution emits a Go
	// strings.NewReplacer expression.
	if tp.registerStringMapProc(name, args, body) {
		return
	}

	// `proc make_record_wrapper {args} { make_fts3record $args }`
	// (fts4record.test) — the test-harness record builder registered as
	// `db func record make_record_wrapper`.
	if tp.registerFts3RecordProc(name, body) {
		return
	}

	// `proc mit {blob} { ... binary scan ... }` (fts3matchinfo) — the
	// matchinfo blob decoder.
	if tp.registerMatchinfoProc(name, body) {
		return
	}

	// `proc utf8_to_hstr {in} { ... }` — badutf2's hex→%XX converter.
	tp.registerUtf8ToHstr(name, body)

	// `proc auth {code arg1 arg2 arg3 arg4 args} { ... }` — a TCL authorizer
	// proc registered via `db authorizer ::auth`. Transpile the body into a
	// Go type implementing auth.Authorizer.
	if tp.processAuthorizerProc(args) {
		return
	}

	if tp.registerProcKinds(name, body) {
		return
	}
	// `proc create_db {{sql ""}} { ... }` (e_vacuum.test) creates test.db
	// with page_size 1024, auto_vacuum settings, and the t1/t2 tables used by
	// the vacuum tests. The file-size return value is VACUUM-dependent and
	// cannot be reproduced; emit the CREATE/INSERT setup so later tests see
	// t1/t2 (the file-size assertions are skipped as VACUUM).
	if name == "create_db" && strings.Contains(body, "CREATE TABLE t1") {
		if tp.constFuncs == nil {
			tp.constFuncs = make(map[string]string)
		}
		if tp.specialFuncs == nil {
			tp.specialFuncs = make(map[string]string)
		}
		tp.specialFuncs[name] = "tclCreateDB"
		return
	}
	tp.emitLine("// proc definition (not transpiled)")
}

// registerProcKinds tries each simple proc kind (constant, counter, predicate,
// join, collation) in order and registers the first match. Returns true when a
// kind was registered (the caller stops processing the proc).
func (tp *transpiler) registerProcKinds(name, body string) bool {
	if constVal := constantProcValue(body); constVal != "" {
		return tp.registerConstProc(name, constVal)
	}
	if varName := counterProcValue(body); varName != "" {
		return tp.registerCounterProc(name, varName)
	}
	if pred := predicateProcValue(body); pred != "" {
		return tp.registerPredProc(name, pred)
	}
	if sep := joinProcValue(body); sep != "" {
		return tp.registerJoinProc(name, sep)
	}
	if prefix := prefixProcValue(body); prefix != "" {
		return tp.registerPrefixProc(name, prefix)
	}
	if goFn := collationProcGo(body); goFn != "" {
		return tp.registerCollateProc(name, goFn)
	}
	return false
}

// emitProcBodyRegistration emits a fresh RegisterFunction for a proc body
// that was REdefined with a different body. TCL's `proc NAME {x} {BODY}`
// replaces the proc the db-func binding points to, so SQL calls see the new
// body (fts4intck1.test redefines slang from a string-map to identity).
// Returns true when the body matched a known proc kind and was emitted.
func (tp *transpiler) emitProcBodyRegistration(name, body string) bool {
	if name == "" {
		return false
	}
	if pairs := stringMapProcValue(body); pairs != "" {
		items := tclCmdWords(pairs)
		if len(items) < 2 || len(items)%2 != 0 {
			return false
		}
		tp.emitLine("// proc %s redefined (string map) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
		tp.emitLine("\ts := tclStr(args[0])")
		for i := 0; i+1 < len(items); i += 2 {
			tp.emitLine("\ts = strings.ReplaceAll(s, %q, %q)", items[i], items[i+1])
		}
		tp.emitLine("\treturn s, nil")
		tp.emitLine("}, 0, -1)")
		return true
	}
	if identityProcValue(body) {
		tp.emitLine("// proc %s redefined (identity) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
		tp.emitLine("\treturn args[0], nil")
		tp.emitLine("}, 0, -1)")
		return true
	}
	if constVal := constantProcValue(body); constVal != "" {
		tp.emitLine("// proc %s redefined (constant) — re-register", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return int64(%s), nil }, 0, -1)", tp.dbVar, name, constVal)
		return true
	}
	return false
}

// registerPrefixProc registers a prefix proc (`proc NAME {args} { return
// "P: $args" }`) so later `db func NAME NAME` emits a scalar SQL function
// that joins its args with a space and prepends the fixed prefix.
func (tp *transpiler) registerPrefixProc(name, prefix string) bool {
	if name == "" {
		return false
	}
	if tp.prefixFuncs == nil {
		tp.prefixFuncs = make(map[string]string)
	}
	tp.prefixFuncs[name] = prefix
	tp.emitLine("// proc %s prepends %q to its args (registered via db func)", name, prefix)
	return true
}

// registerInt2str recognizes the test-harness int2str proc and registers it as
// a special function mapped to the tclInt2str Go helper.
func (tp *transpiler) registerInt2str(name, body string) {
	if name != "int2str" {
		return
	}
	if !strings.Contains(body, "string repeat") || !strings.Contains(body, "450") || !strings.Contains(body, "899") {
		return
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclInt2str"
}

// registerUtf8ToHstr recognizes the test-harness utf8_to_hstr proc (badutf2):
// `proc utf8_to_hstr {in} { regsub -all -- {(..)} $in {%[format "%s" \1]} out;
// subst $out }` — converts a hex string like "C3BF" to "%C3%BF". The body is
// a regsub+subst TCL idiom the transpiler cannot inline, so register a Go
// helper.
func (tp *transpiler) registerUtf8ToHstr(name, body string) {
	if name != "utf8_to_hstr" {
		return
	}
	if !strings.Contains(body, "regsub") || !strings.Contains(body, "subst") {
		return
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclUtf8ToHstr($data)"
}

// registerBlobProc recognizes the test-harness blob proc (`proc blob {a} {
// binary decode hex $a }` — fts3corrupt4). It registers the name in
// specialFuncs so `db func blob blob` emits a Go hex-decoder function.
func (tp *transpiler) registerBlobProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "binary decode hex") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclBlobHexDecode"
	return true
}

// registerStringMapProc recognizes a single-argument character-mapping proc:
// `proc NAME x {return [string map [list K1 V1 K2 V2 ...] $x]}` (json101's
// `proc tx x`, which converts '(' ')' '\” '<' '>' shorthand into JSON
// braces/quotes/brackets). The old/new pairs are stored flat so a later
// [NAME $var] substitution emits a Go strings.NewReplacer chain.
func (tp *transpiler) registerStringMapProc(name string, args []tcl.RawWord, body string) bool {
	if name == "" || len(args) < 2 {
		return false
	}
	param := strings.TrimSpace(args[1].Text)
	if param == "" || strings.ContainsAny(param, " \t") {
		return false // exactly one parameter
	}
	const prefix = "return [string map [list "
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.HasPrefix(compact, prefix) {
		return false
	}
	tail := strings.TrimSpace(compact[len(prefix):])
	closer := "] $" + param + "]"
	if !strings.HasSuffix(tail, closer) {
		return false
	}
	pairsText := strings.TrimSpace(tail[:len(tail)-len(closer)])
	words := tclCmdWords(pairsText)
	if len(words) == 0 || len(words)%2 != 0 {
		return false
	}
	// Resolve TCL backslash escapes (octal \173 → '{', \042 → '"') so the
	// Replacer carries the real characters.
	pairs := make([]string, len(words))
	for i, w := range words {
		pairs[i] = unescapeBareWord(w)
	}
	if tp.procStringMaps == nil {
		tp.procStringMaps = make(map[string][]string)
	}
	tp.procStringMaps[name] = pairs
	tp.emitLine("// proc %s: string map %q", name, pairs)
	return true
}

// registerFts3RecordProc recognizes the fts4record wrapper proc
// (`proc make_record_wrapper {args} { make_fts3record $args }`) — the
// test-harness record builder registered as `db func record
// make_record_wrapper`. The SQL function builds an FTS3 segment record blob
// (varint-encoded integers + raw string bytes, src/test_hexio.c
// make_fts3record).
func (tp *transpiler) registerFts3RecordProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "make_fts3record") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclFts3Record"
	return true
}

// registerMatchinfoProc recognizes the FTS matchinfo test-harness proc
// (`proc mit {blob} { set scan(littleEndian) i*; set scan(bigEndian) I*;
// binary scan $blob $scan($::tcl_platform(byteOrder)) r; return $r }` —
// fts3matchinfo/fts3matchinfo2). It decodes the matchinfo blob as a list of
// little-endian 32-bit integers. It registers the name in specialFuncs so
// `db func mit mit` emits a Go blob→int-list decoder instead of a no-op stub.
func (tp *transpiler) registerMatchinfoProc(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(compact, "binary scan") || !strings.Contains(compact, "littleEndian") {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	tp.specialFuncs[name] = "tclMatchinfoDecode"
	return true
}

// registerZipUnzipProcs recognizes the fts3comp1 compression-harness procs
// (`proc zip {x} { incr ::next_x; set ::strings($::next_x) $x; return
// $::next_x }` and `proc unzip {x} { return $::strings($x) }`). They are
// registered in specialFuncs so `db func $zip zip` emits a stateful Go
// closure (a counter + a map) instead of a no-op stub — the FTS4
// compress/uncompress functions must return integer keys the content table
// stores and the test compares.
func (tp *transpiler) registerZipUnzipProcs(name, body string) bool {
	if name == "" {
		return false
	}
	compact := strings.Join(strings.Fields(body), " ")
	isZip := strings.Contains(compact, "incr ::next_x") &&
		strings.Contains(compact, "set ::strings(") &&
		strings.Contains(compact, "return $::next_x")
	isUnzip := strings.Contains(compact, "return $::strings(")
	if !isZip && !isUnzip {
		return false
	}
	if tp.specialFuncs == nil {
		tp.specialFuncs = make(map[string]string)
	}
	if isZip {
		tp.specialFuncs[name] = "tclZipFn"
	} else {
		tp.specialFuncs[name] = "tclUnzipFn"
	}
	return true
}

// registerAutovacPageCallbackProc recognizes the autovacuum2.test
// `autovac_page_callback` and `autovac_page_callback_off` procs
// (sqlite3_autovacuum_pages). The transpiler emits a Go closure that
// the testgen can pass to db.SetAutovacuumPagesCallback, mirroring the
// TCL semantics:
//   - autovac_page_callback: appends (schema, filesize, freesize, pagesize)
//     to ::autovac_callback_data and returns freesize/2 (per-batch limit).
//   - autovac_page_callback_off: returns 0 (no vacuum this batch).
//
// The Go function name follows the proc name verbatim (camelCase). Stored
// on the transpiler's autovacCallbacks map so the sqlite3_autovacuum_pages
// dispatcher can emit the SetAutovacuumPagesCallback call.
func (tp *transpiler) registerAutovacPageCallbackProc(name, body string) bool {
	if name != "autovac_page_callback" && name != "autovac_page_callback_off" {
		return false
	}
	if tp.autovacCallbacks == nil {
		tp.autovacCallbacks = make(map[string]string)
	}
	if _, exists := tp.autovacCallbacks[name]; exists {
		// Already emitted for this proc (redefinition is a no-op).
		return true
	}
	tp.autovacCallbacks[name] = body
	// Emit a Go variable with the proc name in camelCase, holding a
	// closure that matches the TCL semantics.
	goName := tclVarToGo(name) // autovac_page_callback -> autovacPageCallback
	if name == "autovac_page_callback" {
		// TCL body: global autovac_callback_data; lappend it
		// $schema $filesize $freesize $pagesize; return [expr {$freesize/2}]
		tp.emitLine("// proc %s {schema filesize freesize pagesize}: appends callback args to", name)
		tp.emitLine("// autovac_callback_data and returns freesize/2 (per-batch vacuum limit).")
		tp.emitLine("var %s = func(schema string, fileSize, nFree, pageSize uint32) uint32 {", goName)
		tp.emitLine("\tautovac_callback_data = tclListAppend(autovac_callback_data, schema,")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(fileSize), 10),")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(nFree), 10),")
		tp.emitLine("\t\tstrconv.FormatUint(uint64(pageSize), 10))")
		tp.emitLine("\treturn nFree / 2")
		tp.emitLine("}")
	} else {
		// autovac_page_callback_off: returns 0 (no vacuum).
		tp.emitLine("// proc %s {schema filesize freesize pagesize}: returns 0 (no vacuum).", name)
		tp.emitLine("var %s = func(schema string, fileSize, nFree, pageSize uint32) uint32 {", goName)
		tp.emitLine("\treturn 0")
		tp.emitLine("}")
	}
	return true
}

// registerConstProc registers a constant-returning proc. Returns false when
// the name is empty (caller falls through to the other proc kinds).
func (tp *transpiler) registerConstProc(name, constVal string) bool {
	if name == "" {
		return false
	}
	if tp.constFuncs == nil {
		tp.constFuncs = make(map[string]string)
	}
	tp.constFuncs[name] = constVal
	tp.emitLine("// proc %s returns constant %s (registered via db func)", name, constVal)
	return true
}

// registerCounterProc registers a counter proc (`proc NAME {} { incr ::VAR }`).
func (tp *transpiler) registerCounterProc(name, varName string) bool {
	if name == "" {
		return false
	}
	if tp.counterFuncs == nil {
		tp.counterFuncs = make(map[string]string)
	}
	tp.counterFuncs[name] = varName
	tp.emitLine("// proc %s increments counter var %s (registered via db func)", name, varName)
	return true
}

// registerPredProc registers a predicate proc (`proc NAME {x} { expr $x < N }`).
func (tp *transpiler) registerPredProc(name, pred string) bool {
	if name == "" {
		return false
	}
	if tp.predFuncs == nil {
		tp.predFuncs = make(map[string]string)
	}
	tp.predFuncs[name] = pred
	tp.emitLine("// proc %s predicate %s (registered via db func)", name, pred)
	return true
}

// registerJoinProc registers a join proc (`proc NAME {args} { return [join
// $args -] }`).
func (tp *transpiler) registerJoinProc(name, sep string) bool {
	if name == "" {
		return false
	}
	if tp.joinFuncs == nil {
		tp.joinFuncs = make(map[string]string)
	}
	tp.joinFuncs[name] = sep
	tp.emitLine("// proc %s joins args with %q (registered via db func)", name, sep)
	return true
}

// registerCollateProc registers a collation proc (`proc NAME {a b} { ... }`).
func (tp *transpiler) registerCollateProc(name, goFn string) bool {
	if name == "" {
		return false
	}
	if tp.collateGoFuncs == nil {
		tp.collateGoFuncs = make(map[string]string)
	}
	tp.collateGoFuncs[name] = goFn
	tp.emitLine("// proc %s collation (registered via db collate)", name)
	return true
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

// processExprTest handles expression testing procs (test_expr, do_like_test,
// do_realnum_test, ...). These need table setup, so they emit a comment —
// EXCEPT do_realnum_test bodies that exercise prepared-statement binds or a
// db eval side-effect (CREATE TABLE setup), which must emit their SQL side
// effects so later tests see the inserted rows.
func (tp *transpiler) processExprTest(cmdName string, args []tcl.RawWord) {
	if cmdName == "do_realnum_test" && len(args) >= 2 {
		bodyCmds := tp.parseBracedBody(args, 1)
		if bodyCmds != nil && (containsBindStep(bodyCmds) ||
			(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 3 &&
				bodyCmds[0][0].Text == "db" && bodyCmds[0][1].Text == "eval")) {
			tp.emitLine("{ // %s (do_realnum_test; SQL side effects only)", tp.goStringLiteral(args[0]))
			tp.indent++
			tp.runSubBody(args, 1)
			tp.indent--
			tp.emitLine("}")
			return
		}
	}
	if len(args) > 0 {
		tp.emitLine("// %s %s (expr test, not transpiled)", cmdName, describeArgsShort(args))
	} else {
		tp.emitLine("// %s (expr test, not transpiled)", cmdName)
	}
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

// inlineableParams reports whether a proc parameter word makes the proc
// inlineable: zero parameters ("" after brace stripping) or exactly one
// parameter with a default value ("module rtree" — TCL's {{name default}}
// optional-argument form). Multi-parameter procs are NOT inlineable.
func inlineableParams(paramsInner string) bool {
	if paramsInner == "" {
		return true
	}
	fields := strings.Fields(paramsInner)
	return len(fields) == 2 && !strings.ContainsAny(paramsInner, "\"{}[]$") &&
		isValidGoIdent(tclVarToGo(fields[0]))
}

// inlineProcDefaults returns the Go assignment for an inline proc's optional
// parameter bound to its default value ("module := \"rtree\"").
func inlineProcDefaultAssign(paramsInner string) string {
	fields := strings.Fields(paramsInner)
	if len(fields) != 2 {
		return ""
	}
	name := tclVarToGo(fields[0])
	def := strings.TrimSpace(fields[1])
	def = strings.TrimSuffix(strings.TrimPrefix(def, "{"), "}")
	def = strings.Trim(def, "'\"")
	return fmt.Sprintf("%s = %q", name, def)
}
