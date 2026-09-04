// Package exec implements query execution.
//
// This file holds core DDL execution: CREATE TABLE (with AS SELECT), DROP
// TABLE/VIEW/INDEX, ATTACH/DETACH, auto-index creation, and the generic
// SELECT/expression serializers used by stored objects. It is the
// CREATE/DROP/ATTACH half of the former ddl.go, split out so that each file
// stays within the repository's complexity and size budgets. Trigger, view,
// and virtual-table creation lives in ddl_trigger.go.
package execddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/vtab"
)

func (e *DDLExecutor) echoSourceRows(srcName string) ([][]interface{}, error) {
	srcEntry, _, ferr := e.ctx.FindTable(srcName)
	if ferr != nil {
		return nil, ferr
	}
	tree := e.ctx.TableBTreePg(e.ctx.MainDB().Pager, srcEntry.Name, srcEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil, cerr
	}
	var rows [][]interface{}
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		rows = append(rows, rec.Values)
		okN, nerr := cursor.Next()
		if nerr != nil || !okN {
			break
		}
	}
	return rows, nil
}

// createAutoIndexes creates sqlite_autoindex_* entries for a new table's
// UNIQUE and PRIMARY KEY constraints.
func (e *DDLExecutor) createAutoIndexes(ctx *DatabaseContext, tableName string, s *sql.CreateTableStmt, tableEntry *schema.Entry) *Result {
	uniq := collectUniqueDefs(s)
	if len(uniq) == 0 {
		return &Result{}
	}
	colType := columnTypeLookup(s)
	colPKDesc := columnPKDescLookup(s)
	pkCols := pkConstraintCols(uniq)
	seen := map[string]bool{}
	seq := 0
	for _, u := range uniq {
		if !needsAutoIndex(s, u, pkCols, colType, colPKDesc) {
			continue
		}
		seq++
		key := strings.Join(u.Cols, ",")
		if seen[key] {
			continue // duplicate constraint — no entry, no slot consumed
		}
		seen[key] = true
		// On WITHOUT ROWID, the PK is the table's own key: no separate
		// sqlite_master entry, but the sequence slot is consumed.
		if u.IsPK && s.WithoutRowid {
			continue
		}
		if err := addAutoIndexEntry(ctx, tableName, seq); err != nil {
			return &Result{Error: err}
		}
	}
	return &Result{}
}

// pkConstraintCols returns the column list of the first PRIMARY KEY
// constraint among the unique definitions (used to merge duplicate
// UNIQUE constraints on WITHOUT ROWID tables into the clustered key).
func pkConstraintCols(uniq []uniqDef) []string {
	for _, u := range uniq {
		if u.IsPK {
			return u.Cols
		}
	}
	return nil
}

// needsAutoIndex reports whether a UNIQUE/PK constraint gets its own
// auto-index (and consumes a sqlite_autoindex slot):
//   - An INTEGER PRIMARY KEY (not DESC) is a rowid alias: no autoindex
//     exists at all and no sequence slot is consumed. INTEGER PRIMARY KEY
//     DESC is an ordinary column and DOES get an autoindex.
//   - On WITHOUT ROWID, a UNIQUE constraint duplicating the PRIMARY KEY
//     is merged into the clustered key: no autoindex, no slot consumed.
func needsAutoIndex(s *sql.CreateTableStmt, u uniqDef, pkCols []string, colType func(string) string, colPKDesc func(string) bool) bool {
	if u.IsPK && len(u.Cols) == 1 && execdml.IsIPKRowidAliasCol(sql.ColumnDef{PrimaryKey: true, Type: colType(u.Cols[0]), PKDesc: colPKDesc(u.Cols[0])}) {
		return false
	}
	if s.WithoutRowid && !u.IsPK && sameColumnNames(u.Cols, pkCols) {
		return false
	}
	return true
}

// collectUniqueDefs gathers UNIQUE and PRIMARY KEY constraints (column-level
// and table-level) in creation order.
func collectUniqueDefs(s *sql.CreateTableStmt) []uniqDef {
	var uniq []uniqDef
	// Column-level constraints, in column order. Within a column the UNIQUE
	// is listed before the PRIMARY KEY; when both exist they name the same
	// column, so the ordering only affects which duplicate is skipped.
	for _, cd := range s.Columns {
		if cd.Unique {
			uniq = append(uniq, uniqDef{Cols: []string{cd.Name}})
		}
		if cd.PrimaryKey {
			uniq = append(uniq, uniqDef{Cols: []string{cd.Name}, IsPK: true})
		}
	}
	// Table-level constraints, in list order.
	colIndex := make(map[string]int)
	for i, cd := range s.Columns {
		colIndex[cd.Name] = i
	}
	for _, tc := range s.Constraints {
		if tc.Type != sql.ConstraintUnique && tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		cols := constraintColumnNames(tc, colIndex, s.Columns)
		uniq = append(uniq, uniqDef{Cols: cols, IsPK: tc.Type == sql.ConstraintPrimaryKey})
	}
	return uniq
}

