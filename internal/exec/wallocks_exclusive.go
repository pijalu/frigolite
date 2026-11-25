package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/lockreg"
)

// walExclusiveFileLock implements sqlite3PagerBegin's WAL exclusive-mode
// file-lock gate (pager.c:5928-5938). A write transaction under
// locking_mode=EXCLUSIVE takes the database file's EXCLUSIVE lock and holds
// it past the transaction (pager.c:2158 skips the unlock). Another
// connection's persistent holds block a fresh acquisition (e_wal-3.2.5:
// db's EXCLUSIVE INSERT fails against db2's WAL-mode file-SHARED hold).
//
// The FILE hold is the sole-connection proof. It is skipped while another
// connection is attached to the same wal-index: that peer shares the index
// the exclusive writer updates (writers are serialized on the wal-index
// mutex), so blocking its reads would break WAL visibility; C's shm-lock
// no-ops still apply to this connection alone. A connection opened AFTER
// the exclusive write finds no peer here and still gets the hold (e_wal
// 2.2.2/3.2.2).
func (e *Engine) walExclusiveFileLock(ctx *DatabaseContext) error {
	if ctx == nil || ctx.Pager == nil || e.lockStyle != LockStyleDefault ||
		!strings.EqualFold(e.schemaLockingMode(ctx.Name), "exclusive") {
		return nil
	}
	key := e.dbLockKey(ctx)
	if key == "" {
		return nil
	}
	if _, blocked := lockreg.Global.ExclusiveLockedByOther(key, e.connID); blocked {
		return fmt.Errorf("database is locked")
	}
	// An established exclusive writer keeps its exclusive shm mode: a peer's
	// file-SHARED hold refuses a fresh acquisition but not later commits.
	if !ctx.Pager.WALExclusiveActive() &&
		(lockreg.Global.WALFileSharedByOther(key, e.connID) ||
			lockreg.Global.SharedTxByOther(key, e.connID)) {
		return fmt.Errorf("database is locked")
	}
	if ctx.Pager.WALIndexShared() {
		return nil
	}
	lockreg.Global.SetExclusive(key, e.connID, true)
	if e.walExclusiveFileHeld == nil {
		e.walExclusiveFileHeld = make(map[string]bool)
	}
	e.walExclusiveFileHeld[key] = true
	return nil
}
