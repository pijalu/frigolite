package execddl

import "testing"

func TestFnSQLiteRenameTable(t *testing.T) {
	for _, args := range [][]interface{}{
		{"main", 0, 0, "CREATE TABLE x1(i INTEGER, t TEXT)", "ddd", nil, int64(0)},
		{"main", 0, 0, "CREATE TABLE x1(i INTEGER, t TEXT)", nil, "eee", int64(0)},
		{"main", 0, 0, nil, "ddd", "eee", int64(0)},
	} {
		got, err := FnSQLiteRenameTable(args)
		if err != nil || got != nil {
			t.Fatalf("FnSQLiteRenameTable(%#v) = (%#v, %v), want (nil, nil)", args, got, err)
		}
	}
	got, err := FnSQLiteRenameTable([]interface{}{"main", 0, 0, "CREATE TABLE ddd(a)", "ddd", "eee", int64(0)})
	if err != nil || got != `CREATE TABLE "eee"(a)` {
		t.Fatalf("rename result = (%#v, %v), want quoted renamed SQL", got, err)
	}
}
