package execddl

import (
	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// DDLContext is the capability interface DDL execution needs from the
// execution engine. The Engine in internal/exec implements it; the
// DDLExecutor depends on this interface rather than on the concrete engine
// type (Dependency Inversion).
//
// The interface covers the engine capabilities DDL shares with the rest of
// the execution engine: schema/table lookup, expression evaluation, SELECT
// delegation (CREATE TABLE AS SELECT), DML delegation (index population,
// sqlite_schema writes), FK enforcement (kept in internal/exec for SOLID-12),
// and the schema/cache state the write path maintains.
type DDLContext interface {
	// Statement execution (trigger bodies run Engine.Exec).
	Exec(stmt sql.Stmt) *Result
	Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) error
	AuthorizeResult(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result

	// Engine resources.
	Databases() map[string]*DatabaseContext
	DBList() []*DatabaseContext
	AppendDBList(ctx *DatabaseContext)
	RemoveDBListIndex(i int)
	ResetDBList()
	GetDB(name string) *DatabaseContext
	MainDB() *DatabaseContext
	Schema() *schema.Manager
	Pager() *pager.Pager
	VTables() *vtab.Registry
	FTSTables() map[string]*fts.FTS3Table
	TextEncoding() string
	CheckProgress() error

	// Settings.
	LegacyAlterTable() bool
	WritableSchema() bool
	ForeignKeys() bool
	InTransaction() bool
	DQSAllowDDL() bool
	DQSAllowDML() bool
	IgnoreCheckConstraints() bool
	ColumnLimit() int
	TrustedSchema() bool
	SchemaFunctionSafe(name string) bool

	// BackupLocked reports whether an active backup has locked the named
	// schema's file (blocking DETACH of that database).
	BackupLocked(name string) bool

	// Schema lookup.
	FindTable(name string) (*schema.Entry, *DatabaseContext, error)
	FindView(name string) (*schema.Entry, *DatabaseContext, error)
	FindIndex(name string) (*schema.Entry, *DatabaseContext, error)
	FindTrigger(name string) (*schema.Entry, *DatabaseContext, error)
	ParseColumnDefs(tableName, createSQL string) []sql.ColumnDef
	TableConstraints(tableName, createSQL string) []sql.TableConstraint
	TableColumnNames(tableName string) ([]string, error)
	CheckCollationString(name string) error
	IsNonModifiableTable(entry *schema.Entry) bool
	IsStoragelessVirtualTable(entry *schema.Entry) bool

	// Expression evaluation (delegates to the execexpr Evaluator).
	EvalExpr(expr sql.Expr, row Row) (interface{}, error)
	EvalBool(expr sql.Expr, row Row) (bool, error)
	EvalConstInt(expr sql.Expr) (int64, error)
	ValidateRowValueUse(expr sql.Expr, topLevel bool) error
	CompareValuesCollate(a, b interface{}, collation string) int

	// SELECT delegation (CREATE TABLE AS SELECT, view validation).
	ExecSelect(s *sql.SelectStmt) *Result
	SubqueryColumnCount(s *sql.SelectStmt) int
	ViewColumnDefsFromSelect(sel *sql.SelectStmt) []sql.ColumnDef
	BuildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap
	BuildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string
	BuildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) []interface{}
	BuildOutputRowWithErr(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) ([]interface{}, error)
	HandleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result
	FinalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result

	// DML delegation (index population, sqlite_schema writes).
	InsertRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) *Result
	CheckConstraintText(createSQL, colName string, check sql.Expr) string

	// Table access.
	TableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TablePager(tableName string) *pager.Pager
	RootPage(tableName string, schemaRoot uint32) uint32
	ReadCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error)
	// ContentRowExists reports whether a content=<table> FTS table's external
	// content table has a row with the given rowid.
	ContentRowExists(tableName string, rowID int64) bool

	// Schema/cache mutation.
	InvalidateTableCaches()
	TrackRootPage(name string, root uint32)
	UpdateRootPage(tableName string, newRoot uint32)
	RootPagePg(pg *pager.Pager, tableName string, schemaRoot uint32) uint32
	UpdateRootPagePg(pg *pager.Pager, tableName string, newRoot uint32)
	ColCache() map[string][]sql.ColumnDef
	TcCache() map[string][]sql.TableConstraint
	DeleteTcCacheTable(name string)
	DeleteTableCache(name string)
	DeleteTableRootPage(name string)
	ResetHasTriggersCache()
	AppendDDLBuffer(fn func())
	ClearRowIDState(pg *pager.Pager, rootPage uint32)
	SetAutoIncSeqFor(pg *pager.Pager, rootPage uint32, seq int64)
	CurrentFTSMatch() string
	SetCurrentFTSMatch(name string)
	SetFTSMatchInfo(table string, hasMatch bool, phrases []fts.MatchPhrase)
	ClearFTSMatchInfo()
	FTSMatchInfo() (string, bool, []fts.MatchPhrase)
	// JoinUpdateFromRows builds the combined row maps for an UPDATE ... FROM's
	// target row (the FTS executor evaluates WHERE/SET against the joined
	// columns — fts4upfrom 1.x: UPDATE ft SET b=o.c FROM ft AS o).
	JoinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error)
	InvalidateRowIDCache(pg *pager.Pager, rootPage uint32)
	BumpRowIDCache(pg *pager.Pager, rootPage uint32, rowID int64)

	// FK enforcement (implemented in internal/exec; SOLID-12 will extract).
	CheckDropTableFK(entry *schema.Entry, ctx *DatabaseContext) *Result
	// HasOpenBlobsOnTable reports whether an incremental-blob handle is open
	// on the table (DROP TABLE fails with "database table is locked").
	HasOpenBlobsOnTable(table string) bool
	// ActiveReadStatements reports how many read statements the harness has
	// marked as mid-run (upstream db->nVdbeRead): DROP TABLE/DROP INDEX
	// while any is active fails with SQLITE_LOCKED "database table is
	// locked" (src/vdbe.c OP_Destroy).
	ActiveReadStatements() int
	MarkDropTableFKDirty(entry *schema.Entry, ctx *DatabaseContext)
	ValidateFKDefinitions(tableName string, colDefs []sql.ColumnDef, createSQL string) error
	// DropUnionVtabInstance disconnects and evicts the cached
	// unionvtab/swarmvtab instance of a dropped virtual table
	// (unionvtab.c unionDisconnect on DROP TABLE); a no-op for other tables.
	DropUnionVtabInstance(tableName string)
	// CacheUnionVtabInstance registers the unionvtab/swarmvtab instance
	// created at CREATE VIRTUAL TABLE time so later statements reuse it
	// (unionvtab.c: the UnionTab, incl. open source handles + LRU state,
	// lives for the table's whole lifetime); a no-op for other modules.
	CacheUnionVtabInstance(tableName string, vt vtab.VirtualTable)
}
