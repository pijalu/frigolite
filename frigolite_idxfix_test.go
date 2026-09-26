package frigolite

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/pijalu/frigolite/internal/util"
)

// T33-idxfix pins: autoindex (UNIQUE-constraint) b-trees must stay globally
// value-ordered at multi-page scale. The insert walk used to route every
// index insert to the interior page's rightmost child, so once a tree grew
// past one leaf the entries formed per-batch sorted runs (batch-grouped
// order) instead of one global value order — and the ORDER-BY emitter, which
// trusts the index b-tree's stored order, returned unsorted rows
// (testgen/misc5: "select x from t2 order by x" over a 371-row table).

// idxfixBuildT2 runs the misc5 t2 sequence: a unique-integer table grown by
// doubling/broadening batches until the autoindex b-tree spans many leaves.
func idxfixBuildT2(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	steps := []string{
		"create table t2(x unique)",
		"insert into t2 values(1)",
		"insert or ignore into t2 select x*2 from t2",
		"insert or ignore into t2 select x*4 from t2",
		"insert or ignore into t2 select x*16 from t2",
		"insert or ignore into t2 select x*256 from t2",
		"insert or ignore into t2 select x*65536 from t2",
		"insert or ignore into t2 select x*2147483648 from t2",
		"insert or ignore into t2 select x-1 from t2",
		"insert or ignore into t2 select x+1 from t2",
		"insert or ignore into t2 select -x from t2",
	}
	for _, sql := range steps {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return db
}

// idxfixNumKey canonically keys one numeric value for UNIQUE-set membership:
// int64 and an integral float64 equal under SQLite's comparison share a key
// (5 == 5.0); a float outside the int64 domain keys as itself.
type idxfixNumKey struct {
	isInt bool
	i     int64
	f     float64
}

func idxfixNumKeyOf(v interface{}) idxfixNumKey {
	switch x := v.(type) {
	case int64:
		return idxfixNumKey{isInt: true, i: x}
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) && x >= -9223372036854775808.0 && x < 9223372036854775808.0 {
			return idxfixNumKey{isInt: true, i: int64(x)}
		}
		return idxfixNumKey{f: x}
	}
	return idxfixNumKey{f: math.NaN()}
}

// idxfixApply computes one arithmetic step on one value with SQLite's
// integer-overflow-to-REAL rule.
func idxfixApply(v interface{}, fi func(int64) interface{}, ff func(float64) interface{}) interface{} {
	if i, ok := v.(int64); ok {
		return fi(i)
	}
	return ff(v.(float64))
}

// idxfixIntMul is i*m with overflow conversion to REAL (SQLite's * operator).
func idxfixIntMul(i, m int64) interface{} {
	if i > math.MaxInt64/m || i < math.MinInt64/m {
		return float64(i) * float64(m)
	}
	return i * m
}

// idxfixIntAdd is i+d with overflow conversion to REAL (SQLite's +/-).
func idxfixIntAdd(i, d int64) interface{} {
	if (d > 0 && i > math.MaxInt64-d) || (d < 0 && i < math.MinInt64-d) {
		return float64(i) + float64(d)
	}
	return i + d
}

// idxfixExpectedT2 simulates the t2 sequence in Go and returns the values in
// SQLite value order (numeric; int64 5 == float64 5.0).
func idxfixExpectedT2() []interface{} {
	seen := map[idxfixNumKey]bool{}
	var vals []interface{}
	add := func(v interface{}) {
		k := idxfixNumKeyOf(v)
		if seen[k] {
			return
		}
		seen[k] = true
		vals = append(vals, v)
	}
	mapSet := func(f func(int64) interface{}, ff func(float64) interface{}) {
		snap := append([]interface{}{}, vals...)
		for _, v := range snap {
			add(idxfixApply(v, f, ff))
		}
	}
	// Grow vals in place over a snapshot: INSERT ... SELECT adds the
	// transformed rows to the existing set ("or ignore" drops duplicates).
	grow := func(f func(int64) interface{}) {
		mapSet(f, func(g float64) interface{} { return g })
	}
	add(int64(1))
	grow(func(v int64) interface{} { return idxfixIntMul(v, 2) })
	grow(func(v int64) interface{} { return idxfixIntMul(v, 4) })
	grow(func(v int64) interface{} { return idxfixIntMul(v, 16) })
	grow(func(v int64) interface{} { return idxfixIntMul(v, 256) })
	grow(func(v int64) interface{} { return idxfixIntMul(v, 65536) })
	grow(func(v int64) interface{} { return idxfixIntMul(v, 2147483648) })
	grow(func(v int64) interface{} { return idxfixIntAdd(v, -1) })
	grow(func(v int64) interface{} { return idxfixIntAdd(v, 1) })
	mapSet(func(v int64) interface{} {
		if v == math.MinInt64 {
			return -float64(v) // -(-2^63) overflows to REAL 2^63
		}
		return -v
	}, func(g float64) interface{} { return -g })
	sort.Slice(vals, func(a, b int) bool {
		return util.CompareValues(vals[a], vals[b]) < 0
	})
	return vals
}

