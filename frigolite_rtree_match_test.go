package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Native rtree MATCH tests (t5 slice): geometry callbacks registered through
// the public RegisterRtreeGeometry surface, invoked per-cell by the tree scan
// with rtree.c's semantics, plus the misuse contracts from ext/rtree
// (non-geometry MATCH arguments are SQL logic errors, marker values cannot be
// concatenated or cast).

// cube3D and circle2D re-implement test_rtree.c's callbacks in Go so the UT's
// expected sets derive from the documented region shapes rather than from the
// engine under test.
func cubeIntersects(x1, x2, y1, y2, z1, z2 float64) bool {
	return boxOverlap(x1, x2, 30, 36) && // cube(30,40,50,6,7,8): x [30,36]
		boxOverlap(y1, y2, 40, 47) && //                          y [40,47]
		boxOverlap(z1, z2, 50, 58) //                             z [50,58]
}

func boxOverlap(lo, hi, bLo, bHi float64) bool {
	return lo <= bHi && hi >= bLo
}

func circleMatches(cx, cy, r, x1, x2, y1, y2 float64) bool {
	corners := [4][2]float64{{x1, y1}, {x2, y1}, {x1, y2}, {x2, y2}}
	for _, c := range corners {
		dx, dy := c[0]-cx, c[1]-cy
		if dx*dx+dy*dy < r*r {
			return true
		}
	}
	// Whole-cross-arm containment (aBox[0] then aBox[1] from the C source).
	if x1 <= cx && x2 >= cx && y1 <= cy+r && y2 >= cy-r {
		return true
	}
	return x1 <= cx+r && x2 >= cx-r && y1 <= cy && y2 >= cy
}

func TestNativeRtreeCircleMatch(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatalf("register circle: %v", err)
	}
	rects := seedRects(200)
	insertRects(t, db, rects)

	rows := queryIDs(t, db, "SELECT id FROM rt WHERE y1 MATCH circle(500,500,100)")
	var want []int64
	for _, r := range rects {
		if circleMatches(500, 500, 100, r.x1, r.x2, r.y1, r.y2) {
			want = append(want, r.id)
		}
	}
	got := idsFromRows(rows)
	if !equalIDSets(got, want) || len(got) == 0 {
		t.Fatalf("circle MATCH:\n got %v\nwant %v", got, want)
	}

	// Zero radius matches only cells whose MBR touches the center point
	// (inclusive overlap arms, corners strictly inside impossible).
	zr := queryIDs(t, db, "SELECT id FROM rt WHERE y1 MATCH circle(150,150,0)")
	for _, row := range zr {
		id := row[0].(int64)
		r := rects[id-1]
		touch := (r.x1 <= 150 && r.x2 >= 150 && r.y1 <= 250 && r.y2 >= 50) ||
			(r.x1 <= 250 && r.x2 >= 50 && r.y1 <= 150 && r.y2 >= 150)
		if !touch {
			t.Fatalf("radius 0 matched non-touching id %d (%v)", id, r)
		}
	}
}

// TestNativeRtreeCircleMatchCentered sweeps the queried center across a grid
// of stored boxes so interior MBRs, leaf hits and prunes all occur.
func TestNativeRtreeCircleMatchCentered(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatal(err)
	}
	rects := seedRects(120)
	insertRects(t, db, rects)

	cases := [][3]float64{{100, 100, 60}, {300, 200, 30}, {550, 350, 120}, {-50, -50, 90}}
	for _, c := range cases {
		sql := fmt.Sprintf("SELECT id FROM rt WHERE y1 MATCH circle(%g,%g,%g)", c[0], c[1], c[2])
		got := idsFromRows(queryIDs(t, db, sql))
		var want []int64
		for _, r := range rects {
			if circleMatches(c[0], c[1], c[2], r.x1, r.x2, r.y1, r.y2) {
				want = append(want, r.id)
			}
		}
		if !equalIDSets(got, want) {
			t.Fatalf("circle%v:\n got %v\nwant %v", c, got, want)
		}
	}
}

