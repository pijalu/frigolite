package frigolite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Backup represents an in-progress online backup of one database schema to
// another (the sqlite3_backup_* C API). A backup copies the source schema's
// objects and data into the destination schema; Step advances the copy by a
// number of pages and Finish completes it.
type Backup struct {
	dst       *DB
	dstSchema string
	src       *DB
	srcSchema string

	copied    int    // pages copied so far
	pagecount int    // source page count at init
	done      bool   // the logical copy has completed
	finished  bool   // Finish was called
	rc        string // last step/finish return code
	lastErr   string // last error message (for sqlite3_errmsg)

	initChange uint32 // source change counter at init/restart
	hasChange  bool   // the source exposes a change counter

	// KeepDestPageSize disables setDestPgsz's page-size adoption: VACUUM
	// pre-sizes the vacuum database at a PENDING page size
	// (pragma.c pNextPagesize) that must survive the copy.
	KeepDestPageSize bool

	// FullImageReplace marks the VACUUM copy-back (vacuum.c's second backup:
	// "copy vacuum_db back to the source"). backup.c overwrites the whole
	// destination image, so a populated destination is reset EMPTY first and
	// rebuilt — free pages are reclaimed and the file shrinks. A plain
	// backup leaves a populated destination untouched at the head.
	FullImageReplace bool
}

// NewBackup starts a backup of srcSchema on src into dstSchema on dst,
// equivalent to sqlite3_backup_init. It validates the schemas, rejects
// self-backup, and registers the destination as backup-locked (blocking
// DETACH) until Finish. On error the message is recorded on the source
// destination connection for sqlite3_errmsg.
func (db *DB) NewBackup(dst *DB, dstSchema, srcSchema string) (*Backup, error) {
	if db == nil || dst == nil || db.engine == nil || dst.engine == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	if db == dst {
		return nil, backupInitError(dst, "source and destination must be distinct")
	}
	srcCtx := db.engine.GetDB(srcSchema)
	if srcCtx == nil {
		return nil, backupInitError(dst, "unknown database %s", srcSchema)
	}
	dstCtx := dst.engine.GetDB(dstSchema)
	if dstCtx == nil {
		return nil, backupInitError(dst, "unknown database %s", dstSchema)
	}
	if dst.engine.DestSchemaInUse(dstSchema) {
		return nil, backupInitError(dst, "destination database is in use")
	}

	b := &Backup{
		dst:       dst,
		dstSchema: dstSchema,
		src:       db,
		srcSchema: srcSchema,
		pagecount: int(srcCtx.Pager.NumPages()),
	}
	if cc, ok := srcCtx.Pager.FileChangeCounter(); ok {
		b.initChange = cc
		b.hasChange = true
	}

	// Register an active backup on the source connection (blocks DETACH of
	// the source schema: SQLite holds a read lock on the source that makes
	// DETACH fail with "database X is locked") and on both connections
	// (blocks Close until Finish). The destination is NOT locked against
	// DETACH (backup5-3.2 detaches the destination mid-backup).
	db.engine.AddBackupLock(srcSchema)
	dst.activeBackups++
	if db != dst {
		db.activeBackups++
	}

	// An in-memory destination with a page size different from the source is
	// an error at the first step (SQLITE_READONLY), not at init.
	return b, nil
}

// Finish completes the backup, copying any remaining pages, and releases the
// backup locks. It returns "SQLITE_OK" on success (or the last error code).
func (b *Backup) Finish() string {
	if b == nil {
		return "SQLITE_ERROR"
	}
	defer b.release()
	if b.finished {
		return b.rc
	}
	b.finished = true
	if b.rc == "SQLITE_READONLY" || b.rc == "SQLITE_ERROR" {
		return b.rc
	}
	if b.done {
		b.rc = "SQLITE_OK"
		return b.rc
	}
	// Copy everything remaining.
	b.done = true
	b.copied = b.currentPagecount()
	if err := b.copyLocked(); err != nil {
		b.lastErr = err.Error()
		b.rc = "SQLITE_ERROR"
		return b.rc
	}
	if b.sourceEmpty() {
		b.resetEmptyDestination()
	}
	b.rc = "SQLITE_OK"
	return b.rc
}

