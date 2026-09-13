package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Native unit tests for the geopoly module (ext/rtree/geopoly.c port), kept
// separate from the TCL-transpiled suites under testgen/. Every expected
// output was captured from a geopoly-enabled SQLite oracle (python3's
// sqlite3, 3.53.x) and is reproduced byte-for-byte where deterministic.

const geoTestPoly = `[[0,0],[1,1],[1,0],[0,0]]`

// openGeopolyDB opens an in-memory database with a geopoly table `g` seeded
// with two polygons.
func openGeopolyDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE g USING geopoly(clr)"); res.Error != nil {
		t.Fatalf("create geopoly: %v", res.Error)
	}
	for _, tc := range [][2]string{
		{geoTestPoly, "red"},
		{"[[5,5],[6,6],[6,5],[5,5]]", "green"},
	} {
		res := db.Exec(fmt.Sprintf("INSERT INTO g(_shape,clr) VALUES('%s',%q)", tc[0], tc[1]))
		if res.Error != nil {
			t.Fatalf("insert %s: %v", tc[0], res.Error)
		}
	}
	return db
}

// geoScalar runs a single-value query and fails on error/mismatch.
func geoScalar(t *testing.T, db *DB, sql, want string) {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 {
		t.Fatalf("%s: want 1x1 result, got %v", sql, r.Rows)
	}
	var got string
	switch v := r.Rows[0][0].(type) {
	case nil:
		got = "<NULL>"
	case string:
		got = v
	case []byte:
		got = "x'" + fmt.Sprintf("%x", v) + "'"
	default:
		got = fmt.Sprintf("%v", v)
	}
	if got != want {
		t.Fatalf("%s:\n  got:  %s\n  want: %s", sql, got, want)
	}
}

