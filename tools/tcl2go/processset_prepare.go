// Package main implements the tcl2go tool.
//
// This file handles prepared-statement (`sqlite3_prepare`) set values and the
// `[time { SCRIPT }]` set-value forms.
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
	"github.com/pijalu/frigolite/tools/tclconvert/tcl/tclparser"
)

// processSetTimedValue handles `[time { SCRIPT }]` and `[lindex [time
// { SCRIPT }] N]` set values. Returns true when the value was a timing form.
func (tp *transpiler) processSetTimedValue(goName, bracketText string) bool {
	if strings.HasPrefix(bracketText, "[time ") {
		tp.processSetTimeValue(goName, bracketText)
		return true
	}
	if strings.HasPrefix(bracketText, "[lindex [time ") {
		tp.processSetLindexTimeValue(goName, bracketText)
		return true
	}
	return false
}

// prepareSQLExpr renders prepared SQL text as a Go string expression. Braced
// SQL passes verbatim (TCL performs no $var substitution inside braces);
// otherwise known TCL variables (e.g. bind.test's ?$iMaxVar) become runtime
// concatenations and unknown text stays literal.
func (tp *transpiler) prepareSQLExpr(sqlText string, braced bool) string {
	if braced {
		return fmt.Sprintf("%q", sqlText)
	}
	return tp.buildStringExpr(sqlText)
}

// recordPreparedStatement handles `set ::STMT [sqlite3_prepare db "SQL" -1
// TAIL]` (and the sqlite3_prepare_v2 form), recording the prepared statement
// for bind/step emulation.
func (tp *transpiler) recordPreparedStatement(goName, bracketText string) {
	inner := strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")
	parts := tclCmdWords(inner)
	if len(parts) < 3 || (parts[0] != "sqlite3_prepare" && parts[0] != "sqlite3_prepare_v2") {
		tp.declareStmtHandle(goName)
		return
	}
	sqlText := strings.TrimSpace(parts[2])
	sqlText = strings.Trim(sqlText, `"`)
	ps := tp.preparedStateRef()
	ps.stmts[goName] = sqlText
	conn := "db"
	if len(parts) > 1 {
		conn = tp.dbArgGo(parts[1])
	}
	ps.conns[goName] = conn
	// TCL substitution rules: a braced SQL word passes through verbatim; a
	// quoted/bare word has $var references substituted at runtime.
	braced := false
	if innerCmds := tclparser.ParseCommands(strings.TrimSuffix(strings.TrimPrefix(bracketText, "["), "]")); len(innerCmds) > 0 && len(innerCmds[0]) > 2 {
		braced = innerCmds[0][2].Braced
	}
	ps.braced[goName] = braced
	if !stmtVMEnabled() {
		// Legacy emulation: only queries run at prepare time (so compile
		// errors reach the connection); INSERT/DDL prepares stay inert.
		// The SQL expression honors the TCL substitution rules via
		// prepareSQLExpr, so a quoted "SELECT ... WHERE id = $row" prepare
		// interpolates the variable's runtime value (rtree8-1.3.2) instead
		// of handing the engine a bound-to-nothing $row parameter.
		tp.emitLine("// prepared %s: %s (bind/step emulation)", goName, sanitizeCommentLine(sqlText))
		if isQueryStmt(lastStatementSQL(sqlText)) {
			tp.emitLine("tclPrepareStep(%s, %s, %q)", conn, tp.prepareSQLExpr(sqlText, braced), goName)
		} else if strings.HasPrefix(sqlText, "$") {
			tp.emitLine("tclPrepareStep(%s, %s, %q)", conn, tclVarToGo(strings.TrimPrefix(sqlText, "$")), goName)
		}
		tp.emitPrepareTail(parts, sqlText, braced)
		tp.declareStmtHandle(goName)
		return
	}
	// Runtime prepare (sqlite3_prepare_v2): compile errors set the
	// connection's last-error state; the statement handle is kept for
	// bind/step/reset/finalize emulation. Preparing has no SQL side effects,
	// so INSERT/DDL prepares are safe here too.
	nByte := -1
	if len(parts) > 3 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[3])); err == nil {
			nByte = n
		}
	}
	tp.emitLine("_r = tclPrepareStmt(%s, %q, %s, %d)", conn, goName, tp.prepareSQLExpr(sqlText, braced), nByte)
	tp.emitLine("// prepared %s: %s (bind/step emulation)", goName, sanitizeCommentLine(sqlText))

	// sqlite3_prepare's TAIL argument (parts[4], e.g. `-1 TAIL`) names
	// the variable that receives the SQL text after the first statement.
	// capi2-2.x asserts `set SQL` after a multi-statement prepare returns
	// the tail; assign it (statistically for a literal SQL, at runtime
	// for a $var SQL).
	tp.emitPrepareTail(parts, sqlText, braced)
	tp.declareStmtHandle(goName)
}

// declareStmtHandle emits the prepared-statement handle variable declaration
// (a plain string in the emulation) when it is not already in scope.
func (tp *transpiler) declareStmtHandle(goName string) {
	if !tp.isVarDeclared(goName) {
		tp.emitLine("var %s string", goName)
		tp.vars = append(tp.vars, goName)
	}
	tp.emitLine("_ = %s // prepared statement handle", goName)
}

