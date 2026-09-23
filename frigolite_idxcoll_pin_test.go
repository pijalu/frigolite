// T31-idxcoll native pins: collation-ordered index maintenance and REINDEX.
//
// These drive the engine directly (frigolite.Open/Exec/Query) for the
// contracts the JSON harness cannot express: user-registered collation
// sequences (the TCL `db collate` fixtures behind reindex-2.x/3.x,
// collate8-1.x, minmax3-4.x), a collation comparator REDEFINED mid-session,
// and a second connection that lacks a schema collation (reindex-3.1).
// SQLite (3.54) is ground truth for every expectation here.

package frigolite_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func idxCollStrings(r *frigolite.Result) []string {
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		if len(row) == 0 {
			out = append(out, "")
			continue
		}
		if s, ok := row[0].(string); ok {
			out = append(out, s)
			continue
		}
		out = append(out, fmt.Sprintf("%v", row[0]))
	}
	return out
}

func idxCollJoin(r *frigolite.Result) string {
	return strings.Join(idxCollStrings(r), " ")
}

// idxCollReverse is reindex.test's c1: reverse binary.
func idxCollReverse(a, b string) int { return strings.Compare(b, a) }

// idxCollReverseNocase is reindex.test's c2: reverse of the case-folded
// comparison (ASCII lower).
func idxCollReverseNocase(a, b string) int {
	la, lb := []byte(a), []byte(b)
	for i := range la {
		if la[i] >= 'A' && la[i] <= 'Z' {
			la[i] += 'a' - 'A'
		}
	}
	for i := range lb {
		if lb[i] >= 'A' && lb[i] <= 'Z' {
			lb[i] += 'a' - 'A'
		}
	}
	return strings.Compare(string(lb), string(la))
}

// TestPinIndexDeclaredCollationOrder: index keys order by the keys'
// collations (declared column collations propagate into every index on the
// column; a custom registered collation orders the tree; the sqlite3 oracle
// stores the same shape).
func TestPinIndexDeclaredCollationOrder(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(dir + "/pin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.RegisterCollation("c1", idxCollReverse)
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t(a TEXT COLLATE nocase, b TEXT COLLATE c1, c)")
	mustExec("CREATE INDEX ia ON t(a)")
	mustExec("CREATE INDEX ib ON t(b)")
	for _, v := range []string{"BBB", "aaa", "DDD", "ccc"} {
		mustExec("INSERT INTO t VALUES('" + v + "','" + v + "','x')")
	}
	// nocase-ordered index scan (aaa BBB ccc DDD).
	if got := idxCollJoin(db.Query("SELECT a FROM t ORDER BY a")); got != "aaa BBB ccc DDD" {
		t.Errorf("ORDER BY a (nocase): got %q", got)
	}
	// reverse-ordered custom collation index: binary ascending is
	// BBB DDD aaa ccc, so c1 (reverse binary) yields ccc aaa DDD BBB.
	if got := idxCollJoin(db.Query("SELECT b FROM t ORDER BY b")); got != "ccc aaa DDD BBB" {
		t.Errorf("ORDER BY b (c1 reverse): got %q", got)
	}
	if r := db.Exec("PRAGMA integrity_check"); r.Error != nil {
		t.Errorf("integrity_check: %v", r.Error)
	}
	// The seek path agrees with the stored order (nocase equality).
	r := db.Query("SELECT rowid FROM t WHERE a='AAA'")
	if r.Error != nil {
		t.Fatalf("seek: %v", r.Error)
	}
	if got := idxCollJoin(r); got != "2" {
		t.Errorf("WHERE a='AAA': got %q want 2", got)
	}
}

// TestPinReindexRebuildsUnderChangedCollation: after the c1 comparator is
// redefined (the TCL proc late-binding of reindex-2.5), REINDEX c1 rebuilds
// the index under the NEW comparator, flipping the stored order — and the
// unchanged collation's index is left alone (reindex-2.6/2.8 contracts).
func TestPinReindexRebuildsUnderChangedCollation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/pin.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.RegisterCollation("c1", idxCollReverse)
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t2(a TEXT PRIMARY KEY COLLATE c1, b TEXT UNIQUE COLLATE nocase)")
	for _, v := range []string{"abc", "ABCD", "bcd", "BCDE"} {
		mustExec("INSERT INTO t2 VALUES('" + v + "','" + v + "')")
	}
	if got := idxCollJoin(db.Query("SELECT a FROM t2 ORDER BY a")); got != "bcd abc BCDE ABCD" {
		t.Fatalf("pre-reindex ORDER BY a: got %q", got)
	}
	// reindex-2.5: the comparator c1 is redefined (forward). The JSON file
	// cannot express re-registration, so the fixture here pins the engine
	// contract directly.
	db.RegisterCollation("c1", func(a, b string) int { return strings.Compare(a, b) })
	// reindex-2.6: REINDEX of the OTHER collation succeeds and leaves the
	// nocase index serving seeks. (SQLite's "result unchanged" expectation
	// rides on its index-scan ORDER BY; frigolite re-sorts under the
	// current comparator — see the lessons note — so only the seek
	// contract is pinned here.)
	mustExec("REINDEX nocase")
	r := db.Query("SELECT b FROM t2 WHERE b='abcd'")
	if r.Error != nil {
		t.Fatalf("seek after REINDEX nocase: %v", r.Error)
	}
	if got := idxCollJoin(r); got != "ABCD" {
		t.Fatalf("nocase seek after REINDEX nocase: got %q want ABCD", got)
	}
	// reindex-2.8: REINDEX c1 rebuilds under the NEW (forward) comparator.
	mustExec("REINDEX c1")
	if got := idxCollJoin(db.Query("SELECT a FROM t2 ORDER BY a")); got != "ABCD BCDE abc bcd" {
		t.Fatalf("after REINDEX c1: got %q", got)
	}
	mustExec("PRAGMA integrity_check")
	// The b index (nocase) was NOT rebuilt by REINDEX c1 and still serves
	// nocase seeks.
	r2 := db.Query("SELECT b FROM t2 WHERE b='abcd'")
	if r2.Error != nil {
		t.Fatalf("seek after reindex: %v", r2.Error)
	}
	if got := idxCollJoin(r2); got != "ABCD" {
		t.Errorf("nocase seek after REINDEX c1: got %q want ABCD", got)
	}
	db.Close()
}

