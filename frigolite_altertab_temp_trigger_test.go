package frigolite

import (
	"fmt"
	"testing"
)

func TestAltertabTempTriggerOnAttachedTable(t *testing.T) {
	db := setupDBWithoutEcho(t)
	defer db.Close()
	var calls []string
	db.RegisterFunction("trigger", func(args []interface{}) (interface{}, error) {
		calls = append(calls, fmt.Sprint(args...))
		return nil, nil
	}, 3, 3)
	for _, stmt := range []string{
		"ATTACH ':memory:' AS aux",
		"CREATE TABLE main.t1(a,b,c)",
		"CREATE TABLE aux.t1(a,b,c)",
		"CREATE TEMP TRIGGER tr AFTER INSERT ON aux.t1 BEGIN SELECT trigger(new.a,new.b,new.c); END",
		"INSERT INTO main.t1 VALUES(1,2,3)",
		"INSERT INTO aux.t1 VALUES(4,5,6)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if len(calls) != 1 || calls[0] != "4 5 6" {
		t.Fatalf("trigger calls = %v, want one [4 5 6] callback", calls)
	}
	if res := db.Exec("ALTER TABLE main.t1 RENAME TO t2"); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := db.Query("SELECT tbl_name FROM sqlite_temp_master WHERE name='tr'")
	if res.Error != nil || len(res.Rows) != 1 || res.Rows[0][0] != "t1" {
		t.Fatalf("temp trigger tbl_name after main rename = %#v, err=%v; want t1", res.Rows, res.Error)
	}
}
