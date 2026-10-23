package frigolite

import "testing"

func TestAlterColumnRenameMixedSingleDoubleQuotedName(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if res := db.Exec(`CREATE TABLE t1('a'"b",c); CREATE INDEX i1 ON t1('a'); INSERT INTO t1 VALUES(1,2),(3,4);`); res.Error != nil {
		t.Fatal(res.Error)
	}
	columns := db.Query(`PRAGMA table_info(t1)`)
	t.Logf("columns: %#v", columns.Rows)
	if res := db.Exec(`ALTER TABLE t1 RENAME COLUMN a TO x; PRAGMA integrity_check`); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Query(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='t1'`)
	const want = `CREATE TABLE t1("x" "b",c)`
	if res.Error != nil || len(res.Rows) != 1 || res.Rows[0][0] != want {
		t.Fatalf("stored table SQL = %#v, err=%v; want %q", res.Rows, res.Error, want)
	}
}

func TestAlterColumnRenameUpdatesTriggerUpdateOf(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE c(x); INSERT INTO c VALUES(0);`,
		`CREATE TABLE t6("col a", "col b", "col c");`,
		`CREATE TRIGGER zzz AFTER UPDATE OF "col a", "col c" ON t6 BEGIN UPDATE c SET x=x+1; END;`,
		`INSERT INTO t6 VALUES(0,0,0); UPDATE t6 SET "col c"=1;`,
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if res := db.Exec(`ALTER TABLE t6 RENAME COLUMN "col c" TO "col 3"`); res.Error != nil {
		t.Fatal(res.Error)
	}
	stored := db.Query(`SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='zzz'`)
	const want = `CREATE TRIGGER zzz AFTER UPDATE OF "col a", "col 3" ON t6 BEGIN UPDATE c SET x=x+1; END`
	if stored.Error != nil || len(stored.Rows) != 1 || stored.Rows[0][0] != want {
		t.Fatalf("stored trigger SQL = %#v, err=%v; want %q", stored.Rows, stored.Error, want)
	}
	res := db.Query(`UPDATE t6 SET "col 3"=0; SELECT x FROM c`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(2) {
		t.Fatalf("trigger count after renamed-column update = %#v, want 2", res.Rows)
	}
}
