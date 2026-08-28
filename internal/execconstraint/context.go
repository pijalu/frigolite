// Package execconstraint implements FOREIGN KEY constraint enforcement.
//
// The package owns the FK enforcement family (ON DELETE/ON UPDATE actions,
// deferred FK checks, PRAGMA foreign_key_check scans) that previously lived
// in internal/exec. The ConstraintEnforcer depends on a minimal
// ConstraintContext capability interface (the Engine in internal/exec
// implements it), following the Dependency Inversion pattern used by the
// other sub-executors (execquery, execdml, execddl, execpragma, execexpr).
package execconstraint

import (
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// DatabaseContext aliases the query engine's per-database context so the
// constraint package shares one type with the rest of the execution engine.
type DatabaseContext = execquery.DatabaseContext

// Row provides column value lookup for expression evaluation (alias of the
// query engine's row abstraction).
type Row = execquery.Row

// RowMap is the map-backed Row implementation used by FK row handling.
type RowMap = execquery.RowMap

// Result aliases the query engine's result type.
type Result = execquery.Result

// ConstraintContext is the capability interface FK enforcement needs from the
// execution engine. The Engine in internal/exec implements it; the
// ConstraintEnforcer depends on this interface rather than on the concrete
// engine type (Dependency Inversion).
//
// The interface covers the engine capabilities constraint enforcement shares
// with the rest of the execution engine: schema/table lookup, btree access,
// expression evaluation, rowid-cache maintenance, trigger firing (for
// CASCADE delete), DML context (the table being modified), and the FK-related
// settings (PRAGMA foreign_keys / defer_foreign_keys).
type ConstraintContext interface {
	// Settings.
	ForeignKeys() bool
	DeferForeignKeys() bool

	// Schema lookup.
	Databases() map[string]*DatabaseContext
	DBList() []*DatabaseContext
	Schema() *schema.Manager
	FindTable(name string) (*schema.Entry, *DatabaseContext, error)
	GetDB(name string) *DatabaseContext
	ParseColumnDefs(tableName, createSQL string) []sql.ColumnDef
	TableConstraints(tableName, createSQL string) []sql.TableConstraint

	// Table access.
	TablePager(tableName string) *pager.Pager
	TableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree

	// Rowid cache maintenance (CASCADE/SET NULL/SET DEFAULT rewrite rows).
	InvalidateRowIDCache(pg *pager.Pager, rootPage uint32)
	BumpRowIDCache(pg *pager.Pager, rootPage uint32, rowID int64)

	// Expression evaluation (SET DEFAULT column defaults).
	EvalExpr(expr sql.Expr, row Row) (interface{}, error)
	CompareValuesCollate(a, b interface{}, collation string) int

	// Trigger firing (CASCADE delete fires BEFORE/AFTER DELETE triggers).
	HasTriggersForTable(tableName string) bool
	FireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result
	FireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result

	// DML context: the database context of the table being modified.
	CurrentDMLCtx() *DatabaseContext

	// BumpTotalChanges adds n to the connection's total-changes counter
	// (sqlite3_total_changes). FK actions (CASCADE/SET NULL/SET DEFAULT)
	// modify rows directly and must report them.
	BumpTotalChanges(n int64)
}

// ConstraintEnforcer owns FOREIGN KEY enforcement state and methods. It
// replaces the FK methods that previously lived on the Engine; the Engine
// delegates FK entry points here.
type ConstraintEnforcer struct {
	ctx ConstraintContext

	// fkDirty tracks tables modified during the current transaction/statement
	// whose FK relationships must be re-validated at COMMIT (deferred FK checks
	// only inspect affected tables, mirroring SQLite's incremental counters).
	fkDirty map[fkDirtyKey]bool

	// fkParentDirty tracks tables whose PARENT rows changed (UPDATE/DELETE),
	// so their children's FK references are re-validated at COMMIT. INSERTing
	// a parent row cannot orphan children, so it is not recorded here.
	fkParentDirty map[fkDirtyKey]bool
}

// New creates a ConstraintEnforcer bound to a ConstraintContext.
func New(ctx ConstraintContext) *ConstraintEnforcer {
	return &ConstraintEnforcer{ctx: ctx}
}
