package execdml

import (
	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

// PreupdateEvent holds the current sqlite3_preupdate_hook event: the
// operation type, the database/table names, the rowids, and the old/new
// column values. DML reports each row-level change through
// DMLContext.FirePreupdate; the owning engine stores it and invokes the
// registered hook, which queries count/old/new through the engine accessors.
type PreupdateEvent struct {
	Type   string        // "INSERT", "UPDATE", "DELETE"
	DB     string        // schema name ("main", "aux", ...)
	Table  string        // table name
	RowID  int64         // rowid of the row (0 for WITHOUT ROWID tables)
	RowID2 int64         // second rowid (old rowid on UPDATE)
	Old    []interface{} // old column values (nil for INSERT)
	New    []interface{} // new column values (nil for DELETE)
	// RowidTable reports whether the table is a ROWID table (the
	// sqlite3_update_hook fires only for rowid tables).
	RowidTable bool
	// NoUpdateHook suppresses the sqlite3_update_hook for this event (a
	// REPLACE's internal conflict-delete fires the preupdate hook but NOT the
	// update hook, matching SQLite).
	NoUpdateHook bool
}

// DMLContext is the capability interface DML execution needs from the
// execution engine. The Engine in internal/exec implements it; the
// DMLExecutor depends on this interface rather than on the concrete engine
// type (Dependency Inversion).
//
// The interface covers the engine capabilities DML shares with the rest of
// the execution engine: statement execution for trigger bodies, expression
// evaluation, schema/table lookup, SELECT delegation (INSERT...SELECT,
// RETURNING, subqueries), FK enforcement (kept in internal/exec for
// SOLID-12), trigger state (kept on the Engine for SOLID-13), and the
// rowid/cache state the write path maintains.
type DMLContext interface {
	// Statement execution (trigger bodies run Engine.Exec).
	Exec(stmt sql.Stmt) *Result
	// InFTSFlush reports whether the engine is inside the FTS segment flush
	// (execFlushAutocommit / COMMIT). The flush's internal shadow-table writes
	// are part of the enclosing statement's rollback scope, so per-write
	// pagers snapshots can be skipped (fts4merge4 automerge: without this,
	// every %_segments block insert copies the whole growing pager, O(n^2)).
	InFTSFlush() bool
	Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) error

	// Engine resources.
	Databases() map[string]*DatabaseContext
	GetDB(name string) *DatabaseContext
	Pager() *pager.Pager
	Schema() *schema.Manager
	MainDB() *DatabaseContext
	FTSTables() map[string]*fts.FTS3Table
	LastRowID() int64
	SetLastRowID(id int64)
	LastChanges() int64
	SetLastChanges(v int64)
	CheckProgress() error

	// Schema lookup.
	FindTable(name string) (*schema.Entry, *DatabaseContext, error)
	FindView(name string) (*schema.Entry, *DatabaseContext, error)
	ParseColumnDefs(tableName, createSQL string) []sql.ColumnDef
	TableConstraints(tableName, createSQL string) []sql.TableConstraint
	TableColumnNames(tableName string) ([]string, error)
	CheckCollationString(name string) error

	// Expression evaluation (delegates to the execexpr Evaluator).
	EvalExpr(expr sql.Expr, row Row) (interface{}, error)
	EvalBool(expr sql.Expr, row Row) (bool, error)
	CompareValuesCollate(a, b interface{}, collation string) int
	CompareValuesWithCollate(left, right interface{}) int

	// SELECT delegation (INSERT...SELECT, RETURNING, subquery validation).
	ExecSelect(s *sql.SelectStmt) *Result
	ExecSelectView(viewEntry *schema.Entry) *Result
	BuildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) RowMap
	BuildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef, sel *sql.SelectStmt) []string
	BuildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row Row) []interface{}
	HandleSelectAggregates(s *sql.SelectStmt, rowMaps []RowMap, colDefs []sql.ColumnDef) *Result
	FinalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result
	CompareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int
	RowPassesWhere(where sql.Expr, row Row, cursor *btree.Cursor) (bool, error)
	ValidateDMLSubqueries(stmt sql.Stmt) error
	SelectNeedsRowMaps(s *sql.SelectStmt, tableName string) bool

	// Window-function support for DML: computes a window function's value for
	// every row in rowMaps (UPDATE ... SET expr OVER() over the matched rows).
	ComputeWindowValues(fn *sql.FuncCall, windows []sql.WindowDef, rowMaps []RowMap) ([]interface{}, error)

	// Table access.
	TableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree
	TablePager(tableName string) *pager.Pager
	RootPage(tableName string, schemaRoot uint32) uint32
	RootPagePg(pg *pager.Pager, tableName string, schemaRoot uint32) uint32
	ReadCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error)

	// DML-specific table helpers.
	IsNonModifiableTable(entry *schema.Entry) bool
	IsStoragelessVirtualTable(entry *schema.Entry) bool
	IsVirtualTable(entry *schema.Entry) bool
	LookupCollation(name string) func(a, b string) int
	TableHasAutoIncrement(tableName string) bool
	RandomFreeRowID(tree *btree.BTree) int64
	UpdateRootPage(tableName string, newRoot uint32)
	UpdateRootPagePg(pg *pager.Pager, tableName string, newRoot uint32)
	TrackRootPage(name string, root uint32)
	InvalidateTableCaches()
	RestorePager(pg *pager.Pager, snap *pager.PagerState)
	EchoVTabSource(name string) (string, bool)
	// VTabUpdaterInstance resolves a table name to an updatable virtual-table
	// instance: the eponymous module's implicit instance or a CREATE VIRTUAL
	// TABLE entry's module. ok=false when the name is not a virtual table.
	VTabUpdaterInstance(name string) (vt vtab.VirtualTable, colDefs []sql.ColumnDef, ok bool, err error)
	// DirectOnlyVTab reports whether name resolves to an eponymous module
	// registered SQLITE_VTAB_DIRECTONLY (unsafe inside triggers/views).
	DirectOnlyVTab(name string) bool
	// InTransaction reports whether an explicit BEGIN transaction is open.
	InTransaction() bool
	RewriteEchoInsert(s *sql.InsertStmt, srcName string)
	ExecFTSDelete(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result
	ExecFTSUpdate(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result
	ValidateFTSSegments(tableName string, checkBlocks bool) *Result
	ValidateFTSShadowRoots(tableName string) *Result
	// WriteFTSStat refreshes the FTS4 %_stat doctotal row from the live
	// index (fts3.c fts3UpdateDocTotals runs inside xUpdate, so REPLACE and
	// other xUpdate inserts keep matchinfo's 'n'/'a' current).
	WriteFTSStat(tableName string)
	ValidateAllTableRoots() error
	EstimateFreeSpace() int64
	RebuildFTSIndex(tableName string) *Result
	// ReloadFTSIndex drops an FTS table's in-memory index and reloads it from
	// the persisted %_segdir/%_segments rows. Called after a direct user write
	// to the shadow tables (UPDATE/DELETE/INSERT on %_segdir/%_segments/
	// %_content outside the FTS flush), which makes the in-memory cache stale
	// — SQLite always reads the index from the segments, so a hand-edited
	// segment root must be reflected on the next MATCH/SELECT (fts4record 1.x:
	// UPDATE t1_segdir SET root=<corrupt> then MATCH must fail).
	ReloadFTSIndex(tableName string) *Result
	// JoinUpdateFromRows builds the combined row maps (target row + UPDATE
	// ... FROM tables) for an FTS UPDATE with a FROM clause, so the FTS
	// executor can evaluate WHERE/SET against the joined columns (fts4upfrom
	// 1.x: UPDATE ft SET b=o.c FROM ft AS o WHERE ft.a == ...).
	JoinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error)
	// RunFTSIntegrityCheck verifies an FTS table's in-memory index against its
	// content rows (SQLite's FTS3 integrity-check: INSERT INTO t(t) VALUES
	// ('integrity-check') and PRAGMA integrity_check(t)).
	RunFTSIntegrityCheck(tableName string) *Result
	ValidateFreelistForGrowth() error
	MergeFTS(tableName string, nMerge, nMin int)
	// WriteFTSShadowRow / NextFTSBlockID support the FTS optimize command in
	// the DML layer (the DDLExecutor owns the %_segdir/%_segments writes).
	WriteFTSShadowRow(tableName string, level, idx int, blocks []fts.SegmentBlock, root []byte)
	NextFTSBlockID(tableName string) int
	// ContentRowExists reports whether a content=<table> FTS table's external
	// content table has a row with the given rowid.
	ContentRowExists(tableName string, rowID int64) bool

	// FK enforcement (implemented in internal/exec; SOLID-12 will extract).
	CheckForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64) *Result
	FkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result
	FkParentDeleteReplace(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result

	// Preupdate hook firing: DML reports each row-level INSERT/UPDATE/DELETE
	// to the owning engine, which sets the current preupdate event and invokes
	// the registered sqlite3_preupdate_hook.
	FirePreupdate(ev PreupdateEvent) *Result
	// FireUpdateHook reports a row-level INSERT/UPDATE/DELETE on a ROWID table
	// to the connection's sqlite3_update_hook callback.
	FireUpdateHook(op, db, table string, rowid int64)
	FkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result
	FkCheckReplaceChildren(parentEntry *schema.Entry, parentCtx *DatabaseContext) *Result

	// Trigger state (kept on the Engine; SOLID-13 will extract).
	TriggerDepth() int
	SetTriggerDepth(depth int)
	TriggerDepthLimit() int
	TriggerTables() []string
	SetTriggerTables(tables []string)
	TriggerNewRow() Row
	SetTriggerNewRow(row Row)
	TriggerOldRow() Row
	SetTriggerOldRow(row Row)
	RecursiveTriggers() bool
	ReturningStrict() bool
	ReturningTable() string
	SetReturningStrict(strict bool)
	SetReturningTable(table string)

	// Settings.
	IgnoreCheckConstraints() bool
	ForeignKeys() bool
	ColumnLimit() int
	LengthLimit() int

	// Rowid/cache accessors. The cache maps are keyed by (pager, root page);
	// the accessors abstract the key type so the Engine keeps its cache maps.
	BumpRowIDCache(pg *pager.Pager, rootPage uint32, rowID int64)
	InvalidateRowIDCache(pg *pager.Pager, rootPage uint32)
	NextRowIDFor(pg *pager.Pager, page uint32) (int64, bool)
	SetNextRowIDFor(pg *pager.Pager, page uint32, id int64)
	ResetNextRowIDCache()
	AutoIncSeqFor(pg *pager.Pager, page uint32) (int64, bool)
	SetAutoIncSeqFor(pg *pager.Pager, page uint32, seq int64)
	ResetAutoIncSeq()
	SQLiteSequenceSeqFor(pg *pager.Pager, tableName string) (int64, bool, error)
	WriteSQLiteSequence(pg *pager.Pager, tableName string, seq int64) error

	// Per-table caches.
	CachedTriggerFlag(tableName string) (has bool, ok bool)
	SetCachedTriggerFlag(tableName string, has bool)
	InitValidatedTriggers()
	IsTriggerValidated(key string) bool
	MarkTriggerValidated(key string)
	InitUniqueIdxCache()
	CachedUniqueIdx(tableName string) ([]execquery.UniqueIndexDef, bool)
	SetCachedUniqueIdx(tableName string, defs []execquery.UniqueIndexDef)

	// Scan-table context for DML WHERE/CHECK evaluation (SELECT delegation).
	SetCurrentScanTable(name string)
	CurrentScanTable() string
	PushCTEScope(ctes []sql.CTEDef)
	PopCTEScope()
	FindCTEByName(name string) (sql.CTEDef, bool)
	SchemaFunctionSafe(name string) bool
}
