package frigolite_test

// Perf-tranche pin tests (fleet/perf-dml2): the point-UPDATE in-place
// overwrite and the O(1) point-DELETE (btree.c dropCell) must preserve exact
// row data, file-format legality (integrity_check), and their engagement
// boundaries — in place ONLY when the serialized record replaces the old one
// byte-for-byte at the same size; dropCell freeblocks only where cells were
// actually removed. Page-boundary shapes (split points, overflow
// transitions, deleted-rowid reuse) are exercised explicitly.

import (
	"fmt"
	"testing"

	frigo "github.com/pijalu/frigolite"
)

func mustExecT(t *testing.T, db *frigo.DB, sql string) {
	t.Helper()
	if r := db.Exec(sql); r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
}

func queryIntT(t *testing.T, db *frigo.DB, sql string) int64 {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: %v", sql, r.Error)
	}
	if len(r.Rows) == 0 {
		t.Fatalf("%s: no rows", sql)
	}
	switch v := r.Rows[0][0].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	default:
		t.Fatalf("%s: non-integer result %v (%T)", sql, r.Rows[0][0], r.Rows[0][0])
	}
	return 0
}

func integrityT(t *testing.T, db *frigo.DB) {
	t.Helper()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check reported: %v", r.Rows)
	}
}

// dbstatUnused sums the unused (freeblock + gap + fragmented) bytes across
// the named table's pages — the observable witness of dropCell's O(1)
// accounting (the pre-tranche delete always compacted pages, leaving the
// gap-slack minimum).
func dbstatUnused(t *testing.T, db *frigo.DB, table string) int64 {
	t.Helper()
	r := db.Query(fmt.Sprintf("SELECT sum(unused) FROM dbstat WHERE name='%s'", table))
	if r.Error != nil {
		t.Fatalf("dbstat: %v", r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][0] == nil {
		return 0
	}
	switch v := r.Rows[0][0].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	default:
		t.Fatalf("dbstat sum(unused) = %T", r.Rows[0][0])
	}
	return 0
}

// tableIDs returns the ordered id column of t.
func tableIDs(t *testing.T, db *frigo.DB, table string) []int64 {
	t.Helper()
	r := db.Query(fmt.Sprintf("SELECT id FROM %s ORDER BY id", table))
	if r.Error != nil {
		t.Fatalf("ids: %v", r.Error)
	}
	ids := make([]int64, 0, len(r.Rows))
	for _, row := range r.Rows {
		ids = append(ids, row[0].(int64))
	}
	return ids
}

func wantIDsRange(lo, hi int64, skip func(int64) bool) []int64 {
	var ids []int64
	for i := lo; i <= hi; i++ {
		if skip == nil || !skip(i) {
			ids = append(ids, i)
		}
	}
	return ids
}

func requireIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d id = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestPinInPlaceUpdateAndFastDeleteShapes drives same-size point UPDATEs,
// point DELETEs, size-changing updates, overflow transitions and rowid
// reuse through the point paths, asserting exact rows and integrity_check
// after every shape.
func TestPinInPlaceUpdateAndFastDeleteShapes(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecT(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER, s TEXT)")
	mustExecT(t, db, "BEGIN")
	for i := int64(1); i <= 4000; i++ {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, 's%04d')", i, i*7%1000003, i))
	}
	mustExecT(t, db, "COMMIT")

	// Same-size point UPDATEs (c stays within its integer serial class):
	// the in-place overwrite must engage. Witness: page free space does not
	// move (the old delete+reinsert path re-allocated the cell).
	before := dbstatUnused(t, db, "t")
	mustExecT(t, db, "BEGIN")
	for i := int64(1); i <= 4000; i++ {
		mustExecT(t, db, fmt.Sprintf("UPDATE t SET c=c+1 WHERE id=%d", i))
	}
	mustExecT(t, db, "COMMIT")
	after := dbstatUnused(t, db, "t")
	if after != before {
		t.Fatalf("same-size updates moved page free space: %d -> %d (in-place path did not engage)", before, after)
	}
	if got := queryIntT(t, db, "SELECT c FROM t WHERE id=100"); got != 701 {
		t.Fatalf("c(100) = %d", got)
	}
	integrityT(t, db)

	// Size-changing point UPDATE (serial class grows): must NOT take the
	// in-place path — cell re-allocation changes the free-space picture.
	mustExecT(t, db, "BEGIN")
	for i := int64(1); i <= 200; i++ {
		mustExecT(t, db, fmt.Sprintf("UPDATE t SET c=c*1000000 WHERE id=%d", i*3))
	}
	mustExecT(t, db, "COMMIT")
	if got := queryIntT(t, db, "SELECT c FROM t WHERE id=3"); got != (3*7%1000003+1)*1000000 {
		t.Fatalf("c(3) after growth = %d", got)
	}
	integrityT(t, db)

	// Point DELETEs of every third row: dropCell freeblocks appear (the
	// compaction-forever path would leave the gap-slack minimum), rows
	// stay exact.
	mustExecT(t, db, "BEGIN")
	for i := int64(1); i <= 4000; i += 3 {
		mustExecT(t, db, fmt.Sprintf("DELETE FROM t WHERE id=%d", i))
	}
	mustExecT(t, db, "COMMIT")
	unusedAfterDeletes := dbstatUnused(t, db, "t")
	if unusedAfterDeletes <= after {
		t.Fatalf("point deletes left no free space (%d <= %d): dropCell path did not engage", unusedAfterDeletes, after)
	}
	requireIDs(t, tableIDs(t, db, "t"), wantIDsRange(1, 4000, func(i int64) bool { return i%3 == 1 }))
	integrityT(t, db)

	// Deleted-rowid reuse: fresh inserts after the deletes must land in the
	// vacated slots and stay seekable.
	mustExecT(t, db, "BEGIN")
	for i := int64(1); i <= 100; i += 3 {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO t VALUES(%d, %d, 'r%04d')", i, i, i))
	}
	mustExecT(t, db, "COMMIT")
	if got := queryIntT(t, db, "SELECT c FROM t WHERE id=1"); got != 1 {
		t.Fatalf("reinserted id=1 c=%d", got)
	}
	requireIDs(t, tableIDs(t, db, "t"), func() []int64 {
		reused := map[int64]bool{}
		for i := int64(1); i <= 100; i += 3 {
			reused[i] = true
		}
		return wantIDsRange(1, 4000, func(i int64) bool { return i%3 == 1 && !reused[i] })
	}())
	integrityT(t, db)
}

// TestPinOverflowTransitions pins the point paths across overflow-page
// boundaries: small->big (in-place declines), big->small (declines; the
// freed chain returns to the freelist), and deleting overflow-bearing rows.
func TestPinOverflowTransitions(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecT(t, db, "CREATE TABLE b(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 200; i++ {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO b VALUES(%d, '%s')", i, blobOf(i, 40)))
	}
	integrityT(t, db)

	// small -> big: payload spills to overflow pages (in-place must decline;
	// the cell is re-allocated with an overflow chain).
	for i := 1; i <= 40; i++ {
		mustExecT(t, db, fmt.Sprintf("UPDATE b SET v='%s' WHERE id=%d", blobOf(i, 4000), i))
	}
	if got := dbstatRowCount(t, db, "b"); got != 200 {
		t.Fatalf("rows after grow = %d", got)
	}
	integrityT(t, db)

	// big -> small: the chain frees; the page space accounting stays legal.
	for i := 1; i <= 40; i++ {
		mustExecT(t, db, fmt.Sprintf("UPDATE b SET v='%s' WHERE id=%d", blobOf(i, 30), i))
	}
	integrityT(t, db)

	// Delete overflow-bearing rows (the freed chains return to the freelist
	// before any page mutation).
	mustExecT(t, db, "BEGIN")
	for i := 41; i <= 80; i++ {
		mustExecT(t, db, fmt.Sprintf("UPDATE b SET v='%s' WHERE id=%d", blobOf(i, 5000), i))
	}
	for i := 41; i <= 80; i++ {
		mustExecT(t, db, fmt.Sprintf("DELETE FROM b WHERE id=%d", i))
	}
	mustExecT(t, db, "COMMIT")
	if got := dbstatRowCount(t, db, "b"); got != 160 {
		t.Fatalf("rows after overflow deletes = %d", got)
	}
	integrityT(t, db)
}

