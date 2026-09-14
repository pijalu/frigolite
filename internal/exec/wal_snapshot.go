package exec

// wal_snapshot.go — the engine-level contract layer for the
// sqlite3_snapshot C-API surface (P7.WAL-G7 slice 4). The contracts here
// are main.c's sqlite3_snapshot_get (L4976), sqlite3_snapshot_open (L5016)
// and sqlite3_snapshot_recover (L5072); the wal-index mechanics live in
// internal/pager (walsnapshot.go).
//
// main.c order of checks, mirrored per method:
//
//	sqlite3_snapshot_get:     autoCommit==0 → schema main/attached (not
//	                          temp) → no SQLITE_TXN_WRITE → WAL mode →
//	                          non-empty WAL   (all failures: SQLITE_ERROR)
//	sqlite3_snapshot_open:    autoCommit==0 → schema → no TXN_WRITE →
//	                          [verify+reopen under CKPT | arm+begin] → WAL
//	sqlite3_snapshot_recover: schema → SQLITE_TXN_NONE → begin → recover →
//	                          commit
//
// Bare SQLITE_ERROR returns carry the sqlite3_errmsg text for a bare
// SQLITE_ERROR ("SQL logic error"); the pager reports the
// SQLITE_ERROR_SNAPSHOT stale-snapshot error (see pager.WALSnapshotOpen).

import (
	"errors"
	"strings"

	"github.com/pijalu/frigolite/internal/pager"
)

// errSnapshotAPI is the bare SQLITE_ERROR the C snapshot API returns for
// every contract violation — "SQL logic error" is sqlite3ErrStr's text for
// SQLITE_ERROR (the C functions initialize rc=SQLITE_ERROR and return it
// without setting a specific message).
var errSnapshotAPI = errors.New("SQL logic error")

// snapshotCtx resolves a schema name to its database context the way
// sqlite3FindDbName + the iDb==0||iDb>1 gate do (main.c L4992): "main" (or
// empty) and every ATTACHed database qualify; the temp database (iDb==1)
// and unknown names do not.
func (e *Engine) snapshotCtx(schema string) (*DatabaseContext, error) {
	switch strings.ToUpper(schema) {
	case "", "MAIN":
		if e.mainDB == nil {
			return nil, errSnapshotAPI
		}
		return e.mainDB, nil
	case "TEMP", "TEMPORARY":
		return nil, errSnapshotAPI
	}
	if ctx := e.getDB(schema); ctx != nil {
		return ctx, nil
	}
	return nil, errSnapshotAPI
}

// snapshotInvalidated drops the engine's derived caches after a snapshot
// call re-pinned the WAL read transaction on a different header (the
// engine-side pager_reset — same propagation as walBeginStmtWrite's
// changed path: table/rowid caches and the schema cache).
func (e *Engine) snapshotInvalidated(ctx *DatabaseContext) {
	e.invalidateTableCaches()
	if ctx != nil && ctx.Schema != nil {
		ctx.Schema.InvalidateCache()
	}
}

// SnapshotGet returns the snapshot of the database currently pinned by this
// connection's read transaction (sqlite3_snapshot_get). The connection must
// be inside a transaction (autocommit off — main.c L4989), the schema must
// be main or an attached database (not temp), no write transaction may be
// open, and the database must be in WAL mode with a non-empty WAL; every
// violation is SQLITE_ERROR ("SQL logic error"). The read transaction is
// opened on the snapshot-compatible path when not already open, so the
// snapshot stays valid while the caller's transaction holds.
func (e *Engine) SnapshotGet(schema string) (*pager.Snapshot, error) {
	if !e.tx.inTransaction {
		return nil, errSnapshotAPI
	}
	ctx, err := e.snapshotCtx(schema)
	if err != nil {
		return nil, err
	}
	changed, snap, err := ctx.Pager.WALSnapshotGet()
	if err != nil {
		return nil, err
	}
	if changed {
		e.snapshotInvalidated(ctx)
	}
	return snap, nil
}

// SnapshotOpen opens (or re-anchors) this connection's read transaction on
// the given snapshot (sqlite3_snapshot_open): subsequent reads in the open
// transaction see the database exactly as it was when the snapshot was
// taken. The same contract checks as SnapshotGet apply (autocommit off, no
// write transaction, main/attached schema, WAL mode). When a read
// transaction is already open, it is verified against the live wal-index
// and re-opened on the snapshot; a stale snapshot fails with
// SQLITE_ERROR_SNAPSHOT ("snapshot is out of date"). A nil snapshot opens a
// plain read transaction (C arms nothing).
func (e *Engine) SnapshotOpen(schema string, snap *pager.Snapshot) error {
	if !e.tx.inTransaction {
		return errSnapshotAPI
	}
	ctx, err := e.snapshotCtx(schema)
	if err != nil {
		return err
	}
	changed, err := ctx.Pager.WALSnapshotOpen(snap)
	if err != nil {
		return err
	}
	if changed {
		e.snapshotInvalidated(ctx)
	}
	return nil
}

// SnapshotRecover reduces the shared wal-index's nBackfillAttempted mark as
// far as the main-file content proves safe, so snapshots stranded behind a
// checkpointed WAL prefix become openable again (sqlite3_snapshot_recover).
// The schema must be main or attached, and NO transaction may be open on
// that database (SQLITE_TXN_NONE — a bare deferred BEGIN counts as none);
// any violation is SQLITE_ERROR.
func (e *Engine) SnapshotRecover(schema string) error {
	ctx, err := e.snapshotCtx(schema)
	if err != nil {
		return err
	}
	return ctx.Pager.WALSnapshotRecover()
}

// SnapshotBlobSize is the byte length of a valid snapshot blob
// (sizeof(sqlite3_snapshot)).
const SnapshotBlobSize = pager.SnapshotSize

// SnapshotGetBlob is SnapshotGet's blob form (sqlite3_snapshot_get_blob,
// test1.c L2747): the 48-byte little-endian snapshot image.
func (e *Engine) SnapshotGetBlob(schema string) ([]byte, error) {
	snap, err := e.SnapshotGet(schema)
	if err != nil {
		return nil, err
	}
	blob := make([]byte, pager.SnapshotSize)
	snap.SnapshotBlob(blob)
	return blob, nil
}

// SnapshotOpenBlob is SnapshotOpen's blob form (sqlite3_snapshot_open_blob,
// test1.c L2783). A blob whose length is not sizeof(sqlite3_snapshot) is
// "bad SNAPSHOT".
func (e *Engine) SnapshotOpenBlob(schema string, blob []byte) error {
	snap, err := pager.SnapshotDecode(blob)
	if err != nil {
		return err
	}
	return e.SnapshotOpen(schema, snap)
}
