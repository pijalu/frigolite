package frigolite

import (
	"fmt"
	"testing"
)

// TestScan2Pin pins the scan/aggregate pipeline contracts the
// PERF.PARITY-scan2 tranche touches: the bare-output fast path must not leak
// affinity wrappers into results, the INTEGER PRIMARY KEY rowid-alias fill
// must survive a wrapper-free affinity plan, an output alias referenced by a
// consuming clause must keep its underlying column decoded and wrapped, and
// positional GROUP BY retention must preserve group membership, HAVING,
// window-over-group, and blob-row semantics exactly.
func TestScan2Pin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t (c INTEGER)",
		"CREATE TABLE ipk (id INTEGER PRIMARY KEY, c INTEGER)",
		"CREATE TABLE cn (k TEXT COLLATE NOCASE, v INTEGER)",
		"CREATE TABLE blobt (k INTEGER, b BLOB)",
		"INSERT INTO t VALUES (1),(2),(3),(4),(5)",
		"INSERT INTO ipk VALUES (1, 10), (2, 20), (3, NULL)",
		"INSERT INTO cn VALUES ('abc', 1), ('ABC', 2), ('aBc', 3), ('xyz', 4)",
		"INSERT INTO blobt VALUES (1, x'0102'), (2, x'0304'), (1, x'0506')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}

	// rows renders each row space-joined, rows newline-joined (the harness
	// flatten shape), after unwrapping: a leaked *util.ColumnValue prints
	// differently from its raw scalar, so any wrapper leak changes the text.
	rows := func(q string) string {
		r := db.Query(q)
		if r.Error != nil {
			return "ERR " + r.Error.Error()
		}
		out := ""
		for i, row := range r.Rows {
			if i > 0 {
				out += "\n"
			}
			for j, v := range row {
				if j > 0 {
					out += " "
				}
				out += fmt.Sprintf("%v", v)
			}
		}
		return out
	}
	// firstCellClass reports the Go class of the first row's first cell: the
	// strongest wrapper-leak discriminator (a *util.ColumnValue or
	// *CollatedValue pointer would surface as its own class, never as the
	// scalar's).
	firstCellClass := func(q string) string {
		r := db.Query(q)
		if r.Error != nil {
			return "ERR " + r.Error.Error()
		}
		if len(r.Rows) == 0 || len(r.Rows[0]) == 0 {
			return "empty"
		}
		switch r.Rows[0][0].(type) {
		case int64:
			return "int64"
		case float64:
			return "float64"
		case string:
			return "string"
		case nil:
			return "nil"
		default:
			return fmt.Sprintf("%T", r.Rows[0][0])
		}
	}

	cases := []struct{ q, want string }{
		// Bare-output fast path: values identical to the generic builder.
		{"SELECT c FROM t", "1\n2\n3\n4\n5"},
		{"SELECT c, c FROM t", "1 1\n2 2\n3 3\n4 4\n5 5"},
		// IPK rowid-alias fill survives the fill-only affinity plan
		// (SELECT id must emit the rowid, never the stored NULL).
		{"SELECT id FROM ipk", "1\n2\n3"},
		{"SELECT * FROM ipk", "1 10\n2 20\n3 <nil>"},
		{"SELECT id, c FROM ipk WHERE c > 5", "1 10\n2 20"},
		{"SELECT id, c FROM ipk WHERE id >= 2", "2 20\n3 <nil>"},
		// Wrapper-free scan output feeds GROUP BY / HAVING / ORDER BY with
		// identical semantics.
		{"SELECT c, COUNT(*) FROM t GROUP BY c", "1 1\n2 1\n3 1\n4 1\n5 1"},
		{"SELECT c, COUNT(*) FROM t GROUP BY c HAVING COUNT(*) > 1", ""},
		{"SELECT c AS x FROM t ORDER BY x DESC", "5\n4\n3\n2\n1"},
		{"SELECT c, COUNT(*) FROM t GROUP BY c ORDER BY SUM(c) DESC LIMIT 2", "5 1\n4 1"},
		// Declared-collation grouping through a positional group set: the
		// CollatedValue marker must survive (NOCASE merges 'abc'/'ABC'/'aBc').
		{"SELECT k, COUNT(*) FROM cn GROUP BY k", "abc 3\nxyz 1"},
		{"SELECT k FROM cn GROUP BY k COLLATE NOCASE", "abc\nxyz"},
		{"SELECT k, SUM(v) FROM cn GROUP BY k HAVING SUM(v) > 3", "abc 6\nxyz 4"},
		// Blob rows through the positional arena (deep-copy discipline).
		{"SELECT k, COUNT(*), GROUP_CONCAT(HEX(b)) FROM blobt GROUP BY k", "1 2 0102,0506\n2 1 0304"},
		{"SELECT k, COUNT(*) FROM blobt GROUP BY k ORDER BY k DESC", "2 1\n1 2"},
		// Aggregate scans (feed + generic min/max path).
		{"SELECT SUM(c), COUNT(*), MIN(c), MAX(c) FROM t", "15 5 1 5"},
		{"SELECT SUM(c), COUNT(*) FROM t WHERE c > 2", "12 3"},
		// Window over GROUP BY (per-group map materialization path).
		{"SELECT c, COUNT(*), COUNT(*) OVER () FROM t GROUP BY c", "1 1 5\n2 1 5\n3 1 5\n4 1 5\n5 1 5"},
		{"SELECT c, SUM(c) OVER (ORDER BY c ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t WHERE c <= 3", "1 1\n2 3\n3 5"},
		// reverse_unordered_selects does not change membership.
		{"SELECT k, COUNT(*) FROM blobt GROUP BY k ORDER BY k", "1 2\n2 1"},
	}
	for _, tc := range cases {
		if got := rows(tc.q); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}

	// An output alias referenced by a consuming clause resolves back to the
	// SELECT expression: the underlying column must keep its historical
	// decode+wrap treatment (collate8-2.1/2.2 class).
	aliasCases := []struct{ q, want string }{
		{"SELECT v AS x FROM cn WHERE x = 1", "1"},
		{"SELECT v AS x FROM cn WHERE x = 1 OR x = 4", "1\n4"},
		{"SELECT v AS x FROM cn ORDER BY x DESC", "4\n3\n2\n1"},
		{"SELECT v AS x FROM cn GROUP BY x", "1\n2\n3\n4"},
		{"SELECT v AS x, COUNT(*) FROM cn GROUP BY x HAVING COUNT(*) >= 1", "1 1\n2 1\n3 1\n4 1"},
	}
	for _, tc := range aliasCases {
		if got := rows(tc.q); got != tc.want {
			t.Errorf("%s => got [%s] want [%s]", tc.q, got, tc.want)
		}
	}

	// Output cells are raw scalars: no affinity wrapper leaks into any
	// output shape (bare fast path, star path, IPK fill, positional groups).
	for _, tc := range []struct{ q, class string }{
		{"SELECT c FROM t", "int64"},
		{"SELECT c AS x FROM t", "int64"},
		{"SELECT id FROM ipk", "int64"},
		{"SELECT * FROM ipk", "int64"},
		{"SELECT k, COUNT(*) FROM cn GROUP BY k", "string"},
		{"SELECT c, COUNT(*) FROM t GROUP BY c", "int64"},
		{"SELECT id, COUNT(*) FROM ipk GROUP BY id", "int64"},
	} {
		if got := firstCellClass(tc.q); got != tc.class {
			t.Errorf("%s first cell => %q want %s", tc.q, got, tc.class)
		}
	}
}
