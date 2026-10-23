package frigolite

import (
	"reflect"
	"testing"
)

func TestEKeyMaxparentTriggerCallback(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.RegisterFunction("maxparent", func([]interface{}) (interface{}, error) {
		res := db.Query("SELECT max(x) FROM parent")
		if res.Error != nil {
			return nil, res.Error
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			return nil, nil
		}
		return res.Rows[0][0], nil
	}, 0, 0)
	for _, sql := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE parent(x PRIMARY KEY)",
		"CREATE TRIGGER bu BEFORE UPDATE ON parent BEGIN INSERT INTO parent VALUES(new.x-old.x); END",
		"CREATE TABLE child(a DEFAULT (maxparent()) REFERENCES parent ON UPDATE SET DEFAULT)",
		"CREATE TRIGGER au AFTER UPDATE ON parent BEGIN INSERT INTO parent VALUES(new.x+old.x); END",
		"INSERT INTO parent VALUES(1)",
		"INSERT INTO child VALUES(1)",
		"UPDATE parent SET x=22",
	} {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("Exec(%q): %v", sql, res.Error)
		}
	}
	res := db.Query("SELECT x FROM parent ORDER BY rowid")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]interface{}{{int64(22)}, {int64(21)}, {int64(23)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("parent rows = %#v, want %#v", res.Rows, want)
	}
	res = db.Query("SELECT a FROM child")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(22) {
		t.Fatalf("child rows = %#v, want [[22]]", res.Rows)
	}
}
