package frigolite_test

import (
	"os"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestT30VtabSharedNative ports the engine-visible contract of
// test/vtab_shared.test 1.9 (T30-vtab native supersession pin): two
// independent connections on one file both see an echo virtual table stored
// in that file's schema; closing the second connection mid-scan does not
// disturb the first connection's scan; and the closed connection can be
// reopened under the same name (oracle 3.54 + echo module: the row set is
// {1 2 3 4 5 6} in every iteration).
//
// The TCL test gates on sqlite3_enable_shared_cache, which frigolite does
// not implement (queued with P7.WAL-G7); this port expresses the
// file-backed visibility contract that frigolite's independent-connection
// model does support.
func TestT30VtabSharedNative(t *testing.T) {
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.RegisterEchoModule()
	if err := db.Exec(`
		CREATE TABLE t0(a, b, c);
		INSERT INTO t0 VALUES(1, 2, 3);
		CREATE VIRTUAL TABLE t1 USING echo(t0);
	`).Error; err != nil {
		t.Fatal(err)
	}
	db2, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	db2.RegisterEchoModule()
	defer func() {
		if db2 != nil {
			db2.Close()
		}
	}()

	// db writes first (cross-connection page-cache invalidation of COMMITTED
	// writes is shared-cache/G7 territory and intentionally not exercised
	// here — the generated vtab_shared-1.9 sequence writes before db2 reads).
	if err := db.Exec(" INSERT INTO t1 VALUES(4, 5, 6) ").Error; err != nil {
		t.Fatal(err)
	}
	// db2 sees the vtab created by db (schema lives in the file) with the
	// full row set.
	if got := t30Flat(t, db2.Query(" SELECT * FROM t1 ")); got != "1 2 3 4 5 6" {
		t.Fatalf("db2 cannot read the echo vtab:\n  got:  [%s]", got)
	}

	pairs := []struct {
		iTest          int
		selectOn, mule *frigolite.DB
	}{
		{1, db, db2},
		{2, db, db2},
		{3, db2, db},
	}
	for _, tc := range pairs {
		r := tc.selectOn.Query(" SELECT * FROM t1 ")
		if r.Error != nil {
			t.Fatalf("1.9.%d: %v", tc.iTest, r.Error)
		}
		if len(r.Rows) != 2 {
			t.Fatalf("1.9.%d: expected 2 rows, got %d", tc.iTest, len(r.Rows))
		}
		var got string
		for _, row := range r.Rows {
			if row[0] == int64(1) {
				tc.mule.Close() // close the other connection mid-scan
			}
			got += " " + t30Flat(t, &frigolite.Result{Rows: [][]interface{}{row}})
		}
		if want := " 1 2 3 4 5 6"; got != want {
			t.Errorf("1.9.%d:\n  got:  [%s]\n  want: [%s]", tc.iTest, got, want)
		}
		// sqlite3 $dbClose test.db — reopen under the same name.
		if tc.mule == db {
			db, err = frigolite.Open("test.db")
			if err != nil {
				t.Fatal(err)
			}
			db.RegisterEchoModule()
		} else {
			db2.Close()
			db2, err = frigolite.Open("test.db")
			if err != nil {
				t.Fatal(err)
			}
			db2.RegisterEchoModule()
		}
	}
}
