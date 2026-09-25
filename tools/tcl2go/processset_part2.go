// Package main implements the tcl2go tool.
//
// This file holds the `set VAR [...]` value emitters used by the dispatch
// chain (processset_bracket*.go) plus the catch-block machinery and the
// generic plain-set assignment.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// setCreateDBValue handles `set nPage [create_db "..."]` (e_vacuum.test):
// emits the t1/t2 table setup so later VACUUM tests see the tables, and binds
// the file-size result (VACUUM-dependent) to "0" — the assertions that use it
// are skipped as VACUUM.
func (tp *transpiler) setCreateDBValue(goName string) bool {
	tp.emitLine("_res = db.Exec(\"PRAGMA page_size = 1024;\")")
	tp.emitLine("_ = _res")
	tp.emitLine("_res = db.Exec(\"CREATE TABLE t1(a PRIMARY KEY, b UNIQUE); INSERT INTO t1 VALUES(1, randomblob(400)); INSERT INTO t1 SELECT a+1, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+2, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+4, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+8, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+16, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+32, randomblob(400) FROM t1; INSERT INTO t1 SELECT a+64, randomblob(400) FROM t1; CREATE TABLE t2(a PRIMARY KEY, b UNIQUE); INSERT INTO t2 SELECT * FROM t1;\")")
	tp.emitLine("if _res.Error != nil { t.Errorf(\"create_db: %%v\", _res.Error) }")
	tp.emitLine("%s = \"0\"", goName)
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setLsearchValue handles `set idx [lsearch $prg OpenEphemeral]` — search a
// program string (from tclExecSQL EXPLAIN output) for an opcode name and store
// the result as a string index ("-1" when not found). Skips leading option
// flags (e.g. lsearch -exact $list $opcode).
// setBinaryFormatValue handles `set VAR [binary format SPEC ARGS...]` —
// assigns the resulting byte string to VAR (the corruption tests build the
// modified segdir root blob this way).
func (tp *transpiler) setBinaryFormatValue(goName, cmdText string) (string, bool) {
	inner := strings.TrimSpace(strings.TrimPrefix(cmdText, "binary format"))
	fields := tclCmdWords(inner)
	if len(fields) < 2 {
		return "", false
	}
	spec := fields[0]
	vals := make([]string, 0, len(fields)-1)
	for _, a := range fields[1:] {
		vals = append(vals, tp.binaryArgExpr(a))
	}
	expr, ok := binaryFormatGoExpr(spec, vals)
	if !ok {
		return "", false
	}
	tp.assignSetValue(goName, expr)
	return expr, true
}

// setStringFuncValue handles `set VAR [string first NEEDLE HAY ?START?]`:
// runtime semantics of TCL string first (zipfile.test 24.x patches a
// central-directory offset computed from $zip at runtime). Returns false for
// unsupported subcommands so callers keep their fallbacks.
func setStringFuncValue(tp *transpiler, goName string, cmdParts []string) bool {
	rest := cmdParts[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:] // option flags (-nocase ...): none affect runtime index
	}
	if len(rest) < 2 || rest[0] != "first" {
		return false
	}
	needle, ok := tp.stringWordExpr(rest[1])
	if !ok {
		return false
	}
	hayIdx := 2
	hay, ok := tp.stringWordExpr(rest[hayIdx])
	if !ok {
		return false
	}
	start := ""
	if len(rest) > hayIdx+1 {
		s, ok2 := tp.stringWordExpr(rest[hayIdx+1])
		if !ok2 {
			return false
		}
		start = ", toInt(" + s + ")"
	}
	tp.assignSetValue(goName, fmt.Sprintf("strconv.Itoa(tclStrIndex(%s, %s%s))", hay, needle, start))
	return true
}

// stringWordExpr renders one TCL word as a Go STRING expression: $var binds
// its variable's value, a quoted literal binds the literal text. Anything
// else (brackets, braced scripts) is not supported here.
func (tp *transpiler) stringWordExpr(word string) (string, bool) {
	if strings.HasPrefix(word, "$") && !strings.Contains(word, "(") {
		gv := tclVarToGo(strings.TrimPrefix(word, "$"))
		if gv != "" && isValidGoIdent(gv) {
			return gv, true
		}
		return "", false
	}
	return strconv.Quote(strings.Trim(word, `"`)), true
}

// hexCodecArgExpr renders the VALUE argument of a [binary encode|decode hex]
// word list as a Go string expression; "" when unsupported.
func (tp *transpiler) hexCodecArgExpr(cmdParts []string, idx int) string {
	if idx >= len(cmdParts) {
		return ""
	}
	expr, ok := tp.stringWordExpr(cmdParts[idx])
	if !ok {
		return ""
	}
	return expr
}

