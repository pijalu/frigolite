// Native Go tests pinning the WITHOUT ROWID index-btree storage contracts
// introduced by the WR write-path port (PK-first index-leaf cells) and the
// read/rewrite parity fixes: interior-root scans, join right-side scans,
// ALTER TABLE DROP COLUMN rebuilds, identity-addressed DML (REPLACE /
// UPDATE OR REPLACE / DELETE), and FK parent scans. Each test drives
// frigolite.Open / Exec / Query directly and mirrors a TCL-suite shape.
//
// Run with: go test -run TestNativeWR ./...
package frigolite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// wrNativeOpen opens a file-backed database in a temp dir (page-split
// scenarios need real pager-backed files, not :memory:).
func wrNativeOpen(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "wr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// wrNativeRows runs a query and flattens the result rows (space-separated
// values, " | " between rows) for compact want comparison.
func wrNativeRows(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %q: %v", sql, r.Error)
	}
	var rows []string
	for _, row := range r.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "{}"
			} else {
				cells[i] = fmt.Sprintf("%v", v)
			}
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return strings.Join(rows, " | ")
}

// TestNativeWRInteriorRootScanRemaps: a WITHOUT ROWID table whose root has
// split to an interior index page (0x02) must remap its PK-first records to
// declared order exactly like an index-leaf root (0x0a) — a non-identity PK
// (b not the first declared column) must not leak into WHERE or output.
func TestNativeWRInteriorRootScanRemaps(t *testing.T) {
	db := wrNativeOpen(t)
	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
	}
	mustExec("PRAGMA page_size = 512")
	mustExec("CREATE TABLE t1(a, b, c, PRIMARY KEY(b)) WITHOUT ROWID")
	// ~100 rows at page_size 512 splits the root to an interior index page.
	mustExec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<200) " +
		"INSERT INTO t1(a, b, c) SELECT i, i*10, i*100 FROM s")
	for _, probe := range []struct{ where, want string }{
		{"a=5", "5 50 500"},
		{"b=500", "50 500 5000"},
		{"c=1900", "19 190 1900"},
	} {
		if got := wrNativeRows(t, db, "SELECT * FROM t1 WHERE "+probe.where); got != probe.want {
			t.Errorf("SELECT * WHERE %s = %q, want %q", probe.where, got, probe.want)
		}
	}
	if got := wrNativeRows(t, db, "SELECT count(*) FROM t1"); got != "200" {
		t.Errorf("count = %q, want 200", got)
	}
}

// TestNativeWRJoinRightScanDeclaredOrder: a WITHOUT ROWID table scanned as a
// join's right side must expose declared-order values in its row maps
// (without_rowid/join6-5.2: SELECT o FROM tx NATURAL JOIN tx).
func TestNativeWRJoinRightScanDeclaredOrder(t *testing.T) {
	db := wrNativeOpen(t)
	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE tx(a, b, c, d, e, f, g, h, i, j, k, l, m, n, o PRIMARY KEY) WITHOUT ROWID")
	mustExec("INSERT INTO tx VALUES(1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)")
	if got := wrNativeRows(t, db, "SELECT o FROM tx NATURAL JOIN tx"); got != "15" {
		t.Errorf("NATURAL JOIN o = %q, want 15", got)
	}
	// The base table scanned with the WR table on the LEFT as well.
	if got := wrNativeRows(t, db, "SELECT a FROM tx tx1 NATURAL JOIN tx tx2"); got != "1" {
		t.Errorf("NATURAL JOIN a = %q, want 1", got)
	}
}

// TestNativeWRDropColumnRebuild: ALTER TABLE DROP COLUMN on a WITHOUT ROWID
// table rewrites rows in the OLD storage layout and re-inserts them in the
// NEW layout as PK-keyed index cells (never collapsing rows or nulling PKs).
func TestNativeWRDropColumnRebuild(t *testing.T) {
	t.Run("CompositeNonIdentityPK", func(t *testing.T) {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		mustExec("CREATE TABLE t1(a, b, c, d, PRIMARY KEY(b, c)) WITHOUT ROWID")
		mustExec("INSERT INTO t1 VALUES(1, 2, 3, 4)")
		mustExec("INSERT INTO t1 VALUES(5, 6, 7, 8)")
		mustExec("ALTER TABLE t1 DROP COLUMN d")
		if got := wrNativeRows(t, db, "SELECT * FROM t1"); got != "1 2 3 | 5 6 7" {
			t.Errorf("after drop: %q, want %q", got, "1 2 3 | 5 6 7")
		}
	})
	t.Run("DuplicateCollatePK", func(t *testing.T) {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		// alterdropcol-7: PRIMARY KEY(a COLLATE nocase, a) — the duplicate
		// PK element must not collapse the rebuild into one NULL row.
		mustExec("CREATE TABLE t1(a, b, c, PRIMARY KEY(a COLLATE nocase, a)) WITHOUT ROWID")
		mustExec("INSERT INTO t1 VALUES(1, 2, 3)")
		mustExec("INSERT INTO t1 VALUES(4, 5, 6)")
		mustExec("ALTER TABLE t1 DROP COLUMN c")
		if got := wrNativeRows(t, db, "SELECT * FROM t1"); got != "1 2 | 4 5" {
			t.Errorf("after drop: %q, want %q", got, "1 2 | 4 5")
		}
	})
	t.Run("BulkThenDrop", func(t *testing.T) {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		// alterdropcol-9: the bulk rebuild must stay O(rows) and preserve
		// every row (pre-fix: all 50000 rows collapsed into one NULL row).
		mustExec("CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c) WITHOUT ROWID")
		mustExec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i<5000) " +
			"INSERT INTO t1(a, b, c) SELECT i, 123, 456 FROM s")
		mustExec("ALTER TABLE t1 DROP COLUMN b")
		if got := wrNativeRows(t, db, "SELECT count(*), c FROM t1 GROUP BY c"); got != "5000 456" {
			t.Errorf("after bulk drop: %q, want %q", got, "5000 456")
		}
	})
}