// TestNativeGeopolyFunctions pins the deterministic scalar-function outputs
// against the geopoly-enabled SQLite oracle.
func TestNativeGeopolyFunctions(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	P := "'" + geoTestPoly + "'"
	geoScalar(t, db, "SELECT geopoly_json("+P+")",
		"[[0.0,0.0],[1.0,1.0],[1.0,0.0],[0.0,0.0]]")
	geoScalar(t, db, "SELECT geopoly_area("+P+")", "-0.5")
	geoScalar(t, db, "SELECT hex(geopoly_blob("+P+"))",
		"0100000300000000000000000000803F0000803F0000803F00000000")
	geoScalar(t, db, "SELECT geopoly_svg("+P+")",
		"<polyline points='0,0 1,1 1,0 0,0'></polyline>")
	geoScalar(t, db, `SELECT geopoly_svg(`+P+`, 'fill="red"', '', 'stroke=blue')`,
		`<polyline points='0,0 1,1 1,0 0,0' fill="red" stroke=blue></polyline>`)
	geoScalar(t, db, "SELECT hex(geopoly_bbox("+P+"))",
		"0100000400000000000000000000803F000000000000803F0000803F000000000000803F")
	geoScalar(t, db, "SELECT geopoly_json(geopoly_bbox("+P+"))",
		"[[0.0,0.0],[1.0,0.0],[1.0,1.0],[0.0,1.0],[0.0,0.0]]")
	geoScalar(t, db, "SELECT geopoly_json(geopoly_ccw('[[0,0],[1,0],[1,1],[0,0]]'))",
		"[[0.0,0.0],[1.0,0.0],[1.0,1.0],[0.0,0.0]]")
	geoScalar(t, db, "SELECT geopoly_area(geopoly_ccw('[[0,0],[1,0],[1,1],[0,0]]'))", "0.5")
	geoScalar(t, db, "SELECT geopoly_json(geopoly_regular(10,20,5,4))",
		"[[15.0003,20.0],[10.0,25.0003],[4.99966,20.0],[10.0,14.9997],[15.0003,20.0]]")
	geoScalar(t, db, "SELECT geopoly_json(geopoly_regular(0,0,1,3))",
		"[[1.00007,0.0],[-0.499953,0.866088],[-0.499953,-0.866088],[1.00007,0.0]]")
	geoScalar(t, db, "SELECT geopoly_regular(0,0,1,2)", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_regular(0,0,0,4)", "<NULL>")
	geoScalar(t, db, "SELECT hex(geopoly_xform("+P+", 1,0,0,1,10,20))",
		"01000003000020410000A041000030410000A841000030410000A041")
	geoScalar(t, db, "SELECT geopoly_json('[[1e20,0],[2,2],[2,0],[1e20,0]]')",
		"[[1.0e+20,0.0],[2.0,2.0],[2.0,0.0],[1.0e+20,0.0]]")
	geoScalar(t, db, "SELECT geopoly_svg('[[1e20,0],[2,2],[2,0],[1e20,0]]')",
		"<polyline points='1e+20,0 2,2 2,0 1e+20,0'></polyline>")
}

// TestNativeGeopolyPredicates pins contains_point / within / overlap.
func TestNativeGeopolyPredicates(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	P := "'" + geoTestPoly + "'"
	geoScalar(t, db, "SELECT geopoly_contains_point("+P+", 0.5, 0.5)", "1") // inside
	geoScalar(t, db, "SELECT geopoly_contains_point("+P+", 2, 2)", "0")     // outside
	geoScalar(t, db, "SELECT geopoly_contains_point("+P+", 0, 0)", "1")     // on vertex
	geoScalar(t, db, "SELECT geopoly_contains_point("+P+", 0.5, 0)", "1")   // on edge
	SMALL := "'[[0.5,0.5],[1.5,0.5],[1.5,1.5],[0.5,1.5],[0.5,0.5]]'"
	BIG := "'[[0,0],[2,0],[2,2],[0,2],[0,0]]'"
	geoScalar(t, db, "SELECT geopoly_within("+SMALL+","+BIG+")", "1")
	geoScalar(t, db, "SELECT geopoly_within("+BIG+","+SMALL+")", "0")
	geoScalar(t, db, "SELECT geopoly_within("+SMALL+","+SMALL+")", "2")
	geoScalar(t, db, "SELECT geopoly_overlap("+SMALL+","+BIG+")", "2")
	geoScalar(t, db, "SELECT geopoly_overlap("+BIG+","+SMALL+")", "3")
	geoScalar(t, db, "SELECT geopoly_overlap("+BIG+","+BIG+")", "4")
	geoScalar(t, db, "SELECT geopoly_overlap("+BIG+", '[[5,5],[6,6],[5,6],[5,5]]')", "0")
	geoScalar(t, db, "SELECT geopoly_overlap("+BIG+", '[[1.5,1.5],[3,3],[3,1.5],[1.5,1.5]]')", "1")
	// unusable inputs render NULL (never an error)
	geoScalar(t, db, "SELECT geopoly_area('junk')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area(NULL)", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area(123)", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area('[[0,0],[1,1],[0,0]]')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area('[[0,0],[1,1],[1,0],[0,1]]')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area(x'000000')", "<NULL>")
}

// TestNativeGeopolyJSONParser covers geopolyParseJson acceptance/rejection
// edges: whitespace tolerance, exponents, extra coordinate values, leading
// zeros, signs and trailing junk (oracle-verified).
func TestNativeGeopolyJSONParser(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	geoScalar(t, db, "SELECT geopoly_area(' [ [ 0 , 0 ] , [ 1 , 1 ] , [ 1 , 0 ] , [ 0 , 0 ] ] ')", "-0.5")
	geoScalar(t, db, "SELECT geopoly_json('[[0e1,0],[1e0,1],[1,0],[0,0]]')",
		"[[0.0,0.0],[1.0,1.0],[1.0,0.0],[0.0,0.0]]")
	// a third number inside a vertex pair is validated but discarded
	geoScalar(t, db, "SELECT geopoly_area('[[0,0,1],[1,1],[1,0],[0,0]]')", "-0.5")
	geoScalar(t, db, "SELECT geopoly_area('[[00,0],[1,1],[1,0],[0,0]]')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area('[[+1,0],[1,1],[1,0],[0,0]]')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area('[[0],[1],[1],[0],[0]]')", "<NULL>")
	geoScalar(t, db, "SELECT geopoly_area('[[0,0],[1,1],[1,0],[0,0]] x')", "<NULL>")
}

// TestNativeGeopolyBlobRoundTrip checks the on-disk codec: JSON to blob to
// JSON reproduces the polygon; big-endian-flagged blobs decode via swab.
func TestNativeGeopolyBlobRoundTrip(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	P := "'" + geoTestPoly + "'"
	geoScalar(t, db, "SELECT hex(geopoly_blob(geopoly_blob("+P+")))",
		"0100000300000000000000000000803F0000803F0000803F00000000")
	geoScalar(t, db, "SELECT geopoly_json(geopoly_blob("+P+"))",
		"[[0.0,0.0],[1.0,1.0],[1.0,0.0],[0.0,0.0]]")
	// encoding flag 0 (big-endian coordinates): 1.0 = 0x3F800000 stored BE.
	// The blob holds vertexes (1,0),(1,1),(1,1); geopoly_json repeats the
	// first vertex at the end.
	geoScalar(t, db,
		"SELECT geopoly_json(x'000000033F800000000000003F8000003F8000003F8000003F800000')",
		"[[1.0,0.0],[1.0,1.0],[1.0,1.0],[1.0,0.0]]")
	// a 3-vertex blob below the 28-byte minimum is an unusable type
	geoScalar(t, db, "SELECT geopoly_area(x'0100000000000000000000000000000000000000')", "<NULL>")
}

// TestNativeGeopolyGroupBBox pins the aggregate (including the C quirk that
// unparseable TEXT initializes a zero box while NULL/typed rows are skipped).
func TestNativeGeopolyGroupBBox(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	geoScalar(t, db, "WITH t(p) AS (VALUES ('[[0,0],[1,1],[1,0],[0,0]]'), ('[[10,10],[11,11],[11,10],[10,10]]'))"+
		" SELECT geopoly_json(geopoly_group_bbox(p)) FROM t",
		"[[0.0,0.0],[11.0,0.0],[11.0,11.0],[0.0,11.0],[0.0,0.0]]")
	geoScalar(t, db, "SELECT geopoly_group_bbox(NULL) FROM (SELECT 1) WHERE 0", "<NULL>")
	geoScalar(t, db, "WITH t(p) AS (VALUES ('junk'), (NULL))"+
		" SELECT hex(geopoly_group_bbox(p)) FROM t",
		"010000040000000000000000000000000000000000000000000000000000000000000000")
}

// TestNativeGeopolyVtab covers CREATE/INSERT/SELECT/UPDATE/DELETE through the
// virtual table, aux-column storage, and the error texts.
func TestNativeGeopolyVtab(t *testing.T) {
	db := openGeopolyDB(t)
	defer db.Close()

	// _shape is stored normalized to its blob; clr is an auxiliary column.
	rows := queryIDs(t, db, "SELECT rowid, typeof(_shape), clr FROM g ORDER BY rowid")
	if len(rows) != 2 {
		t.Fatalf("row count: want 2, got %d", len(rows))
	}
	if rows[0][1] != "blob" || rows[0][2] != "red" {
		t.Fatalf("row 0: %v", rows[0])
	}
	// INSERT with unusable _shape type.
	if res := db.Exec("INSERT INTO g(_shape,clr) VALUES(NULL,'x')"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "_shape does not contain a valid polygon") {
		t.Fatalf("NULL insert: %v", res.Error)
	}
	// INSERT with unparseable TEXT succeeds with a zero cell (C rc==OK path).
	if res := db.Exec("INSERT INTO g(_shape,clr) VALUES('junk','blue')"); res.Error != nil {
		t.Fatalf("junk insert: %v", res.Error)
	}
	rows = queryIDs(t, db, "SELECT rowid FROM g WHERE clr='blue'")
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Fatalf("junk row: %v", rows)
	}
	// UPDATE of an aux column and of _shape.
	if res := db.Exec("UPDATE g SET clr='pink' WHERE rowid=1"); res.Error != nil {
		t.Fatalf("update clr: %v", res.Error)
	}
	rows = queryIDs(t, db, "SELECT clr FROM g WHERE rowid=1")
	if rows[0][0] != "pink" {
		t.Fatalf("clr after update: %v", rows)
	}
	if res := db.Exec("UPDATE g SET _shape='[[3,3],[4,4],[4,3],[3,3]]' WHERE rowid=1"); res.Error != nil {
		t.Fatalf("update shape: %v", res.Error)
	}
	geoScalar(t, db, "SELECT geopoly_json(_shape) FROM g WHERE rowid=1",
		"[[3.0,3.0],[4.0,4.0],[4.0,3.0],[3.0,3.0]]")
	// DELETE.
	if res := db.Exec("DELETE FROM g WHERE rowid=2"); res.Error != nil {
		t.Fatalf("delete: %v", res.Error)
	}
	rows = queryIDs(t, db, "SELECT count(*) FROM g")
	if rows[0][0] != int64(2) {
		t.Fatalf("count after delete: %v", rows)
	}
	// Duplicate explicit rowid: UNIQUE constraint text matches the C
	// (rtreeConstraintError resolves the column from the declared schema).
	if res := db.Exec("INSERT INTO g(rowid,_shape) VALUES(1, '[[20,20],[21,21],[21,20],[20,20]]')"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "UNIQUE constraint failed: g._shape") {
		t.Fatalf("dup rowid: %v", res.Error)
	}
	if res := db.Exec("INSERT OR REPLACE INTO g(rowid,_shape) VALUES(1, '[[20,20],[21,21],[21,20],[20,20]]')"); res.Error != nil {
		t.Fatalf("replace: %v", res.Error)
	}
	geoScalar(t, db, "SELECT geopoly_json(_shape) FROM g WHERE rowid=1",
		"[[20.0,20.0],[21.0,21.0],[21.0,20.0],[20.0,20.0]]")
}

