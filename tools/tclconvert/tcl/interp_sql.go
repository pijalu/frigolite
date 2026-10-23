// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// stmtType classifies a captured statement: statements executed inside a
// `catch { ... }` block tolerate errors (the TCL test intends to ignore
// failures), so they are captured with catchsql semantics even when the
// inner command was a plain execsql.
func (i *Interp) stmtType(sqlType string) string {
	if i.catchDepth > 0 && sqlType == "exec" {
		return "catch"
	}
	return sqlType
}

// cmdSQL handles execsql/catchsql commands.
func (i *Interp) cmdSQL(rawWords []rawWord, args []string, sqlType string, localVars map[string]string) error {
	sqlType = i.stmtType(sqlType)
	// execsql { SQL } [db] or execsql [subst { SQL }] [db]
	database := ""
	if len(args) > 1 {
		database = args[len(args)-1]
		if database == "db" {
			database = ""
		}
	}
	for _, rw := range rawWords[1:] {
		if rw.Braced && len(rw.Text) > 0 {
			sql := i.bindSQLParams(rw.Text, localVars)
			if strings.TrimSpace(sql) != "" {
				if sqlType == "query2" {
					i.stmts = append(i.stmts, Stmt{Type: "query2", SQL: sql, Database: database, TestName: i.curTest})
				} else {
					i.stmts = append(i.stmts, Stmt{
						Type:     sqlType,
						SQL:      sql,
						Database: database,
						TestName: i.curTest,
					})
				}
				i.markRowReturningValue()
			}
			break
		}
		// Handle [subst { SQL }] form — the bracket parsing already resolved this
		if !rw.Braced && len(rw.Text) > 0 {
			val, _ := i.evalWord(rw, localVars)
			val = i.bindSQLParams(val, localVars)
			if strings.TrimSpace(val) != "" && looksLikeSQL(val) {
				i.stmts = append(i.stmts, Stmt{
					Type:     sqlType,
					SQL:      val,
					Database: database,
					TestName: i.curTest,
				})
				i.markRowReturningValue()
			}
			break
		}
	}
	return nil
}

// markRowReturningValue marks the statement just captured as holding rows
// that are only known at run time. The value of a row-returning statement is
// marked, so a command post-processing it (a sort) records what it does to
// the rendered rows instead of operating on an empty list.
func (i *Interp) markRowReturningValue() {
	if len(i.stmts) == 0 {
		return
	}
	last := &i.stmts[len(i.stmts)-1]
	if !isQueryStmt(firstStatement(last.SQL)) {
		return
	}
	i.vars[""] = i.markRuntimeValue(last)
}

// cmdDB handles `db eval`, `db onecolumn`, `db transaction`, etc.
func (i *Interp) cmdDB(rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	sub := args[0]
	if i.currentLocalVars != nil && sub == "one" {
		if fn, ok := i.callbackCommands["db"]; ok {
			result, err := fn(args)
			i.vars[""] = result
			return err
		}
	}
	switch sub {
	case "collate":
		return i.dbCollation(args)
	case "function", "func":
		return i.dbFunction(args)
	case "eval":
		return i.dbEval(rawWords, args, localVars)
	case "changes":
		i.vars[""] = "__DB_CHANGES__"
		return nil
	case "close":
		i.stmts = append(i.stmts, Stmt{Type: "close_db", TestName: i.curTest})
		return nil
	case "onecolumn":
		if len(args) >= 2 {
			sql := args[1]
			if len(rawWords) >= 3 && rawWords[2].Braced {
				sql = i.bindSQLParams(sql, localVars)
			}
			i.stmts = append(i.stmts, Stmt{
				Type:     "query",
				SQL:      sql,
				TestName: i.curTest,
			})
			i.vars[""] = i.markRuntimeValue(&i.stmts[len(i.stmts)-1])
		}
		return nil
	case "one":
		// db one { SQL } — single-value query used in `set X [db one ...]`
		// and in do_test bodies (date.test:542, autoinc.test:775).
		if len(args) >= 2 {
			sql := args[1]
			if len(rawWords) >= 3 && rawWords[2].Braced {
				sql = i.bindSQLParams(sql, localVars)
			}
			i.stmts = append(i.stmts, Stmt{
				Type:     "query",
				SQL:      sql,
				TestName: i.curTest,
			})
			i.vars[""] = i.markRuntimeValue(&i.stmts[len(i.stmts)-1])
		}
		return nil
	case "null", "nullvalue":
		// "db nullvalue TOKEN" changes the TCL rendering of SQL NULLs
		// (tester.tcl); record it so expectations can be matched.
		if len(args) >= 2 {
			i.nullToken = args[1]
			i.stmts = append(i.stmts, Stmt{Type: "nullvalue", SQL: args[1], TestName: i.curTest})
		}
		return nil
	case "transaction":
		// db transaction { ... } — execute the body
		if len(rawWords) >= 3 && rawWords[2].Braced {
			return i.execScript(rawWords[2].Text, localVars)
		}
		return nil
	case "auth":
		if len(args) >= 2 && args[1] == "authproc" {
			i.stmts = append(i.stmts, Stmt{Type: "auth_analyze_deny", TestName: i.curTest})
			return nil
		}
		// `db auth <proc>` installs a Tcl authorizer: the proc is called with
		// (action, arg1, arg2, arg3, arg4) and returns SQLITE_OK /
		// SQLITE_DENY / SQLITE_IGNORE. The harness runs the proc for every
		// action, so the proc body travels with the step.
		if len(args) >= 2 {
			procName := strings.TrimPrefix(strings.TrimSpace(args[1]), "::")
			marker := Stmt{Type: "auth_proc", TestName: i.curTest, CallbackVars: cloneTclVars(i.vars)}
			if proc, ok := i.procs[procName]; ok {
				marker.CallbackProcs = []Proc{{Name: proc.Name, Args: proc.Args, Body: proc.Body}}
				marker.AuthProc = proc.Name
				marker.FunctionProc = proc.Name
				marker.FunctionBody = proc.Body
			}
			i.trackCallbackVars(marker.CallbackVars, marker.FunctionBody)
			i.stmts = append(i.stmts, marker)
		}
		return nil
	case "intkey":
		// db intkey TABLE bool — no-op
		return nil
	case "status":
		// `db status step` / `db status sort` read SQLite's statement-status
		// counters (SQLITE_STMTSTATUS_FULLSCAN_STEP / _SORT). They are
		// runtime values: the enclosing query's rendered value must carry
		// them, so the statement the helper captured is marked.
		//
		// where7.test's count_steps_sort helper:
		//   set r [lsort -integer [db eval $sql]]
		//   return "$r scan [db status step] sort [db status sort]"
		i.markStmtStatus(sub)
		return nil

	}
	// on_disconnect/cache/... — no-op
	return nil
}

