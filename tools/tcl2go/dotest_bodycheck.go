// Package main implements the tcl2go tool.
//
// This file holds the do_test body-value comparison dispatch: for a
// multi-command do_test body, emitDoTestBodyComparison walks an ordered
// table of shape checks (doTestBodyChecksPreGate / doTestBodyChecksPostGate)
// and emits the comparison matching the body's final command shape.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// doTestBodyCheckFunc reports whether a do_test body matches one comparison
// shape and, when it does, emits the corresponding comparison. Returns true
// when the body was handled (the dispatch must stop).
type doTestBodyCheckFunc = func(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool

// doTestBodyChecksPreGate lists the shape checks that run before the
// bare-identifier expected gate. Their expected values may be TCL lists
// (badutf.test's "{0 {x 80}}" sqlite3_exec results) or literal booleans, so
// they must be tried before the gate can bail out.
var doTestBodyChecksPreGate = []doTestBodyCheckFunc{
	doTestBodyCheckExprCompare,
	doTestBodyCheckBackupResult,
	doTestBodyCheckLindex,
	doTestBodyCheckSetVar,
	doTestBodyCheckLappendVar,
}

// doTestBodyChecksPostGate lists the shape checks that run once the expected
// value is known to be a bare Go identifier (a variable holding the wanted
// value — usually an error message or result list).
var doTestBodyChecksPostGate = []doTestBodyCheckFunc{
	doTestBodyCheckCatchsql,
	doTestBodyCheckQueryFunc,
	doTestBodyCheckSignature,
	doTestBodyCheckQuotaListSize,
	doTestBodyCheckQuotaValueCmd,
	doTestBodyCheckQuotaGlob,
	doTestBodyCheckEQP,
	(*transpiler).doTestBodyCheckExecsqlQuery,
	(*transpiler).doTestBodyCheckDBEvalQuery,
	doTestBodyCheckStringResult,
	doTestBodyCheckStringMatch,
	doTestBodyCheckFileAttributes,
	doTestBodyCheckIndexExpr,
	doTestBodyCheckLsearchDBEval,
	doTestBodyCheckValueResults,
}

// emitDoTestBodyComparison emits the expected-value comparison for a
// multi-command do_test body, dispatching on the body's final command shape.
func (tp *transpiler) emitDoTestBodyComparison(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) {
	for _, check := range doTestBodyChecksPreGate {
		if check(tp, nameExpr, expectedExpr, bodyCmds) {
			return
		}
	}
	if !isBareGoIdent(expectedExpr) {
		return
	}
	for _, check := range doTestBodyChecksPostGate {
		if check(tp, nameExpr, expectedExpr, bodyCmds) {
			return
		}
	}
	tp.emitErrorResultCheck(nameExpr, expectedExpr)
}

// doTestBodyCheckExprCompare: a body ending in `expr {$a==$b}` compares two
// TCL variables; its expected value is a literal boolean (dataversion1.test's
// dv1/dv2 checks: expected "0" or "1"), so handle it before the bare-ident
// gate.
func doTestBodyCheckExprCompare(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithExprCompare(bodyCmds) {
		return false
	}
	tp.emitExprCompareCheck(nameExpr, expectedExpr, bodyCmds)
	return true
}

// doTestBodyCheckBackupResult: a body ending in a backup/errmsg/sqlite3_exec/
// file-size command leaves its value in _r.
func doTestBodyCheckBackupResult(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithBackupResult(bodyCmds) {
		return false
	}
	if bodyEndsWithSqlite3Exec(bodyCmds) {
		tp.emitSqlite3ExecResultCheck(nameExpr, expectedExpr)
		return true
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckLindex: a body ending in `lindex ...` extracts a value from
// a list variable (badutf2.test's `lindex [lindex $res 1] 1`); the lindex
// result was left in _r and the expected value is a scalar literal.
func doTestBodyCheckLindex(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithLindex(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckSetVar: a body ending in `set VAR` compares the variable's
// value (a TCL list, e.g. e_changes.test's `set ::changes` vs
// "{update 2 trigger 3 ...}").
func doTestBodyCheckSetVar(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	setVar, ok := bodyEndsWithSetVar(tp, bodyCmds)
	if !ok {
		return false
	}
	tp.emitSetVarResultCheck(nameExpr, expectedExpr, setVar)
	return true
}

// doTestBodyCheckLappendVar: a body ending in `lappend VAR $X` compares VAR's
// final list value (sqllimits1-6.3: `set rc [catch {sqlite3_prepare ...}
// STMT]; lappend rc $STMT` vs "1 {(18) statement too long}").
func doTestBodyCheckLappendVar(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	lappVar, ok := bodyEndsWithLappendVar(bodyCmds)
	if !ok {
		return false
	}
	tp.emitSetVarResultCheck(nameExpr, expectedExpr, lappVar)
	return true
}

// doTestBodyCheckCatchsql: the body's last command is a catchsql command (its
// expected value is a {count message} list).
func doTestBodyCheckCatchsql(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyIsCatchsqlCommand(bodyCmds) {
		return false
	}
	tp.emitCatchsqlResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckQueryFunc: the body ends with a query-proc call (e.g.
// `execsql {...} signature`); the last command's query result is in `_r` and
// the expected value is that result list.
func doTestBodyCheckQueryFunc(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithQueryFunc(bodyCmds, tp.queryFuncs) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckSignature: memdb.test .2 bodies end in a bare `signature`
// call (the t3 rollback fingerprint via tclMemdbSignature in _r). queryFuncs
// does not cover it (with-args proc), so dispatch on the fingerprint here.
func doTestBodyCheckSignature(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) < 1 {
		return false
	}
	last := bodyCmds[len(bodyCmds)-1]
	if len(last) != 1 || last[0].Text != "signature" {
		return false
	}
	body, ok := globalProcBodies["signature"]
	if !ok || userProcEmitterFor("signature", body) != "memdb_signature" {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckQuotaListSize: quota.test bodies end in `quota_list` (the
// sorted pattern list is in `_r`; quota-4.4.1 compares [list $quotagroup]) or
// `quota_size NAME` (the tracked group size is in `_r`; quota-4.4.6/4.4.7).
func doTestBodyCheckQuotaListSize(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithCommandName(bodyCmds, "quota_list") && !bodyEndsWithCommandName(bodyCmds, "quota_size") {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckQuotaValueCmd: the body's last command is a value-producing
// quota command (fopen/fread/fwrite/ftell/file_size/...); the transpiler
// left its result in _r and the expected value is that result
// (quota2.test 1.1/1.2.1/1.3/...).
func doTestBodyCheckQuotaValueCmd(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithQuotaValueCmd(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckQuotaGlob: test/quota-glob.test bodies end in
// `sqlite3_quota_glob PATTERN TEXT`; the transpiler mapped it to a runtime
// helper that left the "1"/"0" match result in `_r`.
func doTestBodyCheckQuotaGlob(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithQuotaGlob(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckEQP: the body ends with `eqp "SQL"` — the EXPLAIN QUERY PLAN
// detail list is in `_r` and the expected value is that list (e_fkey-26.x).
func doTestBodyCheckEQP(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithEQP(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckExecsqlQuery: the body's SQL contains a query;
// processCommands ran it through db.Query and left the flattened result in
// `r`. Compare it with the expected variable (a RESULT list, e.g. foreach
// $t232 in without_rowid4-3.2), not an error message.
func (tp *transpiler) doTestBodyCheckExecsqlQuery(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !tp.bodyEndsWithExecsqlQuery(bodyCmds) {
		return false
	}
	tp.emitExecsqlQueryResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckDBEvalQuery: the body's last command is `db eval {SELECT
// ...}` or `db eval $var` (variable holding query SQL) — its result is the
// query rows, not an error. Re-run the SELECT as a query and compare the
// flattened result against the expected value (trans2.test's hash checks,
// autoindex4's foreach loops).
func (tp *transpiler) doTestBodyCheckDBEvalQuery(nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !tp.bodyEndsWithDBEvalQuery(bodyCmds) {
		return false
	}
	tp.emitDBEvalQueryResultCheck(nameExpr, expectedExpr, bodyCmds)
	return true
}

// doTestBodyCheckStringResult: the body's last command is a
// `string map {...} [string tolower $x]` (or similar) chain whose RESULT is
// the do_test value, not an error. The earlier `set x [...]` commands
// populated the variables; emit the lowering/mapping comparison here.
func doTestBodyCheckStringResult(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithStringResult(bodyCmds) {
		return false
	}
	tp.emitStringResultCheck(nameExpr, expectedExpr, bodyCmds)
	return true
}

// doTestBodyCheckStringMatch: a body ending in `string match PATTERN STR` —
// the processStringMatch handler left the "1"/"0" result in `_r`; compare it.
func doTestBodyCheckStringMatch(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithStringMatch(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckFileAttributes: the body ends with `file attributes PATH
// -attr` (the one-arg form returns the current value as a string). The whole
// body runs in the sub-transpiler; the last `file attributes` call leaves its
// result in `_r`. Compare with the expected value (journal3.test 1.2.x.1:
// `file attributes test.db -permissions` returns the current Unix mode bits
// as a perm string).
func doTestBodyCheckFileAttributes(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithFileAttributes(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
}

// doTestBodyCheckIndexExpr: the body ends with `expr {$idx>=0}` after
// `set idx [lsearch $prg OpenEphemeral]` — compare the search result against
// the expected boolean (0/1). The lsearch index is >=0 when the opcode was
// found.
func doTestBodyCheckIndexExpr(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithIndexExpr(bodyCmds) {
		return false
	}
	tp.emitIndexExprCheck(nameExpr, expectedExpr, bodyCmds)
	return true
}

// doTestBodyCheckLsearchDBEval: the body is
// `expr {[lsearch [db eval {SQL}] PATTERN]>=0}` — assert that PATTERN appears
// in the db-eval result rows (ctime-3.0.1).
func doTestBodyCheckLsearchDBEval(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	sqlText, pattern, ok := lsearchDBEvalExpr(bodyCmds)
	if !ok {
		return false
	}
	tp.emitLsearchDBEvalCheck(nameExpr, expectedExpr, sqlText, pattern)
	return true
}

// doTestBodyCheckValueResults: the body ends with a command whose value was
// left in `_r` and is the do_test value — a backup/errmsg/file-size command,
// a prepared-statement metadata query or step, an incremental-blob command,
// a `list ...` result (including a `[catch {...} VAR]` argument), an
// `expr [cmd ...] OP N` resolved truth string (dbstatus.test 5.5.x
// `expr [sqlite3_stmt_status ...]>0`), or a known value-returning TCL builtin
// (`pager_cache_size db`, `execsql {SELECT ...}`, etc. — cache.test 1.3.x,
// memdb.test).
func doTestBodyCheckValueResults(tp *transpiler, nameExpr, expectedExpr string, bodyCmds [][]tcl.RawWord) bool {
	if !bodyEndsWithBackupResult(bodyCmds) && !bodyEndsWithStmtMetadata(bodyCmds) &&
		!bodyEndsWithBlobResult(bodyCmds) && !bodyEndsWithListResult(bodyCmds) &&
		!bodyEndsWithExprResult(bodyCmds) && !bodyEndsWithValueBuiltin(bodyCmds) {
		return false
	}
	tp.emitQueryFuncResultCheck(nameExpr, expectedExpr)
	return true
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

// execsqlSingleBodyHasQuery reports whether a single `execsql {SQL}` body's
// SQL contains a query — such a body returns the flattened query results
// (e.g. foreach $t232 in without_rowid4-3.2).
func execsqlSingleBodyHasQuery(bodyCmds [][]tcl.RawWord) bool {
	return len(bodyCmds) == 1 && len(bodyCmds[0]) >= 2 &&
		bodyCmds[0][0].Text == "execsql" &&
		bodySQLContainsQuery(bodyCmds[0][1].Text)
}

// execsqlLastBodyHasQuery reports whether a multi-command body ENDS in an
// execsql command whose SQL contains a query: `execsql $var` where $var holds
// query SQL (e.g. join3's `set sql "SELECT..."; ...; execsql $sql`), a
// braced `execsql {SELECT ...}` query (e.g. do_test index-3.1 ends with
// `execsql {SELECT name FROM sqlite_master ...}`), or a non-braced literal
// SQL query (e.g. bigrow-2.2's `execsql "SELECT b FROM t1 WHERE a=='abc'"`).
func (tp *transpiler) execsqlLastBodyHasQuery(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) < 1 {
		return false
	}
	lastCmd := bodyCmds[len(bodyCmds)-1]
	if len(lastCmd) < 2 || lastCmd[0].Text != "execsql" {
		return false
	}
	if lastCmd[1].Braced {
		// Braced SQL literal: detect a trailing query statement.
		return bodySQLContainsQuery(lastCmd[1].Text)
	}
	varName := strings.TrimPrefix(lastCmd[1].Text, "$")
	if tp.queryVars[varName] {
		return true
	}
	// A non-braced execsql whose argument is a literal SQL query (not a $var
	// reference) also returns the flattened query result.
	if !strings.HasPrefix(strings.TrimSpace(lastCmd[1].Text), "$") {
		return bodySQLContainsQuery(tclUnescapeQuoted(lastCmd[1].Text))
	}
	return false
}

// bodyEndsWithExecsqlQuery reports whether a do_test body ends with (or is a
// single) execsql command whose SQL contains a query.
func (tp *transpiler) bodyEndsWithExecsqlQuery(bodyCmds [][]tcl.RawWord) bool {
	if execsqlSingleBodyHasQuery(bodyCmds) {
		return true
	}
	return tp.execsqlLastBodyHasQuery(bodyCmds)
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
	tp.emitLine("\tt.Errorf(\"catchsql mismatch\\n  got:  [%%v]\\n  want: [%%s]\\n  body: do_test %%s\", resErrString(_res), %s, %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
}

// emitSetVarResultCheck emits a comparison of a `set VAR`-ending body. The
// variable holds a TCL list (e.g. e_changes.test's ::changes), so compare the
// flattened forms to ignore list-rendering braces. When VAR is a
// sqlite3_prepare TAIL variable (capi2-2.x), compare the collapsed forms: the
// C-API tail pointer and the TCL braced expected may differ in leading/trailing
// whitespace.
func (tp *transpiler) emitSetVarResultCheck(nameExpr, expectedExpr, setVar string) {
	// sqlite_like_count reads come from the engine's LIKE/GLOB invocation
	// counter (func.c sqlite3_like_count, TCL-linked in tester.tcl), not the
	// Go shadow variable — the counter observes the LIKE optimization
	// (like.test 3.x: 12 calls without it, 0 with the index range scan).
	if setVar == "sqlite_like_count" {
		setVar = "tclLikeCount(db)"
	}
	// TCL do_test treats a /pattern/ (or ~/pattern/) expected value as a
	// regexp (inverted) match, not literal equality — intarray-1.1b compares
	// the registered intarray handle ("0 X5") against /0 [0-9A-Z]+/.
	if isTCLRegexPattern(expectedExpr) {
		tp.emitSetVarRegexCheck(nameExpr, expectedExpr, setVar)
		return
	}
	if tp.prepareTailVars[setVar] {
		tp.emitLine("got := tclListFlattenCollapse(%s)", setVar)
		tp.emitLine("want := tclListFlattenCollapse(%s)", expectedExpr)
	} else {
		tp.emitLine("got := tclListFlatten(%s)", setVar)
		tp.emitLine("want := tclListFlatten(%s)", expectedExpr)
	}
	tp.emitSetVarMismatchCheck(nameExpr)
}

// emitSetVarRegexCheck emits the /pattern/ comparison of a `set VAR`-ending
// body. TCL regexes run against the RAW set result: `set ::stmtlist(record)`
// renders a list of sublists BRACED ("{19 {SELECT ...}}") and the C patterns
// match those braces (trace3-3.x/4.x/5.x) — do not flatten away the quoting
// level for pattern comparisons.
func (tp *transpiler) emitSetVarRegexCheck(nameExpr, expectedExpr, setVar string) {
	tp.emitLine("got := %s", setVar)
	inner := regexPatternInner(expectedExpr)
	negated := regexPatternNegated(expectedExpr)
	if strings.HasPrefix(inner, "*") {
		// TCL glob (string match): the inner pattern starts with *.
		tp.emitSetVarGlobCheck(nameExpr, inner, negated)
		return
	}
	tp.emitLine("wantPattern := %s", regexPatternExpr(expectedExpr))
	if negated {
		tp.emitLine("if matched, _ := regexp.MatchString(wantPattern, got); matched {")
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  must not match pattern: [%%s]\\n  body: do_test %%s\", got, wantPattern, %s)", nameExpr)
		tp.emitLine("}")
	} else {
		tp.emitLine("if matched, _ := regexp.MatchString(wantPattern, got); !matched {")
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want pattern: [%%s]\\n  body: do_test %%s\", got, wantPattern, %s)", nameExpr)
		tp.emitLine("}")
	}
}

// emitSetVarGlobCheck emits the glob (string match) comparison of a
// `set VAR`-ending body whose inner pattern starts with `*`.
func (tp *transpiler) emitSetVarGlobCheck(nameExpr, inner string, negated bool) {
	globExpr := fmt.Sprintf("%q", inner)
	if negated {
		tp.emitLine("if globMatch(got, %s) {", globExpr)
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  must not match glob: [%%s]\\n  body: do_test %%s\", got, %s, %s)", globExpr, nameExpr)
		tp.emitLine("}")
	} else {
		tp.emitLine("if !globMatch(got, %s) {", globExpr)
		tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want glob: [%%s]\\n  body: do_test %%s\", got, %s, %s)", globExpr, nameExpr)
		tp.emitLine("}")
	}
}

// emitSetVarMismatchCheck emits the flattened-list equality check shared by
// the `set VAR` comparison forms.
func (tp *transpiler) emitSetVarMismatchCheck(nameExpr string) {
	tp.emitLine("if got != want && !tclFpnumCompare(got, want) {")
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
	if doTestBodyReadsArrayCounter(bodyCmds) {
		tp.emitDoTestSideEffects(nameExpr, bodyCmds, "testvfs sync-counter introspection observes the VFS layer, not the engine; SQL side effects only")
		return true
	}
	return false
}

// doTestBodyReadsArrayCounter reports whether the body's VALUE comes from a
// TCL array read (`array get ::sync` — the testvfs xSync counter the 7xx
// loop of vacuum-into.test compares). The array is harness-side state the
// transpiler never populates ("array get (not transpiled)"), so the
// assertion would compare a stale value; only the SQL side effects are
// meaningful.
func doTestBodyReadsArrayCounter(bodyCmds [][]tcl.RawWord) bool {
	if len(bodyCmds) != 1 {
		return false
	}
	cmd := bodyCmds[0]
	return len(cmd) >= 2 && cmd[0].Text == "array" && cmd[1].Text == "get"
}
