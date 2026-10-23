package frigolite

import (
	"testing"
)

func TestAltertabColumnRenameInAggregateOrderBy(t *testing.T) {
	db := setupDBWithoutEcho(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t5(a, b, c)",
		"CREATE VIEW vt5(x) AS SELECT group_concat(a ORDER BY b) FROM t5",
		"ALTER TABLE t5 RENAME COLUMN b TO bbb",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT sql FROM sqlite_master WHERE name='vt5'")
	if res.Error != nil || len(res.Rows) != 1 || res.Rows[0][0] != "CREATE VIEW vt5(x) AS SELECT group_concat(a ORDER BY bbb) FROM t5" {
		t.Fatalf("view SQL = %#v, err=%v; want aggregate ORDER BY bbb", res.Rows, res.Error)
	}
}

func TestAltertabColumnRenamePreservesViewAlias(t *testing.T) {
	db := setupDBWithoutEcho(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE x(f1 integer NOT NULL)",
		"CREATE VIEW y AS SELECT f1 AS f1 FROM x",
		"CREATE TRIGGER t INSTEAD OF UPDATE OF f1 ON y BEGIN UPDATE x SET f1=NEW.f1; END",
		"INSERT INTO x VALUES(1),(2),(3)",
		"ALTER TABLE x RENAME f1 TO f2",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT sql FROM sqlite_master WHERE name='y'")
	if res.Error != nil || len(res.Rows) != 1 || res.Rows[0][0] != "CREATE VIEW y AS SELECT f2 AS f1 FROM x" {
		t.Fatalf("view SQL = %#v, err=%v; want expression f2 and alias f1", res.Rows, res.Error)
	}
	trigger := db.Query("SELECT sql FROM sqlite_master WHERE name='t'")
	if trigger.Error != nil || len(trigger.Rows) != 1 || trigger.Rows[0][0] != "CREATE TRIGGER t INSTEAD OF UPDATE OF f1 ON y BEGIN UPDATE x SET f2=NEW.f1; END" {
		t.Fatalf("trigger SQL = %#v, err=%v; want UPDATE target f2", trigger.Rows, trigger.Error)
	}
	if result := db.Exec("UPDATE y SET f1='x' WHERE f1=1"); result.Error != nil {
		t.Fatal(result.Error)
	}
	rows := db.Query("SELECT f2 FROM x ORDER BY f2")
	if rows.Error != nil || len(rows.Rows) != 3 || rows.Rows[0][0] != "x" || rows.Rows[1][0] != "x" || rows.Rows[2][0] != "x" {
		t.Fatalf("x.f2 after view update = %#v, err=%v; want [x x x]", rows.Rows, rows.Error)
	}
}
