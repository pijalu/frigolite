// Native repro for the incrvacuum3 freelist-chain corruption (P8.INCRVACUUM
// phase16). Sequence mirrors test/incrvacuum3.test's 1.x loop: auto_vacuum=2,
// page_size=1024, cache_size=5, a table with a UNIQUE index grown to 256
// rows of randomblob(400), then DELETE rowid%8 and a series of in-transaction
// incremental_vacuums with savepoint rollbacks.
//
// Oracle (sqlite3 3.51.0, same sequence): integrity_check returns "ok" after
// every step; after the final COMMIT: count(*)=128, freelist_count=0.
// The engine currently reports "trunk N leafCount=... exceeds maxLeaves=..."
// / "Freelist: size is X but should be Y" — the pager's parallel in-memory
// freelist maps diverge from the on-disk chain.
package frigolite_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

func TestP8IncrVacuum3OracleSequence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "iv3.db")
	db, err := frigolite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doublings := func(n int) []string {
		sqls := []string{
			"CREATE TABLE t1(x UNIQUE);",
			"INSERT INTO t1 VALUES(randomblob(400));",
			"INSERT INTO t1 VALUES(randomblob(400));",
		}
		for i := 0; i < n; i++ {
			sqls = append(sqls, "INSERT INTO t1 SELECT randomblob(400) FROM t1;")
		}
		return sqls
	}

	setup := []string{
		"PRAGMA cache_size = 5;",
		"PRAGMA page_size = 1024;",
		"PRAGMA auto_vacuum = 2;",
	}
	setup = append(setup, doublings(7)...) // 2 -> 256 rows
	for _, sql := range setup {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}

	checkIntegrity := func(stage string) {
		t.Helper()
		r := db.Query("PRAGMA integrity_check;")
		if r.Error != nil {
			t.Fatalf("%s: integrity_check error: %v", stage, r.Error)
		}
		s := ""
		for _, row := range r.Rows {
			for _, v := range row {
				s += fmt.Sprintf("%v ", v)
			}
		}
		if s != "ok " {
			t.Fatalf("%s: integrity_check = %q, want ok", stage, s)
		}
	}
	queryInt := func(stage, pragma string) int64 {
		t.Helper()
		r := db.Query(pragma)
		if r.Error != nil {
			t.Fatalf("%s: %s error: %v", stage, pragma, r.Error)
		}
		var n int64
		fmt.Sprint(r.Rows[0][0]) // force materialization
		n = r.Rows[0][0].(int64)
		return n
	}

	// Step tn=2: DELETE rowid%8 -> 32 rows remain; freelist fills.
	if r := db.Exec("DELETE FROM t1 WHERE rowid%8;"); r.Error != nil {
		t.Fatalf("DELETE: %v", r.Error)
	}
	checkIntegrity("after DELETE")
	freeAfterDelete := queryInt("after DELETE", "PRAGMA freelist_count;")
	if freeAfterDelete == 0 {
		t.Fatalf("after DELETE: freelist_count = 0, want >0 (oracle: ~348)")
	}

	// Step tn=3: BEGIN; incremental_vacuum=100; doubling x3; ROLLBACK.
	// C steps the vacuum in-transaction and ROLLBACK restores the freelist.
	for _, sql := range []string{
		"BEGIN;",
		"PRAGMA incremental_vacuum = 100;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"ROLLBACK;",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("tn3 %s: %v", sql, r.Error)
		}
	}
	checkIntegrity("after tn3 ROLLBACK")
	if got := queryInt("after tn3 ROLLBACK", "SELECT count(*) FROM t1"); got != 32 {
		t.Fatalf("after tn3 ROLLBACK: count(*) = %d, want 32", got)
	}
	if got := queryInt("after tn3 ROLLBACK", "PRAGMA freelist_count;"); got != freeAfterDelete {
		t.Fatalf("after tn3 ROLLBACK: freelist_count = %d, want %d (rollback restores freelist)", got, freeAfterDelete)
	}

	// Steps tn=4..7: savepoint-wrapped vacuum + inserts, rolled back.
	for _, sql := range []string{
		"BEGIN;",
		"SAVEPOINT one;",
		"PRAGMA incremental_vacuum = 100;",
		"SAVEPOINT two;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"ROLLBACK TO two;",
		"ROLLBACK TO one;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"PRAGMA incremental_vacuum = 1000;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"ROLLBACK;",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("tn4-7 %s: %v", sql, r.Error)
		}
	}
	checkIntegrity("after tn4-7 ROLLBACK")

	// Step tn=8: BEGIN; doubling; vacuum; doubling; COMMIT -> full drain.
	for _, sql := range []string{
		"BEGIN;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"PRAGMA incremental_vacuum = 1000;",
		"INSERT INTO t1 SELECT randomblob(400) FROM t1;",
		"COMMIT;",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("tn8 %s: %v", sql, r.Error)
		}
	}
	checkIntegrity("after tn8 COMMIT")
	if got := queryInt("after tn8 COMMIT", "SELECT count(*) FROM t1"); got != 128 {
		t.Fatalf("after tn8 COMMIT: count(*) = %d, want 128 (oracle)", got)
	}
	if got := queryInt("after tn8 COMMIT", "PRAGMA freelist_count;"); got != 0 {
		t.Fatalf("after tn8 COMMIT: freelist_count = %d, want 0 (oracle: full drain)", got)
	}

	// Sanity: file exists and is non-trivial.
	if fi, err := os.Stat(dbPath); err != nil || fi.Size() < 4096 {
		t.Fatalf("db file missing or too small: %v %v", fi, err)
	}
}