// idxfixQueryX returns t2's x column under ORDER BY x as Go values.
func idxfixQueryX(t *testing.T, db *DB, sql string) []interface{} {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("%s: %v", sql, res.Error)
	}
	out := make([]interface{}, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, row[0])
	}
	return out
}

// idxfixAssertSameValueOrder asserts two value lists are equal as multisets
// in the same order under SQLite value comparison.
func idxfixAssertSameValueOrder(t *testing.T, label string, got, want []interface{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows, want %d", label, len(got), len(want))
	}
	for i := range got {
		if util.CompareValues(got[i], want[i]) != 0 {
			t.Fatalf("%s: row %d: got %v (%T), want %v (%T)", label, i, got[i], got[i], want[i], want[i])
		}
	}
}

// TestNativeIdxfixMultiLeafOrder pins the misc5 t2 repro: ORDER BY x over a
// multi-leaf unique-integer autoindex must return the full table in value
// order (the oracle's first twelve values are spelled out below).
func TestNativeIdxfixMultiLeafOrder(t *testing.T) {
	db := idxfixBuildT2(t)
	defer db.Close()

	got := idxfixQueryX(t, db, "select x from t2 order by x")
	want := idxfixExpectedT2()
	if len(want) < 12 {
		t.Fatalf("simulation produced %d rows", len(want))
	}
	idxfixAssertSameValueOrder(t, "order by x", got, want)

	first12 := []interface{}{
		int64(-4611686018427387905), int64(-4611686018427387904), int64(-4611686018427387903),
		int64(-2305843009213693953),
	}
	for i, v := range first12 {
		if util.CompareValues(got[i], v) != 0 {
			t.Fatalf("first-12[%d]: got %v, want %v (full head: %v)", i, got[i], v, got[:12])
		}
	}
}

// TestNativeIdxfixMixedTypeOrder pins a multi-leaf index over MIXED
// integer/text keys: the stored order must be the SQLite type order
// (numerics before text, each class internally ordered), not byte order and
// not batch order.
func TestNativeIdxfixMixedTypeOrder(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Exec("create table m(x); create index m_x on m(x)").Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	keys := []interface{}{
		int64(0), int64(-1), int64(1), int64(2), int64(-300), int64(500),
		"", "a", "b", "abc", "zzz", "A", "Z",
	}
	// Scrambled inserts in several batches so the tree crosses leaf splits
	// between batches; repeated indices make the index a multiset.
	batches := [][]int{
		{3, 0, 7, 11, 5, 9},
		{1, 12, 4, 8, 2, 6},
		{10, 4, 1, 3, 7, 0},
		{2, 5, 6, 8, 9, 10},
		{11, 12},
	}
	for _, b := range batches {
		for _, ki := range b {
			var sql string
			switch v := keys[ki].(type) {
			case int64:
				sql = fmt.Sprintf("insert into m values(%d)", v)
			case string:
				sql = fmt.Sprintf("insert into m values('%s')", v)
			default:
				t.Fatalf("bad key")
			}
			if err := db.Exec(sql).Error; err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
	// The index holds the multiset of everything inserted.
	var want []interface{}
	for _, b := range batches {
		for _, ki := range b {
			want = append(want, keys[ki])
		}
	}
	sort.SliceStable(want, func(a, b int) bool {
		return util.CompareValues(want[a], want[b]) < 0
	})
	got := idxfixQueryX(t, db, "select x from m order by x")
	idxfixAssertSameValueOrder(t, "mixed order by x", got, want)
}

// TestNativeIdxfixReindexPreservesOrder pins REINDEX over a multi-leaf
// autoindex: the rebuilt b-tree must keep the value order (the rebuild
// re-inserts through the same insert walk, so a descent defect would
// re-scramble it).
func TestNativeIdxfixReindexPreservesOrder(t *testing.T) {
	db := idxfixBuildT2(t)
	defer db.Close()

	before := idxfixQueryX(t, db, "select x from t2 order by x")
	if err := db.Exec("reindex t2").Error; err != nil {
		t.Fatalf("reindex: %v", err)
	}
	after := idxfixQueryX(t, db, "select x from t2 order by x")
	idxfixAssertSameValueOrder(t, "after reindex", after, before)

	res := db.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatalf("integrity_check: %v", res.Error)
	}
	if len(res.Rows) != 1 || fmt.Sprintf("%v", res.Rows[0][0]) != "ok" {
		t.Fatalf("integrity_check: %v", res.Rows)
	}
}