// Remaining returns the number of pages not yet copied.
func (b *Backup) Remaining() int {
	if b == nil {
		return 0
	}
	total := b.currentPagecount()
	if b.copied >= total {
		return 0
	}
	return total - b.copied
}

// Pagecount returns the current number of pages in the source database.
func (b *Backup) Pagecount() int {
	if b == nil {
		return 0
	}
	return b.currentPagecount()
}

// ErrMsg returns the last error message recorded by the backup (for
// sqlite3_errmsg).
func (b *Backup) ErrMsg() string {
	if b == nil {
		return ""
	}
	return b.lastErr
}

// currentPagecount reads the source's current page count live (SQLite's
// sqlite3_backup_pagecount reports the current source size). Frigolite's
// PRIMARY KEY / UNIQUE auto-indexes (sqlite_autoindex_*) have no backing
// btree page (rootpage 0; uniqueness is enforced by scan), while SQLite
// allocates one page per auto-index. The backup reports the SQLite-visible
// page count, so each rootpage-0 auto-index adds one page.
func (b *Backup) currentPagecount() int {
	ctx := b.src.engine.GetDB(b.srcSchema)
	if ctx == nil || ctx.Pager == nil {
		return b.pagecount
	}
	return int(ctx.Pager.NumPages()) + autoIndexPageCount(ctx.Schema)
}

// autoIndexPageCount counts schema entries for auto-indexes without a backing
// btree page (rootpage 0).
func autoIndexPageCount(sm *schema.Manager) int {
	entries, err := sm.GetEntries(schema.TypeIndex)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.RootPage == 0 && strings.HasPrefix(strings.ToLower(e.Name), "sqlite_autoindex_") {
			n++
		}
	}
	return n
}

// dstPageMismatch reports whether the destination's page size differs from
// the source's (backup.c setDestPgsz precondition). It applies to memory AND
// file destinations alike: an empty destination adopts the source page size,
// a populated one cannot be resized.
func (b *Backup) dstPageMismatch() bool {
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if dstCtx == nil || dstCtx.Pager == nil {
		return false
	}
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	if srcCtx == nil {
		return false
	}
	return dstCtx.Pager.PageSize() != srcCtx.Pager.PageSize()
}

// checkBusy returns a nonzero rc ("SQLITE_BUSY") when the source or
// destination is locked, or "" when the backup may proceed.
func (b *Backup) checkBusy() string {
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if srcCtx == nil || dstCtx == nil {
		return "SQLITE_ERROR"
	}
	// A write transaction open on the source connection (any connection)
	// blocks the backup.
	if b.src.engine.WriteTxOpenOn(b.srcSchema) {
		return "SQLITE_BUSY"
	}
	// An exclusive lock on the source file by another connection.
	srcKey := b.src.engine.LockKeyForDB(b.srcSchema)
	if _, other := lockreg.Global.ExclusiveLockedByOther(srcKey, b.src.engine.ConnID()); other {
		return "SQLITE_BUSY"
	}
	if lockreg.Global.WriteTxByOther(srcKey, b.src.engine.ConnID()) {
		return "SQLITE_BUSY"
	}
	// A write transaction on the destination file by another connection.
	dstKey := b.dst.engine.LockKeyForDB(b.dstSchema)
	if lockreg.Global.WriteTxByOther(dstKey, b.dst.engine.ConnID()) {
		return "SQLITE_BUSY"
	}
	if _, other := lockreg.Global.ExclusiveLockedByOther(dstKey, b.dst.engine.ConnID()); other {
		return "SQLITE_BUSY"
	}
	return ""
}

