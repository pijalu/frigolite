package frigolite

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestQueryEachStopsAndPropagatesCallbackError(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := db.Exec("CREATE TABLE q(x); INSERT INTO q VALUES(1),(2),(3); CREATE TABLE q2(y); INSERT INTO q2 VALUES(4),(5)"); result.Error != nil {
		t.Fatal(result.Error)
	}
	seen := 0
	result := db.QueryEach("SELECT x FROM q", func([]interface{}) (bool, error) {
		seen++
		return false, nil
	})
	if result.Error != nil || seen != 1 {
		t.Fatalf("early stop: seen=%d err=%v", seen, result.Error)
	}
	wantErr := fmt.Errorf("callback failure")
	result = db.QueryEach("SELECT x FROM q", func([]interface{}) (bool, error) {
		return true, wantErr
	})
	if !errors.Is(result.Error, wantErr) {
		t.Fatalf("callback error = %v, want %v", result.Error, wantErr)
	}
	seen = 0
	result = db.QueryEach("SELECT q.x, q2.y FROM q CROSS JOIN q2 ORDER BY q2.y DESC LIMIT 1", func(row []interface{}) (bool, error) {
		seen++
		if !reflect.DeepEqual(row, []interface{}{int64(1), int64(5)}) {
			t.Fatalf("materialized fallback row = %#v", row)
		}
		return false, nil
	})
	if result.Error != nil || seen != 1 {
		t.Fatalf("fallback early stop: seen=%d err=%v", seen, result.Error)
	}
}

func TestQueryEachDeleteRestoresAtNextOuterKey(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE outer_rows(i INTEGER PRIMARY KEY)",
		"CREATE TABLE inner_rows(x, y)",
		"INSERT INTO outer_rows VALUES(25),(26)",
		"INSERT INTO inner_rows VALUES(25,'a'),(25,'b'),(25,'c'),(26,'d'),(26,'e')",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("%s: %v", statement, result.Error)
		}
	}
	var rows [][]interface{}
	result := db.QueryEach("SELECT x, y FROM outer_rows CROSS JOIN inner_rows WHERE x=i", func(row []interface{}) (bool, error) {
		rows = append(rows, append([]interface{}(nil), row...))
		if row[1] == "b" {
			if deleted := db.Exec("DELETE FROM outer_rows WHERE i=25"); deleted.Error != nil {
				return false, deleted.Error
			}
		}
		return true, nil
	})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := [][]interface{}{{int64(25), "a"}, {int64(25), "b"}, {int64(26), "d"}, {int64(26), "e"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("callback rows after outer-key delete = %#v, want %#v", rows, want)
	}
}

func TestBtreefaultSelectCallbackMutation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE t1(i INTEGER PRIMARY KEY, a, b)",
		"CREATE INDEX i1 ON t1(b)",
		"CREATE TABLE t2(x, y)",
		"INSERT INTO t1 VALUES(25,25,25)",
		"INSERT INTO t2 VALUES(25,'a'),(25,'b'),(25,'c')",
	} {
		if result := db.Exec(statement); result.Error != nil {
			t.Fatalf("%s: %v", statement, result.Error)
		}
	}
	var rows [][]interface{}
	result := db.QueryEach("SELECT x, y FROM t1 CROSS JOIN t2 WHERE t2.x=t1.i AND +t1.i=25 ORDER BY b", func(row []interface{}) (bool, error) {
		rows = append(rows, append([]interface{}(nil), row...))
		if row[1] == "b" {
			if deleted := db.Exec("DELETE FROM t1 WHERE i=25"); deleted.Error != nil {
				return false, deleted.Error
			}
		}
		return true, nil
	})
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	want := [][]interface{}{{int64(25), "a"}, {int64(25), "b"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("callback rows = %#v, want %#v", rows, want)
	}
	remaining := db.Query("SELECT count(*) FROM t1")
	if remaining.Error != nil || len(remaining.Rows) != 1 || remaining.Rows[0][0] != int64(0) {
		t.Fatalf("outer row after callback DELETE = %#v, err=%v", remaining.Rows, remaining.Error)
	}
}
