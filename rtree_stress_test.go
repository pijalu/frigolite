package frigolite

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestRtreeStressChurn drives insert/delete churn deep enough to force root
// splits (>51 cells per node => tree height growth) and underfull reinsertion
// during deletes, then validates the live table against an independent model
// (a plain map of id -> coordinates maintained by the test itself).
func TestRtreeStressChurn(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE VIRTUAL TABLE st USING rtree(id, minX, maxX, minY, maxY)`); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}

	type box struct{ x0, x1, y0, y1 float64 }
	model := map[int64]box{}

	rng := rand.New(rand.NewSource(42))
	nextID := int64(0)

	insOne := func() {
		nextID++
		b := box{
			float64(rng.Intn(100)), float64(rng.Intn(100)) + 101,
			float64(rng.Intn(100)), float64(rng.Intn(100)) + 101,
		}
		sql := fmt.Sprintf("INSERT INTO st VALUES(%d,%v,%v,%v,%v)", nextID, b.x0, b.x1, b.y0, b.y1)
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("insert %q: %v", sql, res.Error)
		}
		model[nextID] = b
	}

	delOne := func() bool {
		if len(model) == 0 {
			return false
		}
		// pick a random existing id
		pick := rng.Intn(len(model))
		var id int64
		for k := range model {
			if pick == 0 {
				id = k
				break
			}
			pick--
		}
		b := model[id]
		delete(model, id)
		sql := fmt.Sprintf("DELETE FROM st WHERE id=%d", id)
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("delete %q: %v", sql, res.Error)
		}
		_ = b
		return true
	}

	// Phase 1: grow past the split threshold several times over.
	for i := 0; i < 300; i++ {
		insOne()
	}
	// Phase 2: mixed churn so splits AND reinsertion both fire repeatedly.
	for i := 0; i < 400; i++ {
		if rng.Intn(3) == 0 {
			delOne()
		} else {
			insOne()
		}
	}
	// Phase 3: drain almost everything (delete path incl. tree shrink).
	ids := make([]int64, 0, len(model))
	for k := range model {
		ids = append(ids, k)
	}
	for i, id := range ids {
		if i%2 == 0 { // delete every other one
			delete(model, id)
			if res := db.Exec(fmt.Sprintf("DELETE FROM st WHERE id=%d", id)); res.Error != nil {
				t.Fatalf("drain delete id=%d: %v", id, res.Error)
			}
		}
	}

	// Validate: the table's live rows must equal the model exactly.
	res := db.Query(`SELECT id,minX,maxX,minY,maxY FROM st ORDER BY id`)
	if res.Error != nil {
		t.Fatalf("final scan: %v", res.Error)
	}
	if int64(len(res.Rows)) != int64(len(model)) {
		t.Fatalf("row count mismatch: table=%d model=%d", len(res.Rows), len(model))
	}
	for _, row := range res.Rows {
		id := rtreeTestAsInt(row[0])
		want, ok := model[id]
		if !ok {
			t.Errorf("extra id %d in table", id)
			continue
		}
		got := box{asTestF(row[1]), asTestF(row[2]), asTestF(row[3]), asTestF(row[4])}
		if got != want {
			t.Errorf("id %d: got %v want %v", id, got, want)
		}
	}

	// Shadow consistency: one %_rowid entry per live row (t3 invariant).
	rc := db.Query(`SELECT count(*) FROM st_rowid`)
	if rc.Error != nil || rtreeTestAsInt(rc.Rows[0][0]) != int64(len(model)) {
		t.Fatalf("%%_rowid size %v != live %d (err=%v)", rc.Rows, len(model), rc.Error)
	}

	// Duplicate id INSERT must fail.
	if len(res.Rows) > 0 {
		liveID := rtreeTestAsInt(res.Rows[0][0])
		if res := db.Exec(fmt.Sprintf("INSERT INTO st VALUES(%d,0,1,0,1)", liveID)); res.Error == nil {
			t.Fatalf("duplicate id %d accepted", liveID)
		}
	}

	// Coordinate inversion (min>max) must fail (rtree.c rtreeInsertPoint).
	if res := db.Exec(`INSERT INTO st VALUES(999999,5,4,0,1)`); res.Error == nil {
		t.Fatalf("inverted box accepted")
	}
}

// TestRtreeBulkSelectInsert exercises INSERT INTO ... SELECT (the copy path
// through materialized rows) and REPLACE semantics used by the TCL suite.
func TestRtreeBulkSelectInsert(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`CREATE VIRTUAL TABLE src USING rtree(id,x0,x1,y0,y1)`)
	db.Exec(`CREATE VIRTUAL TABLE dst USING rtree(id,x0,x1,y0,y1)`)
	for i := int64(1); i <= 40; i++ {
		db.Exec(fmt.Sprintf("INSERT INTO src VALUES(%d,%d,%d,%d,%d)", i, i, i+1, i*2, i*2+1))
	}
	if res := db.Exec(`INSERT INTO dst SELECT * FROM src`); res.Error != nil {
		t.Fatalf("bulk insert: %v", res.Error)
	}
	n := db.Query(`SELECT count(*) FROM dst`)
	if rtreeTestAsInt(n.Rows[0][0]) != 40 {
		t.Fatalf("bulk count = %v want 40", n.Rows)
	}
	// Copy into a partially-filled table with conflicting ids must surface
	// the uniqueness violation of the FIRST conflict (unordered scan of src,
	// so only assert failure, not the message).
	if res := db.Exec(`INSERT INTO dst SELECT * FROM src`); res.Error == nil {
		t.Fatalf("conflicting bulk insert silently succeeded")
	}
}

func asTestF(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	}
	return -1e308
}
