package frigolite

import (
	"reflect"
	"strings"
	"testing"
)

func TestJoinB4NestedUsingGroupsResolveMergedColumns(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a INT, b INT, c INT)",
		"CREATE TABLE t2(a INT, b INT, d INT)",
		"CREATE TABLE t3(a INT, b INT, e INT)",
		"CREATE TABLE t4(a INT, b INT, f INT)",
		"CREATE TABLE t5(a INT, b INT, g INT)",
		"INSERT INTO t1 VALUES(15,25,35)",
		"INSERT INTO t2 VALUES(15,25,35)",
		"INSERT INTO t3 VALUES(15,25,35)",
		"INSERT INTO t4 VALUES(15,25,35)",
		"INSERT INTO t5 VALUES(15,25,35)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query(`SELECT a, c, d, e, f, g
		FROM t1
		INNER JOIN (t2 INNER JOIN t3 USING(a)) USING(a)
		INNER JOIN (t4 LEFT JOIN t5 USING(a)) USING(a)
		WHERE a <= 18 ORDER BY 1 NULLS FIRST`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]interface{}{{int64(15), int64(35), int64(35), int64(35), int64(35), int64(35)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("joinB-4 rows = %#v, want %#v", res.Rows, want)
	}
}

func TestJoinHNestedUsingGroupExposesMergedColumn(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a,c)", "CREATE TABLE t2(a,d)", "CREATE TABLE t3(a,e)",
		"INSERT INTO t1 VALUES(1,2)", "INSERT INTO t2 VALUES(1,3)", "INSERT INTO t3 VALUES(1,4)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT a,c,d,e FROM t1 INNER JOIN (t2 INNER JOIN t3 USING(a)) USING(a)")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]interface{}{{int64(1), int64(2), int64(3), int64(4)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("rows = %#v, want %#v", res.Rows, want)
	}
}

func TestJoinHNestedGroupRowidAmbiguity(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"CREATE TABLE x1(a)", "CREATE TABLE x2(b)", "CREATE TABLE wo2(c PRIMARY KEY) WITHOUT ROWID"} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT rowid FROM x1 JOIN (x2 JOIN wo2)")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "ambiguous column name: rowid") {
		t.Fatalf("nested rowid ambiguity = %v, want ambiguous column name: rowid", res.Error)
	}
}

func TestJoinHDeclaredRowIDAliasNames(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE x1(a INTEGER PRIMARY KEY, b)",
		"CREATE TABLE x2(c, d)",
		"CREATE TABLE x3(rowid, _rowid_)",
		"CREATE TABLE x4(rowid, _rowid_, oid)",
		"INSERT INTO x1 VALUES(1000, 'thousand')",
		"INSERT INTO x2 VALUES('c', 'd')",
		"INSERT INTO x3(oid, rowid, _rowid_) VALUES(43, 'hello', 'world')",
		"INSERT INTO x4(oid, rowid, _rowid_) VALUES('forty three', 'hello', 'world')",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	for _, tc := range []struct {
		query string
		want  [][]interface{}
	}{
		{`SELECT x3.oid FROM x1 JOIN (x2 JOIN x3 ON c='c')`, [][]interface{}{{int64(43)}}},
		{`SELECT x3.rowid FROM x1 JOIN (x2 JOIN x3 ON c='c')`, [][]interface{}{{"hello"}}},
		{`SELECT x4.oid FROM x1 JOIN (x2 JOIN x4 ON c='c')`, [][]interface{}{{"forty three"}}},
	} {
		res := db.Query(tc.query)
		if res.Error != nil {
			t.Fatalf("Query(%q): %v", tc.query, res.Error)
		}
		if !reflect.DeepEqual(res.Rows, tc.want) {
			t.Errorf("Query(%q) rows = %#v, want %#v", tc.query, res.Rows, tc.want)
		}
	}
}

func TestJoinHNaturalLeftThenInnerAffinity(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(c0 INT)",
		"CREATE TABLE t2(c0 BLOB)",
		"CREATE TABLE t3(c0 BLOB)",
		"CREATE TABLE t4(c4 BLOB)",
		"INSERT INTO t1 VALUES(0)",
		"INSERT INTO t3 VALUES('0')",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	intermediate := db.Query("SELECT * FROM t1 NATURAL LEFT JOIN t2")
	if intermediate.Error != nil || !reflect.DeepEqual(intermediate.Rows, [][]interface{}{{int64(0)}}) {
		t.Fatalf("intermediate LEFT join rows=%#v err=%v", intermediate.Rows, intermediate.Error)
	}
	res := db.Query("SELECT * FROM t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3")
	if res.Error != nil || len(res.Rows) != 1 {
		t.Fatalf("NATURAL INNER continuation rows=%#v err=%v", res.Rows, res.Error)
	}
	full := db.Query("SELECT * FROM t1 NATURAL LEFT JOIN t2 NATURAL JOIN t3 FULL JOIN t4 ON true")
	if full.Error != nil || !reflect.DeepEqual(full.Rows, [][]interface{}{{int64(0), nil}}) {
		t.Fatalf("FULL continuation rows=%#v err=%v, want [[0 NULL]]", full.Rows, full.Error)
	}
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0] != int64(0) {
		t.Fatalf("joined rows = %#v, want [[0]]", res.Rows)
	}
}

func TestJoinHUsingRejectsDuplicateRightJoinGroupColumns(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE x0(c0)",
		"CREATE TABLE x1(c0)",
		"CREATE TABLE x2(c0)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT * FROM x0 RIGHT JOIN (x1 LEFT JOIN x2) USING(c0)")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "ambiguous column name: c0") {
		t.Fatalf("USING duplicate right columns = %v, want ambiguous column name: c0", res.Error)
	}
}

func TestJoinHUsingRejectsDuplicateParenthesizedCommaGroup(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE x0(a)",
		"CREATE TABLE x1(a)",
		"CREATE TABLE x2(a)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT * FROM x0 RIGHT JOIN (x1, x2) USING(a)")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "ambiguous column name: a") {
		t.Fatalf("USING parenthesized comma group = %v, want ambiguous column name: a", res.Error)
	}
}

func TestJoinHRightJoinRetainsDerivedLeftColumns(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t0(c0)",
		"CREATE TABLE t1(c0)",
		"CREATE TABLE t2(c0)",
		"INSERT INTO t0 VALUES('1.0')",
		"INSERT INTO t2(c0) VALUES(9)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	firstJoin := db.Query(`SELECT *
		FROM (SELECT CAST(t0.c0 AS REAL) AS c0 FROM t0) AS subquery NATURAL LEFT JOIN t1`)
	if firstJoin.Error != nil || !reflect.DeepEqual(firstJoin.Rows, [][]interface{}{{float64(1)}}) {
		t.Fatalf("before second NATURAL join rows=%#v err=%v", firstJoin.Rows, firstJoin.Error)
	}
	intermediate := db.Query(`SELECT *
			FROM (SELECT CAST(t0.c0 AS REAL) AS c0 FROM t0) AS subquery
			NATURAL LEFT JOIN t1 NATURAL JOIN t0`)
	if intermediate.Error != nil || len(intermediate.Rows) != 1 {
		t.Fatalf("before RIGHT join rows=%#v err=%v", intermediate.Rows, intermediate.Error)
	}

	res := db.Query(`SELECT t0.c0,t2.c0
		FROM (SELECT CAST(t0.c0 AS REAL) AS c0 FROM t0) AS subquery
		NATURAL LEFT JOIN t1 NATURAL JOIN t0 RIGHT JOIN t2 ON 1`)
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]interface{}{{"1.0", int64(9)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("join rows = %#v, want %#v", res.Rows, want)
	}
}

func TestJoinHDerivedTableStarPreservesDuplicateColumns(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{"CREATE TABLE x1(a TEXT)", "CREATE TABLE x2(a TEXT)", "INSERT INTO x1 VALUES('blue')", "INSERT INTO x2 VALUES('red')"} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatal(res.Error)
		}
	}
	res := db.Query("SELECT * FROM (SELECT * FROM x1, x2)")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if want := [][]interface{}{{"blue", "red"}}; !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("derived-table rows = %#v, want %#v", res.Rows, want)
	}
	if err := db.Exec("CREATE TABLE x0(a TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	res = db.Query("SELECT * FROM x0 RIGHT JOIN (SELECT * FROM x1, x2) USING(a)")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if want := [][]interface{}{{"blue", "red"}}; !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("RIGHT JOIN derived-table rows = %#v, want %#v", res.Rows, want)
	}
}

func TestJoinHRightUsingSubqueryDuplicateNameUsesFirstColumn(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t0_a(c0 INT)",
		"CREATE TABLE t0_b(c0 INT)",
		"CREATE TABLE t2(c0 INT)",
		"INSERT INTO t2 VALUES(1)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT * FROM (t0_a RIGHT JOIN (SELECT * FROM t2 LEFT JOIN t0_b) USING(c0))")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	want := [][]interface{}{{int64(1), nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("join rows = %#v, want %#v", res.Rows, want)
	}
}

func TestJoinHRowidAmbiguityInNestedJoin(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE wo1(a PRIMARY KEY, b) WITHOUT ROWID",
		"CREATE TABLE x1(a)",
		"CREATE TABLE x2(b)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, res.Error)
		}
	}
	res := db.Query("SELECT rowid FROM wo1 JOIN (x1 JOIN x2)")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "ambiguous column name: rowid") {
		t.Fatalf("nested rowid ambiguity = %v, want ambiguous column name: rowid", res.Error)
	}
}
