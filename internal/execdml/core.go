// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/sql"
)

// errRaiseIgnore is the sentinel error a RAISE(IGNORE) trigger action returns
// to skip the current row (SQLite's sqlite3TriggerIgnored semantics). It is
// the SAME instance the execexpr evaluator returns, so identity comparisons
// in trigger execution recognize a RAISE(IGNORE) from the trigger body.
var errRaiseIgnore = execexpr.ErrRaiseIgnore

// isSQLiteSequenceName reports whether a table name refers to the
// sqlite_sequence AUTOINCREMENT tracking table (case-insensitive, matching
// SQLite's name resolution).
func isSQLiteSequenceName(name string) bool {
	return strings.EqualFold(name, "sqlite_sequence")
}

// DMLExecutor executes INSERT/UPDATE/DELETE statements. It composes the three
// statement-family executors and owns the DML state fields that the Engine
// previously carried on itself (currentDMLTable, currentDMLCtx,
// updateSetColumns).
type DMLExecutor struct {
	ctx DMLContext

	// Statement-family executors composing this engine. They share this
	// DMLExecutor so inter-statement calls resolve through promoted methods.
	insert InsertExecutor
	update UpdateExecutor
	delete DeleteExecutor

	// DML state extracted from Engine (SOLID-09..10): the table being
	// INSERTed/UPDATEd (for qualified refs in CHECK/defaults), the database
	// context of the table being modified (trigger scoping), and the column
	// names in the current UPDATE's SET clause.
	currentDMLTable  string
	currentDMLCtx    *DatabaseContext
	updateSetColumns []string

	// currentTriggerCtx is the owning database of the trigger currently
	// executing (set by fireTrigger). TEMP triggers may reference tables in
	// any database; non-temp triggers resolve body references only in their
	// owning schema.
	currentTriggerCtx *DatabaseContext

	// txnWrittenFiles tracks the database FILES written during the current
	// transaction (resolved path → schema name). A write to a file already
	// written through a DIFFERENT attached schema raises "database is
	// locked" — SQLite cannot hold two write locks on one file
	// (attach-9.2: one file ATTACHed as aux1 and aux2).
	txnWrittenFiles map[string]string

	// lastFTSDocRowID is the last DOCUMENT rowid inserted into an FTS table
	// by the insert-select path, kept separate from ctx.LastRowID because
	// the statement's internal shadow-table writes clobber the connection
	// counter (see lastInsertedFTSRowID).
	lastFTSDocRowID int64
}

// NewDMLExecutor builds a DML executor over the given context.
func NewDMLExecutor(ctx DMLContext) *DMLExecutor {
	e := &DMLExecutor{ctx: ctx}
	e.insert = InsertExecutor{engine: e}
	e.update = UpdateExecutor{engine: e}
	e.delete = DeleteExecutor{engine: e}
	return e
}

// schemaNameForPager returns the schema name ("main", "aux", ...) whose
// database context uses the given pager. Used for preupdate-hook reporting.
func (e *DMLExecutor) schemaNameForPager(pg *pager.Pager) string {
	for _, ctx := range e.ctx.Databases() {
		if ctx.Pager == pg {
			if ctx.Name != "" {
				return ctx.Name
			}
			return "main"
		}
	}
	return "main"
}

// Insert executes an INSERT statement.
func (e *DMLExecutor) Insert(s *sql.InsertStmt) *Result {
	return e.insert.Insert(s)
}

// Update executes an UPDATE statement.
func (e *DMLExecutor) Update(s *sql.UpdateStmt) *Result {
	return e.update.Update(s)
}

// Delete executes a DELETE statement.
func (e *DMLExecutor) Delete(s *sql.DeleteStmt) *Result {
	return e.delete.Delete(s)
}

// CurrentDMLTable returns the table currently being INSERTed/UPDATEd (for
// qualified refs in CHECK/defaults).
func (e *DMLExecutor) CurrentDMLTable() string {
	return e.currentDMLTable
}

// SetCurrentDMLTable sets the table currently being modified.
func (e *DMLExecutor) SetCurrentDMLTable(name string) {
	e.currentDMLTable = name
}

// CurrentDMLCtx returns the database context of the table being modified
// (trigger scoping).
func (e *DMLExecutor) CurrentDMLCtx() *DatabaseContext {
	return e.currentDMLCtx
}

// SetCurrentDMLCtx sets the database context of the table being modified.
func (e *DMLExecutor) SetCurrentDMLCtx(ctx *DatabaseContext) {
	e.currentDMLCtx = ctx
}

// SetCurrentTriggerCtx records the owning database of the trigger currently
// executing (nil outside trigger bodies).
func (e *DMLExecutor) SetCurrentTriggerCtx(ctx *DatabaseContext) {
	e.currentTriggerCtx = ctx
}

