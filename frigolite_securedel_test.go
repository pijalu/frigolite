package frigolite

import (
	"strings"
	"testing"
)

// TestNativeSecureDeleteContract is the native engine-contract test for
// PRAGMA secure_delete. It covers the engine-visible contract the
// testgen/securedel.test and testgen/securedel2.test packages assert via the
// Tcl "PRAGMA main.secure_delete=X / PRAGMA db2.secure_delete" round-trip:
//
//   - Connection default value is 2 (the SQLITE_FAST_SECURE_DELETE build
//     option equivalent; test/securedel.test DEFAULT_SECDEL=2).
//   - PRAGMA main.secure_delete=X updates only MAIN.
//   - PRAGMA db2.secure_delete=X updates only db2.
//   - PRAGMA secure_delete=X (no schema) sets every attached DB.
//   - New ATTACHed DB inherits MAIN's current value (src/attach.c:207-208).
//
// The deeper side-effect (the pager zero-fills freed pages when secure_delete
// is ON, asserted by detect_blob in test/securedel2.test) is NOT covered —
// the Frigolite pager does not implement zero-on-free. That sub-contract is
// left to a future engine milestone; the testgen/securedel2 package's
// do_test 1.5.2 / 1.6.2 failures come from detect_blob being an unsupported
// Tcl helper AND the engine side-effect being unimplemented.
func TestNativeSecureDeleteContract(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/test.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(a)"); r.Error != nil {
		t.Fatalf("create: %v", r.Error)
	}

	// Default: connection-wide default = 2 (mirrors DEFAULT_SECDEL=2 from
	// test/securedel.test ifcapable fast_secure_delete).
	r := db.Query("PRAGMA secure_delete")
	if r.Error != nil {
		t.Fatalf("default getter: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("default secure_delete: got %v want 2", r.Rows)
	}

	// PRAGMA main.secure_delete=ON updates only MAIN; getter reflects it.
	if r := db.Query("PRAGMA main.secure_delete = ON"); r.Error != nil {
		t.Fatalf("set main: %v", r.Error)
	}
	r = db.Query("PRAGMA main.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("main after ON: got %v want 1", r.Rows)
	}

	// Attach test2.db as db2; it inherits MAIN's current value (1).
	db2Path := dir + "/test2.db"
	if r := db.Exec("ATTACH '" + db2Path + "' AS db2"); r.Error != nil {
		t.Fatalf("attach: %v", r.Error)
	}
	r = db.Query("PRAGMA db2.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("db2 after attach (inherited): got %v want 1", r.Rows)
	}

	// PRAGMA main.secure_delete=FAST updates only MAIN; db2 stays at 1.
	if r := db.Query("PRAGMA main.secure_delete = FAST"); r.Error != nil {
		t.Fatalf("set main=FAST: %v", r.Error)
	}
	r = db.Query("PRAGMA main.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("main after FAST: got %v want 2", r.Rows)
	}
	r = db.Query("PRAGMA db2.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("db2 (unchanged): got %v want 1", r.Rows)
	}

	// PRAGMA secure_delete=OFF (no schema) sets every attached DB.
	if r := db.Query("PRAGMA secure_delete = OFF"); r.Error != nil {
		t.Fatalf("set all=OFF: %v", r.Error)
	}
	r = db.Query("PRAGMA main.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
		t.Fatalf("main after no-schema OFF: got %v want 0", r.Rows)
	}
	r = db.Query("PRAGMA db2.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
		t.Fatalf("db2 after no-schema OFF: got %v want 0", r.Rows)
	}

	// DETACH + re-ATTACH: new db2 inherits MAIN's current value (0).
	if r := db.Exec("DETACH db2"); r.Error != nil {
		t.Fatalf("detach: %v", r.Error)
	}
	if r := db.Exec("ATTACH '" + db2Path + "' AS db2"); r.Error != nil {
		t.Fatalf("re-attach: %v", r.Error)
	}
	r = db.Query("PRAGMA db2.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
		t.Fatalf("db2 after re-attach: got %v want 0", r.Rows)
	}

	// PRAGMA db2.secure_delete (no value) returns the value (getter-only).
	r = db.Query("PRAGMA db2.secure_delete")
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(0) {
		t.Fatalf("db2 getter: got %v want 0", r.Rows)
	}

	// Error path: invalid value.
	r = db.Query("PRAGMA main.secure_delete = 'invalid'")
	if r.Error == nil {
		t.Fatalf("expected error for invalid value, got nil")
	}
	if !strings.Contains(r.Error.Error(), "unsupported secure_delete") {
		t.Fatalf("expected 'unsupported secure_delete' error, got: %v", r.Error)
	}
}