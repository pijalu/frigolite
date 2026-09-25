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

	expectedExpr := tp.resolveDoTestExpected(args)

	// TCL do_test compares the VALUE of the body script with the expected
	// argument. The kind handlers below each recognize one specific body
	// shape (a single sqlite3_limit / lsort / db eval / file size /
	// catchsql / execsql command, or a fixture-proc call) and emit a real
	// result comparison for it.
	for _, handle := range doTestBodyKindHandlers {
		if handle(tp, nameExpr, expectedExpr, bodyCmds, args) {
			return
		}
	}

	tp.emitDoTestTestfixtureBodyDispatch(nameExpr, expectedExpr, bodyCmds, args)
}

// doTestBodyKindHandler transpiles one recognized do_test body shape. It
// returns true when the body was handled (and processDoTest should stop).
type doTestBodyKindHandler = func(*transpiler, string, string, [][]tcl.RawWord, []tcl.RawWord) bool

// doTestBodyKindHandlers lists the single-command do_test body shapes in the
// order processDoTest must try them (first match wins).
var doTestBodyKindHandlers = []doTestBodyKindHandler{
	// A single `sqlite3_limit db LIMIT -1` body queries the current limit;
	// the expected value is the limit number (e.g. attach4-1.1 expects
	// $SQLITE_MAX_ATTACHED). Emit a direct value comparison.
	(*transpiler).doTestHandleLimitComparison,
	// A single `lsort -integer $VAR` body sorts a TCL list variable (the
	// result of an earlier `set VAR [db eval ...]`) and compares it to the
	// expected value (rowvalue4 2.1.x). The variable holds a space-separated
	// list of query result cells.
	(*transpiler).doTestHandleLSortComparison,
	// The most common body form is a single `db eval { SQL }` command;
	// transpile it with a real result comparison (query → flatten →
	// compare), matching do_execsql_test semantics.
	(*transpiler).emitDBEvalComparison,
	// A single `file size PATH` body (extension01 1.5): compare the current
	// file size against the expected value.
	(*transpiler).doTestHandleBareFileSizeComparison,
	// A single `lindex [catchsql SQL] 0` body (e.g. window1 2.x,
	// tkt-bd484a090c 1.x): the do_test value is the catchsql success/error
	// code, so run the SQL and compare (success when expected "0").
	(*transpiler).emitDoTestCatchsqlLindexBody,
	// A single `catchsql SQL` body (e.g. window1 2.x, tkt-bd484a090c 1.x):
	// the do_test value is the catchsql success/error marker, so run the
	// SQL and compare via emitCatchSQLComparison (which mirrors
	// do_catchsql_test).
	(*transpiler).emitDoTestCatchsqlBody,
	// A single `execsql SQL` body whose SQL is a query (fts5simple
	// 11.2/11.3: `do_test 11.3 { execsql "SELECT ..." } {2}`): the do_test
	// value is the flattened query result, so compare it with the expected
	// value.
	(*transpiler).emitDoTestExecsqlCommandBody,
	// A single `<fixtureProc> args...` body where the proc has a runtime Go
	// implementation (vtabH 3.1: `sort_files [execsql {...}] true`): the
	// proc's result is the do_test value; run it and compare.
	(*transpiler).doTestHandleUserProcBody,
}

// doTestHandleLimitComparison adapts emitLimitComparison (which takes no
// trailing args) to the doTestBodyKindHandler signature.
func (tp *transpiler) doTestHandleLimitComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, _ []tcl.RawWord) bool {
	return tp.emitLimitComparison(nameExpr, expectedExpr, bodyCmds)
}

// doTestHandleLSortComparison adapts emitLSortComparison (which takes no
// trailing args) to the doTestBodyKindHandler signature.
func (tp *transpiler) doTestHandleLSortComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, _ []tcl.RawWord) bool {
	return tp.emitLSortComparison(nameExpr, expectedExpr, bodyCmds)
}

