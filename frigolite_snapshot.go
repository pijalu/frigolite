// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

// DB snapshot API (sqlite3_snapshot_* C-API surface).
import (
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
)

// Snapshot is a frozen wal-index header image — the sqlite3_snapshot C-API
// handle (48 bytes, opaque outside the WAL layer). Snapshots are obtained
// with DB.SnapshotGet / DB.SnapshotGetBlob and opened with DB.SnapshotOpen
// / DB.SnapshotOpenBlob; SnapshotCmp orders two of them.
type Snapshot = pager.Snapshot

// SnapshotGet returns the snapshot of the database identified by schema
// ("main" or an attached database name) as currently read by this
// connection (sqlite3_snapshot_get). The connection must be inside a
// transaction (BEGIN issued), must not have a write transaction open on
// that database, and the database must be in WAL mode with a non-empty WAL;
// any other state fails with an SQLITE_ERROR error whose text is SQLite's
// bare-code message ("SQL logic error"). The snapshot stays valid while the
// caller's transaction holds.
func (db *DB) SnapshotGet(schema string) (*Snapshot, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.SnapshotGet(schema)
}

// SnapshotGetBlob is SnapshotGet's blob form (sqlite3_snapshot_get_blob):
// the 48-byte little-endian snapshot image, transferable across
// connections.
func (db *DB) SnapshotGetBlob(schema string) ([]byte, error) {
	if db == nil || db.engine == nil {
		return nil, fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.SnapshotGetBlob(schema)
}

// SnapshotOpen opens this connection's read transaction on snap
// (sqlite3_snapshot_open): the transaction then reads the database exactly
// as it was when the snapshot was taken, even after other connections
// committed or checkpointed — until a WAL wrap or a checkpoint past the
// snapshot makes it unavailable, reported as an SQLITE_ERROR_SNAPSHOT error
// ("snapshot is out of date"; SQLite defines no message text for the
// extended code, frigolite's text is the carrier). The same contract checks
// as SnapshotGet apply (inside a transaction, no write transaction, WAL
// mode). A nil snap opens a plain read transaction, matching C.
func (db *DB) SnapshotOpen(schema string, snap *Snapshot) error {
	if db == nil || db.engine == nil {
		return fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.SnapshotOpen(schema, snap)
}

// SnapshotOpenBlob is SnapshotOpen's blob form (sqlite3_snapshot_open_blob).
// A blob whose length is not 48 bytes fails with "bad SNAPSHOT" (test1.c).
func (db *DB) SnapshotOpenBlob(schema string, blob []byte) error {
	if db == nil || db.engine == nil {
		return fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.SnapshotOpenBlob(schema, blob)
}

// SnapshotCmp compares two snapshots (sqlite3_snapshot_cmp, wal.c L4552):
// a positive value when a is newer than b, negative when older, zero when
// they identify the same snapshot. aSalt[0] (incremented on every WAL
// restart) dominates, then mxFrame.
func SnapshotCmp(a, b *Snapshot) int {
	return pager.SnapshotCmp(a, b)
}

// SnapshotCmpBlob compares two snapshot blobs (sqlite3_snapshot_cmp_blob).
// Either blob having a length other than 48 bytes is an error ("bad
// SNAPSHOT", test1.c L2841).
func SnapshotCmpBlob(a, b []byte) (int, error) {
	sa, err := pager.SnapshotDecode(a)
	if err != nil {
		return 0, err
	}
	sb, err := pager.SnapshotDecode(b)
	if err != nil {
		return 0, err
	}
	return pager.SnapshotCmp(sa, sb), nil
}

// SnapshotRecover walks the shared wal-index's nBackfillAttempted mark back
// as far as the main database file content proves safe, re-enabling
// snapshots stranded behind a checkpointed WAL prefix
// (sqlite3_snapshot_recover). The schema must be main or an attached
// database and NO transaction may be open on it; violations fail with
// SQLITE_ERROR. It is not an error when the mark cannot be reduced at all.
func (db *DB) SnapshotRecover(schema string) error {
	if db == nil || db.engine == nil {
		return fmt.Errorf("frigolite: database not initialized")
	}
	return db.engine.SnapshotRecover(schema)
}
