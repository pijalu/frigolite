package tcl

import "testing"

func TestColumnNameCapturesPreparedStatementName(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set stmt [sqlite3_prepare db {SELECT * FROM t1} -1 tail]
set name [sqlite3_column_name $stmt 0]`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("statements = %#v, want prepare + column-name capture", stmts)
	}
	cn := stmts[1]
	if cn.Type != "stmt_column_name" || cn.Variable != "stmt_1" || cn.ColumnIndex != 0 || cn.AssignVariable != "name" {
		t.Fatalf("column-name step = %#v", cn)
	}
}

func TestIncrblobZeroWriteBecomesSQLBlobOverwrite(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`set fd [db incrblob t1_segments block 2]
puts -nonewline $fd [string repeat "\00" 4]
close $fd`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 1 {
		t.Fatalf("statements = %#v, want one in-place blob overwrite", stmts)
	}
	want := "UPDATE t1_segments SET block = zeroblob(length(block)) WHERE rowid = 2"
	if stmts[0].Type != "exec" || stmts[0].SQL != want {
		t.Fatalf("incrblob step = %#v, want exec %q", stmts[0], want)
	}
}
