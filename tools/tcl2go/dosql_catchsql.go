// Package main implements the tcl2go tool.
//
// This file holds the do_catchsql_test result comparison: presence-based
// runtime-list expectations and the dispatch over expected-value forms
// (success, dynamic message, literal message, any error).
package main

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// catchsqlPresenceVar detects a do_catchsql_test expected value of the form
// `[list [expr {$VAR!=""}] $VAR]` — a runtime list whose first element is 1
// exactly when the error variable is non-empty. Returns the Go variable name
// (or ""). The TCL pattern builds "0 {}" (success) or "1 {msg}" (error).
// presenceListParts validates the outer `[list ...]` wrapper of a
// catchsql-presence expected value and returns its two words.
func presenceListParts(args []tcl.RawWord) ([]string, bool) {
	if len(args) < 3 {
		return nil, false
	}
	text := strings.TrimSpace(args[2].Text)
	if !strings.HasPrefix(text, "[list ") || !strings.HasSuffix(text, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(text[len("[list ") : len(text)-1])
	parts := tclCmdWords(inner)
	if len(parts) != 2 {
		return nil, false
	}
	return parts, true
}

// presenceExprVar extracts the variable name of the first list element —
// `[expr {$VAR!=""}]` (or {[expr {$VAR!="}]}). Returns "" for other forms.
func presenceExprVar(part string) string {
	expr := strings.TrimSpace(part)
	expr = strings.TrimPrefix(expr, "{")
	expr = strings.TrimSuffix(expr, "}")
	if !strings.HasPrefix(expr, "[expr {") || !strings.HasSuffix(expr, "}]") {
		return ""
	}
	cond := expr[len("[expr {") : len(expr)-2]
	if !strings.Contains(cond, "!=\"\"") {
		return ""
	}
	// Extract the variable name from the condition.
	varName := ""
	for _, w := range tclCmdWords(cond) {
		w = strings.TrimSpace(w)
		if strings.HasPrefix(w, "$") {
			varName = strings.TrimPrefix(w, "$")
			if i := strings.Index(varName, "!"); i >= 0 {
				varName = varName[:i]
			}
		}
	}
	return varName
}

func catchsqlPresenceVar(args []tcl.RawWord) string {
	parts, ok := presenceListParts(args)
	if !ok {
		return ""
	}
	// First element: [expr {$VAR!=""}] (or {[expr {$VAR!=""}]}).
	varName := presenceExprVar(parts[0])
	if varName == "" {
		return ""
	}
	// Confirm the second element references the same variable.
	second := strings.TrimSpace(parts[1])
	if !strings.HasPrefix(second, "$") || strings.TrimPrefix(second, "$") != varName {
		return ""
	}
	goName := tclVarToGo("$" + varName)
	if !isValidGoIdent(goName) {
		return ""
	}
	return goName
}

// emitCatchsqlPresenceComparison emits the presence-based check for a
// `[list [expr {$err!=""}] $err]` expected value: "0 {}" when the error
// variable is empty (success), "1 {msg}" otherwise. Returns handled=false
// when msgVar is empty.
func (tp *transpiler) emitCatchsqlPresenceComparison(msgVar, sqlExpr, dbConn string) bool {
	if msgVar == "" {
		return false
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if %s == \"\" {", msgVar)
	tp.emitLine("\tif _res.Error != nil {")
	tp.emitLine("\t\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", resErrString(_res), %s)", sqlExpr)
	tp.emitLine("\t}")
	tp.emitLine("} else {")
	tp.emitLine("\tif _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", msgVar)
	tp.emitLine("\t\tt.Errorf(\"expected error containing %%s, got: %%v\\n  sql: %%s\", %s, resErrString(_res), %s)", msgVar, sqlExpr)
	tp.emitLine("\t}")
	tp.emitLine("}")
	return true
}

// emitCatchsqlDynamicVarComparison handles a bare TCL variable expected value
// (do_catchsql_test NAME SQL $err): the variable holds the TCL catchsql
// result ("1 {msg}" or "0 {}"); use the count-aware runtime comparison so a
// success expectation ("0 {}") is checked as success, not as an empty error
// message. Returns handled=false for other forms.
func (tp *transpiler) emitCatchsqlDynamicVarComparison(args []tcl.RawWord, sqlExpr, dbConn string) bool {
	if !(len(args) >= 3 && strings.HasPrefix(strings.TrimSpace(args[2].Text), "$")) {
		return false
	}
	dynamic := tp.buildStringExpr(strings.TrimSpace(args[2].Text))
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if !tclCatchsqlMatches(_res, %s) {", dynamic)
	tp.emitLine("\tt.Errorf(\"catchsql mismatch\\n  got:  [%%v]\\n  want: [%%s]\\n  sql: %%s\", resErrString(_res), %s, %s)", dynamic, sqlExpr)
	tp.emitLine("}")
	return true
}

// catchsqlQuotedDynamicMsg detects the bare "1 {msg with $vars}" quoted form
// (do_catchsql_test "1 {msg $v}"): the message interpolates $var at runtime.
// Returns the message string expression, or "" for other forms.
func (tp *transpiler) catchsqlQuotedDynamicMsg(args []tcl.RawWord) string {
	if !args[2].Quoted || !strings.HasPrefix(strings.TrimSpace(args[2].Text), "1 {") ||
		!strings.Contains(args[2].Text, "$") {
		return ""
	}
	msg := strings.TrimSpace(args[2].Text)
	msg = strings.TrimSpace(msg[2:]) // drop "1 "
	msg = strings.Trim(msg, "{}")
	return tp.buildStringExpr(msg)
}

// emitCatchSQLComparison emits the do_catchsql_test result comparison,
// dispatching on the expected-value form (success, dynamic message, literal
// message, or any error). dbConn is the connection the SQL runs on (a
// trailing connection argument, e.g. `catchsql { DETACH aux2 } db2` in a
// do_test body, resolved by the caller via resolveSQLConnection).
func (tp *transpiler) emitCatchSQLComparison(nameExpr, sqlExpr, expectedExpr string, args []tcl.RawWord, dbConn string) {
	// TCL `do_catchsql_test NAME SQL [list [expr {$err!=""}] $err]`: the
	// expected value is a RUNTIME list — "0 {}" when the error variable is
	// empty (success), "1 {msg}" otherwise. Emit a presence-based check.
	if tp.emitCatchsqlPresenceComparison(catchsqlPresenceVar(args), sqlExpr, dbConn) {
		return
	}
	errMsg := extractExpectedErrorFromLiteral(expectedExpr)
	raw, _ := strconv.Unquote(expectedExpr)
	expectSuccess := !strings.HasPrefix(raw, "1 ")
	errMsgDynamic := ""
	// A bare TCL variable expected value (do_catchsql_test NAME SQL $err):
	// render the variable's Go value at runtime so the leading "1 " error
	// marker is detected dynamically.
	if tp.emitCatchsqlDynamicVarComparison(args, sqlExpr, dbConn) {
		return
	}
	// TCL [list 1 "<msg with $vars>"] form: the expected error message is a
	// runtime Go expression (the list command builds the message dynamically).
	if msgExpr, ok := tp.listExpectedErrorMsg(args[2].Text); ok {
		expectSuccess = false
		errMsgDynamic = msgExpr
	}
	// Bare "1 {msg with $vars}" quoted form (do_catchsql_test "1 {msg $v}"):
	// the message interpolates $var at runtime.
	if expectSuccess {
		if msg := tp.catchsqlQuotedDynamicMsg(args); msg != "" {
			expectSuccess = false
			errMsgDynamic = msg
		}
	}
	// TCL catchsql regex form "/1 {near .* syntax error}/" (with2 6.7-6.9),
	// "/1.*too big.*/" (basexx1 118-119), "/1 .*corrupt.*/"
	// (rtreefuzz001-210/310), "/fts5: syntax error/" (fts5simple 11.4):
	// do_test/do_catchsql_test apply the regex to the STRING of the whole
	// catchsql RESULT — "0 <result>" on success, "1 {<error>}" on failure —
	// not to the error alone. A statement that behaves exactly like SQLite
	// (rtreecheck succeeding with a report that mentions "corrupt")
	// satisfies "/1 .*corrupt.*/" through the success rendering, so the
	// match must not presuppose an error. tester.tcl treats ANY expectation
	// that both starts and ends with "/" as a regex over the rendered
	// result, so detect the general slash-wrapped form (not just "/1"
	// prefixes) and emit a regexp match over that string.
	if len(raw) >= 2 && strings.HasPrefix(raw, "/") && strings.HasSuffix(raw, "/") {
		tp.emitCatchsqlRegexComparison(sqlExpr, raw, dbConn)
		return
	}
	if expectSuccess {
		// TCL do_catchsql_test {0 {}} — the statement is expected to succeed.
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", resErrString(_res), %s)", sqlExpr)
		tp.emitLine("}")
		return
	}
	if errMsgDynamic != "" {
		// Dynamic error message: the expected text is a runtime Go expression
		// (e.g. the loop variable `_error` holding "row value misused").
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", errMsgDynamic)
		tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %s, resErrString(_res), %s)", errMsgDynamic, sqlExpr)
		tp.emitLine("}")
		return
	}
	if errMsg != "" {
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %q) {", errMsg)
		tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %q, resErrString(_res), %s)", errMsg, sqlExpr)
		tp.emitLine("}")
		return
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if _res.Error == nil {")
	tp.emitLine("\tt.Errorf(\"expected error, got none\\n  sql: %%s\", %s)", sqlExpr)
	tp.emitLine("}")
}
