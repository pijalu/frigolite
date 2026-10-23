package frigolite

import (
	"reflect"
	"testing"
)

func TestCursorhint6CorrelatedHavingKeepsGroupRows(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t6(a TEXT UNIQUE, b TEXT)",
		"INSERT INTO t6(a,b) VALUES('uvw','xyz'),('abc','def')",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatal(result.Error)
		}
	}

	query := `WITH v1(a) AS (SELECT a COLLATE NOCASE FROM t6)
SELECT v1.a, count(*) FROM t6 LEFT JOIN v1 ON true
GROUP BY 1
HAVING (SELECT true FROM t6 AS aa LEFT JOIN t6 AS bb ON length(v1.a)>5)`
	result := db.Query(query)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := [][]interface{}{{"abc", int64(2)}, {"uvw", int64(2)}}
	if !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("query rows = %#v, want %#v", result.Rows, want)
	}
}
