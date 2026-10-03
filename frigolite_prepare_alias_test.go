package frigolite_test

import (
	"testing"

	frigo "github.com/pijalu/frigolite"
)

// TestPinPrepareTemplateAlias pins the retained-Prepare clone contract: two
// prepared statements of one shape (differing only in literals) must hold
// INDEPENDENT ASTs. Engine.Prepare's template-cache hit used to clone onto
// the shared per-exec-depth scratch, so preparing the second statement
// recycled the first one's AST and both handles executed the LAST prepared
// literals (capi2-4/6: INSERT (2,3) then (3,4) stored two (3,4) rows).
func TestPinPrepareTemplateAlias(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t2(a, b)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	s23, err := db.Prepare("INSERT INTO t2 VALUES(2,3)")
	if err != nil {
		t.Fatal(err)
	}
	s34, err := db.Prepare("INSERT INTO t2 VALUES(3,4)")
	if err != nil {
		t.Fatal(err)
	}
	s12, err := db.Prepare("INSERT INTO t2 VALUES(1,2)")
	if err != nil {
		t.Fatal(err)
	}
	// Step in an order that exposes any shared-AST rewrite: the first handle
	// runs after the later prepares already substituted their literals.
	if r := s23.Exec(); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := s34.Exec(); r.Error != nil {
		t.Fatal(r.Error)
	}
	res := db.Query("SELECT * FROM t2 ORDER BY a")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 2 || res.Rows[0][0] != int64(2) || res.Rows[1][0] != int64(3) {
		t.Fatalf("rows = %v, want [[2 3] [3 4]]", res.Rows)
	}
	if r := s12.Exec(); r.Error != nil {
		t.Fatal(r.Error)
	}
	res = db.Query("SELECT * FROM t2 ORDER BY a")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 3 || res.Rows[0][0] != int64(1) || res.Rows[1][0] != int64(2) || res.Rows[2][0] != int64(3) {
		t.Fatalf("rows = %v, want [[1 2] [2 3] [3 4]]", res.Rows)
	}
	// The interleaved raw-Exec shape (same template, immediate consume) must
	// also stay correct against the retained handles.
	if r := db.Exec("INSERT INTO t2 VALUES(9,9)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := s23.Exec(); r.Error != nil {
		t.Fatal(r.Error)
	}
	res = db.Query("SELECT count(*), sum(a) FROM t2")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if res.Rows[0][0] != int64(5) || res.Rows[0][1] != int64(2+3+1+9+2) {
		t.Fatalf("count/sum = %v, want [5 17]", res.Rows[0])
	}
}
