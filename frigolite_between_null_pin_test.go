package frigolite

import "testing"

// TestNativeBetweenNullBoundKleene pins SQLite's BETWEEN lowering
// (expr.c's TK_BETWEEN case: `x >= low AND x <= high` with a Kleene AND —
// FALSE dominates NULL). The old engine short-circuited any NULL bound to
// NULL, which the indexexpr2 8.x probes refute. All expectations were
// verified against sqlite3 3.54.
func TestNativeBetweenNullBoundKleene(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	mustExec(`CREATE TABLE t0(c0)`)
	mustExec(`CREATE INDEX i0 ON t0(c0) WHERE c0 NOT NULL`)
	mustExec(`INSERT INTO t0(c0) VALUES (NULL)`)
	cases := []struct {
		sql  string
		want string
	}{
		// The indexexpr2-8.1 oracle probes: ''<=1 is FALSE, so
		// '' BETWEEN NULL AND TRUE is FALSE, not NULL.
		{`SELECT ~('' BETWEEN c0 AND TRUE) FROM t0`, "-1"},
		{`SELECT (0 == (34 BETWEEN c0 AND 33)) IS TRUE FROM t0`, "1"},
		// Scalar corners.
		{`SELECT 5 BETWEEN NULL AND 100`, ""},
		{`SELECT 5 BETWEEN NULL AND 0`, "0"},
		{`SELECT 5 BETWEEN 1 AND NULL`, ""},
		{`SELECT NULL BETWEEN 1 AND 2`, ""},
		{`SELECT 5 NOT BETWEEN NULL AND 0`, "1"},
		{`SELECT 5 NOT BETWEEN NULL AND 100`, ""},
		{`SELECT 5 NOT BETWEEN 1 AND 10`, "0"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error != nil {
			t.Errorf("%s: %v", tc.sql, res.Error)
			continue
		}
		got := ""
		if len(res.Rows) > 0 && len(res.Rows[0]) > 0 && res.Rows[0][0] != nil {
			got = formatHarnessValue("", res.Rows[0][0])
		}
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.sql, got, tc.want)
		}
	}
}
