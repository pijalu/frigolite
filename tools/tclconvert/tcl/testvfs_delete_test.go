package tcl

import "testing"

func TestTestVFSDeleteUnregistersWrapper(t *testing.T) {
	i := NewInterp()
	if err := i.Execute(`testvfs tv -noshm 1
tv delete`); err != nil {
		t.Fatal(err)
	}
	stmts := i.Stmts()
	if len(stmts) != 2 {
		t.Fatalf("statements = %#v, want register + delete", stmts)
	}
	if stmts[0].Type != "register_testvfs" || stmts[0].Variable != "tv" || stmts[0].SQL != "noshm" {
		t.Fatalf("register step = %#v", stmts[0])
	}
	del := stmts[1]
	if del.Type != "delete_vfs" || del.SQL != "tv" || del.Database != "tv" {
		t.Fatalf("delete step = %#v, want delete_vfs for tv", del)
	}
	if i.testvfsNames["tv"] {
		t.Fatal("tv still registered after delete")
	}
}
