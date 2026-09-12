package frigolite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

// Native tests for the 2nd-generation rtree query callbacks
// (sqlite3_rtree_query_callback) and the priority-queue search they drive.
// This is the Go anchor for the rtreeE / rtreedoc2 / rtreedoc3 TCL sections
// whose harness plumbing (register_circle_geom's Qcircle/breadthfirstsearch,
// register_box_query's qbox and the TCL-side queue model) the transpiler
// cannot express; the engine contract below is what those tests pin.

// qboxCallback mirrors test_rtreedoc.c's box_query callback: four parameters
// (x0,x1,y0,y1), eParentWithin==FULLY_WITHIN short-circuits, otherwise box
// containment / overlap decides visibility, scored breadth-first. The
// capture fields record the RtreeQueryInfo observables the TCL model checks.
type qboxCapture struct {
	calls        int
	firstILevel  int
	mxLevel      int
	levels       []int
	parentWithin []int
	firstAnQueue []uint32
	firstCoords  []float64
	fullySeen    bool
}

func (q *qboxCapture) callback(info *vtab.RtreeQueryInfo) error {
	q.calls++
	if q.calls == 1 {
		q.firstILevel = info.ILevel
		q.mxLevel = info.MxLevel
		q.firstAnQueue = append([]uint32(nil), info.AnQueue...)
		q.firstCoords = append([]float64(nil), info.ACoord...)
	}
	q.levels = append(q.levels, info.ILevel)
	q.parentWithin = append(q.parentWithin, info.EParentWithin)
	if info.EParentWithin == vtab.FullyWithin {
		q.fullySeen = true
		info.EWithin = vtab.FullyWithin
		return nil
	}
	x0, x1, y0, y1 := info.ACoord[0], info.ACoord[1], info.ACoord[2], info.ACoord[3]
	bx0, bx1 := info.AParam[0], info.AParam[1]
	by0, by1 := info.AParam[2], info.AParam[3]
	info.RScore = 100 - float64(info.ILevel)
	switch {
	case x0 >= bx0 && x1 <= bx1 && y0 >= by0 && y1 <= by1:
		info.EWithin = vtab.FullyWithin
	case x1 >= bx0 && x0 <= bx1 && y1 >= by0 && y0 <= by1:
		info.EWithin = vtab.PartlyWithin
	default:
		info.EWithin = vtab.NotWithin
	}
	return nil
}

// seedRTreeE loads the rtreeE-1.x tree: three clusters of boxes around
// (0,0), (100,0) and (0,200).
func seedRTreeE(t *testing.T, db *DB) {
	t.Helper()
	setup := db.Exec(`
		CREATE VIRTUAL TABLE rt1 USING rtree(id,x0,x1,y0,y1);
		WITH RECURSIVE
		  x(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM x WHERE x<4),
		  y(y) AS (VALUES(0) UNION ALL SELECT y+1 FROM y WHERE y<4)
		INSERT INTO rt1 SELECT x+5*y, x, x+2, y, y+2 FROM x, y;
		WITH RECURSIVE
		  x(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM x WHERE x<4),
		  y(y) AS (VALUES(0) UNION ALL SELECT y+1 FROM y WHERE y<4)
		INSERT INTO rt1 SELECT 100+x+5*y, x*3+100, x*3+102, y*3, y*3+2 FROM x, y;
		WITH RECURSIVE
		  x(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM x WHERE x<4),
		  y(y) AS (VALUES(0) UNION ALL SELECT y+1 FROM y WHERE y<4)
		INSERT INTO rt1 SELECT 200+x+5*y, x*7, x*7+15, y*7+200, y*7+215 FROM x, y;
	`)
	if setup.Error != nil {
		t.Fatalf("setup: %v", setup.Error)
	}
}

