// R13-L7 pin: index b-trees written by frigolite must be structurally
// sqlite-compatible — every index key lives exactly once in the tree, either on
// a leaf or on an interior page (sqlite's balance_nonroot pushes one real cell
// per split boundary into the parent, src/btree.c:8791-8849). frigolite used to
// keep every key on a leaf and repeat one key per split as an interior "divider
// copy", which made sqlite readers over-count (PRAGMA integrity_check reported
// "wrong # of entries in index i1" and sqlite's index-driven count(*) returned
// 5049 for a 5000-row table).
//
// The assertions are pure Go (page headers only); the sqlite3 CLI check is an
// extra oracle when the binary is on PATH.
package frigolite

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/storage"
)

// pinIndexTreeCounters is one index tree's page census under sqlite's entry
// rule: every leaf page and every INTERIOR INDEX page contributes its cell
// count (checkTreePage: `if( pPage->leaf || pPage->intKey==0 ) nRow += nCell`,
// src/btree.c:10892); interior table pages contribute nothing.
type pinIndexTreeCounters struct {
	entries     int // sqlite's entry count: leaf cells + interior index cells
	leafCells   int
	interior    int // interior index cell total
	interiorPgs int
	leafPgs     int
}

// pinWalkIndexTree census-walks an index b-tree from its root page.
func pinWalkIndexTree(t *testing.T, pg *pager.Pager, root uint32) pinIndexTreeCounters {
	t.Helper()
	var c pinIndexTreeCounters
	var walk func(pageNum uint32, depth int)
	seen := map[uint32]bool{}
	walk = func(pageNum uint32, depth int) {
		if depth > 32 {
			t.Fatalf("index tree too deep from root %d", root)
		}
		if seen[pageNum] {
			t.Fatalf("index page %d visited twice (child cycle)", pageNum)
		}
		seen[pageNum] = true
		p, err := pg.ReadPage(pageNum)
		if err != nil {
			t.Fatalf("read index page %d: %v", pageNum, err)
		}
		coff := 100
		if pageNum != 1 {
			coff = 0
		}
		page, err := storage.ParsePage(p.Data, int(pg.PageSize()), coff)
		if err != nil {
			t.Fatalf("parse index page %d: %v", pageNum, err)
		}
		// CellPointer adds 8 to its offset argument: leaf cell pointers start at
		// coff+8, interior ones at coff+12 (4-byte rightmost pointer first).
		ptrBase := coff
		if page.PageType == storage.PageTypeInteriorIndex || page.PageType == storage.PageTypeInteriorTable {
			ptrBase = coff + 4
		}
		switch page.PageType {
		case storage.PageTypeLeafIndex, storage.PageTypeLeafTable:
			c.leafPgs++
			c.leafCells += int(page.CellCount)
			c.entries += int(page.CellCount)
			return
		case storage.PageTypeInteriorIndex:
			c.interiorPgs++
			c.interior += int(page.CellCount)
			c.entries += int(page.CellCount)
		case storage.PageTypeInteriorTable:
			// Interior table cells are rowid separators, not entries.
		default:
			t.Fatalf("index page %d: unexpected type 0x%02x", pageNum, page.PageType)
		}
		for i := 0; i < int(page.CellCount); i++ {
			off := int(storage.CellPointer(p.Data, ptrBase, i, int(pg.PageSize())))
			if off < 0 || off+4 > len(p.Data) {
				t.Fatalf("index page %d cell %d: child pointer out of range", pageNum, i)
			}
			walk(binary.BigEndian.Uint32(p.Data[off:off+4]), depth+1)
		}
		if page.RightmostPtr != 0 {
			walk(page.RightmostPtr, depth+1)
		}
	}
	walk(root, 0)
	return c
}