// emitPrepareQueryCheck emits a db.Query run for a prepared query so a
// compile-time error (bad column/table name) sets the connection's last-error
// state, matching the C-API prepare-error tests. Queries have no side effects;
// INSERT/DDL prepares are NOT run (their side effects happen at step).
//
//lint:ignore U1000 retained for generated prepared-query paths.
//lint:ignore U1000 retained for generated prepared-query paths.
func (tp *transpiler) emitPrepareQueryCheck(sqlText string) {
	// A $var SQL whose constant value is known (set earlier in the file) can
	// be classified statically.
	sqlForCheck := sqlText
	if strings.HasPrefix(strings.TrimSpace(sqlText), "$") {
		if v, ok := tp.sqlVarValues[tclVarToGo(strings.TrimPrefix(strings.TrimSpace(sqlText), "$"))]; ok {
			sqlForCheck = v
		}
	}
	if !isQueryStmt(lastStatementSQL(sqlForCheck)) || strings.HasPrefix(strings.TrimSpace(sqlForCheck), "$") {
		return
	}
	// A prepared multi-statement body (capi3-1.4: "SELECT name FROM
	// sqlite_master;SELECT 10") runs only its first statement at prepare;
	// SQLite compiles the whole text but the tail is returned, not executed.
	// db.Query on the full text would run both; use the first statement only
	// for error detection.
	firstStmt := splitSQLStatements(sqlForCheck)[0]
	tp.emitLine("r = db.Query(%q)", firstStmt)
	tp.emitLine("_ = r.Error // prepare error state is read via db.LastErr/LastErrCode")
}

// emitPrepareTail emits the assignment of sqlite3_prepare's TAIL argument
// (the variable that receives the SQL text after the first statement). braced
// reports whether the prepare's SQL word was TCL brace-quoted; a quoted word
// with $var references interpolates them, so the tail derives from the
// interpolated text.
func (tp *transpiler) emitPrepareTail(parts []string, sqlText string, braced bool) {
	if len(parts) < 5 {
		return
	}
	tailVar := strings.TrimSpace(parts[4])
	if tailVar == "" || strings.HasPrefix(tailVar, "-") || tailVar == "notused" || tailVar == "dummy" {
		return
	}
	goTail := tclVarToGo(strings.TrimPrefix(tailVar, "$"))
	if !isValidGoIdent(goTail) {
		return
	}
	if !tp.isVarDeclared(goTail) {
		tp.emitLine("var %s string", goTail)
		tp.vars = append(tp.vars, goTail)
	}
	if strings.HasPrefix(strings.TrimSpace(sqlText), "$") {
		sqlGo := tclVarToGo(strings.TrimPrefix(strings.TrimSpace(sqlText), "$"))
		tp.emitLine("%s = tclSqlTail(%s)", goTail, sqlGo)
	} else if !braced && hasVarRef(sqlText) {
		tp.emitLine("%s = tclSqlTail(%s)", goTail, tp.prepareSQLExpr(sqlText, braced))
	} else {
		tp.emitLine("%s = tclSqlTail(%q)", goTail, sqlText)
	}
	tp.emitLine("_ = %s // suppress unused warning", goTail)
}

// sanitizeCommentLine collapses whitespace (newlines, tabs, runs of spaces) in
// a text so it can be embedded in a single-line Go comment.
func sanitizeCommentLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// emitPrepareInCatch transpiles a `sqlite3_prepare[_v2] DB SQL NBYTE [TAIL]`
// command that appears as a catch BODY (`set rc [catch {sqlite3_prepare
// db $sql $nbytes TAIL} STMT]`). The C wrapper (test1.c test_prepare)
// reports failure as TCL_ERROR with "(<code>) <errmsg>"; the surrounding
// catch block maps that to rc="1" / STMT=message via _catchErr
// (sqllimits1-6.3: "1 {(18) statement too long}").
func (tp *transpiler) emitPrepareInCatch(args []tcl.RawWord) {
	words := make([]string, 0, len(args))
	for _, w := range args {
		words = append(words, w.Text)
	}
	// args exclude the command name: [DB SQL NBYTE TAIL]
	conn := "db"
	if len(words) > 0 {
		conn = tp.dbArgGo(words[0])
	}
	sqlArg := strings.Trim(words[1], `"`)
	sqlExpr := tp.prepareSQLExpr(sqlArg, args[1].Braced)
	name := fmt.Sprintf("catchprep%d", tp.varCount)
	tp.varCount++
	suffix := fmt.Sprintf("%d", tp.varCount)
	nByteExpr := "-1"
	if len(words) > 2 {
		word := strings.TrimSpace(words[2])
		if _, err := strconv.Atoi(word); err == nil {
			nByteExpr = word
		} else {
			// Runtime NBYTE ($var): parse at test runtime (unique temp
			// names so two catch-prepares in one scope don't collide).
			v := tclVarToGo(strings.TrimPrefix(word, "$"))
			tp.emitLine("_catchPrepN%s, _catchPrepErr%s := strconv.Atoi(%s)", suffix, suffix, v)
			tp.emitLine("_catchPrepNV%s := -1", suffix)
			tp.emitLine("if _catchPrepErr%s == nil { _catchPrepNV%s = _catchPrepN%s }", suffix, suffix, suffix)
			nByteExpr = fmt.Sprintf("_catchPrepNV%s", suffix)
		}
	}
	tp.emitLine("_catchPrepRc%s := tclPrepareStmt(%s, %q, %s, %s)", suffix, conn, name, sqlExpr, nByteExpr)
	tp.emitLine("if _catchPrepRc%s != \"SQLITE_OK\" {", suffix)
	tp.indent++
	tp.emitLine("_catchErr = tclPrepareCatchErr(%s, _catchPrepRc%s)", conn, suffix)
	tp.indent--
	tp.emitLine("}")
}