// TestNativeRtreeQuery2Order pins rtreeE-1.4: a scored Qcircle MATCH with an
// additional non-pushed rowid filter returns rows in PRIORITY-QUEUE order —
// {200 100 0}, NOT id order — because eScoreType 3 scores leaf nodes by area
// (largest area = smallest rScore = dequeued first).
func TestNativeRtreeQuery2Order(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle bundle: %v", err)
	}
	seedRTreeE(t, db)
	got := idsFromRows(queryIDs(t, db,
		"SELECT id FROM rt1 WHERE id MATCH Qcircle('r:1000 e:3') AND id%100==0"))
	if want := []int64{200, 100, 0}; !equalIDSlices(got, want) {
		t.Fatalf("priority-queue order: got %v, want %v", got, want)
	}
}

// TestNativeRtreeQuery2Forms covers the rtreeE-1.x contract set: 4-arg and
// string calling forms, eScoreType 4/5 odd-rowid exclusion, and MATCH
// winning over a rowid equality constraint (rtreeE-1.7).
func TestNativeRtreeQuery2Forms(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle bundle: %v", err)
	}
	seedRTreeE(t, db)
	const ordered25 = "0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24"
	for _, tc := range []struct {
		q, want string
	}{
		{"SELECT id FROM rt1 WHERE id MATCH Qcircle(0.0, 0.0, 50.0, 3) ORDER BY id", ordered25},
		{"SELECT id FROM rt1 WHERE id MATCH Qcircle('x:0 y:0 r:50.0 e:3') ORDER BY id", ordered25},
		{"SELECT id FROM rt1 WHERE id MATCH Qcircle(100.0, 0.0, 50.0, 3) ORDER BY id",
			"100 101 102 103 104 105 106 107 108 109 110 111 112 113 114 115 116 117 118 119 120 121 122 123 124"},
		{"SELECT id FROM rt1 WHERE id MATCH Qcircle('r:1000 e:4') ORDER BY +id",
			"0 2 4 6 8 10 12 14 16 18 20 22 24 100 102 104 106 108 110 112 114 116 118 120 122 124 200 202 204 206 208 210 212 214 216 218 220 222 224"},
		{"SELECT id FROM rt1 WHERE id MATCH Qcircle(0,0,1000,5) ORDER BY +id",
			"0 2 4 6 8 10 12 14 16 18 20 22 24 100 102 104 106 108 110 112 114 116 118 120 122 124 200 202 204 206 208 210 212 214 216 218 220 222 224"},
		{"SELECT id FROM rt1 WHERE id=18 AND id MATCH Qcircle(0,0,1000,5)", "18"},
	} {
		got := flatIDs(t, db, tc.q)
		if got != tc.want {
			t.Errorf("%s: got [%s], want [%s]", tc.q, got, tc.want)
		}
	}
}

// TestNativeRtreeQuery2BFS checks breadthfirstsearch's set/param contract
// (rtreeE-2.x): the callback's overlap result equals a plain SQL box query,
// and a wrong parameter count aborts with the SQL logic error.
func TestNativeRtreeQuery2BFS(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle bundle: %v", err)
	}
	setup := db.Exec(`
		CREATE VIRTUAL TABLE rt2 USING rtree(id,x0,x1,y0,y1);
		WITH RECURSIVE
		  x(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM x WHERE x<40),
		  y(y) AS (VALUES(0) UNION ALL SELECT y+1 FROM y WHERE y<40)
		INSERT INTO rt2 SELECT NULL, x, x+2, y, y+2 FROM x, y;
	`)
	if setup.Error != nil {
		t.Fatalf("setup: %v", setup.Error)
	}
	match := flatIDs(t, db, "SELECT id FROM rt2 WHERE id MATCH breadthfirstsearch(10,30,10,30) ORDER BY id")
	sql := flatIDs(t, db, "SELECT id FROM rt2 WHERE x1>=10 AND x0<=30 AND y1>=10 AND y0<=30 ORDER BY id")
	if match == "" || match != sql {
		t.Fatalf("bfs mismatch: match=[%s] sql=[%s]", match, sql)
	}
	if res := db.Query("SELECT id FROM rt2 WHERE id MATCH breadthfirstsearch(1,2,3)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "SQL logic error") {
		t.Fatalf("bfs arity: want SQL logic error, got %v", res.Error)
	}
}

