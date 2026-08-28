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
	if entry, _, err := e.findTable(ev.Table); err == nil && entry != nil {
		if cols := entry.Columns; cols != nil {
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
	}
	if e.preupdateHook != nil {
		e.preupdateHook()
	}
	if ev.RowidTable && e.updateHook != nil && !ev.NoUpdateHook {
		e.updateHook(ev.Type, ev.DB, ev.Table, ev.RowID)
	}
	return nil
}

// FireUpdateHook reports a row-level INSERT/UPDATE/DELETE on a ROWID table to
// the connection's sqlite3_update_hook callback.
func (e *Engine) FireUpdateHook(op, db, table string, rowid int64) {
	e.fireUpdateHook(op, db, table, rowid)
}
