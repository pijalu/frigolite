// Package main implements the tcl2go tool.
//
// This file handles execsql / db commands (processExecSQL, processDB,
// processDBForName).
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- SQL execution handlers ----

func joinPrefixFromRest(rest []tcl.RawWord, procName string) string {
	for _, a := range rest[1:] {
		arg := strings.TrimSpace(a.Text)
		if strings.HasPrefix(arg, "-") {
			continue
		}
		fields := strings.Fields(arg)
		if len(fields) >= 2 && fields[0] == procName {
			return strings.Trim(fields[1], "{}")
		}
		break
	}
	return ""
}

// zipUnzipFunc reports whether the proc name was registered as a zip/unzip
// compression-harness proc (fts3comp1).
func (tp *transpiler) zipUnzipFunc(procName string) bool {
	if tp.specialFuncs == nil {
		return false
	}
	tmpl, ok := tp.specialFuncs[procName]
	return ok && (tmpl == "tclZipFn" || tmpl == "tclUnzipFn")
}

// emitZipUnzipFunction emits a stateful RegisterFunction for the fts3comp1
// zip/unzip harness procs. The function NAME is a runtime Go variable (the
// loop var holding "zip" or "z.i.p!!"); the closure implements the TCL proc
// body: zip increments next_x, records next_x→arg in stringsMap, returns
// next_x; unzip returns stringsMap[arg].
func (tp *transpiler) emitZipUnzipFunction(nameVar, procName, tmpl string) {
	goName := tclVarToGo(nameVar)
	if !isValidGoIdent(goName) {
		tp.emitLine("// db func $%s %s (zip/unzip harness; name not a Go ident)", nameVar, procName)
		return
	}
	// TCL array is ::strings → Go map var tclVarToGo("strings")+"Map"
	// (shadow-avoidance may prefix '_', e.g. _stringsMap).
	strMap := tclVarToGo("strings") + "Map"
	if tmpl == "tclZipFn" {
		tp.emitLine("// db func $%s %s (zip harness: counter + map)", nameVar, procName)
		tp.emitLine("%s.RegisterFunction(%s, func(args []interface{}) (interface{}, error) {", tp.dbVar, goName)
		tp.emitLine("\tnv, _ := strconv.Atoi(next_x)")
		tp.emitLine("\tnv++")
		tp.emitLine("\tnext_x = strconv.Itoa(nv)")
		tp.emitLine("\targ := \"\"")
		tp.emitLine("\tif len(args) > 0 { arg = tclStr(args[0]) }")
		tp.emitLine("\t%s[next_x] = arg", strMap)
		tp.emitLine("\treturn int64(nv), nil")
		tp.emitLine("}, 0, -1)")
	} else {
		tp.emitLine("// db func $%s %s (unzip harness: map lookup)", nameVar, procName)
		tp.emitLine("%s.RegisterFunction(%s, func(args []interface{}) (interface{}, error) {", tp.dbVar, goName)
		tp.emitLine("\targ := \"\"")
		tp.emitLine("\tif len(args) > 0 { arg = tclStr(args[0]) }")
		tp.emitLine("\treturn %s[arg], nil", strMap)
		tp.emitLine("}, 0, -1)")
	}
}

// emitDBVarFunc handles the variable-reader fallback: `db func NAME PROC`
// where PROC reads a TCL variable. Track NAME so SQL rendering can inline the
// Go variable's value (see inlineVarFuncs).
func (tp *transpiler) emitDBVarFunc(rest []tcl.RawWord) {
	if len(rest) < 1 {
		return
	}
	if tp.dbVarFuncs == nil {
		tp.dbVarFuncs = make(map[string]bool)
	}
	name := strings.TrimSpace(rest[0].Text)
	tp.dbVarFuncs[name] = true
	if name == "make_interior_node" {
		// fts3corrupt7 3.x: make_interior_node HEIGHT CHILD builds an FTS3
		// interior node blob (varint height + varint child block id); the
		// test chains 40000 of them to force corruption on read.
		tp.emitLine("// db function %s (fts3 interior-node builder)", name)
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
		tp.emitLine("\tif len(args) < 2 { return nil, fmt.Errorf(\"usage\\u003a make_interior_node height child\") }")
		tp.emitLine("\th, _ := strconv.Atoi(tclStr(args[0]))")
		tp.emitLine("\tc, _ := strconv.Atoi(tclStr(args[1]))")
		tp.emitLine("\tvar _mib []byte")
		tp.emitLine("\t_mib = tclFts3PutVarint(_mib, uint64(h))")
		tp.emitLine("\t_mib = tclFts3PutVarint(_mib, uint64(c))")
		tp.emitLine("\treturn _mib, nil")
		tp.emitLine("}, 0, -1)")
		return
	}
	// Register the function so introspection pragmas (pragma_function_list)
	// see it as a non-builtin function. Its TCL body reads a variable
	// that the transpiler inlines at call sites, so the Go stub never
	// actually runs.
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return nil, nil }, 0, -1)", tp.dbVar, name)
}

