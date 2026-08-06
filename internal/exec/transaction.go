// Package exec implements query execution.
package exec

import "github.com/pijalu/frigolite/internal/pager"

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	e.inTransaction = false
	e.ddlBuffer = nil
	e.txSnapshots = nil
	if err := e.pager.Flush(); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- BEGIN TRANSACTION ---

func (e *Engine) execBegin() *Result {
	e.inTransaction = true
	e.ddlBuffer = nil
	// Snapshot every attached database's pager so ROLLBACK can undo DML
	// (page-level undo images). COMMIT discards the snapshots.
	e.txSnapshots = make(map[string]*pager.PagerState, len(e.databases))
	for name, ctx := range e.databases {
		e.txSnapshots[name] = ctx.Pager.Snapshot()
	}
	return &Result{}
}

// --- ROLLBACK ---

func (e *Engine) execRollback() *Result {
	e.inTransaction = false
	// Undo all DDL operations that were performed during the transaction
	for i := len(e.ddlBuffer) - 1; i >= 0; i-- {
		e.ddlBuffer[i]()
	}
	e.ddlBuffer = nil
	// Restore page-level state taken at BEGIN to undo DML writes.
	for name, ctx := range e.databases {
		if snap, ok := e.txSnapshots[name]; ok {
			ctx.Pager.Restore(snap)
		}
	}
	e.txSnapshots = nil
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
	return &Result{}
}