// doTestHandleBareFileSizeComparison adapts emitBareFileSizeComparison (which
// takes no trailing args) to the doTestBodyKindHandler signature.
func (tp *transpiler) doTestHandleBareFileSizeComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, _ []tcl.RawWord) bool {
	return tp.emitBareFileSizeComparison(nameExpr, expectedExpr, bodyCmds)
}

// doTestHandleUserProcBody adapts emitDoTestUserProcBody (which takes no
// trailing args) to the doTestBodyKindHandler signature.
func (tp *transpiler) doTestHandleUserProcBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, _ []tcl.RawWord) bool {
	return tp.emitDoTestUserProcBody(nameExpr, expectedExpr, bodyCmds)
}

// resolveDoTestExpected renders the do_test expected argument (args[2]) as a
// Go expression. A hexio read (db-header access) is returned as the second
// value: TCL evaluates the expected argument BEFORE the body runs, so it
// must be hoisted into a statement ahead of the transpiled body rather than
// inlined at comparison time.
func (tp *transpiler) resolveDoTestExpected(args []tcl.RawWord) string {
	expectedExpr := `""`
	hoistGoExpr := ""
	if len(args) >= 3 {
		if expr, ok := tp.expectedStringExpr(args[2]); ok {
			expectedExpr = expr
		} else if goExpr, ok := tp.hoistedHexioReadExpr(args[2]); ok {
			// The expected argument reads the db header via hexio; TCL
			// evaluates it before the body runs, so it is hoisted (below)
			// rather than inlined at comparison time.
			hoistGoExpr = goExpr
		} else {
			expectedExpr = tp.expectLiteral(args[2])
		}
	}
	if ov := wantOverride(overrideFile(tp), args[0].Text); ov != "" {
		expectedExpr = tp.expectLiteral(tcl.RawWord{Text: ov, Braced: true})
		hoistGoExpr = ""
	}
	if hoistGoExpr != "" {
		hoistVar := fmt.Sprintf("_wantBase%d", tp.wantHoistCount)
		tp.wantHoistCount++
		tp.emitLine("%s := %s", hoistVar, hoistGoExpr)
		expectedExpr = hoistVar
	}
	return expectedExpr
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

// emitDoTestExecsqlCommandBody handles a do_test whose body is a single `execsql
// SQL` command whose SQL ends with a query (fts5simple 11.2/11.3:
// `do_test 11.3 { execsql "SELECT rowid FROM t4('d\x1A')" } {2}`). TCL's
// do_test compares the flattened result rows with the expected value, so the
// same comparison as do_execsql_test is emitted. Returns true when handled.
// Non-query bodies keep the generic exec-only lowering.
func (tp *transpiler) emitDoTestExecsqlCommandBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if len(bodyCmds) != 1 || len(bodyCmds[0]) < 2 || bodyCmds[0][0].Text != "execsql" {
		return false
	}
	sqlWord := bodyCmds[0][1]
	if !bodySQLContainsQuery(sqlWord.Text) {
		return false
	}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	dbConn := tp.resolveSQLConnection(bodyCmds[0][1:])
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	synth := []tcl.RawWord{{Text: sqlWord.Text, Braced: true}, {Text: sqlWord.Text, Braced: true}}
	if len(args) >= 3 {
		synth = append(synth, args[2])
	} else {
		synth = append(synth, tcl.RawWord{Text: "{}", Braced: true})
	}
	tp.emitExpectedQueryResult(dbConn, sqlExpr, expectedExpr, synth)
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
// catchsqlLindexInner extracts the inner `catchsql <SQL>` text from an
// `lindex [catchsql SQL] 0` body word. The [catchsql SQL] is a TCL command
// substitution; the tcl parser keeps the surrounding brackets in
// RawWord.Text, so strip them before matching. Returns ok=false when the
// word is not a catchsql substitution.
func catchsqlLindexInner(w tcl.RawWord) (string, bool) {
	inner := strings.TrimSpace(w.Text)
	if strings.HasPrefix(inner, "[") && strings.HasSuffix(inner, "]") {
		inner = strings.TrimSpace(inner[1 : len(inner)-1])
	}
	const prefix = "catchsql "
	if !strings.HasPrefix(inner, prefix) {
		return "", false
	}
	return strings.TrimSpace(inner[len(prefix):]), true
}

// catchsqlLindexSQLWord builds the SQL word for an `lindex [catchsql SQL] 0`
// body from the raw SQL part: `$var` stays unbraced, `{...}` loses its outer
// brace layer, anything else passes through verbatim.
func catchsqlLindexSQLWord(sqlPart string) tcl.RawWord {
	switch {
	case strings.HasPrefix(sqlPart, "$"):
		return tcl.RawWord{Text: sqlPart, Braced: false}
	case strings.HasPrefix(sqlPart, "{") && strings.HasSuffix(sqlPart, "}"):
		return tcl.RawWord{Text: sqlPart[1 : len(sqlPart)-1], Braced: true}
	default:
		return tcl.RawWord{Text: sqlPart}
	}
}

// emitCatchsqlLindexAssert emits the SQL execution plus the success/error
// assertion of an `lindex [catchsql SQL] 0` body (success when expected "0").
func (tp *transpiler) emitCatchsqlLindexAssert(nameExpr, sqlExpr string, expectSuccess bool) {
	tp.emitLine("{ // do_test %s", nameExpr)
	tp.indent++
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	if expectSuccess {
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", resErrString(_res), %s)", sqlExpr)
		tp.emitLine("}")
	} else {
		tp.emitLine("if _res.Error == nil {")
		tp.emitLine("\tt.Errorf(\"expected error, got none\\n  sql: %%s\", %s)", sqlExpr)
		tp.emitLine("}")
	}
	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) emitDoTestCatchsqlLindexBody(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord, args []tcl.RawWord) bool {
	if len(bodyCmds) != 1 || len(bodyCmds[0]) != 3 {
		return false
	}
	lcmd := bodyCmds[0]
	if lcmd[0].Text != "lindex" || lcmd[2].Text != "0" {
		return false
	}
	sqlPart, ok := catchsqlLindexInner(lcmd[1])
	if !ok {
		return false
	}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{catchsqlLindexSQLWord(sqlPart)})
	expectSuccess := true
	if len(args) >= 3 && args[2].Text != "0" {
		expectSuccess = false
	}
	tp.emitCatchsqlLindexAssert(nameExpr, sqlExpr, expectSuccess)
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

