// Package execdml implements DML execution.
package execdml

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// insertEndHooks carries the statement-end work an INSERT may owe (fts5
// shadow flush, AUTOINCREMENT sequence write, OR REPLACE rollback). The hook
// pipeline used to live in execInsertInner as defers over a named return —
// taking &ret unconditionally (the replace-snapshot call evaluates its
// **Result argument on every statement) forced the named result to the heap
// for EVERY insert, one allocation and two GC-scanned objects per row. The
// hook-free fast path now runs the body with h == nil (no named return, no
// address-taken result); only statements whose shape needs an end hook pay
// for the wrapper.
type insertEndHooks struct {
	kind uint8 // hookAutoInc | hookReplace
	// fts5: resolved after the target resolves (nil for view targets); the
	// sole authority for whether the statement-end flush runs.
	fts5Entry *schema.Entry
	// autoincrement: pending sqlite_sequence write (insert.c autoIncrementEnd).
	seqPg    *pager.Pager
	seqRoot  uint32
	seqTable string
	// OR REPLACE: statement journal opened before the row writes; the
	// deferred rollback consumed to live at this exact point.
	replaceCtx  *DatabaseContext
	replaceStmt *pager.StmtJournal
}

const (
	hookAutoInc uint8 = 1 << iota
	hookReplace
)

// execInsertInner is execInsert's statement pipeline (the echo write-through
// wrapper above re-routes its errors).
func (e *DMLExecutor) execInsertInner(s *sql.InsertStmt) *Result {
	if !e.insertNeedsEndHooks(s) {
		return e.execInsertInnerBody(s, nil)
	}
	var h insertEndHooks
	if s.IsReplace {
		h.kind |= hookReplace
	}
	res := e.execInsertInnerBody(s, &h)
	e.finishInsertEndHooks(&h, &res)
	return res
}

// insertNeedsEndHooks reports whether the statement's shape may owe
// statement-end work: INSERT OR REPLACE (rollback on error), an fts5 target
// (index blob flush at statement boundary — sqlite3Fts5StorageSync), or an
// AUTOINCREMENT table (sequence write-back). Everything else — the
// bulk-load shape — runs the body directly with no hooks and no heap traffic.
// Echo write-through targets are conservatively hooked: prepareInsertStmt
// rewrites the echo vtab's name to its source table AFTER this gate runs,
// and that source may be fts5 or AUTOINCREMENT — the body's post-resolution
// checks decide the actual work.
func (e *DMLExecutor) insertNeedsEndHooks(s *sql.InsertStmt) bool {
	if s.IsReplace {
		return true
	}
	if _, isFTS5 := e.ctx.FTS5Tables()[s.Table]; isFTS5 {
		return true
	}
	if _, isEcho := e.ctx.EchoVTabSource(s.Table); isEcho {
		return true
	}
	// Conservative gate form: a false verdict must be PROVEN (parsed column
	// definitions on this connection) — any DDL wipes that cache, and a cold
	// ask here would drop this statement's AUTOINCREMENT sequence write
	// (autoIncStatementSetup runs later, after ParseColumnDefs, so its
	// authoritative verdict decides the actual work).
	return e.ctx.TableMayHaveAutoIncrement(s.Table)
}

// finishInsertEndHooks applies the statement-end work in the same order the
// historical defers unwound (LIFO: the replace-snapshot rollback registered
// last ran first, then the AUTOINCREMENT write, then the fts5 flush). It
// runs on every exit of the hooked wrapper — success and failure alike —
// exactly like the defers it replaces.
func (e *DMLExecutor) finishInsertEndHooks(h *insertEndHooks, retp **Result) {
	if h.kind&hookReplace != 0 && h.replaceStmt != nil {
		if *retp != nil && (*retp).Error != nil {
			e.ctx.RollbackPagerStatement(h.replaceCtx.Pager, h.replaceStmt)
			// Rows whose rowids were computed for the aborted statement
			// are gone; the cached rowid counter must not survive.
			e.ctx.ResetNextRowIDCache()
			e.ctx.ResetAutoIncSeq()
		}
		h.replaceCtx.Pager.EndStatement(h.replaceStmt)
		h.replaceStmt = nil
	}
	if h.kind&hookAutoInc != 0 && h.seqPg != nil {
		e.writeAutoIncSeqOnSuccess(h.seqPg, h.seqRoot, h.seqTable, retp)
	}
	if h.fts5Entry != nil {
		e.flushInsertFTS5Shadow(h.fts5Entry, retp)
	}
}

// execInsertInnerBody is execInsertInner's statement pipeline proper. h is
// nil on the hook-free fast path (a bulk VALUES load): every hook site is
// gated on h != nil, so the body takes no statement-end responsibility and
// keeps its results off the heap.

// flushInsertFTS5Shadow persists a dirty fts5 index at statement end,
// surfacing a flush error only when the statement did not already fail.
func (e *DMLExecutor) flushInsertFTS5Shadow(tableEntry *schema.Entry, ret **Result) {
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok && t5 != nil {
		if ferr := e.flushFTS5Shadow(t5); ferr != nil && (*ret == nil || (*ret).Error == nil) {
			*ret = &Result{Error: ferr}
		}
	}
}

// writeAutoIncSeqOnSuccess writes the AUTOINCREMENT running max back to the
// real sqlite_sequence table at statement end (insert.c autoIncrementEnd),
// skipping the write when the statement failed.
func (e *DMLExecutor) writeAutoIncSeqOnSuccess(seqPg *pager.Pager, seqRoot uint32, seqTable string, ret **Result) {
	if *ret != nil && (*ret).Error != nil {
		return
	}
	seq, ok := e.ctx.AutoIncSeqFor(seqPg, seqRoot)
	if !ok {
		seq = 0
	}
	_ = e.ctx.WriteSQLiteSequence(seqPg, seqTable, seq)
}

// validateInsertReturning validates the RETURNING clause against the table's
// column definitions.

// validateInsertReturning validates the RETURNING clause against the table's
// column definitions.

// autoIncStatementSetup validates the sqlite_sequence table up front for an
// AUTOINCREMENT insert and reports the statement-end sequence write (a no-op
// for non-AUTOINCREMENT targets: needSeqWrite=false). The write itself is
// applied by finishInsertEndHooks — the historical closure-based form captured
// the caller's named return by address, forcing it to the heap per statement.
// The sqlite_sequence table must exist and be an ordinary rowid table before
// an AUTOINCREMENT insert uses it (autoinc-12.2/12.3: a renamed-away or
// impostor sqlite_sequence fails the insert with SQLITE_CORRUPT, "database
// disk image is malformed").
func (e *DMLExecutor) autoIncStatementSetup(dbCtx *DatabaseContext, tableEntry *schema.Entry) (needSeqWrite bool, seqPg *pager.Pager, seqRoot uint32, seqTable string, res *Result) {
	if !(e.ctx.TableHasAutoIncrement(tableEntry.Name) && dbCtx != nil) {
		return false, nil, 0, "", nil
	}
	// The sqlite_sequence table must exist and be an ordinary rowid table before
	// an AUTOINCREMENT insert uses it (autoinc-12.2/12.3:
	// a renamed-away or impostor sqlite_sequence fails the insert with
	// SQLITE_CORRUPT, "database disk image is malformed").
	if res := e.validateSequenceTable(dbCtx); res != nil {
		return false, nil, 0, "", res
	}
	return true, dbCtx.Pager, tableEntry.RootPage, tableEntry.Name, nil
}
