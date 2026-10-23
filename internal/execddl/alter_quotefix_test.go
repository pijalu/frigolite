package execddl

import (
	"testing"

	"github.com/pijalu/frigolite/internal/schema"
)

func TestRenameColumnInTriggerQuotedUpdateOf(t *testing.T) {
	input := `CREATE TRIGGER zzz AFTER UPDATE OF "col a", "col c" ON t6 BEGIN UPDATE c SET x=x+1; END`
	got, err := renameColumnInTriggerSQL(input, "col c", "col 3", true)
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE TRIGGER zzz AFTER UPDATE OF "col a", "col 3" ON t6 BEGIN UPDATE c SET x=x+1; END`
	if got != want {
		t.Fatalf("renamed quoted trigger SQL = %q, want %q", got, want)
	}
}

func TestRenameColumnInTriggerReferences(t *testing.T) {
	input := "CREATE TRIGGER ttt AFTER INSERT ON t4 WHEN new.y<0 BEGIN SELECT x,y,z FROM t4; DELETE FROM t4 WHERE y=32; UPDATE t4 SET x=y+1,y=0 WHERE y=32; INSERT INTO t4(x,y,z) SELECT 4,5,6 WHERE 0; END"
	got, err := renameColumnInTriggerSQL(input, "y", "abc", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TRIGGER ttt AFTER INSERT ON t4 WHEN new.abc<0 BEGIN SELECT x,abc,z FROM t4; DELETE FROM t4 WHERE abc=32; UPDATE t4 SET x=abc+1,abc=0 WHERE abc=32; INSERT INTO t4(x,abc,z) SELECT 4,5,6 WHERE 0; END"
	if got != want {
		t.Fatalf("renamed trigger SQL = %q, want %q", got, want)
	}
}

func TestRenameColumnInExpressionIndex(t *testing.T) {
	entry := &schema.Entry{Type: schema.TypeIndex, TblName: "t1", SQL: "CREATE INDEX t1i ON t1(b+b+b+b, c) WHERE b>0"}
	ctx := &RenameContext{OldName: "b", NewName: "d", QuotedNew: "d", TableName: "t1"}
	applyColumnRenameToEntry(entry, "b", "d", ctx)
	got := entry.SQL
	const want = "CREATE INDEX t1i ON t1(d+d+d+d, c) WHERE d>0"
	if got != want {
		t.Fatalf("renamed index SQL = %q, want %q", got, want)
	}
}