// runDoTestBody transpiles a do_test body in a fresh sub-transpiler (sharing
// the output buffer and state), then copies the sub-transpiler's state back
// into tp exactly as the original inline blocks did. It returns the body's
// final preparedState so callers can decide whether to propagate it (the
// original echo/unsupported paths did; the generic path did not).
func (tp *transpiler) runDoTestBody(bodyCmds [][]tcl.RawWord) *preparedState {
	bodyTP := &transpiler{
		sb:           tp.sb,
		indent:       tp.indent,
		dbVar:        tp.dbVar,
		t:            tp.t,
		catchMode:    tp.catchMode,
		varCount:     tp.varCount,
		vars:         tp.vars,
		arrayKeys:    tp.arrayKeys,
		arrayMapVars: tp.arrayMapVars,
		forIncrs:     tp.forIncrs,
		unsetVars:    tp.unsetVars,
		dbVarFuncs:   tp.dbVarFuncs,
		constFuncs:   tp.constFuncs, quotaCallbacks: tp.quotaCallbacks,
		inlineProcs: tp.inlineProcs, inlineProcParams: tp.inlineProcParams,
		identityFuncs: tp.identityFuncs,
		predFuncs:     tp.predFuncs,
		queryFuncs:    tp.queryFuncs,
		specialFuncs:  tp.specialFuncs, procStringMaps: tp.procStringMaps,
		colmetaCmds:         tp.colmetaCmds,
		rangeListFuncs:      tp.rangeListFuncs,
		collateDtorVars:     tp.collateDtorVars,
		collateGoFuncs:      tp.collateGoFuncs,
		collateEmittedProcs: tp.collateEmittedProcs,
		procBodies:          tp.procBodies,
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

// limitIdentSplitRune reports whether r is a TCL variable-identifier
// SEPARATOR (anything other than letters, digits, underscore, namespace
// separator) — the FieldsFunc split predicate for limit tokenization.
func limitIdentSplitRune(r rune) bool {
	return !(r == '_' || r == ':' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
}

// limitExprVarNames collects the distinct SQLITE_MAX_* variable names
// referenced by a sqlite3_limit SET expression, in first-appearance order.
func limitExprVarNames(inner string) []string {
	varNames := []string{}
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(inner, limitIdentSplitRune) {
		t := strings.Trim(tok, ":")
		if !strings.HasPrefix(t, "SQLITE_MAX_") || seen[t] {
			continue
		}
		seen[t] = true
		varNames = append(varNames, t)
	}
	return varNames
}

// limitExprRuntimeExpr renders a `[expr {...}]` sqlite3_limit SET value as a
// runtime tclExprWith call so the helper-test constants divide at runtime.
// Returns ok=false when the expression references no SQLITE_MAX_* variable
// (the caller falls back to limitValueExpr).
func (tp *transpiler) limitExprRuntimeExpr(rawVal string) (string, bool) {
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rawVal, "[expr "), "]"))
	inner = strings.Trim(inner, "{}")
	varNames := limitExprVarNames(inner)
	if len(varNames) == 0 {
		return "", false
	}
	pairs := make([]string, 0, len(varNames)*2)
	for _, v := range varNames {
		pairs = append(pairs, fmt.Sprintf("%q: %s", v, v))
	}
	return fmt.Sprintf("tclExprWith(%q, map[string]string{%s})", inner, strings.Join(pairs, ", ")), true
}

