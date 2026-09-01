// Package main implements the tcl2go tool.
//
// This file handles do_test / do_eqp_test bodies.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

func (tp *transpiler) processDoTest(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	bodyCmds := tp.parseBracedBody(args, 1)

	// A do_test body whose assertion cannot be transpiled (echo-module ABI
	// probes, CLI shell subprocess invocations, VDBE-internal state) is
	// emitted by the kind-specific skip emitter below.
	if tp.emitDoTestSkippedByBodyKind(nameExpr, bodyCmds) {
		return
	}

	// A braced body that is only TCL comments (e.g. temptrigger-1.5's
	// "# Before the bug was fixed ...") has no commands; parseCommands
	// returns nil. Emit a no-op instead of treating the comment text as SQL.
	if isCommentOnlyBody(args) {
		tp.emitLine("{ // %s (comment-only body)", nameExpr)
		tp.emitLine("}")
		return
	}

	expectedExpr := `""`
	if len(args) >= 3 {
		if expr, ok := tp.expectedStringExpr(args[2]); ok {
			expectedExpr = expr
		} else {
			expectedExpr = tp.expectLiteral(args[2])
		}
	}
	if ov := wantOverride(overrideFile(tp), args[0].Text); ov != "" {
		expectedExpr = tp.expectLiteral(tcl.RawWord{Text: ov, Braced: true})
	}

	// A single `sqlite3_limit db LIMIT -1` body queries the current limit;
	// the expected value is the limit number (e.g. attach4-1.1 expects
	// $SQLITE_MAX_ATTACHED). Emit a direct value comparison.
	if tp.emitLimitComparison(nameExpr, expectedExpr, bodyCmds) {
		return
	}

	// A single `lsort -integer $VAR` body sorts a TCL list variable (the
	// result of an earlier `set VAR [db eval ...]`) and compares it to the
	// expected value (rowvalue4 2.1.x). The variable holds a space-separated
	// list of query result cells.
	if tp.emitLSortComparison(nameExpr, expectedExpr, bodyCmds) {
		return
	}

	// TCL do_test compares the VALUE of the body script with the expected
	// argument. The most common body form is a single `db eval { SQL }`
	// command; transpile it with a real result comparison (query → flatten
	// → compare), matching do_execsql_test semantics.
	if tp.emitDBEvalComparison(nameExpr, expectedExpr, bodyCmds, args) {
		return
	}

	// A single `file size PATH` body (extension01 1.5): compare the current
	// file size against the expected value.
	if tp.emitBareFileSizeComparison(nameExpr, expectedExpr, bodyCmds) {
		return
	}

	// A single `lindex [catchsql SQL] 0` body (e.g. window1 2.x,
	// tkt-bd484a090c 1.x): the do_test value is the catchsql success/error
	// code, so run the SQL and compare (success when expected "0").
	if tp.emitDoTestCatchsqlLindexBody(nameExpr, expectedExpr, bodyCmds, args) {
		return
	}

	// A single `catchsql SQL` body (e.g. window1 2.x, tkt-bd484a090c 1.x): the
	// do_test value is the catchsql success/error marker, so run the SQL and
	// compare via emitCatchSQLComparison (which mirrors do_catchsql_test).
	if tp.emitDoTestCatchsqlBody(nameExpr, expectedExpr, bodyCmds, args) {
		return
	}

	// A single `<fixtureProc> args...` body where the proc has a runtime Go
	// implementation (vtabH 3.1: `sort_files [execsql {...}] true`): the
	// proc's result is the do_test value; run it and compare.
	if tp.emitDoTestUserProcBody(nameExpr, expectedExpr, bodyCmds) {
		return
	}

	tp.emitDoTestTestfixtureBodyDispatch(nameExpr, expectedExpr, bodyCmds, args)
}

// emitDoTestTestfixtureBody handles a do_test whose body opens a testfixture
// and runs a SCRIPT on it (lock2/lock4 multi-process locking tests). The
// result of the body is the result of SCRIPT's last command, which the
// standard emitDoTestBodyComparison machinery computes once SCRIPT has run on
// the fixture connection.
func (tp *transpiler) emitDoTestTestfixtureBodyDispatch(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) {
	if tp.emitDoTestTestfixtureBody(nameExpr, expectedExpr, bodyCmds, args) {
		return
	}
	tp.emitDoTestGeneric(nameExpr, expectedExpr, bodyCmds, args)
}

