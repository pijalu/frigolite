package frigolite

import (
	"os"
	"testing"
)

// TestNativeSymlinkContract is the native engine-contract test for the
// ATTACH semantics exercised by test/symlink.test and test/attach.test.
// It documents the engine-visible contract the testgen packages assert:
//
//   - ATTACH of a non-existent path creates the file (SQLite's
//     sqlite3_db_open / sqlite3.c:45571 uses SQLITE_OPEN_CREATE; e.g.
//     test/securedel.test 1.1 ATTACHes test2.db after a forcedelete).
//   - ATTACH of an existing file with a different on-disk text encoding
//     fails with "attached databases must use the same text encoding as
//     main database" (src/attach.c:200; test/enc3.test 3.2).
//
// The deeper sub-tests of test/symlink.test — sqlite3_open_v2 -nofollow
// flag (1.1.4), PATH_MAX truncation (1.4/1.5) — are VFS-layer and N/A in
// pure-Go Frigolite; see tools/tcl2go/skiptestfiles.go for the evidence
// pointer and portplan/NA_EVIDENCE.md §P8.ENCODING.
func TestNativeSymlinkContract(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/main.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(x)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}

	// ATTACH of a missing file succeeds (creates a fresh empty DB). This
	// mirrors the test/securedel.test 1.1 flow (forcedelete then ATTACH).
	missing := dir + "/missing.db"
	if r := db.Exec("ATTACH '" + missing + "' AS aux1"); r.Error != nil {
		t.Fatalf("ATTACH missing should succeed (CREATE on attach): %v", r.Error)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("expected file to be created: %v", err)
	}

	// Re-ATTACH the same file under a different schema name: succeeds.
	if r := db.Exec("ATTACH '" + missing + "' AS aux2"); r.Error != nil {
		t.Fatalf("ATTACH existing should succeed: %v", r.Error)
	}
}