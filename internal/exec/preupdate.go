package exec

import (
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/value"
)

// SetPreupdateHook registers the connection's preupdate hook
// (sqlite3_preupdate_hook). A nil callback clears it. The callback runs
// after every row-level INSERT/UPDATE/DELETE with the current event available
// via PreupdateCount/PreupdateOld/PreupdateNew.
func (e *Engine) SetPreupdateHook(fn func()) {
	e.preupdateHook = fn
	e.preupdate = execdml.PreupdateEvent{}
}

// PreupdateCount returns the number of columns in the current preupdate event
// (sqlite3_preupdate_count).
func (e *Engine) PreupdateCount() int {
	n := len(e.preupdate.Old)
	if len(e.preupdate.New) > n {
		n = len(e.preupdate.New)
	}
	return n
}

// PreupdateType returns the operation type of the current preupdate event
// ("INSERT", "UPDATE", "DELETE").
func (e *Engine) PreupdateType() string {
	return e.preupdate.Type
}

// PreupdateDB returns the schema name of the current preupdate event.
func (e *Engine) PreupdateDB() string {
	return e.preupdate.DB
}

// PreupdateTable returns the table name of the current preupdate event.
func (e *Engine) PreupdateTable() string {
	return e.preupdate.Table
}

// PreupdateRowID returns the first rowid of the current preupdate event.
func (e *Engine) PreupdateRowID() int64 {
	return e.preupdate.RowID
}

// PreupdateRowID2 returns the second rowid of the current preupdate event.
func (e *Engine) PreupdateRowID2() int64 {
	return e.preupdate.RowID2
}

// PreupdateOld returns the old value of column i in the current preupdate
// event (sqlite3_preupdate_old). Index out of range returns nil.
func (e *Engine) PreupdateOld(i int) interface{} {
	if i < 0 || i >= len(e.preupdate.Old) {
		return nil
	}
	return e.preupdate.Old[i]
}

// PreupdateNew returns the new value of column i in the current preupdate
// event (sqlite3_preupdate_new). Index out of range returns nil.
func (e *Engine) PreupdateNew(i int) interface{} {
	if i < 0 || i >= len(e.preupdate.New) {
		return nil
	}
	return e.preupdate.New[i]
}

// PreupdateNeeded implements execdml.DMLContext.PreupdateNeeded: reports
// whether any hook consumes preupdate events — the sqlite3_preupdate_hook
// or the sqlite3_update_hook is registered. The DML executor hoists this
// check above its per-row event build (the old/new value copies), so an
// un-hooked connection's DML never materializes the event.
func (e *Engine) PreupdateNeeded() bool {
	return e.preupdateHook != nil || e.updateHook != nil
}

// FirePreupdate sets the current preupdate event and invokes the registered
// hook (if any). The event state stays valid until the next DML row write, so
// the hook can query count/old/new. For ROWID tables the sqlite3_update_hook
// also fires (with the operation, db, table, and rowid). Returns nil (the
// hooks cannot fail a statement in SQLite).
func (e *Engine) FirePreupdate(ev execdml.PreupdateEvent) *Result {
	e.preupdate = ev
	// sqlite3_preupdate_old/new report values with the column's affinity
	// applied (vdbeaux.c sqlite3VdbePreUpdateHook reads the record and lets
	// the affinity transform integral INTEGERs back to REAL for REAL
	// columns, e.g. bind2.test's IntReal round-trip). Apply the table's
	// declared affinities here so every consumer sees faithful values.
	e.applyPreupdateAffinity()
	if e.preupdateHook != nil {
		e.preupdateHook()
	}
	if ev.RowidTable && e.updateHook != nil && !ev.NoUpdateHook {
		e.updateHook(ev.Type, ev.DB, ev.Table, ev.RowID)
	}
	return nil
}

// applyPreupdateAffinity rewrites the current preupdate event's old/new
// values with the target table's declared column affinities so every
// consumer sees faithful values. The table resolution is memoized per engine
// (preAff* fields): a multi-row DML statement fires this per row for the same
// table, and the per-row findTable (external-mod probe + trigger-scope +
// table-cache lookup) dominated the point DML floor. The memo key is the
// folded schema fingerprint of every attached database plus the table name —
// any DDL (local cookie/mutation epoch or external cache drop) moves a
// fingerprint and rebuilds the entry, the same contract the execquery
// seekColIndexFor and execdml columnIndexFor memos rely on. entry.Columns is
// immutable once cached (schema entries are replaced, never edited), so the
// memo cannot serve mutated column state.
func (e *Engine) applyPreupdateAffinity() {
	name := e.preupdate.Table
	if e.preAffEntry == nil || e.preAffName != name || e.preAffFingerprint != e.allSchemasFingerprint() {
		entry, _, err := e.findTable(name)
		if err != nil || entry == nil {
			// Resolution failed: leave the event values untouched and drop
			// the memo so a later statement re-resolves.
			e.preAffEntry = nil
			return
		}
		e.preAffName, e.preAffFingerprint, e.preAffEntry = name, e.allSchemasFingerprint(), entry
	}
	cols := e.preAffEntry.Columns
	if cols == nil {
		return
	}
	for i := range e.preupdate.Old {
		if i < len(cols) {
			e.preupdate.Old[i] = value.ApplyColumnAffinity(e.preupdate.Old[i], cols[i].Type)
		}
	}
	for i := range e.preupdate.New {
		if i < len(cols) {
			e.preupdate.New[i] = value.ApplyColumnAffinity(e.preupdate.New[i], cols[i].Type)
		}
	}
}

// allSchemasFingerprint folds the schema fingerprint of every attached
// database (main included) into one key, so a change in ANY schema — the
// place findTable may resolve a name from — invalidates engine-side
// name→entry memos.
func (e *Engine) allSchemasFingerprint() uint64 {
	fp := uint64(0)
	for _, ctx := range e.dbList {
		if ctx != nil && ctx.Schema != nil {
			fp = fp*0x9E3779B97F4A7C15 ^ ctx.Schema.SchemaFingerprint()
		}
	}
	return fp
}

// FireUpdateHook reports a row-level INSERT/UPDATE/DELETE on a ROWID table to
// the connection's sqlite3_update_hook callback.
func (e *Engine) FireUpdateHook(op, db, table string, rowid int64) {
	e.fireUpdateHook(op, db, table, rowid)
}
