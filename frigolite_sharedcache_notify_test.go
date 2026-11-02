package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSharedCacheNotifyNative ports notify1.test's observable contracts to
// the pure-Go API (frigolite.Open / Exec / UnlockNotify): shared-cache
// table-level locks ("database table is locked", with the table name from
// the statement path), the single-writer begin-transaction rule (the bare
// form), unlock-notify registration with immediate fire, and the deadlock
// walk. btree.c querySharedCacheTableLock + notify.c sqlite3_unlock_notify
// are the ground truth; notify1-2.3.6..11's exact firing order pins the
// blocked-list walk.
func TestSharedCacheNotifyNative(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db2Path := filepath.Join(dir, "test2.db")
	db3Path := filepath.Join(dir, "test3.db")

	EnableSharedCache(true)
	defer EnableSharedCache(false)

	open := func(path string) *DB {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		return db
	}
	mustExec := func(db *DB, sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
	}

	// --- notify1-1.x: the warm body -------------------------------------
	db := open(dbPath)
	defer db.Close()
	db2 := open(dbPath)
	defer db2.Close()

	mustExec(db, "CREATE TABLE t1(a, b)")

	// db holds the write transaction on t1; db2's write fails on the
	// begin-transaction path — the BARE form (btree.c:3638, no table name).
	mustExec(db, "BEGIN")
	mustExec(db, "INSERT INTO t1 VALUES(1, 2)")
	if r := db2.Exec("INSERT INTO t1 VALUES(3, 4)"); r.Error == nil || r.Error.Error() != "database table is locked" {
		t.Fatalf("db2 write under db's txn: got %v, want bare %q", r.Error, "database table is locked")
	}

	// The unlock-notify callback waits for db's transaction; it must NOT
	// fire while the transaction is open (notify1-1.4).
	fired := false
	if err := db2.UnlockNotify(func() { fired = true }); err != nil {
		t.Fatalf("unlock_notify register: %v", err)
	}
	if fired {
		t.Fatalf("unlock-notify fired while the blocking transaction is open")
	}
	if db2.engine.SharableBlocked() == 0 {
		t.Fatalf("db2 should still report itself blocked on db")
	}

	// COMMIT ends the blocking transaction: the callback fires (notify1-1.5).
	mustExec(db, "COMMIT")
	if !fired {
		t.Fatalf("unlock-notify did not fire at COMMIT")
	}
	mustExec(db2, "INSERT INTO t1 VALUES(3, 4)")

	// The statement path carries the table name: a reader of t1 under
	// another connection's WRITE lock on t1 (notify1-2.2.2's named form).
	// A read of a DIFFERENT table is unaffected — shared cache allows one
	// writer plus readers of other tables (notify1-2.2.1), so t2 exists
	// with its data before the conflicting transaction opens.
	mustExec(db2, "CREATE TABLE t2(a, b)")
	mustExec(db2, "INSERT INTO t2 VALUES('I', 'II')")
	mustExec(db, "BEGIN")
	mustExec(db, "INSERT INTO t1 VALUES(5, 6)")
	if r := db2.Exec("SELECT * FROM t1"); r.Error == nil || r.Error.Error() != "database table is locked: t1" {
		t.Fatalf("db2 read of t1 under db's write lock: got %v, want %q", r.Error, "database table is locked: t1")
	}
	if r := db2.Exec("SELECT * FROM t2"); r.Error != nil {
		t.Fatalf("db2 read of t2: %v", r.Error)
	}
	mustExec(db, "COMMIT")

	// An unlock-notify registration on an UNBLOCKED connection fires
	// immediately (notify1-1.11/1.12's no-blocker path).
	immediate := false
	if err := db.UnlockNotify(func() { immediate = true }); err != nil {
		t.Fatalf("unblocked unlock_notify: %v", err)
	}
	if !immediate {
		t.Fatalf("unblocked unlock_notify must fire the callback immediately")
	}

	// --- notify1-2.3.x: three connections, three files, deadlock walk ----
	// (the TCL section forcedeletes the files first.)
	for _, conn := range []*DB{db, db2} {
		if err := conn.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	for _, p := range []string{dbPath, db2Path, db3Path} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", p, err)
		}
	}
	db = open(dbPath)
	db2 = open(dbPath)
	db3 := open(dbPath)
	defer func() {
		db.Close()
		db2.Close()
		db3.Close()
	}()
	for _, conn := range []*DB{db, db2, db3} {
		mustExec(conn, "ATTACH '"+db2Path+"' AS aux2")
		mustExec(conn, "ATTACH '"+db3Path+"' AS aux3")
	}
	mustExec(db, "CREATE TABLE main.t1(a, b)")
	mustExec(db, "CREATE TABLE aux2.t2(a, b)")
	mustExec(db, "CREATE TABLE aux3.t3(a, b)")
	mustExec(db, "BEGIN; INSERT INTO t1 VALUES(1, 2)")
	mustExec(db2, "BEGIN; INSERT INTO t2 VALUES(1, 2)")
	mustExec(db3, "BEGIN; INSERT INTO t3 VALUES(1, 2)")

	// The lock cycle: db wants t2 (db2's write), db2 wants t3 (db3's write),
	// db3 wants t1 (db's write).
	if r := db.Exec("SELECT * FROM t2"); r.Error == nil || r.Error.Error() != "database table is locked: t2" {
		t.Fatalf("db read t2: got %v, want locked: t2", r.Error)
	}
	if r := db2.Exec("SELECT * FROM t3"); r.Error == nil || r.Error.Error() != "database table is locked: t3" {
		t.Fatalf("db2 read t3: got %v, want locked: t3", r.Error)
	}
	if r := db3.Exec("SELECT * FROM t1"); r.Error == nil || r.Error.Error() != "database table is locked: t1" {
		t.Fatalf("db3 read t1: got %v, want locked: t1", r.Error)
	}

	// Registration order shapes the wait graph: db and db2 register fine
	// (their chains end at a not-yet-registered blocker); db3's registration
	// walks db -> db2 -> db3 and reports the deadlock (notify1-2.3.6/2.3.7).
	var lUnlock []string
	if err := db.UnlockNotify(func() { lUnlock = append(lUnlock, "db") }); err != nil {
		t.Fatalf("db unlock_notify: %v", err)
	}
	if err := db2.UnlockNotify(func() { lUnlock = append(lUnlock, "db2") }); err != nil {
		t.Fatalf("db2 unlock_notify: %v", err)
	}
	if err := db3.UnlockNotify(func() { lUnlock = append(lUnlock, "db3") }); err == nil || err.Error() != "database is deadlocked" {
		t.Fatalf("db3 unlock_notify: got %v, want %q", err, "database is deadlocked")
	}
	if len(lUnlock) != 0 {
		t.Fatalf("no callbacks should have fired before a commit, got %v", lUnlock)
	}

	// db's COMMIT: nobody waits on db yet, and db3's mere block clears
	// (notify1-2.3.8: lUnlock stays empty).
	mustExec(db, "COMMIT")
	if len(lUnlock) != 0 {
		t.Fatalf("after db COMMIT: got %v, want empty", lUnlock)
	}
	// db3 re-registers: its block was cleared by db's COMMIT, so the
	// callback fires immediately (notify1-2.3.9: lUnlock == {db3}).
	if err := db3.UnlockNotify(func() { lUnlock = append(lUnlock, "db3") }); err != nil {
		t.Fatalf("db3 re-register: %v", err)
	}
	if len(lUnlock) != 1 || lUnlock[0] != "db3" {
		t.Fatalf("after db3 re-register: got %v, want [db3]", lUnlock)
	}
	// db2's COMMIT fires db's callback (notify1-2.3.10: {db3 db}).
	mustExec(db2, "COMMIT")
	if len(lUnlock) != 2 || lUnlock[0] != "db3" || lUnlock[1] != "db" {
		t.Fatalf("after db2 COMMIT: got %v, want [db3 db]", lUnlock)
	}
	// db3's COMMIT fires db2's callback (notify1-2.3.11: {db3 db db2}).
	mustExec(db3, "COMMIT")
	if len(lUnlock) != 3 || lUnlock[0] != "db3" || lUnlock[1] != "db" || lUnlock[2] != "db2" {
		t.Fatalf("after db3 COMMIT: got %v, want [db3 db db2]", lUnlock)
	}
}