// limitSetRuntimeExpr renders a sqlite3_limit SET value as a runtime Go
// int expression: [expr {$::SQLITE_MAX_*/2}] becomes
// tclExprWith("$SQLITE_MAX_*/2", map[...]) so the helper-test constants
// divide at runtime; plain values reuse limitValueExpr.
func (tp *transpiler) limitSetRuntimeExpr(rawVal string) string {
	rawVal = strings.TrimSpace(rawVal)
	if strings.HasPrefix(rawVal, "[expr ") {
		if expr, ok := tp.limitExprRuntimeExpr(rawVal); ok {
			return expr
		}
	}
	return tp.limitValueExpr(rawVal)
}

// limitBodyConn resolves the sqlite3_limit connection argument (bodyCmds[0][1]
// — db or db2; sqllimits1-3.x verify the UNTOUCHED db2 connection, so the
// check must read that handle) to the matching Go variable.
func limitBodyConn(bodyCmds [][]tcl.RawWord) string {
	conn := "db"
	if len(bodyCmds) > 0 && len(bodyCmds[0]) >= 2 && bodyCmds[0][0].Text == "sqlite3_limit" {
		if gv := tclVarToGo(strings.TrimSpace(bodyCmds[0][1].Text)); gv == "db" || gv == "db2" {
			conn = gv
		}
	}
	return conn
}

// isLimitSetThenQueryPair reports whether the body is two sqlite3_limit
// commands that SET a limit then QUERY it back (sqllimits1-1.12/1.13 — the
// same limit name in both, query arg -1): the comparison asserts the clamped
// new value.
func isLimitSetThenQueryPair(bodyCmds [][]tcl.RawWord) bool {
	return len(bodyCmds) == 2 && len(bodyCmds[0]) >= 4 && len(bodyCmds[1]) >= 4 &&
		bodyCmds[0][0].Text == "sqlite3_limit" && bodyCmds[1][0].Text == "sqlite3_limit" &&
		strings.TrimSpace(bodyCmds[1][3].Text) == "-1" &&
		strings.TrimSpace(bodyCmds[0][2].Text) == strings.TrimSpace(bodyCmds[1][2].Text)
}

