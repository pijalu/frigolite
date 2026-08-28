// Package main implements the tcl2go tool.
//
// This file handles do_execsql_test / do_catchsql_test and expected-error
// helpers.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// ---- Test pattern handlers ----

func (tp *transpiler) processDoExecSQLTest(args []tcl.RawWord) {
	dbConn, args := tp.resolveTestDBConnection(args)
	if len(args) < 2 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	sqlExpr := `""`
	if len(args) >= 2 {
		// do_execsql_test SQL is evaluated by TCL's uplevel/db eval: $var
		// references are bound as VALUES (not SQL text), so render them as
		// SQL literals via buildSQLStringExpr (see collectSQLExpression).
		sqlExpr = tp.collectSQLExpression(args[1:2])
	}
	expectedExpr := `""`
	if len(args) >= 3 {
		// A TCL associative-array element reference in the expected value
		// ($map($res), $arr($key)) cannot be transpiled: the transpiler maps
		// `set map(K) V` to a Go variable map_K but has no runtime lookup for
		// a dynamic key. SQLite's tests use this for expected-value dispatch
		// (rowvalue 2.x where1/where2). Skip with N-A evidence.
		if tclArrayElementRef(args[2].Text) != "" {
			tp.emitSkippedTestSideEffects("do_execsql_test", args, args[0].Text, "TCL associative-array expected-value lookup not transpiled N-A")
			return
		}
		if expr, ok := tp.expectedStringExpr(args[2]); ok {
			expectedExpr = expr
		} else {
			expectedExpr = tp.expectLiteral(args[2])
		}
	}
	expectedExpr = tp.resolveExecExpectedExpr(expectedExpr, args)
	if ov := wantOverride(overrideFile(tp), args[0].Text); ov != "" {
		expectedExpr = tp.expectLiteral(tcl.RawWord{Text: ov, Braced: true})
	}

	sql := ""
	if len(args) >= 2 {
		sql = args[1].Text
		// `[subst -novar { SQL }]` — resolve to the inner SQL text so query
		// detection (isQueryStmt) and unsupportedSQL see the real statement,
		// not the "-novar {" wrapper.
		if resolved, ok := substNovarBody(sql); ok {
			sql = resolved
		}
		// `[subst {BODY}]` — resolve the body's \uXXXX / \xXX unicode
		// escapes so the SQL carries the literal characters (func-30.x:
		// [subst {SELECT unicode('\\u00A2');}] → unicode('¢')).
		if resolved, ok := substUnescapeBody(sql); ok {
			sql = resolved
		}
	}
	if reason := unsupportedSQL(sanitizeSQL(sql)); reason != "" {
		tp.emitSkippedExec(nameExpr, dbConn, args, reason)
		return
	}

	tp.emitLine("{ // %s", nameExpr)
	tp.indent++
	tp.emitExecSQLTestBody(nameExpr, dbConn, sqlExpr, expectedExpr, sql, args)
	tp.indent--
	tp.emitLine("}")
}

// resolveTestDBConnection handles the optional "-db NAME" prefix of
// do_execsql_test (e.g. "-db db2") selecting a different connection. When
// NAME is a real connection (a frigolite.Open on its own file, e.g. "sqlite3
// db2 test.db2"), run the SQL on that handle; when NAME is aliased to main
// ("sqlite3 db2 test.db"), run on db. The connection name is resolved the
// same way as processExecSQL so both forms agree. Returns the connection and
// the remaining args after the prefix.
func (tp *transpiler) resolveTestDBConnection(args []tcl.RawWord) (string, []tcl.RawWord) {
	dbConn := "db"
	if len(args) >= 2 && args[0].Text == "-db" {
		h := tclVarToGo(args[1].Text)
		if h != "" && h != "db" && (isPreDeclaredDB(h) || tp.isVarDeclared(h)) {
			if target, ok := tp.dbAliases[h]; ok {
				dbConn = target
			} else {
				dbConn = h
			}
		}
		args = args[2:]
	}
	return dbConn, args
}

