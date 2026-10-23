package frigolite

import "testing"

func TestUniqueIndexUpdateToNull(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
CREATE TABLE t1(a UNIQUE, b UNIQUE);
CREATE TABLE t2(a UNIQUE, b, c, UNIQUE(c, b));
CREATE TABLE t3(a, b, c, UNIQUE(a), UNIQUE(b), UNIQUE(c));
CREATE TABLE t4(a, b, c, UNIQUE(a, b, c));
INSERT INTO t1 VALUES(1, 2);
INSERT INTO t1 VALUES(4.3, 5.5);
INSERT INTO t1 VALUES('reveal', 'variableness');
INSERT INTO t1 VALUES(X'123456', X'654321');
INSERT INTO t4 VALUES('xyx', 1, 1);
INSERT INTO t4 VALUES('xyx', 2, 1);
INSERT INTO t4 VALUES('uvw', 1, 1);
INSERT INTO t1 VALUES(NULL, NULL);
INSERT INTO t1 VALUES(NULL, NULL);
`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	rows := db.Query("SELECT count(*) FROM t1")
	if rows.Error != nil || rows.Rows[0][0] != int64(6) {
		t.Fatalf("source 4.7.0 row count = %#v, error %v", rows.Rows, rows.Error)
	}
	rowValues := db.Query("SELECT rowid, quote(a), quote(b) FROM t1 ORDER BY rowid")
	if rowValues.Error != nil {
		t.Fatalf("source 4.7.0 row scan: %v", rowValues.Error)
	}
	indexes := db.Query("SELECT name, rootpage FROM sqlite_master WHERE type='index' ORDER BY name")
	t.Logf("native index roots: %v", indexes.Rows)
	for _, sql := range []string{
		"INSERT INTO t4 VALUES('xyx', 1, 1)",
		"INSERT INTO t4 VALUES('xyx', 2, 1)",
		"INSERT INTO t4 VALUES('uvw', 1, 1)",
		"UPDATE t4 SET a='xyx' WHERE rowid=3",
		"UPDATE t4 SET b=1 WHERE rowid=2",
		"UPDATE t4 SET a=0, b=0, c=0",

		"INSERT INTO t1 VALUES(1, 'one')",
		"INSERT INTO t1 VALUES(4.3, 'two')",
		"INSERT INTO t1 VALUES('reveal', 'three')",
		"UPDATE t1 SET a = 1 WHERE rowid=2",
		"UPDATE t1 SET a = 4.3 WHERE rowid=3",
		"UPDATE t1 SET a = 'reveal' WHERE rowid=4",
		"UPDATE t1 SET a = X'123456' WHERE rowid=1",
	} {
		if result := db.Exec(sql); result.Error == nil {
			t.Fatalf("duplicate key operation unexpectedly succeeded: %s", sql)
		}
		if check := db.Query("SELECT count(*) FROM t1"); check.Error != nil {
			t.Fatalf("database malformed after %s: %v", sql, check.Error)
		}
	}
	if result := db.Exec("INSERT INTO t1 VALUES(X'123456', 'four')"); result.Error == nil {
		t.Fatal("duplicate blob UNIQUE key unexpectedly accepted")
	} else {
		t.Logf("duplicate blob insert error: %v", result.Error)
	}
	if result := db.Exec("UPDATE t1 SET a = NULL"); result.Error != nil {
		t.Fatalf("UPDATE indexed column to NULL: %v", result.Error)
	}
	if result := db.Query("SELECT count(*) FROM t1 WHERE a IS NULL"); result.Error != nil || len(result.Rows) != 1 || result.Rows[0][0] != int64(6) {
		t.Fatalf("NULL rows after indexed update = %#v, error %v", result.Rows, result.Error)
	}
	dropAllHarnessTables(db)
	if result := db.Query("PRAGMA integrity_check"); result.Error != nil || len(result.Rows) != 1 || result.Rows[0][0] != "ok" {
		t.Fatalf("integrity after dropping UNIQUE tables = %#v, error %v", result.Rows, result.Error)
	}
}
