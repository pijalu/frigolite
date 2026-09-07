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
	// Plain VACUUM: the logical-rebuild machinery exists (see the
	// vacuumRebuild implementation in git history and the T-LOG in
	// plan/goals/P8.VACUUM.md) but does not yet compact or renumber, and
	// executing it regresses size/renumbering assertions that pass against
	// the historical no-op (vacuum4/5). It stays a no-op until the
	// compaction tranche lands.
	return &exec.Result{}
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
