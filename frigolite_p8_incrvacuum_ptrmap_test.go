// P8.INCRVACUUM.phase15 UCL repros: ptrmap entries must be written at
// allocation time and updated whenever a page's parent changes, mirroring
// SQLite src/btree.c (ptrmapPut at every allocateBtreePage consumer,
// ptrmapPutOvflPtr when a cell moves pages, PTRMAP_FREEPAGE in freePage).
//
// Fixtures below were produced by /usr/bin/sqlite3 (3.5x) with the exact
// SQL sequence driven here (page_size=1024, auto_vacuum=INCREMENTAL):
//
//	INSERT×3 rows with 5000-byte blobs → 18 pages
//	DELETE a=2 (no vacuum)             → freed pages carry type FREE
//	incremental_vacuum                 → 13 pages, entries compacted
//
// Every fix in this goal must keep these fixtures green (anti-regression)
// and the pre-fix engine must fail them (UCL red-first).

package frigolite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// ptrmapFixture is one ptrmap entry: page number → (type, parent).
type ptrmapFixture map[uint32]struct {
	typ    byte
	parent uint32
}

// oracleInsert is the post-INSERT×3 fixture (18-page file). Layout:
// page 2 = ptrmap; 3 = table root; leaves 12, 13, 18 (parent 3);
// overflow chains 4→5→6→7 (first's parent 12), 8→9→10→11 (13),
// 14→15→16→17 (18). First-overflow parents point at the leaf that
// holds the cell — after the splits moved cells off page 3, proving
// SQLite re-parents overflow chains during balance (ptrmapPutOvflPtr).
var oracleInsert = ptrmapFixture{
	3:  {storage.PtrmapRootpage, 0},
	4:  {storage.PtrmapOverflow1, 12},
	5:  {storage.PtrmapOverflow2, 4},
	6:  {storage.PtrmapOverflow2, 5},
	7:  {storage.PtrmapOverflow2, 6},
	8:  {storage.PtrmapOverflow1, 13},
	9:  {storage.PtrmapOverflow2, 8},
	10: {storage.PtrmapOverflow2, 9},
	11: {storage.PtrmapOverflow2, 10},
	12: {storage.PtrmapBtree, 3},
	13: {storage.PtrmapBtree, 3},
	14: {storage.PtrmapOverflow1, 18},
	15: {storage.PtrmapOverflow2, 14},
	16: {storage.PtrmapOverflow2, 15},
	17: {storage.PtrmapOverflow2, 16},
	18: {storage.PtrmapBtree, 3},
}

// oracleDelete adds the post-DELETE (pre-vacuum) state: the freed chain of
// row 2 (8-11) and the rebalance-freed leaf 18 carry type FREE (btree.c
// freePage → ptrmapPut(PTRMAP_FREEPAGE)). The balance also moves row 3's
// cell from the freed leaf 18 into survivor 13, so the chain's first
// overflow (14) is re-parented to 13 (ptrmapPutOvflPtr, src/btree.c:8783).
var oracleDelete = func() ptrmapFixture {
	f := ptrmapFixture{}
	for pg, e := range oracleInsert {
		f[pg] = e
	}
	for _, pg := range []uint32{8, 9, 10, 11, 18} {
		f[pg] = struct {
			typ    byte
			parent uint32
		}{storage.PtrmapFreelist, 0}
	}
	f[14] = struct {
		typ    byte
		parent uint32
	}{storage.PtrmapOverflow1, 13}
	return f
}()

// oracleVacuum is the post-DELETE + incremental_vacuum fixture (13 pages).
// Relocation walked the freelist and packed row 3's chain into 8-11 in
// reverse (11 = first overflow, parent 13).
var oracleVacuum = ptrmapFixture{
	3:  {storage.PtrmapRootpage, 0},
	4:  {storage.PtrmapOverflow1, 12},
	5:  {storage.PtrmapOverflow2, 4},
	6:  {storage.PtrmapOverflow2, 5},
	7:  {storage.PtrmapOverflow2, 6},
	8:  {storage.PtrmapOverflow2, 9},
	9:  {storage.PtrmapOverflow2, 10},
	10: {storage.PtrmapOverflow2, 11},
	11: {storage.PtrmapOverflow1, 13},
	12: {storage.PtrmapBtree, 3},
	13: {storage.PtrmapBtree, 3},
}

