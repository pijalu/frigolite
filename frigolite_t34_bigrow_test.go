package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// This file pins the bigrow (oversized-row) UPDATE contract natively:
// swapping or rewriting a ~65KB value via UPDATE must preserve it
// byte-for-byte. It carries the engine-visible contract of testgen/bigrow
// "bigrow-2.2", which the generated corpus skips for an emitter-side want
// rendering reason (tclListFlatten collapses the trailing space of the
// single-element TCL list want; the engine's value is byte-exact, verified
// against the sqlite3 oracle and at bases 9372fbb85/01e0371e4).

// t34BigRow builds the bigrow-1.0 corpus string: for i in 1..9999 append
// alphabet[i%26] + " " + fmt.Sprintf("%04d", i) + " " (69993 bytes).
func t34BigRow() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	var sb strings.Builder
	for i := 1; i <= 9999; i++ {
		sb.WriteByte(alphabet[i%26])
		sb.WriteByte(' ')
		fmt.Fprintf(&sb, "%04d", i)
		sb.WriteByte(' ')
	}
	return sb.String()
}

// t34Pattern builds a deterministic value of n bytes ("v%05d " repeating),
// useful for locating the exact byte offset of any corruption.
func t34Pattern(n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		fmt.Fprintf(&sb, "v%05d ", i)
	}
	return sb.String()[:n]
}

// t34Exec runs one statement, failing the test on error.
func t34Exec(t *testing.T, db *frigolite.DB, sql string) {
	t.Helper()
	if err := db.Exec(sql).Error; err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// t34OneText queries a single-row single-TEXT-column result.
func t34OneText(t *testing.T, db *frigolite.DB, sql string) string {
	t.Helper()
	res := db.Query(sql)
	if res.Error != nil {
		t.Fatalf("query %q: %v", sql, res.Error)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		cols := 0
		if len(res.Rows) > 0 {
			cols = len(res.Rows[0])
		}
		t.Fatalf("query %q: want 1x1 row, got %dx%d", sql, len(res.Rows), cols)
	}
	s, ok := res.Rows[0][0].(string)
	if !ok {
		t.Fatalf("query %q: want TEXT, got %T", sql, res.Rows[0][0])
	}
	return s
}

// t34RequireExact fails the test describing the first differing byte of got
// against want.
func t34RequireExact(t *testing.T, got, want string, stage string) {
	t.Helper()
	if got == want {
		return
	}
	at := -1
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			at = i
			break
		}
	}
	t.Fatalf("%s: value mangled: len got=%d want=%d firstDiff=%d (got ctx %q want ctx %q)",
		stage, len(got), len(want), at,
		got[max(0, at-8):min(len(got), at+8)], want[max(0, at-8):min(len(want), at+8)])
}

// t34IntegrityOK asserts PRAGMA integrity_check reports ok.
func t34IntegrityOK(t *testing.T, db *frigolite.DB) {
	t.Helper()
	res := db.Query(`PRAGMA integrity_check`)
	if res.Error != nil {
		t.Fatalf("integrity_check: %v", res.Error)
	}
	if len(res.Rows) > 0 {
		if s, _ := res.Rows[0][0].(string); s != "ok" {
			t.Fatalf("integrity_check: %v", res.Rows[0][0])
		}
	}
}

