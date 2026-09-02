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
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execdml"
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
	// On WITHOUT ROWID tables the PRIMARY KEY is the table's own clustered
	// key; SQLite merges any UNIQUE constraint with the same column list into
	// it, creating no separate autoindex (and consuming no slot).
	var pkCols []string
	for _, u := range uniq {
		if u.IsPK {
			pkCols = u.Cols
			break
		}
	}
	seen := map[string]bool{}
	seq := 0
	for _, u := range uniq {
		key := strings.Join(u.Cols, ",")
		// An INTEGER PRIMARY KEY (not DESC) is a rowid alias: no autoindex
		// exists at all and no sequence slot is consumed (SQLite creates no
		// index). INTEGER PRIMARY KEY DESC is an ordinary column and DOES get
		// an autoindex.
		if u.IsPK && len(u.Cols) == 1 && execdml.IsIPKRowidAliasCol(sql.ColumnDef{PrimaryKey: true, Type: colType(u.Cols[0]), PKDesc: colPKDesc(u.Cols[0])}) {
			continue
		}
		// On WITHOUT ROWID, a UNIQUE constraint duplicating the PRIMARY KEY
		// is merged into the clustered key: no autoindex, no slot consumed.
		if s.WithoutRowid && !u.IsPK && sameColumnNames(u.Cols, pkCols) {
			continue
		}
		seq++
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
	pg := ctx.Pager.AllocatePage()
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

// execDropTable implements DROP TABLE.
func (e *DDLExecutor) execDropTable(s *sql.DropTableStmt) *Result {
	e.ctx.InvalidateTableCaches()
	// SQLITE_IGNORE on SQLITE_DROP_TABLE silently skips the drop (auth-1.23.1
	// returns IGNORE for DROP TABLE and the table survives); DENY errors.
	if res := e.authorizeActionOrSkip(auth.ActionDropTable, s.Name, "", "", ""); res != nil {
		return res
	}
	// SQLite fires SQLITE_DELETE against the table's rows and against
	// sqlite_schema when dropping a table (auth-1.63 denies via
	// SQLITE_DELETE sqlite_master; auth-1.65 denies via SQLITE_DELETE t2;
	// auth-1.71/1.73 IGNORE them and the drop is skipped).
	for _, tgt := range []string{s.Name, "sqlite_master"} {
		if res := e.authorizeActionOrSkip(auth.ActionDelete, tgt, "", "", ""); res != nil {
			return res
		}
	}
	// Force a fresh schema read so a stale schema cache cannot make the DROP
	// target a table that is no longer in the btree ("deleted=0").
	for _, dbCtx := range e.ctx.DBList() {
		dbCtx.Schema.InvalidateCache()
	}
	entry, ctx, err := e.ctx.FindTable(s.Name)
	if err != nil {
		// The name is not a table. If it is a view, SQLite rejects rather
		// than deleting the wrong object ("use DROP VIEW to delete view v1").
		if _, _, vErr := e.ctx.FindView(s.Name); vErr == nil && !s.IfExists {
			return &Result{Error: fmt.Errorf("use DROP VIEW to delete view %s", s.Name)}
		}
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}

	// SQLite refuses to drop tables whose names begin with "sqlite_"
	// (src/build.c:3471 tableMayNotBeDropped, 3560-3561), except the
	// sqlite_statN analysis tables and sqlite_parameters. Error:
	// "table %s may not be dropped".
	lower := strings.ToLower(entry.Name)
	if strings.HasPrefix(lower, "sqlite_") &&
		!strings.HasPrefix(lower, "sqlite_stat") && lower != "sqlite_parameters" {
		return &Result{Error: fmt.Errorf("table %s may not be dropped", entry.Name)}
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
	e.dropTableCascade(ctx, entry)
	e.markDropTableFKDirty(entry, ctx)
	// Remove from schema — by TYPE so a TRIGGER named the same as the table
	// survives (SQLite keeps tables and triggers in separate namespaces;
	// DROP TABLE t1 must not drop a trigger named t1).
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeTable); err != nil {
		return &Result{Error: err}
	}
	// P8.INCRVACUUM: free the table's btree pages so the next
	// AutoVacuumCommit (FULL mode) or PRAGMA incremental_vacuum
	// can truncate the file. Without this, DROP TABLE leaks all
	// the table's pages — autovacuum-9.2 (file size after
	// DROP TABLE x5) needs this to shrink to 1024.
	if entry.RootPage != 0 && ctx.Pager != nil {
		bt := e.ctx.TableBTreePg(ctx.Pager, entry.Name, entry.RootPage, true)
		if err := bt.FreeTable(entry.RootPage); err != nil {
			return &Result{Error: err}
		}
	}
	e.refreshLargestRootPage(ctx)
	if res := e.dropTableCleanup(entry, ctx); res != nil {
		return res
	}
	return &Result{}
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
		// Free the index's btree pages so AutoVacuumCommit (FULL
		// mode) can truncate them. Without this, autovacuum-9.2
		// (file size after DROP TABLE x5) leaves the file at
		// hundreds of pages because the indexes' pages are still
		// "live" even though their schema entries are gone.
		if idx.RootPage != 0 && ctx.Pager != nil {
			ibt := e.ctx.TableBTreePg(ctx.Pager, idx.Name, idx.RootPage, false)
			_ = ibt.FreeTable(idx.RootPage)
		}
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