// markStmtStatus flags the most recently captured statement so the harness
// appends the statement-status counters to its rendered value. The helper
// (`db status step` / `db status sort`) runs right after the query it
// describes, so the last captured statement is the right one.
func (i *Interp) markStmtStatus(kind string) {
	if len(i.stmts) == 0 {
		return
	}
	last := &i.stmts[len(i.stmts)-1]
	if last.Type != "query" && last.Type != "query_assign" && last.Type != "query2" {
		return
	}
	last.StmtStatus = true
}

func (i *Interp) dbCollation(args []string) error {
	if len(args) < 3 {
		return nil
	}
	procName := strings.TrimPrefix(strings.TrimSpace(args[2]), "::")
	marker := Stmt{Type: "register_collation", CollationName: args[1], CollationProc: procName, TestName: i.curTest, CallbackVars: cloneTclVars(i.vars)}
	if proc, ok := i.procs[procName]; ok {
		marker.CollationBody = proc.Body
		marker.FunctionArgs = append([]string(nil), proc.Args...)
	}
	i.trackCallbackVars(marker.CallbackVars, marker.CollationBody)
	i.stmts = append(i.stmts, marker)
	return nil
}

// dbFunction captures `db function NAME ?flags? PROC` for harness registration.
func (i *Interp) dbFunction(args []string) error {
	if len(args) < 3 {
		return nil
	}
	name, procName := args[1], args[len(args)-1]
	// The proc body may read Tcl globals at call time (e_expr's `var`
	// reads "::$nm"), so the harness needs the scope the function was
	// registered in. The snapshot is the same one the collation
	// registrations carry.
	marker := Stmt{Type: "register_function", FunctionName: name, FunctionProc: procName, MinArgs: -1, MaxArgs: -1, TestName: i.curTest, CallbackVars: cloneTclVars(i.vars)}
	for _, arg := range args[2 : len(args)-1] {
		switch arg {
		case "-deterministic":
			marker.Deterministic = true
		case "-innocuous":
			marker.Innocuous = true
		case "-directonly":
			marker.DirectOnly = true
		}
	}
	if proc, ok := i.procs[procName]; ok {
		marker.FunctionArgs = append([]string(nil), proc.Args...)
		marker.FunctionBody = proc.Body
		marker.MinArgs = len(proc.Args)
		if len(proc.Args) == 0 {
			marker.MinArgs, marker.MaxArgs = 0, 0
		}
		variadic := len(proc.Args) > 0 && proc.Args[len(proc.Args)-1] == "args"
		if variadic {
			marker.MaxArgs = -1
			marker.MinArgs--
		}
		for _, arg := range proc.Args[:marker.MinArgs] {
			if _, _, hasDefault := splitArgSpec(arg); hasDefault {
				marker.MinArgs--
			}
		}
	} else if strings.TrimSpace(procName) == "set ::val" {
		// SQLite test harness accepts a Tcl command prefix as callback; this
		// prefix receives one SQL argument and assigns it to global ::val.
		marker.FunctionArgs = []string{"value"}
		marker.FunctionBody = "set ::val $value"
		marker.MinArgs, marker.MaxArgs = 1, 1
	}
	i.trackCallbackVars(nil, marker.FunctionBody)
	i.stmts = append(i.stmts, marker)
	return nil
}

