// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"fmt"
	"sync"
	"testing"
)

// Pooled-wrapper stress (PERF.PARITY-wrap): the btree wrapper/cursor pool
// re-arms recycled objects per statement, and the lock-key / normalize
// scratch caches persist across statements. A reset bug shows up as
// wrong-table reads, lost cursors, or stale lock keys — this loop interleaves
// multi-table statements, mid-loop DDL (root-page and wrapper-pool tenant
// changes), WITHOUT ROWID trees (per-tree key comparators), trigger-driven
// nested statements (mid-scan writes, the misc8-1.6 shape), and ATTACH/DETACH
// (context cache eviction), asserting exact results throughout.

func stressExec(t *testing.T, c *DB, sql string) *Result {
	t.Helper()
	r := c.Exec(sql)
	if r.Error != nil {
		t.Fatalf("exec %q: %v", sql, r.Error)
	}
	return r
}

func stressQueryInt(t *testing.T, c *DB, sql string) int64 {
	t.Helper()
	r := c.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %q: %v", sql, r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("query %q: got %d rows, want 1", sql, len(r.Rows))
	}
	v, ok := r.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("query %q: non-integer %v", sql, r.Rows[0][0])
	}
	return v
}

func stressQueryText(t *testing.T, c *DB, sql string) string {
	t.Helper()
	r := c.Query(sql)
	if r.Error != nil {
		t.Fatalf("query %q: %v", sql, r.Error)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("query %q: got %d rows, want 1", sql, len(r.Rows))
	}
	s, _ := r.Rows[0][0].(string)
	return s
}

// TestPoolStressMultiTableDDL loops statements across several tables with
// schema churn, verifying every statement's effect exactly.
func TestPoolStressMultiTableDDL(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	stressExec(t, c, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT, c INTEGER)")
	stressExec(t, c, "CREATE TABLE wr(k TEXT PRIMARY KEY, v INTEGER) WITHOUT ROWID")
	stressExec(t, c, "CREATE INDEX idx_t1_c ON t1(c)")

	for i := 0; i < 400; i++ {
		// Rowid table: insert, update, read back.
		stressExec(t, c, fmt.Sprintf("INSERT INTO t1 VALUES(%d, 'x%d', %d)", i, i, i*2))
		stressExec(t, c, fmt.Sprintf("UPDATE t1 SET c = %d WHERE a = %d", i*2+1, i))
		if got := stressQueryInt(t, c, fmt.Sprintf("SELECT c FROM t1 WHERE a = %d", i)); got != int64(i*2+1) {
			t.Fatalf("iter %d: t1 c = %d", i, got)
		}
		// WITHOUT ROWID tree: the pooled wrapper must not inherit a previous
		// tenant's key comparator (rowid tables on the same pool).
		stressExec(t, c, fmt.Sprintf("INSERT INTO wr VALUES('k%d', %d)", i, i))
		if got := stressQueryText(t, c, fmt.Sprintf("SELECT k FROM wr WHERE v = %d", i)); got != fmt.Sprintf("k%d", i) {
			t.Fatalf("iter %d: wr k = %q", i, got)
		}
		// Index seek over the secondary index.
		if got := stressQueryInt(t, c, fmt.Sprintf("SELECT a FROM t1 WHERE c = %d", i*2+1)); got != int64(i) {
			t.Fatalf("iter %d: index seek a = %d", i, got)
		}
		// Point delete keeps the table size predictable.
		stressExec(t, c, fmt.Sprintf("DELETE FROM t1 WHERE a = %d", i))
		if got := stressQueryInt(t, c, "SELECT count(*) FROM t1"); got != 0 {
			t.Fatalf("iter %d: t1 count = %d", i, got)
		}
		// Mid-loop schema churn: the statement-side trees and caches must
		// survive a DROP/CREATE of another table (and of the same name —
		// pooled wrappers see new roots under an old key).
		switch i % 4 {
		case 0:
			stressExec(t, c, "CREATE TABLE churn(id INTEGER PRIMARY KEY, z INTEGER)")
		case 1:
			stressExec(t, c, "INSERT INTO churn VALUES(1, 10)")
		case 2:
			stressExec(t, c, "DROP TABLE churn")
			stressExec(t, c, "CREATE TABLE churn(id INTEGER PRIMARY KEY, z INTEGER)")
		case 3:
			stressExec(t, c, "DROP TABLE churn")
		}
		// The wr table must still hold every earlier row after churn.
		if got := stressQueryInt(t, c, "SELECT count(*) FROM wr"); got != int64(i+1) {
			t.Fatalf("iter %d: wr count = %d", i, got)
		}
	}
	// Scan the WITHOUT ROWID tree end-to-end (cursor path stack reuse).
	if got := stressQueryInt(t, c, "SELECT sum(v) FROM wr"); got != 399*400/2 {
		t.Fatalf("wr sum = %d", got)
	}
}