// resolveExecExpectedExpr applies the transpile-time expected-value
// normalizations: [int2str N], [list $var], and [subst {BODY}] forms.
func (tp *transpiler) resolveExecExpectedExpr(expectedExpr string, args []tcl.RawWord) string {
	if len(args) < 3 {
		return expectedExpr
	}
	// TCL "[int2str N]" command substitutions in the expected value (the
	// test-harness int2str proc) are evaluated at transpile time (tempdb2's
	// want is three concatenated int2str results).
	if ev, ok := evalInt2strExpected(args[2].Text); ok {
		return tp.goStringLiteral(tcl.RawWord{Text: ev})
	}
	// TCL "[list $var]" expected values render as the runtime list variable
	// (the list command wraps the value in list syntax; flatten() of the
	// query result produces the same space-joined form).
	if varExpr, ok := tp.listVarExpected(args[2].Text); ok {
		return varExpr
	}
	// TCL "[subst {BODY}]" expected values: evaluate the subst at transpile
	// time when the body has no $var refs (unicode escapes \u00XX become the
	// literal characters; func-30.4 expects [subst {$\u00A2\u20AC}] = "$¢€").
	if lit, ok := substLiteralExpected(args[2].Text); ok {
		return tp.goStringLiteral(tcl.RawWord{Text: lit})
	}
	// TCL "[concat $arr(1) $arr(2) ...]" expected values: join the array
	// element values with spaces (e_fts3 1.7.x concat $R(...) forms).
	if expr, ok := tp.concatArrayExpected(args[2].Text); ok {
		return expr
	}
	return expectedExpr
}

// concatArrayExpected handles a TCL `[concat $arr(K1) $arr(K2) ...]` expected
// value whose elements are array references. Returns a Go expression joining
// the element values with spaces (TCL list rendering), or ("", false) when the
// form does not match.
func (tp *transpiler) concatArrayExpected(rawText string) (string, bool) {
	text := strings.TrimSpace(rawText)
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if !strings.HasPrefix(text, "concat ") {
		return "", false
	}
	inner := strings.TrimSpace(text[len("concat "):])
	fields := strings.Fields(inner)
	if len(fields) == 0 {
		return "", false
	}
	var parts []string
	for _, f := range fields {
		if !strings.HasPrefix(f, "$") {
			return "", false
		}
		expr := tp.buildStringExpr(f)
		if expr == "" {
			return "", false
		}
		parts = append(parts, expr)
	}
	return strings.Join(parts, " + \" \" + "), true
}

// emitSkippedExec emits a no-assertion exec for SQL the engine cannot verify
// (e.g. window functions), running it for side effects only.
func (tp *transpiler) emitSkippedExec(nameExpr, dbConn string, args []tcl.RawWord, reason string) {
	// Run the statement for its side effects (e.g. CREATE VIEW used by
	// later subtests) but do not assert results — the engine cannot
	// verify the window-function semantics. Errors are ignored so
	// unsupported SQL never fails the skipped test.
	tp.emitLine("{ // %s — skipped: %s", nameExpr, reason)
	tp.indent++
	tp.emitLine("_res = %s.Exec(%s)", dbConn, tp.collectSQLExpression(args[1:2]))
	tp.emitLine("_ = _res")
	tp.indent--
	tp.emitLine("}")
}

// emitExecSQLTestBody emits the query-vs-exec comparison for a
// do_execsql_test, dispatching on the statement batch shape and expected
// value.
func (tp *transpiler) emitExecSQLTestBody(nameExpr, dbConn, sqlExpr, expectedExpr, sql string, args []tcl.RawWord) {
	// Multi-statement SQL is passed to db.Query/db.Exec as a single batch
	// (T1.4 behavior): db.Query executes every statement and concatenates
	// all returned rows, matching TCL `db eval` semantics where the expected
	// value is the rows of every result-producing statement (e.g.
	// "SELECT ...; PRAGMA integrity_check" → rows + "ok"). Splitting the
	// batch on ';' would corrupt CREATE TRIGGER bodies (BEGIN ... END).
	stmts := splitSQLStatements(sql)
	hasQuery := false
	for _, st := range stmts {
		if isQueryStmt(st) {
			hasQuery = true
			break
		}
	}

	if expectedExpr != `""` && isErrExpectation(expectedExpr) {
		tp.emitExpectedErrorExec(dbConn, sqlExpr, expectedExpr)
		return
	}
	if hasQuery && expectedExpr != `""` {
		tp.emitExpectedQueryResult(dbConn, sqlExpr, expectedExpr, args)
		return
	}
	if hasQuery {
		tp.emitLine("r = %s.Query(%s)", dbConn, sqlExpr)
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("}")
		return
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")
}

// emitExpectedErrorExec emits a do_execsql_test that expects a failure with a
// specific message (the "1 {msg}" form, e.g. RAISE() outside a trigger).
func (tp *transpiler) emitExpectedErrorExec(dbConn, sqlExpr, expectedExpr string) {
	errMsg := extractExpectedErrorFromLiteral(expectedExpr)
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %q) {", errMsg)
	tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %q, _res.Error, %s)", errMsg, sqlExpr)
	tp.emitLine("}")
}