// sameColumnNames reports whether two column lists are identical in order
// (case-insensitively).
func sameColumnNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// columnTypeLookup returns a lookup function for a column's declared type.
func columnTypeLookup(s *sql.CreateTableStmt) func(string) string {
	return func(name string) string {
		for _, cd := range s.Columns {
			if strings.EqualFold(cd.Name, name) {
				return cd.Type
			}
		}
		return ""
	}
}

// columnPKDescLookup returns a lookup function for a column's PK-DESC flag.
func columnPKDescLookup(s *sql.CreateTableStmt) func(string) bool {
	return func(name string) bool {
		for _, cd := range s.Columns {
			if strings.EqualFold(cd.Name, name) {
				return cd.PKDesc
			}
		}
		return false
	}
}

// addAutoIndexEntry adds one sqlite_autoindex_* schema entry. SQLite stores
// these rows with NULL sql, so they are excluded by `SELECT sql FROM
// sqlite_master WHERE sql!=”`. The uniqueness itself is enforced from the
// table's UNIQUE/PRIMARY KEY constraints (compositeUniqueGroups), not from
// this entry's SQL.
func addAutoIndexEntry(ctx *DatabaseContext, tableName string, seq int) error {
	idxName := fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, seq)
	// SQLite allocates a real root page for every autoindex b-tree at
	// CREATE TABLE time (build.c sqlite3PrimaryKeyIndex work), which the
	// database's page count reflects. The uniqueness itself is enforced
	// from the table's constraint list (compositeUniqueGroups), and query
	// paths never scan the autoindex b-tree, but the physical root keeps
	// page counts and rootpage values at parity with SQLite.
	pg, perr := allocateRootPage(ctx.Pager)
	if perr != nil {
		return perr
	}
	initIndexRootPage(pg, ctx.Pager.PageSize())
	if err := ctx.Pager.WritePage(pg); err != nil {
		return err
	}
	idxEntry := &schema.Entry{
		Type:     schema.TypeIndex,
		Name:     idxName,
		TblName:  tableName,
		RootPage: pg.PageNum,
		SQL:      "",
	}
	return ctx.Schema.AddEntry(idxEntry)
}

// authorizeDropTable runs the authorizer checks DROP TABLE performs:
// SQLITE_DROP_TABLE on the table, then SQLITE_DELETE on the table's rows
// and on sqlite_schema (auth-1.63 denies via SQLITE_DELETE sqlite_master;
// auth-1.65 denies via SQLITE_DELETE t2; auth-1.71/1.73 IGNORE them and
// the drop is skipped). SQLITE_IGNORE on SQLITE_DROP_TABLE silently skips
// the drop (auth-1.23.1 returns IGNORE for DROP TABLE and the table
// survives); DENY errors.
func (e *DDLExecutor) authorizeDropTable(s *sql.DropTableStmt) *Result {
	if res := e.authorizeActionOrSkip(auth.ActionDropTable, s.Name, "", "", ""); res != nil {
		return res
	}
	for _, tgt := range []string{s.Name, "sqlite_master"} {
		if res := e.authorizeActionOrSkip(auth.ActionDelete, tgt, "", "", ""); res != nil {
			return res
		}
	}
	return nil
}

