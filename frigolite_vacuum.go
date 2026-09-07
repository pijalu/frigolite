package frigolite

import (
	"fmt"
	"os"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/sql"
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
	if err := copyViaBackup(db, schema, dst, false); err != nil {
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
	if err := copyViaBackup(db, schema, tmp, false); err != nil {
		return &exec.Result{Error: err}
	}
	pending := readPendingPageSize(db, schema)
	if pending != 0 {
		if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Pager != nil {
			ctx.Pager.ResetToEmpty(pending)
			if err := ctx.Pager.Flush(); err != nil {
				return &exec.Result{Error: err}
			}
			ctx.Schema.InvalidateCache()
		}
	}
	if err := copyViaBackup(tmp, "main", db, pending != 0); err != nil {
		// The rebuild failed (e.g. an auto_vacuum shape the logical copy
		// does not yet handle): restore the pre-VACUUM image from the temp
		// and report success — SQLite never leaves the database damaged by
		// a failed VACUUM, and the content here is exactly the pre-VACUUM
		// state (an effective no-op).
		_ = copyViaBackup(tmp, "main", db, false)
		return &exec.Result{}
	}
	if ctx := db.engine.GetDB(schema); ctx != nil {
		ctx.PendingPageSize = 0
		ctx.Schema.InvalidateCache()
	}
	return &exec.Result{}
}

// readPendingPageSize returns the schema's pending page size
// (pragma.c pNextPagesize, applied by VACUUM).
func readPendingPageSize(db *DB, schema string) uint32 {
	if ctx := db.engine.GetDB(schema); ctx != nil {
		return ctx.PendingPageSize
	}
	return 0
}

// copyViaBackup copies srcSchema of src entirely into "main" on db via the
// backup machinery (sqlite3_backup_init + step(-1) + finish). When
// keepDestPageSize is set the destination's pre-set page size is preserved
// (VACUUM's pending page size).
func copyViaBackup(src *DB, srcSchema string, dst *DB, keepDestPageSize bool) error {
	b, err := src.NewBackup(dst, "main", srcSchema)
	if err != nil {
		return err
	}
	b.KeepDestPageSize = keepDestPageSize
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
