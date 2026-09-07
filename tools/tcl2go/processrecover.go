// Package main implements the tcl2go tool.
//
// This file transpiles recover.test's .recover harness procs
// (recover_with_opts / do_recover_test / compare_result / compare_dbs) to
// in-process RecoverSQL calls (SQLite ext/misc/recover.c port; no CLI
// subprocess).
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processTestFindCli handles `set CLI [test_find_cli]`: the CLI path is
// unused after the in-process port; bind a placeholder.
func (tp *transpiler) processTestFindCli(args []tcl.RawWord) {
	tp.emitLine("// test_find_cli (CLI unused; in-process RecoverSQL)")
}

// processRecoverWithOpts handles `recover_with_opts OPTS`: run in-process
// RecoverSQL on db, rebuild test.db2 from the emitted script, and leave a
// fresh db2 connection open (mirrors the TCL proc's sqlite3 db2 test.db2).
func (tp *transpiler) processRecoverWithOpts(args []tcl.RawWord) {
	opts := ""
	if len(args) >= 1 {
		opts = strings.TrimSpace(args[0].Text)
	}
	ignore := "false"
	if strings.Contains(opts, "ignore-freelist") {
		ignore = "true"
	}
	tp.emitLine("{ // recover_with_opts %s", sanitizeTCLComment(opts))
	tp.indent++
	tp.emitLine("_recSQL, _recErr := db.RecoverSQL(%s)", ignore)
	tp.emitLine("if _recErr != nil { t.Fatalf(\"recover: %%v\", _recErr) }")
	tp.emitLine("_ = os.Remove(\"test.db2\")")
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("_rdb, _rerr := frigolite.Open(\"test.db2\")")
	tp.emitLine("if _rerr != nil { t.Fatal(_rerr) }")
	tp.emitLine("if _rr := _rdb.Exec(_recSQL); _rr.Error != nil { t.Fatalf(\"recover replay: %%v\", _rr.Error) }")
	tp.emitLine("if _cerr := _rdb.Close(); _cerr != nil { t.Fatalf(\"recover close: %%v\", _cerr) }")
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processDoRecoverTest handles `do_recover_test TN [TSQL RES]`:
// recover with default options, then either compare db vs db2 (no TSQL)
// or run a do_execsql_test-equivalent check on db2.
func (tp *transpiler) processDoRecoverTest(args []tcl.RawWord) {
	// Strip optional "-db" prefix (not used by recover.test; be lenient).
	if len(args) >= 1 && args[0].Text == "-db" && len(args) >= 2 {
		args = args[2:]
	}
	if len(args) < 1 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	tp.emitLine("{ // do_recover_test %s", nameExpr)
	tp.indent++
	tp.emitLine("_recSQL, _recErr := db.RecoverSQL(false)")
	tp.emitLine("if _recErr != nil { t.Fatalf(\"recover: %%v\", _recErr) }")
	tp.emitLine("_ = os.Remove(\"test.db2\")")
	tp.emitLine("db2, _derr := frigolite.Open(\"test.db2\")")
	tp.emitLine("if _derr != nil { t.Fatal(_derr) }")
	tp.emitLine("tclConnRegister(\"db2\", db2)")
	tp.emitLine("if _rr := db2.Exec(_recSQL); _rr.Error != nil { t.Fatalf(\"recover replay: %%v\", _rr.Error) }")
	if len(args) == 1 {
		tp.emitLine("tclRecoverCompareDBs(t, db, db2)")
	} else {
		sqlExpr := tp.collectSQLExpression(args[1:2])
		expectedExpr := `""`
		if len(args) >= 3 {
			if expr, ok := tp.expectedStringExpr(args[2]); ok {
				expectedExpr = expr
			} else {
				expectedExpr = tp.expectLiteral(args[2])
			}
		}
		sql := ""
		if len(args) >= 2 {
			sql = args[1].Text
		}
		tp.emitExecSQLTestBody(nameExpr, "db2", sqlExpr, expectedExpr, sql, args)
	}
	tp.emitLine("db2.Close()")
	tp.indent--
	tp.emitLine("}")
}

// processCompareResult handles `compare_result DB1 DB2 SQL`: compare one
// query's flattened rows across two connections.
func (tp *transpiler) processCompareResult(args []tcl.RawWord) {
	if len(args) < 3 {
		tp.emitLine("// compare_result (arity, skipped)")
		return
	}
	c1 := tclVarToGo(args[0].Text)
	c2 := tclVarToGo(args[1].Text)
	if !isValidGoIdent(c1) {
		c1 = "db"
	}
	if !isValidGoIdent(c2) {
		c2 = "db2"
	}
	sqlExpr := tp.collectSQLExpression(args[2:3])
	tp.emitLine("tclRecoverCompareResult(t, %s, %s, %s)", c1, c2, sqlExpr)
}

// processCompareDBs handles `compare_dbs DB1 DB2`: schema + per-table compare.
func (tp *transpiler) processCompareDBs(args []tcl.RawWord) {
	c1, c2 := "db", "db2"
	if len(args) >= 1 && isValidGoIdent(tclVarToGo(args[0].Text)) {
		c1 = tclVarToGo(args[0].Text)
	}
	if len(args) >= 2 && isValidGoIdent(tclVarToGo(args[1].Text)) {
		c2 = tclVarToGo(args[1].Text)
	}
	tp.emitLine("tclRecoverCompareDBs(t, %s, %s)", c1, c2)
}

var _ = fmt.Sprint