// TestPinIndexInteriorLayout asserts R13-L7: a multi-page index written by
// frigolite holds exactly one cell per index entry across leaf AND interior
// pages, so sqlite's entry rule (leaf cells + interior index cells) equals the
// row count, and sqlite3's PRAGMA integrity_check accepts the file.
func TestPinIndexInteriorLayout(t *testing.T) {
	const rows = 3000
	dir := t.TempDir()
	path := filepath.Join(dir, "pin.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, b INTEGER)"); r.Error != nil {
		t.Fatalf("create table: %v", r.Error)
	}
	if r := db.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("begin: %v", r.Error)
	}
	for i := 0; i < rows; i++ {
		if r := db.Exec(fmt.Sprintf("INSERT INTO t VALUES(%d,%d)", i+1, (i*7)%1000)); r.Error != nil {
			t.Fatalf("insert %d: %v", i, r.Error)
		}
	}
	if r := db.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit: %v", r.Error)
	}
	if r := db.Exec("CREATE INDEX i1 ON t(b)"); r.Error != nil {
		t.Fatalf("create index: %v", r.Error)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Pure-Go layout assertions over the written file.
	pg, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("pager open: %v", err)
	}
	defer pg.Close()
	mgr := schema.NewManager(pg)
	if err := mgr.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	entry, err := mgr.FindIndex("i1")
	if err != nil || entry == nil {
		t.Fatalf("find index i1: %v", err)
	}
	c := pinWalkIndexTree(t, pg, entry.RootPage)
	if c.interiorPgs == 0 || c.interior == 0 {
		t.Fatalf("index i1 is not multi-page (%d leaves, %d interior pages, %d interior cells)",
			c.leafPgs, c.interiorPgs, c.interior)
	}
	// The layout property: sqlite's entry rule must equal the row count. A
	// divider COPY makes this rows+interior (the defect: 5049 for 5000 rows).
	if c.entries != rows {
		t.Errorf("index i1 holds %d entries under sqlite's rule (leaf %d + interior %d)"+
			" for %d rows: interior cells must be real entries, not divider copies",
			c.entries, c.leafCells, c.interior, rows)
	}

	// Oracle: sqlite must accept the file and count the rows through the index.
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skipf("sqlite3 not on PATH; pure-Go layout assertions passed (entries=%d, interior=%d)",
			c.entries, c.interior)
	}
	assertSQLiteOK(t, sqlite3, path, rows)

	// Deleting entries that live on INTERIOR pages exercises sqlite's
	// predecessor move (sqlite3BtreeDelete, src/btree.c:9877-9944): the tree must
	// stay count-consistent and sqlite must still accept the file.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if r := db2.Exec("BEGIN"); r.Error != nil {
		t.Fatalf("begin delete: %v", r.Error)
	}
	for x := 0; x < 1000; x += 3 {
		if r := db2.Exec(fmt.Sprintf("DELETE FROM t WHERE b=%d", x)); r.Error != nil {
			t.Fatalf("delete b=%d: %v", x, r.Error)
		}
	}
	if r := db2.Exec("COMMIT"); r.Error != nil {
		t.Fatalf("commit delete: %v", r.Error)
	}
	var after int
	if r := db2.Query("SELECT count(*) FROM t"); r.Error != nil {
		t.Fatalf("count after delete: %v", r.Error)
	} else if len(r.Rows) != 1 {
		t.Fatalf("count after delete: %d rows", len(r.Rows))
	} else if v, ok := r.Rows[0][0].(int64); ok {
		after = int(v)
	} else {
		t.Fatalf("count after delete: unexpected type %T", r.Rows[0][0])
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close after delete: %v", err)
	}
	if after <= 0 || after >= rows {
		t.Fatalf("after deleting every 3rd row: count=%d (want 0 < n < %d)", after, rows)
	}
	pg2, err := pager.Open(path, pager.DefaultPageSize)
	if err != nil {
		t.Fatalf("pager reopen: %v", err)
	}
	defer pg2.Close()
	mgr2 := schema.NewManager(pg2)
	if err := mgr2.Init(); err != nil {
		t.Fatalf("schema reinit: %v", err)
	}
	entry2, err := mgr2.FindIndex("i1")
	if err != nil || entry2 == nil {
		t.Fatalf("refind index i1: %v", err)
	}
	c2 := pinWalkIndexTree(t, pg2, entry2.RootPage)
	if c2.entries != after {
		t.Errorf("after deletes the index holds %d entries under sqlite's rule"+
			" (leaf %d + interior %d) but the table has %d rows",
			c2.entries, c2.leafCells, c2.interior, after)
	}
	assertSQLiteOK(t, sqlite3, path, after)
	_ = os.Remove(path)
}

// assertSQLiteOK runs sqlite3's PRAGMA integrity_check and its index-driven
// count over the file.
func assertSQLiteOK(t *testing.T, sqlite3, path string, want int) {
	t.Helper()
	out, err := exec.Command(sqlite3, path, "PRAGMA integrity_check;").CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 integrity_check: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "ok" {
		t.Errorf("sqlite3 PRAGMA integrity_check = %q, want \"ok\"", got)
	}
	out, err = exec.Command(sqlite3, path, "SELECT count(*) FROM t;").CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 count(*): %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != fmt.Sprint(want) {
		t.Errorf("sqlite3 SELECT count(*) FROM t = %q, want %d (index-driven count)", got, want)
	}
}
