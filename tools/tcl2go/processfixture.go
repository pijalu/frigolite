// Package main implements the tcl2go tool.
//
// This file handles the SQLite TCL test-harness `testfixture` /
// `launch_testfixture` commands used by the multi-process locking tests
// (lock2.test, lock4.test, ...). A testfixture is a persistent second process
// opened on the same database file; commands sent to it run on a separate
// connection, so cross-connection lock semantics (RESERVED / PENDING /
// EXCLUSIVE) apply. Frigolite has no OS-level file locking, so the transpiler
// emulates the second process as a persistent in-process connection keyed by
// the fixture name in the package-level tclFixtureDBs map (see the helpers
// template). The lock registry (internal/lockreg) is process-global and keyed
// by canonical file path, so a main-connection write transaction and a
// fixture-connection read transaction contend exactly as two OS processes
// would.
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// fixtureVarName extracts the fixture name from the first argument of a
// `testfixture $VAR {...}` command. `$::tf1` / `$tf1` → "tf1".
func (tp *transpiler) fixtureVarName(args []tcl.RawWord) string {
	if len(args) < 1 {
		return ""
	}
	name := strings.TrimSpace(args[0].Text)
	name = strings.TrimPrefix(name, "$")
	name = strings.TrimPrefix(name, "::")
	gv := tclVarToGo(name)
	if gv == "" {
		return ""
	}
	return gv
}

// fixtureScriptFile returns the database file opened by a `sqlite3 db FILE`
// command inside the fixture script (the fixture connection's file). Defaults
// to "test.db" — the main test database in the lock tests.
func (tp *transpiler) fixtureScriptFile(scriptCmds [][]tcl.RawWord) string {
	for _, cmd := range scriptCmds {
		if len(cmd) >= 3 && cmd[0].Text == "sqlite3" && tclVarToGo(cmd[1].Text) == "db" {
			return cmd[2].Text
		}
	}
	return "test.db"
}

// emitTestfixtureBlock emits a Go block that runs a testfixture SCRIPT on the
// fixture connection named varName. The fixture connection is opened on demand
// (persisting across calls keyed by varName), the main `db` variable is
// shadowed to the fixture connection inside the block, and the SCRIPT is
// sub-transpiled with tp.fixtureVar set so `sqlite3 db FILE` / `db close`
// operate on the fixture map. When compare is non-nil it is invoked after the
// script to emit the do_test result comparison (reusing the standard
// emitDoTestBodyComparison machinery).
func (tp *transpiler) emitTestfixtureBlock(varName, filename string, scriptCmds [][]tcl.RawWord, compare func()) {
	tp.emitLine("{ // testfixture %s", varName)
	tp.indent++
	tp.emitLine("if tclFixtureDBs[%q] == nil {", varName)
	tp.indent++
	tp.emitLine("_fxdb, _fxerr := frigolite.Open(%q)", filename)
	tp.emitLine("if _fxerr != nil { t.Fatal(_fxerr) }")
	tp.emitLine("tclFixtureDBs[%q] = _fxdb", varName)
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("db := tclFixtureDBs[%q]", varName)
	// Run the fixture script with db shadowed to the fixture connection and
	// tp.fixtureVar set so sqlite3/db-close route to the fixture map.
	tp.fixtureVar = varName
	tp.runDoTestBody(scriptCmds)
	tp.fixtureVar = ""
	tp.emitLine("_ = db")
	if compare != nil {
		compare()
	}
	tp.indent--
	tp.emitLine("}")
}

// processLaunchTestfixture handles `launch_testfixture` (spawns a persistent
// testfixture process in the TCL harness). The transpiler emulates it as a
// fresh fixture name; the command's result is the fixture name string.
func (tp *transpiler) processLaunchTestfixture(args []tcl.RawWord) {
	tp.emitLine("_r = launchTestfixture()")
}

// processTestfixture handles a `testfixture $VAR {SCRIPT}` command appearing as
// a bare statement (outside a do_test body). It runs the SCRIPT for its
// side effects on the fixture connection (e.g. taking a SHARED/RESERVED lock
// that blocks the main connection) but does not compare a result.
func (tp *transpiler) processTestfixture(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	varName := tp.fixtureVarName(args[0:1])
	if varName == "" {
		tp.emitLine("// testfixture (unresolved fixture name)")
		return
	}
	scriptCmds := tp.parseBracedBody(args, 1)
	if scriptCmds == nil {
		tp.emitLine("// testfixture %s (no script body)", varName)
		return
	}
	filename := tp.fixtureScriptFile(scriptCmds)
	tp.emitTestfixtureBlock(varName, filename, scriptCmds, nil)
}