// resolveDropTableTarget locates the DROP TABLE target. A non-nil
// *Result is final (an error, or the silent no-op DROP TABLE IF EXISTS
// takes when the name is gone); nil means entry/ctx are valid and the
// drop proceeds.
func (e *DDLExecutor) resolveDropTableTarget(s *sql.DropTableStmt) (*schema.Entry, *DatabaseContext, *Result) {
	entry, ctx, err := e.ctx.FindTable(s.Name)
	if err != nil {
		// The name is not a table. If it is a view, SQLite rejects rather
		// than deleting the wrong object ("use DROP VIEW to delete view v1").
		if _, _, vErr := e.ctx.FindView(s.Name); vErr == nil && !s.IfExists {
			return nil, nil, &Result{Error: fmt.Errorf("use DROP VIEW to delete view %s", s.Name)}
		}
		if s.IfExists {
			return nil, nil, &Result{}
		}
		return nil, nil, &Result{Error: err}
	}
	// SQLite refuses to drop tables whose names begin with "sqlite_"
	// (src/build.c:3471 tableMayNotBeDropped, 3560-3561), except the
	// sqlite_statN analysis tables and sqlite_parameters. Error:
	// "table %s may not be dropped".
	lower := strings.ToLower(entry.Name)
	if strings.HasPrefix(lower, "sqlite_") &&
		!strings.HasPrefix(lower, "sqlite_stat") && lower != "sqlite_parameters" {
		return nil, nil, &Result{Error: fmt.Errorf("table %s may not be dropped", entry.Name)}
	}
	return entry, ctx, nil
}

// execDropTable implements DROP TABLE.
func (e *DDLExecutor) execDropTable(s *sql.DropTableStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if res := e.authorizeDropTable(s); res != nil {
		return res
	}
	// Force a fresh schema read so a stale schema cache cannot make the DROP
	// target a table that is no longer in the btree ("deleted=0").
	for _, dbCtx := range e.ctx.DBList() {
		dbCtx.Schema.InvalidateCache()
	}
	entry, ctx, res := e.resolveDropTableTarget(s)
	if res != nil {
		return res
	}
	if res := e.dropTableFKChecks(entry, ctx); res != nil {
		return res
	}
	// OP_Destroy interlock (src/vdbe.c: db->nVdbeRead > db->nVDestroy+1):
	// destroying a table while another read VM is mid-RUN fails with
	// SQLITE_LOCKED and the table survives (vtabdrop 1.1: DROP inside a
	// db-eval callback leaves rt intact).
	if e.ctx.ActiveReadStatements() > 0 {
		return &Result{Error: fmt.Errorf("database table is locked")}
	}
	// P8.INCRVACUUM (FIX E): collect every b-tree root this DROP frees
	// (each index's root + the table's own) BEFORE any schema removal —
	// the compaction in dropBtreeRoot must still resolve the owner of the
	// current largest root from the schema.
	drops := e.collectBtreeRootDrops(ctx, entry)
	e.dropTableCascade(ctx, entry)
	e.markDropTableFKDirty(entry, ctx)
	// Remove from schema — by TYPE so a TRIGGER named the same as the table
	// survives (SQLite keeps tables and triggers in separate namespaces;
	// DROP TABLE t1 must not drop a trigger named t1).
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeTable); err != nil {
		return &Result{Error: err}
	}
	// P8.INCRVACUUM (FIX E): free each dropped b-tree root through
	// dropBtreeRoot, which implements btree.c btreeDropTable's
	// auto-vacuum root-block compaction. Roots are processed in
	// DESCENDING order — the same order SQLite's codegen emits the
	// OP_Destroy calls (src/build.c: "dropping the btrees in descending
	// order of root-pages"). Without the compaction the root block stays
	// sparse; the commit drain then meets a root page as the file tail
	// (the btree.c:4030 CORRUPT position) and stalls with freelist pages
	// trapped below it, leaving header.count > file pages (autovacuum
	// 3.1: count 10 on an 8-page file → finalDbSize wrap).
	if err := e.applyBtreeRootDrops(ctx, drops); err != nil {
		return &Result{Error: err}
	}
	e.refreshLargestRootPage(ctx)
	if res := e.dropTableCleanup(entry, ctx); res != nil {
		return res
	}
	return &Result{}
}

// btreeRootDrop is one dropped b-tree root pending page reclamation
// (execDropTable's FIX E collection, processed in descending root order).
type btreeRootDrop struct {
	name    string
	root    uint32
	isTable bool
}

// collectBtreeRootDrops snapshots the b-tree roots a DROP TABLE frees —
// every index root on the table plus the table's own root — sorted in
// DESCENDING root order (src/build.c emits OP_Destroy calls in that
// order). Collection happens before any schema removal so the owner
// lookups inside dropBtreeRoot still see the full schema.
func (e *DDLExecutor) collectBtreeRootDrops(ctx *DatabaseContext, entry *schema.Entry) []btreeRootDrop {
	if ctx == nil || ctx.Pager == nil || entry == nil {
		return nil
	}
	var drops []btreeRootDrop
	indexEntries, _ := ctx.Schema.FindIndexesForTable(entry.Name)
	for _, idx := range indexEntries {
		if idx != nil && idx.RootPage != 0 {
			drops = append(drops, btreeRootDrop{name: idx.Name, root: idx.RootPage})
		}
	}
	if entry.RootPage != 0 {
		drops = append(drops, btreeRootDrop{name: entry.Name, root: entry.RootPage, isTable: true})
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].root > drops[j].root })
	return drops
}