// copyLocked performs the logical copy. The caller guarantees the destination
// is not busy. It drops the destination objects, recreates them from the
// source schema, and copies the data.
func (b *Backup) copyLocked() error {
	srcCtx := b.src.engine.GetDB(b.srcSchema)
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if srcCtx == nil || dstCtx == nil {
		return fmt.Errorf("unknown database")
	}
	// vacuum.c replays the rebuilt schema and copies rows at the PAGE level:
	// no trigger program ever runs. Suppress trigger firing on the
	// destination engine for the whole logical copy — an AFTER INSERT
	// trigger whose body references a table the rebuild has not recreated
	// yet (rowid order) would fail the rebuild (alter3 7.x with a temp
	// trigger surviving an ADD COLUMN).
	prevSuppressed := b.dst.engine.TriggersSuppressed()
	b.dst.engine.SetTriggersSuppressed(true)
	defer b.dst.engine.SetTriggersSuppressed(prevSuppressed)
	srcEntries, err := srcCtx.Schema.GetEntries("")
	if err != nil {
		return err
	}
	dstEntries, err := dstCtx.Schema.GetEntries("")
	if err != nil {
		return err
	}

	// Drop destination objects: triggers and views first (they may reference
	// tables), then tables (DROP TABLE removes its indexes).
	if err := b.dropDestObjects(dstEntries); err != nil {
		return err
	}

	// Create objects in the source's sqlite_master order (by rowid) so the
	// destination's sqlite_master row order matches (the dbcksum hashes
	// sqlite_master rows in rowid order). Table data is copied right after
	// each table is created; the engine resolves references (indexes,
	// triggers) lazily so a table may be created before its index.
	// sqlite_sequence is NOT created (its DDL is engine-reserved and the
	// AUTOINCREMENT tables recreate it) but its rows ARE copied, matching
	// vacuum.c: the CREATE pass excludes sqlite_sequence while the data-copy
	// loop (rootpage>0) includes it — so an AUTOINCREMENT counter survives a
	// backup/VACUUM unchanged.
	sort.Slice(srcEntries, func(i, j int) bool { return srcEntries[i].RowID < srcEntries[j].RowID })
	if err := b.createSourceObjects(srcEntries); err != nil {
		return err
	}
	// Copy the sqlite_statN ANALYZE tables (their CREATE TABLE DDL is
	// reserved, so create them via the engine's stat-table path).
	return b.copyStatTables(srcEntries)
}

// dropDestObjects drops the destination's triggers, views and tables so the
// rebuild starts from an empty schema. Triggers and views go first (they may
// reference tables), then tables (DROP TABLE removes its indexes). For a
// non-main destination schema, the DROP is qualified so the correct schema's
// object is removed (an unqualified DROP VIEW removes the main-schema object).
func (b *Backup) dropDestObjects(dstEntries []*schema.Entry) error {
	for _, typ := range []schema.SchemaType{schema.TypeTrigger, schema.TypeView, schema.TypeTable} {
		if err := b.dropDestObjectsOfType(dstEntries, typ); err != nil {
			return err
		}
	}
	return nil
}

// dropDestObjectsOfType drops every destination object of one schema type.
// System tables (sqlite_schema, sqlite_sequence, sqlite_statN) are skipped.
func (b *Backup) dropDestObjectsOfType(dstEntries []*schema.Entry, typ schema.SchemaType) error {
	dropQual := schemaQualifier(b.dstSchema)
	drop := "DROP " + strings.ToUpper(string(typ)) + " "
	for _, e := range dstEntries {
		if e.Type != typ {
			continue
		}
		if typ == schema.TypeTable && isSystemSchemaTable(e.Name) {
			continue
		}
		if r := b.dst.Exec(drop + dropQual + quotedTableName(e.Name)); r.Error != nil {
			return r.Error
		}
	}
	return nil
}

// createSourceObjects recreates the source's schema objects in the
// destination in sqlite_master order and copies each table's rows right
// after its CREATE.
func (b *Backup) createSourceObjects(srcEntries []*schema.Entry) error {
	for _, e := range srcEntries {
		if err := b.createSourceObject(e); err != nil {
			return err
		}
	}
	return nil
}

// createSourceObject recreates one source schema object in the destination:
// tables take the data-copy path (sqlite_sequence its counter-row path,
// system tables are skipped), indexes/views/triggers are created from their
// stored DDL (qualified for non-main destination schemas).
func (b *Backup) createSourceObject(e *schema.Entry) error {
	switch e.Type {
	case schema.TypeTable:
		return b.copySourceTableEntry(e)
	case schema.TypeIndex, schema.TypeView, schema.TypeTrigger:
		sql := e.SQL
		if q := schemaQualifier(b.dstSchema); q != "" {
			sql = qualifyCreateObjectSQL(sql, q, string(e.Type))
		}
		if r := b.dst.Exec(sql); r.Error != nil {
			return r.Error
		}
	}
	return nil
}