// TestNativeWRConflictIdentityDML: REPLACE INTO, UPDATE OR REPLACE, and
// DELETE address WITHOUT ROWID rows by PRIMARY KEY, never by the synthetic
// RowID 0 (pre-fix: REPLACE deleted every row; UPDATE OR REPLACE over-deleted
// conflicting rows; DELETE matched nothing).
func TestNativeWRConflictIdentityDML(t *testing.T) {
	newDB := func(t *testing.T) *DB {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		mustExec("CREATE TABLE t1(a, b, c, d, PRIMARY KEY(c, a)) WITHOUT ROWID")
		mustExec("CREATE INDEX t1bd ON t1(b, d)")
		mustExec("INSERT INTO t1 VALUES('journal','sherman','ammonia','helena')")
		mustExec("INSERT INTO t1 VALUES('dynamic','juliet','flipper','command')")
		mustExec("INSERT INTO t1 VALUES('journal','sherman','gamma','patriot')")
		mustExec("INSERT INTO t1 VALUES('arctic','sleep','ammonia','helena')")
		return db
	}
	t.Run("REPLACEKeepsNonConflictingRows", func(t *testing.T) {
		db := newDB(t)
		if r := db.Exec("REPLACE INTO t1 VALUES('dynamic','phone','flipper','harvard')"); r.Error != nil {
			t.Fatalf("replace: %v", r.Error)
		}
		want := "arctic sleep ammonia helena | journal sherman ammonia helena | " +
			"dynamic phone flipper harvard | journal sherman gamma patriot"
		if got := wrNativeRows(t, db, "SELECT * FROM t1 ORDER BY c, a"); got != want {
			t.Errorf("after REPLACE: %q, want %q", got, want)
		}
	})
	t.Run("DeleteByNonPKColumn", func(t *testing.T) {
		db := newDB(t)
		if r := db.Exec("DELETE FROM t1 WHERE a='journal'"); r.Error != nil {
			t.Fatalf("delete: %v", r.Error)
		}
		if got := wrNativeRows(t, db, "SELECT count(*) FROM t1"); got != "2" {
			t.Errorf("after DELETE: count %q, want 2", got)
		}
	})
	t.Run("UpdateOrReplaceDeletesOnlyConflict", func(t *testing.T) {
		db := newDB(t)
		if r := db.Exec("UPDATE OR REPLACE t1 SET a='journal' WHERE c='flipper'"); r.Error != nil {
			t.Fatalf("update or replace: %v", r.Error)
		}
		// No uniqueness conflict on the new PK (c,a)=(flipper,journal):
		// every row survives and the targeted row takes the new value
		// (verified against the sqlite3 oracle).
		want := "arctic sleep ammonia helena | journal sherman ammonia helena | " +
			"journal juliet flipper command | journal sherman gamma patriot"
		if got := wrNativeRows(t, db, "SELECT a, b, c, d FROM t1 ORDER BY c, a"); got != want {
			t.Errorf("after UPDATE OR REPLACE: %q, want %q", got, want)
		}
	})
	t.Run("MultiConflictReplaceOrder", func(t *testing.T) {
		// hook2-2.1.5: REPLACE with BOTH a UNIQUE-index conflict and a PK
		// conflict deletes the index-conflict row first, then the PK row.
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		mustExec("CREATE TABLE t2(a DEFAULT 4, b, c, PRIMARY KEY(b, c)) WITHOUT ROWID")
		mustExec("CREATE UNIQUE INDEX t2a ON t2(a)")
		mustExec("INSERT INTO t2(b, c) VALUES(1, 1)")
		mustExec("INSERT INTO t2 VALUES(31, 32, 33)")
		if r := db.Exec("REPLACE INTO t2(c, b) VALUES(33, 32)"); r.Error != nil {
			t.Fatalf("replace: %v", r.Error)
		}
		// Both conflicts (UNIQUE-index row a=4 and PK row b,c=32,33) are
		// deleted; only the new row remains (hook2-2.1.5 contract).
		want := "4 32 33"
		if got := wrNativeRows(t, db, "SELECT a, b, c FROM t2 ORDER BY b"); got != want {
			t.Errorf("after multi-conflict REPLACE: %q, want %q", got, want)
		}
	})
}

