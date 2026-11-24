// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"strings"
	"testing"
)

// TestEExpr32RealText pins e_expr-32.2.8 against the in-tree SQLite 3.51
// oracle's REAL-to-text spelling. The fixture's 17-digit spelling comes from
// SQLite 3.54; the engine value is the same.
func TestEExpr32RealText(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := `WITH t1(x) AS (VALUES
	 ('9000000000000000001'),
	 ('9223372036854775808'),
	 ('   9223372036854775808   '),
	 ('9223372036854775807.0'),
	 ('9223372036854775807e+0'))
	SELECT typeof(CAST(x AS NUMERIC)), CAST(x AS NUMERIC)||'' FROM t1;`
	res := db.Query(q)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	var got []string
	for _, row := range res.Rows {
		got = append(got, formatSQLiteValue("", row[0])+" "+formatSQLiteValue("", row[1]))
	}
	want := []string{
		"integer 9000000000000000001",
		"real 9.22337203685478e+18",
		"real 9.22337203685478e+18",
		"real 9.22337203685478e+18",
		"real 9.22337203685478e+18",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
}