// setLsearchValue handles `set VAR [lsearch ...]` — assigns the lsearch
// result to VAR.
func (tp *transpiler) setLsearchValue(goName, cmdText string, cmdParts []string) bool {
	// Re-parse cmdText with the TCL tokenizer (not strings.Fields) so bracketed
	// command substitutions like `[make_str $d $ENTRY_LEN]` survive intact
	// (autovacuum.test 1.x: `set idx [lsearch $::tbl_data [make_str ...]]`).
	parts := tclCmdWords(cmdText)
	if len(parts) < 3 {
		return false
	}
	rest := parts[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	if len(rest) < 2 {
		return false
	}
	// The first arg (list) is a TCL variable reference; resolve to its Go
	// identifier so the emitted tclLsearch sees the live list value.
	listExpr := strings.TrimPrefix(rest[0], "$")
	goList := tclVarToGo(listExpr)
	if !isValidGoIdent(goList) {
		return false
	}
	// The value arg may itself contain a [cmd] substitution (autovacuum.test:
	// `set idx [lsearch $::tbl_data [make_str $d $ENTRY_LEN]]`). Re-join the
	// remaining words into a single string and pass through buildStringExpr
	// so nested brackets are evaluated at runtime.
	valueArg := strings.Join(rest[1:], " ")
	valueExpr := tp.buildStringExpr(valueArg)
	tp.emitLine("%s = strconv.Itoa(tclLsearch(%s, %s))", goName, goList, valueExpr)
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setMakeExprValue handles `set VAR [make_exprN cList vList op]` —
// rowvalue2's expression-building procs. Emits a call to the Go helper with
// the runtime variable values.
func (tp *transpiler) setMakeExprValue(goName, cmdText, cmdName string) bool {
	goFn := map[string]string{"make_expr1": "tclMakeExpr1", "make_expr2": "tclMakeExpr2", "make_expr3": "tclMakeExpr3"}[cmdName]
	words := tclCmdWords(cmdText)
	if len(words) < 4 {
		return false
	}
	argExprs := make([]string, 3)
	for i := 1; i <= 3; i++ {
		w := words[i]
		if strings.HasPrefix(w, "$") {
			argExprs[i-1] = tclVarToGo(strings.TrimPrefix(w, "$"))
		} else if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			argExprs[i-1] = tp.goStringLiteral(tcl.RawWord{Text: w[1 : len(w)-1]})
		} else {
			argExprs[i-1] = tp.goStringLiteral(tcl.RawWord{Text: strings.Trim(w, `"`)})
		}
	}
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("%s = %s(%s, %s, %s)", goName, goFn, argExprs[0], argExprs[1], argExprs[2])
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setRegexpValue handles `set VAR [regexp PATTERN [db one {SQL}]]` — evaluate
// the capability regexp against the engine's answer. UTF-16 encoding is not
// supported (PRAGMA encoding is always UTF-8), so isutf16 = "0".
func (tp *transpiler) setRegexpValue(goName string, cmdParts []string) bool {
	setTo := "0"
	pattern := strings.Trim(cmdParts[1], `"`)
	if !strings.Contains(pattern, "16") {
		// Non-UTF16 capability check: no reliable answer; leave 0.
		setTo = "0"
	}
	tp.emitLine("%s = %q // capability regexp %q not matched (engine default)", goName, setTo, pattern)
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	return true
}

// setDBEvalValue handles `set var [db eval "SQL"]` — run the query and assign
// the flattened result. The double-quoted SQL substitutes $var as RAW TEXT
// (TCL string substitution) before db eval runs, so a variable holding an
// expression fragment (rowvalue4: WHERE $where) embeds verbatim. Braced db
// eval binds $var as VALUES (buildSQLStringExpr handles that path elsewhere).
func (tp *transpiler) setDBEvalValue(goName, cmdText string, cmdParts []string) bool {
	sqlText := strings.TrimSpace(strings.TrimPrefix(cmdText, cmdParts[0]+" eval"))
	sqlText = strings.TrimSpace(sqlText)
	connName := cmdParts[0]
	connGo := connName
	if connName == "db" {
		connGo = "db"
	}
	braced := len(sqlText) >= 2 && sqlText[0] == '{' && sqlText[len(sqlText)-1] == '}'
	if braced {
		sqlText = strings.TrimSpace(sqlText[1 : len(sqlText)-1])
	} else {
		sqlText = strings.Trim(sqlText, `"`)
	}
	var sqlExpr string
	if braced {
		// Braced db eval binds $var as SQL VALUES (sqlLiteral), and [cmd]
		// bracket-quoted identifiers stay literal.
		sqlExpr = tp.buildSQLStringExprNoCmd(sqlText)
	} else {
		sqlExpr = tp.buildStringExpr(sqlText)
	}
	dbEvalVar := fmt.Sprintf("_dbeval%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclExecSQL(%s, %s)", dbEvalVar, connGo, sqlExpr)
	tp.assignSetValue(goName, dbEvalVar)
	return true
}

// setDBOneValue handles `set var [db one "SQL"]` / [db onecolumn "SQL"] — run
// the query and assign the first column of the first row (TCL's db one alias
// for onecolumn). The Go variable holds the rendered value as a string for
// later expected-value comparisons.
func (tp *transpiler) setDBOneValue(goName, cmdText string, cmdParts []string) bool {
	sqlText := strings.TrimSpace(strings.TrimPrefix(cmdText, "db "+cmdParts[1]))
	sqlText = strings.TrimSpace(strings.Trim(sqlText, `"`))
	sqlExpr := tp.buildSQLStringExpr(sqlText)
	oneVar := fmt.Sprintf("_dbone%d", tp.varCount)
	tp.varCount++
	tp.emitLine("%s := tclExecSQL(db, %s)", oneVar, sqlExpr)
	tp.assignSetValue(goName, oneVar)
	return true
}

// setCatchsqlSetValue handles `set VAR [catchsql {SQL}]` — execute the SQL
// and bind VAR to the catchsql result string ("0 {rows}" on success,
// "1 {msg}" on error) via tclCatchsqlString, matching TCL's [catchsql]
// command-substitution value. The SQL may reference runtime $vars (e.g.
// `set ans [catchsql {SELECT compileoption_get($N)}]`).
func (tp *transpiler) setCatchsqlSetValue(goName, cmdText string, cmdParts []string) bool {
	// Re-parse cmdText with brace-aware splitting (cmdParts from
	// processSetBracketValue uses strings.Fields, which breaks braced SQL
	// like {SELECT ...} into separate tokens). This recovers the full SQL
	// body and any trailing connection argument.
	parts := tclCmdWords(cmdText)
	if len(parts) < 2 {
		tp.assignSetValue(goName, `""`)
		return true
	}
	// parts[1] is the brace-stripped SQL body (or a $var holding SQL).
	sqlWord := tcl.RawWord{Text: parts[1], Braced: strings.HasPrefix(parts[1], "{")}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	if sqlExpr == `""` {
		tp.assignSetValue(goName, `""`)
		return true
	}
	// A trailing connection argument (catchsql {SQL} db2) is rare here;
	// resolve through the standard connection dispatcher when present.
	dbConn := "db"
	if len(parts) >= 3 {
		args := []tcl.RawWord{{Text: "catchsql"}, sqlWord}
		for _, extra := range parts[2:] {
			args = append(args, tcl.RawWord{Text: extra})
		}
		dbConn = tp.resolveSQLConnection(args)
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("%s = tclCatchsqlString(_res)", goName)
	return true
}

// normalizeListValueText normalizes a `list ...` body: TCL backslash-newline
// continuations are removed (they are not part of the list value), and the
// catchsql-style `1 {message}` two-element error form collapses to its
// message text.
func normalizeListValueText(listText string) string {
	listText = strings.ReplaceAll(listText, "\\\r\n", " ")
	listText = strings.ReplaceAll(listText, "\\\n", " ")
	listText = strings.TrimSpace(listText)
	// A trailing lone backslash is a line-continuation remnant of `... \ ]`
	// (the parser keeps it as a literal word); TCL folds it away, so drop it.
	if strings.HasSuffix(strings.TrimSpace(listText), "\\") {
		listText = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(listText), "\\"))
	}
	// A [list 1 {message}] form (catchsql-style error) is often stored
	// for later do_catchsql_test $var comparisons; keep just the
	// message text so strings.Contains against a real error matches.
	// Guard on the EXACT two-element shape: a longer data list whose first
	// element happens to be 1 (json102's correct_answer starts
	// "1 {$.id} 123 ...") must keep every element.
	if elems := tclSplitList(listText); len(elems) == 2 && elems[0] == "1" {
		msg := strings.TrimSpace(elems[1])
		if len(msg) >= 2 && msg[0] == '{' && msg[len(msg)-1] == '}' {
			msg = strings.TrimSpace(msg[1 : len(msg)-1])
		}
		listText = msg
	}
	return listText
}

// setListValue handles `set var [list a b c]` — build a TCL-list string
// without the "list" command word so tclSplitList at Go runtime returns
// exactly the list elements.
func (tp *transpiler) setListValue(goName, cmdText string) bool {
	listText := normalizeListValueText(strings.TrimPrefix(cmdText, "list"))
	valExpr := tp.goStringLiteral(tcl.RawWord{Text: listText})
	// A list whose elements are SQL scripts with declared $var references
	// (incrvacuum.test's `set TestScriptList [list {...$::str1...} ...]`,
	// consumed later as `foreach sql $TestScriptList { execsql $sql }`)
	// must render each $var as a SQL literal (TCL's `db eval` binds $var
	// as a parameter); the default raw-variable rendering produces
	// syntactically invalid SQL. Lists containing top-level command
	// substitutions ([catch {...} msg]) keep the default rendering.
	if (hasDeclaredDollarVarRef(listText, tp) || hasColonVarRef(listText, tp)) &&
		looksLikeSQLText(listText) && !hasTopLevelCmdSubst(listText) {
		valExpr = tp.buildSQLStringExprNoCmd(listText)
	}
	tp.assignSetValue(goName, valExpr)
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = listText
	return true
}

// setSqlite3Value handles `set var [sqlite3 db <file>]` — reopen a connection
// as a side effect. The connection handle is not used in Go, so assign an
// empty placeholder after performing the reopen. A preceding "db close"
// already emitted db.Close().
func (tp *transpiler) setSqlite3Value(goName string, cmdParts []string) bool {
	goName2 := tclVarToGo(cmdParts[1])
	filename := tp.buildStringExpr(cmdParts[2])
	if tp.pendingFileReset[cmdParts[2]] {
		delete(tp.pendingFileReset, cmdParts[2])
		filename = tp.buildStringExpr(cmdParts[2])
	}
	tp.dqsDDL = true // a fresh connection resets DQS to SQLite defaults
	tp.dqsDML = true
	tp.emitLine("%s, err = frigolite.Open(%s)", goName2, filename)
	tp.emitLine("if err != nil { t.Fatal(err) }")
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = \"\"", goName)
	} else {
		tp.emitLine("var %s = \"\"", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
	return true
}

// setExprValue handles `set var [expr {...}]` — evaluate constant expressions
// at generation time, or emit runtime evaluation for variable/command/query
// expressions.
// readCountExpr renders the byte-count argument of `read $fd N` as a Go int
// expression for tclReadFileWithLen. A plain literal or variable reference
// passes through unchanged; a bracket-balanced `[expr ...]` count
// (memdb1.test: `set data [read $fd [expr 20*1024]]`) is transpile-folded to
// an integer literal when possible, otherwise evaluated at runtime via
// toInt(runtimeExprValue(...)).
func (tp *transpiler) readCountExpr(countText string) (string, bool) {
	countText = strings.TrimSpace(countText)
	if countText == "" {
		return "", false
	}
	if strings.HasPrefix(countText, "[expr") && strings.HasSuffix(countText, "]") {
		exprStr := strings.TrimSpace(countText[len("[expr") : len(countText)-1])
		if len(exprStr) >= 2 && exprStr[0] == '{' && exprStr[len(exprStr)-1] == '}' {
			exprStr = exprStr[1 : len(exprStr)-1]
		}
		if res, err := tcl.EvalExpr(exprStr, nil, nil); err == nil {
			if n, perr := strconv.ParseInt(strings.TrimSpace(res), 10, 64); perr == nil {
				return strconv.FormatInt(n, 10), true
			}
		}
		return "toInt(" + tp.runtimeExprValue(exprStr) + ")", true
	}
	return countText, true
}

// exprConstantValue folds a TCL expr string into a Go value expression at
// generation time. Returns "" when the expression needs runtime evaluation.
func (tp *transpiler) exprConstantValue(exprStr string) string {
	result, err := tcl.EvalExpr(exprStr, nil, nil)
	if err == nil {
		return fmt.Sprintf("%q", result)
	}
	if fs, ok := fileSizeArithExpr(exprStr); ok {
		// [file size PATH] arithmetic (e.g. `[file size test.db]/1024`) —
		// render the file size and arithmetic at runtime.
		return "strconv.Itoa(" + fs + ")"
	}
	if strings.Contains(exprStr, "[") && strings.Contains(exprStr, "]") {
		// [cmd] command substitutions (e.g. [string length $word]).
		if goExpr, ok := tp.exprCmdToGo(exprStr); ok {
			return "strconv.Itoa(" + goExpr + ")"
		}
		return ""
	}
	if strings.Contains(exprStr, "rand(") {
		// rand() is the one TCL math function the runtime evaluator
		// (tclEvalFuncs) cannot compute, so render it as native Go code
		// (the helpers import math/rand). Other functions (log, pow,
		// int, sqrt, ...) and pure $var arithmetic fall through to
		// tclExprWith, which evaluates at runtime with float semantics
		// (critical when a var holds a real like 2460369.5 — toInt()
		// would truncate it).
		if goExpr, ok := tp.exprCmdToGo(exprStr); ok {
			return "strconv.Itoa(" + goExpr + ")"
		}
	}
	return ""
}

func (tp *transpiler) setExprValue(goName, cmdText string) bool {
	exprStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cmdText), "expr"))
	if len(exprStr) >= 2 && exprStr[0] == '{' && exprStr[len(exprStr)-1] == '}' {
		exprStr = exprStr[1 : len(exprStr)-1]
	}
	// A bare integer literal keeps its exact decimal text: EvalExpr's
	// float64 path would render 9223372036854775807 as
	// "9.223372036854776e+18" (unionvtab.test 3.8/4.x S/L bounds).
	if n, perr := strconv.ParseInt(strings.TrimSpace(exprStr), 10, 64); perr == nil {
		tp.assignSetValue(goName, fmt.Sprintf("%q", strconv.FormatInt(n, 10)))
		return true
	}
	valExpr := tp.exprConstantValue(exprStr)
	if valExpr == "" {
		valExpr = tp.runtimeExprValue(exprStr)
	}
	tp.assignSetValue(goName, valExpr)
	return true
}