// applyBtreeRootDrops frees every collected root in order.
func (e *DDLExecutor) applyBtreeRootDrops(ctx *DatabaseContext, drops []btreeRootDrop) error {
	for _, d := range drops {
		if err := e.dropBtreeRoot(ctx, d.name, d.root, d.isTable); err != nil {
			return err
		}
	}
	return nil
}

// dropBtreeRoot frees one dropped b-tree's pages with btree.c
// btreeDropTable's auto-vacuum root-block compaction (src/btree.c
// 10310-10365): when the dropped root is NOT the largest root, the page
// holding the largest root is moved into the vacated slot (BTree.MoveRoot)
// and the vacated root's old page is freed instead; meta[3] then steps
// down one usable slot (skipping pointer-map and pending-byte pages) in
// both branches. The schema entry whose rootpage was moved (C's *piMoved
// handshake: OP_Destroy returns the moved page and the DDL layer rewrites
// its sqlite_master rootpage) is persisted via UpdateEntryRoot so the
// move survives a reopen. On non-auto-vacuum databases this is a plain
// FreeTable — src/btree.c:10314 gates the whole max-root maintenance on
// pBt->autoVacuum.
func (e *DDLExecutor) dropBtreeRoot(ctx *DatabaseContext, name string, root uint32, isTable bool) error {
	pg := ctx.Pager
	if pg == nil || root == 0 {
		return nil
	}
	bt := e.ctx.TableBTreePg(pg, name, root, isTable)
	if err := bt.FreeTable(root); err != nil {
		return err
	}
	if !pg.AutoVacuum() {
		return nil
	}
	maxRoot := pg.LargestRootPage()
	if maxRoot > 1 && root != maxRoot && maxRoot <= pg.NumPages() {
		if err := e.avRelocateMaxRoot(ctx, pg, bt, root, maxRoot); err != nil {
			return err
		}
	}
	avStepDownLargestRoot(pg, maxRoot)
	return nil
}

// avMaxRootOwner finds the schema entry (table or index) whose rootpage
// equals maxRoot, mirroring destroyRootPage's "UPDATE ..schema SET
// rootpage=%d WHERE rootpage=#r1" match (src/build.c:3296-3299). The
// UPDATE simply matches zero rows when the watermark points at a page
// no root owns, so a false result must never suppress the move itself —
// it only gates the schema rewrite.
func avMaxRootOwner(ctx *DatabaseContext, maxRoot uint32) (string, bool) {
	for _, st := range []schema.SchemaType{schema.TypeTable, schema.TypeIndex} {
		entries, err := ctx.Schema.GetEntries(st)
		if err != nil {
			continue
		}
		for _, en := range entries {
			if en != nil && en.RootPage == maxRoot {
				return en.Name, true
			}
		}
	}
	return "", false
}

// avRelocateMaxRoot implements the btreeDropTable relocation branch
// (src/btree.c:10341-10359): move WHATEVER page currently sits at
// meta[3] into the vacated slot — usually the largest root, but the
// watermark can also point at a free or content page (btreeCreateTable
// only guarantees the top of the root block is dense, not that every
// watermark slot is a root). The vacated slot was freed by FreeTable
// (the C clearTable frees only the content and keeps the root allocated
// for the relocation), so it is popped back off the freelist first,
// giving MoveRoot an allocated destination (allocateBtreePage
// BTALLOC_EXACT parity).
func (e *DDLExecutor) avRelocateMaxRoot(ctx *DatabaseContext, pg *pager.Pager, bt *btree.BTree, root, maxRoot uint32) error {
	ownerName, found := avMaxRootOwner(ctx, maxRoot)
	pg.TakePageFromFreelist(root)
	if err := bt.MoveRoot(maxRoot, root); err != nil {
		return err
	}
	if found {
		// destroyRootPage's "UPDATE ..schema SET rootpage=%d WHERE
		// rootpage=#r1" parity. UpdateEntryRoot is name-keyed and
		// works for index rows too (UpdateRootPagePg only resolves
		// tables — using it here would leave a moved index's
		// schema row pointing at the freed page).
		return ctx.Schema.UpdateEntryRoot(ownerName, root)
	}
	return nil
}

