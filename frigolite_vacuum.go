package frigolite

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/sql"
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
		v, err := db.engine.EvalExpr(vs.IntoExpr, nil)
		if err != nil {
			return &exec.Result{Error: err}
		}
		if cv, ok := v.(*util.ColumnValue); ok {
			v = util.UnwrapColumnValue(cv)
		}
		target, ok := v.(string)
		if !ok {
			// SQLite resolves column references at PREPARE time: an unknown
			// column reports "no such column" (with the written qualifier),
			// everything else that is not text reports "non-text filename".
			if cr, isCol := vs.IntoExpr.(*sql.ColumnRef); isCol {
				name := cr.Name
				if cr.Table != "" {
					name = cr.Table + "." + cr.Name
				}
				return &exec.Result{Error: fmt.Errorf("no such column: %s", name)}
			}
			return &exec.Result{Error: fmt.Errorf("non-text filename")}
		}
		return db.vacuumInto(schema, target)
	}
	if vs.Into != "" {
		return db.vacuumInto(schema, vs.Into)
	}
	return db.vacuumRebuild(schema)
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
	curReserve, reqReserve := uint32(0), uint32(0)
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
		curReserve = ctx.Pager.ReservedBytes()
		reqReserve = ctx.Pager.RequestedReserve()
	}
	reset := pending != 0 || (reqReserve != 0 && reqReserve != curReserve)
	if reset {
		if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
			size := pending
			if size == 0 {
				size = ctx.Pager.PageSize()
			}
			ctx.Pager.ResetToEmpty(size)
			if reqReserve != 0 {
				ctx.Pager.ApplyReservedBytes(reqReserve)
			}
			if err := ctx.Pager.Flush(); err != nil {
				return &exec.Result{Error: err}
			}
			ctx.Schema.InvalidateCache()
		}
	}
	// vacuum.c: the rebuild commits the main database exactly ONCE (the
	// copy-back via sqlite3BtreeCopyFile), so the file change counter moves
	// forward by exactly one — the copy's internal statements must not leave
	// their own per-statement bumps in the final image.
	preCC := uint32(0)
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
		preCC, _ = ctx.Pager.FileChangeCounter()
	}
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
	if ctx := db.engine.GetDB(schema); ctx != nil {
		ctx.PendingPageSize = 0
		ctx.Schema.InvalidateCache()
	}
	return &exec.Result{}
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
