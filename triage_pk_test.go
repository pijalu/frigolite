package frigolite

import (
	"reflect"
	"testing"
)

// Triage: composite PRIMARY KEY with quoted identifiers + ASC.
// TCL where4-5.2: CREATE TABLE t4(x,y,z,PRIMARY KEY('x' ASC, "y" ASC));
// then INSERT (1,1),(1,2),(1,3),(2,2) must all succeed (PK is (x,y)).
// Regression guard: frigolite rows must match the sqlite3 oracle exactly.
func TestTriageCompositePKAsc(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := `CREATE TABLE t4(x,y,z,PRIMARY KEY('x' ASC, "y" ASC));
INSERT INTO t4 VALUES(1,1,11);
INSERT INTO t4 VALUES(1,2,12);
INSERT INTO t4 VALUES(1,3,13);
INSERT INTO t4 VALUES(2,2,22);`
	query := "SELECT rowid FROM t4 WHERE x IN (1,9,2,5) AND y IN (1,3,NULL,2) AND z!=13;"

	runSQL(t, db, setup)
	got := queryRows(t, db, query)
	want := oracleRows(t, setup+"\n"+query) // sqlite3: rowids 1 2 4
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows mismatch:\n got %#v\nwant %#v", got, want)
	}
}

// Triage: WHERE a IS NULL must return rows where a is NULL.
// TCL where4-8.2: u9(a UNIQUE,b) with rows (NULL,1),(NULL,2);
// SELECT * FROM u9 WHERE a IS NULL → NULL 1 NULL 2.
// Regression guard: frigolite rows must match the sqlite3 oracle exactly.
func TestTriageIsNull(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := `CREATE TABLE u9(a UNIQUE, b);
INSERT INTO u9 VALUES(NULL, 1);
INSERT INTO u9 VALUES(NULL, 2);`
	query := "SELECT * FROM u9 WHERE a IS NULL"

	runSQL(t, db, setup)
	got := queryRows(t, db, query)
	want := oracleRows(t, setup+"\n"+query) // sqlite3: {} 1 {} 2 ({} = NULL token)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows mismatch:\n got %#v\nwant %#v", got, want)
	}
}
