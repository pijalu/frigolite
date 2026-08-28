package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// Native unit tests for the rtree module core, kept separate from the
// TCL-transpiled suites under testgen/. These exercise the engine directly via
// Open/Exec/Query and mirror canonical sqlite usage patterns from
// ext/rtree/*.test. Oracle: /usr/bin/sqlite3 (see .agents/rtree_oracle/).

// openRtreeDB opens an in-memory database and creates a 2-D rtree table `rt`
// with the standard (id,x1,x2,y1,y2) layout.
func openRtreeDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE rt USING rtree(id,x1,x2,y1,y2)"); res.Error != nil {
		t.Fatalf("create rtree: %v", res.Error)
	}
	return db
}

// TestNativeRtreeCreate verifies the framework wiring for a plain rtree:
// shadow tables exist, declared columns resolve, empty scan returns nothing.
func TestNativeRtreeCreate(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	rows := db.Query("SELECT name FROM sqlite_schema WHERE name IN ('rt_node','rt_rowid','rt_parent') ORDER BY name")
	if rows.Error != nil {
		t.Fatalf("shadow table query: %v", rows.Error)
	}
	if len(rows.Rows) != 3 {
		t.Fatalf("shadow tables: want 3, got %d (%v)", len(rows.Rows), rows.Rows)
	}

	scan := db.Query("SELECT * FROM rt")
	if scan.Error != nil {
		t.Fatalf("scan rt: %v", scan.Error)
	}
	wantCols := []string{"id", "x1", "x2", "y1", "y2"}
	if len(scan.Columns) != len(wantCols) {
		t.Fatalf("columns: got %v", scan.Columns)
	}
	for i, want := range wantCols {
		if scan.Columns[i] != want {
			t.Fatalf("column %d: want %s got %s", i, want, scan.Columns[i])
		}
	}
	if len(scan.Rows) != 0 {
		t.Fatalf("empty tree: want 0 rows, got %v", scan.Rows)
	}
}

// TestNativeRtreeInsertScanRoundtrip inserts a deterministic rectangle set and
// verifies SELECT * reproduces it exactly.
func TestNativeRtreeInsertScanRoundtrip(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	rects := seedRects(50)
	insertRects(t, db, rects)

	got := queryIDs(t, db, "SELECT id,x1,x2,y1,y2 FROM rt ORDER BY id")
	if len(got) != len(rects) {
		t.Fatalf("row count: want %d got %d", len(rects), len(got))
	}
	for i, r := range rects {
		row := got[i]
		want := []interface{}{r.id, r.x1, r.x2, r.y1, r.y2}
		if !equalRow(row, want) {
			t.Fatalf("row %d: want %v got %v", i, want, row)
		}
	}
}

// TestNativeRtreeShadowAccounting checks the backing tables track the tree:
// one %_rowid entry per entry and node blobs reachable from the root.
func TestNativeRtreeShadowAccounting(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	// Enough entries to force at least one split (fan-out > default capacity).
	rects := seedRects(120)
	insertRects(t, db, rects)

	if n := scalarInt(t, db, "SELECT count(*) FROM rt_rowid"); n != int64(len(rects)) {
		t.Fatalf("rt_rowid count: want %d got %d", len(rects), n)
	}
	nNodes := scalarInt(t, db, "SELECT count(*) FROM rt_node")
	if nNodes < 2 {
		t.Fatalf("node count after 120 inserts: want >=2 got %d", nNodes)
	}
	// Root must be an interior page: stored depth >= 1 proves multi-level tree.
	depth := scalarInt(t, db, "SELECT rtreedepth((SELECT data FROM rt_node WHERE nodeno=1))")
	if depth < 1 {
		t.Fatalf("root depth: want >=1 got %d", depth)
	}
}

