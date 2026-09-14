package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/schema"
)

// DDL glue for the fts5 module: CREATE registration, reopen rehydration and
// DROP teardown (fts5_main.c's xCreate/xConnect/xDestroy lifecycle edges).
// The module instance itself (created by the generic vtab path through
// vtab.Module.Create + SchemaBoundVTab.BindSchema) owns the shadow-table DDL
// and the persistent index; these helpers route the engine's per-table maps.

// fts5Module resolves the registered fts5 module.
func (e *DDLExecutor) fts5Module() (*fts5.Module, bool) {
	m, ok := e.ctx.VTables().Find("fts5")
	if !ok {
		return nil, false
	}
	mod, ok := m.(*fts5.Module)
	return mod, ok
}

// registerFTS5VTab records the module-bound fts5 table in the engine's map so
// INSERT/SELECT/UPDATE/DELETE route to the fts5 machinery. BindSchema has
// already parsed the configuration and created the shadow family; a missing
// instance means the generic create path never bound (defensive).
func (e *DDLExecutor) registerFTS5VTab(tableName string) error {
	mod, ok := e.fts5Module()
	if !ok {
		return fmt.Errorf("no such module: fts5")
	}
	t, ok := mod.GetTable(tableName)
	if !ok {
		return fmt.Errorf("vtable constructor failed: %s", tableName)
	}
	e.ctx.FTS5Tables()[tableName] = t
	return nil
}

// EnsureFTS5ForTable rehydrates a persisted fts5 table on a fresh connection
// (xConnect): the configuration is re-parsed from the stored CREATE VIRTUAL
// TABLE SQL and the index is rebuilt from the shadow tables.
func (e *DDLExecutor) EnsureFTS5ForTable(entry *schema.Entry) {
	// Re-entrancy guard: loadFromShadow's shadow-table reads run through the
	// engine and can trigger a schema reload, which re-dispatches here for
	// the same table while the outer hydration is still in flight. The outer
	// call completes the hydration; nested dispatches are no-ops.
	if fts5EnsureActive {
		return
	}
	if entry == nil || !strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
		return
	}
	if _, ok := e.ctx.FTS5Tables()[entry.Name]; ok {
		return
	}
	modName, args, err := parseVTabSQL(entry.SQL)
	if err != nil || !strings.EqualFold(modName, "fts5") {
		return
	}
	mod, ok := e.fts5Module()
	if !ok {
		return
	}
	fts5EnsureActive = true
	defer func() { fts5EnsureActive = false }()
	// FindTable returns (entry, dbCtx, error): the DATABASE context is the
	// second result. Binding the first result (the schema entry) made
	// ctxName the TABLE name, so a reopened table hydrated against
	// "<table>.<table>_content", loadFromShadow failed "no such table", and
	// the fts5 instance never registered — FROM t('query') on a reopened
	// database then fell through to "'t' is not a function" (fts5connect).
	ctxName := ""
	if _, dbCtx, terr := e.ctx.FindTable(entry.Name); terr == nil && dbCtx != nil {
		ctxName = dbCtx.Name
	}
	t, err := mod.Load(ctxName, entry.Name, args)
	if err != nil {
		return
	}
	e.ctx.FTS5Tables()[entry.Name] = t
}

// dropFTS5State tears down an fts5 table on DROP TABLE: the module forgets
// its instance and the shadow family is removed (fts5DestroyMethod). It
// reports whether the table was an fts5 table.
func (e *DDLExecutor) dropFTS5State(tableName string) bool {
	t, ok := e.ctx.FTS5Tables()[tableName]
	if !ok {
		return false
	}
	delete(e.ctx.FTS5Tables(), tableName)
	if mod, ok := e.fts5Module(); ok {
		mod.DropTable(tableName)
	}
	if t != nil {
		_ = t.Drop()
	}
	return true
}

// renameFTS5 renames an fts5 table and its shadow family (fts5StorageRename)
// and moves the engine map key.
func (e *DDLExecutor) renameFTS5(oldName, newName string) error {
	t, ok := e.ctx.FTS5Tables()[oldName]
	if !ok {
		return nil
	}
	if err := t.Rename(newName); err != nil {
		return err
	}
	e.ctx.FTS5Tables()[newName] = t
	delete(e.ctx.FTS5Tables(), oldName)
	return nil
}

// fts5EnsureActive guards EnsureFTS5ForTable against schema-reload
// re-entrancy from its own shadow-table reads.
var fts5EnsureActive bool