// avStepDownLargestRoot steps meta[3] down one slot after a root-block
// compaction, skipping pointer-map and pending-byte pages
// (src/btree.c:10360-10366); floor at 1 (SQLite keeps meta[3]=1 when no
// roots remain).
func avStepDownLargestRoot(pg *pager.Pager, maxRoot uint32) {
	newMax := int64(maxRoot) - 1
	for newMax > 1 && (storage.IsPtrmapPageNo(uint32(newMax), pg.PageSize()) || uint32(newMax) == pg.PendingBytePage()) {
		newMax--
	}
	if newMax < 1 {
		newMax = 1
	}
	pg.SetLargestRootPage(uint32(newMax))
}

// refreshLargestRootPage recomputes meta[3] (header[52:56], the largest
// root b-tree page number) from the remaining table+index schema entries
// and writes it via pager.SetLargestRootPage. Mirrors btree.c
// btreeDropTable's maxRootPgno-- update (sqlite3BtreeUpdateMeta(p, 4, ...)):
// without it a mass DROP leaves meta[3] above the file's page count after
// AutoVacuumCommit shrinks the file, and the next CREATE fails
// ValidateHeader ("database disk image is malformed", autovacuum-2.5.1).
// SQLite keeps meta[3] at 1 when no tables remain (observed on the oracle:
// drop-all leaves largestRoot=1, and the next CREATE takes rootpage 3).
func (e *DDLExecutor) refreshLargestRootPage(ctx *DatabaseContext) {
	if ctx == nil || ctx.Pager == nil || ctx.Schema == nil {
		return
	}
	// btree.c:10314: the whole max-root-page maintenance lives inside
	// "if( pBt->autoVacuum )". A non-autovacuum database never writes
	// meta[3] (header[52:56]) — btreeDropTable's else branch is just
	// freePage. Writing it unconditionally here flips header[52:56]
	// nonzero in a mode-0 file, and the next Open restores FULL
	// auto-vacuum from that field (btree.c lockBtree:3419 reads the
	// same offset), firing a drain against a file that has no
	// pointer-map pages and corrupting it.
	if !ctx.Pager.AutoVacuum() {
		return
	}
	var largest uint32 = 1
	for _, st := range []schema.SchemaType{schema.TypeTable, schema.TypeIndex} {
		entries, err := ctx.Schema.GetEntries(st)
		if err != nil {
			return
		}
		for _, en := range entries {
			if en == nil || en.RootPage <= 1 {
				continue
			}
			if en.RootPage > largest {
				largest = en.RootPage
			}
		}
	}
	ctx.Pager.SetLargestRootPage(largest)
}

// dropTableFKChecks enforces FOREIGN KEY constraints: DROP TABLE fails if a
// child table references this table's rows and the FK is immediate (SQLite
// "FOREIGN KEY constraint failed"). Deferred FKs are checked at COMMIT.
func (e *DDLExecutor) dropTableFKChecks(entry *schema.Entry, ctx *DatabaseContext) *Result {
	return e.ctx.CheckDropTableFK(entry, ctx)
}

// dropTableCascade drops all triggers and indexes associated with a table
// (SQLite semantics: DROP TABLE removes all associated indexes). It also
// removes the dropped table's rows from sqlite_stat1 (SQLite src/build.c
// sqlite3ClearStatTables), which the ANALYZE step recorded earlier.
func (e *DDLExecutor) dropTableCascade(ctx *DatabaseContext, entry *schema.Entry) {
	triggers, _ := ctx.Schema.FindTriggersForTable(entry.Name)
	for _, t := range triggers {
		_ = ctx.Schema.RemoveEntry(t.Name)
	}
	indexes, _ := ctx.Schema.FindIndexesForTable(entry.Name)
	for _, idx := range indexes {
		// FIX E: the index's b-tree pages are freed by dropBtreeRoot
		// (with auto-vacuum root-block compaction, in descending root
		// order) — not here. Freeing them inline would put the freed
		// pages on the freelist while higher roots stay live, and the
		// commit drain would stall on a root page as the file tail.
		_ = ctx.Schema.RemoveEntry(idx.Name)
	}
	// Remove sqlite_stat1 entries for the dropped table and any of its
	// indexes. SQLite's source does this in sqlite3ClearStatTables inside
	// sqlite3DropTable (src/build.c). Without it, SELECT DISTINCT tbl FROM
	// sqlite_stat1 keeps returning the dropped table's name (analyze-5.4).
	e.ctx.ClearStatsForTable(entry.Name)
}