// isLimitSingleSet reports whether the body is a single sqlite3_limit command
// with a non-"-1" value (sqllimits1-2.x — the command returns the PRIOR
// limit, so it must be captured before setting).
func isLimitSingleSet(bodyCmds [][]tcl.RawWord) bool {
	return len(bodyCmds) == 1 && len(bodyCmds[0]) >= 4 &&
		bodyCmds[0][0].Text == "sqlite3_limit" &&
		strings.TrimSpace(bodyCmds[0][3].Text) != "-1"
}

// isLimitSingleQuery reports whether the body is a single sqlite3_limit
// command with a "-1" query value (e.g. attach4-1.1).
func isLimitSingleQuery(bodyCmds [][]tcl.RawWord) bool {
	return len(bodyCmds) == 1 && len(bodyCmds[0]) >= 4 &&
		bodyCmds[0][0].Text == "sqlite3_limit" &&
		strings.TrimSpace(bodyCmds[0][3].Text) == "-1"
}

// emitLimitSetQueryComparison emits the set-then-query form: the value
// compared is the CLAMPED new limit.
func (tp *transpiler) emitLimitSetQueryComparison(nameExpr, expectedExpr, connVar, limitName, setVal string) {
	tp.emitLine("{ // do_test %s (sqlite3_limit %s set+query)", nameExpr, limitName)
	tp.indent++
	tp.emitLine("%s.SetLimit(%q, toInt(%s))", connVar, limitName, tp.limitValueExpr(setVal))
	tp.emitLine("got := %s.Limit(%q)", connVar, limitName)
	tp.emitLine("if strconv.Itoa(got) != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"limit mismatch\\n  got:  [%%d]\\n  want: [%%s]\\n  body: do_test %%s\", got, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// emitLimitSetPriorComparison emits the single-SET form: the value compared
// is the PRIOR limit, captured before the SetLimit call.
func (tp *transpiler) emitLimitSetPriorComparison(nameExpr, expectedExpr, connVar, limitName, rawVal string) {
	tp.emitLine("{ // do_test %s (sqlite3_limit %s set-prior)", nameExpr, limitName)
	tp.indent++
	tp.emitLine("prior := %s.Limit(%q)", connVar, limitName)
	tp.emitLine("%s.SetLimit(%q, toInt(%s))", connVar, limitName, tp.limitSetRuntimeExpr(rawVal))
	tp.emitLine("if strconv.Itoa(prior) != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"limit mismatch\\n  got:  [%%d]\\n  want: [%%s]\\n  body: do_test %%s\", prior, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// emitLimitQueryComparison emits the single-query form (-1): the value
// compared is the current limit.
func (tp *transpiler) emitLimitQueryComparison(nameExpr, expectedExpr, connVar, limitName string) {
	tp.emitLine("{ // do_test %s (sqlite3_limit %s -1)", nameExpr, limitName)
	tp.indent++
	tp.emitLine("got := %s.Limit(%q)", connVar, limitName)
	tp.emitLine("if strconv.Itoa(got) != %s {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"limit mismatch\\n  got:  [%%d]\\n  want: [%%s]\\n  body: do_test %%s\", got, %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// emitLimitComparison handles `sqlite3_limit db LIMIT ...` do_test
// bodies: single `-1` bodies query the current limit (e.g. attach4-1.1);
// single SET bodies (sqllimits1-2.x) return the PRIOR limit, so capture it
// before setting; two-command set-then-query bodies (sqllimits1-1.12/1.13)
// compare the clamped new value. Returns true when handled.
func (tp *transpiler) emitLimitComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	connVar := limitBodyConn(bodyCmds)
	if isLimitSetThenQueryPair(bodyCmds) {
		limitName := strings.TrimSpace(bodyCmds[0][2].Text)
		setVal := strings.TrimSpace(bodyCmds[0][3].Text)
		tp.emitLimitSetQueryComparison(nameExpr, expectedExpr, connVar, limitName, setVal)
		return true
	}
	if isLimitSingleSet(bodyCmds) {
		limitName := strings.TrimSpace(bodyCmds[0][2].Text)
		rawVal := strings.TrimSpace(bodyCmds[0][3].Text)
		tp.emitLimitSetPriorComparison(nameExpr, expectedExpr, connVar, limitName, rawVal)
		return true
	}
	if isLimitSingleQuery(bodyCmds) {
		limitName := strings.TrimSpace(bodyCmds[0][2].Text)
		tp.emitLimitQueryComparison(nameExpr, expectedExpr, connVar, limitName)
		return true
	}
	return false
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
	tp.emitLine("if got != want && !tclFpnumCompare(got, want) {")
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
			tp.emitLine("\tt.Errorf(\"expected error containing %%s, got: %%v\\n  sql: %%s\", %s, resErrString(_res), %s)", expectedExpr, sqlExpr)
			tp.emitLine("}")
		} else {
			tp.emitLine("_res = db.Exec(%s)", sqlExpr)
			tp.emitLine("if _res.Error != nil {")
			tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", resErrString(_res), %s)", sqlExpr)
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
		tp.emitDBEvalRegexWant(expectedExpr)
		return
	}
	if dbEvalSQL, isSubst, quoted, ok := dbEvalExpected(args[2]); ok {
		// [db eval { SQL }] or [db eval [subst -novar { SQL }]] —
		// render $var/[cmd] refs as a Go string expression (double-
		// quoted substitutes $var as RAW TEXT).
		tp.emitDBEvalNestedQueryWant(dbEvalSQL, isSubst, quoted)
		return
	}
	// Normalize TCL list variable expectations (see processDoExecSQLTest).
	tp.emitDBEvalPlainWant(expectedExpr)
}

// emitDBEvalRegexWant emits the comparison of the flattened result against a
// /pattern/ (or ~/pattern/) expected value — a regexp (inverted) match, or a
// TCL glob when the pattern starts with `*` (mirrors the TCL do_test branch:
// "if {[string index $re 0]=="*"} ...").
func (tp *transpiler) emitDBEvalRegexWant(expectedExpr string) {
	negated := regexPatternNegated(expectedExpr)
	inner := regexPatternInner(expectedExpr)
	if strings.HasPrefix(inner, "*") {
		tp.emitDBEvalGlobWant(inner, negated)
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
}

// emitDBEvalGlobWant emits the glob (string match) comparison of the
// flattened result against a `*...` expected pattern.
func (tp *transpiler) emitDBEvalGlobWant(inner string, negated bool) {
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
}

// emitDBEvalNestedQueryWant emits the comparison against a NESTED
// `[db eval { SQL }]` expected value: the expected rows are themselves
// queried at runtime and both sides are flattened. A $var/[cmd] reference in
// the expected SQL is rendered as a Go string expression (double-quoted
// substitutes $var as RAW TEXT).
func (tp *transpiler) emitDBEvalNestedQueryWant(dbEvalSQL string, isSubst, quoted bool) {
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
	tp.emitLine("if got != want && !tclFpnumCompare(got, want) {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// emitDBEvalPlainWant emits the comparison against a literal/list-variable
// expected value. Variable (bare-identifier) expectations and expectations
// carrying embedded newlines are normalized through tclListFlattenCollapse;
// the actual result is normalized identically (cells may carry embedded
// newlines — rtreecheck reports — which collapse to single spaces).
func (tp *transpiler) emitDBEvalPlainWant(expectedExpr string) {
	if isBareGoIdent(expectedExpr) || strings.Contains(expectedExpr, `\n`) {
		tp.emitLine("want := tclListFlattenCollapse(%s)", expectedExpr)
		tp.emitLine("got = tclListFlattenCollapse(got)")
	} else {
		tp.emitLine("want := %s", expectedExpr)
	}
	tp.emitLine("if got != want && !tclFpnumCompare(got, want) {")
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
