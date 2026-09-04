// Package exec: per-statement external file validation (pager.c
// sqlite3PagerSharedLock / btree.c lockBtree emulation) and the statement
// classification that decides which statements open a database b-tree.
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// execResetExternalChecks clears the per-statement external-mod check flag on
// every database's schema manager at the start of an outermost statement, so
// the first schema read observes commits made by other connections between
// statements (schema.Manager.checkExternalMod's checkedThisStmt gate then
// skips the redundant FileChangeCounter Pread for the rest of the statement).
func (e *Engine) execResetExternalChecks() {
	for _, ctx := range e.databases {
		if ctx != nil && ctx.Schema != nil {
			ctx.Schema.ResetStatementCheck()
		}
	}
}

// execDBFileChecks performs the per-statement database-file validation of
// pager.c sqlite3PagerSharedLock and btree.c lockBtree: every file-backed
// database (main and attached) re-reads its change-version stamp — dropping
// the page cache and schema caches when the file changed externally — and
// validates the header's page count against the file's actual size (a
// truncated file is corruption). A non-nil result carries that error; TEMP
// and in-memory databases are skipped. The FTS flush's internal statements
// run mid-flush on unflushed pages and must not drop the pager cache.
// Like SQLite, whose vdbe programs only carry OP_Transaction (and hence
// lockBtree) when the statement actually uses a database b-tree, a pure
// literal SELECT ("SELECT 1") skips the checks entirely (incrcorrupt 1.12/
// 2.12 report SQLITE_OK on a corrupt database).
func (e *Engine) execDBFileChecks(stmt sql.Stmt) *Result {
	if e.tx.inFTSFlush {
		return nil
	}
	if !stmtTouchesDatabase(stmt) {
		return nil
	}
	changed := false
	for _, ctx := range e.databases {
		dbChanged, err := checkDBFileCtx(ctx, e.settings.writableSchema)
		if err != nil {
			return &Result{Error: err}
		}
		changed = changed || dbChanged
	}
	if changed {
		e.clearExternalCaches()
	}
	return nil
}

// checkDBFileCtx validates one attached database's file: an external change
// (change-version stamp or size difference) drops that database's caches,
// and a header page count beyond the file is corruption. TEMP and in-memory
// databases have no file and are skipped.
//
// PRAGMA writable_schema=ON mirrors btree.c lockBtree's escape hatch: the
// nPage>nPageFile corruption is reported only "if( sqlite3WritableSchema
// (pBt->db)==0 )" (src/btree.c:3415-3418) — with the flag set the btree
// tolerates a header leading the file (incrvacuum-17.1 runs
// incremental_vacuum against such an image and expects success).
func checkDBFileCtx(ctx *DatabaseContext, writableSchema bool) (changed bool, err error) {
	if ctx == nil || ctx.Pager == nil || ctx.IsMemory || ctx.Pager.IsMemory() {
		return false, nil
	}
	upper := strings.ToUpper(ctx.Name)
	if upper == "TEMP" || upper == "TEMPORARY" {
		return false, nil
	}
	if ctx.Pager.CheckExternalFile() {
		if ctx.Schema != nil {
			ctx.Schema.InvalidateCache()
		}
		changed = true
	}
	if !writableSchema && ctx.Pager.HeaderBeyondFile() {
		return changed, fmt.Errorf("database disk image is malformed")
	}
	return changed, nil
}

// stmtTouchesDatabase reports whether a statement uses a database b-tree
// (and would therefore run OP_Transaction / lockBtree in SQLite's vdbe).
// SELECTs with no FROM clause touch no b-tree unless an expression
// subquery references one; every other statement kind is conservatively
// assumed to touch the database.
func stmtTouchesDatabase(stmt sql.Stmt) bool {
	s, ok := stmt.(*sql.SelectStmt)
	if !ok {
		if p, isPragma := stmt.(*sql.PragmaStmt); isPragma {
			// PRAGMA writable_schema never opens a b-tree transaction in
			// SQLite: the vdbe program just sets the connection flag
			// (pragma.c PragTyp_WRITABLE_SCHEMA emits no OP_Transaction),
			// so it succeeds even on an image whose header page count
			// exceeds the file (incrvacuum-17.1 relies on this).
			if strings.EqualFold(p.Name, "writable_schema") {
				return false
			}
		}
		return true
	}
	return selectTouchesDatabase(s)
}