// TestPinReindexSecondConnectionMissingCollation: a connection that did not
// register a schema collation fails REINDEX of that collation's indexes with
// "no such collation sequence" (reindex-3.1), succeeds after registering it
// via the collation-needed hook (reindex-3.2), and a whole-schema REINDEX
// names the FIRST missing collation in reverse declaration order
// (reindex-3.3: c2 before c1).
func TestPinReindexSecondConnectionMissingCollation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/pin.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.RegisterCollation("c1", idxCollReverse)
	db.RegisterCollation("c2", idxCollReverseNocase)
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t2(a TEXT PRIMARY KEY COLLATE c1, b TEXT UNIQUE COLLATE c2)")
	mustExec("INSERT INTO t2 VALUES('abc','abc')")
	db.Close()

	// reindex-3.1: a second connection without c1/c2.
	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if r := db2.Exec("REINDEX c1"); r.Error == nil || r.Error.Error() != "no such collation sequence: c1" {
		t.Errorf("REINDEX c1 without c1: got %v", r.Error)
	}
	if r := db2.Exec("REINDEX"); r.Error == nil || r.Error.Error() != "no such collation sequence: c2" {
		t.Errorf("REINDEX without c2: got %v", r.Error)
	}
	// reindex-3.2: collation_needed registers c1; REINDEX c1 succeeds.
	db2.RegisterCollationNeeded(func(name string) {
		if name == "c1" {
			db2.RegisterCollation("c1", idxCollReverse)
		}
	})
	if r := db2.Exec("REINDEX c1"); r.Error != nil {
		t.Errorf("REINDEX c1 after registration: %v", r.Error)
	}
	// The rebuilt PK index still orders reverse (the registered comparator).
	if got := idxCollJoin(db2.Query("SELECT a FROM t2 ORDER BY a")); got != "abc" {
		t.Errorf("post-reindex order: got %q", got)
	}
}

