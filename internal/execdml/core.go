// Package execdml implements DML execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
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
	ciFingerprint uint64
	ciDefs        *sql.ColumnDef
	ciLen         int
	ciCache       map[string]int
	ctx           DMLContext

	// delPlan* memoize the point-delete row plan (pointDeleteRowPlan) under
	// the schema fingerprint, the same guard pattern as ciCache above: the
	// plan is immutable after construction (execquery.DMLRowPlan), so one
	// instance serves every point DELETE against the same table layout.
	delPlanFingerprint uint64
	delPlanDefs        *sql.ColumnDef
	delPlanLen         int
	delPlan            *execquery.DMLRowPlan

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

	// encBuf is the DML write path's reusable record-encoding buffer
	// (appendEncodedRecord). The btree write paths copy payload bytes into
	// pages synchronously and never retain the slice, so one buffer serves
	// every statement of this connection.
	encBuf []byte

	// cellBuf is the point-UPDATE write path's reusable table-leaf cell
	// image buffer (appendEncodedCell): like encBuf, its bytes are copied
	// into pages synchronously and never retained by the btree.
	cellBuf []byte

	// ptValues/ptOldValues are the point-UPDATE collect's pooled value-slot
	// pair (pointUpdateValueSlots), fully consumed within one statement.
	ptValues    []interface{}
	ptOldValues []interface{}

	// ptRowMap is the point-UPDATE collect's pooled name-keyed row map
	// (pointUpdateRowMap): cleared and refilled per statement, gated to
	// shapes whose SET evaluation cannot retain the map (no subqueries).
	ptRowMap RowMap

	// andTerms is the reusable WHERE-conjunct scratch (splitAndTermsInto):
	// the seek planner and the point-op gates decompose WHERE clauses per
	// statement, consuming the terms before the next decomposition.
	andTerms []sql.Expr

	// lookupCache memoizes the prepare-time DML column lookup
	// (dmlColumnLookup) under the schema fingerprint, the same guard pattern
	// as ciCache above.
	lookupFingerprint uint64
	lookupDefs        *sql.ColumnDef
	lookupLen         int
	lookupHasRowid    bool
	lookupCache       map[string]bool

	// echoWriteDepth counts in-flight echo write-through statements. A
	// non-zero depth marks every statement error as coming from the source
	// write the echo module's xUpdate performed, so it reports through the
	// module's error prefix (test8.c echoError, "echo-vtab-error: %s").
	echoWriteDepth int

	// indexDefsCache caches per-(database, table) index maintenance-def
	// lists (see allTableIndexes). Entries are validated against the owning
	// schema manager's fingerprint on every lookup, so any DDL rebuilds the
	// affected list airtight — no explicit invalidation hooks.
	indexDefsCache map[indexDefCacheKey]cachedIndexDefs

	// INSERT-path scratch (writeTableRow): the row cell, the on-disk record
	// buffer, and the IPK rowid-alias substitution copy. All three are
	// consumed synchronously inside the row's encode+InsertCell window — the
	// btree copies payload bytes into pages and no trigger can interleave
	// there — so one executor (single-goroutine statement funnel) reuses
	// them across rows/statements without observable aliasing.
	insCell    storage.Cell
	insRecBuf  []byte
	insIPKVals []interface{}

	// insTree is the insert write path's cached b-tree wrapper (see
	// insertWriteTree): one wrapper per (pager, root, kind) identity, closed
	// and replaced on identity change, never re-armed after Close.
	insTree    *btree.BTree
	insTreeKey insTreeKey

	// updTree/delTree are the point-UPDATE and point-DELETE write paths'
	// cached b-tree wrappers — the insertWriteTree pattern extended to the
	// other point-op families (one NewBTree + track + Close per statement
	// dominated the point-op profiles the same way it dominated insert's).
	// Same ownership rules: identity-keyed, closed+replaced on identity
	// change, pager layout hook invalidates, and each statement releases the
	// btree write primitives' leaked seek cursors (ReleaseIdleCursors).
	updTree    *btree.BTree
	updTreeKey insTreeKey
	delTree    *btree.BTree
	delTreeKey insTreeKey

	// ati* is the allTableIndexes one-slot memo (schema fingerprint-guarded):
	// the point-op paths resolve the same table's index list several times
	// per statement, and each resolution walks the databases map.
	atiName string
	atiFP   uint64
	atiDefs []indexDef

	// dmlStmtSeq/fpSeq/fpCache memoize the schema fingerprint per DML
	// statement (schemaFingerprint): the memo guards read it several times
	// per statement and the schema is frozen mid-statement.
	dmlStmtSeq uint64
	fpSeq      uint64
	fpCache    uint64

	// vltFP/vltDone memoize the loaded-trigger validation walk
	// (validateLoadedTriggers): with the schema frozen the walk re-finds
	// only already-validated triggers.
	vltFP   uint64
	vltDone bool
}

