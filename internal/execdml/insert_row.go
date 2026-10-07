package execdml

import (
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Row-write entry points: the hot VALUES chain's insertRowSh (statement
// shape threaded) and its nil-shape wrapper insertRow (cold callers: the
// DDL/PRAGMA write paths, upsert, REPLACE).
// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).
// fireInsertRowBeforeTriggers fires BEFORE INSERT triggers for a row about to
// be written, re-allocating the rowid when the triggers consumed the
// pre-computed one. Returns a non-nil Result on trigger failure (errRowSkipped
// for RAISE(IGNORE)).
// insertRow inserts one row (the nil-shape entry point: DDL/PRAGMA write
// paths, upsert and REPLACE resolve the shape inside).
func (e *DMLExecutor) insertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	return e.insertRowSh(pg, tableEntry, colDefs, values, fixedRowID, orConflict, nil)
}

// insertRowSh writes one row; sh is the statement's pre-resolved table shape
// (nil resolves it here — the cold paths). The hot VALUES chain threads the
// shape resolved once per statement so the row loop pays no memo
// revalidation per row.
func (e *DMLExecutor) insertRowSh(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string, sh *insertTableShape) *Result {
	// Route FTS virtual table inserts directly to the FTS table. The shape
	// memo's noFTS flag (a pure function of the schema row) skips the two
	// engine map lookups for every plain b-tree row.
	if sh == nil {
		sh = e.insertShapeFor(tableEntry, colDefs)
		if sh == nil {
			// Cold internal writes (ANALYZE's sqlite_stat1 rows) pass no
			// column defs, so the memo — which requires them — declines.
			// Resolve a minimal non-memoized shape: FTS routing still applies
			// (a nil colDefs write to an FTS table must not skip it); the
			// remaining flags mirror the pre-threading cold path, which
			// derived them per call from the same inputs.
			sh = &insertTableShape{
				autoinc:     tableEntry != nil && e.ctx.TableHasAutoIncrement(tableEntry.Name),
				hasTriggers: tableEntry != nil && e.hasTriggersForTable(tableEntry.Name),
			}
			sh.noFTS = tableEntry == nil || !e.isFTSBackedInsert(tableEntry.Name)
		}
	}
	if !sh.noFTS {
		if res := e.insertFTSRow(tableEntry, values, fixedRowID, orConflict); res != nil {
			return res
		}
	}

	// Open a statement journal so a statement-end FOREIGN KEY failure
	// (checked after AFTER triggers, SQLite checks immediate FKs at statement
	// end) can roll back the row, index entries, and any trigger side
	// effects. The journal captures before-images lazily at first page write,
	// so a row that fails before writing anything costs O(1) — the whole-pager
	// snapshot this used to take was O(pages) per row. Skip the journal for
	// the FTS flush's internal shadow-table writes: they are part of the
	// enclosing statement's rollback scope, and the per-block scope churn was
	// measurable across the automerge's many flushes (fts4merge4 2.2.x).
	//
	// P8.PRAGMA (tkt2686): also skip when FK enforcement is OFF — the
	// rollback call site is gated on ForeignKeys(), so the scope would be
	// dead weight for plain inserts in the test's max_page_count loop. Cap
	// enforcement happens at the pager itself (AllocatePage returns nil once
	// numPages exceeds maxPageCount), so transaction-level consistency is
	// preserved by the BEGIN/ROLLBACK pairing without a per-row scope.
	var stmt *pager.StmtJournal
	if !e.ctx.InFTSFlush() && e.ctx.ForeignKeys() {
		stmt = pg.BeginStatement()
		defer pg.EndStatement(stmt)
	}

	nextRowID, res := e.prepareInsertRowValuesSh(tableEntry, colDefs, values, fixedRowID, orConflict, sh)
	if res != nil {
		return res
	}

	// Unwrap collation wrappers (a trigger body may pass a column value
	// wrapped with its collation) so only raw values are stored.
	unwrapCollationWrappers(values)

	_, res = e.writeTableRowSh(pg, tableEntry, colDefs, values, nextRowID, sh)
	if res != nil {
		return res
	}
	// tree is the executor's cached write tree (insertWriteTree) — it is
	// NOT closed here: its lifetime is the executor's (a per-row Close
	// would turn the reuse into a per-row Close+reopen); cursors opened on
	// it are released explicitly by their probes (Cursor.Close).
	// Fire the preupdate hook with the new row's values (fireInsertRowPreupdate
	// skips the event entirely when no hook consumes it).
	if res := e.fireInsertRowPreupdate(pg, tableEntry, nextRowID, values); res != nil {
		return res
	}

	// Maintain indexes: evaluate partial predicates and expression keys in a
	// pure context (a non-deterministic date function raises SQLite's
	// "non-deterministic use of ... in an index" error) and write the new
	// row's index entries. On failure the just-written table row is removed
	// (SQLite rolls the whole statement back).
	if err := e.maintainIndexesOnInsert(tableEntry, colDefs, values, nextRowID); err != nil {
		e.rollbackInsertedRow(pg, tableEntry, nextRowID)
		return &Result{Error: err}
	}

	// Fire AFTER INSERT triggers — but only if triggers exist for this table.
	if res := e.fireAfterInsertRowTriggers(tableEntry, colDefs, values, nextRowID); res != nil {
		return res
	}

	// Enforce FOREIGN KEY constraints at statement end (only when PRAGMA
	// foreign_keys is ON). The check runs after the AFTER triggers so a
	// trigger may repair the violation (e_fkey-31.3). On failure the whole
	// statement is rolled back to the pre-insert statement journal.
	if e.ctx.ForeignKeys() {
		if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, values, 0); res.Error != nil {
			e.ctx.RollbackPagerStatement(pg, stmt)
			e.ctx.InvalidateRowIDCache(pg, tableEntry.RootPage)
			return res
		}
	}
	// Reusable per-row success result (executor scratch — the fields are
	// consumed by insertOneTuple before the next row; see insRowRes). At
	// INSERT nesting depth >= 2 the outer statement's row staging may still
	// be pending, so the nested row builds fresh (see insDepth).
	res = &e.insRowRes
	if e.insDepth > 1 {
		res = &Result{}
	}
	*res = Result{Changes: 1, LastInsertRowID: nextRowID}
	return res
}