// dbEval implements `db eval { SQL } ?script?`.
func (i *Interp) dbEval(rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 2 {
		return nil
	}
	// The SQL may be a braced word containing $var references.
	// Substitution applies ONLY to double-quoted words: TCL performs no
	// substitution inside braces, so bracketed JSON like [1,[2,3],4]
	// must survive verbatim.
	sql := args[1]
	if len(rawWords) >= 3 && !rawWords[2].Braced {
		// Re-substitute from the raw word to handle $var in quoted SQL,
		// then bind any remaining $name references. TCL evaluation has
		// already consumed unescaped $var substitutions, so what is left
		// comes from escaped \$name (atof1.test: `db eval "...\$x"`), which
		// testfixture binds from the TCL variable of the same name.
		sql = i.substitute(rawWords[2].Text, localVars)
		sql = i.bindSQLParams(sql, localVars)
	} else if len(rawWords) >= 3 && rawWords[2].Braced {
		// TCL's "db eval {SQL}" binds $name references inside the braced
		// SQL from TCL variables (sqlite3_bind_parameter equivalents).
		// Inline them as SQL literals; brackets stay untouched.
		sql = i.bindSQLParams(sql, localVars)
	}
	if strings.TrimSpace(sql) != "" {
		typ := "exec"
		up := strings.ToUpper(strings.TrimSpace(sql))
		if strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "VALUES") || strings.HasPrefix(up, "WITH") {
			// Row-returning statement: capture as a query so the expected
			// result is compared against the rendered rows.
			typ = "query"
			// `db eval {SQL} { lappend res $x }` runs a callback per row and
			// collects the FIRST column of each row into a list
			// (orderby8.test: 200 cases). The callback's script is not
			// evaluated offline, so the statement is captured as a
			// first-column query and the harness renders the same list.
			if len(rawWords) >= 4 && rawWords[3].Braced && collectsFirstColumn(rawWords[3].Text) {
				typ = "query_first_column"
			}
		}
		i.stmts = append(i.stmts, Stmt{
			Type:     i.stmtType(typ),
			SQL:      sql,
			TestName: i.curTest,
		})
		if typ != "exec" {
			// The rows of the query are only known at run time. The
			// value is marked so that a command post-processing it (a
			// sort, where7's count_steps_sort) records what it does to
			// the rendered rows instead of sorting an empty list.
			i.vars[""] = i.markRuntimeValue(&i.stmts[len(i.stmts)-1])
		}
	}
	return nil
}

// collectsFirstColumn reports whether a `db eval` callback script only
// appends the row's first column to a list (`lappend res $x`), which is the
// shape the harness can reproduce without a Tcl interpreter.
func collectsFirstColumn(script string) bool {
	fields := strings.Fields(script)
	if len(fields) != 3 || fields[0] != "lappend" {
		return false
	}
	column := fields[2]
	return column == "$x" || column == "${x}" || column == "$::x" || column == "$column"
}