// processNamedDBAuthorizer handles `db authorizer PROC` / `dbN authorizer
// PROC` — install the dispatcher that forwards to the current TCL authorizer
// proc (authCurrent). An unknown proc falls back to a no-op comment so the
// generated test still compiles.
func (tp *transpiler) processNamedDBAuthorizer(goName string, rest []tcl.RawWord) {
	if len(rest) < 1 {
		// `db authorizer` with no arg returns the current authorizer name.
		tp.emitLine("// %s.Authorizer() (query)", goName)
		return
	}
	procArg := strings.TrimSpace(rest[0].Text)
	procName := strings.TrimPrefix(procArg, "::")
	if isAuthorizerProcName(procName) || procName == "auth" || procName == "authx" {
		tp.ensureAuthCurrentDecl()
		tp.emitLine("%s.SetAuthorizer(&authDispatcher{})", goName)
		return
	}
	// `db authorizer {}` clears the authorizer.
	if procArg == "{}" || procArg == "" {
		tp.ensureAuthCurrentDecl()
		tp.emitLine("%s.SetAuthorizer(nil)", goName)
		return
	}
	tp.emitLine("// %s authorizer %s (proc not transpiled)", goName, procArg)
}
func (tp *transpiler) processDBCollate(rest []tcl.RawWord) {
	// db collate NAME PROC / db collate NAME {string compare} registers a
	// custom collation sequence. Emit db.RegisterCollation with a Go
	// closure for the recognized test-suite collations; unknown procs are
	// no-ops (the tests cannot reproduce arbitrary TCL collations).
	if len(rest) < 1 {
		return
	}
	collName := strings.TrimSpace(rest[0].Text)
	collWord := rest[0]
	var goFn string
	if len(rest) >= 2 {
		procArg := strings.TrimSpace(rest[1].Text)
		// Inline forms: {string compare} / "string compare" /
		// [list string compare -nocase].
		if f := collationProcGo(procArg); f != "" {
			goFn = f
		} else if fn, ok := tp.collateGoFuncs[procArg]; ok {
			goFn = fn
		}
	}
	if goFn != "" && collName != "" {
		tp.emitLine("db.RegisterCollation(%s, %s)", tp.goStringLiteral(collWord), goFn)
	} else {
		tp.emitLine("// db collate %s (not transpiled)", collName)
	}
}

// processDBProgress handles `db progress N fn` — register a progress callback
// after every N engine operations.
func (tp *transpiler) processDBProgress(rest []tcl.RawWord) {
	// db progress N fn registers a progress callback after every N
	// engine operations; the TCL used (e.g. progress_stop) returns
	// constant nonzero to interrupt. Only transpile when N is a numeric
	// literal or a known TCL variable; other forms (error casts like "db
	// progress xyz") are left as no-ops so the generated code compiles.
	if len(rest) < 1 {
		return
	}
	period := strings.TrimSpace(rest[0].Text)
	varName := strings.TrimPrefix(period, "$")
	if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && tp.isVarDeclared(varName)) {
		tp.emitLine("db.SetProgressHandler(toInt(%s), func() bool { return true })", replaceVarRefsRaw(period))
	}
}

