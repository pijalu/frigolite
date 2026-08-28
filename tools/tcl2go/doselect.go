// Package main implements the tcl2go tool.
//
// This file handles do_select_tests and its wrapper procs (do_createtable_tests,
// do_delete_tests, do_insert_tests, do_update_tests, do_reindex_tests).
//
// The SQLite test-harness proc do_select_tests runs a list of test cases, each
// of the form {LABEL SQL EXPECTED}, with optional setup (-repair), expected
// error (-error), and post-query (-query / -tclquery) options:
//
//	do_select_tests NAME [-repair {SCRIPT}] [-error {MSG}] [-query {SQL}]
//	                  [-tclquery {EXPR}] { CASE ... }
//
// For each case:
//   - -repair  : run the SCRIPT commands first (side effects).
//   - -error   : the SQL must fail with an error equal to format(MSG, EXPECTED...)
//     (EXPECTED is {} for a literal message, or the format arguments).
//   - -query   : run the case SQL, then run the query SQL and compare its rows
//     with EXPECTED.
//   - -tclquery: run the case SQL, then evaluate EXPR (a TCL expression such
//     as `table_column_names x1`) and compare with EXPECTED.
//   - default  : run the case SQL and compare its rows with EXPECTED
//     (do_execsql_test semantics).
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// processDoSelectTests transpiles a do_select_tests-family command (including
// the wrapper procs). prefix is the
// test-name prefix the wrapper proc applies to its first argument ("" for
// do_select_tests itself, "e_createtable-" for do_createtable_tests, etc.).
func (tp *transpiler) processDoSelectTests(prefix string, args []tcl.RawWord) {
	tp.doSelectTests(prefix, args)
}

func (tp *transpiler) doSelectTests(prefix string, args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	name := prefix + strings.TrimSpace(args[0].Text)

	// Parse option/value pairs, then the trailing cases block.
	opts, casesIdx := parseSelectTestOptions(args)
	if casesIdx < 0 || casesIdx >= len(args) {
		return
	}
	casesText := args[casesIdx].Text

	// The cases block may be a `[list ...]` expression (e_select-1.4.5):
	// resolve it to the literal list text at transpile time.
	if resolved, ok := evalListCasesExpr(casesText); ok {
		casesText = resolved
	}

	cases := parseSelectTestCases(casesText)
	if len(cases) == 0 {
		return
	}

	tp.emitLine("{ // %s (do_select_tests)", name)
	tp.indent++

	// TCL do_select_tests runs `eval $repair` before the loop AND after each
	// case. The after-case repair is what keeps the schema clean between
	// cases (e.g. e_createtable-2.2's repair drops x1 after its last case so
	// 2.3.1 can recreate it). Emit the repair before each case, then again
	// after the last case to match that behavior.
	for _, tc := range cases {
		tp.emitLine("{ // %s.%s", name, tc.label)
		tp.indent++
		if opts.repair != "" {
			tp.transpileSelectRepair(opts.repair)
		}
		switch {
		case opts.errorMsg != "":
			tp.emitSelectErrorCase(name, tc, opts.errorMsg)
		case opts.query != "":
			tp.emitSelectQueryCase(name, tc, opts.query)
		case opts.tclquery != "":
			tp.emitSelectTCLQueryCase(name, tc, opts.tclquery)
		case opts.countOnly:
			tp.emitSelectCountCase(name, tc)
		default:
			tp.emitSelectPlainCase(name, tc)
		}
		tp.indent--
		tp.emitLine("}")
		if opts.repair != "" {
			tp.transpileSelectRepair(opts.repair)
		}
	}

	tp.indent--
	tp.emitLine("}")
}

// selectTestOptions holds the parsed -repair/-error/-query/-tclquery/-count
// options of a do_select_tests call.
type selectTestOptions struct {
	repair    string
	errorMsg  string
	query     string
	tclquery  string
	countOnly bool
}