// emitDoTestCatchsqlBody handles a do_test whose body is a single `catchsql
// SQL` command (e.g. window1's `do_test "2.N" {catchsql $sql}` and
// tkt-bd484a090c's `do_test "1.1" {catchsql { SELECT datetime(...) }}). TCL's
// catchsql returns the marker "0" on success (the do_test expected value) or
// "1 {msg}" on failure; reuse emitCatchSQLComparison, which already encodes
// that marker (success vs error-message forms, variable/regex variants), but
// build a synthetic args slice so its contract (args[1]=SQL word, args[2]=expected
// word) is satisfied.
//
// This replaces the generic body transpilation, which otherwise lowered
// `catchsql $sql` to `_r = tclLIndex("catchsql $sql", "0")` — a literal string
// expression that never executes the SQL, so the comparison (`_r != "0"`) always
// failed. Returns true when the body was handled.
func (tp *transpiler) emitDoTestCatchsqlBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if len(bodyCmds) != 1 || len(bodyCmds[0]) < 2 || bodyCmds[0][0].Text != "catchsql" {
		return false
	}
	sqlWord := bodyCmds[0][1]
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	// Preserve the original expected word's attributes (Quoted/Braced, a leading
	// "$" for a variable form) so emitCatchSQLComparison can dispatch on it.
	expWord := tcl.RawWord{Text: "0"}
	if len(args) >= 3 {
		expWord = args[2]
	}
	synth := []tcl.RawWord{
		{Text: "catchsql"},
		sqlWord,
		expWord,
	}
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	// Honor a trailing connection argument (backup-4.2.2: `catchsql {
	// DETACH aux2 } db2`) with the same resolution processExecSQL uses for
	// `execsql SQL db2` (bodyCmds[0][1:] = [SQL, conn]).
	dbConn := tp.resolveSQLConnection(bodyCmds[0][1:])
	tp.emitCatchSQLComparison(nameExpr, sqlExpr, expectedExpr, synth, dbConn)
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitDoTestCatchsqlLindexBody handles a do_test whose body is
// `lindex [catchsql SQL] 0` (e.g. window1's `do_test 2.$tn {lindex [catchsql
// $sql] 0} 0`, tkt-bd484a090c's datetime variants). TCL's catchsql returns a
// two-element list "{code {message}}"; `lindex ... 0` extracts the code (0 =
// success, 1 = error), which the do_test compares against its expected value.
// The `[catchsql SQL]` is a command substitution stored as a flat word
// (RawWord.Text == "catchsql <SQL>"), so detect the prefix and run the SQL via
// db.Exec, asserting success (expected "0") or an error (any other expected
// code). Returns true when handled.
func (tp *transpiler) emitDoTestCatchsqlLindexBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if len(bodyCmds) != 1 || len(bodyCmds[0]) != 3 {
		return false
	}
	lcmd := bodyCmds[0]
	if lcmd[0].Text != "lindex" || lcmd[2].Text != "0" {
		return false
	}
	inner := strings.TrimSpace(lcmd[1].Text)
	// The [catchsql SQL] is a TCL command substitution; the tcl parser keeps
	// the surrounding brackets in RawWord.Text, so strip them before matching.
	if strings.HasPrefix(inner, "[") && strings.HasSuffix(inner, "]") {
		inner = strings.TrimSpace(inner[1 : len(inner)-1])
	}
	const prefix = "catchsql "
	if !strings.HasPrefix(inner, prefix) {
		return false
	}
	sqlPart := strings.TrimSpace(inner[len(prefix):])
	var sqlWord tcl.RawWord
	switch {
	case strings.HasPrefix(sqlPart, "$"):
		sqlWord = tcl.RawWord{Text: sqlPart, Braced: false}
	case strings.HasPrefix(sqlPart, "{") && strings.HasSuffix(sqlPart, "}"):
		sqlWord = tcl.RawWord{Text: sqlPart[1 : len(sqlPart)-1], Braced: true}
	default:
		sqlWord = tcl.RawWord{Text: sqlPart}
	}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	expectSuccess := true
	if len(args) >= 3 && args[2].Text != "0" {
		expectSuccess = false
	}
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	if expectSuccess {
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("}")
	} else {
		tp.emitLine("if _res.Error == nil {")
		tp.emitLine("\tt.Errorf(\"expected error, got none\\n  sql: %%s\", %s)", sqlExpr)
		tp.emitLine("}")
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitDoTestUserProcBody handles a do_test whose body is a single call to a
// fixture proc with a runtime Go implementation (vtabH 3.1:
// `sort_files [execsql {SELECT ...}] true`). TCL do_test compares the VALUE
// of the body — the proc's result — with the expected argument, so emit the
// runtime registry call into _r and compare it against the expected value
// (which expectedStringExpr renders as the matching callTclUserProc call).
// Returns true when the body was handled.
func (tp *transpiler) emitDoTestUserProcBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) != 1 || len(bodyCmds[0]) == 0 {
		return false
	}
	cmd := bodyCmds[0]
	name := cmd[0].Text
	if !globalUserProcs[name] {
		return false
	}
	args := make([]string, 0, len(cmd)-1)
	for _, w := range cmd[1:] {
		args = append(args, tp.userProcArgExpr(w))
	}
	call := fmt.Sprintf("callTclUserProc(%q", name)
	if len(args) > 0 {
		call += ", " + strings.Join(args, ", ")
	}
	call += ")"
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	tp.emitLine("_r = %s", call)
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	tp.indent--
	tp.emitLine("}")
	return true
}

// userProcArgExpr renders one argument word of a fixture-proc call: a [cmd]
// substitution through the command-expression emitters ([execsql {SQL}]
// yields the flattened query result via tclExecSQL), a $var as its Go
// variable, and anything else as a string literal.
func (tp *transpiler) userProcArgExpr(w tcl.RawWord) string {
	t := strings.TrimSpace(w.Text)
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		return tp.cmdExpr(strings.TrimSpace(t[1 : len(t)-1]))
	}
	if strings.HasPrefix(t, "$") {
		gv := tclVarToGo(strings.TrimPrefix(t, "$"))
		if isValidGoIdent(gv) {
			return gv
		}
	}
	return strconv.Quote(t)
}