// runtimeExprValue builds a Go expression that evaluates a TCL expr string at
// runtime (with live $var values), handling the db-eval-eq-empty form and the
// generic tclExprWith fallback.
func (tp *transpiler) runtimeExprValue(exprStr string) string {
	// `$a eq $b` / `$a ne $b` — native Go string comparison; avoids the
	// token-wise runtime evaluator on multi-word values (rtree2 dumps).
	if m := eqNeExpr.FindStringSubmatch(exprStr); m != nil {
		op := "=="
		if m[2] == "ne" {
			op = "!="
		}
		return fmt.Sprintf("tclBool01(%s %s %s)",
			tp.exprVarValue(strings.TrimPrefix(m[1], "$")), op,
			tp.exprVarValue(strings.TrimPrefix(m[3], "$")))
	}
	// TCL `[expr {[db eval {SQL}] eq {{}}}]` — a boolean computed from
	// a query result (e.g. func4.test's highPrecision flags). Emit a
	// runtime db eval that runs the SQL and compares the flattened
	// result against the empty string, returning "1"/"0" like TCL
	// expr. This cannot be evaluated at generation time because the
	// transpiler has no engine.
	if dbEvalSQL, ok := dbEvalEqEmptyExpr(exprStr); ok {
		sqlExpr := tp.buildSQLStringExpr(dbEvalSQL)
		// The TCL expr compares the db eval result (rendered, NULL as
		// "{}") against the empty list {}: a NULL result is equal.
		return fmt.Sprintf("func() string { _r := tclExecSQL(db, %s); if _r == \"\" || _r == \"{}\" { return \"1\" }; return \"0\" }()", sqlExpr)
	}
	// Runtime evaluation with live $var values.
	exprVarNames, exprGo := tclExprToGo(exprStr, tp.vars)
	if len(exprVarNames) == 0 {
		return fmt.Sprintf("tclExpr(%q)", exprGo)
	}
	var parts []string
	for _, name := range exprVarNames {
		parts = append(parts, fmt.Sprintf("%q: %s", name, tp.exprVarValue(name)))
	}
	return fmt.Sprintf("tclExprWith(%q, map[string]string{%s})", exprGo, strings.Join(parts, ", "))
}

