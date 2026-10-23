package frigolite

import (
	"strings"
	"testing"
)

func TestAltertabColumnRenameAmbiguousTriggerReference(t *testing.T) {
	db := setupDBWithoutEcho(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(x, y)",
		"CREATE TABLE t2(a, b)",
		"CREATE TABLE log(c)",
		"CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN INSERT INTO log SELECT y FROM t1, t2; END",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if res := db.Exec("INSERT INTO t1 VALUES(1,2)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Exec("ALTER TABLE t2 RENAME b TO y")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "error in trigger tr1 after rename: ambiguous column name: y") {
		t.Fatalf("column rename error = %v, want trigger ambiguity", res.Error)
	}
	cols := db.Query("PRAGMA table_info(t2)")
	if cols.Error != nil || len(cols.Rows) != 2 || cols.Rows[1][1] != "b" {
		t.Fatalf("t2 columns after failed rename = %#v, err=%v; want column b preserved", cols.Rows, cols.Error)
	}
}
