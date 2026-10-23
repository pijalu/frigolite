package tcl

import (
	"strings"
	"testing"
)

func TestAttachLoopCapturesDynamicSQL(t *testing.T) {
	i := NewInterp()
	src := `for {set i 0} {$i<$SQLITE_MAX_ATTACHED} {incr i} {
  db eval "ATTACH ':memory:' AS a$i"
}
set m "a[expr {$SQLITE_MAX_ATTACHED-1}]"
db eval "CREATE TABLE $m.t1(a INTEGER PRIMARY KEY, b);"`
	if err := i.Execute(src); err != nil {
		t.Fatal(err)
	}
	var attaches int
	foundA9Table := false
	for _, stmt := range i.Stmts() {
		if strings.HasPrefix(stmt.SQL, "ATTACH ") {
			attaches++
		}
		if stmt.SQL == "CREATE TABLE a9.t1(a INTEGER PRIMARY KEY, b);" {
			foundA9Table = true
		}
	}
	if attaches != 10 || !foundA9Table {
		t.Fatalf("expanded attachment count=%d final table=%v statements=%#v", attaches, foundA9Table, i.Stmts())
	}
}
