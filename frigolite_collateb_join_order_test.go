package frigolite

import (
	"reflect"
	"testing"
)

func TestCollateBJoinOrderKeepsMatchedRightRow(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY)",
		"CREATE TABLE t2(b INTEGER PRIMARY KEY, x1 INT COLLATE NOCASE)",
		"CREATE TABLE t3(x2 INT)",
		"INSERT INTO t1(a) VALUES(1),(2),(3)",
		"INSERT INTO t2(b,x1) VALUES(11,1),(22,2),(33,3)",
		"INSERT INTO t3(x2) VALUES(11),(22),(33)",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, result.Error)
		}
	}
	for _, query := range []string{
		"SELECT *,'|' FROM t2, t1, t3 WHERE x2=b AND x1=a AND a=1",
		"SELECT *,'|' FROM t2, t1, t3 WHERE b=x2 AND a=x1 AND 1=a",
		"SELECT *,'|' FROM t2, t1, t3 WHERE t3.x2=t2.b AND t2.x1=t1.a AND t1.a=1",
	} {
		result := db.Query(query)
		if result.Error != nil {
			t.Fatalf("Query(%q): %v", query, result.Error)
		}
		want := [][]interface{}{{int64(11), int64(1), int64(1), int64(11), "|"}}
		if !reflect.DeepEqual(result.Rows, want) {
			t.Errorf("Query(%q) rows = %#v, want %#v", query, result.Rows, want)
		}
	}
}