// TestNativeRtreeDeleteAndUpdate exercises the xUpdate delete path and bound
// updates, comparing against a Go-side model of the same data.
func TestNativeRtreeDeleteAndUpdate(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	rects := seedRects(80)
	insertRects(t, db, rects)

	// Delete every odd rowid through SQL.
	res := db.Exec("DELETE FROM rt WHERE id%2=1")
	if res.Error != nil {
		t.Fatalf("delete odds: %v", res.Error)
	}

	model := map[int64]rect{}
	for _, r := range rects {
		if r.id%2 == 0 {
			model[r.id] = r
		}
	}
	assertMatchesModel(t, db, model)

	// Re-insert previously deleted ids then update one box's extents.
	insertRects(t, db, seedRects(80))
	model = map[int64]rect{}
	for _, r := range seedRects(80) {
		model[r.id] = r
	}
	if res := db.Exec("UPDATE rt SET x1=1000, x2=1005 WHERE id=4"); res.Error != nil {
		t.Fatalf("update bounds: %v", res.Error)
	}
	model[4] = rect{id: 4, x1: 1000, x2: 1005, y1: seedRects(80)[3].y1, y2: seedRects(80)[3].y2}
	assertMatchesModel(t, db, model)

	// INSERT OR REPLACE on an existing rowid rewrites the box (rtree-12 flow).
	// Replace swaps the WHOLE row (rtree1-12): the untouched-in-spirit y pair
	// becomes the literal new values too.
	if res := db.Exec("INSERT OR REPLACE INTO rt VALUES(7, -5, -1, -5, -1)"); res.Error != nil {
		t.Fatalf("replace insert: %v", res.Error)
	}
	if n := scalarInt(t, db, "SELECT count(*) FROM rt WHERE id=7"); n != 1 {
		t.Fatalf("duplicate id=7 rows: %d", n)
	}
	row := db.Query("SELECT x1 FROM rt WHERE id=7")
	if row.Error != nil || len(row.Rows) != 1 || !floatEq(row.Rows[0][0], -5) {
		t.Fatalf("replaced box x1: err=%v rows=%v", row.Error, row.Rows)
	}
	model[7] = rect{id: 7, x1: -5, x2: -1, y1: -5, y2: -1}
	assertMatchesModel(t, db, model)
}

// TestNativeRtreeDropCascades drops the virtual table and asserts all three
// shadow tables vanish from sqlite_schema.
func TestNativeRtreeDropCascades(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	insertRects(t, db, seedRects(10))

	if res := db.Exec("DROP TABLE rt"); res.Error != nil {
		t.Fatalf("drop rt: %v", res.Error)
	}
	rows := db.Query("SELECT name FROM sqlite_schema WHERE name LIKE 'rt\\_%' ESCAPE '\\' ORDER BY name")
	if rows.Error != nil {
		t.Fatalf("residue query: %v", rows.Error)
	}
	if n := len(rows.Rows); n != 0 {
		var names []string
		for _, r := range rows.Rows {
			names = append(names, r[0].(string))
		}
		t.Fatalf("shadow residue after DROP: %v", names)
	}
}

// TestNativeRtreeI32 stores and reads back int coordinates through the i32
// codec: values must round-trip exactly and come back as integers.
func TestNativeRtreeI32CreateAndRoundtrip(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE VIRTUAL TABLE ri USING rtree_i32(id,x1,x2,y1,y2)"); res.Error != nil {
		t.Fatalf("create rtree_i32: %v", res.Error)
	}
	rows := db.Query("SELECT name FROM sqlite_schema WHERE name IN ('ri_node','ri_rowid','ri_parent') ORDER BY name")
	if rows.Error != nil || len(rows.Rows) != 3 {
		t.Fatalf("i32 shadow tables: err=%v rows=%d", rows.Error, len(rows.Rows))
	}

	rects := seedI32Rects(64)
	for _, r := range rects {
		sql := sprintfInsert("ri", r.id, float64(r.x1), float64(r.x2), float64(r.y1), float64(r.y2))
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("insert %d: %v", r.id, res.Error)
		}
	}
	got := queryIDs(t, db, "SELECT id,x1,x2,y1,y2 FROM ri ORDER BY id")
	if len(got) != len(rects) {
		t.Fatalf("i32 row count: want %d got %d", len(rects), len(got))
	}
	for i, r := range rects {
		want := []interface{}{r.id, int64(r.x1), int64(r.x2), int64(r.y1), int64(r.y2)}
		if !equalRow(got[i], want) {
			t.Fatalf("i32 row %d: want %v got %v", i, want, got[i])
		}
	}
}

