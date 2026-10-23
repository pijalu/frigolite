package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVtab1EchoRegistrationReopenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Exec("CREATE VIRTUAL TABLE v USING echo(v)").Error; got == nil || !strings.Contains(got.Error(), "no such module: echo") {
		t.Fatalf("unregistered module error = %v", got)
	}
	db.RegisterEchoModule()
	for _, stmt := range []string{
		"CREATE TABLE real(a,b,c)",
		"CREATE VIRTUAL TABLE e USING echo(real)",
	} {
		if got := db.Exec(stmt).Error; got != nil {
			t.Fatalf("Exec(%q): %v", stmt, got)
		}
	}
	db.UnregisterEchoModule()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.Query("SELECT * FROM e").Error; got == nil || !strings.Contains(got.Error(), "no such module: echo") {
		t.Fatalf("reopened unregistered module error = %v", got)
	}
	db.RegisterEchoModule()
	if got := db.Query("SELECT name FROM sqlite_schema WHERE name='e'").Error; got != nil {
		t.Fatalf("schema after registration: %v", got)
	}
	if got := db.Query("SELECT * FROM e").Error; got != nil {
		t.Fatalf("query after re-registration: %v", got)
	}
}