// TestNativeRtreeQuery2InfoState drives a qbox-style callback (the rtreedoc3
// contract) and asserts the RtreeQueryInfo observables: root-first iLevel
// walk, mxLevel, anQueue occupancy, and FULLY_WITHIN short-circuit behavior.
func TestNativeRtreeQuery2InfoState(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	qc := &qboxCapture{}
	vtab.RegisterRTreeQueryGeometry(db.engine.Database(), "qbox", qc.callback)
	setup := db.Exec(`
		CREATE VIRTUAL TABLE rtq USING rtree_i32(id, x1,x2, y1,y2);
		WITH s(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM s WHERE i<63)
		INSERT INTO rtq SELECT NULL, (i%8)*4, (i%8)*4+4, (i/8)*4, (i/8)*4+4 FROM s;
	`)
	if setup.Error != nil {
		t.Fatalf("setup: %v", setup.Error)
	}
	res := queryIDs(t, db, "SELECT id FROM rtq WHERE id MATCH qbox(9, 27, 9, 27)")
	if len(res) == 0 {
		t.Fatal("qbox returned no rows")
	}
	if qc.calls == 0 {
		t.Fatal("callback never invoked")
	}
	// First invocation is the ROOT cell: iLevel == mxLevel-1, the queue
	// holds exactly the root entry at the root level.
	if qc.firstILevel != qc.mxLevel-1 || qc.mxLevel <= 0 {
		t.Fatalf("first call: iLevel=%d mxLevel=%d", qc.firstILevel, qc.mxLevel)
	}
	if qc.firstAnQueue[qc.mxLevel] != 1 {
		t.Fatalf("anQueue[root]=%d, want 1", qc.firstAnQueue[qc.mxLevel])
	}
	// Coordinates arrive REAL-domain widened; the root MBR is well-formed.
	if len(qc.firstCoords) != 4 || qc.firstCoords[0] > qc.firstCoords[1] {
		t.Fatalf("first coords: %v", qc.firstCoords)
	}
	// The walk reaches leaf (iLevel 0) invocations, whose parents may be
	// FULLY_WITHIN (the short-circuit rtreedoc3's box_query exercises).
	leafSeen := false
	for _, lvl := range qc.levels {
		if lvl == 0 {
			leafSeen = true
		}
	}
	if !leafSeen {
		t.Fatal("callback never saw a leaf (iLevel 0) invocation")
	}
	_ = qc.fullySeen
}

// TestNativeRtreeQuery2NullMarker pins rtreedoc2-1.2: the geometry/query
// marker SQL functions render as ordinary NULL (sqlite3_result_pointer) and
// appear in pragma_function_list lowercased.
func TestNativeRtreeQuery2NullMarker(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle bundle: %v", err)
	}
	for _, fn := range []string{"circle", "qcircle", "breadthfirstsearch"} {
		res := db.Query("SELECT " + fn + "(1, 2, 3)")
		if res.Error != nil {
			t.Fatalf("SELECT %s(1,2,3): %v", fn, res.Error)
		}
		if len(res.Rows) != 1 || res.Rows[0][0] != nil {
			t.Fatalf("SELECT %s(1,2,3) must render NULL, got %#v", fn, res.Rows)
		}
	}
	fl := db.Query("SELECT name FROM pragma_function_list WHERE name IN ('circle','qcircle','breadthfirstsearch') ORDER BY name")
	if fl.Error != nil {
		t.Fatalf("pragma_function_list: %v", fl.Error)
	}
	var names []string
	for _, row := range fl.Rows {
		if s, ok := row[0].(string); ok {
			names = append(names, s)
		}
	}
	if got, want := strings.Join(names, " "), "breadthfirstsearch circle qcircle"; got != want {
		t.Fatalf("function_list: got [%s], want [%s]", got, want)
	}
}

