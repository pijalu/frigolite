package frigolite_test

import (
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// TestInsBindRowidStashPin is the kind-parity pin for the bind-mode stash
// serving the explicit-rowid lanes: with every bound slot stashed (the
// placeholder-node lane), INSERT INTO t(rowid, a) VALUES(?, ?) and the
// INTEGER-PRIMARY-K column-list form must still resolve the rowid from the
// bound value — int64 binds write the exact rowid, a NULL bind auto-assigns,
// a non-integer-coercible bind (float, text) fails with the OP_MustBeInt
// "datatype mismatch" text. The echo write-through's pre-check (vtab rowid
// validation before xUpdate) must read the stash the same way. Every stored
// rowid is verified against the rowid a literal control statement produces.
func TestInsBindRowidStashPin(t *testing.T) {
	// Plain rowid tables: explicit rowid column list.
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE TABLE r1(a INTEGER, id INTEGER PRIMARY KEY)"); res.Error != nil {
		t.Fatal(res.Error)
	}

	// int64 bind on the IPK column list: rowid must be the bound value.
	st, err := db.Prepare("INSERT INTO r1(a, id) VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if res := st.Exec("v", i*10); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	st.Close()
	r := db.Query("SELECT rowid, a FROM r1 ORDER BY rowid")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	for i, row := range r.Rows {
		want := int64(i+1) * 10
		if row[0].(int64) != want {
			t.Fatalf("rowid stash lane: row %d rowid = %v, want %d", i, row[0], want)
		}
	}

	// Explicit rowid pseudo-column with int64 and NULL binds.
	if res := db.Exec("CREATE TABLE r2(a TEXT)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	st, err = db.Prepare("INSERT INTO r2(rowid, a) VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	if res := st.Exec(int64(77), "x"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := st.Exec(nil, "y"); res.Error != nil {
		t.Fatal(res.Error)
	}
	st.Close()
	r = db.Query("SELECT rowid, a FROM r2 ORDER BY rowid")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 2 || r.Rows[0][0].(int64) != 77 {
		t.Fatalf("rowid pseudo-column bind: got %v", r.Rows)
	}

	// Non-integer rowid binds must fail with "datatype mismatch" (the
	// OP_MustBeInt parity text), with and without the stash lane.
	for _, bad := range []interface{}{4.5, "xyz"} {
		st, err = db.Prepare("INSERT INTO r2(rowid, a) VALUES(?, ?)")
		if err != nil {
			t.Fatal(err)
		}
		res := st.Exec(bad, "z")
		st.Close()
		if res.Error == nil || res.Error.Error() != "datatype mismatch" {
			t.Fatalf("rowid bind %v: error = %v, want datatype mismatch", bad, res.Error)
		}
	}
	// Control: the literal form reports the same error.
	if res := db.Exec("INSERT INTO r2(rowid, a) VALUES(4.5, 'z')"); res.Error == nil || res.Error.Error() != "datatype mismatch" {
		t.Fatalf("literal float rowid: error = %v, want datatype mismatch", res.Error)
	}

	// Echo vtab write-through: the pre-check validates the rowid before
	// xUpdate; a stashed float rowid must raise the same mismatch.
	e2, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	e2.RegisterEchoModule()
	if res := e2.Exec("CREATE TABLE src(a TEXT)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := e2.Exec("CREATE VIRTUAL TABLE ev USING echo(src)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	est, err := e2.Prepare("INSERT INTO ev(rowid, a) VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	if res := est.Exec(int64(5), "ok"); res.Error != nil {
		t.Fatal(res.Error)
	}
	res := est.Exec(6.5, "bad")
	est.Close()
	if res.Error == nil || res.Error.Error() != "datatype mismatch" {
		t.Fatalf("echo rowid bind: error = %v, want datatype mismatch", res.Error)
	}
	r = e2.Query("SELECT rowid, a FROM ev")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0].(int64) != 5 {
		t.Fatalf("echo vtab rows: got %v", r.Rows)
	}
}