func TestNativeRtreeCubeMatch(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.RegisterRtreeGeometry("cube"); err != nil {
		t.Fatalf("register cube: %v", err)
	}
	res := db.Exec("CREATE VIRTUAL TABLE rc USING rtree(id,x1,x2,y1,y2,z1,z2)")
	if res.Error != nil {
		t.Fatalf("create 3-D rtree: %v", res.Error)
	}

	type cbox struct {
		id                     int64
		x1, x2, y1, y2, z1, z2 float64
	}
	var boxes []cbox
	for i := 1; i <= 120; i++ {
		b := cbox{
			id: int64(i),
			x1: float64(10 * i), x2: float64(10*i + 4),
			y1: float64((i * 31) % 300), y2: float64((i*31)%300 + 5),
			z1: float64((i * 17) % 240), z2: float64((i*17)%240 + 3),
		}
		if res := db.Exec(sprintfInsert("rc", b.id, b.x1, b.x2, b.y1, b.y2, b.z1, b.z2)); res.Error != nil {
			t.Fatalf("insert %d: %v", b.id, res.Error)
		}
		boxes = append(boxes, b)
	}
	// A cluster deliberately interleaved with the queried cube region:
	// straddling, corner-touching and fully-containing shapes (ids 201..204).
	extra := []cbox{
		{id: 201, x1: 32, x2: 40, y1: 42, y2: 50, z1: 52, z2: 60},    // contains it
		{id: 202, x1: 28, x2: 33, y1: 38, y2: 43, z1: 48, z2: 53},    // corner overlap
		{id: 203, x1: 36.5, x2: 44, y1: 46, y2: 55, z1: 57, z2: 70},  // edge touch
		{id: 204, x1: 0, x2: 1000, y1: 0, y2: 1000, z1: 0, z2: 1000}, // huge MBR
	}
	for _, b := range extra {
		if res := db.Exec(sprintfInsert("rc", b.id, b.x1, b.x2, b.y1, b.y2, b.z1, b.z2)); res.Error != nil {
			t.Fatalf("insert %d: %v", b.id, res.Error)
		}
		boxes = append(boxes, b)
	}

	rows := queryIDs(t, db,
		"SELECT id FROM rc WHERE x1 MATCH cube(30,40,50,6,7,8)")
	var want []int64
	for _, b := range boxes {
		if cubeIntersects(b.x1, b.x2, b.y1, b.y2, b.z1, b.z2) {
			want = append(want, b.id)
		}
	}
	got := idsFromRows(rows)
	if !equalIDSets(got, want) || len(got) == 0 {
		t.Fatalf("cube MATCH:\n got %v\nwant %v", got, want)
	}
}

func TestNativeRtreeMatchMisuse(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterRtreeGeometry("cube"); err != nil {
		t.Fatal(err)
	}
	rects := seedRects(20)
	insertRects(t, db, rects)

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"literal-string-rhs", "SELECT id FROM rt WHERE x1 MATCH '1234'", "SQL logic error"},
		{"unknown-function", "SELECT id FROM rt WHERE x1 MATCH nosuchgeom(1,2)", "no such function"},
		{"concat-with-marker", "SELECT id FROM rt WHERE x1 MATCH 'abc' || circle(1,1,1)", "SQL logic error"},
		{"cast-marker", "SELECT id FROM rt WHERE x1 MATCH cast(circle(1,1,1) AS INTEGER)", "SQL logic error"},
		{"circle-wrong-coords", "SELECT id FROM rt WHERE x1 MATCH cube(1,1)", "SQL logic error"},
		{"cube-bad-dims", "SELECT id FROM rt WHERE x1 MATCH cube(1,1,1,0,1,1)", "SQL logic error"},
		{"cube-arity", "SELECT id FROM rt WHERE x1 MATCH cube(1,1,1,1,1)", "SQL logic error"},
		{"circle-neg-radius", "SELECT id FROM rt WHERE x1 MATCH circle(1,1,-1)", "SQL logic error"},
	}
	for _, tc := range cases {
		res := db.Query(tc.sql)
		if res.Error == nil {
			t.Fatalf("%s: want error %q, got rows %v", tc.name, tc.want, res.Rows)
		}
		if !strings.Contains(strings.ToLower(res.Error.Error()), strings.ToLower(tc.want)) {
			t.Fatalf("%s: got %q want substring %q", tc.name, res.Error.Error(), tc.want)
		}
	}
}

// TestNativeRtreeMatchWithConjunct verifies MATCH combines correctly with a
// residual coordinate conjunct evaluated outside the pushed filter.
func TestNativeRtreeMatchWithConjunct(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	if err := db.RegisterRtreeGeometry("circle"); err != nil {
		t.Fatal(err)
	}
	rects := seedRects(120)
	insertRects(t, db, rects)

	rows := queryIDs(t, db, "SELECT id FROM rt WHERE y1 MATCH circle(600,600,80) AND x2<=700")
	var want []int64
	for _, r := range rects {
		if circleMatches(600, 600, 80, r.x1, r.x2, r.y1, r.y2) && r.x2 <= 700 {
			want = append(want, r.id)
		}
	}
	got := idsFromRows(rows)
	if !equalIDSets(got, want) {
		t.Fatalf("MATCH + conjunct:\n got %v\nwant %v", got, want)
	}
}