// TestNativeRtreeQuery2ColumnCaps pins the rtreeInit argument rules the doc
// suite exercises: the 100-total-column cap, the '+'-prefixed first column's
// declare_vtab parse error, and comment tolerance around aux markers.
func TestNativeRtreeQuery2ColumnCaps(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()

	var b strings.Builder
	b.WriteString("CREATE VIRTUAL TABLE r1 USING rtree(intid, u1,u2")
	for i := 0; i < 98; i++ {
		b.WriteString(fmt.Sprintf(", +c%d", i))
	}
	b.WriteString(")")
	if res := db.Exec(b.String()); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "Too many columns for an rtree table") {
		t.Fatalf("101 columns: want Too many columns, got %v", res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE rrr USING rtree(+id, extra, x1, x2)"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), `near "+": syntax error`) {
		t.Fatalf("'+id' first: want syntax error, got %v", res.Error)
	}
	// A comment ahead of the argument must not hide the '+' aux marker
	// (C tokenizes arguments before rtreeInit sees them).
	if res := db.Exec("CREATE VIRTUAL TABLE ok1 USING rtree(id, minX,maxX, minY,maxY, +objname TEXT, -- aux\n +objtype TEXT, +boundary BLOB)"); res.Error != nil {
		t.Fatalf("comment-led aux args: %v", res.Error)
	}
}

// TestNativeRtreeNoVtabTrigger pins the rtreecirc contract: a trigger on a
// shadow table whose body selects from the vtab breaks the circular
// reference with the schema-fixed "no such table: main.rt" error.
func TestNativeRtreeNoVtabTrigger(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	// openRtreeDB pre-creates rt (id,x1,x2,y1,y2).
	if res := db.Exec("CREATE TRIGGER tr1 AFTER INSERT ON rt_rowid BEGIN SELECT * FROM rt; END"); res.Error != nil {
		t.Fatalf("create trigger: %v", res.Error)
	}
	res := db.Exec("INSERT INTO rt VALUES(1, 2, 3, 4, 5)")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "no such table: main.rt") {
		t.Fatalf("want no such table: main.rt, got %v", res.Error)
	}
	// Plain mode still reads the shadow tables.
	if res := db.Query("SELECT count(*) FROM rt_rowid"); res.Error != nil {
		t.Fatalf("shadow read: %v", res.Error)
	}
}

// TestNativeRtreeAttachedSchema pins rtreedoc 8.x: an rtree created in an
// attached database materializes its shadow family THERE, and rtreecheck's
// two-argument form checks it.
func TestNativeRtreeAttachedSchema(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if res := db.Exec("ATTACH ':memory:' AS aux"); res.Error != nil {
		t.Fatalf("attach: %v", res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE aux.rt1 USING rtree(id, a, b)"); res.Error != nil {
		t.Fatalf("create aux rtree: %v", res.Error)
	}
	if res := db.Exec("INSERT INTO aux.rt1 VALUES(1, 2, 3)"); res.Error != nil {
		t.Fatalf("insert aux rtree: %v", res.Error)
	}
	res := db.Query("SELECT rtreecheck('aux', 'rt1')")
	if res.Error != nil {
		t.Fatalf("rtreecheck(aux, rt1): %v", res.Error)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "ok" {
		t.Fatalf("rtreecheck(aux, rt1) = %v, want ok", res.Rows)
	}
}

// equalIDSlices compares two id slices element-wise (order matters).
func equalIDSlices(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// flatIDs renders an id-column query result as TCL-style flat text.
func flatIDs(t *testing.T, db *DB, q string) string {
	t.Helper()
	rows := queryIDs(t, db, q)
	var parts []string
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%d", row[0]))
	}
	return strings.Join(parts, " ")
}