// parseSelectTestOptions scans a do_select_tests argument list for option/value
// pairs (stopping at the first non-option argument, the cases block). Returns
// the parsed options and the index of the cases block (-1 when absent).
func parseSelectTestOptions(args []tcl.RawWord) (selectTestOptions, int) {
	var opts selectTestOptions
	casesIdx := -1
	i := 1
	for i < len(args) {
		opt := strings.TrimSpace(args[i].Text)
		switch opt {
		case "-repair", "-error", "-errorformat", "-query", "-tclquery", "-count":
			if opt == "-count" {
				opts.countOnly = true
				i++
				continue
			}
			if i+1 >= len(args) {
				return opts, -1
			}
			val := args[i+1].Text
			switch opt {
			case "-repair":
				opts.repair = val
			case "-error", "-errorformat":
				opts.errorMsg = val
			case "-query":
				opts.query = val
			case "-tclquery":
				opts.tclquery = val
			}
			i += 2
		default:
			// First non-option argument is the cases block.
			casesIdx = i
			return opts, casesIdx
		}
	}
	return opts, casesIdx
}

// selectTestCase is one {LABEL SQL EXPECTED} triple from a do_select_tests
// case list.
type selectTestCase struct {
	label    string
	sql      string
	sqlWord  tcl.RawWord
	expected string
}

// parseSelectTestCases parses the flat TCL list of {LABEL SQL EXPECTED}
// triples into cases. It splits the text into lines (each line is one case;
// multi-line braced SQL/expected stays intact because ParseCommands keeps
// braces together) and reads three words per command. The SQL word keeps its
// Braced/Quoted flags so collectSQLExpression applies the right unescaping.
func parseSelectTestCases(text string) []selectTestCase {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	cmds := tcl.ParseCommands(text)
	var cases []selectTestCase
	for _, c := range cmds {
		if len(c) < 3 {
			continue
		}
		cases = append(cases, selectTestCase{
			label:    strings.TrimSpace(c[0].Text),
			sql:      strings.TrimSpace(c[1].Text),
			sqlWord:  c[1],
			expected: strings.TrimSpace(c[2].Text),
		})
	}
	return cases
}

// evalListCasesExpr resolves a `[list ...]` cases block to its literal list
// text (the list command's elements joined with spaces). Returns ok=false when
// the text is not a list expression.
func evalListCasesExpr(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "[list") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[list"), "]")
	return strings.TrimSpace(inner), true
}

// transpileSelectRepair transpiles a do_select_tests -repair script by running
// its commands in a sub-transpiler (the script may set variables used later).
// A bare proc name (e.g. `-repair rebuild_t1`) is treated as a call to that
// proc (inlined by processDefaultCommand).
func (tp *transpiler) transpileSelectRepair(repair string) {
	repair = strings.TrimSpace(repair)
	if repair != "" && isPlainTCLVarName(repair) {
		tp.processCommand([]tcl.RawWord{{Text: repair}})
		return
	}
	body := parseCommands(repair)
	if len(body) == 0 {
		return
	}
	sub := &transpiler{
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
		rangeListFuncs:  tp.rangeListFuncs,
		collateDtorVars: tp.collateDtorVars,
		collateGoFuncs:  tp.collateGoFuncs,
		testPrefix:      tp.testPrefix,
		queryVars:       tp.queryVars,
		dbAliases:       tp.dbAliases,
		dbClosed:        tp.dbClosed,
		dqsDDL:          tp.dqsDDL,
		dqsDML:          tp.dqsDML,
		preparedState:   tp.preparedState,
	}
	sub.processCommands(body)
	tp.varCount = sub.varCount
	tp.vars = sub.vars
	tp.queryVars = sub.queryVars
	tp.preparedState = sub.preparedState
}

