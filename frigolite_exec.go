// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// Statement execution entry points (Exec / Query) and the supporting
// trace plumbing, debug dump and token-aware statement splitter.
import (
	"fmt"
	"strings"
	"time"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/recover"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/value"
)

// execResult converts an exec.Result to a public Result.
func execResult(er *exec.Result) *Result {
	if er == nil {
		return nil
	}
	return &Result{
		Columns:         er.Columns,
		Rows:            er.Rows,
		Changes:         er.Changes,
		Error:           er.Error,
		LastInsertRowID: er.LastInsertRowID,
	}
}

// EvalExecSQL runs SQL text via the engine and returns every result cell of
// every row of every SELECT statement, joined by sep. NULL cells render as
// the empty string; non-SELECT statements contribute no cells. An error in
// any statement aborts with that error. This mirrors SQLite's ext/misc/eval.c
// eval() function and is used by the test-harness SQL-executing UDF
// (`db function execsql execsql` in tkt3080.test).
func (db *DB) EvalExecSQL(sqlStr, sep string) (string, error) {
	if db == nil || db.engine == nil {
		return "", fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.EvalExecSQL(sqlStr, sep)
}

// RecoverSQL produces the .recover-style SQL rebuild script for this
// connection's database, reading pages in-process through the pager
// (SQLite ext/misc/recover.c sqlite3recover port in internal/recover).
// Pending writes are flushed first so the script reflects committed state.
// ignoreFreelist selects the .recover -ignore-freelist option: freelist
// pages are skipped when scanning for orphaned content, so no
// lost_and_found rows are emitted for them.
func (db *DB) RecoverSQL(ignoreFreelist bool) (string, error) {
	if db == nil || db.pager == nil {
		return "", fmt.Errorf("frigolite: database not initialized")
	}
	if err := db.pager.Flush(); err != nil {
		return "", fmt.Errorf("frigolite: recover flush: %w", err)
	}
	return recover.RecoverSQL(db.pager, recover.Options{IgnoreFreelist: ignoreFreelist})
}

// statementTexts returns the per-statement raw texts for the trace hooks
// (splitSQLStatements), short-circuiting the common single-statement form:
// when the batch contains no semicolon byte at all there is nothing to split
// (a string/blob literal containing one goes through the full tokenizer), and
// splitSQLStatements would return exactly [sqlStr] — its EOF branch appends
// the untrimmed tail verbatim. A nil return tells stmtTextAt to use the batch
// text itself, skipping the tokenizer walk on every plain single-statement
// Exec/Query call.
func statementTexts(sqlStr string) []string {
	if strings.IndexByte(sqlStr, ';') < 0 {
		return nil
	}
	return splitSQLStatements(sqlStr)
}

// stmtTextAt returns the raw source text for statement si, or "" when the
// prepared statement list is longer than the split texts. texts == nil means
// the batch had no semicolon anywhere, so the whole batch text is statement
// 0's text (and a multi-statement batch always split, so no later si exists).
func stmtTextAt(sqlStr string, texts []string, si int) string {
	if texts == nil {
		if si == 0 {
			return sqlStr
		}
		return ""
	}
	if si < len(texts) {
		return texts[si]
	}
	return ""
}

// execPrepared runs one prepared statement under the statement trace hooks:
// VACUUM is routed through the connection's internal vacuum executor with
// internal tracing enabled, any other statement goes through engine.Exec
// with one trace-row event per result row (the sqlite3_trace/profile hook
// call sequence, shared by Exec and Query).
//
// With no trace/profile hook registered (the default) the whole gate — the
// elapsed-time clock and the Begin/End/FireTraceRow calls — is skipped: it
// would otherwise cost two wall-clock reads and three calls per statement
// on every statement the connection runs.
func (db *DB) execPrepared(stmt sql.Stmt, stmtText string) *exec.Result {
	if !db.engine.StmtHooksActive() {
		if vs, ok := stmt.(*sql.VacuumStmt); ok {
			db.engine.SetTraceInternal(true)
			res := db.execVacuumStmt(vs)
			db.engine.SetTraceInternal(false)
			return res
		}
		return db.engine.Exec(stmt)
	}
	t0 := time.Now()
	db.engine.BeginStmtTrace(stmtText)
	if vs, ok := stmt.(*sql.VacuumStmt); ok {
		db.engine.SetTraceInternal(true)
		res := db.execVacuumStmt(vs)
		db.engine.SetTraceInternal(false)
		db.engine.EndStmtTrace(stmtText, time.Since(t0).Nanoseconds())
		return res
	}
	res := db.engine.Exec(stmt)
	for ri := 0; ri < len(res.Rows); ri++ {
		db.engine.FireTraceRow()
	}
	db.engine.EndStmtTrace(stmtText, time.Since(t0).Nanoseconds())
	return res
}

// Exec executes a SQL statement that does not return rows.
// Multiple statements in the same string are all executed (consistent with
// SQLite's sqlite3_prepare_v2 behavior for DDL/DML batches).
func (db *DB) Exec(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized")}
	}
	stmts, err := db.engine.PrepareExec(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: err}
	}

	texts := statementTexts(sqlStr)
	// The whole-batch BEGIN EXCLUSIVE check is a property of the batch TEXT,
	// not of any single statement: compute it once (a per-statement
	// EqualFold over the whole batch made multi-statement batches O(n^2) in
	// the batch length). The length screen rejects every real statement
	// before the fold.
	batchText := strings.TrimSpace(strings.TrimSuffix(sqlStr, ";"))
	wholeBatchBeginExclusive := len(batchText) == 15 && strings.EqualFold(batchText, "BEGIN EXCLUSIVE")
	var lastResult *exec.Result
	for si, stmt := range stmts {
		res := db.execPrepared(stmt, stmtTextAt(sqlStr, texts, si))
		if res.Error != nil {
			db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
			return execResult(res)
		}
		lastResult = res
		if wholeBatchBeginExclusive {
			db.engine.BeginExclusive()
		}
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
	}

	if err != nil {
		// The parseable prefix executed without error; report the trailing
		// syntax error (SQLite reaches it only after the prefix runs).
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: err}
	}

	// A successful statement clears the connection's last-error state
	// (sqlite3_errcode returns SQLITE_OK after the most recent API call
	// succeeds; sqlite3_errmsg returns "not an error").
	db.engine.SetLastErr("", "")

	if lastResult == nil {
		return &Result{}
	}

	result := execResult(lastResult)

	return result
}