// bindSQLParams replaces Tcl SQLite parameters in braced SQL with literals.
// A parameter whose value is only known at run time (a query's result) keeps
// the value it had at conversion time: the marker that identifies the
// runtime value is an internal representation and must never reach a SQL
// statement.
// Variables that are set at conversion time are inlined with their TCL type
// (numeric values stay bare, text is quoted). Variables that are *not* set are
// left as `$name` references: tclsqlite.c:1494 leaves the parameter unbound
// when Tcl_GetVar2Ex fails, so the value is NULL either way at run time — but
// keeping the parameter text preserves parse-time parameter checks such as
// "trigger cannot use variables" (triggerE-1.1.11 uses a literal `$1` inside
// a trigger body), which replacing the text with NULL would defeat.
func (i *Interp) bindSQLParams(sql string, localVars map[string]string) string {
	var b strings.Builder
	pos := 0
	var quote byte
	lineComment, blockComment := false, false
	for pos < len(sql) {
		ch := sql[pos]
		if lineComment {
			b.WriteByte(ch)
			pos++
			if ch == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			b.WriteByte(ch)
			pos++
			if ch == '*' && pos < len(sql) && sql[pos] == '/' {
				b.WriteByte('/')
				pos++
				blockComment = false
			}
			continue
		}
		if quote != 0 {
			b.WriteByte(ch)
			pos++
			if ch == quote {
				if pos < len(sql) && sql[pos] == quote {
					b.WriteByte(sql[pos])
					pos++
				} else {
					quote = 0
				}
			}
			continue
		}
		if ch == '-' && pos+1 < len(sql) && sql[pos+1] == '-' {
			b.WriteString("--")
			pos += 2
			lineComment = true
			continue
		}
		if ch == '/' && pos+1 < len(sql) && sql[pos+1] == '*' {
			b.WriteString("/*")
			pos += 2
			blockComment = true
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' || ch == '[' {
			quote = ch
			if ch == '[' {
				quote = ']'
			}
			b.WriteByte(ch)
			pos++
			continue
		}
		if ch != '$' {
			b.WriteByte(ch)
			pos++
			continue
		}
		if pos+1 < len(sql) && sql[pos+1] == '{' {
			if np := i.appendBracedParam(&b, sql, pos, localVars); np >= 0 {
				pos = np
				continue
			}
			b.WriteByte(ch)
			pos++
			continue
		}
		j := scanBoundVarName(sql, pos+1)
		if j == pos+1 {
			b.WriteByte(ch)
			pos++
			continue
		}
		pos = i.appendNamedParam(&b, sql, pos, j, localVars)
	}
	return b.String()
}

// appendBracedParam writes the SQL literal for the ${name} reference at pos
// and returns the position after it, or -1 when the reference has no closing
// brace or the name is unresolvable (the caller binds SQL NULL).
func (i *Interp) appendBracedParam(b *strings.Builder, sql string, pos int, localVars map[string]string) int {
	end := strings.IndexByte(sql[pos+2:], '}')
	if end < 0 {
		return -1
	}
	name := sql[pos+2 : pos+2+end]
	if i.dynamicVars[strings.TrimPrefix(name, "::")] {
		// Runtime-assigned variable (query result): keep a $name reference
		// for the harness to expand from its runtime value.
		b.WriteString("$" + name)
		return pos + 3 + end
	}
	v, ok := i.getVar(name, localVars)
	if !ok {
		// Not set at conversion time: keep the parameter reference so
		// parse-time checks (e.g. trigger bodies) still see a parameter.
		// tclsqlite leaves it unbound, which is NULL at run time.
		b.WriteString("${" + name + "}")
		return pos + 3 + end
	}
	b.WriteString(tclSQLLiteral(v))
	return pos + 3 + end
}

// scanBoundVarName returns the end of the variable name starting at start
// (letters, digits, '_' and ':'), mirroring the original inline scan.
func scanBoundVarName(sql string, start int) int {
	j := start
	for j < len(sql) && (unicode.IsLetter(rune(sql[j])) || unicode.IsDigit(rune(sql[j])) || sql[j] == '_' || sql[j] == ':') {
		j++
	}
	if j < len(sql) && sql[j] == '(' {
		if end := strings.IndexByte(sql[j:], ')'); end > 0 {
			j += end + 1
		}
	}
	return j
}

// appendNamedParam writes the bound value, or SQL NULL for an unset name, for
// the named reference spanning pos..j and
// returns the position after it.
func (i *Interp) appendNamedParam(b *strings.Builder, sql string, pos, j int, localVars map[string]string) int {
	name := sql[pos+1 : j]
	if i.dynamicVars[strings.TrimPrefix(name, "::")] {
		// Runtime-assigned variable (query result): keep the $name reference
		// for the harness to expand from its runtime value.
		b.WriteString(sql[pos:j])
		return j
	}
	if v, ok := i.getVar(name, localVars); ok {
		b.WriteString(tclSQLLiteral(v))
		return j
	}
	// Not set at conversion time: keep the parameter reference (tclsqlite
	// leaves it unbound → NULL at run time, and parse-time checks such as
	// "trigger cannot use variables" must still see the parameter).
	b.WriteString(sql[pos:j])
	return j
}

// tclSQLLiteral renders a bound TCL variable value as the SQL literal the real
// tcl interface would produce. tclsqlite binds each $name/:name/@name parameter
// with the variable's TCL type (tclsqlite.c:1519 sqlite3_bind_int64, :1526
// sqlite3_bind_double, :1537 sqlite3_bind_text64), so a numeric value becomes a
// *bare* numeric literal while other values become quoted text. The
// mini-interpreter keeps only string values, so "is a plain decimal number"
// stands in for TCL's numeric type tag; this matches the common `set x [expr N]`
// idiom (e.g. `WHERE i<$nrow` with nrow=10 must render as `WHERE i<10`, not
// `i<'10'`, or the recursive CTE never terminates).
func tclSQLLiteral(v string) string {
	if s := strings.TrimSpace(v); decimalNumber.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// decimalNumber matches a bare SQLite decimal integer or real literal: an
// optional sign, digits, an optional fraction and an optional exponent. Hex,
// Inf and NaN spellings do not match, so a hit is always inside the SQL numeric
// grammar.
var decimalNumber = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// fixTestName mirrors tester.tcl's fix_testname: when the file sets a
// testprefix and the test name starts with a digit, the prefix is prepended
// ("10.1" → "prefix-10.1"); names that already carry the prefix are kept.
func (i *Interp) fixTestName(name string, localVars map[string]string) string {
	if name == "" || name[0] < '0' || name[0] > '9' {
		return name
	}
	if prefix, ok := i.getVar("testprefix", localVars); ok && prefix != "" {
		return prefix + "-" + name
	}
	return name
}

// runtimeCompareSQL reports the SQL text of a bracketed runtime query used as
// the expected-result argument of do_execsql_test/do_test:
// `[db eval { SQL }]` or `[execsql { SQL }]`. Such an argument is the value
// the SQLite test harness obtains at runtime and compares against the test SQL
// result (update2.test:38, analyze5.test:186-197). The converter cannot
// evaluate it offline, so callers capture both statements and let the harness
// compare their rows (compare_query step).
func (i *Interp) runtimeCompareSQL(rawWord rawWord, localVars map[string]string) (string, bool) {
	text := strings.TrimSpace(rawWord.Text)
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSpace(text[1 : len(text)-1])
	cmds := parseCommands(inner)
	if len(cmds) != 1 {
		return "", false
	}
	words := cmds[0]
	if len(words) < 2 {
		return "", false
	}
	head := strings.TrimSpace(words[0].Text)
	offset := -1
	switch head {
	case "execsql":
		offset = 1
	case "db":
		if len(words) >= 3 && strings.TrimSpace(words[1].Text) == "eval" {
			offset = 2
		}
	}
	if offset < 0 || offset >= len(words) {
		return "", false
	}
	if words[offset].Braced {
		return i.bindSQLParams(words[offset].Text, localVars), true
	}
	// Unbraced SQL substitutes normally (quoted form).
	sql, _ := i.evalWord(words[offset], localVars)
	return i.bindSQLParams(sql, localVars), true
}

func (i *Interp) cmdDoExecSQL(rawWords []rawWord, localVars map[string]string) error {
	// Detect a runtime-comparison expectation before substituting words, so
	// the inner query is not executed offline and its stale TCL result does
	// not become the expectation.
	wordList := rawWords[1:]
	auxSQL := ""
	compare := false
	if len(wordList) >= 3 {
		if aux, ok := i.runtimeCompareSQL(wordList[len(wordList)-1], localVars); ok {
			auxSQL, compare = aux, true
			wordList = wordList[:len(wordList)-1]
		}
	}
	words, _ := i.evalAllWords(wordList, localVars)
	// Skip optional -db flag
	idx := 0
	database := ""
	if idx+1 < len(words) && words[idx] == "-db" {
		database = words[idx+1]
		idx += 2
	}
	if idx >= len(words) {
		return nil
	}
	name := words[idx]
	sql := ""
	expected := ""
	expectedExpr := ""
	idx++
	sqlRawIdx := idx
	if idx < len(words) {
		sql = words[idx]
		idx++
	}
	switch {
	case compare && isQueryStmt(lastStatement(sql)):
		expected = "ok"
	case compare:
		// Non-query test SQL: keep the TCL substitution value, as before.
		expected, _ = i.evalWord(rawWords[len(rawWords)-1], localVars)
		compare = false
	case idx < len(words):
		expected = words[idx]
		// An expectation whose value is only known at run time (a variable
		// holding a query's rows) is compared at run time.
		if name, ok := runtimeVarRef(strings.TrimSpace(rawWords[idx+1].Text)); ok && i.dynamicVars[name] {
			expectedExpr = "$" + name
			expected = ""
		} else {
			expected = stripRuntimeValues(expected)
		}
	}
	// A braced SQL word keeps TCL variables unexpanded; sqlite3 binds them
	// as named parameters from TCL scope. A bracketed word such as
	// [subst -novariables {...}] leaves $name references the same way
	// (rowvalue2.test). Inline resolvable ones as SQL literals (brackets
	// stay verbatim, preserving JSON arrays).
	if sqlRawIdx+1 < len(rawWords) && sql != "" {
		w := rawWords[sqlRawIdx+1]
		if w.Braced || strings.HasPrefix(strings.TrimSpace(w.Text), "[") {
			sql = i.bindSQLParams(sql, localVars)
		}
	}

	// Determine if it's a query or exec
	sqlType := "exec"
	lastStmt := lastStatement(sql)
	if isQueryStmt(lastStmt) {
		sqlType = "query"
	}
	// Tcl expected value {{}} is a one-element list containing empty text,
	// distinct from {} (empty result list). Preserve that distinction for
	// the harness instead of flattening both to the same empty JSON string.
	if sqlType == "query" && len(rawWords) > 1 && rawWords[len(rawWords)-1].Braced && rawWords[len(rawWords)-1].Text == "{}" {
		sqlType = "emptytext"
	}

	stmtType := sqlType
	aux := ""
	if compare && sqlType == "query" {
		stmtType = "compare_query"
		aux = auxSQL
	}

	name = i.fixTestName(name, localVars)
	// The current test name is scoped to this command: statements captured
	// afterwards (outside any do_test) belong to the enclosing setup group.
	// Leaving it set split e_expr's `parameter_test` trio (prepare in one
	// test case, step/finalize in another), which made the harness lose the
	// prepared handle and left the statement open — blocking every later
	// write with "database is locked".
	previous := i.curTest
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:         stmtType,
		SQL:          sql,
		Expected:     expected,
		ExpectedExpr: expectedExpr,
		AuxSQL:       aux,
		TestName:     name,
		Database:     database,
	})
	i.curTest = previous
	return nil
}

