package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestNativeTclvarDML is the native regression anchor for the tclvar virtual
// table's write path (src/test_tclvar.c xUpdate parity): INSERT/UPDATE/DELETE
// keyed by fullname, including value=NULL deletion and fullname renames. It
// mirrors vtabJ.test 100-161 without relying on TCL `array names`
// introspection loops (which the transpiler cannot express — see
// lessons_learned "tclvar module" notes).
func TestNativeTclvarDML(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE VIRTUAL TABLE tclvar USING tclvar;`); res.Error != nil {
		t.Fatal(res.Error)
	}

	// INSERT: array elements and scalars.
	if res := db.Exec(`
		INSERT INTO tclvar(fullname, value) VALUES('vtabJ(1)','this');
		INSERT INTO tclvar(fullname, value) VALUES('vtabJ(two)','is');
		INSERT INTO tclvar(fullname, value) VALUES('xx','a');
	`); res.Error != nil {
		t.Fatal(res.Error)
	}
	r := db.Query(`SELECT fullname, value FROM tclvar ORDER BY fullname`)
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	want := "vtabJ(1) this vtabJ(two) is xx a"
	if got := flattenQueryRows(r); got != want {
		t.Fatalf("insert: got [%s] want [%s]", got, want)
	}

	// UPDATE by value.
	if res := db.Exec(`UPDATE tclvar SET value=55 WHERE fullname='vtabJ(two)'`); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = db.Query(`SELECT value FROM tclvar WHERE fullname='vtabJ(two)'`)
	if got := flattenQueryRows(r); got != "55" {
		t.Fatalf("update value: got [%s] want [55]", got)
	}

	// UPDATE rename: fullname change moves the entry.
	if res := db.Exec(`UPDATE tclvar SET fullname='vtabJ(2)' WHERE fullname='vtabJ(two)'`); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = db.Query(`SELECT fullname FROM tclvar WHERE value='55'`)
	if got := flattenQueryRows(r); got != "vtabJ(2)" {
		t.Fatalf("rename: got [%s] want [vtabJ(2)]", got)
	}

	// INSERT with NULL value deletes the variable.
	if res := db.Exec(`INSERT INTO tclvar(fullname, value) VALUES('vtabJ(2)',NULL)`); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = db.Query(`SELECT count(*) FROM tclvar WHERE fullname='vtabJ(2)'`)
	if got := flattenQueryRows(r); got != "0" {
		t.Fatalf("null insert delete: got [%s] want [0]", got)
	}

	// DELETE by predicate.
	if res := db.Exec(`DELETE FROM tclvar WHERE arrayname='two' OR name='xx'`); res.Error != nil {
		t.Fatal(res.Error)
	}
	r = db.Query(`SELECT fullname FROM tclvar ORDER BY fullname`)
	if got := flattenQueryRows(r); got != "vtabJ(1)" {
		t.Fatalf("delete: got [%s] want [vtabJ(1)]", got)
	}
}

// flattenQueryRows renders each row's columns space-separated, rows joined by
// spaces with NULL as {} — the harness flatten convention.
func flattenQueryRows(r *frigolite.Result) string {
	out := ""
	for i, row := range r.Rows {
		if i > 0 {
			out += " "
		}
		first := true
		for _, cell := range row {
			if !first {
				out += " "
			}
			if cell == nil {
				out += "{}"
			} else {
				out += cellString(cell)
			}
			first = false
		}
	}
	return out
}

func cellString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return fmt.Sprintf("%d", x)
	}
	return fmt.Sprintf("%v", v)
}
