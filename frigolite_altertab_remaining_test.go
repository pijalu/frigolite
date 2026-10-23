package frigolite

import (
	"strings"
	"testing"
)

func TestAltertabRemainingLikelihoodAndRenameValidation(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE a(b)",
		"CREATE VIEW c AS SELECT NULL INTERSECT SELECT NULL ORDER BY likelihood(NULL, (d, (SELECT c)))",
	} {
		if got := db.Exec(sql); got.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, got.Error)
		}
	}
	if got := db.Query("SELECT likelihood(NULL, (d, (SELECT c)))"); got.Error == nil || !strings.Contains(got.Error.Error(), "second argument to likelihood()") {
		t.Fatalf("likelihood diagnostic = %v, want constant-probability error", got.Error)
	}
	if got := db.Query("SELECT * FROM c"); got.Error == nil || !strings.Contains(got.Error.Error(), "1st ORDER BY term does not match any column") {
		t.Fatalf("view diagnostic = %v, want invalid ORDER BY error", got.Error)
	}
	if got := db.Exec("ALTER TABLE a RENAME TO e"); got.Error == nil || !strings.Contains(got.Error.Error(), "error in view c: 1st ORDER BY term") {
		t.Fatalf("rename diagnostic = %v, want invalid-view error", got.Error)
	}
}

func TestAltertabRemainingRenameBrokenTriggerViaView(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a, b)",
		"CREATE TRIGGER AFTER INSERT ON t1 BEGIN INSERT INTO v1 VALUES(new.a) ON CONFLICT(a) DO NOTHING; END",
		"CREATE VIEW v1 AS SELECT * FROM nosuchtable",
	} {
		if got := db.Exec(sql); got.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, got.Error)
		}
	}
	got := db.Exec("ALTER TABLE t1 RENAME TO t2")
	if got.Error == nil || !strings.Contains(got.Error.Error(), "error in trigger AFTER: no such table: main.nosuchtable") {
		t.Fatalf("rename diagnostic = %v, want broken-trigger-view error", got.Error)
	}
}

func TestAltertabRemainingSystemVirtualTableRename(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	got := db.Exec("ALTER TABLE json_each RENAME TO t4")
	if got.Error == nil || !strings.Contains(got.Error.Error(), "table json_each may not be altered") {
		t.Fatalf("rename diagnostic = %v, want protected-table error", got.Error)
	}
}

func TestAltertabRemainingMalformedTriggerRename(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(x)",
		"CREATE TRIGGER r1 BEFORE INSERT ON t1 BEGIN UPDATE t1 SET x=x FROM (SELECT*); END",
	} {
		if got := db.Exec(sql); got.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, got.Error)
		}
	}
	got := db.Exec("ALTER TABLE t1 RENAME TO x")
	if got.Error == nil || !strings.Contains(got.Error.Error(), "error in trigger r1: no tables specified") {
		t.Fatalf("rename diagnostic = %v, want trigger reparse error", got.Error)
	}
}

func TestAltertabRemainingRenameColumnBrokenTrigger(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a TEXT)",
		"INSERT INTO t1(a) VALUES('abc'),('def'),(NULL)",
		"CREATE TABLE t2(b TEXT)",
		"CREATE TRIGGER r3 AFTER INSERT ON t1 BEGIN UPDATE t2 SET (b,a)=(SELECT 1) FROM t1 JOIN t2 ON (SELECT * FROM (SELECT a)); END",
	} {
		if got := db.Exec(sql); got.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, got.Error)
		}
	}
	got := db.Exec("ALTER TABLE t1 RENAME COLUMN a TO b")
	if got.Error == nil || !strings.Contains(got.Error.Error(), "error in trigger r3 after rename: no such column: a") {
		t.Fatalf("rename diagnostic = %v, want broken-trigger-column error", got.Error)
	}
	got = db.Query("SELECT quote(a) FROM t1 ORDER BY +a")
	if got.Error != nil || len(got.Rows) != 3 {
		t.Fatalf("original table after failed rename: rows=%v err=%v", got.Rows, got.Error)
	}
}