// TestT34BigRowCorpus22 replays the exact bigrow corpus statement sequence
// through 2.2 (insert big1, insert+delete big2, swap 1.5, two small rows,
// index on a, swap 2.2) and requires the swapped ~65KB value back
// byte-for-byte. Oracle-verified against /usr/bin/sqlite3.
func TestT34BigRowCorpus22(t *testing.T) {
	big1 := t34BigRow()[:65520]
	if !strings.HasSuffix(big1, "9360 ") {
		t.Fatalf("shape drift: big1 suffix %q", big1[len(big1)-10:])
	}

	path := t.TempDir() + "/t34corpus.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	t34Exec(t, db, `CREATE TABLE t1(a text, b text, c text)`)
	t34Exec(t, db, "INSERT INTO t1 VALUES('abc','"+big1+"', 'xyz');")
	t34Exec(t, db, "INSERT INTO t1 VALUES('abc2','"+t34BigRow()[:65521]+"', 'xyz2');")
	t34Exec(t, db, `DELETE FROM t1 WHERE a='abc2'`)
	t34Exec(t, db, `UPDATE t1 SET a=b, b=a`)
	t34Exec(t, db, `INSERT INTO t1 VALUES('1','2','3');`)
	t34Exec(t, db, `INSERT INTO t1 VALUES('A','B','C');`)
	t34Exec(t, db, `CREATE INDEX i1 ON t1(a)`)

	t34Exec(t, db, `UPDATE t1 SET a=b, b=a`)
	t34RequireExact(t, t34OneText(t, db, `SELECT b FROM t1 WHERE a=='abc'`), big1, "bigrow-2.2 swap")

	// Swap back (bigrow-2.3) and check the small-column value too.
	t34Exec(t, db, `UPDATE t1 SET a=b, b=a`)
	t34RequireExact(t, t34OneText(t, db, "SELECT b FROM t1 WHERE a=='"+big1+"'"), "abc", "bigrow-2.3 swap back")

	t34IntegrityOK(t, db)

	// Persistence: the swapped value must survive close/reopen byte-exact.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	t34RequireExact(t, t34OneText(t, db2, "SELECT a FROM t1 WHERE b=='abc'"), big1, "after reopen")
}

// t34SwapCase drives one swap-shape case at one page size with/without an
// index on the big column: INSERT big, swap twice, verify byte-exactness
// in-session, after integrity_check, and after close/reopen.
func t34SwapCase(t *testing.T, ps int, withIndex bool, big1 string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/t34swap.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ps > 0 {
		t34Exec(t, db, fmt.Sprintf("PRAGMA page_size=%d", ps))
	}
	t34Exec(t, db, `CREATE TABLE t1(a text, b text, c text)`)
	if withIndex {
		t34Exec(t, db, `CREATE INDEX i1 ON t1(a)`)
	}
	t34Exec(t, db, "INSERT INTO t1 VALUES('seed0','"+big1+"','xyz')")

	// Swap big from b to a and back: both directions byte-exact.
	t34Exec(t, db, `UPDATE t1 SET a=b, b=a`)
	t34RequireExact(t, t34OneText(t, db, `SELECT a FROM t1 WHERE b=='seed0'`), big1, "swap b->a")
	t34Exec(t, db, `UPDATE t1 SET a=b, b=a`)
	t34RequireExact(t, t34OneText(t, db, `SELECT b FROM t1 WHERE a=='seed0'`), big1, "swap a->b")

	t34IntegrityOK(t, db)

	// Close/reopen: the persisted file must carry the value intact.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	t34RequireExact(t, t34OneText(t, db2, `SELECT b FROM t1 WHERE a=='seed0'`), big1, "after reopen")
}

// TestT34BigRowSwapByteExact swaps a large value via UPDATE SET a=b, b=a at
// several page sizes with and without an index on the big column, requiring
// byte-exact round trips in-session, after integrity_check, and after
// close/reopen (oracle-verified: /usr/bin/sqlite3, ps 512..65536).
func TestT34BigRowSwapByteExact(t *testing.T) {
	big1 := t34BigRow()[:65520]
	for _, ps := range []int{0, 512, 1024, 4096, 65536} {
		for _, withIndex := range []bool{false, true} {
			ps, withIndex := ps, withIndex
			t.Run(fmt.Sprintf("ps%d/idx%v", ps, withIndex), func(t *testing.T) {
				t34SwapCase(t, ps, withIndex, big1)
			})
		}
	}
}