// indexDefCacheKey identifies a cached index-maintenance-def list: the owning
// database context plus the lowercased table name.
type indexDefCacheKey struct {
	ctx   *DatabaseContext
	table string
}

// cachedIndexDefs is one allTableIndexes cache entry: the schema fingerprint
// it was built at plus the def list (treated as read-only).
type cachedIndexDefs struct {
	fp   uint64
	defs []indexDef
}

// wrapEchoWriteError applies the echo module's error prefix to a failed
// write-through statement (test8.c echoError sets
// pVtab->zErrMsg = "echo-vtab-error: %s"; the core surfaces zErrMsg
// verbatim, vtab1.12-2). Already-prefixed errors (nested write-through)
// pass through unchanged.
func (e *DMLExecutor) wrapEchoWriteError(err error) error {
	if err == nil || e.echoWriteDepth == 0 {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "echo-vtab-error: ") {
		return err
	}
	return fmt.Errorf("echo-vtab-error: %s", msg)
}

// NewDMLExecutor builds a DML executor over the given context.
func NewDMLExecutor(ctx DMLContext) *DMLExecutor {
	e := &DMLExecutor{ctx: ctx}
	e.insert = InsertExecutor{engine: e}
	e.update = UpdateExecutor{engine: e}
	e.delete = DeleteExecutor{engine: e}
	return e
}

// insertWriteTree builds (or returns the cached) table b-tree a row is
// written through — insert-path glue over the context's root resolution.
//
// A fresh BTree wrapper per ROW dominated the insert-phase allocation
// profile (one NewBTree + initFrom + Close + registry churn per row, since
// even single-row INSERTs are whole statements). The executor therefore
// keeps ONE wrapper for the insert write path and reuses it while the
// (pager, resolved-root, isTable) identity is unchanged: the wrapper is
// stateless over that identity (every operation reads the current pages),
// cursors opened on it are released explicitly (Cursor.Close), and any
// identity change (other table, ATTACH, split-moved root re-resolved on the
// next row) closes it and builds a fresh one — a Close stays terminal, and
// a closed wrapper is never re-armed, only replaced (the btree_pool.go
// contract). Ownership is the executor's: single-goroutine statement
// funnel, like every other DMLExecutor field.
func (e *DMLExecutor) insertWriteTree(pg *pager.Pager, tableEntry *schema.Entry, withoutRowid bool) *btree.BTree {
	root := e.ctx.RootPagePg(pg, tableEntry.Name, tableEntry.RootPage)
	key := insTreeKey{pg: pg, root: root, isTable: !withoutRowid}
	if t := e.insTree; t != nil && !t.Closed() && e.insTreeKey == key {
		return t
	}
	if e.insTree != nil && !e.insTree.Closed() {
		e.insTree.Close()
	}
	t := btree.NewBTree(pg, root, !withoutRowid)
	e.insTree = t
	e.insTreeKey = key
	return t
}