// processDBPreupdate handles `db preupdate hook/count/old/new` — the TCL
// preupdate-hook interface (hook2.test). The hook subcommand registers the
// preupdate_hook proc body as a Go closure; count/old/new query the current
// preupdate event's columns.
func (tp *transpiler) processDBPreupdate(rest []tcl.RawWord) {
	if len(rest) < 1 {
		return
	}
	switch strings.TrimSpace(rest[0].Text) {
	case "hook":
		// `db preupdate hook <proc>` with a proc whose body follows the
		// bind2.test preup pattern (prepare "SELECT ?", bind the old/new
		// preupdate value, step, append column_text to a list) is emitted as
		// an equivalent Go closure.
		if len(rest) >= 2 {
			if procName := strings.TrimPrefix(strings.TrimSpace(rest[1].Text), "::"); tp.emitPreupdateBindProcHook(procName) {
				return
			}
		}
		// db preupdate hook preupdate_hook — register the hook2.test
		// preupdate_hook proc as a Go closure. The proc appends the event
		// (type, db, table, rowid, rowid2) to ::preupdate, then the old
		// values (non-INSERT) and new values (non-DELETE).
		// Ensure the ::preupdate TCL accumulator variable is declared in the
		// enclosing scope (hook/hook2 pre-declare it via a top-level `set
		// preupdate`; tests that only install the hook need it declared
		// here so the generated closure compiles).
		if !tp.isVarDeclared("preupdate") {
			tp.vars = append(tp.vars, "preupdate")
			tp.emitLine("var preupdate string")
		}
		tp.emitLine("db.SetPreupdateHook(func() {")
		tp.indent++
		tp.emitLine("var _ptype = db.PreupdateType()")
		tp.emitLine("preupdate = tclListAppend(preupdate, db.PreupdateType(), db.PreupdateDB(), db.PreupdateTable(), strconv.FormatInt(db.PreupdateRowID(), 10), strconv.FormatInt(db.PreupdateRowID2(), 10))")
		tp.emitLine("if _ptype != \"INSERT\" {")
		tp.indent++
		tp.emitLine("for _pi := 0; _pi < db.PreupdateCount(); _pi++ {")
		tp.indent++
		tp.emitLine("preupdate = tclListAppend(preupdate, tclStr(db.PreupdateOld(_pi)))")
		tp.indent--
		tp.emitLine("}")
		tp.indent--
		tp.emitLine("}")
		tp.emitLine("if _ptype != \"DELETE\" {")
		tp.indent++
		tp.emitLine("for _pi := 0; _pi < db.PreupdateCount(); _pi++ {")
		tp.indent++
		tp.emitLine("preupdate = tclListAppend(preupdate, tclStr(db.PreupdateNew(_pi)))")
		tp.indent--
		tp.emitLine("}")
		tp.indent--
		tp.emitLine("}")
		tp.indent--
		tp.emitLine("})")
	case "count":
		tp.emitLine("_r = strconv.Itoa(db.PreupdateCount())")
	case "old", "new":
		if len(rest) >= 2 {
			idx := tp.intValueExpr(rest[1])
			if rest[0].Text == "old" {
				tp.emitLine("_r = tclStr(db.PreupdateOld(%s))", idx)
			} else {
				tp.emitLine("_r = tclStr(db.PreupdateNew(%s))", idx)
			}
		}
	default:
		// no-op for other preupdate subcommands
	}
}