// emitDoTestSkippedByBodyKind dispatches a do_test body whose assertion cannot
// be transpiled to the kind-specific skip emitter. Returns true when the body
// was handled (and processDoTest should return). Three kinds are recognized:
//
//   - echo-module ABI probes: the final assertion reads the echo module's
//     internal callback log ($echo_module Tcl variable, populated by the
//     test-only C echo module in src/test8.c) and probes the C module ABI
//     (xFilter/xCreate string logging). Frigolite's echo module is
//     engine-implemented and does not expose such a log. Emit the SQL side
//     effects (the setup CREATEs matter for later tests) but skip the
//     C-ABI assertion.
//
//   - CLI shell subprocess invocations (catchcmd / catchcmdex): the body
//     exercises shell.c behaviors — command-line option parsing, .import,
//     .dump, .schema, .lint, .clone, .open, .mode, etc. The transpiler
//     cannot reproduce the subprocess's file/DB manipulation (the shell
//     creates and imports the database file), and later statements in the
//     same body depend on those effects (e.g. `sqlite3 db test.db` then
//     `db eval {SELECT ...}` after an .import). Emit the whole body as a
//     comment: running only the SQL parts would assert against missing state.
//
//   - VDBE-internal state (statement journal usage, prepared-statement
//     stepping): the commands are emitted as comments, but the assertion
//     would compare the LAST sqlite3_exec result against a boolean/state
//     value that has no SQL equivalent. Emit the SQL side effects (db
//     eval/execsql run, and prepared-statement binds are emulated as
//     INSERTs) so later tests see the same database state, but skip the
//     meaningless assertion.
func (tp *transpiler) emitDoTestSkippedByBodyKind(nameExpr string, bodyCmds [][]tcl.RawWord) bool {
	if bodyCmds == nil {
		return false
	}
	if doTestBodyReadsEchoModule(bodyCmds) {
		tp.emitDoTestSideEffects(nameExpr, bodyCmds, "echo module callback log is C test-module ABI; SQL side effects only")
		return true
	}
	if doTestBodyHasShellCommand(bodyCmds) {
		tp.emitDoTestShellSkipped(nameExpr, bodyCmds)
		return true
	}
	if doTestBodyUnsupported(bodyCmds) {
		tp.emitDoTestSideEffects(nameExpr, bodyCmds, "prepare-step internals; SQL side effects only")
		return true
	}
	return false
}

// runDoTestBody transpiles a do_test body in a fresh sub-transpiler (sharing
// the output buffer and state), then copies the sub-transpiler's state back
// into tp exactly as the original inline blocks did. It returns the body's
// final preparedState so callers can decide whether to propagate it (the
// original echo/unsupported paths did; the generic path did not).
func (tp *transpiler) runDoTestBody(bodyCmds [][]tcl.RawWord) *preparedState {
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
		unsetVars:     tp.unsetVars,
		dbVarFuncs:    tp.dbVarFuncs,
		constFuncs:    tp.constFuncs,
		identityFuncs: tp.identityFuncs,
		predFuncs:     tp.predFuncs,
		queryFuncs:    tp.queryFuncs,
		specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
		colmetaCmds:         tp.colmetaCmds,
		rangeListFuncs:      tp.rangeListFuncs,
		collateDtorVars:     tp.collateDtorVars,
		collateGoFuncs:      tp.collateGoFuncs,
		testPrefix:          tp.testPrefix,
		queryVars:           tp.queryVars,
		dbAliases:           tp.dbAliases,
		dbClosed:            tp.dbClosed,
		fixtureVar:          tp.fixtureVar,
		dqsDDL:              tp.dqsDDL,
		dqsDML:              tp.dqsDML,
		preparedState:       tp.preparedState,
		prepareTailVars:     tp.prepareTailVars,
		connFailedOpen:      tp.connFailedOpen,
		connClosed:          tp.connClosed,
		authTypeName:        tp.authTypeName,
		authProcCount:       tp.authProcCount,
		authProcGo:          tp.authProcGo,
		authPreamble:        tp.authPreamble,
		authCurrentDeclared: tp.authCurrentDeclared,
		testDir:             tp.testDir,
		genesisPreamble:     tp.genesisPreamble,
		ftsBuildPreamble:    tp.ftsBuildPreamble,
		varConstValues:      tp.varConstValues,
		sqlVarValues:        tp.sqlVarValues,
		foreachLitValues:    tp.foreachLitValues,
		varsetLoopVars:      tp.varsetLoopVars,
		dbConnVars:          tp.dbConnVars,
		runtimeConnVars:     tp.runtimeConnVars,
		varRenames:          tp.varRenames,
		blobChans:           tp.blobChans,
		blobChannelVars:     tp.blobChannelVars,
		blobVarNames:        tp.blobVarNames,
		blobSeq:             tp.blobSeq,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.rangeListFuncs = bodyTP.rangeListFuncs
	tp.dbAliases = bodyTP.dbAliases
	tp.queryVars = bodyTP.queryVars
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.authTypeName = bodyTP.authTypeName
	tp.authProcCount = bodyTP.authProcCount
	tp.authProcGo = bodyTP.authProcGo
	tp.authPreamble = bodyTP.authPreamble
	tp.authCurrentDeclared = bodyTP.authCurrentDeclared
	tp.genesisPreamble = bodyTP.genesisPreamble
	tp.ftsBuildPreamble = bodyTP.ftsBuildPreamble
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	// Copy connection-state maps unconditionally so deletes (a reopen clearing
	// a prior closed/failed-open state) propagate to the outer transpiler.
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq
	// NOTE: preparedState is intentionally NOT copied back here. The original
	// generic do_test path copied only the fields above; a preparedState
	// created inside a body stayed local to it. emitDoTestSideEffects copies
	// it back to match the original echo/unsupported paths.
	return bodyTP.preparedState
}

