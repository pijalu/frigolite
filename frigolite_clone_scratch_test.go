package frigolite

import (
	"sync"
	"testing"
)

// TestCloneScratchReuseShapes pins the depth-indexed clone scratch's reuse
// discipline against the shapes that alias if a recycled clone is handed to
// two coexisting levels of one substitution: UNION chains (head vs member),
// WITH-clause bodies cloned before the INSERT body (the pinned walk order),
// and multi-row VALUES chains. Every round re-executes the same shapes with
// different literals through the same connection — each round's clone is the
// previous round's retired tenant — and checks exact row sets.
func TestCloneScratchReuseShapes(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	mustExec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")
	mustExec("CREATE TABLE u(id INTEGER PRIMARY KEY, c TEXT)")
	// Seed the template cache (first parse of each shape stores the template).
	for round := 0; round < 40; round++ {
		// INSERT ... VALUES through the template cache (clone reuse round 2+).
		if r := db.Exec(renderQ("INSERT INTO t VALUES(?,?)", int64(round), int64(round*3+1))); r.Error != nil {
			t.Fatalf("round %d insert: %v", round, r.Error)
		}
		// UNION chain: head and member must be distinct clones.
		r := db.Query(renderQ("SELECT id FROM t WHERE id=? UNION SELECT id FROM t WHERE id=?", int64(round), int64(round-1)))
		if r.Error != nil {
			t.Fatalf("round %d union: %v", round, r.Error)
		}
		wantRows := 1
		if round > 0 {
			wantRows = 2
		}
		if len(r.Rows) != wantRows {
			t.Fatalf("round %d union rows = %d, want %d", round, len(r.Rows), wantRows)
		}
		// WITH-body-cloned-first INSERT ... SELECT (CTE body and the INSERT's
		// SELECT must be distinct clones).
		if r := db.Exec(renderQ("WITH s(x) AS (SELECT ? UNION ALL SELECT ?+100) INSERT INTO u SELECT x, 'r' || x FROM s", int64(round*1000), int64(round*1000))); r.Error != nil {
			t.Fatalf("round %d cte insert: %v", round, r.Error)
		}
	}
	// Final state proves no tenant clobbered another level's clone.
	r := db.Query("SELECT COUNT(*) FROM t")
	if r.Error != nil || r.Rows[0][0].(int64) != 40 {
		t.Fatalf("t count = %v, want 40", r.Rows)
	}
	r = db.Query("SELECT COUNT(*) FROM u")
	if r.Error != nil || r.Rows[0][0].(int64) != 80 {
		t.Fatalf("u count = %v, want 80", r.Rows)
	}
	r = db.Query("SELECT c FROM t WHERE id=17")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0].(int64) != 52 {
		t.Fatalf("t id=17 c = %v, want 52", r.Rows)
	}
}

// TestCloneScratchBindInterleave pins the bind-mode substitution's use of the
// same per-depth scratch as the template path: a prepared statement and
// literal-Exec statements interleave on one connection, each round rotating
// the slot's retired clones.
func TestCloneScratchBindInterleave(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	stmt, err := db.Prepare("INSERT INTO t VALUES(?, ?*2)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()
	for i := 1; i <= 30; i++ {
		if r := stmt.Exec(int64(i), int64(i)); r.Error != nil {
			t.Fatalf("stmt exec %d: %v", i, r.Error)
		}
		// A template-cache exec between bind execs shares the depth-0 slot.
		if r := db.Exec(renderQ("INSERT INTO t VALUES(?,?)", int64(1000+i), int64(i))); r.Error != nil {
			t.Fatalf("exec %d: %v", i, r.Error)
		}
	}
	r := db.Query("SELECT COUNT(*) FROM t WHERE c = id*2")
	if r.Error != nil || r.Rows[0][0].(int64) != 30 {
		t.Fatalf("bind rows = %v, want 30", r.Rows)
	}
	r = db.Query("SELECT COUNT(*) FROM t WHERE id > 1000 AND c < 1000")
	if r.Error != nil || r.Rows[0][0].(int64) != 30 {
		t.Fatalf("literal rows = %v, want 30", r.Rows)
	}
}

// TestCloneScratchConcurrentConnections is the -race stress test for the
// clone scratch: separate connections (separate engines, separate slots)
// churn the same statement shapes concurrently. Sharing one connection
// across goroutines is outside the engine's model (every per-engine cache is
// unsynchronized); this test proves the scratch adds no cross-engine state.
func TestCloneScratchConcurrentConnections(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			db, err := Open(":memory:")
			if err != nil {
				t.Error(err)
				return
			}
			defer db.Close()
			if r := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)"); r.Error != nil {
				t.Error(r.Error)
				return
			}
			for i := 1; i <= 200; i++ {
				if r := db.Exec(renderQ("INSERT INTO t VALUES(?,?)", int64(i), int64(i*7+id))); r.Error != nil {
					t.Error(r.Error)
					return
				}
				r := db.Query(renderQ("SELECT c FROM t WHERE id=?", int64(i)))
				if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0].(int64) != int64(i*7+id) {
					t.Errorf("g%d i%d: %v %v", id, i, r.Error, r.Rows)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// renderQ substitutes ?-placeholders with literal renderings so each call
// produces a fresh SQL text (template-cache hit, clone path) without adding
// a fmt dependency to the file's hot loops.
func renderQ(sql string, args ...int64) string {
	buf := make([]byte, 0, len(sql)+40)
	it := 0
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' {
			buf = appendInt(buf, args[it])
			it++
		} else {
			buf = append(buf, sql[i])
		}
	}
	return string(buf)
}

func appendInt(buf []byte, v int64) []byte {
	if v == 0 {
		return append(buf, '0')
	}
	neg := v < 0
	if neg {
		v = -v
		buf = append(buf, '-')
	}
	var digits [20]byte
	pos := len(digits)
	for v > 0 {
		pos--
		digits[pos] = byte('0' + v%10)
		v /= 10
	}
	return append(buf, digits[pos:]...)
}
