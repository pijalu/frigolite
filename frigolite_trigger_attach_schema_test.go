package frigolite

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTriggerOnAttachedTableRejectedOutsideTemp(t *testing.T) {
	dir := t.TempDir()
	target, err := Open(filepath.Join(dir, "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	source, err := Open(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if r := source.Exec("CREATE TABLE t1(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := target.Exec("ATTACH DATABASE '" + filepath.Join(dir, "source.db") + "' AS orig"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := target.Exec("CREATE TRIGGER r1 AFTER INSERT ON orig.t1 BEGIN SELECT 'no-op'; END")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "trigger r1 cannot reference objects in database orig") {
		t.Fatalf("cross-database trigger error = %v, want SQLite schema restriction", r.Error)
	}
}