// emitDoTestSideEffects wraps a do_test body whose assertion cannot be
// transpiled, emitting only its SQL side effects.
func (tp *transpiler) emitDoTestSideEffects(nameExpr string, bodyCmds [][]tcl.RawWord, reason string) {
	tp.emitLine("{ // %s (%s)", nameExpr, reason)
	tp.indent++
	tp.preparedState = tp.runDoTestBody(bodyCmds)
	tp.indent--
	tp.emitLine("}")
}

// doTestBodyHasShellCommand reports whether a do_test body invokes the CLI
// shell subprocess (catchcmd / catchcmdex — the TCL test harness's wrapper for
// running the sqlite3 shell binary). Shell tests (TESTRUNNER: shell) drive
// shell.c through this command; its effects (creating/importing DB files,
// running .import/.clone/.open/.lint, parsing command-line options) cannot be
// reproduced by the pure-Go engine, so the whole body — including statements
// that depend on the shell's effects — is emitted as a comment.
func doTestBodyHasShellCommand(bodyCmds [][]tcl.RawWord) bool {
	for _, cmd := range bodyCmds {
		for _, w := range cmd {
			if strings.Contains(w.Text, "catchcmd") {
				return true
			}
		}
	}
	return false
}

// emitDoTestShellSkipped emits a do_test body that invokes the CLI shell
// subprocess as a comment-only block. No SQL side effects are emitted: the
// shell's effects (file creation, imports, clones) cannot be reproduced, and
// dependent statements in the body would assert against missing state.
func (tp *transpiler) emitDoTestShellSkipped(nameExpr string, bodyCmds [][]tcl.RawWord) {
	tp.emitLine("{ // %s (CLI shell subprocess harness, not transpiled)", nameExpr)
	tp.indent++
	for _, cmd := range bodyCmds {
		joined := ""
		for _, w := range cmd {
			if joined != "" {
				joined += " "
			}
			joined += w.Text
		}
		tp.emitLine("// %s", sanitizeTCLComment(joined))
	}
	tp.indent--
	tp.emitLine("}")
}

