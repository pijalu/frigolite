package frigolite

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Btree churn pin tests: overflow-cell delete/insert churn at page_size
// 1024 must leave a file that passes PRAGMA integrity_check (engine AND
// oracle). This pins the T24-discovered bedrock bug behind the fts4merge4
// level-1-drain blocker (commit 832176d96):
//
//   - every cell clear must return its overflow-page chain to the freelist
//     (btree.c clearCell -> freePageChain): the rowid-overwrite path
//     (deleteCellOnPage, driven by UPDATE/OR REPLACE) leaked whole chains,
//     surfacing as "Page N is never used" in the oracle's integrity_check;
//   - page layouts must pack cells from the usable end with exact
//     free-space accounting (defragmentPage parity): the engine used to
//     reserve a phantom 4-byte page-end trailer and leave unaccounted dead
//     space after interior-cell removal, surfacing as "free space
//     corruption" / "Fragmentation of N bytes reported as 0".
//
// Rows use EXPLICIT rowids so the engine-visible rowid set is deterministic;
// scans are compared as full sets, and the oracle cross-checks the file the
// engine wrote.

func churnScanIDs(t *testing.T, db *DB) []string {
	t.Helper()
	res := db.Query("SELECT id FROM t ORDER BY id")
	if res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, fmt.Sprintf("%v", row[0]))
	}
	return out
}

func churnIntegrity(t *testing.T, db *DB, label string) {
	t.Helper()
	res := db.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatalf("%s: integrity_check: %v", label, res.Error)
	}
	lines := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		lines = append(lines, fmt.Sprintf("%v", row[0]))
	}
	if len(lines) != 1 || lines[0] != "ok" {
		t.Fatalf("%s: engine integrity_check:\n%s", label, strings.Join(lines, "\n"))
	}
}

func churnOracleIntegrity(t *testing.T, dbPath string) string {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skipf("sqlite3 CLI not on PATH: %v", err)
	}
	cmd := exec.Command(bin, dbPath, "PRAGMA integrity_check")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle integrity_check run: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func churnCheckSet(t *testing.T, db *DB, alive map[int]bool, label string) {
	t.Helper()
	got := churnScanIDs(t, db)
	want := make([]string, 0, len(alive))
	for id := range alive {
		want = append(want, fmt.Sprint(id))
	}
	sortChurnIDs(want)
	if len(got) != len(want) {
		t.Fatalf("%s: scan n=%d want %d\ngot=%v\nwant=%v", label, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: scan rowset diverged\ngot=%v\nwant=%v", label, got, want)
		}
	}
}

func sortChurnIDs(s []string) {
	// Numeric sort (ids are small; string compare would misorder 9 vs 11).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			a, b := len(s[j-1]), len(s[j])
			if a < b || (a == b && s[j-1] <= s[j]) {
				break
			}
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func churnExec(t *testing.T, db *DB, q string) {
	t.Helper()
	if res := db.Exec(q); res.Error != nil {
		t.Fatalf("exec %q: %v", q, res.Error)
	}
}

func churnOpen(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func churnHexBlob(rng *rand.Rand, n int) string {
	blob := make([]byte, n)
	for j := range blob {
		blob[j] = byte(rng.Intn(256))
	}
	return strings.ToUpper(hex.EncodeToString(blob))
}

// TestBtreeOverflowChurnIntegrity drives INSERT/DELETE churn with
// overflow-sized cells (multi-level table btrees after round 1) and
// requires a clean integrity_check from the engine after every round, from
// a fresh connection on the closed file, and from the oracle.
func TestBtreeOverflowChurnIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("slow churn")
	}
	dbPath := filepath.Join(t.TempDir(), "churn.db")
	db := churnOpen(t, dbPath)
	churnExec(t, db, "PRAGMA page_size=1024")
	churnExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)")

	rng := rand.New(rand.NewSource(42))
	alive := map[int]bool{}
	nextID := 1
	rounds := 10
	for round := 0; round < rounds; round++ {
		// Insert 40 fresh rows of overflow-sized blobs (1.5KB-3.5KB: past
		// maxLocal(989) at usable 1024, so each cell owns an overflow chain).
		for i := 0; i < 40; i++ {
			id := nextID
			nextID++
			churnExec(t, db, fmt.Sprintf("INSERT INTO t(id,b) VALUES(%d,x'%s')", id, churnHexBlob(rng, 1500+rng.Intn(2000))))
			alive[id] = true
		}
		// Delete every other live row.
		del := 0
		for id := 1; id <= nextID; id++ {
			if !alive[id] || id%2 != round%2 {
				continue
			}
			churnExec(t, db, fmt.Sprintf("DELETE FROM t WHERE id=%d", id))
			delete(alive, id)
			del++
		}
		churnCheckSet(t, db, alive, fmt.Sprintf("round %d (deleted %d)", round, del))
		churnIntegrity(t, db, fmt.Sprintf("round %d", round))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Fresh connection on the closed file: the engine must re-validate it.
	db2 := churnOpen(t, dbPath)
	defer db2.Close()
	churnCheckSet(t, db2, alive, "reopened file")
	churnIntegrity(t, db2, "reopened file")

	// Oracle ground truth on the engine-written file.
	if got := churnOracleIntegrity(t, dbPath); got != "ok" {
		t.Fatalf("oracle integrity_check on engine file:\n%s", got)
	}
}

// TestBtreeOverflowReplaceChurnIntegrity drives the same-rowid overwrite
// path (UPDATE + INSERT OR REPLACE on rows whose cells overflow): the old
// cell is cleared through deleteCellOnPage, which must free the replaced
// cell's overflow chain (btree.c clearCell) — leaking it orphans pages the
// oracle reports as "Page N is never used" and corrupts freelist accounting.
func TestBtreeOverflowReplaceChurnIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("slow churn")
	}
	dbPath := filepath.Join(t.TempDir(), "replace.db")
	db := churnOpen(t, dbPath)
	churnExec(t, db, "PRAGMA page_size=1024")
	churnExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB)")

	rng := rand.New(rand.NewSource(7))
	liveIDs := make([]int, 0, 60)
	for i := 1; i <= 60; i++ {
		churnExec(t, db, fmt.Sprintf("INSERT INTO t(id,b) VALUES(%d,x'%s')", i, churnHexBlob(rng, 1500+rng.Intn(2000))))
		liveIDs = append(liveIDs, i)
	}
	for round := 0; round < 8; round++ {
		for _, id := range liveIDs {
			if (id+round)%3 == 0 {
				// UPDATE path: same-rowid cell overwrite.
				churnExec(t, db, fmt.Sprintf("UPDATE t SET b=x'%s' WHERE id=%d", churnHexBlob(rng, 1200+rng.Intn(2400)), id))
			} else if (id+round)%3 == 1 {
				// OR REPLACE path: same-rowid cell overwrite.
				churnExec(t, db, fmt.Sprintf("INSERT OR REPLACE INTO t(id,b) VALUES(%d,x'%s')", id, churnHexBlob(rng, 1200+rng.Intn(2400))))
			}
		}
		want := map[int]bool{}
		for _, id := range liveIDs {
			want[id] = true
		}
		churnCheckSet(t, db, want, fmt.Sprintf("replace round %d", round))
		churnIntegrity(t, db, fmt.Sprintf("replace round %d", round))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2 := churnOpen(t, dbPath)
	defer db2.Close()
	churnIntegrity(t, db2, "reopened replace file")
	if got := churnOracleIntegrity(t, dbPath); got != "ok" {
		t.Fatalf("oracle integrity_check on engine file:\n%s", got)
	}
}