// TestPinSplitBoundaryChurn pins delete/insert churn across leaf split
// points: the tree reorganizes under the point paths while every committed
// image stays readable and rowid-ordered.
func TestPinSplitBoundaryChurn(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecT(t, db, "CREATE TABLE s(id INTEGER PRIMARY KEY, v TEXT)")
	for i := 1; i <= 3000; i++ {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO s VALUES(%d, '%s')", i, blobOf(i, 24)))
	}
	// Delete a window that spans several leaves, then refill it.
	mustExecT(t, db, "BEGIN")
	for i := 900; i <= 1100; i++ {
		mustExecT(t, db, fmt.Sprintf("DELETE FROM s WHERE id=%d", i))
	}
	mustExecT(t, db, "COMMIT")
	for i := 900; i <= 1100; i++ {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO s VALUES(%d, '%s')", i, blobOf(i, 24)))
	}
	if got := dbstatRowCount(t, db, "s"); got != 3000 {
		t.Fatalf("rows after churn = %d", got)
	}
	if got := queryIntT(t, db, "SELECT count(*) FROM s WHERE id BETWEEN 950 AND 1050"); got != 101 {
		t.Fatalf("refilled window count = %d", got)
	}
	integrityT(t, db)
}

// TestPinPointRollbackExactness pins the statement journal's before-image
// exactness under the point paths: a failed statement inside a transaction
// restores exactly its own pages, leaving earlier statements' writes intact.
func TestPinPointRollbackExactness(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExecT(t, db, "CREATE TABLE r(id INTEGER PRIMARY KEY, c INTEGER)")
	for i := 1; i <= 300; i++ {
		mustExecT(t, db, fmt.Sprintf("INSERT INTO r VALUES(%d, %d)", i, i))
	}
	// A UNIQUE table so the second update of the txn can fail.
	mustExecT(t, db, "CREATE TABLE u(k INTEGER PRIMARY KEY, v UNIQUE)")
	mustExecT(t, db, "INSERT INTO u VALUES(1, 10)")
	mustExecT(t, db, "INSERT INTO u VALUES(2, 20)")

	mustExecT(t, db, "BEGIN")
	mustExecT(t, db, "UPDATE r SET c=c+1 WHERE id=5") // succeeds, commits to the txn
	if res := db.Exec("UPDATE u SET v=10 WHERE k=2"); res.Error == nil {
		t.Fatalf("expected UNIQUE violation")
	}
	// A point DELETE after the failed statement: the txn is still usable.
	mustExecT(t, db, "DELETE FROM r WHERE id=7")
	mustExecT(t, db, "COMMIT")

	if got := queryIntT(t, db, "SELECT c FROM r WHERE id=5"); got != 6 {
		t.Fatalf("earlier statement's write lost: c(5)=%d", got)
	}
	if got := queryIntT(t, db, "SELECT count(*) FROM r WHERE id=7"); got != 0 {
		t.Fatalf("post-failure delete not applied")
	}
	if got := queryIntT(t, db, "SELECT v FROM u WHERE k=2"); got != 20 {
		t.Fatalf("failed statement's partial write leaked: v(2)=%d", got)
	}
	integrityT(t, db)
}

func blobOf(seed, n int) string {
	b := make([]byte, n)
	x := seed
	for i := range b {
		x = x*1103515245 + 12345
		b[i] = byte('a' + x%26)
	}
	return string(b)
}

func dbstatRowCount(t *testing.T, db *frigo.DB, table string) int64 {
	t.Helper()
	return queryIntT(t, db, fmt.Sprintf("SELECT count(*) FROM %s", table))
}
