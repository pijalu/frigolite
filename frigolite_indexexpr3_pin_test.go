// SPDX-License-Identifier: GPL-3.0-or-later

package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestIndexexpr3FunctionCount pins the engine-visible contract behind
// indexexpr3-1.5/1.6: `do_hasfunction_test` counts OP_Function rows in the
// EXPLAIN stream before running the SELECT. The fixture's SELECT 0 marker
// is a converter artifact only when that count is really two.
func TestIndexexpr3FunctionCount(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	setup := db.Exec(`
		CREATE TABLE t1(a, j);
		INSERT INTO t1 VALUES(1, '{x:"one"}');
		INSERT INTO t1 VALUES(2, '{x:"two"}');
		INSERT INTO t1 VALUES(3, '{x:"three"}');
		CREATE INDEX i1 ON t1( json_extract(j, '$.x') );
		CREATE INDEX i2 ON t1( a, json_extract(j, '$.x') );
	`)
	if setup.Error != nil {
		t.Fatal(setup.Error)
	}
	queries := []string{
		`SELECT json_insert( '{}', '$.y', json_extract(j, '$.x') ) FROM t1 WHERE a=2`,
		`SELECT json_insert( '{}', '$.y', coalesce( json_extract(j, '$.x'), 'five' ) ) FROM t1 WHERE a=2`,
	}
	for _, q := range queries {
		explain := db.Query("EXPLAIN " + q)
		if explain.Error != nil {
			t.Fatalf("EXPLAIN %q: %v", q, explain.Error)
		}
		n := 0
		for _, op := range explainColumn(explain, "opcode") {
			if op == "Function" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("Function rows for %q = %d, want 2; ops=%s", q, n, strings.Join(explainColumn(explain, "opcode"), " "))
		}
		res := db.Query(q)
		if res.Error != nil {
			t.Fatalf("query %q: %v", q, res.Error)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
			t.Fatalf("query %q rows=%q", q, fmt.Sprintf("%v", res.Rows))
		}
		got := fmt.Sprintf("%v", res.Rows[0][0])
		if !strings.Contains(got, `{"y":"two"}`) {
			t.Errorf("query %q got=%q", q, got)
		}
	}
}
