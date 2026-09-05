package frigolite_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// The PRAGMA max_page_count contract pinned here is oracle-verified against
// /usr/bin/sqlite3 3.51.0 (§5f UCL): the default cap is the documented
// SQLITE_MAX_PAGE_COUNT default 1073741823; the setter echoes the clamped
// value (never below the current page count, pragma.c OP_MaxPgcnt's
// MAX(sqlite3BtreeLastPage(pBt), N)); a non-numeric or non-positive value
// leaves the cap unchanged (the pragma behaves as a getter); and an INSERT
// that needs a page beyond the cap fails with "database or disk is full"
// (pager.c getPageNo → SQLITE_FULL).

func TestNativeMaxPageCount_DefaultIsDocumentedLimit(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "mp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Query("PRAGMA max_page_count;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(1073741823) {
		t.Fatalf("default max_page_count: want [1073741823], got %v", r.Rows)
	}
}

func TestNativeMaxPageCount_SetterEchoesClampedValue(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "mp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA page_size=1024;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("CREATE TABLE t1(x);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Setter echoes the new cap.
	r := db.Query("PRAGMA max_page_count=5;")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(5) {
		t.Fatalf("setter echo: want [5], got %v (err=%v)", r.Rows, r.Error)
	}
	// Getter reports the cap.
	r = db.Query("PRAGMA max_page_count;")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(5) {
		t.Fatalf("getter: want [5], got %v (err=%v)", r.Rows, r.Error)
	}
}

func TestNativeMaxPageCount_NeverBelowCurrentPageCount(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "mp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA page_size=1024;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Two pages of payload: schema page + table page + overflow pages.
	if r := db.Exec("CREATE TABLE t1(x); INSERT INTO t1 VALUES(zeroblob(5000));"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// A cap below the current page count clamps up to the current count
	// (OP_MaxPgcnt: newMax = MAX(sqlite3BtreeLastPage(pBt), N)).
	r := db.Query("PRAGMA max_page_count=2;")
	if r.Error != nil {
		t.Fatalf("query error: %v", r.Error)
	}
	got := r.Rows[0][0].(int64)
	if got <= 1 {
		t.Fatalf("cap clamped to %d; want the current page count (> 1)", got)
	}
	// The same value again: the cap already equals the page count.
	r = db.Query("PRAGMA max_page_count;")
	if r.Rows[0][0] != got {
		t.Fatalf("getter %v != setter echo %v", r.Rows[0][0], got)
	}
}

func TestNativeMaxPageCount_InsertPastCapIsFull(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "mp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA page_size=1024;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("CREATE TABLE t1(x);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Cap at the current page count so the next payload page must fail.
	r := db.Query("PRAGMA page_count;")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("page_count: %v", r.Error)
	}
	cur := r.Rows[0][0].(int64)
	if r := db.Query("PRAGMA max_page_count=" + strconv.FormatInt(cur, 10)); r.Error != nil {
		t.Fatal(r.Error)
	}
	res := db.Exec("INSERT INTO t1 VALUES(zeroblob(2000))")
	if res.Error == nil || !strings.Contains(res.Error.Error(), "database or disk is full") {
		t.Fatalf("INSERT past cap: want 'database or disk is full', got %v", res.Error)
	}
}

func TestNativeMaxPageCount_NonPositiveOrJunkIsGetter(t *testing.T) {
	db, err := frigolite.Open(filepath.Join(t.TempDir(), "mp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("PRAGMA max_page_count=10;"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, v := range []string{"0", "-5", "'abc'"} {
		r := db.Query("PRAGMA max_page_count=" + v + ";")
		if r.Error != nil {
			t.Fatalf("max_page_count=%s: query error: %v", v, r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != int64(10) {
			t.Fatalf("max_page_count=%s: want [10] (unchanged), got %v", v, r.Rows)
		}
	}
}
