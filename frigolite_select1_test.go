package frigolite

import (
	"reflect"
	"testing"
)

// TestSelect1EmptyResultColumnNames pins the engine contract behind select1.test-9.2..9.5.
// SQLite's Tcl db eval callback exposes result column names even for empty results.
func TestSelect1EmptyResultColumnNames(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE test1(f1 INT, f2 INT)",
		"CREATE TABLE test2(f1 INT)",
		"INSERT INTO test1 VALUES(11,22),(33,44)",
		"INSERT INTO test2 VALUES(1),(2)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	queries := []string{
		"SELECT * FROM test1 WHERE f1<0",
		"SELECT * FROM test1 WHERE f1<(SELECT count(*) FROM test2)",
		"SELECT * FROM test1 ORDER BY f1",
		"SELECT * FROM test1 WHERE f1<0 ORDER BY f1",
	}
	for _, query := range queries {
		res := db.Query(query)
		if res.Error != nil {
			t.Fatalf("Query(%q): %v", query, res.Error)
		}
		if !reflect.DeepEqual(res.Columns, []string{"f1", "f2"}) {
			t.Errorf("Query(%q) columns = %#v, want [f1 f2]", query, res.Columns)
		}
	}
}
