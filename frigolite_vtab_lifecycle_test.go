package frigolite_test

import (
	"path/filepath"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func TestEchoLogTableCreateDropLifecycle(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.RegisterEchoModule()
	for _, stmt := range []string{
		"CREATE TABLE source(a)",
		"CREATE TABLE logmsg(log)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if res := db.Exec("CREATE VIRTUAL TABLE echo_source USING echo(source, logmsg)"); res.Error == nil || res.Error.Error() != "table 'logmsg' already exists" {
		t.Fatalf("CREATE with existing log table error = %v", res.Error)
	}
	if res := db.Exec("DROP TABLE logmsg"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE echo_source USING echo(source, logmsg)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("DROP TABLE echo_source"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Query("SELECT name FROM sqlite_master WHERE name='logmsg'"); res.Error != nil || len(res.Rows) != 0 {
		t.Fatalf("log table after echo drop = %#v, error %v", res.Rows, res.Error)
	}
}

func TestFileBackedEchoCloseReopenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.RegisterEchoModule()
	for _, stmt := range []string{
		"CREATE TABLE source(a, b)",
		"INSERT INTO source VALUES('kept', 7)",
		"CREATE VIRTUAL TABLE echo_source USING echo(source)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Query("SELECT * FROM echo_source"); res.Error == nil || !strings.Contains(res.Error.Error(), "no such module: echo") {
		t.Fatalf("query without per-connection registration error = %v, want no such module", res.Error)
	}
	if res := db.Exec("DROP TABLE echo_source"); res.Error == nil || !strings.Contains(res.Error.Error(), "no such module: echo") {
		t.Fatalf("drop without per-connection registration error = %v, want no such module", res.Error)
	}
	if res := db.Query("SELECT name FROM sqlite_master WHERE name='echo_source'"); res.Error != nil || len(res.Rows) != 1 {
		t.Fatalf("virtual table after failed drop = %#v, error %v; want persisted schema", res.Rows, res.Error)
	}
	db.RegisterEchoModule()
	res := db.Query("SELECT * FROM echo_source")
	if res.Error != nil {
		t.Fatalf("query after re-registration: %v", res.Error)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 2 || res.Rows[0][0] != "kept" || res.Rows[0][1] != int64(7) {
		t.Fatalf("persisted rows = %#v, want [[kept 7]]", res.Rows)
	}
}
