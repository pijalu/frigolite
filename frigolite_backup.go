package frigolite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
	"github.com/pijalu/frigolite/internal/schema"
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

// Step advances the backup by nPages pages (nPages < 0 copies the whole
// database, nPages == 0 copies nothing). It returns the SQLite result code
// string: "SQLITE_DONE" when the backup has completed, "SQLITE_OK" when
// pages remain, "SQLITE_BUSY" when the source or destination is locked, and
// "SQLITE_READONLY" for a populated in-memory destination with a mismatched
// page size. An empty in-memory destination adopts the source page size on its
// first step, matching SQLite's backup.c setDestPgsz behavior.
func (b *Backup) Step(nPages int) string {
	if b == nil {
		return "SQLITE_ERROR"
	}
	if b.finished {
		return b.rc
	}
	if b.done {
		b.rc = "SQLITE_DONE"
		return b.rc
	}

	// SQLite's backup.c calls setDestPgsz on the first step. An empty
	// destination (memory or file) adopts the source page size before any
	// page is written; an in-memory destination cannot be resized once it
	// holds pages and a mismatch fails with SQLITE_READONLY. A populated
	// file-backed destination proceeds: frigolite rebuilds it logically,
	// so the destination page size adapts through the copy itself.
	if b.dstPageMismatch() {
		srcCtx := b.src.engine.GetDB(b.srcSchema)
		dstCtx := b.dst.engine.GetDB(b.dstSchema)
		if srcCtx == nil || dstCtx == nil {
			b.rc = "SQLITE_ERROR"
			b.lastErr = "unknown database"
			return b.rc
		}
		if dstCtx.IsMemory {
			if dstCtx.Pager.NumPages() > 1 {
				b.rc = "SQLITE_READONLY"
				b.lastErr = "attempt to write a readonly database"
				return b.rc
			}
			dstCtx.Pager.ResetToEmpty(srcCtx.Pager.PageSize())
			dstCtx.Schema.InvalidateCache()
		} else if dstCtx.Pager.OpenedEmpty() || dstCtx.Pager.NumPages() == 0 {
			// setDestPgsz for a file destination never written to:
			// re-create it at the source page size. ResetToEmpty +
			// immediate Flush keeps the on-disk image self-consistent
			// (canonical header inside page 1) so the per-statement file
			// checks never observe a truncated/zeroed image.
			dstCtx.Pager.ResetToEmpty(srcCtx.Pager.PageSize())
			_ = dstCtx.Pager.Flush()
			dstCtx.Schema.InvalidateCache()
		}
	}

	// Lock checks: an open write transaction on the source, an exclusive lock
	// on the source by another connection, or a write transaction on the
	// destination by another connection all return SQLITE_BUSY. A missing
	// destination schema (detached mid-backup, backup5-3.3) returns
	// SQLITE_ERROR with "unknown database <name>" on the destination.
	if b.dst.engine.GetDB(b.dstSchema) == nil {
		b.rc = "SQLITE_ERROR"
		b.lastErr = "unknown database " + b.dstSchema
		b.dst.engine.SetLastErr(b.lastErr, "SQLITE_ERROR")
		return b.rc
	}
	if rc := b.checkBusy(); rc != "" {
		b.rc = rc
		return b.rc
	}

	// A source modified since the backup started (or since the last restart)
	// restarts the backup for in-memory sources; file-backed sources continue
	// (their previously copied pages remain valid snapshots).
	if b.hasChange {
		if srcCtx := b.src.engine.GetDB(b.srcSchema); srcCtx != nil {
			if cc, ok := srcCtx.Pager.FileChangeCounter(); ok && cc != b.initChange {
				if srcCtx.IsMemory {
					b.copied = 0
					b.initChange = cc
				}
			}
		}
	}

	// step(0) copies nothing: SQLITE_OK unless already complete.
	if nPages == 0 {
		if b.copied >= b.currentPagecount() {
			b.done = true
			b.rc = "SQLITE_DONE"
			return b.rc
		}
		b.rc = "SQLITE_OK"
		return b.rc
	}

	// nPages < 0 copies the whole database.
	if nPages < 0 {
		nPages = b.currentPagecount()
	}

	total := b.currentPagecount()
	b.copied += nPages
	if b.copied >= total {
		b.copied = total
		b.done = true
		b.rc = "SQLITE_DONE"
		if err := b.copyLocked(); err != nil {
			b.lastErr = err.Error()
			b.rc = "SQLITE_ERROR"
		} else if b.sourceEmpty() {
			b.resetEmptyDestination()
		}
		return b.rc
	}
	b.rc = "SQLITE_OK"
	return b.rc
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
	srcEntries, err := srcCtx.Schema.GetEntries("")
	if err != nil {
		return err
	}
	dstEntries, err := dstCtx.Schema.GetEntries("")
	if err != nil {
		return err
	}

	// Drop destination objects: triggers and views first (they may reference
	// tables), then tables (DROP TABLE removes its indexes). For a non-main
	// destination schema, qualify the DROP so the correct schema's object is
	// removed (an unqualified DROP VIEW removes the main-schema object).
	dropQual := schemaQualifier(b.dstSchema)
	for _, e := range dstEntries {
		if e.Type == schema.TypeTrigger {
			if r := b.dst.Exec("DROP TRIGGER " + dropQual + bareTableName(e.Name)); r.Error != nil {
				return r.Error
			}
		}
	}
	for _, e := range dstEntries {
		if e.Type == schema.TypeView {
			if r := b.dst.Exec("DROP VIEW " + dropQual + bareTableName(e.Name)); r.Error != nil {
				return r.Error
			}
		}
	}
	for _, e := range dstEntries {
		if e.Type == schema.TypeTable && !isSystemSchemaTable(e.Name) {
			if r := b.dst.Exec("DROP TABLE " + dropQual + bareTableName(e.Name)); r.Error != nil {
				return r.Error
			}
		}
	}

	// Create objects in the source's sqlite_master order (by rowid) so the
	// destination's sqlite_master row order matches (the dbcksum hashes
	// sqlite_master rows in rowid order). Table data is copied right after
	// each table is created; the engine resolves references (indexes,
	// triggers) lazily so a table may be created before its index.
	sort.Slice(srcEntries, func(i, j int) bool { return srcEntries[i].RowID < srcEntries[j].RowID })
	for _, e := range srcEntries {
		switch e.Type {
		case schema.TypeTable:
			if isSystemSchemaTable(e.Name) {
				continue
			}
			if err := b.copyTable(e); err != nil {
				return err
			}
		case schema.TypeIndex, schema.TypeView, schema.TypeTrigger:
			sql := e.SQL
			if q := schemaQualifier(b.dstSchema); q != "" {
				sql = qualifyCreateObjectSQL(sql, q, string(e.Type))
			}
			if r := b.dst.Exec(sql); r.Error != nil {
				return r.Error
			}
		}
	}
	// Copy the sqlite_statN ANALYZE tables (their CREATE TABLE DDL is
	// reserved, so create them via the engine's stat-table path).
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
	r := b.src.Query("SELECT * FROM " + srcQual + bareTableName(e.Name))
	if r.Error != nil {
		return r.Error
	}
	destTable := schemaQualifier(b.dstSchema) + bareTableName(e.Name)
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
	// CREATE TABLE cannot be schema-qualified in the stored DDL for the main
	// schema; for attached/temp destinations, qualify the name.
	sql := e.SQL
	if qual != "" {
		sql = qualifyCreateTableSQL(sql, qual)
	}
	if r := b.dst.Exec(sql); r.Error != nil {
		return r.Error
	}
	// Read rows from the source and insert into the destination. WITHOUT
	// ROWID tables have no rowid column; detect from the DDL. For rowid
	// tables the SELECT includes the rowid first so the INSERT preserves
	// exact rowids (a page-level backup does). The qualified table reference
	// uses the bare name (schema.tablename); the engine's INSERT rejects a
	// quoted table after a schema prefix ("temp.\"t1\"").
	withoutRowid := strings.Contains(strings.ToUpper(e.SQL), "WITHOUT ROWID")
	srcQual := schemaQualifier(b.srcSchema)
	tableRef := srcQual + bareTableName(e.Name)
	var srcQuery string
	var colNames []string
	if withoutRowid {
		srcQuery = "SELECT * FROM " + tableRef
	} else {
		srcQuery = "SELECT rowid, * FROM " + tableRef
	}
	r := b.src.Query(srcQuery)
	if r.Error != nil {
		return r.Error
	}
	// Column list for the INSERT: for rowid tables the first SELECT column is
	// rowid (insert as "rowid"); the rest are the table's columns.
	destTable := schemaQualifier(b.dstSchema) + bareTableName(e.Name)
	if !withoutRowid {
		colNames = append(colNames, "rowid")
	}
	for _, c := range r.Columns {
		if withoutRowid || c != "rowid" {
			colNames = append(colNames, c)
		}
	}
	colList := ""
	if len(colNames) > 0 {
		var q []string
		for _, c := range colNames {
			q = append(q, quoteIdent(c))
		}
		colList = "(" + strings.Join(q, ", ") + ")"
	}
	for _, row := range r.Rows {
		var vals []string
		for _, v := range row {
			vals = append(vals, sqlLiteral(v))
		}
		ins := "INSERT INTO " + destTable + colList + " VALUES(" + strings.Join(vals, ", ") + ")"
		if ir := b.dst.Exec(ins); ir.Error != nil {
			return ir.Error
		}
	}
	return nil
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
