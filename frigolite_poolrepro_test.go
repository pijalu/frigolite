// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"testing"
)

// Deterministic repro candidates for the pooled-wrapper use-after-Close
// crash: an enclosing statement's scan cursor on table T must survive a
// nested statement (trigger body / eval()) that opens the SAME table
// through the statement funnel and ends mid-scan.

func TestPoolReproEvalDuringScan(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mustExecRepro(t, c, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)")
	for i := 0; i < 200; i++ {
		mustExecRepro(t, c, fmt.Sprintf("INSERT INTO t1 VALUES(%d, 'x%d')", i, i))
	}
	// eval() opens t1 through the funnel while the outer SELECT scan holds a
	// cursor on the same (pager, rootPage).
	r := c.Query("SELECT count(*) FROM t1 WHERE eval('SELECT count(*) FROM t1') IS NOT NULL")
	if r.Error != nil {
		t.Fatalf("eval-during-scan: %v", r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("eval-during-scan rows = %d", len(r.Rows))
	}
}

func TestPoolReproCTEEval(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mustExecRepro(t, c, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)")
	for i := 0; i < 200; i++ {
		mustExecRepro(t, c, fmt.Sprintf("INSERT INTO t1 VALUES(%d, 'x%d')", i, i))
	}
	r := c.Query("WITH c AS (SELECT * FROM t1) SELECT count(*) FROM c WHERE eval('SELECT count(*) FROM t1') IS NOT NULL")
	if r.Error != nil {
		t.Fatalf("cte+eval: %v", r.Error)
	}
}

func TestPoolReproCTEScanNestedWrite(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mustExecRepro(t, c, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)")
	mustExecRepro(t, c, "CREATE TABLE trg(m INTEGER)")
	for i := 0; i < 300; i++ {
		mustExecRepro(t, c, fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d)", i, i))
	}
	mustExecRepro(t, c, "CREATE TRIGGER tr AFTER INSERT ON trg BEGIN DELETE FROM t1 WHERE a = NEW.m; END")
	// CTE materialized over t1 while nested trigger writes hit t1.
	r := c.Query("WITH c(x) AS (SELECT count(*) FROM t1) SELECT (SELECT x FROM c) + (SELECT count(*) FROM t1)")
	if r.Error != nil {
		t.Fatalf("cte nested write: %v", r.Error)
	}
}

func mustExecRepro(t *testing.T, c *DB, sql string) {
	t.Helper()
	if r := c.Exec(sql); r.Error != nil {
		t.Fatalf("exec %q: %v", sql, r.Error)
	}
}