// isCommentOnlyBody reports whether a do_test's braced body contains only TCL
// comments (parseCommands returns nil for it).
func isCommentOnlyBody(args []tcl.RawWord) bool {
	if len(args) < 2 || !args[1].Braced || strings.TrimSpace(args[1].Text) == "" {
		return false
	}
	for _, line := range strings.Split(args[1].Text, "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

// emitLimitComparison handles a single `sqlite3_limit db LIMIT -1` do_test
// body. Returns true when handled.
func (tp *transpiler) emitLimitComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 4 &&
		bodyCmds[0][0].Text == "sqlite3_limit" &&
		strings.TrimSpace(bodyCmds[0][3].Text) == "-1") {
		return false
	}
	limitName := strings.TrimSpace(bodyCmds[0][2].Text)
	tp.emitLine("{ // do_test %s (sqlite3_limit %s -1)", nameExpr, limitName)
	tp.indent++
	tp.emitLine("got := db.Limit(%q)", limitName)
	tp.emitLine("if strconv.Itoa(got) != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"limit mismatch\\n  got:  [%%d]\\n  want: [%%s]\\n  body: do_test %%s\", got, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitBareFileSizeComparison handles a single `file size PATH` do_test body
// (extension01 1.5): compare the current file size against the expected value.
// Returns true when handled.
func (tp *transpiler) emitBareFileSizeComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 2 &&
		bodyCmds[0][0].Text == "file" && bodyCmds[0][1].Text == "size") {
		return false
	}
	pathWord := tcl.RawWord{Text: bodyCmds[0][2].Text}
	pathExpr := tp.goStringLiteral(pathWord)
	tp.emitLine("{ // do_test %s (file size %s)", nameExpr, pathWord.Text)
	tp.indent++
	tp.emitLine("got := strconv.Itoa(tclFileSize(%s))", pathExpr)
	tp.emitLine("if got != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", got, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitLSortComparison handles a single `lsort [-integer] $VAR` do_test body.
// Returns true when handled.
func (tp *transpiler) emitLSortComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 2 && bodyCmds[0][0].Text == "lsort") {
		return false
	}
	sortListVar := ""
	for _, w := range bodyCmds[0][1:] {
		if strings.HasPrefix(w.Text, "$") && len(w.Text) > 1 {
			sortListVar = strings.TrimPrefix(w.Text, "$")
			break
		}
	}
	if sortListVar == "" {
		return false
	}
	goVar := tclVarToGo(sortListVar)
	// lsort -integer sorts numerically; a plain lsort sorts as text.
	sortMode := ""
	for _, w := range bodyCmds[0][1:] {
		if w.Text == "-integer" {
			sortMode = "int"
			break
		}
	}
	sortFn := "tclSort"
	if sortMode == "int" {
		sortFn = "tclSortInt"
	}
	tp.emitLine("{ // do_test %s (lsort %s)", nameExpr, sortListVar)
	tp.indent++
	tp.emitLine("got := %s(%s)", sortFn, goVar)
	tp.emitLine("want := %s", expectedExpr)
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", got, want, %s)", nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitDBEvalComparison handles a single `db eval { SQL }` do_test body (query
// → flatten → compare). Returns true when handled.
func (tp *transpiler) emitDBEvalComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if !(len(bodyCmds) == 1 && len(bodyCmds[0]) >= 3 &&
		bodyCmds[0][0].Text == "db" && bodyCmds[0][1].Text == "eval") {
		return false
	}
	sqlExpr := tp.collectSQLExpression(bodyCmds[0][2:3])
	sql := bodyCmds[0][2].Text
	lastStmt := lastStatementSQL(sql)
	isQuery := isQueryStmt(lastStmt)
	// A db eval whose SQL is a bare variable reference (e.g. `db eval $sql`
	// in a foreach loop) cannot be classified statically. Most such bodies in
	// do_test are SELECTs whose result is compared to the expected value, so
	// default to the query path.
	if !isQuery && strings.HasPrefix(strings.TrimSpace(sql), "$") {
		isQuery = true
	}

	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	if isQuery && expectedExpr != `""` {
		tp.emitDBEvalQueryResult(nameExpr, expectedExpr, sqlExpr, args)
	} else if isQuery {
		tp.emitLine("r = db.Query(%s)", sqlExpr)
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("}")
	} else {
		if isBareGoIdent(expectedExpr) {
			// The expected value is a variable holding an error message
			// (e.g. foreach $error in "13.2.$tn.1"): the statement must fail
			// with that message.
			tp.emitLine("_res = db.Exec(%s)", sqlExpr)
			tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", expectedExpr)
			tp.emitLine("\tt.Errorf(\"expected error containing %%s, got: %%v\\n  sql: %%s\", %s, _res.Error, %s)", expectedExpr, sqlExpr)
			tp.emitLine("}")
		} else {
			tp.emitLine("_res = db.Exec(%s)", sqlExpr)
			tp.emitLine("if _res.Error != nil {")
			tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
			tp.emitLine("}")
		}
	}
	tp.indent--
	tp.emitLine("}")
	return true
}

// emitDBEvalQueryResult emits the query-result comparison for a single
// `db eval` do_test body with a non-empty expected value.
func (tp *transpiler) emitDBEvalQueryResult(nameExpr, expectedExpr, sqlExpr string, args []tcl.RawWord) {
	tp.emitLine("r = db.Query(%s)", sqlExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := flatten(r)")
	if isTCLRegexPattern(expectedExpr) {
		negated := regexPatternNegated(expectedExpr)
		inner := regexPatternInner(expectedExpr)
		if strings.HasPrefix(inner, "*") {
			// TCL glob (string match) — inner starts with * (mirrors TCL
			// do_test branch: "if {[string index $re 0]==\"*\"} ...")
			globExpr := fmt.Sprintf("%q", inner)
			if negated {
				tp.emitLine("wantGlob := %s", globExpr)
				tp.emitLine("if globMatch(got, wantGlob) {")
				tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  must not match glob: [%%s]\", got, wantGlob)")
				tp.emitLine("}")
			} else {
				tp.emitLine("wantGlob := %s", globExpr)
				tp.emitLine("if !globMatch(got, wantGlob) {")
				tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want glob: [%%s]\", got, wantGlob)")
				tp.emitLine("}")
			}
			return
		}
		patternExpr := regexPatternExpr(expectedExpr)
		tp.emitLine("wantPattern := %s", patternExpr)
		if negated {
			// "~/.../" — the pattern must NOT match.
			tp.emitLine("if matched, _ := regexp.MatchString(wantPattern, got); matched {")
			tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  must not match pattern: [%%s]\", got, wantPattern)")
			tp.emitLine("}")
		} else {
			tp.emitLine("if matched, _ := regexp.MatchString(wantPattern, got); !matched {")
			tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want pattern: [%%s]\", got, wantPattern)")
			tp.emitLine("}")
		}
		return
	}
	if dbEvalSQL, isSubst, quoted, ok := dbEvalExpected(args[2]); ok {
		// [db eval { SQL }] or [db eval [subst -novar { SQL }]] —
		// render $var/[cmd] refs as a Go string expression (double-
		// quoted substitutes $var as RAW TEXT).
		dbEvalExpr := fmt.Sprintf("%q", dbEvalSQL)
		if hasVarRef(dbEvalSQL) {
			if isSubst {
				dbEvalExpr = tp.renderSubstNovarSQL(dbEvalSQL)
			} else if quoted {
				dbEvalExpr = tp.buildStringExpr(dbEvalSQL)
			} else {
				dbEvalExpr = tp.buildSQLStringExpr(dbEvalSQL)
			}
		}
		wantVar := fmt.Sprintf("_want%d", tp.varCount)
		tp.varCount++
		tp.emitLine("%s := db.Query(%s)", wantVar, dbEvalExpr)
		tp.emitLine("if %s.Error != nil {", wantVar)
		tp.emitLine("\tt.Errorf(\"expected query error: %%v\\n  sql: %%s\", %s.Error, %s)", wantVar, dbEvalExpr)
		tp.emitLine("\treturn")
		tp.emitLine("}")
		tp.emitLine("want := flatten(%s)", wantVar)
		tp.emitLine("if got != want {")
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
		tp.emitLine("}")
		return
	}
	// Normalize TCL list variable expectations (see processDoExecSQLTest).
	if isBareGoIdent(expectedExpr) || strings.Contains(expectedExpr, `\n`) {
		tp.emitLine("want := tclListFlattenCollapse(%s)", expectedExpr)
		// Normalize the actual result identically: cells may carry embedded
		// newlines (rtreecheck reports), which collapse to single spaces.
		tp.emitLine("got = tclListFlattenCollapse(got)")
	} else {
		tp.emitLine("want := %s", expectedExpr)
	}
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// emitDoTestGeneric handles a multi-command (or string-bodied) do_test: wrap
// the body, transpile it, then compare its value with the expected argument.
func (tp *transpiler) emitDoTestGeneric(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) {
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++

	if bodyCmds != nil {
		tp.runDoTestBody(bodyCmds)
		// A multi-command body whose expected value is a variable holding an
		// error message (e.g. foreach $error in "13.2.$tn.1"): the last
		// statement must fail with that message. When the body is a catchsql
		// command, the expected value is a TCL {count message} list (e.g.
		// "1 {FOREIGN KEY constraint failed}" or "0 {}"), so use the
		// count-aware runtime comparison.
		tp.emitDoTestBodyComparison(nameExpr, expectedExpr, bodyCmds)
	} else {
		tp.emitDoTestStringBody(nameExpr, expectedExpr, bodyCmds, args)
	}

	tp.indent--
	tp.emitLine("}")
}