// TestPoolStressTriggerNestedWrite is the misc8-1.6 shape on pooled
// wrappers: an outer scan cursor stays positioned while a trigger's nested
// statement deletes rows from the table being scanned (a nested write saves
// and restores the outer cursor through the cross-statement registry).
func TestPoolStressTriggerNestedWrite(t *testing.T) {
	c, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	stressExec(t, c, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)")
	stressExec(t, c, "CREATE TABLE log(m INTEGER)")
	for i := 0; i < 100; i++ {
		stressExec(t, c, fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d)", i, i))
	}
	// The trigger's DELETE runs while an outer statement scans t1.
	stressExec(t, c, "CREATE TRIGGER tr AFTER INSERT ON log BEGIN DELETE FROM t1 WHERE a = NEW.m; END")

	// Fire the trigger 30 times; each fire deletes exactly one t1 row.
	for i := 0; i < 30; i++ {
		stressExec(t, c, fmt.Sprintf("INSERT INTO log VALUES(%d)", i))
	}
	if got := stressQueryInt(t, c, "SELECT count(*) FROM t1"); got != 70 {
		t.Fatalf("t1 count = %d, want 70", got)
	}
	// Scans stay correct afterwards: full range reads every remaining row.
	if got := stressQueryInt(t, c, "SELECT count(*) FROM t1 WHERE a < 100"); got != 70 {
		t.Fatalf("post-delete scan count = %d, want 70", got)
	}
	if got := stressQueryInt(t, c, "SELECT sum(a) FROM t1"); got != int64(99*100/2-30*29/2) {
		t.Fatalf("post-delete sum = %d", got)
	}
	// Mid-scan nested write (eval() shape): an UPDATE scans t2 while its own
	// trigger deletes OTHER rows of the same table (rows 50..99 die as rows
	// 0..49 are updated) — the scan cursor must be saved/restored around the
	// nested statement's writes.
	stressExec(t, c, "DROP TRIGGER tr")
	stressExec(t, c, "DROP TABLE t1")
	stressExec(t, c, "ALTER TABLE log RENAME TO t1_old")
	stressExec(t, c, "CREATE TABLE t2(a INTEGER PRIMARY KEY, b INTEGER)")
	for i := 0; i < 100; i++ {
		stressExec(t, c, fmt.Sprintf("INSERT INTO t2 VALUES(%d, %d)", i, i))
	}
	stressExec(t, c, "CREATE TRIGGER tr2 AFTER UPDATE ON t2 BEGIN DELETE FROM t2 WHERE a = OLD.a + 50; END")
	r := stressExec(t, c, "UPDATE t2 SET b = b WHERE a < 50")
	if r.Changes != 50 {
		t.Fatalf("update changes = %d, want 50", r.Changes)
	}
	if got := stressQueryInt(t, c, "SELECT count(*) FROM t2"); got != 50 {
		t.Fatalf("t2 count after mid-scan trigger deletes = %d, want 50", got)
	}
	if got := stressQueryInt(t, c, "SELECT sum(a) FROM t2"); got != 1225 {
		t.Fatalf("t2 sum after mid-scan trigger deletes = %d, want 1225", got)
	}
}