// TestNativeWRFKParentScan: FK parent lookups and self-referential scans over
// WITHOUT ROWID tables match declared-order values (fkey8: a self-referential
// FK(b,c) REFERENCES t2(d,e) finds its parent row; gencol1: ON DELETE CASCADE
// fires through a generated-column child FK).
func TestNativeWRFKParentScan(t *testing.T) {
	t.Run("SelfReferentialParent", func(t *testing.T) {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		mustExec("PRAGMA foreign_keys=ON")
		mustExec("CREATE TABLE t2(a PRIMARY KEY, b, c, d, e, FOREIGN KEY(b, c) REFERENCES t2(d, e)) WITHOUT ROWID")
		mustExec("CREATE UNIQUE INDEX idx ON t2(d, e)")
		mustExec("INSERT INTO t2 VALUES(1, 'one', 'one', 'one', 'one')")
		mustExec("INSERT INTO t2 VALUES(2, 'one', 'one', 'one', NULL)")
		if got := wrNativeRows(t, db, "SELECT count(*) FROM t2"); got != "2" {
			t.Errorf("row count = %q, want 2", got)
		}
	})
	t.Run("CascadeThroughGeneratedChild", func(t *testing.T) {
		db := wrNativeOpen(t)
		mustExec := func(sql string) {
			t.Helper()
			if r := db.Exec(sql); r.Error != nil {
				t.Fatalf("exec %q: %v", sql, r.Error)
			}
		}
		mustExec("PRAGMA foreign_keys=ON")
		mustExec("CREATE TABLE t1(gcb AS (b*1), a INTEGER PRIMARY KEY, gcc AS (c+0), b UNIQUE, gca AS (1*a+0), c UNIQUE) WITHOUT ROWID")
		mustExec("INSERT INTO t1 VALUES(1,2,3)")
		mustExec("INSERT INTO t1 VALUES(4,5,6)")
		mustExec("INSERT INTO t1 VALUES(7,8,9)")
		mustExec("CREATE TABLE t1a(gcx AS (x+0) REFERENCES t1(a) ON DELETE CASCADE, id, x, gcid AS (1*id))")
		mustExec("INSERT INTO t1a VALUES(1, 1)")
		mustExec("INSERT INTO t1a VALUES(2, 4)")
		mustExec("INSERT INTO t1a VALUES(3, 7)")
		mustExec("DELETE FROM t1 WHERE b=5")
		// Deleting t1 row b=5 (a=4) cascades to t1a id=2.
		if got := wrNativeRows(t, db, "SELECT id, x FROM t1a ORDER BY id"); got != "1 1 | 3 7" {
			t.Errorf("after cascade: %q, want %q", got, "1 1 | 3 7")
		}
	})
}

// TestNativeWRTriggerVanishedRowSkip: a BEFORE UPDATE trigger that deletes
// the row being updated makes SQLite skip that row's update silently
// (without_rowid1-10.6 — pre-fix the update resurrected the deleted row).
func TestNativeWRTriggerVanishedRowSkip(t *testing.T) {
	db := wrNativeOpen(t)
	mustExec := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %q: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t1(a, b, c UNIQUE, PRIMARY KEY(a, b)) WITHOUT ROWID")
	mustExec("INSERT INTO t1 VALUES('a', 'a', 1)")
	mustExec("INSERT INTO t1 VALUES('a', 'b', 2)")
	mustExec("INSERT INTO t1 VALUES('b', 'a', 3)")
	mustExec("INSERT INTO t1 VALUES('b', 'b', 4)")
	mustExec("CREATE TRIGGER t1_tr BEFORE UPDATE ON t1 BEGIN\n" +
		"DELETE FROM t1 WHERE a = new.a;\nEND")
	mustExec("UPDATE t1 SET c = c+1 WHERE a = 'a'")
	// The trigger (DELETE WHERE a = new.a) deletes the 'a' rows while the
	// UPDATE scans them; the pre-fix engine resurrected the deleted row,
	// yielding four rows. Only the two 'b' rows survive, per the TCL want
	// ("b a 3 b b 4").
	got := wrNativeRows(t, db, "SELECT a, b, c FROM t1 ORDER BY b, a")
	if got != "b a 3 | b b 4" {
		t.Errorf("after trigger delete during update: %q, want %q", got, "b a 3 | b b 4")
	}
}
