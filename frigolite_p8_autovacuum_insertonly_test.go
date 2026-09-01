// Pure-Go native test for the autovacuum btree rebalance + chain-pop
// + relocatePage fixes (P8.INCRVACUUM). This test is the oracle-verified
// native contract for the autovacuum.test package.
//
// Mirrors autovacuum_test.go lines 118-185: PRAGMA auto_vacuum=1,
// create table + index, insert 20 rows of large strings, per-row
// DELETE, SELECT a FROM av1 ORDER BY rowid, PRAGMA integrity_check.
package frigolite_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestP8AutovacuumInsertOnlyIntegrity(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if err := db.Exec("CREATE TABLE av1(a)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec("CREATE INDEX av1_idx ON av1(a)").Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := 1; i <= 20; i++ {
		val := strings.Repeat(itoa(i)+".", 3500)
		val = val[:3500]
		if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + val + "')").Error; err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	r := db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check after inserts: %v", r.Error)
	}
	if len(r.Rows) == 0 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after inserts FAIL: %v", r.Rows)
	}
	// Delete rows one at a time.
	for del := 1; del <= 20; del++ {
		if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoa(del)).Error; err != nil {
			t.Fatalf("delete %d: %v", del, err)
		}
		pc := db.Query("PRAGMA page_count")
		fc := db.Query("PRAGMA freelist_count")
		t.Logf("after delete %d: pc=%v fl=%v", del, pc.Rows, fc.Rows)
		r = db.Query("PRAGMA integrity_check")
		if r.Error != nil {
			t.Fatalf("integrity_check after delete %d: %v", del, r.Error)
		}
		if len(r.Rows) == 0 {
			t.Fatalf("integrity_check after delete %d returned no rows", del)
		}
		if r.Rows[0][0] != "ok" {
			t.Fatalf("integrity_check after delete %d FAIL: %v", del, r.Rows)
		}
	}
	// Now try to re-insert
	for i := 1; i <= 5; i++ {
		val := strings.Repeat(itoa(i)+".", 3500)
		val = val[:3500]
		if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + val + "')").Error; err != nil {
			t.Fatalf("re-insert %d: %v", i, err)
		}
	}
	r = db.Query("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check after re-inserts: %v", r.Error)
	}
	if r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check after re-inserts FAIL: %v", r.Rows)
	}
}

// TestP8AutovacuumTwoIterations reproduces the autovacuum-1.2 failure
// pattern: run the autovacuum-1.1 scenario once (20 inserts, 20 deletes),
// then run it a second time (20 inserts, 20 deletes) on the same DB and
// connection. The autovacuum-1.1 case passes; the autovacuum-1.2 case
// fails with "database disk image is malformed" on the very first delete
// of the second iteration — indicating the freelist chain is corrupted
// at the end of iteration 1 in a way the second iteration trips over.
func TestP8AutovacuumTwoIterations(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := db.Exec("PRAGMA auto_vacuum = 1").Error; err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if err := db.Exec("CREATE TABLE av1(a)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Exec("CREATE INDEX av1_idx ON av1(a)").Error; err != nil {
		t.Fatalf("create index: %v", err)
	}
	for iter := 1; iter <= 2; iter++ {
		for i := 1; i <= 20; i++ {
			val := strings.Repeat(itoa(i)+".", 3500)
			val = val[:3500]
			if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + val + "')").Error; err != nil {
				t.Fatalf("iter %d insert %d: %v", iter, i, err)
			}
		}
		ic := db.Query("PRAGMA integrity_check")
		if ic.Error != nil {
			t.Fatalf("iter %d integrity_check after inserts: %v", iter, ic.Error)
		}
		if ic.Rows[0][0] != "ok" {
			t.Fatalf("iter %d integrity_check after inserts FAIL: %v", iter, ic.Rows)
		}
		pc0 := db.Query("PRAGMA page_count").Rows
		fc0 := db.Query("PRAGMA freelist_count").Rows
		t.Logf("iter %d before deletes: pc=%v fl=%v", iter, pc0, fc0)
		for del := 1; del <= 20; del++ {
			if err := db.Exec("DELETE FROM av1 WHERE oid = " + itoa(del)).Error; err != nil {
				t.Fatalf("iter %d delete %d: %v", iter, del, err)
			}
			pc := db.Query("PRAGMA page_count")
			fc := db.Query("PRAGMA freelist_count")
			t.Logf("iter %d after delete %d: pc=%v fl=%v", iter, del, pc.Rows, fc.Rows)
			ic := db.Query("PRAGMA integrity_check")
			if ic.Error != nil {
				t.Fatalf("iter %d integrity_check after delete %d: %v", iter, del, ic.Error)
			}
			if ic.Rows[0][0] != "ok" {
				t.Fatalf("iter %d delete %d integrity_check FAIL: %v", iter, del, ic.Rows)
			}
		}
	}
}

// TestP8AutovacuumIter1SelectAfter20Deletes reproduces the
// autovacuum-1.1.(20).3 failure: 20 inserts, 20 deletes, then
// SELECT a FROM av1 should return 0 rows. The test fails if the
// engine leaves a phantom row in the table.
func TestP8AutovacuumIter1SelectAfter20Deletes(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Match the testgen setup precisely: single multi-statement query
	// for auto_vacuum + CREATE TABLE + CREATE INDEX.
	r := db.Query(`
     PRAGMA auto_vacuum = 1;
     CREATE TABLE av1(a);
     CREATE INDEX av1_idx ON av1(a);
   `)
	if r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}
	ic := db.Query(`
         pragma integrity_check
       `)
	if ic.Error != nil || ic.Rows[0][0] != "ok" {
		t.Fatalf("setup integrity_check: %v %v", ic.Error, ic.Rows)
	}
	for i := 1; i <= 20; i++ {
		val := strings.Repeat(itoa(i)+".", 3500)[:3500]
		if err := db.Exec("INSERT INTO av1 (oid, a) VALUES(" + itoa(i) + ", '" + val + "')").Error; err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	for del := 1; del <= 20; del++ {
		if err := db.Exec("\n        DELETE FROM av1 WHERE oid = " + itoa(del) + "\n      ").Error; err != nil {
			t.Fatalf("delete %d: %v", del, err)
		}
		ic := db.Query("\n          pragma integrity_check\n        ")
		if ic.Error != nil || ic.Rows[0][0] != "ok" {
			t.Logf("integrity_check after del %d: %v %v", del, ic.Error, ic.Rows)
		}
		pc := db.Query("PRAGMA page_count").Rows
		fc := db.Query("PRAGMA freelist_count").Rows
		t.Logf("after del %d: pc=%v fl=%v", del, pc, fc)
		r := db.Query("\n        select a from av1 order by rowid\n      ")
		if r.Error != nil {
			t.Fatalf("select after del %d: %v", del, r.Error)
		}
		if len(r.Rows) != 20-del {
			t.Fatalf("after del %d: expected %d rows, got %d (rows=%v)", del, 20-del, len(r.Rows), r.Rows)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var s string
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
