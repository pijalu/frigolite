package frigolite

import (
	"fmt"
	"testing"
)

// Native rtree QUERY tests (t4 slice): every case runs a spatial SQL query
// against the engine and compares the returned id set with a brute-force
// evaluation of the same predicate over the seeded rectangle model, computed
// here in plain Go. This keeps query-path regressions independent from the
// TCL transpiler suites in testgen/.

func TestNativeRtreeCoordinatePushdown(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	rects := seedRects(120)
	insertRects(t, db, rects)

	cases := []struct {
		name string
		sql  string
		want func(rect) bool
	}{
		{"x-lower-bound", "SELECT id FROM rt WHERE x1>=200",
			func(r rect) bool { return r.x1 >= 200 }},
		{"x-upper-bound", "SELECT id FROM rt WHERE x2<=305",
			func(r rect) bool { return r.x2 <= 305 }},
		{"y-window", "SELECT id FROM rt WHERE y1>=150 AND y2<=280",
			func(r rect) bool { return r.y1 >= 150 && r.y2 <= 280 }},
		{"canonical-window", "SELECT id FROM rt WHERE x1>=200 AND x2<=800 AND y1>=100 AND y2<=300",
			func(r rect) bool { return r.x1 >= 200 && r.x2 <= 800 && r.y1 >= 100 && r.y2 <= 300 }},
		{"mixed-dims", "SELECT id FROM rt WHERE x1>500 AND y1<120",
			func(r rect) bool { return r.x1 > 500 && r.y1 < 120 }},
		{"empty-window", "SELECT id FROM rt WHERE x1>=100000",
			func(r rect) bool { return false }},
		{"crossing-null-window", "SELECT id FROM rt WHERE x1>=800 AND x2<=10",
			func(r rect) bool { return false }},
		{"strict-greater", "SELECT id FROM rt WHERE x2>790 AND x2<830",
			func(r rect) bool { return r.x2 > 790 && r.x2 < 830 }},
	}
	for _, tc := range cases {
		got := idsFromRows(queryIDs(t, db, tc.sql))
		var want []int64
		for _, r := range rects {
			if tc.want(r) {
				want = append(want, r.id)
			}
		}
		if !equalIDSets(got, want) {
			t.Fatalf("%s:\n got %v\nwant %v", tc.name, got, want)
		}
	}
}

// TestNativeRtreeRowidPredicates verifies `id` pushdown (exact, IN-list,
// BETWEEN, OR forms) combined with residual non-pushed conjuncts.
func TestNativeRtreeRowidPredicates(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	rects := seedRects(90)
	insertRects(t, db, rects)

	in := func(list ...int64) map[int64]bool {
		m := make(map[int64]bool, len(list))
		for _, v := range list {
			m[v] = true
		}
		return m
	}
	between := func(r rect, lo, hi int64) bool { return r.id >= lo && r.id <= hi }

	cases := []struct {
		sql  string
		want func(rect) bool
	}{
		{"SELECT id FROM rt WHERE id=40", func(r rect) bool { return r.id == 40 }},
		{"SELECT id FROM rt WHERE id IN (5,10,15,999)",
			func(r rect) bool { return in(5, 10, 15, 999)[r.id] }},
		{"SELECT id FROM rt WHERE id BETWEEN 3 AND 20", func(r rect) bool { return between(r, 3, 20) }},
		{"SELECT id FROM rt WHERE id<=5 OR id>=85",
			func(r rect) bool { return r.id <= 5 || r.id >= 85 }},
		// Rowid pushed AND residual coordinate conjunct evaluated afterwards.
		{"SELECT id FROM rt WHERE id BETWEEN 3 AND 60 AND y2>360",
			func(r rect) bool { return between(r, 3, 60) && r.y2 > 360 }},
		// Coordinate pushed AND residual scalar conjunct.
		{"SELECT id FROM rt WHERE y1>=380 AND id%7=3",
			func(r rect) bool { return r.y1 >= 380 && r.id%7 == 3 }},
	}
	for _, tc := range cases {
		got := idsFromRows(queryIDs(t, db, tc.sql))
		var want []int64
		for _, r := range rects {
			if tc.want(r) {
				want = append(want, r.id)
			}
		}
		if !equalIDSets(got, want) {
			t.Fatalf("%s:\n got %v\nwant %v", tc.sql, got, want)
		}
	}
}

// TestNativeRtreeOrderLimit asserts deterministic ordering on top of scans and
// LIMIT truncation after pushdown pruning.
func TestNativeRtreeOrderLimit(t *testing.T) {
	db := openRtreeDB(t)
	defer db.Close()
	rects := seedRects(60)
	insertRects(t, db, rects)

	rows := queryIDs(t, db, "SELECT id FROM rt WHERE x1<400 ORDER BY x1 DESC, id ASC")
	if len(rows) < 10 {
		t.Fatalf("too few pushed rows: %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1][0].(int64), rows[i][0].(int64)
		p1, c1 := lookupX1(t, db, prev), lookupX1(t, db, cur)
		if p1 < c1 {
			t.Fatalf("order violated at %d: %v then %v", i, p1, c1)
		}
	}

	lim := queryIDs(t, db, "SELECT id FROM rt ORDER BY id LIMIT 7")
	if len(lim) != 7 {
		t.Fatalf("LIMIT: want 7 got %d", len(lim))
	}
	for i, r := range lim {
		if want := int64(i + 1); r[0] != want {
			t.Fatalf("LIMIT rank %d: want %d got %v", i, want, r[0])
		}
	}
}

func lookupX1(t *testing.T, db *DB, id int64) float64 {
	t.Helper()
	rows := queryIDs(t, db, fmt.Sprintf("SELECT x1 FROM rt WHERE id=%d", id))
	if len(rows) != 1 {
		t.Fatalf("lookup %d: %d rows", id, len(rows))
	}
	f, _ := rows[0][0].(float64)
	return f
}

// ---- small helpers ----

func idsFromRows(rows [][]interface{}) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[0].(int64))
	}
	return out
}

// equalIDSets compares id lists as sets (rtree scans have no SQL-guaranteed
// row order; ordering is asserted separately via ORDER BY cases).
func equalIDSets(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := append([]int64(nil), a...), append([]int64(nil), b...)
	insertionSort(sa)
	insertionSort(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func insertionSort(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
