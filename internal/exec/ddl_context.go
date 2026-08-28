package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execddl"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// This file implements the execddl.DDLContext capability interface on the
// Engine. The DDL executor (internal/execddl) depends on this interface
// rather than on the concrete Engine type (Dependency Inversion).

// Compile-time probe: Engine implements execddl.DDLContext.
var _ execddl.DDLContext = (*Engine)(nil)

// DBList returns the attached databases in ATTACH order (main first).
func (e *Engine) DBList() []*DatabaseContext {
	return e.dbList
}

// AppendDBList appends a database context to the attach-order list.
func (e *Engine) AppendDBList(ctx *DatabaseContext) {
	e.dbList = append(e.dbList, ctx)
}

// RemoveDBListIndex removes the database context at index i from the
// attach-order list.
func (e *Engine) RemoveDBListIndex(i int) {
	e.dbList = append(e.dbList[:i], e.dbList[i+1:]...)
}

// ResetDBList resets the attach-order list to contain only main.
func (e *Engine) ResetDBList() {
	e.dbList = []*DatabaseContext{e.mainDB}
}

// DQSAllowDDL reports whether double-quoted strings are allowed in DDL.
func (e *Engine) DQSAllowDDL() bool {
	return e.settings.dqsDDL || (e.settings.writableSchema && e.settings.dqsDML)
}

// DQSAllowDML reports whether double-quoted strings are allowed in DML.
func (e *Engine) DQSAllowDML() bool {
	return e.settings.dqsDML
}

// FindIndex returns the schema entry and database context for an index.
func (e *Engine) FindIndex(name string) (*schema.Entry, *DatabaseContext, error) {
	return e.findIndex(name)
}

// FindTrigger returns the schema entry and database context for a trigger.
func (e *Engine) FindTrigger(name string) (*schema.Entry, *DatabaseContext, error) {
	return e.findTrigger(name)
}

// EvalConstInt evaluates a constant integer expression.
func (e *Engine) EvalConstInt(expr sql.Expr) (int64, error) {
	return e.dml.EvalConstInt(expr)
}

// ValidateRowValueUse validates a row-value expression usage.
func (e *Engine) ValidateRowValueUse(expr sql.Expr, topLevel bool) error {
	return e.validateRowValueUse(expr, topLevel)
}

// InsertRow inserts one row into a table.
func (e *Engine) InsertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result {
	return e.dml.InsertRow(pg, tableEntry, colDefs, values, fixedRowID, orConflict)
}

// CheckConstraintText formats a column-level CHECK constraint's SQL text.
func (e *Engine) CheckConstraintText(createSQL, colName string, check sql.Expr) string {
	return e.dml.CheckConstraintText(createSQL, colName, check)
}

// ColCache returns the engine's column-definition cache map.
func (e *Engine) ColCache() map[string][]sql.ColumnDef {
	return e.caches.colCache
}

// TcCache returns the engine's table-constraint cache map.
func (e *Engine) TcCache() map[string][]sql.TableConstraint {
	return e.caches.tcCache
}

// DeleteTcCacheTable removes every cached table-constraint entry for a table
// (keys are table name + SQL; a DDL rewrite changes the SQL).
func (e *Engine) DeleteTcCacheTable(name string) {
	prefix := name + "\x00"
	for k := range e.caches.tcCache {
		if strings.HasPrefix(k, prefix) {
			delete(e.caches.tcCache, k)
		}
	}
}

// DeleteTableCache removes a table from the table-entry cache.
func (e *Engine) DeleteTableCache(name string) {
	delete(e.caches.tableCache, name)
}

// DeleteTableRootPage removes every tracked root page for the given name
// across all pagers (the name may exist in several attached databases).
func (e *Engine) DeleteTableRootPage(name string) {
	for k := range e.caches.tableRootPages {
		if k.name == name {
			delete(e.caches.tableRootPages, k)
		}
	}
}

// ResetHasTriggersCache clears the cached trigger-existence flags.
func (e *Engine) ResetHasTriggersCache() {
	e.triggers.ResetHasTriggersCache()
}

// AppendDDLBuffer appends a DDL undo operation for transaction rollback.
func (e *Engine) AppendDDLBuffer(fn func()) {
	e.tx.ddlBuffer = append(e.tx.ddlBuffer, fn)
}

// ClearRowIDState drops the rowid and AUTOINCREMENT sequence cache entries
// for a (pager, root page).
func (e *Engine) ClearRowIDState(pg *pager.Pager, rootPage uint32) {
	key := e.rowidCacheKey(pg, rootPage)
	delete(e.caches.nextRowIDCache, key)
	delete(e.caches.autoIncSeq, key)

}

// SetCurrentFTSMatch sets the current FTS table for MATCH evaluation.
func (e *Engine) SetCurrentFTSMatch(name string) {
	e.currentFTSMatch = name
}

// CheckDropTableFK enforces FOREIGN KEY constraints when dropping a table:
// DROP TABLE fails if a child table references this table's rows and the FK
// is immediate (SQLite "FOREIGN KEY constraint failed"). Deferred FKs are
// checked at COMMIT.
func (e *Engine) CheckDropTableFK(entry *schema.Entry, ctx *DatabaseContext) *Result {
	if !e.settings.foreignKeys {
		return nil
	}
	colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
	tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if res := e.constraints.FkParentDropTable(entry, colDefs, row); res.Error != nil {
			return res
		}
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return nil
}

// ValidateFKDefinitions validates FOREIGN KEY definitions at CREATE TABLE
// time (child columns exist, cardinality matches), delegating to the
// constraint enforcer. The check runs regardless of PRAGMA foreign_keys and
// does not validate the parent table (SQLite R-36018-21755).
func (e *Engine) ValidateFKDefinitions(tableName string, colDefs []sql.ColumnDef, createSQL string) error {
	return e.constraints.ValidateFKDefinitions(tableName, colDefs, createSQL)
}

// MarkDropTableFKDirty marks child tables dirty so DEFERRED foreign keys are
// re-checked at COMMIT: a child row referencing the dropped parent now has
// no parent (tkt_b1d3a2e: DROP TABLE pp1 with cc2 referencing it fails at
// COMMIT). Done before the schema entry is removed (fkChildRefs needs the
// parent's FK metadata). Mark unconditionally: the DROP itself may orphan
// children even when no prior DML dirtied the child.
func (e *Engine) MarkDropTableFKDirty(entry *schema.Entry, ctx *DatabaseContext) {
	if !e.settings.foreignKeys {
		return
	}
	for _, ref := range e.constraints.ChildRefs(entry, ctx) {
		if ref.ChildCtx != nil && ref.ChildTable != "" {
			if childEntry, cerr := ref.ChildCtx.Schema.FindTable(ref.ChildTable); cerr == nil {
				e.constraints.MarkFKDirty(childEntry, ref.ChildCtx)
			}
		}
	}
}