// foldQueryResult folds one executed statement's result into Query's batch
// accumulator. A multi-statement batch concatenates row headers and keeps the
// first non-nil Columns (the append-based historical semantics); a
// single-statement batch hands the engine's rows and columns to the caller
// verbatim — exec.Result.Rows is freshly allocated per engine call and never
// retained, and the Exec path already passes er.Rows through unchanged, so
// caller ownership is unchanged and the redundant per-query backing-array
// copy disappears. The append path yields a nil Rows for a zero-row result
// (append to nil adds nothing), so the single-statement branch keeps that
// exact nil/empty distinction.
func foldQueryResult(allRows [][]interface{}, allColumns []string, res *exec.Result, multi bool) ([][]interface{}, []string) {
	if !multi {
		if len(res.Rows) == 0 {
			return nil, res.Columns
		}
		return res.Rows, res.Columns
	}
	allRows = append(allRows, res.Rows...)
	if allColumns == nil {
		allColumns = res.Columns
	}
	return allRows, allColumns
}

// runSingleStmt executes one parsed statement through the shared statement
// pipeline (trace hooks, zero-blob expansion, connection last-error state) —
// the per-statement core of DB.Query, reused by the prepared-statement fast
// path.
func (db *DB) runSingleStmt(stmt sql.Stmt, stmtText string) *exec.Result {
	res := db.execPrepared(stmt, stmtText)
	if res.Error != nil {
		db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
		return res
	}
	expandResultZeroBlobs(res)
	if res.LastInsertRowID > 0 {
		db.lastRowID = res.LastInsertRowID
	}
	db.engine.SetLastErr("", "")
	return res
}

