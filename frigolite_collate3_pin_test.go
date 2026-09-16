package frigolite_test

// Native pin for the engine-visible contract of collate3.test (FULL-SUITE-
// DRIFT pairs-querya cluster).
//
// C contract (callback.c sqlite3GetCollSeq + main.c sqlite3_collation_needed):
// when a statement resolves a collation that is not registered, the engine
// invokes the collation-needed callback with the missing name
// (callCollNeeded), re-resolves, and the referencing statement succeeds when
// the callback registered it. The callback fires ONCE per collation per
// connection — after registration the lookup succeeds directly. Without the
// hook (or after a close/reopen that did not re-register) the statement
// fails with "no such collation sequence: NAME".
//
//   - collate3-5.2/5.4/5.6: ORDER BY ... COLLATE unk succeeds under the hook;
//     the factory fired exactly once (5.3/5.5) and stays at one call.
//   - collate3-5.7: CREATE TABLE t(a COLLATE unk) succeeds once "unk" is
//     registered; after a close/reopen WITHOUT re-registering, ORDER BY 1
//     (schema collation resolution) fails naming "unk".
//   - collate3-4.8.2/4.8.3: a close/reopen mid-test leaves the schema usable
//     (DROP TABLE works on the fresh connection).

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestCollate3CollationNeededContract(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// collate3-5.0 without the hook: the unknown collation errors.
	if err := db.Exec("CREATE TABLE collate3t1(a); INSERT INTO collate3t1 VALUES(10);").Error; err != nil {
		t.Fatal(err)
	}
	r := db.Exec("SELECT a FROM collate3t1 ORDER BY 1 COLLATE unk;")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such collation sequence: unk") {
		t.Fatalf("pre-hook ORDER BY COLLATE unk: got %v, want no-such-collation error", r.Error)
	}

	// collate3-5.1: install the collation factory (db collation_needed cfact).
	var calls int64
	db.RegisterCollationNeeded(func(name string) {
		atomic.AddInt64(&calls, 1)
		db.RegisterCollation(name, func(a, b string) int { return strings.Compare(a, b) })
	})

	// collate3-5.2: the same ORDER BY now succeeds.
	if r := db.Exec("SELECT a FROM collate3t1 ORDER BY 1 COLLATE unk;"); r.Error != nil {
		t.Fatalf("post-hook ORDER BY COLLATE unk: %v", r.Error)
	}
	// collate3-5.3: the factory fired exactly once.
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("collation-needed fired %d times, want 1", n)
	}
	// collate3-5.4/5.5: repeated statements stay successful and do not
	// re-invoke the factory (the collation is now registered).
	for i := 0; i < 2; i++ {
		if r := db.Exec("SELECT a FROM collate3t1 ORDER BY 1 COLLATE unk;"); r.Error != nil {
			t.Fatalf("repeat ORDER BY COLLATE unk: %v", r.Error)
		}
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("collation-needed fired %d times after repeats, want 1", n)
	}

	// collate3-5.7 setup: with "unk" registered the schema accepts it. The
	// table is recreated WITHOUT rows: 5.8 expects {0 {}} (empty result).
	if err := db.Exec("DROP TABLE collate3t1; CREATE TABLE collate3t1(a COLLATE unk);").Error; err != nil {
		t.Fatalf("CREATE TABLE with factory-registered collation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := frigolite.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	// collate3-5.7: on the fresh connection the schema-declared collation no
	// longer resolves — the ORDER BY fails with the collation's name
	// (catchsql {1 {no such collation sequence: unk}}).
	r2 := db2.Exec("SELECT a FROM collate3t1 ORDER BY 1;")
	if r2.Error == nil || !strings.Contains(r2.Error.Error(), "no such collation sequence: unk") {
		t.Fatalf("reopen without re-registration: got %v, want no-such-collation error", r2.Error)
	}
	// CREATE-time validation still fires on the fresh connection too (a name
	// that is not registered is rejected before any schema entry is written).
	if err := db2.Exec("CREATE TABLE collate3t2(a COLLATE unk);").Error; err == nil ||
		!strings.Contains(err.Error(), "no such collation sequence: unk") {
		t.Fatalf("CREATE on fresh connection: got %v, want no-such-collation error", err)
	}

	// collate3-5.8: re-installing the factory makes the same statement
	// succeed with an empty result and exactly one factory call.
	var calls2 int64
	db2.RegisterCollationNeeded(func(name string) {
		atomic.AddInt64(&calls2, 1)
		db2.RegisterCollation(name, func(a, b string) int { return strings.Compare(a, b) })
	})
	r3 := db2.Exec("SELECT a FROM collate3t1 ORDER BY 1;")
	if r3.Error != nil {
		t.Fatalf("reopen with factory re-registered: %v", r3.Error)
	}
	if len(r3.Rows) != 0 {
		t.Fatalf("rows = %v, want empty result", r3.Rows)
	}
	if n := atomic.LoadInt64(&calls2); n != 1 {
		t.Fatalf("reopened-connection factory fired %d times, want 1", n)
	}

	// collate3-5.9 (and collate3-4.8.3's contract): the reopened connection's
	// schema remains fully usable.
	if err := db2.Exec("DROP TABLE collate3t1;").Error; err != nil {
		t.Fatalf("DROP TABLE after reopen: %v", err)
	}
}
