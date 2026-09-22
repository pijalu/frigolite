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

// stmtTextAt returns the raw source text for statement si, or "" when the
// prepared statement list is longer than the split texts.
func stmtTextAt(texts []string, si int) string {
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
func (db *DB) execPrepared(stmt sql.Stmt, stmtText string) *exec.Result {
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
	stmts, err := db.engine.Prepare(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: err}
	}

	texts := splitSQLStatements(sqlStr)
	// The whole-batch BEGIN EXCLUSIVE check is a property of the batch TEXT,
	// not of any single statement: compute it once (a per-statement
	// EqualFold over the whole batch made multi-statement batches O(n^2) in
	// the batch length).
	wholeBatchBeginExclusive := strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(sqlStr, ";")), "BEGIN EXCLUSIVE")
	var lastResult *exec.Result
	for si, stmt := range stmts {
		res := db.execPrepared(stmt, stmtTextAt(texts, si))
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

// Query executes a SQL query and returns rows.
// Multiple semicolon-separated statements are all executed and their results
// concatenated, matching SQLite's behavior for multi-statement queries.
func (db *DB) Query(sqlStr string) *Result {
	if db == nil || db.engine == nil {
		return &Result{Error: fmt.Errorf("frigolite: database not initialized"), SQL: sqlStr}
	}
	stmts, err := db.engine.Prepare(sqlStr)
	if err != nil && len(stmts) == 0 {
		db.engine.SetLastErr(err.Error(), "SQLITE_ERROR")
		return &Result{Error: err, SQL: sqlStr}
	}

	if len(stmts) == 0 {
		db.engine.SetLastErr("", "")
		return &Result{SQL: sqlStr}
	}

	var allRows [][]interface{}
	var allColumns []string
	texts := splitSQLStatements(sqlStr)
	for si, stmt := range stmts {
		res := db.execPrepared(stmt, stmtTextAt(texts, si))
		if res.Error != nil {
			db.engine.SetLastErr(res.Error.Error(), db.errorCode(res.Error))
			r := execResult(res)
			r.SQL = sqlStr
			return r
		}
		expandResultZeroBlobs(res)
		allRows = append(allRows, res.Rows...)
		if allColumns == nil {
			allColumns = res.Columns
		}
		if res.LastInsertRowID > 0 {
			db.lastRowID = res.LastInsertRowID
		}
	}

	db.engine.SetLastErr("", "")

	return &Result{
		Columns: allColumns,
		Rows:    allRows,
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