// copySourceTableEntry copies one table-type source entry into the
// destination.
func (b *Backup) copySourceTableEntry(e *schema.Entry) error {
	if strings.EqualFold(e.Name, "sqlite_sequence") {
		return b.copySequenceTable(e)
	}
	if isSystemSchemaTable(e.Name) {
		return nil
	}
	return b.copyTable(e)
}

// copyStatTables copies the sqlite_statN ANALYZE tables of the source.
func (b *Backup) copyStatTables(srcEntries []*schema.Entry) error {
	for _, e := range srcEntries {
		if e.Type != schema.TypeTable || !strings.HasPrefix(strings.ToLower(e.Name), "sqlite_stat") {
			continue
		}
		if err := b.copyStatTable(e); err != nil {
			return err
		}
	}
	return nil
}

// copySequenceTable copies the AUTOINCREMENT counter rows of sqlite_sequence
// into the destination. The table itself is NOT created (its DDL is
// engine-reserved; creating an AUTOINCREMENT table on the destination
// materializes it, so it exists by the time this runs — vacuum.c relies on
// the same ordering). Rows are replaced, not appended: a page-level backup
// overwrites the whole table.
func (b *Backup) copySequenceTable(e *schema.Entry) error {
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if dstCtx == nil {
		return fmt.Errorf("unknown database %s", b.dstSchema)
	}
	if _, err := dstCtx.Schema.GetEntries(""); err != nil {
		return err
	}
	found := false
	for _, de := range dstEntriesOfType(dstCtx.Schema, schema.TypeTable) {
		if strings.EqualFold(de.Name, "sqlite_sequence") {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	destTable := qualifiedTableRef(schemaQualifier(b.dstSchema), e.Name)
	if r := b.dst.Exec("DELETE FROM " + destTable); r.Error != nil {
		return r.Error
	}
	srcQual := schemaQualifier(b.srcSchema)
	r := b.src.Query("SELECT * FROM " + qualifiedTableRef(srcQual, e.Name))
	if r.Error != nil {
		return r.Error
	}
	for _, row := range r.Rows {
		var vals []string
		for _, v := range row {
			vals = append(vals, sqlLiteral(v))
		}
		ins := "INSERT INTO " + destTable + " VALUES(" + strings.Join(vals, ", ") + ")"
		if ir := b.dst.Exec(ins); ir.Error != nil {
			return ir.Error
		}
	}
	return nil
}

// dstEntriesOfType lists the destination schema entries of one type.
func dstEntriesOfType(m *schema.Manager, typ schema.SchemaType) []*schema.Entry {
	entries, err := m.GetEntries("")
	if err != nil {
		return nil
	}
	var out []*schema.Entry
	for _, e := range entries {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// copyStatTable copies a sqlite_statN ANALYZE statistics table: create it via
// the engine's stat-table path (CREATE TABLE sqlite_statN is reserved) then
// copy its rows.
func (b *Backup) copyStatTable(e *schema.Entry) error {
	// Parse the column list from the stored DDL: CREATE TABLE
	// sqlite_stat1(tbl,idx,stat) → "tbl,idx,stat".
	cols := statColumns(e.SQL)
	if b.dstSchema == "" || strings.EqualFold(b.dstSchema, "main") {
		if err := b.dst.engine.EnsureStatTable(e.Name, cols); err != nil {
			return err
		}
	} else {
		if err := b.dst.engine.EnsureStatTableIn(b.dstSchema, e.Name, cols); err != nil {
			return err
		}
	}
	srcQual := schemaQualifier(b.srcSchema)
	r := b.src.Query("SELECT * FROM " + qualifiedTableRef(srcQual, e.Name))
	if r.Error != nil {
		return r.Error
	}
	destTable := qualifiedTableRef(schemaQualifier(b.dstSchema), e.Name)
	for _, row := range r.Rows {
		var vals []string
		for _, v := range row {
			vals = append(vals, sqlLiteral(v))
		}
		ins := "INSERT INTO " + destTable + " VALUES(" + strings.Join(vals, ", ") + ")"
		if ir := b.dst.Exec(ins); ir.Error != nil {
			return ir.Error
		}
	}
	return nil
}

// statColumns extracts the column list from a sqlite_statN CREATE TABLE DDL.
func statColumns(createSQL string) string {
	upper := strings.ToUpper(createSQL)
	idx := strings.Index(upper, "(")
	if idx < 0 {
		return "tbl,idx,stat"
	}
	end := strings.LastIndex(createSQL, ")")
	if end < idx {
		return "tbl,idx,stat"
	}
	return createSQL[idx+1 : end]
}

// copyTable recreates one table in the destination and copies its rows.
func (b *Backup) copyTable(e *schema.Entry) error {
	qual := schemaQualifier(b.dstSchema)
	// Virtual-table schema entries (RootPage 0) carry no storage: a page-level
	// backup copies only the module's SHADOW tables, and the vtab's own rows
	// are whatever the module instance reads back from them. Create the vtab
	// in the destination (xCreate materializes fresh shadow tables there) and
	// copy no rows — the shadow entries later in sqlite_master order are real
	// tables and take the shadow path below.
	if isVirtualTableEntry(e) {
		return b.copyVtabEntry(e, qual)
	}
	// Vtab shadow tables already exist in the destination (xCreate made them
	// when the CREATE VIRTUAL TABLE entry was copied moments ago): replace
	// rows instead of re-creating — a second CREATE would fail with
	// "table already exists".
	shadow := b.destTableExists(e.Name)
	if err := b.createOrClearDestTable(e, qual, shadow); err != nil {
		return err
	}
	// Read rows from the source and insert into the destination. WITHOUT
	// ROWID tables have no rowid column; detect from the DDL. Tables whose
	// rowid is aliased by an INTEGER PRIMARY KEY column keep the rowid via
	// that column (SELECT * alone preserves it — and the shadow tables of
	// rtree-style modules name their IPK literally "rowid", where the old
	// "SELECT rowid, *" + name filter produced an arity mismatch). For plain
	// rowid tables the SELECT probes the rowid under a non-shadowed alias
	// name so the INSERT preserves exact rowids (a page-level backup does).
	// The qualified table reference uses the bare name (schema.tablename);
	// the engine's INSERT rejects a quoted table after a schema prefix
	// ("temp.\"t1\"").
	withoutRowid := strings.Contains(strings.ToUpper(e.SQL), "WITHOUT ROWID")
	defs := b.src.engine.ParseColumnDefs(e.Name, e.SQL)
	alias := ipkRowidAliasColumnName(defs)
	r := b.src.Query(srcSelectQuery(defs, qualifiedTableRef(schemaQualifier(b.srcSchema), e.Name), withoutRowid, alias))
	if r.Error != nil {
		return r.Error
	}
	// Column list for the INSERT: for the rowid-probe form the first SELECT
	// column is the rowid probe (insert as "rowid"); the rest are the table's
	// columns. For SELECT * forms the columns arrive in declared order.
	colNames := insertColumnList(r.Columns, withoutRowid, alias)
	return b.copyRowsToDest(e, qual, r, colNames)
}

// copyVtabEntry creates the virtual table in the destination (its shadow
// tables materialize at xCreate); no rows are copied.
func (b *Backup) copyVtabEntry(e *schema.Entry, qual string) error {
	sql := e.SQL
	if qual != "" {
		sql = qualifyCreateVirtualTableSQL(sql, qual)
	}
	if r := b.dst.Exec(sql); r.Error != nil {
		return r.Error
	}
	return nil
}

// createOrClearDestTable creates the destination table from its stored DDL
// (qualified for non-main schemas), or — when the table already exists (a
// vtab shadow materialized by xCreate moments ago) — clears its rows.
func (b *Backup) createOrClearDestTable(e *schema.Entry, qual string, exists bool) error {
	if !exists {
		sql := e.SQL
		if qual != "" {
			sql = qualifyCreateTableSQL(sql, qual)
		}
		if r := b.dst.Exec(sql); r.Error != nil {
			return r.Error
		}
		return nil
	}
	if r := b.dst.Exec("DELETE FROM " + qualifiedTableRef(qual, e.Name)); r.Error != nil {
		return r.Error
	}
	return nil
}

// srcSelectQuery builds the backup SELECT for one table: SELECT * forms keep
// the declared column order (WITHOUT ROWID / IPK-aliased rowid tables), the
// plain rowid form probes the rowid under a non-shadowed alias name.
func srcSelectQuery(defs []sql.ColumnDef, tableRef string, withoutRowid bool, alias string) string {
	if withoutRowid || alias != "" {
		return "SELECT * FROM " + tableRef
	}
	return "SELECT " + quoteIdent(rowidProbeName(defs)) + ", * FROM " + tableRef
}

// insertColumnList maps the SELECT's result columns to the INSERT's column
// list: for the rowid-probe form the first column is the probe (insert as
// "rowid"; a table may still DECLARE a column named "rowid" — `SELECT probe, *`
// then yields the same name twice and only the probe maps to the implicit
// rowid), the rest arrive in declared order.
func insertColumnList(columns []string, withoutRowid bool, alias string) []string {
	if withoutRowid || alias != "" {
		return columns
	}
	colNames := []string{"rowid"}
	for i, c := range columns {
		if i == 0 {
			continue // the probe itself
		}
		if c == "rowid" {
			continue
		}
		colNames = append(colNames, c)
	}
	return colNames
}

// copyRowsToDest inserts the SELECT's rows into the destination table with
// the exact column list, preserving exact rowids (a page-level backup does).
func (b *Backup) copyRowsToDest(e *schema.Entry, qual string, r *Result, colNames []string) error {
	colList := ""
	if len(colNames) > 0 {
		var q []string
		for _, c := range colNames {
			q = append(q, quoteIdent(c))
		}
		colList = "(" + strings.Join(q, ", ") + ")"
	}
	for _, row := range r.Rows {
		if len(row) != len(colNames) {
			return fmt.Errorf("backup: column mismatch for %s (%d values, %d columns)", e.Name, len(row), len(colNames))
		}
		var vals []string
		for _, v := range row {
			vals = append(vals, sqlLiteral(v))
		}
		ins := "INSERT INTO " + qualifiedTableRef(qual, e.Name) + colList + " VALUES(" + strings.Join(vals, ", ") + ")"
		if ir := b.dst.Exec(ins); ir.Error != nil {
			return ir.Error
		}
	}
	return nil
}

// isVirtualTableEntry reports whether e is a CREATE VIRTUAL TABLE schema
// entry (RootPage 0, no storage of its own).
func isVirtualTableEntry(e *schema.Entry) bool {
	return e.RootPage == 0 && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(e.SQL)), "CREATE VIRTUAL TABLE")
}

// destTableExists reports whether the destination schema already holds a
// table with the given name (vtab shadows materialize at xCreate time).
func (b *Backup) destTableExists(name string) bool {
	dstCtx := b.dst.engine.GetDB(b.dstSchema)
	if dstCtx == nil {
		return false
	}
	for _, de := range dstEntriesOfType(dstCtx.Schema, schema.TypeTable) {
		if strings.EqualFold(de.Name, name) {
			return true
		}
	}
	return false
}

// ipkRowidAliasColumnName returns the name of the column that aliases the
// rowid (INTEGER PRIMARY KEY, not DESC), or "" (build.c sqlite3AddPrimaryKey
// rule — the same shape as execdml's isIPKRowidAliasCol).
func ipkRowidAliasColumnName(defs []sql.ColumnDef) string {
	for _, cd := range defs {
		if cd.PrimaryKey && !cd.PKDesc && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER") {
			return cd.Name
		}
	}
	return ""
}

// rowidProbeName picks a rowid alias name NOT shadowed by a declared column
// so "SELECT <probe>, *" always reads the implicit rowid.
func rowidProbeName(defs []sql.ColumnDef) string {
	declared := make(map[string]bool, len(defs))
	for _, cd := range defs {
		declared[strings.ToLower(cd.Name)] = true
	}
	for _, probe := range []string{"_rowid_", "oid", "rowid"} {
		if !declared[probe] {
			return probe
		}
	}
	return "rowid"
}

func (b *Backup) sourceEmpty() bool {
	ctx := b.src.engine.GetDB(b.srcSchema)
	if ctx == nil || ctx.Pager == nil {
		return true
	}
	entries, err := ctx.Schema.GetEntries("")
	return err == nil && len(entries) == 0
}

func (b *Backup) resetEmptyDestination() {
	dst := b.dst.engine.GetDB(b.dstSchema)
	src := b.src.engine.GetDB(b.srcSchema)
	if dst == nil || src == nil || dst.Pager == nil || src.Pager == nil {
		return
	}
	// backup.c nSrcPage==0 branch: sqlite3BtreeNewDb rewrites the
	// destination as a fresh empty database at the source page size and
	// truncates it to one page. ResetToEmpty keeps the on-disk image
	// self-consistent (header page size inside page 1 matches the new
	// pageSize) so a later open does not read a truncated/corrupt file.
	dst.Pager.ResetToEmpty(src.Pager.PageSize())
	_ = dst.Pager.Flush()
	dst.Schema.InvalidateCache()
}

// release unregisters the backup locks held on the source and destination.
func (b *Backup) release() {
	if b.src != nil && b.src.engine != nil {
		b.src.engine.RemoveBackupLock(b.srcSchema)
	}
	if b.dst != nil && b.dst.engine != nil {
		b.dst.activeBackups--
		if b.dst.activeBackups < 0 {
			b.dst.activeBackups = 0
		}
	}
	if b.src != nil && b.src != b.dst && b.src.engine != nil {
		b.src.activeBackups--
		if b.src.activeBackups < 0 {
			b.src.activeBackups = 0
		}
	}
}

// LastErr returns the last error message recorded on this connection (for
// sqlite3_errmsg). SQLite's sqlite3_errmsg returns "not an error" when the
// most recent API call succeeded; mirror that for the empty state.
func (db *DB) LastErr() string {
	if db == nil || db.engine == nil {
		return ""
	}
	if msg := db.engine.LastErr(); msg != "" {
		return msg
	}
	return "not an error"
}

// SetLastErr records the last error message and code on this connection (for
// sqlite3_errmsg / sqlite3_errcode emulation). The code is an SQLITE_* result
// code string (e.g. "SQLITE_CONSTRAINT").
func (db *DB) SetLastErr(msg, code string) {
	if db == nil || db.engine == nil {
		return
	}
	db.engine.SetLastErr(msg, code)
}

// SetErrMsg implements sqlite3_set_errmsg: set the connection's error code
// and message so a later sqlite3_errmsg returns msg (main.c sqlite3_set_errmsg
// — "intended to be called by outside extensions"). A nil handle reports
// SQLITE_MISUSE; any other handle accepts the message and reports SQLITE_OK.
func (db *DB) SetErrMsg(errcode int, msg string) string {
	if db == nil || db.engine == nil {
		return "SQLITE_MISUSE"
	}
	db.engine.SetLastErr(msg, sqliteResultCodeName(errcode))
	return "SQLITE_OK"
}

// sqliteResultCodeName maps the small set of numeric result codes the C-API
// tests pass to sqlite3_set_errmsg onto their SQLITE_* names (rescode.h:
// SQLITE_OK=0, SQLITE_ERROR=1, SQLITE_MISUSE=21); unknown codes report the
// generic SQLITE_ERROR.
func sqliteResultCodeName(code int) string {
	switch code {
	case 0:
		return "SQLITE_OK"
	case 21:
		return "SQLITE_MISUSE"
	default:
		return "SQLITE_ERROR"
	}
}

// LastErrCode returns the last error code recorded on this connection (for
// sqlite3_errcode), e.g. "SQLITE_ERROR".
func (db *DB) LastErrCode() string {
	if db == nil || db.engine == nil {
		return "SQLITE_OK"
	}
	return db.engine.LastErrCode()
}

// BeginExclusive marks every database file of this connection as exclusively
// locked (BEGIN EXCLUSIVE emulation for backup lock tests).
func (db *DB) BeginExclusive() {
	if db == nil || db.engine == nil {
		return
	}
	if r := db.Exec("BEGIN EXCLUSIVE"); r.Error != nil {
		db.engine.BeginExclusive()
		return
	}
	db.engine.BeginExclusive()
}