// emitSelectErrorCase emits a do_select_tests case with -error: the SQL must
// fail with the formatted error message.
func (tp *transpiler) emitSelectErrorCase(name string, tc selectTestCase, errorMsg string) {
	msgExpr := `""`
	if strings.TrimSpace(tc.expected) == "" || tc.expected == "{}" {
		// Literal message (no format args). Braced=true keeps any [cmd]-like
		// text (e.g. the literal "[x]" in SQLite's DEFAULT error message)
		// as-is instead of treating it as a TCL command substitution.
		msgExpr = tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSpace(errorMsg), Braced: true})
	} else {
		// EXPECTED holds the format arguments: format(MSG, EXPECTED...).
		formatArgs := tclCmdWords(tc.expected)
		argExprs := make([]string, 0, len(formatArgs))
		for _, a := range formatArgs {
			argExprs = append(argExprs, tp.buildStringExpr(a))
		}
		msgExpr = fmt.Sprintf("tclFormat(%s, %s)", tp.goStringLiteral(tcl.RawWord{Text: strings.TrimSpace(errorMsg)}), strings.Join(argExprs, ", "))
	}
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{tc.sqlWord})
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error == nil || !strings.Contains(_res.Error.Error(), %s) {", msgExpr)
	tp.emitLine("\tt.Errorf(\"expected error containing %%q, got: %%v\\n  sql: %%s\", %s, _res.Error, %s)", msgExpr, sqlExpr)
	tp.emitLine("}")
}

// emitSelectQueryCase emits a do_select_tests case with -query: run the case
// SQL (side effects), then run the query SQL and compare rows with EXPECTED.
// The TCL expected for -query is `[list {*}$res]` (the case's res expanded as
// a list), so the want is the res list's elements space-joined — the form
// flatten() produces for the query result.
func (tp *transpiler) emitSelectQueryCase(name string, tc selectTestCase, query string) {
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{tc.sqlWord})
	queryExpr := tp.collectSQLExpression([]tcl.RawWord{{Text: query, Braced: true}})
	wantExpr := selectWantExpr(tc.expected)

	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")
	tp.emitLine("r = db.Query(%s)", queryExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", queryExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := flatten(r)")
	tp.emitLine("want := %s", wantExpr)
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// selectWantExpr renders a do_select_tests case's expected value as the Go
// expression the query/tclquery result is compared against: the res list's
// elements space-joined (matching flatten() of the query result). A bare
// TCL variable reference is rendered as its Go runtime value.
func selectWantExpr(expected string) string {
	expected = strings.TrimSpace(expected)
	if strings.HasPrefix(expected, "$") {
		gv := tclVarToGo(strings.TrimPrefix(expected, "$"))
		if isValidGoIdent(gv) {
			return gv
		}
	}
	// An empty expected (TCL {} — the empty list) matches an empty query
	// result, which flatten() renders as "{}" (not "").
	if expected == "" {
		return `"{}"`
	}
	if elems := tclSplitList(expected); len(elems) > 1 || (len(elems) == 1 && elems[0] != expected) {
		var parts []string
		for _, e := range elems {
			e = strings.TrimSpace(e)
			if e == "" {
				parts = append(parts, "{}")
			} else {
				parts = append(parts, e)
			}
		}
		return fmt.Sprintf("%q", strings.Join(parts, " "))
	}
	// A multi-line / multi-space expected value (TCL list rendering with
	// alignment spaces and newlines) compares against flatten()'s
	// single-space-joined output: collapse the whitespace.
	if fields := strings.Fields(expected); len(fields) > 1 {
		return fmt.Sprintf("%q", strings.Join(fields, " "))
	}
	return fmt.Sprintf("%q", strings.TrimSpace(expected))
}