// TestNativeRtreeAuxColumns declares trailing auxiliary columns and verifies
// they store and return their payload untouched.
func TestNativeRtreeAuxColumns(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if res := db.Exec("CREATE VIRTUAL TABLE ax USING rtree(id,x1,x2,+note)"); res.Error != nil {
		t.Fatalf("create aux rtree: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO ax VALUES(1,0,10,'first')"); res.Error != nil {
		t.Fatalf("aux insert: %v", res.Error)
	}
	rows := db.Query("SELECT note FROM ax WHERE id=1")
	if rows.Error != nil || len(rows.Rows) != 1 {
		t.Fatalf("aux select: err=%v rows=%v", rows.Error, rows.Rows)
	}
	if s, ok := rows.Rows[0][0].(string); !ok || s != "first" {
		t.Fatalf("aux payload: want 'first' got %#v", rows.Rows[0][0])
	}
	// Aux columns participate in UPDATE like ordinary columns.
	if res := db.Exec("UPDATE ax SET note='updated' WHERE id=1"); res.Error != nil {
		t.Fatalf("aux update: %v", res.Error)
	}
	if got := scalarText(t, db, "SELECT note FROM ax WHERE id=1"); got != "updated" {
		t.Fatalf("aux updated value: %q", got)
	}
}

// ---- shared helpers (used by the focused rtree *_test.go files too) ----

type rect struct {
	id              int64
	x1, x2, y1, y2  float64
}

func floatEq(a interface{}, b float64) bool {
	f, ok := a.(float64)
	return ok && f == b
}

func equalRow(got []interface{}, want []interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		switch w := want[i].(type) {
		case int64:
			g, ok := got[i].(int64)
			if !ok || g != w {
				return false
			}
		default:
			g, ok := got[i].(float64)
			wf, _ := want[i].(float64)
			if !ok || g != wf {
				return false
			}
		}
	}
	return true
}

// seedRects builds n deterministic 2-D boxes with varied extents so splits,
// overlap handling and multi-dim pruning are all exercised.
func seedRects(n int) []rect {
	out := make([]rect, 0, n)
	for i := 1; i <= n; i++ {
		x1 := float64(i * 10)
		y1 := float64((i * 37) % 400)
		out = append(out, rect{
			id: int64(i),
			x1: x1, x2: x1 + float64(5+i%4),
			y1: y1, y2: y1 + float64(10+(i*13)%30),
		})
	}
	return out
}

type irect struct {
	id             int64
	x1, x2, y1, y2 int
}

func seedI32Rects(n int) []irect {
	out := make([]irect, 0, n)
	for i := 1; i <= n; i++ {
		x1 := i * 7
		y1 := (i * 41) % 500
		out = append(out, irect{id: int64(i),
			x1: x1, x2: x1 + 3 + i%5,
			y1: y1, y2: y1 + 8 + i%12})
	}
	return out
}

func insertRects(t *testing.T, db *DB, rs []rect) {
	t.Helper()
	for _, r := range rs {
		sql := sprintfInsert("rt", r.id, r.x1, r.x2, r.y1, r.y2)
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("insert %d: %v", r.id, res.Error)
		}
	}
}

func sprintfInsert(table string, id int64, xs ...float64) string {
	var b strings.Builder
	b.WriteString("INSERT OR IGNORE INTO ")
	b.WriteString(table)
	b.WriteString(" VALUES(")
	b.WriteString(itoa64(id))
	for _, x := range xs {
		b.WriteByte(',')
		b.WriteString(ftoa(x))
	}
	b.WriteString(")")
	return b.String()
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// queryIDs runs q and returns its rows; fatal on error.
func queryIDs(t *testing.T, db *DB, q string) [][]interface{} {
	t.Helper()
	res := db.Query(q)
	if res.Error != nil {
		t.Fatalf("query %q: %v", q, res.Error)
	}
	return res.Rows
}

// scalarInt returns the first column of the first row as int64.
func scalarInt(t *testing.T, db *DB, q string) int64 {
	t.Helper()
	rows := queryIDs(t, db, q)
	if len(rows) == 0 {
		t.Fatalf("scalar %q: no rows", q)
	}
	switch v := rows[0][0].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		t.Fatalf("scalar %q: non-numeric %#v", q, rows[0][0])
		return 0
	}
}

// scalarText returns the first column of the first row as text.
func scalarText(t *testing.T, db *DB, q string) string {
	t.Helper()
	rows := queryIDs(t, db, q)
	if len(rows) == 0 {
		t.Fatalf("scalar %q: no rows", q)
	}
	s, _ := rows[0][0].(string)
	return s
}

func assertMatchesModel(t *testing.T, db *DB, model map[int64]rect) {
	t.Helper()
	got := queryIDs(t, db, "SELECT id,x1,x2,y1,y2 FROM rt ORDER BY id")
	if len(got) != len(model) {
		t.Fatalf("model size %d but engine returned %d rows", len(model), len(got))
	}
	i := 0
	for _, id := range sortedKeys(model) {
		r := model[id]
		want := []interface{}{r.id, r.x1, r.x2, r.y1, r.y2}
		if !equalRow(got[i], want) {
			t.Fatalf("rank %d: want %v got %v", i, want, got[i])
		}
		i++
	}
}

func sortedKeys(m map[int64]rect) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
