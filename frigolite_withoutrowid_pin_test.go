package frigolite

import (
	"testing"
)

// Pins for WITHOUT ROWID conflict handling (without_rowid3/without_rowid4).

// UPDATE OR IGNORE on a WITHOUT ROWID table must skip rows whose new PK
// conflicts with an existing row — every WR cell shares the synthetic RowID
// 0, so the rowid-based self-exclusion never fires there and the conflict
// check needs the OLD-PK-key identity (without_rowid4-6.2's trigger cascades
// wrote duplicate PKs before the fix).
func TestWithoutRowidUpdateIgnoreSkipsPKConflict(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t(a PRIMARY KEY, b) WITHOUT ROWID",
		"INSERT INTO t VALUES (1,2), (3,4)",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	if res := db.Exec("UPDATE OR IGNORE t SET a = 1 WHERE a = 3"); res.Error != nil {
		t.Fatalf("update error: %v", res.Error)
	}
	r := db.Query("SELECT * FROM t")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	want := "1 2 3 4"
	if got := flattenResult(r); got != want {
		t.Fatalf("got [%s], want [%s] — the conflicting row must keep (3,4)", got, want)
	}
}

// without_rowid3-3.1.x: an ON UPDATE CASCADE into a WITHOUT ROWID child must
// rewrite the child row as an index-leaf cell with PK-first storage order — a
// table-leaf cell in the WR index btree reads back as "database disk image is
// malformed".
func TestWithoutRowidCascadeUpdateChildStorage(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"PRAGMA foreign_keys = ON",
		`CREATE TABLE ab(a PRIMARY KEY, b) WITHOUT rowid`,
		`CREATE TABLE cd(c PRIMARY KEY REFERENCES ab ON UPDATE CASCADE ON DELETE CASCADE, d) WITHOUT rowid`,
		"INSERT INTO ab VALUES(1, 'b')",
		"INSERT INTO cd VALUES(1, 'd')",
	} {
		if res := db.Exec(s); res.Error != nil {
			t.Fatalf("exec error: %v\n  sql: %s", res.Error, s)
		}
	}
	if res := db.Exec("UPDATE ab SET a = 5"); res.Error != nil {
		t.Fatalf("cascade update error: %v", res.Error)
	}
	r := db.Query("SELECT * FROM cd")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if got := flattenResult(r); got != "5 d" {
		t.Fatalf("cd got [%s], want [5 d]", got)
	}
}
