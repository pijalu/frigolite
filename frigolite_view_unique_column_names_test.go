package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestViewUniqueColumnNames(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t(a, b)",
		"CREATE VIEW direct AS SELECT a, a FROM t",
		"CREATE VIEW expanded AS SELECT * FROM (SELECT a, a FROM t)",
	} {
		if r := db.Exec(stmt); r.Error != nil {
			t.Fatalf("%s: %v", stmt, r.Error)
		}
	}
	for _, tc := range []struct{ view, want string }{
		{"direct", "a a:1"},
		{"expanded", "a a:1"},
	} {
		r := db.Query("PRAGMA table_info(" + tc.view + ")")
		if r.Error != nil {
			t.Fatalf("%s: %v", tc.view, r.Error)
		}
		var got string
		for _, row := range r.Rows {
			if len(row) > 1 {
				if got != "" {
					got += " "
				}
				got += fmt.Sprint(row[1])
			}
		}
		if got != tc.want {
			t.Errorf("%s columns got %q want %q", tc.view, got, tc.want)
		}
	}
}
