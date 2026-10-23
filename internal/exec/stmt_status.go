package exec

// SQLite's statement-status counters (sqlite3_stmt_status). The testfixture
// reads them through `db status step` / `db status sort` — where7.test's
// count_steps_sort helper appends them to the query result, so the counters
// must be exact and per-statement.
//
// The semantics follow ../sqlite/src/vdbe.c:
//
//   - SQLITE_STMTSTATUS_FULLSCAN_STEP is incremented by OP_Next/OP_Prev of a
//     loop whose p5 is FULLSCAN_STEP (wherecode.c sets that flag for a loop
//     with no constraint on the table) — so it counts the rows *advanced
//     past* in a full table scan (the first row is reached by OP_Rewind and
//     is not counted).
//   - SQLITE_STMTSTATUS_SORT is incremented once per OP_Sort/OP_SorterSort —
//     i.e. once per sorter invocation.
//
// sqlite3_stmt_status(stmt, op, 1) resets the counter when it is read; the
// accessors below do the same.

// RecordFullScanSteps adds n to the SQLITE_STMTSTATUS_FULLSCAN_STEP counter.
func (e *Engine) RecordFullScanSteps(n int) {
	if n > 0 {
		e.fullScanSteps += int64(n)
	}
}

// RecordSortOp counts one sorter invocation (SQLITE_STMTSTATUS_SORT).
func (e *Engine) RecordSortOp() {
	e.sortOps++
}

// StmtStatusFullScanStep returns the number of rows stepped by a full table
// scan in the last statement and resets the counter.
func (e *Engine) StmtStatusFullScanStep() int64 {
	value := e.fullScanSteps
	e.fullScanSteps = 0
	return value
}

// StmtStatusSort returns the number of sorter invocations in the last
// statement and resets the counter.
func (e *Engine) StmtStatusSort() int64 {
	value := e.sortOps
	e.sortOps = 0
	return value
}

// StmtStatusSortPeek returns the number of sorter invocations in the last
// statement WITHOUT resetting the counter. The testfixture's cksort helper
// (collate4.test) reports whether the statement sorted ("sort" or "nosort")
// from the same counter the `db status sort` helper reads.
func (e *Engine) StmtStatusSortPeek() int64 {
	return e.sortOps
}

// ResetStmtStatus clears both counters at the start of a statement (each
// SQLite statement carries its own counters).
func (e *Engine) ResetStmtStatus() {
	e.fullScanSteps = 0
	e.sortOps = 0
}