// emitSelectPlainCase emits a do_select_tests case without options: run the
// case SQL and compare its rows with EXPECTED (do_execsql_test semantics).
func (tp *transpiler) emitSelectPlainCase(name string, tc selectTestCase) {
	sql := tc.sql
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{tc.sqlWord})
	expectedExpr := tp.expectLiteral(tcl.RawWord{Text: tc.expected, Braced: true})
	// A bare TCL variable expected value ($t1_cross_t2) compares against the
	// Go variable's runtime value, flattened (the variable holds a TCL list
	// with alignment whitespace; flatten() produces single-space-joined
	// values).
	if strings.HasPrefix(strings.TrimSpace(tc.expected), "$") {
		gv := tclVarToGo(strings.TrimPrefix(strings.TrimSpace(tc.expected), "$"))
		if isValidGoIdent(gv) {
			expectedExpr = fmt.Sprintf("tclListFlatten(%s)", gv)
		}
	}

	if expectedExpr != `""` && isErrExpectation(expectedExpr) {
		tp.emitExpectedErrorExec("db", sqlExpr, expectedExpr)
		return
	}
	stmts := splitSQLStatements(sql)
	hasQuery := false
	for _, st := range stmts {
		if isQueryStmt(st) {
			hasQuery = true
			break
		}
	}
	if hasQuery && expectedExpr != `""` {
		tp.emitExpectedQueryResult("db", sqlExpr, expectedExpr, []tcl.RawWord{{Text: sql, Braced: true}, {Text: sql, Braced: true}, {Text: tc.expected, Braced: true}})
		return
	}
	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")
}

// emitSelectCountCase emits a do_select_tests case with -count: run the SQL
// and count the number of rows returned (matching TCL's `set nRow 0; db eval
// $sql {incr nRow}`).
func (tp *transpiler) emitSelectCountCase(name string, tc selectTestCase) {
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{tc.sqlWord})
	expectedExpr := tp.expectLiteral(tcl.RawWord{Text: tc.expected, Braced: true})

	tp.emitLine("r = db.Query(%s)", sqlExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := strconv.Itoa(len(r.Rows))")
	tp.emitLine("want := %s", expectedExpr)
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// emitSelectTCLQueryCase emits a do_select_tests case with -tclquery: run the
// case SQL, then evaluate the TCL expression (table_column_names /
// table_column_decltypes are e_createtable-specific queries over the created
// table's columns) and compare with EXPECTED.
func (tp *transpiler) emitSelectTCLQueryCase(name string, tc selectTestCase, tclquery string) {
	sqlExpr := tp.collectSQLExpression([]tcl.RawWord{tc.sqlWord})
	wantExpr := selectWantExpr(tc.expected)

	tp.emitLine("_res = db.Exec(%s)", sqlExpr)
	tp.emitLine("if _res.Error != nil {")
	tp.emitLine("\tt.Errorf(\"exec error: %%v\\n  sql: %%s\", _res.Error, %s)", sqlExpr)
	tp.emitLine("}")

	tq := strings.TrimSpace(tclquery)
	var querySQL string
	switch {
	case strings.HasPrefix(tq, "table_column_names "):
		table := strings.TrimSpace(strings.TrimPrefix(tq, "table_column_names "))
		querySQL = fmt.Sprintf("SELECT name FROM pragma_table_info(%s) ORDER BY cid", sqlLit(table))
	case strings.HasPrefix(tq, "table_column_decltypes "):
		table := strings.TrimSpace(strings.TrimPrefix(tq, "table_column_decltypes "))
		querySQL = fmt.Sprintf("SELECT type FROM pragma_table_info(%s) ORDER BY cid", sqlLit(table))
	default:
		// Unsupported tclquery expression — emit a comment (side effects of
		// the case SQL are already emitted above).
		tp.emitLine("// tclquery %s (unsupported, not transpiled)", sanitizeTCLComment(tq))
		return
	}

	queryExpr := tp.collectSQLExpression([]tcl.RawWord{{Text: querySQL, Braced: true}})
	tp.emitLine("r = db.Query(%s)", queryExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", queryExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("got := flatten(r)")
	tp.emitLine("want := %s", wantExpr)
	tp.emitLine("if got != want {")
	tp.emitLine("\tt.Errorf(\"result mismatch\\n  got:  [%%s]\\n  want: [%%s]\", got, want)")
	tp.emitLine("}")
}

// sqlLit renders a string as a single-quoted SQL literal.
func sqlLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