// assignSetValue emits an assignment (declaration or update) of valueExpr to
// goName with the standard suppress-unused line.
func (tp *transpiler) assignSetValue(goName, valueExpr string) {
	if tp.isVarDeclared(goName) {
		tp.emitLine("%s = %s", goName, valueExpr)
	} else {
		tp.emitLine("var %s = %s", goName, valueExpr)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // suppress unused warning", goName)
}

// findBracedBody scans s for the first complete {...} group and returns the
// text inside the braces and the trimmed remainder after the closing brace.
func findBracedBody(s string) (body, rest string, ok bool) {
	if !strings.HasPrefix(s, "{") {
		return "", "", false
	}
	depth := 0
	bodyStart := -1
	for i, c := range s {
		if c == '{' {
			if depth == 0 {
				bodyStart = i + 1
			}
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 && bodyStart >= 0 {
				return s[bodyStart:i], strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

// setCatchValue handles `set v [catch {execsql ...} msg]` — transpile the
// catch body as a Go block that records the result code and error message.
func (tp *transpiler) setCatchValue(goName, cmdText string) bool {
	varName := goName
	errVar := "_catchErrMsg"
	// Find the braced body and optional error var
	restAfterCatch := cmdText
	restAfterCatch = strings.TrimSpace(strings.TrimPrefix(restAfterCatch, "catch"))
	bodyStr, restStr, ok := findBracedBody(restAfterCatch)
	if !ok {
		return false
	}
	// If there's an error variable name
	errVar = "_catchErrMsg"
	if restStr != "" {
		errVar = tclVarToGo(restStr)
	}
	// Avoid using Go's 'err' (error type) as catch error var
	if errVar == "err" {
		errVar = tclVarToGo("err")
	}
	tp.emitCatchBlock(varName, errVar, bodyStr)
	return true
}

// dbEvalConnPrefix consumes the leading "<conn> eval" prefix (connection
// token "db" optionally followed by digits) from b. Returns the remainder and
// true when the prefix is present.
func dbEvalConnPrefix(b string) (string, bool) {
	i := 0
	if !strings.HasPrefix(b, "db") {
		return "", false
	}
	i = 2
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	rest := strings.TrimSpace(b[i:])
	if !strings.HasPrefix(rest, "eval") {
		return "", false
	}
	return strings.TrimSpace(rest[len("eval"):]), true
}

// bracedSQLBody reports whether rest is exactly one {...} group followed only
// by whitespace; it returns the group's inner text.
func bracedSQLBody(rest string) (string, bool) {
	if len(rest) == 0 || rest[0] != '{' {
		return "", false
	}
	// Find the matching close brace; nothing but whitespace may follow.
	depth := 0
	end := -1
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 || strings.TrimSpace(rest[end+1:]) != "" {
		return "", false
	}
	return rest[1:end], true
}

// singleDbEvalSelectRows reports whether the catch body is exactly one
// "db eval {SELECT ...}" command (any connection: db, db2, ...) with no array
// variable or trailing row script. TCL assigns the query's flattened row
// values to the catch message variable on success (tclsqlite.c: the command
// result is the row list), so emitCatchBlock binds errVar from _res.Rows in
// that case (lock.test 1.21/1.22). Returns false for every other body shape.
func singleDbEvalSelectRows(body string) bool {
	b := strings.TrimSpace(body)
	rest, ok := dbEvalConnPrefix(b)
	if !ok {
		return false
	}
	sqlBody, ok := bracedSQLBody(rest)
	if !ok {
		return false
	}
	sql := strings.ToUpper(strings.TrimSpace(sqlBody))
	return isRowStmtKeyword(sql)
}

// bodyEndsWithExecsqlSelect reports whether the catch body is exactly one
// `execsql {SELECT ...}` statement: its row values are the TCL result the
// `catch` stores in the message var on success (quote-1.3.4: msg = "hello 10").
func bodyEndsWithExecsqlSelect(body string) bool {
	cmds := parseCommands(body)
	if len(cmds) != 1 || len(cmds[0]) < 2 || cmds[0][0].Text != "execsql" {
		return false
	}
	return sqlBatchEndsWithRowStmt(cmds[0][1].Text)
}

// bodyIsExecsql2Select reports whether the catch body is exactly one
// `execsql2 {SELECT ...}` statement: TCL `catch {execsql2 {...}} msg` binds
// the NAME/VALUE pairs of every result row to msg on success (select1-6.x
// checks PRAGMA full_column_names through this shape), not the bare values.
func bodyIsExecsql2Select(body string) bool {
	cmds := parseCommands(body)
	if len(cmds) != 1 || len(cmds[0]) < 2 || cmds[0][0].Text != "execsql2" {
		return false
	}
	return sqlBatchEndsWithRowStmt(cmds[0][1].Text)
}

// skipSQLComment advances i past a `--` comment body up to (not including)
// the terminating newline; the caller's i++ resumes after it.
func skipSQLComment(sqlText string, i int) int {
	for i < len(sqlText) && sqlText[i] != '\n' {
		i++
	}
	return i
}

// isSQLCommentStart reports whether position i begins a `--` SQL comment.
func isSQLCommentStart(sqlText string, i int) bool {
	return sqlText[i] == '-' && i+1 < len(sqlText) && sqlText[i+1] == '-'
}

// isRowStmtKeyword reports whether an upper-cased statement text begins with
// a row-producing keyword (SELECT, WITH, or VALUES).
func isRowStmtKeyword(upperSQL string) bool {
	return strings.HasPrefix(upperSQL, "SELECT") || strings.HasPrefix(upperSQL, "WITH") || strings.HasPrefix(upperSQL, "VALUES")
}

// lastSQLStatementText returns the text of the last statement of a
// multi-statement SQL batch, honoring ' and " quoting and skipping -- comments.
func lastSQLStatementText(sqlText string) string {
	last := ""
	cur := strings.Builder{}
	inS, inD := false, false // ' and " quotes
	for i := 0; i < len(sqlText); i++ {
		c := sqlText[i]
		switch {
		case inS:
			cur.WriteByte(c)
			inS = c != '\''
		case inD:
			cur.WriteByte(c)
			inD = c != '"'
		case c == '\'' || c == '"':
			inS, inD = c == '\'', c == '"'
			cur.WriteByte(c)
		case isSQLCommentStart(sqlText, i):
			// -- comment: skip to end of line
			i = skipSQLComment(sqlText, i)
		case c == ';':
			if strings.TrimSpace(cur.String()) != "" {
				last = cur.String()
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		last = cur.String()
	}
	return last
}

// sqlBatchEndsWithRowStmt reports whether the last statement of a
// multi-statement SQL batch produces rows (SELECT/WITH/VALUES). TCL
// `catch {execsql {END TRANSACTION; SELECT ...}} msg` binds the batch's
// row output to msg (tclsqlite.c appends every statement's rows to the
// command result), so a batch that ENDS in a row-producing statement needs
// the same msg wiring as a single SELECT (trans-4.9).
func sqlBatchEndsWithRowStmt(sqlText string) bool {
	last := strings.ToUpper(strings.TrimSpace(lastSQLStatementText(sqlText)))
	return isRowStmtKeyword(last)
}

// emitCatchBlock transpiles `catch { BODY } MSGVAR` at statement scope,
// assigning the TCL result code (1/0) to varName and the error message
// to errVar.
func (tp *transpiler) emitCatchBlock(varName, errVar, bodyStr string) {
	// Declare variables at function scope (indent 1)
	// so they're accessible from all do_test blocks.
	savedIndent := tp.indent
	tp.indent = 1
	if !tp.isVarDeclared(varName) {
		tp.emitLine("var %s string", varName)
		tp.vars = append(tp.vars, varName)
	}
	tp.emitLine("_ = %s // suppress unused warning", varName)
	// msg is declared at function level in preamble
	if errVar != "msg" && !tp.isVarDeclared(errVar) {
		tp.emitLine("var %s string", errVar)
		tp.vars = append(tp.vars, errVar)
	}
	tp.emitLine("_ = %s // suppress unused warning", errVar)
	tp.indent = savedIndent
	tp.emitLine("{ // catch block")
	tp.indent++
	tp.emitLine("var _catchErr error")
	// Parse and transpile the body
	bodyCmds := parseCommands(bodyStr)
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, quotaCallbacks: tp.quotaCallbacks, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.varCount = bodyTP.varCount
	tp.queryVars = bodyTP.queryVars
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.dbAliases = bodyTP.dbAliases
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	// Copy connection-state maps unconditionally so deletes (a reopen clearing
	// a prior closed/failed-open state) propagate to the outer transpiler.
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	// After body, set result and error message
	tp.emitLine("if _catchErr != nil {")
	tp.indent++
	tp.emitLine("%s = \"1\"", varName)
	tp.emitLine("%s = _catchErr.Error()", errVar)
	tp.indent--
	tp.emitLine("} else {")
	tp.indent++
	tp.emitLine("%s = \"0\"", varName)
	// TCL `catch {db eval {SELECT...}} msg` assigns the query RESULT (the
	// flattened row values) to msg on success (tclsqlite.c: the command
	// result is the row list). When the body is exactly one db-eval SELECT,
	// bind errVar from the captured rows (lock.test 1.21/1.22); the
	// `execsql {SELECT ...}` form routes through the standard query var r
	// (quote-1.3.4).
	if singleDbEvalSelectRows(bodyStr) {
		tp.emitLine("%s = tclRowValuesFlat(_res)", errVar)
	} else if bodyIsExecsql2Select(bodyStr) {
		tp.emitLine("%s = tclRowNamesValuesFlat(r)", errVar)
	} else if bodyEndsWithExecsqlSelect(bodyStr) {
		tp.emitLine("%s = tclRowValuesFlat(r)", errVar)
	} else {
		tp.emitLine("%s = \"\"", errVar)
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processSetTimeValue handles `set var [time { SCRIPT }]` — transpile the
// inner script and bind the variable to "".
func (tp *transpiler) processSetTimeValue(goName, bracketText string) {
	cmdText := strings.TrimPrefix(bracketText, "[")
	cmdText = strings.TrimSuffix(cmdText, "]")
	cmdText = strings.TrimSpace(strings.TrimPrefix(cmdText, "time"))
	if bodyStr, _, ok := findBracedBody(cmdText); ok {
		bodyCmds := parseCommands(bodyStr)
		bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, varCount: tp.varCount}
		bodyTP.processCommands(bodyCmds)
		tp.indent = bodyTP.indent
	}
	tp.assignSetValue(goName, `""`)
}

// processSetLindexTimeValue handles `set var [lindex [time { SCRIPT }] N]` —
// the timing command wraps a script (usually a db eval) as
// `[lindex [time {...}] 0]`. Transpile the inner script as statements and bind
// the variable to "0".
func (tp *transpiler) processSetLindexTimeValue(goName, bracketText string) {
	cmdText := strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")
	// cmdText now: lindex [time {SCRIPT}] N
	if timeIdx := strings.Index(cmdText, "[time "); timeIdx >= 0 {
		afterTime := cmdText[timeIdx+len("[time "):]
		if bodyStr, _, ok := findBracedBody(afterTime); ok {
			bodyCmds := parseCommands(bodyStr)
			bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, varCount: tp.varCount}
			bodyTP.processCommands(bodyCmds)
			tp.indent = bodyTP.indent
		}
	}
	tp.assignSetValue(goName, `"0"`)
}

// trackSetBareConstValue records simple bare-word constants (set var value)
// so later commands can resolve the value statically — e.g. `set db_dest db2`
// followed by `execsql {SQL} $db_dest` resolves the connection to db2.
func (tp *transpiler) trackSetBareConstValue(goName string, rest []tcl.RawWord) {
	if len(rest) < 1 || rest[0].Braced || rest[0].Quoted ||
		strings.HasPrefix(rest[0].Text, "[") || strings.HasPrefix(rest[0].Text, "$") {
		return
	}
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = rest[0].Text
}

// trackSetQuotedValue records quoted string assignments so later commands
// (e.g. the colmeta.test `set tests "concat $tests {LIST}"` accumulation) can
// resolve the variable's constant list value.
func (tp *transpiler) trackSetQuotedValue(goName string, rest []tcl.RawWord) {
	if len(rest) < 1 || !rest[0].Quoted {
		return
	}
	if tp.varConstValues == nil {
		tp.varConstValues = make(map[string]string)
	}
	tp.varConstValues[goName] = tclUnescapeQuoted(rest[0].Text)
}

// trackSetQueryVarValue tracks variables whose assigned value is (or begins
// with) a query statement, so `execsql $var` bodies can be recognized as
// queries. Also records braced SQL constants in a dedicated map so
// sqlite3_prepare can classify a $var SQL (capi3-1.7 prepares `SELECT namex
// ...` via a $sql variable) without disturbing the varConstValues concat/list
// machinery.
func (tp *transpiler) trackSetQueryVarValue(goName string, args, rest []tcl.RawWord) {
	if len(rest) < 1 {
		return
	}
	if rest[0].Braced {
		tp.markQueryVar(args[0].Text, rest[0].Text)
		if !strings.HasPrefix(strings.TrimSpace(rest[0].Text), "$") {
			if tp.sqlVarValues == nil {
				tp.sqlVarValues = make(map[string]string)
			}
			tp.sqlVarValues[goName] = strings.TrimSpace(rest[0].Text)
		}
		return
	}
	tp.markQueryVar(args[0].Text, rest[0].Text)
}

// trackSetTestPrefix tracks `set testprefix NAME` so the skipTests lookup can
// resolve bare test names (e.g. whereF's "4.0") to their TCL-effective names
// ("whereF-4.0"), matching tester.tcl's prefixing. This keeps generic
// keys like "4.0" from colliding across packages.
func (tp *transpiler) trackSetTestPrefix(args, rest []tcl.RawWord) {
	if args[0].Text == "testprefix" && len(rest) >= 1 && !rest[0].Braced {
		tp.testPrefix = strings.TrimSpace(rest[0].Text)
	}
}

// processSetGeneric handles the plain `set var value` assignment: build the
// value expression, track query vars / testprefix, and emit the assignment.
func (tp *transpiler) processSetGeneric(goName string, args, rest []tcl.RawWord) {
	valueExpr := tp.varValueExpr(rest)
	tp.trackSetBareConstValue(goName, rest)
	tp.trackSetQuotedValue(goName, rest)
	tp.trackSetQueryVarValue(goName, args, rest)
	tp.trackSetTestPrefix(args, rest)
	tp.assignSetValue(goName, valueExpr)
}
