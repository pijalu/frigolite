package frigolite

// Native regression tests for PRAGMA incremental_vacuum's guard semantics,
// pinned against the SQLite C oracle (btree.c sqlite3BtreeIncrVacuum) and
// incrvacuum-5.3/17.1.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestP8IncrVacuumAutoVacuumOffNoOp: sqlite3BtreeIncrVacuum returns
// SQLITE_DONE when !pBt->autoVacuum — on a database not in auto-vacuum
// mode the pragma is a silent no-op (no rows, no error), NOT a corrupt
// error (finalDbSize's unsigned ptrmap arithmetic wraps for such
// databases and must not be consulted).
func TestP8IncrVacuumAutoVacuumOffNoOp(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "avoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"PRAGMA auto_vacuum = 'none';",
		"CREATE TABLE t(x);",
		"INSERT INTO t VALUES(1);",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	r := db.Query("PRAGMA incremental_vacuum;")
	if r.Error != nil {
		t.Fatalf("incremental_vacuum on av=none must be a no-op, got: %v", r.Error)
	}
	if len(r.Rows) != 0 {
		t.Fatalf("expected no rows (SQLITE_DONE), got %v", r.Rows)
	}
}

// TestP8IncrVacuumInTransactionYields: an incremental_vacuum inside an
// open transaction must not report corruption — during the transaction
// the header page count legitimately leads the on-disk file (uncommitted
// appends live in the page cache). C trusts the in-memory btree state
// (btreePagecount) here; the engine's phase7 guard yields a row and
// defers the file truncation to commit.
func TestP8IncrVacuumInTransactionYields(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "intxn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	str := strings.Repeat("abcdefghij", 130)
	setup := []string{
		"PRAGMA auto_vacuum = 'incremental';",
		"BEGIN;",
		"CREATE TABLE t1(a, b);",
		"CREATE INDEX t1_i ON t1(a);",
		"INSERT INTO t1 VALUES('" + str + "', '" + str + "');",
		"INSERT INTO t1 SELECT b, a FROM t1;",
		"DELETE FROM t1 WHERE rowid % 2;",
	}
	for _, sql := range setup {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	r := db.Query("PRAGMA incremental_vacuum;")
	if r.Error != nil {
		t.Fatalf("in-transaction incremental_vacuum must not error, got: %v", r.Error)
	}
	// ROLLBACK restores a clean state (phase7 fidelity).
	if r := db.Exec("ROLLBACK;"); r.Error != nil {
		t.Fatalf("ROLLBACK: %v", r.Error)
	}
	if r := db.Query("PRAGMA integrity_check;"); r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	} else if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after ROLLBACK: %v", r.Rows)
	}
}

// TestP8IncrVacuumWritableSchemaHeaderBeyondFile: incrvacuum-17.1 parity.
// A crafted image whose header page count (7) exceeds the file (5 pages)
// with MATCHING change counters is corrupt for schema reads — but
// PRAGMA writable_schema=ON sets its flag without opening a b-tree
// transaction (no OP_Transaction), and btree.c lockBtree only reports the
// nPage>nPageFile corruption "if( sqlite3WritableSchema(pBt->db)==0 )" —
// so with the flag set, incremental_vacuum succeeds.
func TestP8IncrVacuumWritableSchemaHeaderBeyondFile(t *testing.T) {
	// Header: page size 4096, change counter 5 == version-valid-for 5,
	// header page count 7, freelist trunk 4 count 1, largest root 3
	// (auto-vacuum on), incremental-vacuum flag on; file truncated to 5
	// pages.
	img := make([]byte, 5*4096)
	hdr := []byte{
		'S', 'Q', 'L', 'i', 't', 'e', ' ', 'f', 'o', 'r', 'm', 'a', 't', ' ', '3', 0x00,
		0x10, 0x00, // page size 4096
		0x01, 0x01, // write/read version
		0x00, 0x40, 0x20, 0x20, // reserved, max/min payload
		0x00, 0x00, 0x00, 0x05, // change counter
		0x00, 0x00, 0x00, 0x07, // header page count (7 > file 5)
		0x00, 0x00, 0x00, 0x04, // freelist trunk
		0x00, 0x00, 0x00, 0x01, // freelist count
		0x00, 0x00, 0x00, 0x03, // schema cookie
		0x00, 0x00, 0x00, 0x04, // schema format
		0x00, 0x00, 0x00, 0x00, // default cache size
		0x00, 0x00, 0x00, 0x03, // largest root page (av on)
		0x00, 0x00, 0x00, 0x01, // text encoding UTF-8
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01, // incremental-vacuum mode
	}
	copy(img, hdr)
	img[92] = 0x00
	img[93] = 0x00
	img[94] = 0x00
	img[95] = 0x05 // version-valid-for == counter
	path := filepath.Join(t.TempDir(), "deser.db")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Schema reads report corruption (C: page_count/auto_vacuum rc=11).
	if r := db.Query("PRAGMA page_count;"); r.Error == nil {
		t.Fatalf("page_count on truncated-header image should fail without writable_schema")
	}
	// writable_schema=ON + incremental_vacuum(10): C expects {0 {}}.
	if r := db.Exec("PRAGMA writable_schema=ON;"); r.Error != nil {
		t.Fatalf("writable_schema=ON must succeed (flag-only statement): %v", r.Error)
	}
	if r := db.Query("PRAGMA incremental_vacuum(10);"); r.Error != nil {
		t.Fatalf("incremental_vacuum with writable_schema=ON must succeed, got: %v", r.Error)
	}
}
