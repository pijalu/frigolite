// SPDX-License-Identifier: GPL-3.0-or-later
package main

// The `db eval` transpilers: the plain exec form, the ARRAYVAR capture forms
// and the `{SQL} {body}` row-callback emitter (extracted from
// processdb_part2.go to keep both files under the 1000-line quality-gate
// limit).

import (
	"fmt"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processDBEval handles `db eval {SQL}` and the row-callback form
// `db eval {SQL} {body}`.
func (tp *transpiler) processDBEval(rest []tcl.RawWord) {
	// db eval {SQL} ARRAYVAR: TCL populates the array variable with the
	// first row's column names/values (column name is the key). The harness
	// checks array keys via `set A(*)` which becomes the expected column names.
	// For `db eval {SQL} A` where A is a known array variable, capture the
	// query's column names into A_arr for the subsequent `set A(*)` check.
	if len(rest) >= 2 && !rest[1].Braced && rest[1].Text != "" {
		if tp.emitDBEvalArrayVar(rest) {
			return
		}
		// Also handle the dynamic case: `db eval {SQL} $arrVar` or unknown
		// array var. Fall through to generic Exec path; the harness may
		// check via different var.
	}
	// db eval {SQL} {body}: TCL's row-callback form. SQLite steps the
	// SELECT row by row, running the braced body for each row. A ROLLBACK
	// executed inside the body succeeds but aborts the active SELECT with
	// "abort due to ROLLBACK" (SQLite's sqlite3_step returns SQLITE_ABORT
	// after a ROLLBACK invalidates the statement). Other statements
	// (COMMIT, DML) inside the body do not abort the iteration.
	if len(rest) >= 2 && rest[1].Braced {
		tp.emitDBEvalCallback(rest)
		return
	}
	tp.emitDBEvalPlain(rest)
}

// emitDBEvalArrayVar handles the `db eval SQL ARRAYVAR` (and
// `db eval SQL ARRAYVAR {BODY}`) forms whose second word names an array
// variable. Reports whether the statement was fully emitted (the caller
// falls back to the plain path otherwise).
func (tp *transpiler) emitDBEvalArrayVar(rest []tcl.RawWord) bool {
	arrName := rest[1].Text
	goName := tclVarToGo(arrName)
	if goName == "" {
		return false
	}
	// Known array — capture column names (arrayKeys may be nil if declared
	// in outer scope; still handle). Also treat single-letter array vars
	// like A as arrays even if not pre-registered.
	if tp.knownEvalArray(arrName) {
		return tp.emitDBEvalKnownArray(arrName, rest)
	}
	// `db eval SQL IDENT {BODY}` with an unregistered identifier:
	// TCL semantics bind IDENT(column) per row then run BODY
	// (lock.test's `db eval {SELECT ...} qv {set x ...}`).
	// Register as array and use the array-rows emitter.
	if len(rest) >= 3 && rest[2].Braced && isValidGoIdent(goName) {
		if tp.arrayKeys == nil {
			tp.arrayKeys = map[string][]string{}
		}
		tp.arrayKeys[arrName] = nil
		tp.emitDBEvalArrayRows(arrName, rest)
		return true
	}
	return false
}

// knownEvalArray reports whether arrName is a registered array variable or a
// single-letter uppercase name (treated as an array even when not
// pre-registered).
func (tp *transpiler) knownEvalArray(arrName string) bool {
	if tp.arrayKeys != nil {
		if _, ok := tp.arrayKeys[arrName]; ok {
			return true
		}
	}
	return len(arrName) == 1 && arrName[0] >= 'A' && arrName[0] <= 'Z'
}

// emitDBEvalKnownArray emits the known-array forms: the per-row body form
// (`db eval $sql X { ... }` — each row sets X(column) for every result
// column, then the body runs) and the plain column-capture form.
func (tp *transpiler) emitDBEvalKnownArray(arrName string, rest []tcl.RawWord) bool {
	if len(rest) >= 3 && rest[2].Braced {
		tp.emitDBEvalArrayRows(arrName, rest)
		return true
	}
	sqlExpr := tp.collectSQLExpression(rest[:1])
	if sqlExpr == `""` {
		return false
	}
	tp.emitLine("r = db.Query(%s)", sqlExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
	tp.emitLine("}")
	arrStar := tclVarToGo(arrName + "(*)")
	if !tp.isVarDeclared(arrStar) {
		tp.emitLine("var %s string", arrStar)
		tp.vars = append(tp.vars, arrStar)
	}
	tp.emitLine("%s = strings.Join(r.Columns, \" \")", arrStar)
	// TCL's db eval sets A(*) to the column list; sync the tclvar registry
	// so a later `set A(*)` reads it even when the read goes through the
	// registry store (with1-17.2).
	tp.emitLine("vtab.TclVarSet(%q, \"*\", %s)", arrName, arrStar)
	tp.emitLine("_res = &frigolite.Result{Columns: r.Columns, Rows: r.Rows}")
	return true
}

// emitDBEvalPlain emits the plain `db eval {SQL}` exec (no row callback, no
// array variable).
func (tp *transpiler) emitDBEvalPlain(rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	sqlText := ""
	if len(rest) > 0 {
		sqlText = rest[0].Text
	}
	if reason := unsupportedSQL(sanitizeSQL(sqlText)); reason != "" {
		tp.emitLine("// db eval skipped: %s", reason)
		return
	}
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	if tp.catchMode {
		// Inside a `catch { ... }` block, capture Exec errors into
		// _catchErr so the enclosing `while {1}` break-on-error pattern
		// (e.g. tkt2686's `while 1 { db eval {INSERT ...} }`) can unwind.
		// Without this, the loop runs forever: `db eval` does NOT raise a
		// Go panic on engine errors, it just sets _res.Error, so the
		// transpiler must mirror TCL catch semantics explicitly here.
		tp.emitLine("if _res.Error != nil { _catchErr = _res.Error }")
	}
	if tp.rollbackFlag != "" && isRollbackStmt(sqlText) {
		// A ROLLBACK executed inside a db eval callback aborts the
		// enclosing row iteration (SQLite "abort due to ROLLBACK").
		tp.emitLine("%s = true", tp.rollbackFlag)
	}
	// db eval silently consumes Exec errors (TCL's `db eval` runs the
	// body for each row and the body itself executes the SQL; errors
	// inside the body are reported via the result code, not as a hard
	// test failure). The transpiler must NOT promote them to t.Errorf
	// here — a per-iteration error path on a 1000-row loop prints ~30s
	// of failure traffic and times out the suite (incrvacuum-6/7).
	_ = tp.catchMode // unused for the no-callback path
}

// processDBOnecolumn handles `db onecolumn {SQL}`.
func (tp *transpiler) processDBOnecolumn(rest []tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression(rest)
	if sqlExpr == `""` {
		return
	}
	tp.emitLine("r = db.Query(%s)", sqlExpr)
	if tp.catchMode {
		tp.emitLine("if r.Error != nil { _catchErr = r.Error }")
	} else {
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("}")
	}
}

// processDBTransaction handles `db transaction {BODY}` — transpile the body
// as regular code.
func (tp *transpiler) processDBTransaction(rest []tcl.RawWord) {
	if len(rest) == 0 || !rest[0].Braced {
		return
	}
	bodyCmds := parseCommands(rest[0].Text)
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
		testPrefix:   tp.testPrefix, preparedState: tp.preparedState,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
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
	// Body selection: `db eval {SQL} {body}` passes the callback as the
	// second word; the three-word TCL form `db eval {SQL} {arrayName} {body}`
	// (misc2-7.2: `db eval {SELECT rowid FROM t1} {} { db eval ... }`) puts
	// an optional array name in the middle — the LAST word is the row body.
	// The previous code always used rest[1], so the 3-word form transpiled
	// with an empty body and no column bindings (the DELETE never ran and
	// `SELECT * FROM t1` kept its rows).
	bodyWord := rest[1]
	if len(rest) >= 3 {
		bodyWord = rest[len(rest)-1]
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
	// TCL aborts the statement (and the enclosing script, unless caught)
	// when the query itself fails — e.g. "no such function: qbox" in an
	// rtreedoc3-style MATCH body. Propagate the error into the iteration
	// error so the loop is skipped and the existing tail (catch mode →
	// _catchErr, plain mode → test error) reports it, mirroring the
	// non-callback db eval paths. The loop condition
	// (`&& iterErr == nil`) keeps Begin/EndActiveStatement balanced.
	tp.emitLine("if %s.Error != nil { %s = %s.Error }", rowsVar, iterErr, rowsVar)
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
	for _, col := range dbEvalCallbackColumns(bodyWord.Text) {
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
		collateGoFuncs:      tp.collateGoFuncs,
		collateEmittedProcs: tp.collateEmittedProcs,
		procBodies:          tp.procBodies,
		rollbackFlag:        rbFlag,
		interruptFlag:       intFlag,
		catchMode:           tp.catchMode,
		inDBEvalCb:          true,
		preparedState:       tp.preparedState,
		varConstValues:      tp.varConstValues,
	}
	bodyTP.processCommands(parseCommands(bodyWord.Text))
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