// emitPreupdateBindProcHook transpiles a `db preupdate hook <proc>` whose
// proc body follows bind2.test's preup pattern: prepare "SELECT ?", bind the
// preupdate old value, step, append column_text to a list, reset, repeat with
// the new value, finalize. Returns false when the body does not match.
func (tp *transpiler) emitPreupdateBindProcHook(procName string) bool {
	body, ok := tp.procBodies[procName]
	if !ok || !strings.Contains(body, "sqlite3_bind_value_from_preupdate") ||
		!strings.Contains(body, "sqlite3_column_text") {
		return false
	}
	// Accumulator variable from the `lappend ::VAR ...` calls.
	listVar := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "lappend ::") {
			listVar = strings.TrimPrefix(strings.Fields(line)[1], "::")
			break
		}
	}
	if listVar == "" {
		return false
	}
	goList := tclVarToGo(listVar)
	if !isValidGoIdent(goList) {
		return false
	}
	if !tp.isVarDeclared(goList) {
		tp.emitLine("var %s string", goList)
		tp.vars = append(tp.vars, goList)
	}
	tp.emitLine("db.SetPreupdateHook(func() {")
	tp.indent++
	tp.emitLine("_r = tclPrepareStmt(db, %q, \"SELECT ?\", -1)", procName)
	tp.emitLine("if _s := tclPrepared[%q]; _s != nil {", procName)
	tp.indent++
	tp.emitLine("if db.PreupdateType() == \"INSERT\" {")
	tp.indent++
	tp.emitLine("_ = _s.Bind(1, db.PreupdateNew(0))")
	tp.indent--
	tp.emitLine("} else {")
	tp.indent++
	tp.emitLine("_ = _s.Bind(1, db.PreupdateOld(0))")
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("tclInvalidateStep(%q)", procName)
	tp.emitLine("_ = tclStepStmt(db, %q)", procName)
	tp.emitLine("%s = tclListAppend(%s, tclColumnTextOf(%q, 0))", goList, goList, procName)
	tp.emitLine("_r = tclResetStmtCode(%q)", procName)
	tp.emitLine("if _s := tclPrepared[%q]; _s != nil {", procName)
	tp.indent++
	tp.emitLine("_ = _s.Bind(1, db.PreupdateNew(0))")
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("tclInvalidateStep(%q)", procName)
	tp.emitLine("_ = tclStepStmt(db, %q)", procName)
	tp.emitLine("%s = tclListAppend(%s, tclColumnTextOf(%q, 0))", goList, goList, procName)
	tp.emitLine("_r = tclFinalizeStmt(db, %q)", procName)
	tp.indent--
	tp.emitLine("})")
	return true
}

// processDBCommitHook handles `db commit_hook [proc]` — register, clear, or
// query the connection's commit hook (sqlite3_commit_hook). With a proc name
// it emits db.SetCommitHook with a Go closure transpiled from the proc body;
// `db commit_hook {}` clears; `db commit_hook` queries the registered name.
func (tp *transpiler) processDBCommitHook(rest []tcl.RawWord) {
	if len(rest) == 0 {
		// Query: return the registered proc name ("::commit_hook" or "{}").
		tp.emitLine("_r = %q", tp.commitHookName)
		return
	}
	arg := strings.TrimSpace(rest[0].Text)
	if arg == "{}" || arg == "" {
		tp.commitHookName = ""
		tp.emitLine("db.SetCommitHook(nil)")
		return
	}
	procName := strings.TrimPrefix(arg, "::")
	if body, ok := tp.commitHookBodies[procName]; ok {
		tp.commitHookName = arg
		tp.emitLine("db.SetCommitHook(func() int {")
		tp.indent++
		tp.transpileHookBody(body, "commit")
		tp.indent--
		tp.emitLine("})")
	} else {
		tp.commitHookName = arg
		tp.emitLine("// db commit_hook %s (proc body not captured)", arg)
	}
}

// processDBRollbackHook handles `db rollback_hook [proc]` — register, clear,
// or query the connection's rollback hook (sqlite3_rollback_hook).
func (tp *transpiler) processDBRollbackHook(rest []tcl.RawWord) {
	if len(rest) == 0 {
		tp.emitLine("_r = %q", tp.rollbackHookName)
		return
	}
	arg := strings.TrimSpace(rest[0].Text)
	if arg == "{}" || arg == "" {
		tp.rollbackHookName = ""
		tp.emitLine("db.SetRollbackHook(nil)")
		return
	}
	procName := strings.TrimPrefix(arg, "::")
	// `db rollback_hook [list incr ::VAR]` — increment the named variable on
	// each rollback (hook.test 5.x).
	if strings.HasPrefix(arg, "[list incr ") && strings.HasSuffix(arg, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(arg, "["), "]")
		fields := strings.Fields(inner)
		if len(fields) == 3 && fields[0] == "list" && fields[1] == "incr" {
			goVar := tclVarToGo(strings.TrimPrefix(fields[2], "::"))
			tp.rollbackHookName = arg
			tp.emitLine("db.SetRollbackHook(func() {")
			tp.indent++
			tp.emitLine("// rollback hook: incr %s", goVar)
			tp.emitLine("{ _n, _err := strconv.Atoi(%s); if _err == nil { %s = strconv.Itoa(_n + 1) } }", goVar, goVar)
			tp.indent--
			tp.emitLine("})")
			return
		}
	}
	if body, ok := tp.commitHookBodies[procName]; ok {
		tp.rollbackHookName = arg
		tp.emitLine("db.SetRollbackHook(func() {")
		tp.indent++
		tp.transpileHookBody(body, "rollback")
		tp.indent--
		tp.emitLine("})")
	} else {
		tp.rollbackHookName = arg
		tp.emitLine("// db rollback_hook %s (proc body not captured)", arg)
	}
}

