package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestCompoundOrderPin pins compound-SELECT ORDER BY/LIMIT semantics that the
// general and materialized execution tails must agree on (oracle: sqlite3
// 3.54; TCL refs selectA-2.7/2.9/2.10/2.15, selectB-6.9, limit-7.3,
// limit-9.4).
//
// 1. A bare compound ORDER BY term inherits the LEFTMOST member's result
// column collation (resolve.c resolveCompoundOrderBy + select.c
// sqlite3MultiSelectCollSeq): "ORDER BY c" over t1(a,b,c COLLATE NOCASE)
// UNION ALL ... sorts c case-insensitively.
// 2. An explicit COLLATE on an ORDER BY term that must be rewritten to a
// non-first member's result position is preserved (selectA-2.15 "ORDER BY b
// COLLATE NOCASE" over "SELECT x,y,z FROM t2 UNION ALL SELECT a,b,c FROM t1").
// 3. A compound's trailing LIMIT/OFFSET applies ONCE to the merged rows, not
// per member (limit-7.3), including over FROM-subquery members whose
// materialized tail previously dropped them entirely (limit-9.4).
func TestCompoundOrderPin(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a,b,c COLLATE NOCASE)",
		"INSERT INTO t1 VALUES(1,'a','a'),(9.9,'b','B'),(NULL,'C','c'),('hello','d','D'),(x'616263','e','e')",
		"CREATE TABLE t2(x,y,z COLLATE NOCASE)",
		"INSERT INTO t2 VALUES(NULL,'U','u'),('mad','Z','z'),(x'68617265','m','M'),(5.2e6,'X','x'),(-23,'Y','y')",
		"CREATE TABLE t6(v)",
		"INSERT INTO t6 VALUES(1),(2),(3),(4)",
		"CREATE TABLE t7(w)",
		"INSERT INTO t7 VALUES(1),(2),(3)",
		// selectB's schema: t1(a,b,c), t2(d,e,f).
		"CREATE TABLE s1(a,b,c)",
		"INSERT INTO s1 VALUES(2,4,6),(8,10,12),(14,16,18)",
		"CREATE TABLE s2(d,e,f)",
		"INSERT INTO s2 VALUES(3,6,9),(12,15,18),(21,24,27)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	render := func(rows [][]interface{}) string {
		var sb strings.Builder
		for i, row := range rows {
			if i > 0 {
				sb.WriteByte('|')
			}
			for j, v := range row {
				if j > 0 {
					sb.WriteByte(' ')
				}
				if v == nil {
					sb.WriteString("{}")
					continue
				}
				if b, ok := v.([]byte); ok {
					sb.Write(b)
					continue
				}
				sb.WriteString(fmt.Sprintf("%v", v))
			}
		}
		return sb.String()
	}
	for _, tc := range []struct{ q, want string }{
		// selectA-2.7: bare ORDER BY c sorts NOCASE (leftmost c is NOCASE).
		{"SELECT a,b,c FROM t1 UNION ALL SELECT x,y,z FROM t2 ORDER BY c,b,a",
			"1 a a|9.9 b B|{} C c|hello d D|abc e e|hare m M|{} U u|5.2e+06 X x|-23 Y y|mad Z z"},
		// selectA-2.9: c DESC NOCASE.
		{"SELECT a,b,c FROM t1 UNION ALL SELECT x,y,z FROM t2 ORDER BY c DESC,a,b",
			"mad Z z|-23 Y y|5.2e+06 X x|{} U u|hare m M|abc e e|hello d D|{} C c|9.9 b B|1 a a"},
		// selectA-2.15: explicit COLLATE survives the position rewrite.
		{"SELECT x,y,z FROM t2 UNION ALL SELECT a,b,c FROM t1 ORDER BY b COLLATE NOCASE,a,c",
			"1 a a|9.9 b B|{} C c|hello d D|abc e e|hare m M|{} U u|5.2e+06 X x|-23 Y y|mad Z z"},
		// selectA-2.10: explicit COLLATE BINARY overrides the column collation.
		{"SELECT a,b,c FROM t1 UNION ALL SELECT x,y,z FROM t2 ORDER BY c COLLATE BINARY DESC,a,b",
			"mad Z z|-23 Y y|5.2e+06 X x|{} U u|abc e e|{} C c|1 a a|hare m M|hello d D|9.9 b B"},
		// selectB-6.9: ORDER BY term naming a NON-first member's column; the
		// EXCEPT emits the survivors and the trailing ORDER BY sorts DESC.
		{"SELECT * FROM (SELECT e FROM s2 UNION ALL SELECT f FROM s2) EXCEPT SELECT c FROM s1 ORDER BY c DESC",
			"27|24|15|9"},
		// limit-7.3: trailing LIMIT 3 OFFSET 1 over the merged rows.
		{"SELECT x FROM t2 UNION ALL SELECT v FROM t6 LIMIT 3 OFFSET 1",
			"mad|hare|5.2e+06"},
		// limit-9.4: UNION over FROM-subqueries with their own LIMIT; the
		// outer trailing LIMIT 2 must still cut the merged rows.
		{"SELECT * FROM (SELECT * FROM t6 LIMIT 3) UNION SELECT * FROM (SELECT * FROM t7 LIMIT 3) ORDER BY 1 LIMIT 2",
			"1|2"},
	} {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Errorf("%s => ERR %v", tc.q, r.Error)
			continue
		}
		if got := render(r.Rows); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}
}