// selectTouchesDatabase walks a SELECT tree: any FROM clause or table-
// referencing subquery/EXISTS in the projection, WHERE, GROUP BY, HAVING, or
// ORDER BY makes the statement use a b-tree.
func selectTouchesDatabase(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	// A zero TableRef (no FROM clause) touches no b-tree; a named table,
	// FROM-subquery, or table-valued function does.
	if s.From.Name != "" || s.From.Subquery != nil || len(s.From.Args) > 0 {
		return true
	}
	if anyColumnTouchesDatabase(s.Columns) {
		return true
	}
	if anyExprTouchesDatabase([]sql.Expr{s.Where, s.Having}) {
		return true
	}
	if anyExprTouchesDatabase(s.GroupBy) {
		return true
	}
	for _, o := range s.OrderBy {
		if exprTouchesDatabase(o.Expr) {
			return true
		}
	}
	if s.Union != nil {
		return selectTouchesDatabase(s.Union)
	}
	return false
}

// anyColumnTouchesDatabase reports whether any SELECT-list column carries a
// table-referencing subquery.
func anyColumnTouchesDatabase(cols []sql.SelectColumn) bool {
	for _, col := range cols {
		if exprTouchesDatabase(col.Expr) {
			return true
		}
	}
	return false
}

// anyExprTouchesDatabase reports whether any expression in the slice carries
// a table-referencing subquery.
func anyExprTouchesDatabase(exprs []sql.Expr) bool {
	for _, expr := range exprs {
		if exprTouchesDatabase(expr) {
			return true
		}
	}
	return false
}

// exprTouchesDatabase reports whether an expression contains a subquery
// that references a table (a correlated or uncorrelated SELECT inside an
// expression still runs OP_Transaction when it scans a b-tree).
func exprTouchesDatabase(expr sql.Expr) bool {
	switch v := expr.(type) {
	case nil:
		return false
	case *sql.Subquery:
		return selectTouchesDatabase(v.Select)
	case *sql.ExistsExpr:
		return selectTouchesDatabase(v.Select)
	case *sql.ParenExpr:
		return exprTouchesDatabase(v.Expr)
	default:
		return compoundExprTouchesDatabase(expr)
	}
}

// compoundExprTouchesDatabase handles the composite node kinds of
// exprTouchesDatabase (operators, function calls, CASE, BETWEEN, IN).
func compoundExprTouchesDatabase(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return exprTouchesDatabase(v.Left) || exprTouchesDatabase(v.Right)
	case *sql.UnaryOp:
		return exprTouchesDatabase(v.Operand)
	case *sql.Between:
		return exprTouchesDatabase(v.Operand) || exprTouchesDatabase(v.Low) || exprTouchesDatabase(v.High)
	case *sql.FuncCall:
		return anyExprTouchesDatabase(v.Args)
	case *sql.CaseExpr:
		return caseExprTouchesDatabase(v)
	case *sql.InList:
		return anyExprTouchesDatabase(append([]sql.Expr{v.Operand}, v.List...))
	default:
		return false
	}
}

// caseExprTouchesDatabase walks a CASE expression's operand, WHEN/THEN
// pairs, and ELSE branch.
func caseExprTouchesDatabase(c *sql.CaseExpr) bool {
	if exprTouchesDatabase(c.Operand) || exprTouchesDatabase(c.Else) {
		return true
	}
	for _, w := range c.Whens {
		if exprTouchesDatabase(w.When) || exprTouchesDatabase(w.Then) {
			return true
		}
	}
	return false
}

// clearExternalCaches drops the engine's derived caches after a pager cache
// invalidation (external file change): table entries, rowid sequences, and
// AUTOINCREMENT sequences are re-read from the reloaded file.
func (e *Engine) clearExternalCaches() {
	e.caches.tableCache = make(map[string]*cachedTableEntry)
	e.caches.nextRowIDCache = make(map[rowidCacheKey]int64)
	e.caches.autoIncSeq = make(map[rowidCacheKey]int64)
}
