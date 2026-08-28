package frigolite

import (
	"fmt"
	"testing"
)

// TestRtreeSmoke exercises the rtree virtual table module end-to-end: CREATE,
// INSERT, SELECT (full scan + result set), UPDATE and DELETE.
func TestRtreeSmoke(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ddl := `CREATE VIRTUAL TABLE demo USING rtree(id, minX, maxX, minY, maxY)`
	if res := db.Exec(ddl); res.Error != nil {
		t.Fatalf("create rtree: %v", res.Error)
	}

	// Verify the three shadow tables were created.
	for _, sh := range []string{"demo_node", "demo_rowid", "demo_parent"} {
		res := db.Query("SELECT name FROM sqlite_master WHERE name = '" + sh + "'")
		if res.Error != nil {
			t.Fatalf("query sqlite_master for %s: %v", sh, res.Error)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("shadow table %s not created (got %d rows)", sh, len(res.Rows))
		}
	}

	inserts := [][5]float64{
		{1, 0, 1, 0, 1},
		{2, 2, 3, 2, 3},
		{3, 4, 5, 4, 5},
		{4, -1, 0, -1, 0},
	}
	for _, r := range inserts {
		sql := fmt.Sprintf(`INSERT INTO demo VALUES(%d, %v, %v, %v, %v)`,
			int64(r[0]), r[1], r[2], r[3], r[4])
		res := db.Exec(sql)
		if res.Error != nil {
			t.Fatalf("insert id=%v: %v", r[0], res.Error)
		}
	}

	// Full scan returns all four rows.
	res := db.Query("SELECT id, minX, maxX, minY, maxY FROM demo ORDER BY id")
	if res.Error != nil {
		t.Fatalf("select: %v", res.Error)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("expected 4 rows, got %d: %v", len(res.Rows), res.Rows)
	}
	// Check first row values (float32 stored, surfaced as float64; the id
	// column is the INTEGER PRIMARY KEY and comes back as int64).
	want := inserts[0]
	got := res.Rows[0]
	if gid, ok := got[0].(int64); !ok || gid != int64(want[0]) {
		t.Fatalf("row0 id = %T %v want int64 %v", got[0], got[0], int64(want[0]))
	}
	for i := 1; i < 5; i++ {
		gf, ok := got[i].(float64)
		if !ok {
			t.Fatalf("row0 col%d not float64: %T %v", i, got[i], got[i])
		}
		if gf != want[i] {
			t.Errorf("row0 col%d = %v want %v", i, gf, want[i])
		}
	}

	// UPDATE id=2 to a new box.
	if res := db.Exec(`UPDATE demo SET minX=-2, maxX=-1, minY=-2, maxY=-1 WHERE id=2`); res.Error != nil {
		t.Fatalf("update: %v", res.Error)
	}
	res = db.Query("SELECT minX, maxX, minY, maxY FROM demo WHERE id=2")
	if res.Error != nil {
		t.Fatalf("select updated: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row after update, got %v", res.Rows)
	}
	if r := res.Rows[0]; r[0] != -2.0 || r[3] != -1.0 {
		t.Fatalf("update did not apply: %v", res.Rows)
	}

	// DELETE id=3.
	if res := db.Exec(`DELETE FROM demo WHERE id=3`); res.Error != nil {
		t.Fatalf("delete: %v", res.Error)
	}
	res = db.Query("SELECT id FROM demo ORDER BY id")
	if res.Error != nil {
		t.Fatalf("select after delete: %v", res.Error)
	}
	ids := []int64{}
	for _, row := range res.Rows {
		ids = append(ids, rtreeTestAsInt(row[0]))
	}
	wantIDs := []int64{1, 2, 4}
	if len(ids) != len(wantIDs) {
		t.Fatalf("expected ids %v after delete, got %v", wantIDs, ids)
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] {
			t.Fatalf("expected ids %v after delete, got %v", wantIDs, ids)
		}
	}
}

// TestRtreeI32 exercises the rtree_i32 variant: integer coordinates.
func TestRtreeI32Smoke(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if res := db.Exec(`CREATE VIRTUAL TABLE r USING rtree_i32(id, minX, maxX, minY, maxY)`); res.Error != nil {
		t.Fatalf("create rtree_i32: %v", res.Error)
	}
	if res := db.Exec(`INSERT INTO r VALUES(1, 0, 10, 0, 10)`); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	res := db.Query("SELECT id, minX, maxX, minY, maxY FROM r")
	if res.Error != nil {
		t.Fatalf("select: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][1].(int64) != 0 || res.Rows[0][2].(int64) != 10 {
		t.Fatalf("int32 coords not surfaced as INTEGER: %v", res.Rows[0])
	}
}

// rtreeTestAsInt coerces a query cell (int64 or float64) to int64.
func rtreeTestAsInt(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return -1
}