// cmdDoCatchSQL handles do_catchsql_test name { SQL } { expected_error }
func (i *Interp) cmdDoCatchSQL(rawWords []rawWord, localVars map[string]string) error {
	words, _ := i.evalAllWords(rawWords[1:], localVars)
	if len(words) < 2 {
		return nil
	}
	name := words[0]
	sql := words[1]
	expected := ""
	if len(words) >= 3 {
		expected = words[2]
	}
	name = i.fixTestName(name, localVars)
	// Scope the current test name to this command (see cmdDoExecSQL).
	previous := i.curTest
	i.curTest = name
	// testfixture's catchsql binds named parameters ($i) from the caller's
	// TCL variables (tclsqlite.c); do_catchsql_test gets the same treatment as
	// catchsql. Loop variables are compile-time known when the converter
	// unrolls the loop, so inline them — otherwise every iteration of e.g.
	// rtree8-2.1.3.$i would run the same SQL with an unbound parameter.
	sql = i.bindSQLParams(sql, localVars)
	i.stmts = append(i.stmts, Stmt{
		Type:     "catch",
		SQL:      sql,
		Expected: expected,
		TestName: name,
	})
	i.curTest = previous
	return nil
}

// cmdDoTest handles do_test name { body } { expected }
// The body may contain execsql, db eval, or other TCL code.
func (i *Interp) cmdDoTest(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 3 {
		return nil
	}
	nameWord := rawWords[1]
	name, _ := i.evalWord(nameWord, localVars)
	name = i.fixTestName(name, localVars)

	// Find the body (braced) and expected (braced or a $var reference).
	// A bracketed runtime query (`[db eval {...}]`, `[execsql {...}]`) is a
	// runtime result comparison: capture the second query instead of
	// substituting it offline.
	bodyWord := rawWords[2]
	expected := ""
	expectedExpr := ""
	auxSQL := ""
	compare := false
	if len(rawWords) >= 4 {
		if aux, ok := i.runtimeCompareSQL(rawWords[3], localVars); ok {
			auxSQL, compare = aux, true
		} else if rawWords[3].Braced {
			expected = rawWords[3].Text
		} else {
			// Unbraced expectation (e.g. `$res` set from a loop list):
			// substitute so the captured statement carries the real value.
			expected, _ = i.evalWord(rawWords[3], localVars)
			// An expectation whose value is only known at run time (a
			// variable holding a query's rows) is compared at run time:
			// the harness evaluates the variable against the result.
			if name, ok := runtimeVarRef(rawWords[3].Text); ok && i.dynamicVars[name] {
				expectedExpr = "$" + name
				expected = ""
			} else {
				expected = stripRuntimeValues(expected)
			}
		}
	}

	i.curTest = name

	// Execute the body — this captures SQL statements
	if bodyWord.Braced {
		i.execScript(bodyWord.Text, localVars)
	} else {
		body, _ := i.evalWord(bodyWord, localVars)
		i.execScript(body, localVars)
	}

	// C-API lifecycle tests compare result codes plus DB changes. Keep the exact
	// Tcl result expression as a runtime assertion after the ordered operations.
	if strings.Contains(bodyWord.Text, "sqlite3_step") || strings.Contains(bodyWord.Text, "sqlite3_reset") {
		i.stmts = append(i.stmts, Stmt{Type: "runtime_assert", SQL: i.vars[""], Expected: expected, TestName: name})
		i.curTest = ""
		return nil
	}
	// Global Tcl variable reads are runtime assertions; capture them explicitly
	// so SQL callbacks can populate the value during earlier steps.
	//
	// SQLite's testfixture compares the *value of the body* (the result of its
	// last command) with the do_test expectation — not the result of the last
	// statement the body happened to capture. When the body's value is a
	// variable, the expectation belongs to that variable (rtree2: the body's
	// value is `set rc`, i.e. the boolean of `$t1 eq $t2`).
	bodyValueCaptured := false
	bodyCommands := parseCommands(bodyWord.Text)
	if len(bodyCommands) > 0 && len(bodyCommands[len(bodyCommands)-1]) == 2 && bodyCommands[len(bodyCommands)-1][0].Text == "set" {
		rawText := strings.TrimSpace(bodyCommands[len(bodyCommands)-1][1].Text)
		varName := strings.TrimPrefix(rawText, "::")
		// `set ::name` was always captured; bare names only when the harness
		// owns the value (a query_assign target or a global written by a
		// registered callback such as e_expr's `set likeargs`).
		if varName != "" && !strings.ContainsAny(varName, " \t\r\n") &&
			(strings.HasPrefix(rawText, "::") || i.dynamicVars[varName] || i.trackedVars[varName]) {
			i.stmts = append(i.stmts, Stmt{Type: "runtime_var", Variable: varName, Expected: expected, TestName: name})
			bodyValueCaptured = true
			// The variable's value is compared at run time, so it must
			// hold *every* row (Tcl's execsql/db eval yield a list of
			// rows), not just the first one: rtree2 compares the whole
			// rtree table with the whole shadow table.
			for idx := range i.stmts {
				if i.stmts[idx].TestName == name && i.stmts[idx].Type == "query_assign" && i.stmts[idx].ResultAll {
					i.stmts[idx].ResultRows = true
				}
			}
		}
	}

	// The body's final command may be an `expr` over values that only exist at
	// runtime (date.test:545-548: `set date [db one {...}]` followed by
	// `expr {$date eq ...}`). The offline interpreter cannot compute it, so
	// capture the assertion for the harness instead of leaving the expectation
	// stuck on the query_assign step. Variables whose values are already known
	// are baked in as literals; runtime-assigned ones stay as $name references.
	if len(bodyCommands) > 0 && expected != "" {
		// The body's value may be a Tcl `list` over runtime values
		// (conflict.test: `list $r0 $r1 $r2 $r3`): the list is built at run
		// time from the captured values.
		if lastCmd := bodyCommands[len(bodyCommands)-1]; len(lastCmd) >= 2 && lastCmd[0].Text == "list" {
			if values, ok := i.runtimeListValues(lastCmd[1:], localVars); ok {
				i.stmts = append(i.stmts, Stmt{Type: "runtime_list", ListValues: values, Expected: expected, TestName: name})
				bodyValueCaptured = true
			}
		}
		if lastCmd := bodyCommands[len(bodyCommands)-1]; len(lastCmd) == 2 && lastCmd[0].Text == "expr" {
			if exprText := strings.TrimSpace(lastCmd[1].Text); i.exprReadsRuntimeVar(exprText) {
				// The expectation may itself be a runtime expression: the
				// do_test's expected argument is a command substitution over
				// runtime values (`[expr {$nFree - $nFree2}]`,
				// incrvacuum_ioerr), so it must be evaluated at run time and
				// compared with the body's value - not baked at conversion
				// time (where the runtime variables are empty).
				expectedExpr := ""
				if len(rawWords) >= 4 && !rawWords[3].Braced {
					if inner, ok := bracketedExprText(rawWords[3].Text); ok && i.exprReadsRuntimeVar(inner) {
						expectedExpr = i.bakeKnownExprVars(inner, localVars)
					}
				}
				runtimeStep := Stmt{Type: "runtime_expr", SQL: i.bakeKnownExprVars(exprText, localVars), Expected: expected, TestName: name}
				if expectedExpr != "" {
					runtimeStep.Expected = ""
					runtimeStep.ExpectedExpr = expectedExpr
				}
				i.stmts = append(i.stmts, runtimeStep)
				bodyValueCaptured = true
			}
		}
	}

	// Preserve Tcl list postprocessing that changes only captured SQL output.
	if strings.HasPrefix(strings.TrimSpace(bodyWord.Text), "lreplace ") && len(i.stmts) > 0 {
		parts := parseCommands(bodyWord.Text)
		if len(parts) == 1 && len(parts[0]) == 5 && parts[0][0].Text == "lreplace" {
			if start, err1 := strconv.Atoi(parts[0][2].Text); err1 == nil {
				if end, err2 := strconv.Atoi(parts[0][3].Text); err2 == nil && start == end {
					last := &i.stmts[len(i.stmts)-1]
					if last.TestName == name && last.Type == "query" {
						last.ResultTransform = fmt.Sprintf("lreplace:%d:%d:%s", start, end, parts[0][4].Text)
					}
				}
			}
		}
	}

	// Runtime result comparison: the body's final query result must equal the
	// result of the runtime query in the expected argument.
	if compare && len(i.stmts) > 0 {
		last := &i.stmts[len(i.stmts)-1]
		if last.TestName == name && last.Type == "query" {
			last.Type = "compare_query"
			last.AuxSQL = auxSQL
			last.Expected = "ok"
			i.curTest = ""
			return nil
		}
	}

	// If expected is non-empty, attach it to the last captured statement —
	// unless the body's value was captured separately (a runtime variable or
	// a runtime expression): then the expectation describes the body's value,
	// and attaching it to the last statement would compare the statement's
	// rows with a scalar (rtree2: 1000 rows against "1").
	if (expected != "" || expectedExpr != "") && len(i.stmts) > 0 && !bodyValueCaptured {
		last := &i.stmts[len(i.stmts)-1]
		if last.TestName == name && last.Expected == "" && last.ExpectedExpr == "" {
			last.Expected = expected
			last.ExpectedExpr = expectedExpr
		}
	}

	i.curTest = ""
	return nil
}

