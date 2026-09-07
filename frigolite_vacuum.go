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
	if err := copyViaBackup(db, schema, dst); err != nil {
		return &exec.Result{Error: err}
	}
	return &exec.Result{}
}

// vacuumRebuild implements plain VACUUM: the database is copied (a logical
// rebuild — objects re-created and data re-inserted) into an in-memory temp
// database and copied back. Content-preserving today; the file-shrink
// (compaction) step — replacing main's pager image with the compact temp
// image — is the remaining P8.VACUUM engine tranche (a cross-pager
// Pager.Restore corrupts: the snapshot's page size/fileSize come from a
// different pager; a file-level copy + cache reload is the next option).
func (db *DB) vacuumRebuild(schema string) *exec.Result {
	if db.path == "" || db.pager == nil {
		// An in-memory main cannot be file-replaced: rebuild content-only
		// (no file-shrink).
		tmp, err := Open(":memory:")
		if err != nil {
			return &exec.Result{Error: err}
		}
		defer tmp.Close()
		if err := copyViaBackup(db, schema, tmp); err != nil {
			return &exec.Result{Error: err}
		}
		return &exec.Result{}
	}

	// Rebuild the database into a temporary file (a logical copy: objects
	// re-created and data re-inserted — free pages are not carried over),
	// then replace main's file image with the compact one and make the
	// pager reload from disk (sqlite3BtreeCopyFile equivalent).
	tmpPath := db.path + "-vacuum-tmp"
	os.Remove(tmpPath)
	tmp, err := Open(tmpPath)
	if err != nil {
		return &exec.Result{Error: err}
	}
	copyErr := copyViaBackup(db, schema, tmp)
	if cerr := tmp.Close(); cerr != nil && copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		os.Remove(tmpPath)
		return &exec.Result{Error: copyErr}
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return &exec.Result{Error: err}
	}
	// Persist any pending main state, then replace the file image in place
	// (truncate + write, keeping the inode the pager holds open) and reload
	// the pager cache/header/page count from the new image.
	_ = db.pager.Flush()
	if err := os.WriteFile(db.path, data, 0644); err != nil {
		os.Remove(tmpPath)
		return &exec.Result{Error: err}
	}
	os.Remove(tmpPath)
	db.pager.CheckExternalFile()
	if ctx := db.engine.GetDB(schema); ctx != nil && ctx.Schema != nil {
		ctx.Schema.InvalidateCache()
	}
	return &exec.Result{}
}

// copyViaBackup copies srcSchema of src entirely into "main" on db via the
// backup machinery (sqlite3_backup_init + step(-1) + finish).
func copyViaBackup(src *DB, srcSchema string, dst *DB) error {
	b, err := src.NewBackup(dst, "main", srcSchema)
	if err != nil {
		return err
	}
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
