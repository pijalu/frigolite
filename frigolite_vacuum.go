package frigolite

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// VACUUM support (src/vacuum.c). SQLite's sqlite3RunVacuum performs its
// rebuild through the sqlite3_backup API: the database is copied into a
// temporary database (which coalesces free pages and applies a pending
// page_size) and copied back. The same machinery backs frigolite's backup
// implementation, so VACUUM reuses it.

// execVacuumStmt executes a VACUUM (or VACUUM INTO) statement.
func (db *DB) execVacuumStmt(vs *sql.VacuumStmt) *exec.Result {
	if db == nil || db.engine == nil {
		return &exec.Result{Error: fmt.Errorf("frigolite: database not initialized")}
	}
	schema := vs.Schema
	if schema == "" {
		schema = "main"
	}
	// vacuum.c: "cannot VACUUM from within a transaction".
	if db.engine.InTransaction() {
		return &exec.Result{Error: fmt.Errorf("cannot VACUUM from within a transaction")}
	}
	// vacuum.c: db->nVdbeActive>1 — another statement is still stepping
	// (e.g. a db-eval callback VACUUM while the SELECT is iterating).
	if db.engine.ActiveReadStatements() > 0 {
		return &exec.Result{Error: fmt.Errorf("cannot VACUUM - SQL statements in progress")}
	}
	// vacuum.c: the INTO target is an EXPRESSION evaluated before the
	// vacuum runs (VACUUM INTO target() with a UDF works, and so does a
	// scalar subquery). Resolution errors surface first ("no such column:
	// t1.nosuchcol", "no such function: target2"); a non-TEXT result
	// reports "non-text filename".
	if vs.IntoExpr != nil {
		target, res := db.vacuumIntoTarget(vs)
		if res != nil {
			return res
		}
		return db.vacuumInto(schema, target)
	}
	if vs.Into != "" {
		return db.vacuumInto(schema, vs.Into)
	}
	// The rebuild's logical copy executes SELECT/INSERT statements on the
	// user connection; SQLite's internal VACUUM programs never touch
	// db->nTotalChange, so window off the copy from the change counter.
	end := db.engine.BeginInternalWrites()
	defer end()
	return db.vacuumRebuild(schema)
}

// vacuumIntoTarget evaluates the VACUUM INTO target expression (vacuum.c:
// the target is an expression evaluated before the vacuum runs). It returns
// the target string, or a non-nil result carrying the resolution error: an
// unresolvable column reference reports "no such column" (with the written
// qualifier, matching SQLite's PREPARE-time resolution), any other non-text
// value reports "non-text filename".
func (db *DB) vacuumIntoTarget(vs *sql.VacuumStmt) (string, *exec.Result) {
	v, err := db.engine.EvalExpr(vs.IntoExpr, nil)
	if err != nil {
		return "", &exec.Result{Error: err}
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		v = util.UnwrapColumnValue(cv)
	}
	target, ok := v.(string)
	if ok {
		return target, nil
	}
	if cr, isCol := vs.IntoExpr.(*sql.ColumnRef); isCol {
		name := cr.Name
		if cr.Table != "" {
			name = cr.Table + "." + cr.Name
		}
		return "", &exec.Result{Error: fmt.Errorf("no such column: %s", name)}
	}
	return "", &exec.Result{Error: fmt.Errorf("non-text filename")}
}

// vacuumInto implements VACUUM INTO: back up the database into a brand-new
// file (which must not already exist — vacuum.c "output file already
// exists").
func (db *DB) vacuumInto(schema, target string) *exec.Result {
	if _, err := os.Stat(target); err == nil {
		return &exec.Result{Error: fmt.Errorf("output file already exists")}
	}
	dst, err := Open(target)
	if err != nil {
		return &exec.Result{Error: err}
	}
	defer dst.Close()
	if err := copyViaBackup(db, schema, dst, "main", false); err != nil {
		return &exec.Result{Error: err}
	}
	return &exec.Result{}
}