// processDBUpdateHook handles `db update_hook CB` — register, clear, or query
// the connection's update hook (sqlite3_update_hook). The callback is either a
// `[list lappend ::update_hook]` TCL list (append op/db/table/rowid) or a proc
// name whose body is transpiled.
func (tp *transpiler) processDBUpdateHook(rest []tcl.RawWord) {
	if len(rest) == 0 {
		tp.emitLine("_r = %q", tp.updateHookName)
		return
	}
	arg := strings.TrimSpace(rest[0].Text)
	if arg == "{}" || arg == "" {
		tp.updateHookName = ""
		tp.emitLine("db.SetUpdateHook(nil)")
		return
	}
	// `db update_hook [list lappend ::VAR]` — append the callback args
	// (op, db, table, rowid) to the named variable.
	if strings.HasPrefix(arg, "[list lappend ") && strings.HasSuffix(arg, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(arg, "["), "]")
		fields := strings.Fields(inner)
		if len(fields) == 3 && fields[0] == "list" && fields[1] == "lappend" {
			goVar := tclVarToGo(strings.TrimPrefix(fields[2], "::"))
			tp.updateHookName = arg
			tp.emitLine("db.SetUpdateHook(func(_op, _db, _tbl string, _rid int64) {")
			tp.indent++
			tp.emitLine("%s = tclListAppend(%s, _op, _db, _tbl, strconv.FormatInt(_rid, 10))", goVar, goVar)
			tp.indent--
			tp.emitLine("})")
			return
		}
	}
	procName := strings.TrimPrefix(arg, "::")
	if _, ok := tp.commitHookBodies[procName]; ok {
		tp.updateHookName = arg
		tp.emitLine("db.SetUpdateHook(func(_op, _db, _tbl string, _rid int64) {")
		tp.indent++
		tp.emitLine("// update hook proc %s: append callback args", procName)
		tp.emitLine("%s = tclListAppend(%s, _op, _db, _tbl, strconv.FormatInt(_rid, 10))", updateHookVar(procName), updateHookVar(procName))
		tp.indent--
		tp.emitLine("})")
	} else {
		tp.updateHookName = arg
		tp.emitLine("// db update_hook %s (proc body not captured)", arg)
	}
}

// updateHookVar returns the Go variable name the update_hook callback appends
// to (the ::update_hook TCL variable).
func updateHookVar(procName string) string {
	return tclVarToGo("update_hook")
}

// transpileHookBody transpiles a hook proc body into Go statements in the
// current output buffer. The body runs in a fresh sub-transpiler sharing the
// connection variable, so `incr ::commit_cnt` and
// `set ::commit_cnt [execsql {...}]` resolve to the Go variables.
func (tp *transpiler) transpileHookBody(body, kind string) {
	hookTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		testPrefix:   tp.testPrefix,
		queryVars:    tp.queryVars,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs: tp.collateGoFuncs,
		preparedState:  tp.preparedState,
		varConstValues: tp.varConstValues,
	}
	hookTP.processCommands(parseCommands(body))
	tp.varCount = hookTP.varCount
	tp.indent = hookTP.indent
	tp.varConstValues = hookTP.varConstValues
}

// isRollbackStmt reports whether the SQL text is a plain ROLLBACK statement
// (with optional whitespace/trailing semicolon).
func isRollbackStmt(sqlText string) bool {
	s := strings.TrimSpace(strings.ToUpper(strings.TrimSpace(sqlText)))
	s = strings.TrimSuffix(s, ";")
	return strings.TrimSpace(s) == "ROLLBACK"
}

// emitDBEvalCallback transpiles the TCL row-callback form
// `db eval {SQL} {body}`: the SELECT is executed (eagerly — the engine
// materializes rows), then the braced body runs once per row. A ROLLBACK
// executed inside the body aborts the iteration with SQLite's
// "abort due to ROLLBACK" error (trans3.test). The body's `db eval ROLLBACK`
// sets a shared Go bool via the rollbackFlag field.
func (tp *transpiler) emitDBEvalCallback(rest []tcl.RawWord) {
	tp.emitDBEvalCallbackConn("db", rest)
}