// cmdDoEQP handles do_eqp_test name { SQL } { expected }
func (i *Interp) cmdDoEQP(rawWords []rawWord, localVars map[string]string) error {
	if len(rawWords) < 3 {
		return nil
	}
	name, _ := i.evalWord(rawWords[1], localVars)
	name = i.fixTestName(name, localVars)
	sql := ""
	if rawWords[2].Braced {
		sql = i.bindSQLParams(rawWords[2].Text, localVars)
	} else {
		sql, _ = i.evalWord(rawWords[2], localVars)
	}
	previous := i.curTest
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     "query",
		SQL:      "EXPLAIN QUERY PLAN " + sql,
		TestName: name,
	})
	i.curTest = previous
	return nil
}

// --- Variable management ---

// --- Helpers ---

// looksLikeSQL checks if a string looks like a SQL statement.
func looksLikeSQL(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	upper := strings.ToUpper(s[:min(len(s), 20)])
	keywords := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP",
		"ALTER", "PRAGMA", "WITH", "REPLACE", "ATTACH", "DETACH", "BEGIN",
		"COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "ANALYZE", "REINDEX",
		"VACUUM", "EXPLAIN"}
	for _, kw := range keywords {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}

func firstStatement(sql string) string {
	for _, statement := range strings.Split(sql, ";") {
		statement = strings.TrimSpace(stripSQLComments(statement))
		if statement != "" {
			return statement
		}
	}
	return ""
}

