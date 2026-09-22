package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestHookVetoPin pins the sqlite3_commit_hook veto contract
// (test/hook.test 3.x): a commit hook returning nonzero fails the commit
// with "constraint failed" (SQLITE_CONSTRAINT_COMMITHOOK) and rolls back
// the statement's (autocommit) or the whole transaction's (explicit
// COMMIT) changes — vdbeCommit invokes db->xCommitCallback BEFORE the
// btree commit phases (src/vdbeaux.c:2978-2982), so the vetoed commit
// never persists. The engine gap this pins: a vetoed autocommit INSERT
// previously kept its in-memory dirty pages, persisting on the NEXT
// statement's commit.
func TestHookVetoPin(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE t2(a,b)")
	must("INSERT INTO t2 VALUES(1,2)")

	// Autocommit veto: the row is gone and the failure is reported.
	db.SetCommitHook(func() int { return 1 })
	if r := db.Exec("INSERT INTO t2 VALUES(5,6)"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "constraint failed") {
		t.Fatalf("vetoed autocommit: want constraint failed, got %v", r.Error)
	}
	db.SetCommitHook(nil)
	if r := db.Query("SELECT count(*) FROM t2"); r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("vetoed autocommit insert must not persist, got %v / %v", r.Rows, r.Error)
	}

	// Non-vetoing hook allows the commit.
	db.SetCommitHook(func() int { return 0 })
	must("INSERT INTO t2 VALUES(6,7)")
	if r := db.Query("SELECT count(*) FROM t2"); r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("allowed commit must persist, got %v / %v", r.Rows, r.Error)
	}

	// Explicit COMMIT veto: the whole transaction rolls back and the
	// transaction ends (a later COMMIT reports "no transaction is active").
	db.SetCommitHook(func() int { return 1 })
	if r := db.Exec("BEGIN; INSERT INTO t2 VALUES(9,9); COMMIT"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "constraint failed") {
		t.Fatalf("vetoed COMMIT: want constraint failed, got %v", r.Error)
	}
	db.SetCommitHook(nil)
	if r := db.Query("SELECT count(*) FROM t2"); r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("vetoed COMMIT must roll the transaction back, got %v / %v", r.Rows, r.Error)
	}
	if r := db.Exec("COMMIT"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "no transaction is active") {
		t.Fatalf("transaction must be closed after a vetoed COMMIT, got %v", r.Error)
	}
}
