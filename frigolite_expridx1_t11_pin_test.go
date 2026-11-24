package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestExpridx1EnginePins locks the engine contracts behind expridx1: an
// imposter table's rootpage rewrite distorts an expression index, a correlated
// expression bound drives the stale-key seek, a WITHOUT ROWID secondary index
// serves the same seek from its PK tail, and integrity_check reports stale
// floating-point keys within two ULPs as imprecise rather than missing.
func TestExpridx1EnginePins(t *testing.T) {
	open := func(t *testing.T, setup ...string) *DB {
		t.Helper()
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		for _, stmt := range setup {
			if res := db.Exec(stmt); res.Error != nil {
				t.Fatalf("setup %q: %v", stmt, res.Error)
			}
		}
		return db
	}
	column := func(t *testing.T, db *DB, sql string) string {
		t.Helper()
		res := db.Query(sql)
		if res.Error != nil {
			t.Fatalf("query %q: %v", sql, res.Error)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Fatalf("query %q rows = %v, want one cell", sql, res.Rows)
		}
		return fmt.Sprint(res.Rows[0][0])
	}
	flat := func(t *testing.T, db *DB, sql string) string {
		t.Helper()
		res := db.Query(sql)
		if res.Error != nil {
			t.Fatalf("query %q: %v", sql, res.Error)
		}
		var vals []string
		for _, row := range res.Rows {
			vals = append(vals, fmt.Sprint(row[0]))
		}
		return "[" + strings.Join(vals, " ") + "]"
	}

	const rewrite = `
PRAGMA writable_schema=ON;
UPDATE sqlite_schema SET rootpage=(SELECT rootpage FROM sqlite_schema WHERE name='%s') WHERE name='x1';
PRAGMA writable_schema=OFF;`

	t.Run("correlated expression seek", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE t1(a INTEGER PRIMARY KEY, b REAL)`,
			`INSERT INTO t1 VALUES(10,10.0),(15,15.0),(20,20.0),(25,25.0),(30,30.0)`,
			`CREATE INDEX i1 ON t1((b+0.0))`,
			`CREATE TABLE x1(b, rowid, PRIMARY KEY(b,rowid)) WITHOUT ROWID`,
			fmt.Sprintf(rewrite, "i1"),
			`UPDATE x1 SET b=21.0 WHERE rowid=20`,
		)
		got := column(t, db, `SELECT count(*) FROM (SELECT rowid FROM t1 AS o NOT INDEXED WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE +a=o.a AND b+0.0=o.b+0.0))`)
		if got != "1" {
			t.Fatalf("stale expression index probe count = %s, want 1", got)
		}
		got = column(t, db, `SELECT rowid FROM t1 AS o NOT INDEXED WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE +a=o.a AND b+0.0=o.b+0.0)`)
		if got != "20" {
			t.Fatalf("stale expression index probe = %s, want 20", got)
		}
	})

	t.Run("without rowid covering expression seek", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE t1(a, b, c, PRIMARY KEY(a,b)) WITHOUT ROWID`,
			`INSERT INTO t1 VALUES(1,2,'x'),(2,3,'y'),(3,4,'z')`,
			`CREATE INDEX t1c ON t1(+c)`,
			`CREATE TABLE x1(c, a, b, PRIMARY KEY(c,a,b)) WITHOUT ROWID`,
			fmt.Sprintf(rewrite, "t1c"),
			`UPDATE x1 SET c='stale' WHERE a=2`,
		)
		got := column(t, db, `SELECT a FROM t1 AS o NOT INDEXED WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE +a=o.a AND +b=o.b AND +c=o.c)`)
		if got != "2" {
			t.Fatalf("stale WITHOUT ROWID index probe = %s, want 2", got)
		}
	})

	t.Run("imprecise float integrity diagnostics", func(t *testing.T) {
		db := open(t,
			`CREATE TABLE z1(a INTEGER PRIMARY KEY, b)`,
			`CREATE INDEX z1b ON z1(b+0.0)`,
			`INSERT INTO z1 VALUES(1,1.0),(2,4.0),(3,4.0),(4,4.0),(5,4.0),(6,4.0),(7,4.0),(8,1.0)`,
			`CREATE TABLE x1(b, a, PRIMARY KEY(b,a)) WITHOUT ROWID`,
			fmt.Sprintf(rewrite, "z1b"),
			`UPDATE x1 SET b=4.000000000000001 WHERE a=2`,
			`UPDATE x1 SET b=4.000000000000002 WHERE a=3`,
			`UPDATE x1 SET b=4.000000000000003 WHERE a=4`,
			`UPDATE x1 SET b=3.9999999999999996 WHERE a=5`,
			`UPDATE x1 SET b=3.9999999999999992 WHERE a=6`,
			`UPDATE x1 SET b=3.9999999999999988 WHERE a=7`,
		)
		got := flat(t, db, `PRAGMA integrity_check`)
		want := "[index z1b stores an imprecise floating-point value for row 2 index z1b stores an imprecise floating-point value for row 3 row 4 missing from index z1b index z1b stores an imprecise floating-point value for row 5 index z1b stores an imprecise floating-point value for row 6 row 7 missing from index z1b]"
		if got != want {
			t.Fatalf("integrity_check = %s, want %s", got, want)
		}
	})
}
