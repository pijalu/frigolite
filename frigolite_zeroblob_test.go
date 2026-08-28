package frigolite

import (
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// TestZeroblobMaxBlobsize verifies the SQLite-faithful
// sqlite3_max_blobsize instrumentation (src/vdbe.c UPDATE_MAX_BLOBSIZE on
// OP_MakeRecord): the recorded size is nHdr+nData with trailing zeroblobs
// left as an unmaterialized zero tail, while a zeroblob followed by real
// data is expanded and counted.
func TestZeroblobMaxBlobsize(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "zb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.Exec("CREATE TABLE t1(a,b,c,d)").Error != nil {
		t.Fatal(db.Exec("CREATE TABLE t1(a,b,c,d)").Error)
	}

	cases := []struct {
		name string
		sql  string
		want int
	}{
		// (2,3,4,zeroblob(1000000)): hdr 7 (1 size + 3 int serials + 3-byte
		// blob serial varint) + data 3; trailing zeroblob unexpanded.
		{"trailing-1e6", "INSERT INTO t1 VALUES(2,3,4,zeroblob(1000000))", 10},
		// (3,4,zeroblob(10000),5): non-null column after the zeroblob
		// forces expansion: hdr 7 + data (1+1+10000+1) = 10010.
		{"mid-10000", "INSERT INTO t1 VALUES(3,4,zeroblob(10000),5)", 10010},
		// (4,5,zeroblob(10000),zeroblob(10000)): both trailing zeroblobs
		// stay unexpanded: hdr 9 + data 2 = 11.
		{"two-trailing", "INSERT INTO t1 VALUES(4,5,zeroblob(10000),zeroblob(10000))", 11},
		// (5,zeroblob(10000),NULL,zeroblob(10000)): NULLs between trailing
		// zeroblobs pass through the backward scan: hdr 9 + data 1 = 10.
		{"null-between", "INSERT INTO t1 VALUES(5,zeroblob(10000),NULL,zeroblob(10000))", 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storage.SetMaxBlobsize(0) // set ::sqlite3_max_blobsize 0
			if r := db.Exec(tc.sql); r.Error != nil {
				t.Fatalf("exec %q: %v", tc.sql, r.Error)
			}
			if got := storage.MaxBlobsize(); got != tc.want {
				t.Errorf("max_blobsize after %q = %d, want %d", tc.sql, got, tc.want)
			}
		})
	}
}
