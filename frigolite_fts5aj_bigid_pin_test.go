package frigolite

import (
	"fmt"
	"strconv"
	"testing"
)

// Native pins for the superseded testgen/fts5aj and testgen/fts5bigid
// packages (T33r-fts, 2026-09-26). fts5aj's substance is a rolling-window
// DML workload with periodic 'integrity-check'; fts5bigid's is big and
// random rowids through REPLACE/DELETE/INSERT round trips. The generated
// packages were superseded for wall-clock reasons (50k/60k autocommit
// statements) and transpiler artifacts, not engine gaps; these pins hold
// the same engine-visible contracts at a scale that completes (fts5delete's
// 1.x/1.2 cover the same machinery at 5000 rows in the generated corpus).

// TestFTS5RollingWindowIntegrityPin drives the fts5aj shape: a virtual
// table fills past a fixed window size, every insert past the window
// deletes the oldest rowid, and 'integrity-check' runs at checkpoints and
// at the end. Any divergence between the in-memory index and the mirror's
// shadow state (the fts5aj 2.0 integrity-check's job) must fail here.
func TestFTS5RollingWindowIntegrityPin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must(`CREATE VIRTUAL TABLE t1 USING fts5(x);
INSERT INTO t1(t1, rank) VALUES('pgsz', 64);`)

	const window = 200
	const total = 2000
	for i := 0; i < total; i++ {
		must("INSERT INTO t1(rowid, x) VALUES(" + strconv.Itoa(i) + ", 'w" + strconv.Itoa(i%37) + " tail" + strconv.Itoa(i%11) + "');")
		if i >= window {
			must("DELETE FROM t1 WHERE rowid=" + strconv.Itoa(i-window) + ";")
		}
		if (i+1)%500 == 0 {
			must("INSERT INTO t1(t1) VALUES('integrity-check');")
		}
	}
	must("INSERT INTO t1(t1) VALUES('integrity-check');")

	rows := db.Query("SELECT count(*) FROM t1").Rows
	if n := rows[0][0].(int64); n != window {
		t.Errorf("row count after rolling window = %d, want %d", n, window)
	}
	// The newest window's terms are searchable; the oldest deleted ones are
	// gone (rowids, not terms, age out — check both term classes).
	rows = db.Query("SELECT count(*) FROM t1 WHERE t1 MATCH 'w" + strconv.Itoa((total-1)%37) + "'").Rows
	if rows[0][0].(int64) == 0 {
		t.Error("newest-window term missing after rolling deletes")
	}
	rows = db.Query("SELECT rowid FROM t1 WHERE rowid < " + strconv.Itoa(total-window)).Rows
	if len(rows) != 0 {
		t.Errorf("%d rows survived outside the window", len(rows))
	}
}

// TestFTS5BigRowidRoundTrip holds the fts5bigid contract: random rowids and
// rowids near 0x6FFFFFFFFFFFFFFF survive REPLACE, DELETE FROM and re-INSERT
// with MATCH results intact (the generated package's three phases, scaled).
func TestFTS5BigRowidRoundTrip(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	must := func(sql string) {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	count := func(sql string) int64 {
		rows := db.Query(sql).Rows
		return rows[0][0].(int64)
	}
	must("CREATE VIRTUAL TABLE x1 USING fts5(a)")

	// Phase 1 — REPLACE at pseudo-random rowids (the corpus uses random();
	// a deterministic LCG keeps the pin reproducible).
	seed := uint64(12345)
	for i := 0; i < 2000; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		rowid := int64(seed>>8) % (1 << 40)
		must(fmt.Sprintf("REPLACE INTO x1(rowid, a) VALUES(%d, 'movement at the station');", rowid))
	}
	if n := count("SELECT count(*) FROM x1 WHERE x1 MATCH 'movement'"); n == 0 {
		t.Error("phase 1: 'movement' not found after random-rowid REPLACEs")
	}

	// Phase 2 — delete everything.
	must("DELETE FROM x1")
	if n := count("SELECT count(*) FROM x1 WHERE x1 MATCH 'movement'"); n != 0 {
		t.Errorf("phase 2: %d rows survived DELETE FROM", n)
	}

	// Phase 3 — reinsert near 2^62 (0x6FFFFFFFFFFFFFFF + i).
	const base = "0x6FFFFFFFFFFFFFFF"
	for i := 0; i < 2000; i++ {
		must(fmt.Sprintf("INSERT INTO x1(rowid, a) VALUES(%s + %d, 'movement at the station');", base, i))
	}
	if n := count("SELECT count(*) FROM x1 WHERE x1 MATCH 'movement'"); n != 2000 {
		t.Errorf("phase 3: 'movement' rows = %d, want 2000", n)
	}
	rows := db.Query("SELECT rowid FROM x1 WHERE x1 MATCH 'station' LIMIT 1").Rows
	if len(rows) == 0 {
		t.Fatal("phase 3: 'station' query returned no rows")
	}
	got := rows[0][0].(int64)
	if want := int64(0x6FFFFFFFFFFFFFFF); got < want || got >= want+2000 {
		t.Errorf("phase 3: returned rowid %d outside [%d, %d)", got, want, want+2000)
	}
}
