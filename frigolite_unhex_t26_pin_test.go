package frigolite

import "testing"

func TestUnhexFixtureContracts(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	named := []struct {
		name string
		sql  string
		want interface{}
	}{
		{"bound leading zeros", "SELECT hex(unhex($hex))", "0000"},
		{"bound lower leading zeros", "SELECT hex(unhex(lower($hex)))", "0000"},
	}
	for _, tc := range named {
		t.Run(tc.name, func(t *testing.T) {
			st, err := db.Prepare(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.BindNamed("$hex", "0000"); err != nil {
				t.Fatal(err)
			}
			r := st.Exec()
			if r.Error != nil {
				t.Fatal(r.Error)
			}
			if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != tc.want {
				t.Fatalf("got %#v want %#v", r.Rows, tc.want)
			}
		})
	}

	literal := []struct {
		name string
		sql  string
		want interface{}
	}{
		{"numeric literal is not text", "SELECT hex(unhex(0000))", nil},
		{"unicode separator", "SELECT hex(unhex('กABCDข', 'กข'))", "ABCD"},
		{"wrong unicode separator", "SELECT typeof(unhex('กABCDข', 'ฃข'))", "null"},
		{"unicode separator with space", "SELECT hex(unhex('กAB CDขก', 'ก ข'))", "ABCD"},
	}
	for _, tc := range literal {
		t.Run(tc.name, func(t *testing.T) {
			r := db.Query(tc.sql)
			if r.Error != nil {
				t.Fatal(r.Error)
			}
			if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != tc.want {
				t.Fatalf("got %#v want %#v", r.Rows, tc.want)
			}
		})
	}
}
