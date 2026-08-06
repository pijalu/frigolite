package frigolite

import (
	"strings"
	"testing"
)

// Triage: composite PRIMARY KEY with quoted identifiers + ASC.
// TCL where4-5.2: CREATE TABLE t4(x,y,z,PRIMARY KEY('x' ASC, "y" ASC));
// then INSERT (1,1),(1,2),(1,3),(2,2) must all succeed (PK is (x,y)).
func TestTriageCompositePKAsc(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE t4(x,y,z,PRIMARY KEY('x' ASC, "y" ASC));`).Error; err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	for _, sql := range []string{
		"INSERT INTO t4 VALUES(1,1,11);",
		"INSERT INTO t4 VALUES(1,2,12);",
		"INSERT INTO t4 VALUES(1,3,13);",
		"INSERT INTO t4 VALUES(2,2,22);",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatalf("INSERT %q: %v", strings.TrimSpace(sql), err)
		}
	}
	r := db.Query("SELECT rowid FROM t4 WHERE x IN (1,9,2,5) AND y IN (1,3,NULL,2) AND z!=13;")
	if r.Error != nil {
		t.Fatalf("SELECT: %v", r.Error)
	}
	t.Logf("rows=%v", r.Rows)
	// Oracle (sqlite3): rowids 1 2 4
}

// Triage: WHERE a IS NULL must return rows where a is NULL.
// TCL where4-8.2: u9(a UNIQUE,b) with rows (NULL,1),(NULL,2);
// SELECT * FROM u9 WHERE a IS NULL → NULL 1 NULL 2.
func TestTriageIsNull(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(`CREATE TABLE u9(a UNIQUE, b); INSERT INTO u9 VALUES(NULL, 1); INSERT INTO u9 VALUES(NULL, 2);`).Error; err != nil {
		t.Fatalf("setup: %v", err)
	}
	r := db.Query("SELECT * FROM u9 WHERE a IS NULL")
	if r.Error != nil {
		t.Fatalf("SELECT: %v", r.Error)
	}
	t.Logf("rows=%v", r.Rows)
	// Oracle (sqlite3): NULL 1 NULL 2 → [{} 1 {} 2] with {} NULL token
}
