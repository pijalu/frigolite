package frigolite

import "testing"

func TestStmtBindStepReset(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(v INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	st, err := db.Prepare("SELECT ? + :n")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindInt(1, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.BindNamed("n", 3); err != nil {
		t.Fatal(err)
	}
	ok, err := st.Step()
	if err != nil || !ok {
		t.Fatalf("step: %v %v", ok, err)
	}
	if got := st.Row()[0]; got != int64(5) {
		t.Fatalf("got %v", got)
	}
	ok, err = st.Step()
	if err != nil || ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	ok, err = st.Step()
	if err != nil || !ok {
		t.Fatalf("reset step: %v %v", ok, err)
	}
}

func TestStmtExecNamedText(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := db.Prepare("SELECT :x")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BindText(1, "unused"); err != nil {
		t.Fatal(err)
	}
	if err := st.BindNamed("x", "hello"); err != nil {
		t.Fatal(err)
	}
	r := st.Exec()
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "hello" {
		t.Fatalf("result: %#v", r)
	}
}