// readPtrmap parses every ptrmap entry the file records for pages ≥ 3.
func readPtrmap(t *testing.T, path string) (ptrmapFixture, uint32) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	const pageSize = 1024
	if len(data)%pageSize != 0 {
		t.Fatalf("file size %d not a multiple of %d", len(data), pageSize)
	}
	pageCount := uint32(len(data) / pageSize)
	out := ptrmapFixture{}
	for pgno := uint32(3); pgno <= pageCount; pgno++ {
		pp := storage.PtrmapPageNo(pgno, pageSize)
		if pp < 1 || int(pp*pageSize) > len(data) {
			continue
		}
		pageData := data[int((pp-1)*pageSize) : int(pp*pageSize)]
		typ, parent, err := storage.PtrmapEntry(pageData, pgno, pageSize)
		if err != nil {
			continue // pgno is itself a ptrmap page
		}
		if typ == 0 {
			continue
		}
		out[pgno] = struct {
			typ    byte
			parent uint32
		}{typ, parent}
	}
	return out, pageCount
}

func assertPtrmap(t *testing.T, path string, want ptrmapFixture, wantPages uint32, stage string) {
	t.Helper()
	got, pages := readPtrmap(t, path)
	if pages != wantPages {
		t.Fatalf("%s: page_count = %d, want %d", stage, pages, wantPages)
	}
	for pg, w := range want {
		g, ok := got[pg]
		if !ok {
			t.Errorf("%s: page %d has no ptrmap entry (type 0), want type %d parent %d", stage, pg, w.typ, w.parent)
			continue
		}
		if g.typ != w.typ || g.parent != w.parent {
			t.Errorf("%s: page %d ptrmap = (type %d, parent %d), want (type %d, parent %d)", stage, pg, g.typ, g.parent, w.typ, w.parent)
		}
	}
	for pg := range got {
		if _, ok := want[pg]; !ok {
			t.Errorf("%s: unexpected ptrmap entry for page %d: %+v", stage, pg, got[pg])
		}
	}
}

// openPtrmapDB opens a fresh auto_vacuum=INCREMENTAL 1024-byte-page file db
// holding table t(a INTEGER PRIMARY KEY, b BLOB) with the 3 fixture rows.
func openPtrmapDB(t *testing.T, dir string) (*DB, string) {
	t.Helper()
	path := filepath.Join(dir, "ptrmap.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, sql := range []string{
		"PRAGMA page_size=1024",
		"PRAGMA auto_vacuum=INCREMENTAL",
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b BLOB)",
		"INSERT INTO t VALUES(1, '" + strings5000('A') + "')",
		"INSERT INTO t VALUES(2, '" + strings5000('B') + "')",
		"INSERT INTO t VALUES(3, '" + strings5000('C') + "')",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("Exec %q: %v", sql, r.Error)
		}
	}
	return db, path
}

func strings5000(c byte) string {
	b := make([]byte, 5000)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func mustIntegrity(t *testing.T, db *DB, stage string) {
	t.Helper()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("%s: integrity_check: %v", stage, r.Error)
	}
	if len(r.Rows) != 1 || fmt.Sprint(r.Rows[0][0]) != "ok" {
		t.Fatalf("%s: integrity_check = %v, want ok", stage, r.Rows)
	}
}

