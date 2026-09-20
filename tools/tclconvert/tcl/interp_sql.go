// SPDX-License-Identifier: GPL-3.0-or-later

package tcl

import (
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
	for _, rw := range rawWords[1:] {
		if rw.Braced && len(rw.Text) > 0 {
			sql := i.substitute(rw.Text, localVars)
			if strings.TrimSpace(sql) != "" {
				i.stmts = append(i.stmts, Stmt{
					Type:     sqlType,
					SQL:      sql,
					TestName: i.curTest,
				})
			}
			break
		}
		// Handle [subst { SQL }] form — the bracket parsing already resolved this
		if !rw.Braced && len(rw.Text) > 0 {
			val, _ := i.evalWord(rw, localVars)
			if strings.TrimSpace(val) != "" && looksLikeSQL(val) {
				i.stmts = append(i.stmts, Stmt{
					Type:     sqlType,
					SQL:      val,
					TestName: i.curTest,
				})
			}
			break
		}
	}
	return nil
}

// cmdDB handles `db eval`, `db onecolumn`, `db transaction`, etc.
func (i *Interp) cmdDB(rawWords []rawWord, args []string, localVars map[string]string) error {
	if len(args) < 1 {
		return nil
	}
	sub := args[0]
	switch sub {
	case "eval":
		return i.dbEval(rawWords, args, localVars)
	case "onecolumn":
		if len(args) >= 2 {
			i.stmts = append(i.stmts, Stmt{
				Type:     "query",
				SQL:      args[1],
				TestName: i.curTest,
			})
		}
		return nil
	case "null":
		// "db null TOKEN" changes the TCL rendering of SQL NULLs
		// (tester.tcl); record it so expectations can be matched.
		if len(args) >= 2 {
			i.nullToken = args[1]
		}
		return nil
	case "transaction":
		// db transaction { ... } — execute the body
		if len(rawWords) >= 3 && rawWords[2].Braced {
			return i.execScript(rawWords[2].Text, localVars)
		}
		return nil
	case "intkey":
		// db intkey TABLE bool — no-op
		return nil
	}
	// close/on_disconnect/cache/... — no-op
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
		// Re-substitute from the raw word to handle $var in quoted SQL
		sql = i.substitute(rawWords[2].Text, localVars)
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
		}
		i.stmts = append(i.stmts, Stmt{
			Type:     i.stmtType(typ),
			SQL:      sql,
			TestName: i.curTest,
		})
	}
	return nil
}

// bindSQLParams replaces $name / ${name} parameter references in braced SQL
// with SQL string literals of the variable values (TCL db-eval binding
// semantics). Unresolvable names are left verbatim.
func (i *Interp) bindSQLParams(sql string, localVars map[string]string) string {
	var b strings.Builder
	pos := 0
	for pos < len(sql) {
		ch := sql[pos]
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
// brace or the name is unresolvable (the caller emits the '$' verbatim).
func (i *Interp) appendBracedParam(b *strings.Builder, sql string, pos int, localVars map[string]string) int {
	end := strings.IndexByte(sql[pos+2:], '}')
	if end < 0 {
		return -1
	}
	name := sql[pos+2 : pos+2+end]
	v, ok := i.getVar(name, localVars)
	if !ok {
		return -1
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
	return j
}

// appendNamedParam writes the bound value (or, when the name is unresolvable,
// the verbatim $name text) for the named reference spanning pos..j and
// returns the position after it.
func (i *Interp) appendNamedParam(b *strings.Builder, sql string, pos, j int, localVars map[string]string) int {
	name := sql[pos+1 : j]
	if v, ok := i.getVar(name, localVars); ok {
		b.WriteString(tclSQLLiteral(v))
		return j
	}
	b.WriteString(sql[pos:j])
	return j
}

// tclSQLLiteral renders v as a SQL string literal.
func tclSQLLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

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
func (i *Interp) cmdDoExecSQL(rawWords []rawWord, localVars map[string]string) error {
	words, _ := i.evalAllWords(rawWords[1:], localVars)
	// Skip optional -db flag
	idx := 0
	if idx < len(words) && words[idx] == "-db" {
		idx += 2
	}
	if idx >= len(words) {
		return nil
	}
	name := words[idx]
	sql := ""
	expected := ""
	idx++
	sqlRawIdx := idx
	if idx < len(words) {
		sql = words[idx]
		idx++
	}
	if idx < len(words) {
		expected = words[idx]
	}
	// A braced SQL word keeps TCL variables unexpanded; sqlite3 binds them
	// as named parameters from TCL scope. Inline resolvable ones as SQL
	// literals (brackets stay verbatim, preserving JSON arrays).
	if sqlRawIdx < len(rawWords) && rawWords[sqlRawIdx+1].Braced && sql != "" {
		sql = i.bindSQLParams(sql, localVars)
	}

	// Determine if it's a query or exec
	sqlType := "exec"
	lastStmt := lastStatement(sql)
	if isQueryStmt(lastStmt) {
		sqlType = "query"
	}

	name = i.fixTestName(name, localVars)
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     sqlType,
		SQL:      sql,
		Expected: expected,
		TestName: name,
	})
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
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     "catch",
		SQL:      sql,
		Expected: expected,
		TestName: name,
	})
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

	// Find the body (braced) and expected (braced or a $var reference)
	bodyWord := rawWords[2]
	expected := ""
	if len(rawWords) >= 4 {
		if rawWords[3].Braced {
			expected = rawWords[3].Text
		} else {
			// Unbraced expectation (e.g. `$res` set from a loop list):
			// substitute so the captured statement carries the real value.
			expected, _ = i.evalWord(rawWords[3], localVars)
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

	// If expected is non-empty, attach it to the last captured statement
	if expected != "" && len(i.stmts) > 0 {
		last := &i.stmts[len(i.stmts)-1]
		if last.TestName == name && last.Expected == "" {
			last.Expected = expected
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
		sql = i.substitute(rawWords[2].Text, localVars)
	} else {
		sql, _ = i.evalWord(rawWords[2], localVars)
	}
	i.curTest = name
	i.stmts = append(i.stmts, Stmt{
		Type:     "query",
		SQL:      "EXPLAIN QUERY PLAN " + sql,
		TestName: name,
	})
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