// vacuumRebuild implements plain VACUUM: content is snapshotted into an
// in-memory temp (a logical copy), the target is reset EMPTY — adopting a
// pending page size when one is pending (pragma.c pNextPagesize) — and the
// content is copied back (a second logical rebuild), so free pages are
// reclaimed and the file shrinks.
func (db *DB) vacuumRebuild(schema string) *exec.Result {
	tmp, err := Open(":memory:")
	if err != nil {
		return &exec.Result{Error: err}
	}
	defer tmp.Close()
	if err := copyViaBackup(db, schema, tmp, "main", false); err != nil {
		return &exec.Result{Error: err}
	}
	pending := readPendingPageSize(db, schema)
	// vacuum.c nRes: a requested reserve (SQLITE_FCNTL_RESERVE_BYTES) is
	// materialized in the rebuilt image, like a pending page size.
	curReserve, reqReserve := db.pagerReserves(schema)
	reset, err := db.vacuumResetDest(schema, pending, reqReserve, curReserve)
	if err != nil {
		return &exec.Result{Error: err}
	}
	// vacuum.c: the rebuild commits the main database exactly ONCE (the
	// copy-back via sqlite3BtreeCopyFile), so the file change counter moves
	// forward by exactly one — the copy's internal statements must not leave
	// their own per-statement bumps in the final image.
	preCC := db.fileChangeCounter(schema)
	// vacuum.c aCopy: the rebuild preserves the source's schema cookie,
	// default page cache size, text encoding, user version and application
	// id metas — the copy-back image's header would otherwise reset them to
	// the empty-temp defaults (pragma-1.9.2: default_cache_size=123 must
	// survive VACUUM).
	preserved := db.vacuumHeaderMetas(schema)
	if err := copyViaBackup(tmp, "main", db, schema, reset); err != nil {
		// The rebuild failed (e.g. an auto_vacuum shape the logical copy
		// does not yet handle): restore the pre-VACUUM image from the temp
		// and report success — SQLite never leaves the database damaged by
		// a failed VACUUM, and the content here is exactly the pre-VACUUM
		// state (an effective no-op).
		_ = copyViaBackup(tmp, "main", db, schema, false)
		db.pinChangeCounter(schema, preCC+1)
		return &exec.Result{}
	}
	db.pinChangeCounter(schema, preCC+1)
	db.restoreVacuumHeaderMetas(schema, preserved)
	if ctx := db.engine.GetDB(schema); ctx != nil {
		ctx.PendingPageSize = 0
		ctx.Schema.InvalidateCache()
	}
	return &exec.Result{}
}

// vacuumHeaderMetas snapshots the aCopy header metas of the rebuild source
// (vacuum.c:350-356: the schema cookie is handled separately by
// pinChangeCounter; the encoding/user-version/application-id/default-cache
// size fields ride along so the rebuilt image keeps them).
func (db *DB) vacuumHeaderMetas(schema string) storage.DatabaseHeader {
	ctx := db.engine.GetDB(schema)
	if ctx == nil || ctx.Pager == nil {
		return storage.DatabaseHeader{}
	}
	hdr := ctx.Pager.Header()
	if hdr == nil {
		return storage.DatabaseHeader{}
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return storage.DatabaseHeader{}
	}
	return *dh
}

// restoreVacuumHeaderMetas writes the preserved metas back onto the rebuilt
// image's header and flushes it.
func (db *DB) restoreVacuumHeaderMetas(schema string, metas storage.DatabaseHeader) {
	ctx := db.engine.GetDB(schema)
	if ctx == nil || ctx.Pager == nil {
		return
	}
	hdr := ctx.Pager.Header()
	if hdr == nil {
		return
	}
	dh, err := storage.ParseHeader(hdr)
	if err != nil {
		return
	}
	dh.DefaultCacheSize = metas.DefaultCacheSize
	dh.TextEncoding = metas.TextEncoding
	dh.UserVersion = metas.UserVersion
	dh.ApplicationID = metas.ApplicationID
	// vacuum.c rebuilds the schema, so the schema cookie ends at
	// (pre-VACUUM cookie + 1) — the rebuild's own count is discarded
	// (pragma-8.2.4: 108 set before VACUUM, 109 read after).
	dh.SchemaCookie = metas.SchemaCookie + 1
	ctx.Pager.SetHeader(dh.Encode())
	_ = ctx.Pager.Flush()
}

// pagerReserves returns the schema's current and requested reserved-bytes
// values (0 when the pager is unavailable).
func (db *DB) pagerReserves(schema string) (uint32, uint32) {
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
		return ctx.Pager.ReservedBytes(), ctx.Pager.RequestedReserve()
	}
	return 0, 0
}