// emitDoTestTestfixtureBody handles a do_test whose body opens a testfixture
// and runs a SCRIPT on it (lock2.test's pattern). The body is either a single
// `testfixture $VAR {SCRIPT}` command, or a `set ::VAR [launch_testfixture]`
// followed by `testfixture $VAR {SCRIPT}`. The result of the body is the result
// of the SCRIPT's last command, which the standard emitDoTestBodyComparison
// machinery computes once SCRIPT has run on the fixture connection. Returns
// true when the body was handled.
func (tp *transpiler) emitDoTestTestfixtureBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if bodyCmds == nil {
		return false
	}
	fxIdx := -1
	for i, cmd := range bodyCmds {
		if len(cmd) > 0 && cmd[0].Text == "testfixture" {
			fxIdx = i
			break
		}
	}
	if fxIdx < 0 {
		return false
	}
	fxCmd := bodyCmds[fxIdx]
	if len(fxCmd) < 2 {
		return false
	}
	scriptCmds := tp.parseBracedBody(fxCmd, 1)
	if scriptCmds == nil {
		return false
	}
	varName := tp.fixtureVarName(fxCmd[1:2])
	if varName == "" {
		return false
	}
	filename := tp.fixtureScriptFile(scriptCmds)

	tp.emitLine("{ // do_test %s (testfixture)", nameExpr)
	tp.indent++
	// Emit any preamble commands (e.g. `set ::tf1 [launch_testfixture]`).
	for _, cmd := range bodyCmds[:fxIdx] {
		if len(cmd) >= 2 && cmd[0].Text == "set" {
			rhs := strings.TrimSpace(cmd[1].Text)
			if strings.Contains(rhs, "launch_testfixture") {
				gv := tclVarToGo(cmd[1].Text)
				if !tp.isVarDeclared(gv) {
					tp.emitLine("var %s string", gv)
					tp.vars = append(tp.vars, gv)
				}
				tp.emitLine("%s = launchTestfixture()", gv)
				continue
			}
		}
		tp.emitLine("// %s (testfixture preamble, not transpiled)", sanitizeTCLComment(commandsToText(cmd)))
	}
	// Build the fixture block with the do_test comparison as the trailing step.
	tp.emitTestfixtureBlockInline(varName, filename, scriptCmds, nameExpr, expectedExpr, args)
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitTestfixtureBlockInline is emitTestfixtureBlock specialized for do_test
// bodies: it opens the fixture, shadows db, runs the script, then emits the
// result comparison (so the do_test assertion checks the script's last-command
// result, e.g. "database is locked" from a blocked CREATE, or the rows of a
// fixture-side SELECT).
func (tp *transpiler) emitTestfixtureBlockInline(varName, filename string, scriptCmds [][]tcl.RawWord, nameExpr, expectedExpr string, args []tcl.RawWord) {
	tp.emitLine("if tclFixtureDBs[%q] == nil {", varName)
	tp.indent++
	tp.emitLine("_fxdb, _fxerr := frigolite.Open(%q)", filename)
	tp.emitLine("if _fxerr != nil { t.Fatal(_fxerr) }")
	tp.emitLine("tclFixtureDBs[%q] = _fxdb", varName)
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("db := tclFixtureDBs[%q]", varName)
	tp.fixtureVar = varName
	tp.runDoTestBody(scriptCmds)
	tp.fixtureVar = ""
	tp.emitLine("_ = db")
	tp.emitFixtureResultComparison(nameExpr, expectedExpr, scriptCmds, args)
}

// emitFixtureResultComparison emits the do_test value comparison for a
// testfixture do_test body. The body's value is the result of SCRIPT's last
// command: a `set VAR` leaves VAR's value (typically an error message captured
// by `catch {…} VAR`); a `db eval`/`db onecolumn` leaves the flattened query
// rows. Reuse the existing single-command comparison emitters so the
// normalization (tclListFlatten / flatten) matches the rest of the corpus.
func (tp *transpiler) emitFixtureResultComparison(nameExpr, expectedExpr string, scriptCmds [][]tcl.RawWord, args []tcl.RawWord) {
	last := scriptCmds[len(scriptCmds)-1]
	if last[0].Text == "set" && len(last) >= 2 {
		tp.emitSetVarResultCheck(nameExpr, expectedExpr, tclVarToGo(last[1].Text))
		return
	}
	sqlExpr := ""
	if len(last) >= 3 && last[0].Text == "db" && (last[1].Text == "eval" || last[1].Text == "onecolumn") {
		sqlExpr = tp.collectSQLExpression(last[2:3])
	} else {
		sqlExpr = tp.collectSQLExpression(last)
	}
	if sqlExpr == `""` {
		return
	}
	// emitDBEvalQueryResult re-runs the SQL on the (fixture-shadowed) db and
	// flattens the result for comparison, handling both TCL-list and regex
	// expected values via args[2].
	tp.emitDBEvalQueryResult(nameExpr, expectedExpr, sqlExpr, args)
}

// commandsToText renders a TCL command's words as a single line for a comment.
func commandsToText(cmd []tcl.RawWord) string {
	var parts []string
	for _, w := range cmd {
		parts = append(parts, w.Text)
	}
	return strings.Join(parts, " ")
}