// TestPoolStressAttachDetach cycles ATTACH/DETACH while statements run: the
// memoized lock keys must evict with the context (a DETACHed file's key must
// never answer for a later ATTACH of the same path).
func TestPoolStressAttachDetach(t *testing.T) {
	c1, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	stressExec(t, c1, "CREATE TABLE t1(a INTEGER PRIMARY KEY, b TEXT)")
	for i := 0; i < 50; i++ {
		stressExec(t, c1, fmt.Sprintf("INSERT INTO t1 VALUES(%d, 'x%d')", i, i))
	}

	path := t.TempDir() + "/pool_stress_aux.db"
	c2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stressExec(t, c2, "CREATE TABLE aux(id INTEGER PRIMARY KEY, v TEXT)")
	stressExec(t, c2, "INSERT INTO aux VALUES(7, 'seven')")
	c2.Close()

	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("aux%d", i%2)
		stressExec(t, c1, fmt.Sprintf("ATTACH '%s' AS %s", path, name))
		if got := stressQueryText(t, c1, "SELECT v FROM aux WHERE id = 7"); got != "seven" {
			t.Fatalf("iter %d: aux v = %q", i, got)
		}
		// Statement over the MAIN database while attached.
		if got := stressQueryInt(t, c1, fmt.Sprintf("SELECT count(*) FROM t1 WHERE a = %d", i)); got != 1 {
			t.Fatalf("iter %d: main lookup = %d", i, got)
		}
		stressExec(t, c1, "DETACH "+name)
		if got := stressQueryInt(t, c1, "SELECT count(*) FROM t1"); got != 50 {
			t.Fatalf("iter %d: main count = %d", i, got)
		}
	}
}

// TestPoolStressConcurrentOpenCloseDDL is the -race stress for the
// ownership-token wrapper reuse (PERF.BTREEUSE): several connections
// (separate engines, each with its own TreeFreeList, all sharing the btree
// package's GLOBAL cursor pool and append-slot registry) churn through
// open/close cycles, per-connection DDL, page-size VACUUMs (pager layout
// replacement fires the engine + DML free-list purges), and point
// statements. A recycled-wrapper reset bug or a stale-lease miss shows up
// as a DATA RACE, a nil-pager panic, or a wrong/missing row.
func TestPoolStressConcurrentOpenCloseDDL(t *testing.T) {
	dir := t.TempDir()
	const goroutines = 4
	const rounds = 12
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			path := fmt.Sprintf("%s/stress-%d.db", dir, g)
			for round := 0; round < rounds; round++ {
				c, err := Open(path)
				if err != nil {
					errs[g] = fmt.Errorf("open: %w", err)
					return
				}
				exec := func(sql string) bool {
					if r := c.Exec(sql); r.Error != nil {
						errs[g] = fmt.Errorf("round %d exec %q: %v", round, sql, r.Error)
						return false
					}
					return true
				}
				table := fmt.Sprintf("t%d_%d", g, round)
				if !exec("CREATE TABLE " + table + "(a INTEGER PRIMARY KEY, b TEXT)") {
					c.Close()
					return
				}
				if !exec("CREATE INDEX idx_" + table + " ON " + table + "(b)") {
					c.Close()
					return
				}
				for i := 0; i < 25; i++ {
					if !exec(fmt.Sprintf("INSERT INTO %s VALUES(%d, 'v%d')", table, i, i)) {
						c.Close()
						return
					}
				}
				for i := 0; i < 25; i++ {
					r := c.Query(fmt.Sprintf("SELECT b FROM %s WHERE a = %d", table, i))
					if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != fmt.Sprintf("v%d", i) {
						c.Close()
						errs[g] = fmt.Errorf("round %d seek %d: %v (rows %d)", round, i, r.Error, len(r.Rows))
						return
					}
				}
				if !exec("UPDATE "+table+" SET b = 'u' WHERE a = 3") ||
					!exec("DELETE FROM "+table+" WHERE a = 4") ||
					!exec("DROP INDEX idx_"+table) ||
					!exec("DROP TABLE "+table) {
					c.Close()
					return
				}
				// Layout replacement (VACUUM at a different page size) fires
				// the pager layout hook: both free lists must purge.
				if round%3 == 0 {
					if !exec("PRAGMA page_size=8192; VACUUM") || !exec("PRAGMA page_size=4096; VACUUM") {
						c.Close()
						return
					}
				}
				if err := c.Close(); err != nil {
					errs[g] = fmt.Errorf("close: %w", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", g, err)
		}
	}
}