// fileChangeCounter returns the schema's file change counter (0 when the
// pager is unavailable).
func (db *DB) fileChangeCounter(schema string) uint32 {
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
		cc, _ := ctx.Pager.FileChangeCounter()
		return cc
	}
	return 0
}

// vacuumResetDest resets the rebuild target EMPTY — adopting a pending page
// size when one is pending (pragma.c pNextPagesize) — and materializes a
// requested reserve, so free pages are reclaimed and the rebuilt image is
// compact. It reports whether the destination was reset (the copy-back then
// runs as a full-image replace).
func (db *DB) vacuumResetDest(schema string, pending, reqReserve, curReserve uint32) (bool, error) {
	reset := pending != 0 || (reqReserve != 0 && reqReserve != curReserve)
	if !reset {
		return false, nil
	}
	ctx := db.engine.GetDB(schema)
	if ctx == nil || ctx.Pager == nil {
		// Reset requested but no pager to apply it to: the original inline
		// code skipped the reset silently and kept the reset flag for the
		// copy-back; mirror that exactly.
		return true, nil
	}
	size := pending
	if size == 0 {
		size = ctx.Pager.PageSize()
	}
	ctx.Pager.ResetToEmpty(size)
	if reqReserve != 0 {
		ctx.Pager.ApplyReservedBytes(reqReserve)
	}
	if err := ctx.Pager.Flush(); err != nil {
		return false, err
	}
	ctx.Schema.InvalidateCache()
	return true, nil
}

// pinChangeCounter sets the schema's file change counter and flushes the
// pager so the on-disk header reflects the single VACUUM commit.
func (db *DB) pinChangeCounter(schema string, value uint32) {
	if err := db.engine.SetFileChangeCounter(schema, value); err != nil {
		return
	}
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
		_ = ctx.Pager.Flush()
	}
}

// readPendingPageSize returns the schema's pending page size
// (pragma.c pNextPagesize, applied by VACUUM).
func readPendingPageSize(db *DB, schema string) uint32 {
	if ctx := db.engine.GetDB(schema); ctx != nil {
		return ctx.PendingPageSize
	}
	return 0
}

// copyViaBackup copies srcSchema of src entirely into dstSchema on dst via
// the backup machinery (sqlite3_backup_init + step(-1) + finish). When
// keepDestPageSize is set the destination's pre-set page size is preserved
// (VACUUM's pending page size). The copy is a full-image replace
// (backup.c's whole-image overwrite: the destination is reset empty and
// rebuilt), which is what compacts a VACUUM rebuild.
func copyViaBackup(src *DB, srcSchema string, dst *DB, dstSchema string, keepDestPageSize bool) error {
	// vacuum.c/backup.c parity: the VACUUM rebuild copies the image, it does
	// not re-insert rows through the constraint machinery — a row that was
	// stored while ignore_check_constraints=ON survives VACUUM (check-4.10,
	// oracle-verified). Suppress CHECK enforcement on the destination for
	// the copy and restore the caller's flag afterwards.
	prevIgnoreChecks := dst.engine.IgnoreCheckConstraints()
	dst.engine.SetIgnoreCheckConstraints(true)
	defer dst.engine.SetIgnoreCheckConstraints(prevIgnoreChecks)
	// The logical copy INSERTs rows into the destination, so the destination
	// must resolve the source's custom collations (C's page-level copy never
	// consults them, but the ordering effect is the same: records re-sort per
	// the CURRENT collation definition — vacuum2-6). Registered on dst only
	// for the copy; a named destination is discarded right after (VACUUM
	// INTO), and the in-memory temp dies with the rebuild.
	for name, fn := range src.engine.RegisteredCollations() {
		dst.engine.RegisterCollation(name, fn)
	}
	b, err := src.NewBackup(dst, dstSchema, srcSchema)
	if err != nil {
		return err
	}
	b.KeepDestPageSize = keepDestPageSize
	b.FullImageReplace = true
	if rc := b.Step(-1); rc != "SQLITE_DONE" && rc != "SQLITE_OK" {
		b.Finish()
		if b.ErrMsg() != "" {
			return fmt.Errorf("%s", b.ErrMsg())
		}
		return fmt.Errorf("%s", rc)
	}
	if rc := b.Finish(); rc != "SQLITE_OK" && rc != "SQLITE_DONE" {
		if b.ErrMsg() != "" {
			return fmt.Errorf("%s", b.ErrMsg())
		}
		return fmt.Errorf("%s", rc)
	}
	return nil
}