// emitDoTestBodyComparison emits the expected-value comparison for a
// multi-command do_test body, dispatching on the body's final command shape.
func (tp *transpiler) emitDoTestBodyComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) {
	// A body ending in `expr {$a==$b}` compares two TCL variables; its
	// expected value is a literal boolean (dataversion1.test's dv1/dv2
	// checks: expected "0" or "1"), so handle it before the bare-ident gate.
	if bodyEndsWithExprCompare(bodyCmds) {
		tp.emitExprCompareCheck(nameExpr, expectedExpr, bodyCmds)
		return
	}
	// A body ending in a backup/errmsg/sqlite3_exec/file-size command leaves
	// its value in _r; the expected value may be a TCL list (badutf.test's
	// "{0 {x 80}}" sqlite3_exec results), so handle it before the gate.
	if bodyEndsWithBackupResult(bodyCmds) {
		if bodyEndsWithSqlite3Exec(bodyCmds) {
			tp.emitSqlite3ExecResultCheck(nameExpr, expectedExpr)
			return
		}
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	// A body ending in `lindex ...` extracts a value from a list variable
	// (badutf2.test's `lindex [lindex $res 1] 1`); the lindex result was left
	// in _r. The expected value is a scalar literal, so handle it before the
	// bare-ident gate.
	if bodyEndsWithLindex(bodyCmds) {
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	// A body ending in `set VAR` compares the variable's value (a TCL list,
	// e.g. e_changes.test's `set ::changes` vs "{update 2 trigger 3 ...}").
	if setVar, ok := bodyEndsWithSetVar(tp, bodyCmds); ok {
		tp.emitSetVarResultCheck(nameExpr, expectedExpr, setVar)
		return
	}
	if !isBareGoIdent(expectedExpr) {
		return
	}
	if bodyIsCatchsqlCommand(bodyCmds) {
		tp.emitCatchsqlResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithQueryFunc(bodyCmds, tp.queryFuncs) {
		// The body ends with a query-proc call (e.g. `execsql {...}
		// signature`); the last command's query result is in `_r` and the
		// expected value is that result list.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithEQP(bodyCmds) {
		// The body ends with `eqp "SQL"` — the EXPLAIN QUERY PLAN detail
		// list is in `_r` and the expected value is that list (e_fkey-26.x).
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if tp.bodyEndsWithExecsqlQuery(bodyCmds) {
		// The body's SQL contains a query; processCommands ran it through
		// db.Query and left the flattened result in `r`. Compare it with the
		// expected variable (a RESULT list, e.g. foreach $t232 in
		// without_rowid4-3.2), not an error message.
		tp.emitExecsqlQueryResultCheck(nameExpr, expectedExpr)
		return
	}
	if tp.bodyEndsWithDBEvalQuery(bodyCmds) {
		// The body's last command is `db eval {SELECT ...}` or `db eval $var`
		// (variable holding query SQL) — its result is the query rows, not an
		// error. Re-run the SELECT as a query and compare the flattened result
		// against the expected value (trans2.test's hash checks, autoindex4's
		// foreach loops).
		tp.emitDBEvalQueryResultCheck(nameExpr, expectedExpr, bodyCmds)
		return
	}
	if bodyEndsWithStringResult(bodyCmds) {
		// The body's last command is a `string map {...} [string tolower $x]`
		// (or similar) chain whose RESULT is the do_test value, not an error.
		// The earlier `set x [...]` commands populated the variables; emit the
		// lowering/mapping comparison here.
		tp.emitStringResultCheck(nameExpr, expectedExpr, bodyCmds)
		return
	}
	if bodyEndsWithStringMatch(bodyCmds) {
		// A body ending in `string match PATTERN STR` — the processStringMatch
		// handler left the "1"/"0" result in `_r`; compare it.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithFileAttributes(bodyCmds) {
		// The body ends with `file attributes PATH -attr` (the one-arg
		// form returns the current value as a string). The whole body
		// runs in the sub-transpiler; the last `file attributes` call
		// leaves its result in `_r`. Compare with the expected value
		// (journal3.test 1.2.x.1: `file attributes test.db -permissions`
		// returns the current Unix mode bits as a perm string).
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithIndexExpr(bodyCmds) {
		// The body ends with `expr {$idx>=0}` after `set idx [lsearch $prg
		// OpenEphemeral]` — compare the search result against the expected
		// boolean (0/1). The lsearch index is >=0 when the opcode was found.
		tp.emitIndexExprCheck(nameExpr, expectedExpr, bodyCmds)
		return
	}
	if sqlText, pattern, ok := lsearchDBEvalExpr(bodyCmds); ok {
		// The body is `expr {[lsearch [db eval {SQL}] PATTERN]>=0}` — assert
		// that PATTERN appears in the db-eval result rows (ctime-3.0.1).
		tp.emitLsearchDBEvalCheck(nameExpr, expectedExpr, sqlText, pattern)
		return
	}
	if bodyEndsWithBackupResult(bodyCmds) {
		// The body's last command is a backup/errmsg/file-size command; its
		// value was left in `_r` by the transpiled handler. Compare it with
		// the expected value.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithStmtMetadata(bodyCmds) {
		// The body's last command is a prepared-statement metadata query or
		// step; its value was left in `_r` by the runtime Stmt helpers.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithBlobResult(bodyCmds) {
		// The body's last command is an incremental-blob command; its value
		// was left in `_r` by the transpiled handler. Compare it with the
		// expected value.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithListResult(bodyCmds) {
		// The body's last command is a `list ...` whose value (including a
		// `[catch {...} VAR]` argument) was left in `_r`. Compare it with the
		// expected value.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	if bodyEndsWithExprResult(bodyCmds) {
		// The body ends with `expr [cmd ...] OP N` — processExpr resolved the
		// command and left the TCL truth string in `_r` (dbstatus.test
		// 5.5.x `expr [sqlite3_stmt_status ...]>0`). Compare it directly.
		tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
		return
	}
	tp.emitErrorResultCheck(nameExpr, expectedExpr)
}

// bodyEndsWithFileAttributes reports whether the do_test body's last command
// is `file attributes PATH -ATTR` (the value-returning form, not the
// setter form `file attributes PATH -ATTR VAL`). journal3.test 1.2.x.1 uses
// this pattern: `file attributes test.db -permissions $perm ; file attributes
// test.db -permissions` to read back the perms.
func bodyEndsWithFileAttributes(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) < 1 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) < 2 {
		return false
	}
	if last[0].Text != "file" || (last[1].Text != "attributes" && last[1].Text != "attr") {
		return false
	}
	// file attributes PATH -ATTR      → 4 words: file attributes PATH -ATTR
	// file attributes PATH -ATTR VAL  → 5 words (setter, no return value)
	if len(last) == 5 {
		return false
	}
	return true
}

// bodyIsCatchsqlCommand reports whether a do_test body's last command is a
// catchsql command (its expected value is a {count message} list).
func bodyIsCatchsqlCommand(bodyCmds [][]tcl.RawWord) bool {
	return len(bodyCmds) >= 1 && len(bodyCmds[len(bodyCmds)-1]) >= 1 && bodyCmds[len(bodyCmds)-1][0].Text == "catchsql"
}

// bodyEndsWithExecsqlQuery reports whether a do_test body ends with (or is a
// single) execsql command whose SQL contains a query.
func (tp *transpiler) bodyEndsWithExecsqlQuery(bodyCmds [][]tcl.RawWord) bool {
	// A single `execsql {SQL}` body whose SQL contains a query returns the
	// flattened query results (e.g. foreach $t232 in without_rowid4-3.2).
	if len(bodyCmds) == 1 && len(bodyCmds[0]) >= 2 && bodyCmds[0][0].Text == "execsql" {
		if bodySQLContainsQuery(bodyCmds[0][1].Text) {
			return true
		}
	}
	// Multi-command bodies ending in `execsql $var` where $var holds query SQL
	// (e.g. join3's `set sql "SELECT..."; ...; execsql $sql`) or in a braced
	// `execsql {SELECT ...}` query also return the flattened query result.
	if len(bodyCmds) >= 1 {
		lastCmd := bodyCmds[len(bodyCmds)-1]
		if len(lastCmd) >= 2 && lastCmd[0].Text == "execsql" {
			if lastCmd[1].Braced {
				// Braced SQL literal: detect a trailing query statement
				// (e.g. do_test index-3.1 ends with
				// `execsql {SELECT name FROM sqlite_master ...}`).
				return bodySQLContainsQuery(lastCmd[1].Text)
			}
			varName := strings.TrimPrefix(lastCmd[1].Text, "$")
			if tp.queryVars[varName] {
				return true
			}
			// A non-braced execsql whose argument is a literal SQL query (not
			// a $var reference) also returns the flattened query result (e.g.
			// bigrow-2.2's `execsql "SELECT b FROM t1 WHERE a=='abc'"`).
			if !strings.HasPrefix(strings.TrimSpace(lastCmd[1].Text), "$") {
				return bodySQLContainsQuery(tclUnescapeQuoted(lastCmd[1].Text))
			}
		}
	}
	return false
}

// bodySQLContainsQuery reports whether a SQL text ends with a query statement
// (a statement that produces result rows).
func bodySQLContainsQuery(sqlText string) bool {
	for _, stmt := range strings.Split(sqlText, ";") {
		if isQueryStmt(lastStatementSQL(strings.TrimSpace(stmt))) {
			return true
		}
	}
	return false
}

// bodyEndsWithDBEvalQuery reports whether a do_test body's last command is
// `db eval {SELECT ...}` or `db eval $var` (variable holding query SQL).
func (tp *transpiler) bodyEndsWithDBEvalQuery(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) < 1 {
		return false
	}
	lastCmd := bodyCmds[len(bodyCmds)-1]
	if len(lastCmd) < 3 || lastCmd[0].Text != "db" || lastCmd[1].Text != "eval" {
		return false
	}
	sqlText := lastCmd[2].Text
	// `db eval $var` (a variable reference): treat as a query when the
	// variable was assigned query SQL (tracked by markQueryVar), e.g.
	// autoindex4's `set sql "SELECT * ..."; ... db eval $sql`.
	if strings.HasPrefix(strings.TrimSpace(sqlText), "$") {
		varName := strings.TrimPrefix(strings.TrimSpace(sqlText), "$")
		return tp.queryVars[varName]
	}
	return bodySQLContainsQuery(sqlText)
}

// emitCatchsqlResultCheck emits a catchsql count-aware comparison.
func (tp *transpiler) emitCatchsqlResultCheck(nameExpr, expectedExpr string) {
	tp.emitLine("if !tclCatchsqlMatches(_res, %s) {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"catchsql mismatch\\n  got:  [%%v]\\n  want: [%%s]\\n  body: do_test %%s\", _res.Error, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
}

// emitSetVarResultCheck emits a comparison of a `set VAR`-ending body. The
// variable holds a TCL list (e.g. e_changes.test's ::changes), so compare the
// flattened forms to ignore list-rendering braces. When VAR is a
// sqlite3_prepare TAIL variable (capi2-2.x), compare the collapsed forms: the
// C-API tail pointer and the TCL braced expected may differ in leading/trailing
// whitespace.
func (tp *transpiler) emitSetVarResultCheck(nameExpr, expectedExpr, setVar string) {
	if tp.prepareTailVars[setVar] {
		tp.emitLine("got := tclListFlattenCollapse(%s)", setVar)
		tp.emitLine("want := tclListFlattenCollapse(%s)", expectedExpr)
	} else {
		tp.emitLine("got := tclListFlatten(%s)", setVar)
		tp.emitLine("want := tclListFlatten(%s)", expectedExpr)
	}
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", got, want, %s)", nameExpr)
	tp.emitLine("}")
}

// emitQueryFuncResultCheck emits a comparison of a query-proc-ending body.
func (tp *transpiler) emitQueryFuncResultCheck(nameExpr, expectedExpr string) {
	tp.emitLine("if _r != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", _r, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
}

// emitSqlite3ExecResultCheck emits a comparison of a body ending in
// `sqlite3_exec db {SQL}`: the harness result "{code {headers values}}" is a
// TCL list, so compare the flattened forms (the expected value's rendering
// braces are normalized away by the transpiler).
func (tp *transpiler) emitSqlite3ExecResultCheck(nameExpr, expectedExpr string) {
	tp.emitLine("if tclListFlatten(_r) != tclListFlatten(%s) {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", _r, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
}

// emitExecsqlQueryResultCheck emits a comparison of an execsql-query body.
func (tp *transpiler) emitExecsqlQueryResultCheck(nameExpr, expectedExpr string) {
	// Normalize the expected value through tclListFlatten so empty TCL
	// lists (raw "" after an lreplace that removed the last element)
	// match flatten()'s "{}" rendering of an empty SELECT result.
	tp.emitLine("if flatten(r) != tclListFlatten(%s) {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_test %%s\", flatten(r), tclListFlatten(%s), %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
}

// emitDBEvalQueryResultCheck emits a comparison of a db-eval-query body.
