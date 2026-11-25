package frigolite

import (
	"strings"
	"testing"
)

// TestInsert17_15UniqueColumnName pins the UNIQUE constraint error text for
// the insert-17.15 harness leaf: a REPLACE whose AFTER DELETE trigger
// re-inserts a conflicting row must report the constrained index column (d),
// not the INTEGER PRIMARY KEY alias (a). Mirrors sqlite test/insert.test.
func TestInsert17_15UniqueColumnName(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The ordered insert.test fixture enables recursive triggers in an
	// earlier case (insert-17.1); that state persists through 17.15.
	if res := db.Exec(`PRAGMA recursive_triggers = true`); res.Error != nil {
		t.Fatal(res.Error)
	}
	exec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec %q: %v", sql, res.Error)
		}
	}
	exec(`CREATE TABLE t3(a INTEGER PRIMARY KEY, b INT, c INT, d INT)`)
	exec(`CREATE UNIQUE INDEX t3bpi ON t3(b) WHERE c<=d`)
	exec(`CREATE UNIQUE INDEX t3d ON t3(d)`)
	exec(`INSERT INTO t3(a,b,c,d) VALUES(1,1,1,1),(2,1,3,2),(3,4,5,6)`)
	exec(`CREATE TRIGGER t3r1 AFTER DELETE ON t3 BEGIN SELECT 'hi'; END`)
	exec(`REPLACE INTO t3(a,b,c,d) VALUES(4,4,8,9)`)
	exec(`REPLACE INTO t3(a,b,c,d) VALUES(5,1,11,2)`)
	exec(`DELETE FROM t3`)
	exec(`INSERT INTO t3(a,b,c,d) VALUES(1,1,1,1),(2,1,3,2),(3,4,5,6)`)
	exec(`DROP TRIGGER t3r1`)
	exec(`CREATE TRIGGER t3r1 AFTER DELETE ON t3 BEGIN INSERT INTO t3(b,c,d) VALUES(old.b,old.c,old.d); END`)

	res := db.Exec(`REPLACE INTO t3(a,b,c,d) VALUES(4,4,8,9)`)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "t3.b") {
		t.Fatalf("17.14: want UNIQUE t3.b, got %v", res.Error)
	}
	res = db.Exec(`REPLACE INTO t3(a,b,c,d) VALUES(5,1,11,2)`)
	if res.Error == nil {
		t.Fatal("17.15: want UNIQUE t3.d error, got success")
	}
	want := "UNIQUE constraint failed: t3.d"
	if got := res.Error.Error(); got != want {
		t.Fatalf("17.15: want %q, got %q", want, got)
	}
}

// TestInsert53ExplainOpenEphemeral pins the insert-5.3 harness leaf: EXPLAIN
// of an INSERT ... SELECT into the same table must include an OpenEphemeral
// opcode (the source is spooled before the destination is written).
func TestInsert53ExplainOpenEphemeral(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec(`CREATE TEMP TABLE t4(x); INSERT INTO t4 VALUES(1)`); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Query(`EXPLAIN INSERT INTO t4 SELECT x+2 FROM t4`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	found := false
	idx := -1
	for i, row := range res.Rows {
		if len(row) == 0 {
			continue
		}
		op := row[1]
		if s, ok := op.(string); ok && s == "OpenEphemeral" {
			found = true
			idx = i
			break
		}
	}
	if !found {
		t.Fatalf("EXPLAIN stream has no OpenEphemeral opcode:\n%+v", res.Rows)
	}
	if idx <= 0 {
		t.Fatalf("OpenEphemeral at addr %d, want > 0", idx)
	}
}