// tclVarRef matches $name and $::name variable references in a Tcl expression.
var tclVarRef = regexp.MustCompile(`\$(?:::)?([A-Za-z_][A-Za-z0-9_]*)`)

// exprReadsRuntimeVar reports whether a Tcl expression references a variable
// whose value is only known at runtime (assigned from a query result), meaning
// the expression cannot be evaluated offline by the converter.
func (i *Interp) exprReadsRuntimeVar(expr string) bool {
	for _, m := range tclVarRef.FindAllStringSubmatch(expr, -1) {
		if i.dynamicVars[m[1]] {
			return true
		}
	}
	return false
}

// bakeKnownExprVars renders a deferred Tcl expression as harness-evaluable
// text: variables whose values the converter knows are replaced by literals
// (numbers stay bare so arithmetic keeps TCL integer/double typing, other
// values become quoted string operands, since expr binds variable values as
// atomic operands), while runtime-assigned variables keep their $name
// reference for the harness to fill from captured query results. do_test
// e_expr-7.* is the driving case: `expr {... $op ... $t ...}` with $op known
// from the enclosing enumeration loop and $t captured by `db one`.
func (i *Interp) bakeKnownExprVars(expr string, localVars map[string]string) string {
	var b strings.Builder
	pos := 0
	for pos < len(expr) {
		if expr[pos] != '$' {
			b.WriteByte(expr[pos])
			pos++
			continue
		}
		name, next := i.exprVarNameAt(expr, pos, localVars)
		if name == "" {
			b.WriteByte('$')
			pos++
			continue
		}
		if i.dynamicVars[strings.TrimPrefix(name, "::")] {
			b.WriteString(expr[pos:next])
			pos = next
			continue
		}
		if val, ok := i.getVar(name, localVars); ok {
			b.WriteString(exprDeferAtom(val))
			pos = next
			continue
		}
		b.WriteString(expr[pos:next])
		pos = next
	}
	return b.String()
}

