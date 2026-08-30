// TestCorrupt2CheckTreePage is the native regression test for corrupt2-5.1:
// it crafts a database where two b-trees both point to the same leaf page
// and asserts the engine's PRAGMA integrity_check emits
// "Tree 2 page 2 cell 0: 2nd reference to page 10" and
// "Page 4: never used".
//
// The DB is built using the sqlite3 CLI (ground truth) into a TempDir,
// then the test mutates the on-disk bytes exactly as the TCL test does
// (read t1's first cell's child-page bytes, write them over t2's first
// cell's child-page bytes) and asks frigolite to validate the result.
//
// If sqlite3 is not on PATH the test is skipped.
package frigolite

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorrupt2CheckTreePage(t *testing.T) {
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skipf("sqlite3 CLI not on PATH: %v", err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	corruptPath := filepath.Join(dir, "corrupt.db")

	// Step 1: build a real SQLite DB with the same setup as corrupt2-5.1.
	setupSQL := `
PRAGMA auto_vacuum = 0;
PRAGMA page_size = 1024;
CREATE TABLE t1(a, b, c);
CREATE TABLE t2(a, b, c);
INSERT INTO t2 VALUES(randomblob(100), randomblob(100), randomblob(100));
INSERT INTO t2 SELECT * FROM t2;
INSERT INTO t2 SELECT * FROM t2;
INSERT INTO t2 SELECT * FROM t2;
INSERT INTO t2 SELECT * FROM t2;
INSERT INTO t1 SELECT * FROM t2;
`
	cmd := exec.Command(sqlite3, dbPath)
	cmd.Stdin = strings.NewReader(setupSQL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 setup: %v\n%s", err, out)
	}

	// Step 2: copy the DB to corrupt.db and apply the same corruption
	// pattern as corrupt2.test 5.1: read t1's first cell's child-page
	// bytes from page 2 and write them over t2's first cell on page 3.
	orig, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), orig...)

	// t1 root is page 2; t2 root is page 3 (sqlite3's allocator for
	// 1024-byte pages with the test setup puts the first two tables
	// consecutively on disk).
	cellPtrT1 := binary.BigEndian.Uint16(corrupt[1024+12 : 1024+14])
	childPageBytes := corrupt[1024+int(cellPtrT1) : 1024+int(cellPtrT1)+4]
	cellPtrT2 := binary.BigEndian.Uint16(corrupt[2*1024+12 : 2*1024+14])
	dst := 2*1024 + int(cellPtrT2)
	copy(corrupt[dst:dst+4], childPageBytes)
	if err := os.WriteFile(corruptPath, corrupt, 0644); err != nil {
		t.Fatal(err)
	}

	// Step 3: open with frigolite and run PRAGMA integrity_check.
	// frigolite's reader does not need a btree it created — it reads
	// the file in standard SQLite format. The engine's checkTreePage
	// (internal/exec/pragma_quickcheck.go) walks the b-trees and
	// detects the duplicate child pointer + orphaned page.
	db, err := Open(corruptPath)
	if err != nil {
		t.Fatalf("frigolite.Open: %v", err)
	}
	defer db.Close()
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check error: %v", r.Error)
	}
	if len(r.Rows) != 3 {
		t.Fatalf("integrity_check: got %d rows, want 3\nrows=%v", len(r.Rows), r.Rows)
	}
	got := flatten(r)
	want := "*** in database main ***\n" +
		"Tree 2 page 2 cell 0: 2nd reference to page 10\n" +
		"Page 4: never used"
	if got != want {
		t.Errorf("integrity_check mismatch\n  got:  [%s]\n  want: [%s]", got, want)
	}
}

// flatten concatenates all first-column string values with newlines
// (matching how TCL's `tclListFlatten` collapses the testgen result
// rows into a single comparable string).
func flatten(r *Result) string {
	if len(r.Rows) == 0 {
		return ""
	}
	var buf bytes.Buffer
	for i, row := range r.Rows {
		if i > 0 {
			buf.WriteByte('\n')
		}
		if s, ok := row[0].(string); ok {
			buf.WriteString(s)
		} else {
			buf.WriteString("?")
		}
	}
	return buf.String()
}