// markDropTableFKDirty marks child tables dirty so DEFERRED foreign keys are
// re-checked at COMMIT: a child row referencing the dropped parent now has
// no parent (tkt_b1d3a2e: DROP TABLE pp1 with cc2 referencing it fails at
// COMMIT). Done before the schema entry is removed (fkChildRefs needs the
// parent's FK metadata). Mark unconditionally: the DROP itself may orphan
// children even when no prior DML dirtied the child.
func (e *DDLExecutor) markDropTableFKDirty(entry *schema.Entry, ctx *DatabaseContext) {
	e.ctx.MarkDropTableFKDirty(entry, ctx)
}

// dropTableCleanup clears caches and FTS state after a table drop: the
// findTable call above re-populated the table cache with the entry about to
// be dropped; clear it again so a later statement cannot find a stale table
// of the same name. Also drops the rowid/AUTOINCREMENT sequence cache for
// the dropped root page so a recreated table on the same page starts fresh,
// and cleans up any FTS virtual table.
func (e *DDLExecutor) dropTableCleanup(entry *schema.Entry, ctx *DatabaseContext) *Result {
	e.ctx.InvalidateTableCaches()
	e.ctx.ClearRowIDState(ctx.Pager, entry.RootPage)
	e.dropFTSState(ctx, entry.Name)
	e.dropVtabModuleState(ctx, entry)
	return nil
}

// dropFTSState tears down FTS module state and the backing-store shadow
// tables. SQLite's fts3DestroyMethod drops the shadow tables (%_content,
// %_segments, %_segdir, and %_docsize/%_stat for FTS4) when the FTS virtual
// table is dropped, so a recreated FTS table of the same name starts fresh
// (e_fts3 1.1.7/1.1.8 DROP TABLE data then CREATE VIRTUAL TABLE data).
func (e *DDLExecutor) dropFTSState(ctx *DatabaseContext, tableName string) {
	if ftsMod := e.getFTSModuleForTable(tableName); ftsMod != nil {
		ftsMod.DropTable(tableName)
		delete(e.ctx.FTSTables(), tableName)
		e.dropShadowTables(ctx, tableName,
			[]string{"_content", "_segments", "_segdir", "_docsize", "_stat"})
	}
}

// dropVtabModuleState notifies the owning module of a vtab drop: the rtree
// module (rtree.c rtreeDestroy) destroys its three shadow tables
// (<name>_node, <name>_rowid, <name>_parent) and generic modules announce
// the same via vtab.TableDropper (spellfix.c spellfix1Uninit drops
// "%w_vocab" and frees the per-table cost-table state).
func (e *DDLExecutor) dropVtabModuleState(ctx *DatabaseContext, entry *schema.Entry) *Result {
	tableName := entry.Name
	if modName, _, isVtab := parseVTabSQL(entry.SQL); isVtab == nil &&
		(strings.EqualFold(modName, "rtree") || strings.EqualFold(modName, "rtree_i32")) {
		e.dropShadowTables(ctx, tableName, []string{"_node", "_rowid", "_parent"})
	}
	if modName, _, isVtab := parseVTabSQL(entry.SQL); isVtab == nil && modName != "" {
		// unionvtab/swarmvtab hold a cached per-table instance (open swarm
		// source handles + maxopen LRU); disconnect it (unionDisconnect).
		e.ctx.DropUnionVtabInstance(tableName)
		return e.dropViaTableDropper(ctx, modName, tableName)
	}
	return nil
}

// dropViaTableDropper invokes the module's TableDropper when it implements
// one.
func (e *DDLExecutor) dropViaTableDropper(ctx *DatabaseContext, modName, tableName string) *Result {
	r := e.ctx.VTables()
	if r == nil {
		return nil
	}
	m, ok := r.Find(modName)
	if !ok {
		return nil
	}
	if d, ok := m.(vtab.TableDropper); ok {
		if err := d.DropTable(ctx.Name, tableName); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// dropShadowTables removes the named shadow-table schema entries (and their
// btree pages) when their owning virtual table is dropped.
func (e *DDLExecutor) dropShadowTables(ctx *DatabaseContext, tableName string, suffixes []string) {
	for _, suffix := range suffixes {
		name := tableName + suffix
		if ent, _, err := e.ctx.FindTable(name); err == nil && ent != nil {
			_ = ctx.Schema.RemoveEntryOfType(name, schema.TypeTable)
			e.ctx.ClearRowIDState(ctx.Pager, ent.RootPage)
		}
	}
}