// emitExpectedQueryResult emits a do_execsql_test query-path comparison: run
// the batch via db.Query, flatten, and compare with the expected value
// (handling regex patterns, runtime db-eval expectations, and list
// normalization).
func (tp *transpiler) emitExpectedQueryResult(dbConn, sqlExpr, expectedExpr string, args []tcl.RawWord) {
	tp.emitLine("r = %s.Query(%s)", dbConn, sqlExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := flatten(r)")
	// TCL do_test expectations may be regex patterns (e.g. /B-TREE/ or
	// ~/SCAN/ used by do_eqp_test). Detect the ~/.../ or /.../ form and
	// emit a regexp.MatchString comparison instead of literal equality.
	if isTCLRegexPattern(expectedExpr) {
		tp.emitRegexPatternComparison(expectedExpr)
		return
	}
	if dbEvalSQL, isSubst, quoted, ok := dbEvalExpected(args[2]); ok {
		tp.emitRuntimeDBEvalComparison(dbConn, expectedExpr, dbEvalSQL, isSubst, quoted)
		return
	}
	// When the expected value is a TCL list variable (bare identifier) or a
	// literal containing braced sub-lists, normalize it: multi-row expectations
	// hold list braces that flatten() does not produce (TCL list equality is
	// brace- and whitespace-insensitive).
	if tp.expectPreFlattened {
		// normalizeExpectedWord already produced the final flat form;
		// runtime tclListFlatten would strip the data braces a second time.
		tp.emitLine("want := %s", expectedExpr)
	} else if isBareGoIdent(expectedExpr) || strings.Contains(expectedExpr, "{") {
		// A single fully-braced element whose inner content is itself
		// structured (JSON objects/arrays, nested braces, quotes, colons)
		// equals flatten()'s raw cell output verbatim — TCL-list flattening
		// would wrongly strip the JSON's own braces.
		if isSingleBracedStructuredLiteral(expectedExpr) {
			tp.emitLine("want := %s", expectedExpr)
		} else {
			tp.emitLine("want := tclListFlatten(%s)", expectedExpr)
		}
	} else if strings.Contains(expectedExpr, `\n`) {
		// Multi-line expected results are TCL lists whose separators include
		// newlines/indentation; normalize like flatten() output.
		tp.emitLine("want := tclListFlattenCollapse(%s)", expectedExpr)
		tp.emitLine("got = tclListFlattenCollapse(got)")
	} else {
		tp.emitLine("want := %s", expectedExpr)
	}
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// emitRegexPatternComparison emits a regexp.MatchString comparison against a
// TCL regex-pattern expected value.
func (tp *transpiler) emitRegexPatternComparison(expectedExpr string) {
	patternExpr := regexPatternExpr(expectedExpr)
	tp.emitLine("wantPattern := %s", patternExpr)
	if regexPatternNegated(expectedExpr) {
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

// emitRuntimeDBEvalComparison emits a comparison where the expected value is
// itself a runtime db eval ([db eval { SQL }] or [db eval [subst -novar { SQL
// }]]).
func (tp *transpiler) emitRuntimeDBEvalComparison(dbConn, expectedExpr, dbEvalSQL string, isSubst, quoted bool) {
	// [db eval { SQL }] or [db eval [subst -novar { SQL }]] — run the
	// query at runtime for the expected value. SQL with $var/[cmd]
	// refs must be rendered as a Go string expression (the plain
	// braced form binds $var as SQL literals; the subst -novar form
	// renders [cmd] raw and $var as SQL literals; the double-quoted
	// form substitutes $var as RAW TEXT).
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
	tp.emitLine("%s := %s.Query(%s)", wantVar, dbConn, dbEvalExpr)
	tp.emitLine("if %s.Error != nil {", wantVar)
	tp.emitLine("\tt.Errorf(\"expected query error: %%v\\n  sql: %%s\", %s.Error, %s)", wantVar, dbEvalExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("want := flatten(%s)", wantVar)
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// isErrExpectation reports whether a do_execsql_test/do_catchsql_test
// expected literal is an error expectation of the form "1 {message}"
// (SQLite error code 1 followed by a braced message). Result expectations
// such as "1 2 3" do not contain braces and are NOT error expectations.
// A multi-column result row whose first value is 1 and that contains NULL
// cells (e.g. "1 {} {} ...") also starts with "1 {" and ends with "}", but
// is a row result, not an error: an error expectation is exactly a two-element
// list ("1" plus one braced message), and the message is never empty.
func isErrExpectation(expected string) bool {
	raw, err := strconv.Unquote(expected)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(raw, "1 ") {
		return false
	}
	rest := strings.TrimSpace(raw[2:])
	if !strings.HasPrefix(rest, "{") || !strings.HasSuffix(rest, "}") {
		return false
	}
	elems := tclSplitList(raw)
	if len(elems) != 2 {
		return false
	}
	return strings.TrimSpace(elems[1]) != ""
}

// catchsqlPresenceVar detects a do_catchsql_test expected value of the form
// `[list [expr {$VAR!=""}] $VAR]` — a runtime list whose first element is 1
// exactly when the error variable is non-empty. Returns the Go variable name
// (or ""). The TCL pattern builds "0 {}" (success) or "1 {msg}" (error).
func catchsqlPresenceVar(args []tcl.RawWord) string {
	if len(args) < 3 {
		return ""
	}
	text := strings.TrimSpace(args[2].Text)
	if !strings.HasPrefix(text, "[list ") || !strings.HasSuffix(text, "]") {
		return ""
	}
	inner := strings.TrimSpace(text[len("[list ") : len(text)-1])
	parts := tclCmdWords(inner)
	if len(parts) != 2 {
		return ""
	}
	// First element: [expr {$VAR!=""}] (or {[expr {$VAR!=""}]}).
	expr := strings.TrimSpace(parts[0])
	expr = strings.TrimPrefix(expr, "{")
	expr = strings.TrimSuffix(expr, "}")
	if !strings.HasPrefix(expr, "[expr {") || !strings.HasSuffix(expr, "}]") {
		return ""
	}
	cond := expr[len("[expr {") : len(expr)-2]
	if !strings.Contains(cond, "!=\"\"") {
		return ""
	}
	// Extract the variable name from the condition and confirm the second
	// element references the same variable.
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

func extractExpectedErrorFromLiteral(expected string) string {
	// expected is a Go-quoted string literal (e.g. "1 {near \"#1\": syntax error}").
	// Unquote it so embedded escapes resolve to real characters, then extract
	// the message after the leading "1 " (SQLite error code + message).
	raw, err := strconv.Unquote(expected)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(raw, "1 ") {
		return ""
	}
	msg := strings.TrimSpace(raw[2:])
	msg = strings.Trim(msg, "{}")
	return strings.TrimSpace(msg)
}

// substBracedBody extracts the body of a "[subst {BODY}]" form, returning the
// inner text (without the surrounding braces). Returns ok=false when the text
// is not that form.
func substBracedBody(rawText string) (string, bool) {
	text := strings.TrimSpace(rawText)
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if !strings.HasPrefix(text, "subst ") {
		return "", false
	}
	body := strings.TrimSpace(text[len("subst "):])
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(body, "}") {
		return "", false
	}
	return body[1 : len(body)-1], true
}

// substUnescapeText resolves \uXXXX / \xXX unicode escapes to literal
// characters (the $var refs, if any, are left for later substitution).
func substUnescapeText(body string) string {
	var sb strings.Builder
	for i := 0; i < len(body); i++ {
		if next, consumed, ok := substEscapeAt(body, i); ok {
			sb.WriteRune(next)
			i += consumed
			continue
		}
		sb.WriteByte(body[i])
	}
	return sb.String()
}

// substEscapeAt resolves a \uXXXX or \xXX escape at position i (the
// backslash). Returns the rune, the number of additional positions consumed
// past i, and whether an escape was present.
func substEscapeAt(body string, i int) (rune, int, bool) {
	if body[i] != '\\' || i+1 >= len(body) {
		return 0, 0, false
	}
	n := body[i+1]
	if n == 'u' && i+6 <= len(body) {
		if r, err := strconv.ParseUint(body[i+2:i+6], 16, 32); err == nil {
			return rune(r), 5, true
		}
	}
	if n == 'x' && i+4 <= len(body) {
		if r, err := strconv.ParseUint(body[i+2:i+4], 16, 32); err == nil {
			return rune(r), 3, true
		}
	}
	return 0, 0, false
}

// substUnescapeBody detects the TCL "[subst {BODY}]" SQL form and returns
// the BODY with \uXXXX / \xXX unicode escapes resolved to literal characters
// (the $var refs, if any, are left for later substitution). Returns ok=false
// when the text is not that form.
func substUnescapeBody(rawText string) (string, bool) {
	body, ok := substBracedBody(rawText)
	if !ok {
		return "", false
	}
	return substUnescapeText(body), true
}

// substLiteralExpected detects the TCL "[subst {BODY}]" expected-value form
// and returns the substituted literal when BODY has no $var/[cmd] references
// (only \u / \x unicode escapes and plain text). Returns ok=false otherwise.
func substLiteralExpected(rawText string) (string, bool) {
	body, ok := substBracedBody(rawText)
	if !ok {
		return "", false
	}
	if strings.Contains(body, "[") {
		// A [cmd] would need runtime evaluation; not a literal.
		return "", false
	}
	return substUnescapeText(body), true
}

// listVarExpected detects the TCL "[list $var]" (or "list $var") expected
// value form used by do_execsql_test / do_test (e.g. [list $after] where
// $after is a foreach list variable). It returns the Go variable expression
// for the runtime value, or ("", false) when the form does not match.
func (tp *transpiler) listVarExpected(rawText string) (string, bool) {
	text := strings.TrimSpace(rawText)
	// Accept both "[list $var]" and the bracket-stripped "list $var" form
	// (the TCL tokenizer may consume the outer [ ]).
	if strings.HasPrefix(text, "[list ") && strings.HasSuffix(text, "]") {
		text = strings.TrimSpace(text[1 : len(text)-1]) // strip [ ]
	}
	if !strings.HasPrefix(text, "list ") {
		return "", false
	}
	inner := strings.TrimSpace(text[len("list "):])
	if inner == "" || !strings.HasPrefix(inner, "$") {
		return "", false
	}
	// Render the $var as the Go variable expression.
	expr := tp.buildStringExpr(inner)
	return expr, true
}

// listExpectedErrorMsg detects the TCL "[list 1 <msg>]" form used as a
// do_catchsql_test expected value (SQLite error code 1 plus a message that
// may interpolate $vars at runtime). It returns the message as a Go string
// expression (with $var rendered as Go variables), or ("", false) when the
// form does not match.
func (tp *transpiler) listExpectedErrorMsg(rawText string) (string, bool) {
	text := strings.TrimSpace(rawText)
	if !strings.HasPrefix(text, "[list ") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSpace(text[len("[list ") : len(text)-1])
	if inner == "" {
		return "", false
	}
	// Handle the TCL {*} list-splice form: [list {*}{1 <msg>}] evaluates to
	// the same two-element list as [list 1 <msg>]. Strip the {*} marker and
	// the surrounding braces of the spliced list.
	if strings.HasPrefix(inner, "{*}") {
		inner = strings.TrimSpace(inner[len("{*}"):])
		if len(inner) >= 2 && inner[0] == '{' && inner[len(inner)-1] == '}' {
			inner = strings.TrimSpace(inner[1 : len(inner)-1])
		}
	}
	if !strings.HasPrefix(inner, "1") {
		return "", false
	}
	rest := strings.TrimSpace(inner[1:])
	if rest == "" {
		return "", false
	}
	// The message is a TCL word: strip outer double-quotes or braces and
	// resolve TCL quoted escapes before interpolation.
	msg := stripExpectedMsgQuotes(rest)
	return tp.buildStringExpr(msg), true
}

// stripExpectedMsgQuotes strips outer double-quote or brace delimiters from a
// TCL message word and resolves TCL quoted escapes.
func stripExpectedMsgQuotes(msg string) string {
	if len(msg) >= 2 && msg[0] == '"' && msg[len(msg)-1] == '"' {
		msg = msg[1 : len(msg)-1]
		msg = tclUnescapeQuoted(msg)
	} else if len(msg) >= 2 && msg[0] == '{' && msg[len(msg)-1] == '}' {
		msg = msg[1 : len(msg)-1]
	}
	return msg
}

func (tp *transpiler) processDoCatchSQLTest(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	sqlExpr := `""`
	if len(args) >= 2 {
		// do_catchsql_test SQL is evaluated by TCL's uplevel/db eval: $var
		// references are bound as VALUES, rendered as SQL literals here.
		sqlExpr = tp.collectSQLExpression(args[1:2])
	}
	expectedExpr := `""`
	if len(args) >= 3 {
		expectedExpr = tp.expectLiteral(args[2])
	}

	sql := ""
	if len(args) >= 2 {
		sql = args[1].Text
	}
	if reason := unsupportedSQL(sanitizeSQL(sql)); reason != "" {
		// Run the statement for its side effects (e.g. CREATE TABLE used by
		// later subtests) but do not assert results or errors. See
		// processDoExecSQLTest for the rationale.
		tp.emitLine("{ // %s — skipped: %s", nameExpr, reason)
		tp.indent++
		tp.emitLine("_res = db.Exec(%s)", tp.collectSQLExpression(args[1:2]))
		tp.emitLine("_ = _res")
		tp.indent--
		tp.emitLine("}")
		return
	}

	tp.emitLine("{ // %s", nameExpr)
	tp.indent++

	tp.emitCatchSQLComparison(nameExpr, sqlExpr, expectedExpr, args, tp.resolveSQLConnection(args[1:]))

	tp.indent--
	tp.emitLine("}")
}

// processFTSWriteTest handles e_fts3.test's `write_test tn tbl sql` wrapper
// proc. In the TCL source this is
// `uplevel [list do_write_test e_fts3-$tn $tbl $sql]`; with DO_MALLOC_TEST=0
// do_write_test reduces to executing the SQL and asserting success
// (catchsql returns {0 {}}). Emit exactly that — the SQL is args[2].
func (tp *transpiler) processFTSWriteTest(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	tp.emitFTSWriteExec(args[0], args[2])
}

// processFTSDDLTest handles e_fts3.test's `ddl_test tn ddl` wrapper proc
// (do_write_test against the sqlite_master table — same success assertion,
// SQL is args[1]).
func (tp *transpiler) processFTSDDLTest(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	tp.emitFTSWriteExec(args[0], args[1])
}

// emitFTSWriteExec emits `db.Exec(sql)` with a success assertion for the
// write_test/ddl_test wrapper procs. nameWord is the test-number word and
// sqlWord the SQL body word.
func (tp *transpiler) emitFTSWriteExec(nameWord, sqlWord tcl.RawWord) {
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{sqlWord})
	tp.emitLine("{ // %s", tp.goStringLiteral(nameWord))
	tp.indent++
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processFTSReadTest handles e_fts3.test's `read_test tn sql result` wrapper
// proc (do_read_test: run the SQL and compare the result rows).
func (tp *transpiler) processFTSReadTest(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	// Skip advanced-FTS tests (matchinfo/snippet/offsets and old-parens
	// behavior) recorded in skipTests under the e_fts3-<tn> name; the engine
	// implements these features in later phases (P6.FTS-D/E/H) or matches
	// modern SQLite precedence instead of the 2007 test expectation.
	if reason, ok := skipTestReason("e_fts3-" + args[0].Text); ok {
		tp.emitSkippedTest("e_fts3-"+args[0].Text, reason)
		return
	}
	sqlExpr := tp.collectSQLExpression(args[1:2])
	expectedExpr := `""`
	if expr, ok := tp.expectedStringExpr(args[2]); ok {
		expectedExpr = expr
	} else {
		expectedExpr = tp.expectLiteral(args[2])
	}
	expectedExpr = tp.resolveExecExpectedExpr(expectedExpr, args)
	sql := args[1].Text
	if reason := unsupportedSQL(sanitizeSQL(sql)); reason != "" {
		tp.emitSkippedExec(tp.goStringLiteral(args[0]), "db", args, reason)
		return
	}
	tp.emitLine("{ // %s", tp.goStringLiteral(args[0]))
	tp.indent++
	tp.emitExecSQLTestBody(tp.goStringLiteral(args[0]), "db", sqlExpr, expectedExpr, sql, args)
	tp.indent--
	tp.emitLine("}")
}

// processFTSErrorTest handles e_fts3.test's `error_test tn sql result` wrapper
// proc (do_error_test: run the SQL and assert it fails with the given error).
func (tp *transpiler) processFTSErrorTest(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	sqlExpr := tp.collectSQLExpression(args[1:2])
	expectedExpr := tp.expectLiteral(args[2])
	tp.emitLine("{ // %s", tp.goStringLiteral(args[0]))
	tp.indent++
	tp.emitCatchSQLComparison(tp.goStringLiteral(args[0]), sqlExpr, expectedExpr, args, "db")
	tp.indent--
	tp.emitLine("}")
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
	if msgVar := catchsqlPresenceVar(args); msgVar != "" {
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if %s == \"\" {", msgVar)
		tp.emitLine("\tif _res.Error != nil {")
		tp.emitLine("\t\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("\t}")
		tp.emitLine("} else {")
		tp.emitLine("\tif _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", msgVar)
		tp.emitLine("\t\tt.Errorf(\"expected error containing %%s, got: %%v\\n  sql: %%s\", %s, _res.Error, %s)", msgVar, sqlExpr)
		tp.emitLine("\t}")
		tp.emitLine("}")
		return
	}
	errMsg := extractExpectedErrorFromLiteral(expectedExpr)
	raw, _ := strconv.Unquote(expectedExpr)
	expectSuccess := !strings.HasPrefix(raw, "1 ")
	errMsgDynamic := ""
	// A bare TCL variable expected value (do_catchsql_test NAME SQL $err):
	// render the variable's Go value at runtime so the leading "1 " error
	// marker is detected dynamically.
	if len(args) >= 3 && strings.HasPrefix(strings.TrimSpace(args[2].Text), "$") {

		dynamic := tp.buildStringExpr(strings.TrimSpace(args[2].Text))
		raw = ""
		expectSuccess = false
		errMsg = ""
		// The variable holds the TCL catchsql result ("1 {msg}" or "0 {}");
		// use the count-aware runtime comparison so a success expectation
		// ("0 {}") is checked as success, not as an empty error message.
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if !tclCatchsqlMatches(_res, %s) {", dynamic)
		tp.emitLine("\tt.Errorf(\"catchsql mismatch\\n  got:  [%%v]\\n  want: [%%s]\\n  sql: %%s\", _res.Error, %s, %s)", dynamic, sqlExpr)
		tp.emitLine("}")
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
	if expectSuccess && args[2].Quoted && strings.HasPrefix(strings.TrimSpace(args[2].Text), "1 {") &&
		strings.Contains(args[2].Text, "$") {
		msg := strings.TrimSpace(args[2].Text)
		msg = strings.TrimSpace(msg[2:]) // drop "1 "
		msg = strings.Trim(msg, "{}")
		expectSuccess = false
		errMsgDynamic = tp.buildStringExpr(msg)
	}
	// TCL catchsql regex form "/1 {near .* syntax error}/" (with2 6.7-6.9) or
	// "/1.*too big.*/" (basexx1 118-119): the message is a regex the error
	// must match. Detect the leading "/1" (space optional) and trailing "/"
	// and emit a regexp.MatchString comparison.
	if strings.HasPrefix(raw, "/1") && strings.HasSuffix(raw, "/") {
		pattern := strings.TrimSpace(strings.TrimSuffix(raw, "/"))
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "/1"))
		// The pattern is TCL brace-quoted (e.g. "/1 {near .* syntax error}/");
		// strip the surrounding { } so the emitted Go regex is valid.
		pattern = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(pattern, "{"), "}"))
		pattern = strings.ReplaceAll(pattern, `\y`, `\b`)
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error == nil || !func() bool { m, _ := regexp.MatchString(%q, _res.Error.Error()); return m }() {", pattern)
		tp.emitLine("\tt.Errorf(\"expected error matching %s, got: %%v\\n  sql: %%s\", %q, _res.Error, %s)", `%q`, pattern, sqlExpr)
		tp.emitLine("}")
		return
	}
	if expectSuccess {
		// TCL do_catchsql_test {0 {}} — the statement is expected to succeed.
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"expected success, got error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("}")
		return
	}
	if errMsgDynamic != "" {
		// Dynamic error message: the expected text is a runtime Go expression
		// (e.g. the loop variable `_error` holding "row value misused").
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", errMsgDynamic)
		tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %s, _res.Error, %s)", errMsgDynamic, sqlExpr)
		tp.emitLine("}")
		return
	}
	if errMsg != "" {
		tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
		tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %q) {", errMsg)
		tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %q, _res.Error, %s)", errMsg, sqlExpr)
		tp.emitLine("}")
		return
	}
	tp.emitLine("_res = %s.Exec(%s)", dbConn, sqlExpr)
	tp.emitLine("if _res.Error == nil {")
	tp.emitLine("\tt.Errorf(\"expected error, got none\\n  sql: %%s\", %s)", sqlExpr)
	tp.emitLine("}")
}

// processDoChangesTest handles `do_changes_test {tn} {sql} {res}` — the
// e_changes.test wrapper proc that runs the SQL and appends [db changes] to
// the execsql result (the changes() count after the statement).
func (tp *transpiler) processDoChangesTest(args []tcl.RawWord) {
	tp.processDoChangesLikeTest(args, "Changes")
}

// processDoTCtest handles `do_tc_test {tn} {sql} {res}` — the
// e_totalchanges.test wrapper proc that runs the SQL and appends
// [db total_changes] to the execsql result.
func (tp *transpiler) processDoTCtest(args []tcl.RawWord) {
	tp.processDoChangesLikeTest(args, "TotalChanges")
}

// processDoPreupdateTest handles `do_preupdate_test {tn} {sql} {res}` — the
// hook2.test wrapper proc: reset ::preupdate to an empty list, run the SQL,
// and compare the resulting preupdate-hook log (type db table rowid rowid2
// [old values] [new values] per row) with the expected argument.
func (tp *transpiler) processDoPreupdateTest(args []tcl.RawWord) {
	if len(args) < 3 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	sqlExpr := tp.collectSQLExpression(args[1:2])
	expectedExpr := `""`
	if expr, ok := tp.expectedStringExpr(args[2]); ok {
		expectedExpr = expr
	} else {
		expectedExpr = tp.expectLiteral(args[2])
	}
	tp.emitLine("{ // %s (preupdate)", nameExpr)
	tp.indent++
	tp.emitLine("preupdate = \"\"")
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"preupdate exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")
	tp.emitLine("if tclListFlatten(preupdate) != strings.Join(tclSplitList(%s), \" \") {", expectedExpr)
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\\n  body: do_preupdate_test %%s\", tclListFlatten(preupdate), strings.Join(tclSplitList(%s), \" \"), %s)", expectedExpr, nameExpr)
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

// processDoChangesLikeTest implements do_changes_test / do_tc_test: run the
// SQL batch, append the connection's changes (or total_changes) count, and
// compare the combined value with the expected argument. The change counter is
// read AFTER the statement completes (SQLite's sqlite3_changes returns the
// last statement's count).
func (tp *transpiler) processDoChangesLikeTest(args []tcl.RawWord, counter string) {
	if len(args) < 3 {
		return
	}
	nameExpr := tp.goStringLiteral(args[0])
	sqlExpr := tp.collectSQLExpression(args[1:2])
	expectedExpr := `""`
	if expr, ok := tp.expectedStringExpr(args[2]); ok {
		expectedExpr = expr
	} else {
		expectedExpr = tp.expectLiteral(args[2])
	}
	tp.emitLine("{ // %s", nameExpr)
	tp.indent++
	// Run the SQL batch (db.Query handles multi-statement batches, including
	// DDL + DML + SELECT; the execsql result rows are the query rows).
	sql := ""
	if len(args) >= 2 {
		sql = args[1].Text
	}
	stmts := splitSQLStatements(sql)
	hasQuery := false
	for _, st := range stmts {
		if isQueryStmt(st) {
			hasQuery = true
			break
		}
	}
	if hasQuery {
		tp.emitLine("r = db.Query(%s)", sqlExpr)
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("\treturn")
		tp.emitLine("}")
		tp.emitLine("got := flatten(r)")
		tp.emitLine("if got == \"{}\" { got = \"\" } // empty result rows")
	} else {
		tp.emitLine("_res = db.Exec(%s)", sqlExpr)
		tp.emitLine("if _res.Error != nil {")
		tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
		tp.emitLine("}")
		tp.emitLine("got := \"\"")
	}
	// Append the changes counter: [execsql $sql] [db changes] concatenates
	// the query rows (empty when the batch has no SELECT) and the change count.
	counterExpr := fmt.Sprintf("db.%s()", counter)
	tp.emitLine("got = strings.TrimSpace(got + \" \" + strconv.FormatInt(%s, 10))", counterExpr)
	if isBareGoIdent(expectedExpr) {
		tp.emitLine("want := tclListFlatten(%s)", expectedExpr)
	} else {
		tp.emitLine("want := %s", expectedExpr)
	}
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}