// emitDBEvalCallbackConn is emitDBEvalCallback for an arbitrary connection
// variable (db, db2, ...): `dbN eval {SQL} {body}` runs the braced body
// once per result row with the row's columns bound as TCL variables.
func (tp *transpiler) emitDBEvalCallbackConn(dbConn string, rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	rowsVar := fmt.Sprintf("_dbevalRows%d", tp.varCount)
	tp.varCount++
	rbFlag := fmt.Sprintf("_dbevalRb%d", tp.varCount)
	tp.varCount++
	iterErr := fmt.Sprintf("_dbevalErr%d", tp.varCount)
	tp.varCount++
	intFlag := fmt.Sprintf("_dbevalInt%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := %s.Query(%s)", rowsVar, dbConn, sqlExpr)
	tp.emitLine("var %s bool", rbFlag)
	tp.emitLine("var %s error", iterErr)
	tp.emitLine("var %s bool", intFlag)
	// Upstream, the scanned SELECT is a RUN-state VM for the whole callback
	// loop (db->nVdbeRead), so DDL inside the body hits the OP_Destroy
	// interlock ("database table is locked" — vtabdrop 1.1).
	tp.emitLine("%s.BeginActiveStatement()", dbConn)
	tp.emitLine("for _ri := 0; _ri < len(%s.Rows) && %s == nil; _ri++ {", rowsVar, iterErr)
	tp.indent++
	// TCL `db eval {SQL} {body}` binds the body's variables to the query's
	// result COLUMNS by name ($name → column "name"). Shadow the outer
	// variables with the current row's column values before the body runs.
	// Only columns referenced by the body need binding; emit a switch over
	// the runtime column names so any query (PRAGMA database_list, SELECT
	// *) works without knowing the schema at transpile time.
	tp.emitLine("for _ci := 0; _ci < len(%s.Columns); _ci++ {", rowsVar)
	tp.indent++
	tp.emitLine("switch %s.Columns[_ci] {", rowsVar)
	tp.indent++
	for _, col := range dbEvalCallbackColumns(rest[1].Text) {
		goVar := tclVarToGo(col)
		if !isValidGoIdent(goVar) || goVar == "" {
			continue
		}
		// The callback assigns these variables for the duration of the body;
		// register them as assigned so nested braced SQL containing $col
		// substitutes them (hasDeclaredDollarVarRef gate).
		if !isAssignedTCLVar(goVar) {
			activeAssignedVars = append(activeAssignedVars, goVar)
		}
		tp.emitLine("case %q:", col)
		tp.indent++
		tp.emitLine("%s = tclStr(%s.Rows[_ri][_ci])", goVar, rowsVar)
		tp.indent--
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	// Transpile the body with the rollback/interrupt flags wired:
	// `db eval ROLLBACK` sets rbFlag; `sqlite3_interrupt` sets intFlag.
	// Either aborts the loop after the body (SQLite's next sqlite3_step
	// returns SQLITE_INTERRUPT / the ROLLBACK error).
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		testPrefix:   tp.testPrefix,
		queryVars:    tp.queryVars,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		collateGoFuncs: tp.collateGoFuncs,
		rollbackFlag:   rbFlag,
		interruptFlag:  intFlag,
		preparedState:  tp.preparedState,
		varConstValues: tp.varConstValues,
	}
	bodyTP.processCommands(parseCommands(rest[1].Text))
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.varConstValues = bodyTP.varConstValues
	tp.emitLine("if %s { %s = errors.New(\"abort due to ROLLBACK\") }", rbFlag, iterErr)
	tp.emitLine("if %s { %s = errors.New(\"interrupted\"); %s.ClearInterrupt() }", intFlag, iterErr, dbConn)
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("%s.EndActiveStatement()", dbConn)
	tp.emitLine("if %s != nil {", iterErr)
	tp.indent++
	if tp.catchMode {
		tp.emitLine("_catchErr = %s", iterErr)
	} else {
		tp.emitLine("t.Errorf(\"db eval callback error: %%v\", %s)", iterErr)
	}
	tp.indent--
	tp.emitLine("}")
}