// t34SweepUpdateRewrite drives one UPDATE rewrite shape at one size/page
// size: INSERT a big value, UPDATE it to a different big value (+3 bytes,
// different content), then verify byte-exactness in-session, after
// integrity_check, and after close/reopen.
func t34SweepUpdateRewrite(t *testing.T, ps, n int) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/t34sweep.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ps > 0 {
		t34Exec(t, db, fmt.Sprintf("PRAGMA page_size=%d", ps))
	}
	t34Exec(t, db, `CREATE TABLE t1(a text, b text, c text)`)

	seed := t34Pattern(n)
	t34Exec(t, db, "INSERT INTO t1 VALUES('seed0','"+seed+"','xyz')")
	t34RequireExact(t, t34OneText(t, db, `SELECT b FROM t1 WHERE a=='seed0'`), seed, "after insert")

	want := t34Pattern(n + 3)
	t34Exec(t, db, "UPDATE t1 SET b='"+want+"' WHERE a=='seed0'")
	t34RequireExact(t, t34OneText(t, db, `SELECT b FROM t1 WHERE a=='seed0'`), want, "after update")

	t34IntegrityOK(t, db)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	t34RequireExact(t, t34OneText(t, db2, `SELECT b FROM t1`), want, "after reopen")
}

// TestT34BigRowUpdateSizeSweep sweeps UPDATE rewrites of large values across
// the overflow-boundary sizes (minLocal/maxLocal transitions for page sizes
// 512..65536, the 256/257 and 65536/65537 row-size transitions bigrow-3/4
// probe, and the 65520 corpus size). Wants are oracle-verified
// (/usr/bin/sqlite3 byte-exact) at 240/256/257/65520/65537 for every page
// size probed here.
func TestT34BigRowUpdateSizeSweep(t *testing.T) {
	sizes := []int{200, 240, 241, 256, 257, 489, 490, 989, 990, 4061, 4062, 12288, 65514, 65518, 65519, 65520, 65521, 65522, 65535, 65536, 65537, 65538, 131072, 131073}
	pageSizes := []int{0, 512, 1024, 4096, 65536}
	for _, ps := range pageSizes {
		for _, n := range sizes {
			ps, n := ps, n
			t.Run(fmt.Sprintf("ps%d/n%d", ps, n), func(t *testing.T) {
				t.Parallel()
				t34SweepUpdateRewrite(t, ps, n)
			})
		}
	}
}

// TestT34BigRowSeamSweep targets the partial-local branch of the surplus
// formula — the seam where the cell's local fragment lands strictly between
// minLocal and maxLocal — and the seam-CROSSING sizes where an UPDATE's
// +3-byte growth flips local from maxLocal to minLocal (the local fragment
// shrinks and the overflow chain is rebuilt; the rewrite boundary the
// bigrow-2.2 residue pointed at). Sizes are derived per page size from
// LocalPayloadSize's formula.
func TestT34BigRowSeamSweep(t *testing.T) {
	cases := map[int][]int{
		// ps=512: maxLocal=477, minLocal=39, period 508.
		512: {478, 479, 480, 547, 916, 917, 918, 985, 1424, 1425, 1426},
		// ps=1024: maxLocal=989, minLocal=104, period 1020.
		1024: {990, 991, 992, 1093, 1873, 1874, 1875, 1876, 2000, 2893, 2894, 2895, 2896, 4061},
		// ps=4096: maxLocal=4061, minLocal=489, period 4092; 8152->8155
		// (via +3) crosses local 4060 -> 489.
		4096: {4092, 4093, 4094, 5000, 8150, 8151, 8152, 8153, 8154, 12288},
		// ps=65536: maxLocal=65501, minLocal=8199, period 65532; the
		// partial branch lives near n=98304.
		65536: {65502, 65503, 98304, 98305, 131072},
	}
	for ps, sizes := range cases {
		for _, n := range sizes {
			ps, n := ps, n
			t.Run(fmt.Sprintf("ps%d/n%d", ps, n), func(t *testing.T) {
				t.Parallel()
				t34SweepUpdateRewrite(t, ps, n)
			})
		}
	}
}
