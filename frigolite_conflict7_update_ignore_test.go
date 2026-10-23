package frigolite

import (
	"fmt"
	"reflect"
	"testing"
)

func TestConflict7UpdateIgnorePreservesOneNonConflictingRow(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	if result := db.Exec("CREATE TABLE t1(a UNIQUE, b)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	for i := 1; i <= 50; i++ {
		if result := db.Exec(fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d)", i, i+1)); result.Error != nil {
			t.Fatalf("insert row %d: %v", i, result.Error)
		}
	}
	if result := db.Exec("UPDATE OR IGNORE t1 SET a=1000"); result.Error != nil {
		t.Fatal(result.Error)
	}
	result := db.Query("SELECT b FROM t1 WHERE a=1000")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if want := [][]interface{}{{int64(2)}}; !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("surviving updated row = %#v, want %#v", result.Rows, want)
	}
	result = db.Query("SELECT count(*) FROM t1")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	if want := [][]interface{}{{int64(50)}}; !reflect.DeepEqual(result.Rows, want) {
		t.Fatalf("row count after UPDATE OR IGNORE = %#v, want %#v", result.Rows, want)
	}
}
