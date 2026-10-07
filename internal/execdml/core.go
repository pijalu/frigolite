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
	"github.com/pijalu/frigolite/internal/util"
)

// errRaiseIgnore is the sentinel error a RAISE(IGNORE) trigger action returns
// to skip the current row (SQLite's sqlite3TriggerIgnored semantics). It is
// the SAME instance the execexpr evaluator returns, so identity comparisons
// in trigger execution recognize a RAISE(IGNORE) from the trigger body.
var errRaiseIgnore = execexpr.ErrRaiseIgnore

// isSQLiteSequenceName reports whether a table name refers to the
// sqlite_sequence AUTOINCREMENT tracking table (case-insensitive, matching
// SQLite's name resolution). A length screen skips the fold for every other
// name (per-row calls on the DML paths).
func isSQLiteSequenceName(name string) bool {
	return len(name) == 15 && util.EqualFoldASCII(name, "sqlite_sequence")
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

	// affClass* memoize the per-column affinity classes of one colDefs slice
	// under the schema fingerprint (same guard as ciCache): applyColumnAffinities
	// reads the precomputed classes instead of re-classifying each declared
	// type name per row.
	affClassFingerprint uint64
	affClassDefs        *sql.ColumnDef
	affClassLen         int
	affClassCache       []byte

	// ipkIdx* memoizes the INTEGER PRIMARY KEY rowid-alias column index of
	// one colDefs slice (-1 none) under the schema fingerprint (same guard).
	ipkIdxFingerprint uint64
	ipkIdxDefs        *sql.ColumnDef
	ipkIdxLen         int
	ipkIdxCache       int

	// shape* memoizes one table's insert-path shape (insertShapeFor — the
	// WITHOUT ROWID / STRICT / AUTOINCREMENT / trigger / FTS /
	// uniqueness-source probes and the index list) under the schema
	// fingerprint, the same guard pattern as ciCache: SQLite derives the
	// equivalent once at CREATE/prepare time (tabFlags, constraint masks,
	// VDBE index ops); the shape struct is shared and read-only.
	shapeFingerprint uint64
	shapeEntry       *schema.Entry
	shapeDefs        *sql.ColumnDef
	shapeLen         int
	shapeCache       *insertTableShape

	// dbList caches the non-nil, schema-bearing database contexts of
	// e.ctx.Databases() for per-statement walks over all databases
	// (databasesSchemaStamp, validateLoadedTriggers), refreshed when the
	// map's length moves: an ATTACH or DETACH always changes it (a same-length
	// context replacement would need a DETACH and an ATTACH in ONE statement,
	// which the engine never runs), and a lower length forces the rebuild the
	// next statement's ATTACH would need anyway.
	dbList  []*DatabaseContext
	dbListN int

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
	// ptDecode is the collect's pooled decode target (decodePointUpdateRecord):
	// storage.DecodeRecordValuesInto fills it straight from the cell payload,
	// no intermediate Record struct per row.
	ptValues    []interface{}
	ptOldValues []interface{}
	ptDecode    []interface{}

	// laneOps/laneResults are the typed SET fast lane's compile + evaluation
	// scratch (compileTypedPointUpdateSet/applyTypedPointUpdateSet): the
	// assignment list compiles into at most len(laneOps) slot operations and
	// the results stage before the first store, all within one statement —
	// a longer SET list takes the generic row-map path.
	laneOps     [8]setLaneOp
	laneResults [8]interface{}

	// wrFlag* memoizes the point-UPDATE path's WITHOUT ROWID flag
	// (withoutRowidCached) under the schema fingerprint + entry identity —
	// the same guard pattern as ciCache: one resolve per table instead of a
	// CREATE TABLE tail scan at each of the path's 3-4 per-statement gates.
	wrFlagFp    uint64
	wrFlagEntry *schema.Entry
	wrFlagVal   bool

	// resultScratch holds one recycled *Result per execDepth
	// (emptyResultFor): the DML hot paths' no-error control-flow markers
	// ("no conflict", "no match", the 1-change statement result) allocate a
	// fresh Result struct per statement today. The slot resets on acquire
	// (*r = Result{}), so only the current statement's fields survive; a
	// nested statement (trigger body, eval()) takes a deeper slot and can
	// never clobber a result an enclosing statement still reads. Every
	// converted site's result is dead before the next same-depth acquire
	// (each is checked for Error and dropped, or copied out by the statement
	// boundary before the next statement runs).
	resultScratch []*Result

	// seekPlanScratch holds one recycled *dmlSeekPlan per execDepth
	// (seekPlanFor): the point UPDATE/DELETE planner builds one plan struct
	// per statement and consumes it within the statement's collection loop.
	seekPlanScratch []*dmlSeekPlan

	// outerColScratch holds one recycled equality-side name set per execDepth
	// (outerColsFor): planDMLSeek's column-name membership map, cleared and
	// refilled per statement.
	outerColScratch []map[string]bool

	// uniqColsScratch holds one recycled UNIQUE/PK column-index list per
	// execDepth (uniqColsFor): checkUpdateConflicts' per-statement column
	// scan. Only call sites whose list dies before a nested same-depth call
	// may take the slot.
	uniqColsScratch [][]int

	// setColsBuf backs pushUpdateSetColumns's SET-column name list: the
	// outermost push (updateSetColumns == nil) reuses it instead of growing
	// a fresh slice per statement. A nested push (an UPDATE inside a trigger
	// body while an outer UPDATE is active) allocates fresh — the outer
	// statement's list must stay intact until its restore runs.
	setColsBuf []string

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

	// vExpr* memoizes the prepare-time expression-resolution verdict
	// (validateDMLExprsVerdictMemo) per (template-stable statement pointer,
	// schema fingerprint) — the engine pfAST slot's guard pattern: a recycled
	// COW clone address never enters the slot, so a pointer key cannot serve
	// another statement's verdict. Repeated point UPDATE/DELETE statements
	// from the template cache skip the whole WHERE/SET resolution walk.
	vExprStmt   sql.Stmt
	vExprStable bool
	vExprFp     uint64
	vExprErr    error

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
	// insTupleVals is the VALUES-tuple scratch (evalTuplePooled): one
	// allocation ever for the identity-mapped shape, reused per row. Only
	// the no-RETURNING single-VALUES-list INSERT drives it (the RETURNING
	// row set escapes the statement and keeps fresh slices; the preupdate
	// event and the constraint machinery copy what they retain).
	insTupleVals []interface{}

	// insRowRes / insStmtRes are the insert path's reusable success results:
	// insertRow's per-row {Changes:1} and execInsertTuples' per-statement
	// change count (two heap Results per INSERT dominated the insert-phase
	// allocation profile). The scratch is only handed out at INSERT nesting
	// depth 1 (see insDepth): an outer statement's staged Result is still
	// unconsumed while the statement's own side work runs — the fts5
	// statement-end shadow flush, FK actions, sqlite_sequence upkeep and
	// trigger bodies all issue nested INSERTs through this same executor
	// AFTER the outer execInsertTuples has staged its result, and the
	// engine's execTrackChanges reads the fields only once the whole
	// statement has returned. A nested statement (insDepth >= 2) therefore
	// builds its Result fresh instead of overwriting the outer staging
	// (fts5lastrowid 1.1/1.3: an autocommit INSERT into an fts5 table
	// flushed %_data block id 10 mid-statement; reusing the scratch let the
	// outer execTrackChanges publish 10 as last_insert_rowid instead of the
	// fts5 rowid). Every other result shape (errors, RETURNING row sets,
	// upsert outcomes) is built fresh. Each reuse assigns a full composite
	// literal, so no field (including the unexported flags) survives.
	insDepth   int
	insRowRes  Result
	insStmtRes Result

	// insTree is the insert write path's cached b-tree wrapper (see
	// insertWriteTree): one wrapper per (pager, root, kind) identity, closed
	// and replaced on identity change, never re-armed after Close.
	insTree    *btree.BTree
	insTreeKey insTreeKey

	// treeFree recycles the write-tree cache's CLOSED wrappers
	// (btree.TreeFreeList, executor-scoped, statement-funnel goroutine
	// only): an identity change or a layout-hook invalidation closes the
	// old wrapper and returns it here, so the next miss re-arms it via
	// btree.Reinit instead of allocating. Same ownership discipline as the
	// engine's funnel list — single owner, single goroutine, Put only after
	// Close at a provably-dead ownership point.
	treeFree btree.TreeFreeList

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

	// delRes is the point-DELETE path's reusable statement-result slot (the
	// encBuf pooling pattern): every fast-path hit returned a fresh
	// &Result{Changes: 1} per statement, one allocation per delete. The
	// staged value is consumed synchronously — Engine.Exec's funnel reads
	// Error/Changes/LastInsertRowID and the frigolite boundary copies the
	// fields out — and no holder keeps the pointer across the next
	// statement, so one slot serves them all.
	delRes Result

	// ati* is the allTableIndexes one-slot memo (guarded by the
	// cross-database schema stamp): the point-op paths resolve the same
	// table's index list several times per statement, and each resolution
	// walks the databases map. The stamp covers every database because the
	// list aggregates indexes across all of them.
	atiName  string
	atiStamp uint64
	atiDefs  []indexDef

	// dmlStmtSeq/fpSeq/fpCache memoize the schema fingerprint per DML
	// statement (schemaFingerprint): the memo guards read it several times
	// per statement and the schema is frozen mid-statement.
	dmlStmtSeq uint64
	fpSeq      uint64
	fpCache    uint64

	// vltStamp/vltDone memoize the loaded-trigger validation walk
	// (validateLoadedTriggers): with every schema frozen the walk re-finds
	// only already-validated triggers. The stamp folds every database's
	// schema fingerprint (loadedTriggerSchemaStamp) — ATTACH/DETACH move it
	// even though MAIN's own fingerprint stands still.
	vltStamp uint64
	vltDone  bool
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
// funnel, like every other DMLExecutor field. A replaced wrapper goes to
// the executor's TreeFreeList, so the next identity-change miss re-arms it
// (btree.Reinit) instead of allocating.
func (e *DMLExecutor) insertWriteTree(pg *pager.Pager, tableEntry *schema.Entry, withoutRowid bool) *btree.BTree {
	root := e.ctx.RootPagePg(pg, tableEntry.Name, tableEntry.RootPage)
	key := insTreeKey{pg: pg, root: root, isTable: !withoutRowid}
	if t := e.insTree; t != nil && !t.Closed() && e.insTreeKey == key {
		return t
	}
	if e.insTree != nil && !e.insTree.Closed() {
		e.insTree.Close()
		e.treeFree.Put(e.insTree)
	}
	var t *btree.BTree
	if recycled := e.treeFree.Get(); recycled != nil {
		t = recycled.Reinit(pg, root, !withoutRowid)
	} else {
		t = btree.NewBTree(pg, root, !withoutRowid)
	}
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
	// The whole free list is purged, not fed: a layout replacement staled
	// EVERY pooled wrapper's snapshot geometry (btree.TreeFreeList cargo is
	// closed wrappers), whichever pager the change touched. The engine's
	// funnel list is purged by the same hook (Engine.setPagerLayoutHook);
	// the next acquire rebuilds fresh via NewBTree.
	e.treeFree.Purge()
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
		e.treeFree.Put(t)
	}
	var t *btree.BTree
	if recycled := e.treeFree.Get(); recycled != nil {
		t = recycled.Reinit(pg, root, true)
	} else {
		t = btree.NewBTree(pg, root, true)
	}
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

// emptyResultFor returns this depth's recycled no-error Result marker,
// zeroed for a new statement. See resultScratch for the slot/lifetime
// contract: the marker is dead before the next same-depth acquire (checked
// for Error and dropped, or copied out at the statement boundary), and a
// nested DML statement always takes a deeper slot.
func (e *DMLExecutor) emptyResultFor() *Result {
	d := e.ctx.ExecDepth()
	if d >= len(e.resultScratch) {
		e.resultScratch = append(e.resultScratch, make([]*Result, d+1-len(e.resultScratch))...)
	}
	r := e.resultScratch[d]
	if r == nil {
		r = &Result{}
		e.resultScratch[d] = r
		return r
	}
	*r = Result{}
	return r
}

// seekPlanFor returns this depth's recycled point-lookup plan struct
// (planDMLSeek / dmlIndexedSeekPlan / dmlRowidSeekPlan fill it; the caller
// consumes the plan within the statement's collection loop).
func (e *DMLExecutor) seekPlanFor() *dmlSeekPlan {
	d := e.ctx.ExecDepth()
	if d >= len(e.seekPlanScratch) {
		e.seekPlanScratch = append(e.seekPlanScratch, make([]*dmlSeekPlan, d+1-len(e.seekPlanScratch))...)
	}
	p := e.seekPlanScratch[d]
	if p == nil {
		p = &dmlSeekPlan{}
		e.seekPlanScratch[d] = p
		return p
	}
	*p = dmlSeekPlan{}
	return p
}

// outerColsFor returns this depth's recycled equality-side name set,
// cleared for a new statement (planDMLSeek refills it from colDefs).
func (e *DMLExecutor) outerColsFor() map[string]bool {
	d := e.ctx.ExecDepth()
	if d >= len(e.outerColScratch) {
		e.outerColScratch = append(e.outerColScratch, make([]map[string]bool, d+1-len(e.outerColScratch))...)
	}
	m := e.outerColScratch[d]
	if m == nil {
		m = make(map[string]bool, 8)
		e.outerColScratch[d] = m
		return m
	}
	clear(m)
	return m
}

// uniqColsFor is uniqueColsForTable over this depth's recycled index list.
// Only for call sites whose list dies before a nested same-depth call
// (checkUpdateConflicts' list is consumed within the conflict gate).
func (e *DMLExecutor) uniqColsFor(colDefs []sql.ColumnDef) []int {
	d := e.ctx.ExecDepth()
	if d >= len(e.uniqColsScratch) {
		e.uniqColsScratch = append(e.uniqColsScratch, make([][]int, d+1-len(e.uniqColsScratch))...)
	}
	u := e.uniqColsScratch[d]
	u = u[:0]
	for i := range colDefs {
		if colDefs[i].Unique || colDefs[i].PrimaryKey {
			u = append(u, i)
		}
	}
	e.uniqColsScratch[d] = u
	return u
}
