// SPDX-License-Identifier: GPL-3.0-or-later
package main

// The `dbN eval {SQL} {arrayName} {body}` row-callback emitter (extracted
// from processdb_part2.go to keep both files under the 1000-line
// quality-gate limit).

import (
	"fmt"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

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