// TestNativeGeopolyVtabQueries covers the geopoly_overlap / geopoly_within
// overload strategies through the vtab scan (bbox candidate narrowing with a
// per-row function re-check).
func TestNativeGeopolyVtabQueries(t *testing.T) {
	db := openGeopolyDB(t)
	defer db.Close()
	if res := db.Exec("INSERT INTO g(_shape) VALUES('[[10,10],[20,20],[20,10],[10,10]]')"); res.Error != nil {
		t.Fatalf("insert 3: %v", res.Error)
	}
	// Overlap: bbox intersect AND true polygon overlap.
	rows := queryIDs(t, db, "SELECT rowid FROM g WHERE geopoly_overlap(_shape, '[[0.5,0.5],[2,2],[2,0.5],[0.5,0.5]]')")
	if len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("overlap query: %v", rows)
	}
	rows = queryIDs(t, db, "SELECT rowid FROM g WHERE geopoly_overlap(_shape, '[[50,50],[51,51],[51,50],[50,50]]')")
	if len(rows) != 0 {
		t.Fatalf("disjoint overlap query: %v", rows)
	}
	// Within: the cell box is contained in the query box.
	rows = queryIDs(t, db, "SELECT rowid FROM g WHERE geopoly_within(_shape, '[[0,0],[30,0],[30,30],[0,30],[0,0]]')")
	if len(rows) != 3 || rows[0][0] != int64(1) || rows[1][0] != int64(2) || rows[2][0] != int64(3) {
		t.Fatalf("within query: %v", rows)
	}
	rows = queryIDs(t, db, "SELECT rowid FROM g WHERE geopoly_within(_shape, '[[0,0],[3,0],[3,3],[0,3],[0,0]]')")
	if len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("tight within query: %v", rows)
	}
	// The function value may come from another table's column (no pushdown,
	// residual re-check only).
	if res := db.Exec("CREATE TABLE q(p JSON)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if res := db.Exec("INSERT INTO q VALUES('[[0.2,0.2],[0.9,0.9],[0.9,0.2],[0.2,0.2]]')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	rows = queryIDs(t, db, "SELECT g.rowid FROM g, q WHERE geopoly_overlap(g._shape, q.p)")
	if len(rows) != 1 || rows[0][0] != int64(1) {
		t.Fatalf("join query: %v", rows)
	}
	// A malformed query polygon fails the statement like xFilter's
	// geopolyBBox error path ("SQL logic error").
	if res := db.Query("SELECT rowid FROM g WHERE geopoly_within(_shape, '[[0,0],[3,3],[3,0],[0,3]]')"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "SQL logic error") {
		t.Fatalf("malformed query polygon: %v", res.Error)
	}
}