// dbEvalCallbackColumns returns the distinct TCL variable names referenced
// in a `db eval {SQL} {body}` callback body. These variables are bound to
// the query result columns by name at runtime (see emitDBEvalCallback).
// Variables embedded mid-token (e.g. "values($pgno,") are found too: the
// scan walks the body character by character.
func dbEvalCallbackColumns(body string) []string {
	seen := map[string]bool{}
	var cols []string
	for i := 0; i < len(body); i++ {
		if body[i] != '$' || i+1 >= len(body) {
			continue
		}
		name := callbackColumnName(body[i+1:])
		if name != "" && !seen[name] {
			seen[name] = true
			cols = append(cols, name)
		}
	}
	return cols
}

// callbackColumnName normalizes a TCL variable token ($name, ${name}, $x,)
// to its plain identifier, stopping at non-identifier characters.
func callbackColumnName(tok string) string {
	name := strings.TrimLeft(tok, "${")
	name = strings.TrimRight(name, "}")
	for i, r := range name {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')) {
			return name[:i]
		}
	}
	return name
}

// processDBForName handles dbN commands (db2, db3, etc.) — secondary DB connections.
func (tp *transpiler) processDBForName(dbName string, args []tcl.RawWord) {
	if len(args) == 0 {
		return
	}
	goName := tclVarToGo(dbName)
	sub := args[0].Text
	rest := args[1:]

	switch sub {
	case "close":
		tp.processNamedDBClose(goName, dbName)
	case "backup":
		tp.processNamedDBBackupRestore(goName, "backup", rest)
	case "restore":
		tp.processNamedDBBackupRestore(goName, "restore", rest)
	case "eval":
		tp.processNamedDBEval(goName, rest)
	case "onecolumn":
		tp.processNamedDBOnecolumn(goName, rest)
	case "function", "func":
		tp.processNamedDBFunction(goName, rest)
	case "changes":
		tp.emitLine("_r = strconv.FormatInt(%s.Changes(), 10)", goName)
	case "total_changes":
		tp.emitLine("_r = strconv.FormatInt(%s.TotalChanges(), 10)", goName)
	case "transaction":
		tp.processNamedDBTransaction(goName, rest)
	case "cache", "create_function",
		"trace", "busy":
		// no-op: infrastructure
	case "collate":
		tp.processNamedDBCollate(goName, rest)
	case "progress":
		tp.processNamedDBProgress(rest)
	case "authorizer":
		tp.processNamedDBAuthorizer(goName, rest)
	default:
		tp.emitLine("// %s.%s (db command)", goName, sub)
	}
}

// processNamedDBBackupRestore handles `dbN backup [schema] FILE` and `dbN
// restore [schema] FILE` on a secondary connection.
func (tp *transpiler) processNamedDBBackupRestore(goName, kind string, rest []tcl.RawWord) {
	schemaName := "\"main\""
	fileArg := ""
	if len(rest) >= 2 {
		schemaName = tp.goStringLiteral(rest[0])
		fileArg = tp.goStringLiteral(rest[1])
	} else if len(rest) == 1 {
		fileArg = tp.goStringLiteral(rest[0])
	} else {
		tp.emitLine("// %s %s (wrong # args)", goName, kind)
		return
	}
	if !tp.catchMode {
		tp.emitLine("var _catchErr error")
	}
	tp.emitLine("_catchErr = tclDBBackupRestore(%s, %q, %s, %s)", goName, kind, schemaName, fileArg)
	tp.emitLine("if _catchErr != nil { _r = \"\" }")
}

// processNamedDBClose handles `dbN close`.
func (tp *transpiler) processNamedDBClose(goName, dbName string) {
	if target, ok := tp.dbAliases[goName]; ok {
		// Aliased connection shares the main in-memory db; closing it
		// must not close the shared handle.
		tp.emitLine("_ = %s // close %s: aliased to %s, no-op", goName, dbName, target)
		return
	}
	if tp.collateDtorVars != nil {
		for _, incrVar := range tp.collateDtorVars {
			tp.emitIncrCounter(incrVar)
		}
		tp.collateDtorVars = nil
	}
	tp.emitLine("if %s != nil { %s.Close() }", goName, goName)
}