// TestNativePtrmapAllocOverflowChainFixture pins the allocation-time ptrmap
// contract: every page 3..18 typed, first overflow of each chain parented to
// the leaf holding the cell (post-split), rest chained. Pre-phase15 the
// btree allocates overflow/leaf pages without ptrmap writes → type 0.
func TestNativePtrmapAllocOverflowChainFixture(t *testing.T) {
	dir := t.TempDir()
	db, path := openPtrmapDB(t, dir)
	mustIntegrity(t, db, "post-insert")
	assertPtrmap(t, path, oracleInsert, 18, "post-insert")
}

// TestNativePtrmapFreepageAfterDelete pins freePage's PTRMAP_FREEPAGE write
// (btree.c:6841): pages 8-11 (row 2's chain) and 18 (rebalance-freed leaf)
// must flip to FREE with parent 0 immediately after DELETE, pre-vacuum.
func TestNativePtrmapFreepageAfterDelete(t *testing.T) {
	dir := t.TempDir()
	db, path := openPtrmapDB(t, dir)
	if r := db.Exec("DELETE FROM t WHERE a=2"); r.Error != nil {
		t.Fatalf("DELETE: %v", r.Error)
	}
	mustIntegrity(t, db, "post-delete")
	assertPtrmap(t, path, oracleDelete, 18, "post-delete")
}

// TestNativePtrmapIncrementalVacuumShrink pins the end-to-end incremental
// vacuum contract on the oracle fixture: after DELETE + incremental_vacuum
// the file is exactly 13 pages with the compacted ptrmap table.
func TestNativePtrmapIncrementalVacuumShrink(t *testing.T) {
	dir := t.TempDir()
	db, path := openPtrmapDB(t, dir)
	if r := db.Exec("DELETE FROM t WHERE a=2"); r.Error != nil {
		t.Fatalf("DELETE: %v", r.Error)
	}
	if r := db.Exec("PRAGMA incremental_vacuum"); r.Error != nil {
		t.Fatalf("incremental_vacuum: %v", r.Error)
	}
	mustIntegrity(t, db, "post-vacuum")
	assertPtrmap(t, path, oracleVacuum, 13, "post-vacuum")

	// Remaining rows must survive relocation byte-exact.
	for _, tc := range []struct{ a int64; prefix byte }{{1, 'A'}, {3, 'C'}} {
		q := db.Query(fmt.Sprintf("SELECT length(b), substr(b,1,1), unicode(substr(b,5000,1)) FROM t WHERE a=%d", tc.a))
		if q.Error != nil {
			t.Fatalf("SELECT a=%d: %v", tc.a, q.Error)
		}
		if len(q.Rows) != 1 || q.Rows[0][0] != int64(5000) {
			t.Fatalf("SELECT a=%d rows = %v", tc.a, q.Rows)
		}
	}
}

// TestNativePtrmapRollbackFidelity pins rollback fidelity for ptrmap writes:
// a rolled-back transaction must leave no allocated-page ptrmap residue and
// no corruption (journal must restore the ptrmap page alongside data pages).
func TestNativePtrmapRollbackFidelity(t *testing.T) {
	dir := t.TempDir()
	db, path := openPtrmapDB(t, dir)
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("BEGIN: %v", r.Error)
	}
	for i := 4; i <= 6; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d, '%s')", i, strings5000('D'))); r.Error != nil {
			t.Fatalf("INSERT %d: %v", i, r.Error)
		}
	}
	if r := db.Exec("ROLLBACK"); r.Error != nil {
		t.Fatalf("ROLLBACK: %v", r.Error)
	}
	mustIntegrity(t, db, "post-rollback")
	_, pages := readPtrmap(t, path)
	if pages != 18 {
		t.Fatalf("post-rollback page_count = %d, want 18 (file must shrink back)", pages)
	}
	got, _ := readPtrmap(t, path)
	for pg := range got {
		if _, ok := oracleInsert[pg]; !ok {
			t.Errorf("post-rollback: stale ptrmap entry for page %d: %+v", pg, got[pg])
		}
	}
	for pg := range oracleInsert {
		if _, ok := got[pg]; !ok {
			t.Errorf("post-rollback: missing ptrmap entry for page %d", pg)
		}
	}
}