// TestNativeGeopolyLifecycle covers module plumbing: shadow family naming,
// reopen-over-existing-shadows (xConnect adoption), ALTER RENAME shadow
// follow and DROP shadow cleanup, and the rtreecheck verdict for a geopoly
// table (Schema corrupt or not an rtree, like SQLite).
func TestNativeGeopolyLifecycle(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res := db.Exec("CREATE VIRTUAL TABLE geo USING geopoly(type,clr)"); res.Error != nil {
		t.Fatal(res.Error)
	}
	rows := queryIDs(t, db, "SELECT name FROM sqlite_master WHERE name LIKE 'geo%' ORDER BY name")
	want := []string{"geo", "geo_node", "geo_parent", "geo_rowid"}
	for i, w := range want {
		if rows[i][0] != w {
			t.Fatalf("shadow family: got %v", rows)
		}
	}
	if res := db.Exec("INSERT INTO geo(_shape,type,clr) VALUES('[[0,0],[1,1],[1,0],[0,0]]','box','red')"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// Declared columns resolve; _shape has no declared type.
	rows = queryIDs(t, db, "SELECT * FROM pragma_table_info('geo')")
	if len(rows) != 3 || rows[0][1] != "_shape" || rows[1][1] != "type" || rows[2][1] != "clr" {
		t.Fatalf("table_info: %v", rows)
	}
	// rtreecheck on a geopoly family reports the same verdict as SQLite.
	geoScalar(t, db, "SELECT rtreecheck('geo')", "Schema corrupt or not an rtree")
	// ALTER TABLE RENAME follows the shadow tables.
	if res := db.Exec("ALTER TABLE geo RENAME TO geo2"); res.Error != nil {
		t.Fatal(res.Error)
	}
	rows = queryIDs(t, db, "SELECT name FROM sqlite_master WHERE name LIKE 'geo%' ORDER BY name")
	if rows[0][0] != "geo2" || rows[1][0] != "geo2_node" {
		t.Fatalf("rename: %v", rows)
	}
	if res := db.Exec("DROP TABLE geo2"); res.Error != nil {
		t.Fatal(res.Error)
	}
	rows = queryIDs(t, db, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'geo%'")
	if rows[0][0] != int64(0) {
		t.Fatalf("drop cleanup: %v", rows)
	}
	// geopoly is not eponymous: FROM geopoly fails without a created table.
	if r := db.Query("SELECT * FROM geopoly"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "no such table") {
		t.Fatalf("eponymous use: %v", r.Error)
	}
}