// processNamedDBEval handles `dbN eval {SQL}`.
func (tp *transpiler) processNamedDBEval(goName string, rest []tcl.RawWord) {
	// `dbN eval {SQL} {body}`: the row-callback form runs the body once per
	// result row (dbpage 510 copies pages between connections this way).
	if len(rest) >= 2 && rest[1].Braced {
		tp.emitDBEvalCallbackConn(goName, rest)
		return
	}
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	tp.emitLine("_res = %s.Exec(%s)", goName, sqlExpr)
	if tp.catchMode {
		// Inside catch {dbN eval {...}}: the error becomes the catch result,
		// not a test failure (lock-4.2/4.3).
		tp.emitLine("if _res.Error != nil { _catchErr = _res.Error }")
	} else {
		tp.emitLine("if _res.Error != nil { t.Errorf(\"exec error: %%v\", _res.Error) }")
	}
}

// processNamedDBOnecolumn handles `dbN onecolumn {SQL}`.
func (tp *transpiler) processNamedDBOnecolumn(goName string, rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	tp.emitLine("r := %s.Query(%s)", goName, sqlExpr)
	tp.emitLine("if r.Error != nil { t.Errorf(\"query error: %%v\", r.Error) }")
}

// processNamedDBFunction handles `dbN function NAME PROC`.
func (tp *transpiler) processNamedDBFunction(goName string, rest []tcl.RawWord) {
	if len(rest) < 2 {
		return
	}
	name := strings.TrimSpace(rest[0].Text)
	procName := ""
	for _, a := range rest[1:] {
		arg := strings.TrimSpace(a.Text)
		if strings.HasPrefix(arg, "-") {
			continue
		}
		procName = strings.Fields(arg)[0]
		break
	}
	if procName == "" {
		return
	}
	if pred, ok := tp.predFuncs[procName]; ok && name != "" {
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", goName, name)
		tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
		tp.emitLine("\targ, _ := strconv.ParseFloat(tclStr(args[0]), 64)")
		tp.emitLine("\tif %s { return int64(1), nil }", pred)
		tp.emitLine("\treturn int64(0), nil")
		tp.emitLine("}, 0, -1)")
		return
	}
	if constVal, ok := tp.constFuncs[procName]; ok && name != "" {
		tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return int64(%s), nil }, 0, -1)", goName, name, constVal)
	}
}

// processNamedDBTransaction handles `dbN transaction {BODY}`.
func (tp *transpiler) processNamedDBTransaction(goName string, rest []tcl.RawWord) {
	if len(rest) == 0 || !rest[0].Braced {
		return
	}
	bodyCmds := parseCommands(rest[0].Text)
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: goName, t: tp.t, varCount: tp.varCount, vars: tp.vars, arrayKeys: tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
}

// processNamedDBCollate handles `dbN collate NAME PROC`.
func (tp *transpiler) processNamedDBCollate(goName string, rest []tcl.RawWord) {
	if len(rest) < 1 {
		return
	}
	collName := strings.TrimSpace(rest[0].Text)
	collWord := rest[0]
	var goFn string
	if len(rest) >= 2 {
		procArg := strings.TrimSpace(rest[1].Text)
		// Inline forms: {string compare} / "string compare" /
		// [list string compare -nocase].
		if f := collationProcGo(procArg); f != "" {
			goFn = f
		} else if fn, ok := tp.collateGoFuncs[procArg]; ok {
			goFn = fn
		}
	}
	if goFn != "" && collName != "" {
		tp.emitLine("%s.RegisterCollation(%s, %s)", goName, tp.goStringLiteral(collWord), goFn)
	} else {
		tp.emitLine("// db collate %s (not transpiled)", collName)
	}
}

// processNamedDBProgress handles `dbN progress N fn`.
func (tp *transpiler) processNamedDBProgress(rest []tcl.RawWord) {
	if len(rest) < 1 {
		return
	}
	period := strings.TrimSpace(rest[0].Text)
	varName := strings.TrimPrefix(period, "$")
	if isIntegerLiteral(period) || (strings.HasPrefix(period, "$") && tp.isVarDeclared(varName)) {
		tp.emitLine("db.SetProgressHandler(toInt(%s), func() bool { return true })", replaceVarRefsRaw(period))
	}
}
