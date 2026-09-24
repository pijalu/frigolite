package frigolite_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// Pins for FULL-SUITE-DRIFT.T32-collate (fleet/collate-res): the compound
// set-operator merge-key SURVIVOR representation and the GROUP BY compound
// collation-key merge, both behind collate5 (testgen + JSON harness), and
// the collate1 hex UDF fixture contract. Expectations oracle-verified
// against sqlite3 3.54 (the collate5.test TCL wants are current-suite
// green there).

// collateResMustQuery runs q and returns the flattened single-column (or
// joined multi-column) rendering, one space per row, NULL as {}.
func collateResMustQuery(t *testing.T, db *frigolite.DB, q string) string {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("query %q: %v", q, r.Error)
	}
	var out []string
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				parts[i] = "{}"
				continue
			}
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
				continue
			}
			switch tv := v.(type) {
			case float64:
				parts[i] = strconv.FormatFloat(tv, 'g', -1, 64)
			default:
				parts[i] = fmt.Sprintf("%v", tv)
			}
		}
		out = append(out, strings.Join(parts, " "))
	}
	return strings.Join(out, " ")
}

// TestPinCompoundSetOpSurvivor: a compound UNION/EXCEPT/INTERSECT merges
// through a temp b-tree keyed by the compound's per-column collations where
// the LAST row inserted for a key supplies the surviving REPRESENTATION
// (select.c merge storage overwrites the payload on duplicate keys):
// collate5-2.1.1 UNION {A B N}, collate5-2.1.3 UNION (a,b)
// {A Apple A apple B Banana b banana N {}}, collate5-2.2.1 EXCEPT {N},
// collate5-2.2.3 EXCEPT (a,b) {A Apple N {}}, collate5-2.3.1 INTERSECT
// {A B}, collate5-2.3.3 INTERSECT (a,b) {a apple B banana}.
func TestPinCompoundSetOpSurvivor(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// collate5-1.0 (t1) and collate5-2.0 (t2) — the TCL section-2 t2.
	stmts := []string{
		"CREATE TABLE t1(a COLLATE nocase, b COLLATE binary)",
		"INSERT INTO t1 VALUES('a','apple'),('A','Apple'),('b','banana')," +
			"('B','banana'),('n',NULL),('N',NULL)",
		"CREATE TABLE t2(a COLLATE binary, b COLLATE nocase)",
		"INSERT INTO t2 VALUES('a','apple'),('A','apple'),('b','banana')," +
			"('B','Banana')",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
	cases := []struct{ name, q, want string }{
		{"2.1.1", "SELECT a FROM t1 UNION SELECT a FROM t2", "A B N"},
		{"2.1.3", "SELECT a, b FROM t1 UNION SELECT a, b FROM t2",
			"A Apple A apple B Banana b banana N {}"},
		{"2.2.1", "SELECT a FROM t1 EXCEPT SELECT a FROM t2", "N"},
		{"2.2.3", "SELECT a, b FROM t1 EXCEPT SELECT a, b FROM t2",
			"A Apple N {}"},
		{"2.3.1", "SELECT a FROM t1 INTERSECT SELECT a FROM t2", "A B"},
		{"2.3.3", "SELECT a, b FROM t1 INTERSECT SELECT a, b FROM t2",
			"a apple B banana"},
	}
	for _, tc := range cases {
		if got := collateResMustQuery(t, db, tc.q); got != tc.want {
			t.Errorf("%s: %s\n  got:  %s\n  want: %s", tc.name, tc.q, got, tc.want)
		}
	}
}

// TestPinGroupByCollationKeyMerge: compound GROUP BY keys compare per-term
// under the term's collation — values equal under a term's collation share
// a group even when their textual forms differ (collate5-4.2: '1' and
// '1.0' under a COLLATE NUMERIC column; numeric_collate is TCL ==, numeric
// equality). The group count is 3, not 4.
func TestPinGroupByCollationKeyMerge(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The TCL numeric_collate proc: TCL == (numeric) then a numeric >.
	db.RegisterCollation("numeric", func(a, b string) int {
		af, aerr := strconv.ParseFloat(a, 64)
		bf, berr := strconv.ParseFloat(b, 64)
		if aerr == nil && berr == nil {
			if af == bf {
				return 0
			}
			if af > bf {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	stmts := []string{
		"CREATE TABLE t4(a COLLATE nocase, b COLLATE numeric)",
		"INSERT INTO t4 VALUES('a','1'),('A','1.0'),('b','2'),('B','3')",
	}
	for _, s := range stmts {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %q: %v", s, r.Error)
		}
	}
	got := collateResMustQuery(t, db,
		"SELECT a, b, count(*) FROM t4 GROUP BY a, b ORDER BY a, b")
	// The representative row of the merged (a,'1'/'1.0') group is one of
	// ('a','1') / ('A','1.0') — accept either, like the TCL /regex/ want.
	if got != "a 1 2 b 2 1 B 3 1" && got != "A 1.0 2 b 2 1 B 3 1" {
		t.Errorf("collate5-4.2 GROUP BY merge\n  got:  %s\n  want: [aA] 1(.0)? 2 [bB] 2 1 [bB] 3 1", got)
	}
}