// exprVarNameAt resolves the variable reference starting at s[pos] (a '$'),
// returning the variable name (empty when the '$' is literal) and the position
// after the reference.
func (i *Interp) exprVarNameAt(s string, pos int, localVars map[string]string) (string, int) {
	pos++
	if pos >= len(s) {
		return "", pos
	}
	if s[pos] == '{' {
		end := strings.IndexByte(s[pos+1:], '}')
		if end < 0 {
			return "", pos
		}
		return s[pos+1 : pos+1+end], pos + 2 + end
	}
	if unicode.IsLetter(rune(s[pos])) || s[pos] == '_' || s[pos] == ':' {
		return i.readVarName(s, pos, localVars)
	}
	return "", pos
}

// exprDeferAtom renders a Tcl value as an operand for deferred expression text:
// numeric values stay bare (so `$a + 1` keeps integer arithmetic), everything
// else becomes a quoted string operand.
func exprDeferAtom(v string) string {
	t := strings.TrimSpace(v)
	if t != "" {
		if _, err := strconv.ParseFloat(t, 64); err == nil {
			return t
		}
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func lastStatement(sql string) string {
	stmts := strings.Split(sql, ";")
	for i := len(stmts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(stripSQLComments(stmts[i]))
		if s != "" {
			return s
		}
	}
	return ""
}

// stripSQLComments removes SQL comments (/* ... */ and -- to end of line)
// outside string literals, so statement classification is not confused by
// braces or keywords inside comments (e.g. json101-11.2's trailing "*/ } */").
func stripSQLComments(sql string) string {
	var b strings.Builder
	inS, inD := false, false
	for i := 0; i < len(sql); i++ {
		var next int
		next, inS, inD = stripSQLCommentStep(&b, sql, i, inS, inD)
		i = next
	}
	return b.String()
}

// stripSQLCommentStep classifies sql[i] during comment stripping, writing the
// output byte(s) and returning the index the scanner should continue from
// (the loop adds its own step) along with updated quote flags.
func stripSQLCommentStep(b *strings.Builder, sql string, i int, inS, inD bool) (int, bool, bool) {
	c := sql[i]
	switch {
	case inS:
		b.WriteByte(c)
		if c == '\'' {
			inS = false
		}
	case inD:
		b.WriteByte(c)
		if c == '"' {
			inD = false
		}
	case c == '\'':
		inS = true
		b.WriteByte(c)
	case c == '"':
		inD = true
		b.WriteByte(c)
	case isLineCommentStart(sql, i):
		i = skipLineComment(sql, i)
		b.WriteByte('\n')
		return i, inS, inD
	case isBlockCommentStart(sql, i):
		i = skipBlockComment(sql, i)
		b.WriteByte(' ')
		return i, inS, inD
	default:
		b.WriteByte(c)
	}
	return i, inS, inD
}

// isLineCommentStart reports whether a -- comment starts at i.
func isLineCommentStart(sql string, i int) bool {
	return sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-'
}

// isBlockCommentStart reports whether a /* comment starts at i.
func isBlockCommentStart(sql string, i int) bool {
	return sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*'
}

// skipLineComment advances past a -- comment, stopping on the terminating
// newline (which the caller emits) or at end of input.
func skipLineComment(sql string, i int) int {
	for i < len(sql) && sql[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment advances past a /* ... */ comment, returning the index of
// the '/' of the terminator, or the last scanned index at end of input.
func skipBlockComment(sql string, i int) int {
	i += 2
	for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
		i++
	}
	return i + 1
}

func isQueryStmt(stmt string) bool {
	stmt = strings.TrimSpace(stmt)
	upper := strings.ToUpper(stmt[:min(len(stmt), 10)])
	return strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "PRAGMA") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "WITH")
}

func isTrue(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// TCL: true if it's a non-zero number
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f != 0
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