// runSQLText executes a SQL text (already single-statement in the prepared
// path, potentially a batch otherwise) through engine.Prepare plus the
// statement pipeline, returning the internal equivalent of DB.Query's
// result: concatenated rows/columns for batches, the statement's own rows
// for a single statement (zero rows → nil Rows), and the last-error state
// maintained on the connection.
func (db *DB) runSQLText(sqlStr string) *exec.Result {
	stmts, err := db.engine.PrepareExec(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &exec.Result{Error: err}
	}
	if len(stmts) == 0 {
		db.engine.SetLastErr("", "")
		return &exec.Result{}
	}

	var allRows [][]interface{}
	var allColumns []string
	texts := statementTexts(sqlStr)
	multi := len(stmts) > 1
	var last *exec.Result
	for si, stmt := range stmts {
		res := db.execPrepared(stmt, stmtTextAt(sqlStr, texts, si))
		if res.Error != nil {
			db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
			return res
		}
		expandResultZeroBlobs(res)
		allRows, allColumns = foldQueryResult(allRows, allColumns, res, multi)
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
		last = res
	}

	db.engine.SetLastErr("", "")

	if !multi {
		// Single statement: hand the engine's own result through — the
		// fold's pass-through contract made the trailing copy a field-for-
		// field duplicate (Rows/Columns verbatim; zero rows normalized to
		// nil Rows). `last` is engine-internal and dead once the caller has
		// read it, so the copy bought nothing on this path.
		if len(last.Rows) == 0 {
			last.Rows = nil
		}
		last.Columns = allColumns
		return last
	}
	out := *last
	out.Rows, out.Columns = allRows, allColumns
	return &out
}

// Query executes a SQL query and returns rows.
// Multiple semicolon-separated statements are all executed and their results
// concatenated, matching SQLite's behavior for multi-statement queries.
func (db *DB) Query(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized"), SQL: sqlStr}
	}
	r := db.runSQLText(sqlStr)
	if r.Error != nil {
		out := execResult(r)
		out.SQL = sqlStr
		return out
	}
	return &Result{
		Columns: r.Columns,
		Rows:    r.Rows,
		SQL:     sqlStr,
	}
}

// expandResultZeroBlobs materializes zeroblob(N) cells into their expanded
// zero bytes before a result leaves the engine (vdbeapi.c
// sqlite3_column_blob expands the MEM_Zero flag on access; the value a host
// application observes is always the N zero bytes, never a lazy marker).
func expandResultZeroBlobs(res *exec.Result) {
	for _, row := range res.Rows {
		for i, v := range row {
			if z, ok := v.(value.ZeroBlob); ok {
				row[i] = z.Bytes()
			}
		}
	}
}

// DumpAll logs all schema entries and table contents (debug helper).
func (db *DB) DumpAll() {
	entries, err := db.schema.GetEntries("")
	if err != nil {
		fmt.Printf("dump error: %v\n", err)
		return
	}
	fmt.Printf("=== Schema (%d entries) ===\n", len(entries))
	for _, e := range entries {
		fmt.Printf("  type=%s name=%s tbl_name=%s root=%d\n", e.Type, e.Name, e.TblName, e.RootPage)
	}

	// Dump table contents
	for _, e := range entries {
		if e.Type == schema.TypeTable {
			res := db.Query("SELECT rowid, * FROM " + e.Name)
			if res.Error != nil {
				fmt.Printf("  dump %s: %v\n", e.Name, res.Error)
				continue
			}
			fmt.Printf("\n=== %s (%d rows) ===\n", e.Name, len(res.Rows))
			fmt.Printf("  columns: %v\n", res.Columns)
			for _, row := range res.Rows {
				fmt.Printf("  %v\n", row)
			}
		}
	}
}

// splitSQLStatements cuts a SQL script into per-statement raw texts at
// top-level semicolons. The cut is token-aware (sqlite3_prepare's walk):
// semicolons inside string literals, blob literals, bracket identifiers, or
// comments never split, so each chunk is exactly the text of one statement
// including its trailing semicolon (sqlite3_stmt_sql semantics for the
// trace/profile hooks).
func splitSQLStatements(sqlStr string) []string {
	tok := sql.NewTokenizer(sqlStr)
	var texts []string
	start := 0
	for {
		t := tok.Next()
		switch t.Type {
		case sql.TokenEOF, sql.TokenError:
			if tail := sqlStr[start:]; strings.TrimSpace(tail) != "" {
				texts = append(texts, tail)
			}
			return texts
		case sql.TokenSemicolon:
			text := sqlStr[start : t.Pos+len(t.Value)]
			texts = append(texts, strings.TrimLeft(text, " \t\n\r\v\f"))
			start = t.Pos + len(t.Value)
		}
	}
}
