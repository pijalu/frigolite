package frigolite

import "github.com/pijalu/frigolite/internal/vtab"

// RegisterVtabModule registers a virtual-table module under the given name
// (sqlite3_create_module parity), making it available to CREATE VIRTUAL
// TABLE ... USING <name> and FROM <name>(...) statements on this connection.
// It exists so native tests can install fixture modules that exercise the
// xBestIndex/xFilter contract (plan/goals/P7.PLANNER.md T27).
func (db *DB) RegisterVtabModule(name string, m vtab.Module) {
	if db != nil && db.engine != nil {
		db.engine.RegisterVtabModule(name, m)
	}
}