// insertWriteTreeSync re-keys the cached write tree after a split moved the
// root (the wrapper tracks its own new root; the cache key must follow it so
// the next row's resolved-root lookup hits).
func (e *DMLExecutor) insertWriteTreeSync(root uint32) {
	e.insTreeKey.root = root
}

// InvalidateWriteTree drops the cached write trees: each open wrapper is
// closed (terminal, per the btree_pool.go contract) and its cache key
// cleared, so the next writeTree builds a fresh wrapper at the CURRENT pager
// geometry.
//
// The engine fires this from the pager's layout hook: a wrapper snapshots
// pageSize/usableSize at build time (btree initFrom) — state SQLite keeps in
// the file-shared BtShared, where an in-place layout replacement is instantly
// visible to every cursor. When the pager's layout is replaced under a live
// cached wrapper (VACUUM's ResetToEmpty at a pending page size, a backup's
// full-image replace, a materialized reserve change), the stale wrapper would
// keep writing the OLD geometry into the NEW layout — the vacuum-11.x
// copy-back inserts then fail "database disk image is malformed" and the
// rebuild's restore path silently reverts the pending page size.
func (e *DMLExecutor) InvalidateWriteTree() {
	for _, slot := range []*struct {
		tree *btree.BTree
		key  *insTreeKey
	}{
		{e.insTree, &e.insTreeKey},
		{e.updTree, &e.updTreeKey},
		{e.delTree, &e.delTreeKey},
	} {
		if slot.tree != nil && !slot.tree.Closed() {
			slot.tree.Close()
		}
		*slot.key = insTreeKey{}
	}
	e.insTree, e.updTree, e.delTree = nil, nil, nil
}

// pointWriteTree returns the cached slot's b-tree wrapper for a point-op
// write (the insertWriteTree pattern shared by the UPDATE and DELETE point
// paths): one wrapper per (pager, resolved-root, table) identity, closed and
// replaced on identity change. The tree is built OUTSIDE the engine's
// statement-tracking funnel — it outlives the statement, so the point paths
// release its per-statement leaked seek cursors explicitly
// (ReleaseIdleCursors) and re-sync the key after any root move.
func (e *DMLExecutor) pointWriteTree(slot **btree.BTree, key *insTreeKey, pg *pager.Pager, tableName string, rootPage uint32) *btree.BTree {
	root := e.ctx.RootPagePg(pg, tableName, rootPage)
	k := insTreeKey{pg: pg, root: root, isTable: true}
	if t := *slot; t != nil && !t.Closed() && *key == k {
		return t
	}
	if t := *slot; t != nil && !t.Closed() {
		t.Close()
	}
	t := btree.NewBTree(pg, root, true)
	*slot = t
	*key = k
	return t
}

// pointWriteTreeSync re-keys a cached point-op write tree after a split or
// rebalance moved the root, persisting the new root like the insert path's
// persistTreeRootPage (the wrapper tracks its own root; the cache key must
// follow so the next statement's resolved-root lookup hits the same
// wrapper).
func (e *DMLExecutor) pointWriteTreeSync(slot *insTreeKey, pg *pager.Pager, tableName string, rootPage uint32, tree *btree.BTree) {
	e.persistTreeRootPage(pg, tableName, rootPage, tree)
	slot.root = tree.RootPage()
}

// insTreeKey identifies the insert path's cached b-tree wrapper: the owning
// pager, the table's current root, and the tree kind (a rowid table's b-tree
// and a WITHOUT ROWID table's PK-keyed index b-tree are different trees even
// at the same root).
type insTreeKey struct {
	pg      *pager.Pager
	root    uint32
	isTable bool
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
	e.bumpDMLStmtSeq()
	return e.insert.Insert(s)
}

// Update executes an UPDATE statement.
func (e *DMLExecutor) Update(s *sql.UpdateStmt) *Result {
	e.bumpDMLStmtSeq()
	return e.update.Update(s)
}

// Delete executes a DELETE statement.
func (e *DMLExecutor) Delete(s *sql.DeleteStmt) *Result {
	e.bumpDMLStmtSeq()
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