// CheckSameFileWriteConflict raises "database is locked" when the target
// schema's file was already written through a DIFFERENT attached schema in
// the same transaction, and records the write otherwise (attach-9.2).
func (e *DMLExecutor) CheckSameFileWriteConflict(ctx *DatabaseContext) error {
	if ctx == nil || ctx.IsMemory || ctx.FilePath == "" {
		return nil
	}
	if e.txnWrittenFiles == nil {
		e.txnWrittenFiles = make(map[string]string)
	}
	if owner, ok := e.txnWrittenFiles[ctx.FilePath]; ok && owner != ctx.Name {
		return fmt.Errorf("database is locked")
	}
	e.txnWrittenFiles[ctx.FilePath] = ctx.Name
	return nil
}

// CheckSameFileWriteConflictRes is CheckSameFileWriteConflict returning a
// *Result for the statement executors.
func (e *DMLExecutor) CheckSameFileWriteConflictRes(ctx *DatabaseContext) *Result {
	if err := e.CheckSameFileWriteConflict(ctx); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// ClearTxnWrittenFiles resets the same-file write tracker at transaction
// boundaries (COMMIT/ROLLBACK) and after each autocommit statement.
func (e *DMLExecutor) ClearTxnWrittenFiles() {
	e.txnWrittenFiles = nil
}

// TxnWrittenFiles returns the registry lock keys of the database FILES
// written during the current transaction (one key per file). Used by the
// COMMIT upgrade gate, which only upgrades files holding RESERVED.
func (e *DMLExecutor) TxnWrittenFiles() []string {
	if len(e.txnWrittenFiles) == 0 {
		return nil
	}
	keys := make([]string, 0, len(e.txnWrittenFiles))
	for path := range e.txnWrittenFiles {
		keys = append(keys, path)
	}
	return keys
}

// CurrentTriggerCtx returns the owning database of the trigger currently
// executing, or nil when no trigger body is running.
func (e *DMLExecutor) CurrentTriggerCtx() *DatabaseContext {
	return e.currentTriggerCtx
}

// UpdateSetColumns returns the column names in the current UPDATE's SET
// clause.
func (e *DMLExecutor) UpdateSetColumns() []string {
	return e.updateSetColumns
}

// SetUpdateSetColumns sets the column names in the current UPDATE's SET
// clause.
func (e *DMLExecutor) SetUpdateSetColumns(cols []string) {
	e.updateSetColumns = cols
}

// InsertExecutor executes INSERT statements (SOLID-09). It is composed by
// the DMLExecutor and exposes the INSERT entry point.
type InsertExecutor struct {
	engine *DMLExecutor
}

// UpdateExecutor executes UPDATE statements (SOLID-10). It is composed by
// the DMLExecutor and exposes the UPDATE entry point.
type UpdateExecutor struct {
	engine *DMLExecutor
}

// DeleteExecutor executes DELETE statements. It is composed by the
// DMLExecutor and exposes the DELETE entry point.
type DeleteExecutor struct {
	engine *DMLExecutor
}

// Insert executes an INSERT statement.
func (x *InsertExecutor) Insert(s *sql.InsertStmt) *Result {
	return x.engine.execInsert(s)
}

// Update executes an UPDATE statement.
func (x *UpdateExecutor) Update(s *sql.UpdateStmt) *Result {
	return x.engine.execUpdate(s)
}

// Delete executes a DELETE statement.
func (x *DeleteExecutor) Delete(s *sql.DeleteStmt) *Result {
	return x.engine.execDelete(s)
}

// Compile-time probes: DMLExecutor implements the statement-family entry
// points and the sub-executors expose their concern's public surface (LSP).
var (
	_ insertExecutor = (*DMLExecutor)(nil)
	_ updateExecutor = (*DMLExecutor)(nil)
	_ deleteExecutor = (*DMLExecutor)(nil)
)

// insertExecutor is the INSERT-execution capability (SOLID-09).
type insertExecutor interface {
	execInsert(s *sql.InsertStmt) *Result
}

// updateExecutor is the UPDATE-execution capability (SOLID-10).
type updateExecutor interface {
	execUpdate(s *sql.UpdateStmt) *Result
}

// deleteExecutor is the DELETE-execution capability.
type deleteExecutor interface {
	execDelete(s *sql.DeleteStmt) *Result
}

// Row aliases the shared row abstraction.
type Row = execquery.Row

// RowMap aliases the map-backed row abstraction.
type RowMap = execquery.RowMap

// Result aliases the shared statement result type.
type Result = execquery.Result

// DatabaseContext aliases the shared per-database state type.
type DatabaseContext = execquery.DatabaseContext

// uniqueIndexDef aliases the query engine's UNIQUE index description.
type uniqueIndexDef = execquery.UniqueIndexDef

// orConstraint aliases one constant equality inside an OR-index plan branch.
type orConstraint = execquery.OrConstraint

// orBranchPlan aliases one OR term of an OR-index plan.
type orBranchPlan = execquery.OrBranchPlan

// collatedValue aliases the collation-wrapping value type.
type collatedValue = execquery.CollatedValue