// TestPinOrderByAliasCollation is the native port of collate8-1.11/1.13/1.15:
// an ORDER BY term naming a result-column alias inherits the aliased
// expression's collation — through the quoted-alias spellings and unary +.
func TestPinOrderByAliasCollation(t *testing.T) {
	db, err := frigolite.Open(t.TempDir() + "/pin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t1(a TEXT COLLATE nocase)")
	for _, v := range []string{"aaa", "BBB", "ccc", "DDD"} {
		mustExec("INSERT INTO t1 VALUES('" + v + "')")
	}
	for _, q := range []string{
		"SELECT a AS x FROM t1 ORDER BY \"x\"",
		"SELECT a AS x FROM t1 ORDER BY [x]",
		"SELECT a AS x FROM t1 ORDER BY +x",
		"SELECT a AS x FROM t1 ORDER BY x",
	} {
		r := db.Query(q)
		if r.Error != nil {
			t.Fatalf("%q: %v", q, r.Error)
		}
		if got := idxCollJoin(r); got != "aaa BBB ccc DDD" {
			t.Errorf("%q: got %q want nocase order", q, got)
		}
	}
	// collate8-1.13: a binary-collated WHERE comparison with a nocase
	// alias-ordered output.
	r := db.Query("SELECT a AS x FROM t1 WHERE x<'ccc' COLLATE binary ORDER BY [x]")
	if r.Error != nil {
		t.Fatalf("1.13: %v", r.Error)
	}
	if got := idxCollJoin(r); got != "aaa BBB DDD" {
		t.Errorf("1.13: got %q want %q", got, "aaa BBB DDD")
	}
}

// TestPinMinMaxCollateArgument is the native port of minmax3-4.x: a
// single-argument MIN/MAX compares its argument under the argument's
// collation (explicit COLLATE operator or declared column collation).
func TestPinMinMaxCollateArgument(t *testing.T) {
	db, err := frigolite.Open(t.TempDir() + "/pin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t4(x)")
	mustExec("INSERT INTO t4 VALUES('abc')")
	mustExec("INSERT INTO t4 VALUES('BCD')")
	cases := []struct {
		q, want string
	}{
		{"SELECT max(x) FROM t4", "abc"},
		{"SELECT max(x COLLATE nocase) FROM t4", "BCD"},
		{"SELECT max(x COLLATE binary) FROM t4", "abc"},
		{"SELECT min(x COLLATE nocase) FROM t4", "abc"},
		{"SELECT min(x) FROM t4", "BCD"},
		// Two-argument min/max is the SCALAR function (binary collation).
		{"SELECT min('abc','BCD')", "BCD"},
	}
	for _, tc := range cases {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Fatalf("%q: %v", tc.q, r.Error)
		}
		if got := idxCollJoin(r); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.q, got, tc.want)
		}
	}
	// A declared column collation also drives the reduction.
	mustExec("CREATE TABLE t5(y TEXT COLLATE nocase)")
	mustExec("INSERT INTO t5 VALUES('abc')")
	mustExec("INSERT INTO t5 VALUES('BCD')")
	if got := idxCollJoin(db.Query("SELECT max(y) FROM t5")); got != "BCD" {
		t.Errorf("max over declared-nocase column: got %q want BCD", got)
	}
}

// TestPinSecondConnectionReadsCollationOrderedIndex: a database written by
// one connection (nocase-declared column + custom collation index) is read
// back correctly by a fresh connection that re-registers the collations —
// the on-disk order IS the collation order (frigolite↔frigolite; validated
// against the sqlite3 CLI during FULL-SUITE-DRIFT.T31-idxcoll).
func TestPinSecondConnectionReadsCollationOrderedIndex(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/pin.db"
	db, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.RegisterCollation("c1", idxCollReverse)
	mustExec := func(q string) {
		t.Helper()
		if r := db.Exec(q); r.Error != nil {
			t.Fatalf("exec %q: %v", q, r.Error)
		}
	}
	mustExec("CREATE TABLE t(a TEXT COLLATE nocase, b TEXT COLLATE c1)")
	mustExec("CREATE INDEX ia ON t(a)")
	mustExec("CREATE INDEX ib ON t(b)")
	for _, v := range []string{"BBB", "aaa", "DDD", "ccc"} {
		mustExec("INSERT INTO t VALUES('" + v + "','" + v + "')")
	}
	db.Close()

	db2, err := frigolite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.RegisterCollation("c1", idxCollReverse)
	r := db2.Exec("PRAGMA integrity_check")
	if r.Error != nil {
		t.Fatalf("integrity_check: %v", r.Error)
	}
	if got := idxCollJoin(db2.Query("SELECT a FROM t ORDER BY a")); got != "aaa BBB ccc DDD" {
		t.Errorf("reopened nocase order: got %q", got)
	}
	if got := idxCollJoin(db2.Query("SELECT b FROM t ORDER BY b")); got != "ccc aaa DDD BBB" {
		t.Errorf("reopened c1 order: got %q", got)
	}
}
