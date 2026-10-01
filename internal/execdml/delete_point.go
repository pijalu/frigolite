package execdml

// Rowid-pinned single-row DELETE fast path (the OLTP workhorse
// "DELETE FROM t WHERE id=?"). The generic pipeline collects matching rows
// (per-candidate row maps), then walks the delete machinery: identity maps,
// per-row preupdate lookups, the statement-wide rowid-cache bookkeeping. When
// the statement is the plain point shape — the WHERE clause is exactly one
// rowid equality — one seek reads the row, one seek deletes it, and the same
// per-row steps (index maintenance, preupdate hook, rowid-cache invalidate)
// run inline. Every gate mirrors execDeleteBulk's routing: anything the fast
// path does not handle exactly returns handled=false and the statement runs
// the unmodified generic pipeline.

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// execPointDelete applies the rowid-pinned single-row DELETE fast path.
// handled=false means the statement does not fit the fast path's exact shape
// and must run the generic pipeline (collectDeleteRows → execDeleteBulk).
func (e *DMLExecutor) execPointDelete(s *sql.DeleteStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, tree *btree.BTree) (*Result, bool) {
	if !e.pointDeleteEligible(s, tableEntry) {
		return nil, false
	}
	plan := e.planDMLSeek(tableEntry, colDefs, s.Where, tableEntry.Name, e.currentDMLCtx)
	// plan.index != nil is an indexed-seek plan: candidate narrowing only,
	// the WHERE must still be evaluated per candidate — not this shape.
	if plan == nil || plan.empty || plan.index != nil {
		return nil, false
	}
	// The pinned rowid equality must be the WHOLE WHERE clause: the seek's
	// hit then matches by construction and the clause needs no evaluation.
	if len(splitAndTerms(s.Where)) != 1 {
		return nil, false
	}
	// execDeleteBulk skips the statement journal inside the FTS flush (the
	// flush's shadow deletes share the enclosing rollback scope) — that
	// context keeps the generic path.
	if e.ctx.InFTSFlush() {
		return nil, false
	}
	// Statement journal for the FK-failure rollback (pager.c sub-journal),
	// exactly as execDeleteBulk opens it for every statement.
	stmt := dbCtx.Pager.BeginStatement()
	defer dbCtx.Pager.EndStatement(stmt)

	// SQLITE_TEST interrupt countdown: one op per row examined
	// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
	if err := e.ctx.CheckProgress(); err != nil {
		return nil, false // the scan path surfaces progress errors itself
	}
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, false // anomaly: generic pipeline
	}
	found, serr := cursor.SeekToRowID(plan.rowid)
	if serr != nil {
		return nil, false // anomaly: generic pipeline
	}
	if !found {
		// No-match point DELETE: execDeleteBulk still invalidates the rowid
		// cache on its way out.
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
		return &Result{}, true
	}
	payload, realRowID, rerr := cursor.ReadCellData()
	if rerr != nil {
		return nil, false // anomaly: generic pipeline
	}
	rec, derr := storage.DecodeRecord(payload)
	if derr != nil || rec == nil {
		return nil, false // anomaly: generic pipeline
	}

	// The row's positional snapshot (dropped-column re-alignment, added-column
	// DEFAULTs, INTEGER PRIMARY KEY rowid-alias substitution) — the same
	// raw values the generic collect retains for the preupdate hook and the
	// index-maintenance keys.
	rowPlan := e.ctx.NewDMLRowPlan(colDefs, nil, nil)
	values := e.ctx.DMLRowSnapshot(rowPlan, rec.Values, len(rec.Values), realRowID)
	row := &dmlRow{plan: rowPlan, values: values, valueCount: len(rec.Values), rowID: realRowID}

	if _, err := tree.DeleteCellByRowID(realRowID); err != nil {
		return &Result{Error: err}, true
	}
	if err := e.maintainIndexesOnDelete(tableEntry, colDefs, []*dmlRow{row}); err != nil {
		return &Result{Error: err}, true
	}
	if res := e.fireDeletePreupdate(tableEntry, dbCtx, colDefs, row); res != nil {
		return res, true
	}
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage)
	return &Result{Changes: 1}, true
}

// pointDeleteEligible reports the statement shape the fast path rewrites
// exactly: a plain DELETE (no RETURNING, no ORDER BY/LIMIT) of an ordinary
// rowid table without triggers or FK enforcement, that is not an FTS content
// shadow table (recordDeletedContentDocs tracks those deletes).
func (e *DMLExecutor) pointDeleteEligible(s *sql.DeleteStmt, tableEntry *schema.Entry) bool {
	if s.HasReturning || len(s.OrderBy) > 0 || s.Limit != nil {
		return false
	}
	if tableIsWithoutRowid(tableEntry.SQL) {
		return false
	}
	if e.hasTriggersForTable(tableEntry.Name) {
		return false
	}
	if e.ctx.ForeignKeys() {
		return false
	}
	if strings.HasSuffix(strings.ToLower(tableEntry.Name), "_content") {
		return false
	}
	return true
}
