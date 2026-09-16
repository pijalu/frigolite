package frigolite_test

// Native pins for the engine-visible contracts of the collate4 / func3 /
// func4 testgen packages (FULL-SUITE-DRIFT pairs-querya cluster).
//
//   - func4-3.18: INTEGER-affinity storage of '-9223372036854775809' keeps
//     the value REAL (util.c sqlite3Atoi64 overflow; vdbeaux.c
//     sqlite3VdbeIntegerAffinity refuses ix == SMALLEST_INT64), so
//     tointeger(x) reads NULL and CHECK(tointeger(x) IS NOT NULL) rejects
//     the row (ext/misc/totype.c totypeAtoi64 returns overflow → NULL).
//   - collate4-3.11: a UNIQUE index key uses its explicit COLLATE (build.c
//     sqlite3CreateIndex), falling back to the column's declared collation,
//     so INSERT 'abc' + 'ABC' under COLLATE NOCASE conflicts.
//   - func3-2.2 (engine-visible half): re-registering a UDF replaces the old
//     registration; the new implementation answers the next query. The
//     xDestroy callback count itself is C-API-only (skip map func3-2.2).

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

func TestFunc4IntegerAffinityOverflowStaysReal(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// func4-3.18: the CHECK rejects the overflow text.
	if err := db.Exec("CREATE TABLE t1(x INTEGER CHECK(tointeger(x) IS NOT NULL));").Error; err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("INSERT INTO t1 (x) VALUES ('-9223372036854775809');"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "CHECK constraint failed: tointeger(x) IS NOT NULL") {
		t.Fatalf("INSERT overflow text: got %v, want CHECK failure", r.Error)
	}

	// Affinity model (matches sqlite3 shell): overflow text and the REAL
	// -2^63 stay REAL; the in-range text converts to INTEGER.
	if err := db.Exec("CREATE TABLE t2(x INTEGER); INSERT INTO t2 VALUES('-9223372036854775809'),('-9223372036854775808'),(-9223372036854775808.0),(1234.0);").Error; err != nil {
		t.Fatal(err)
	}
	q := db.Query("SELECT typeof(x) FROM t2 ORDER BY rowid;")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	want := []string{"real", "integer", "real", "integer"}
	if len(q.Rows) != len(want) {
		t.Fatalf("rows = %v, want %d rows", q.Rows, len(want))
	}
	for i, row := range q.Rows {
		if got := row[0]; got != want[i] {
			t.Fatalf("row %d typeof = %v, want %s", i, got, want[i])
		}
	}
}

func TestCollate4UniqueIndexKeyCollation(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// collate4-3.11: explicit COLLATE on the index key.
	if err := db.Exec("CREATE TABLE collate4t1(a); CREATE UNIQUE INDEX collate4i1 ON collate4t1(a COLLATE NOCASE);").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO collate4t1 VALUES('abc');").Error; err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("INSERT INTO collate4t1 VALUES('ABC');"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "UNIQUE constraint failed: collate4t1.a") {
		t.Fatalf("unique index COLLATE NOCASE: got %v, want UNIQUE failure", r.Error)
	}

	// collate4-3.5/3.6: the column-level form — a UNIQUE column's declared
	// collation is the autoindex key's collation.
	if err := db.Exec("DROP TABLE collate4t1; CREATE TABLE collate4t1(a COLLATE NOCASE UNIQUE);").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO collate4t1 VALUES('abc');").Error; err != nil {
		t.Fatal(err)
	}
	if r := db.Exec("INSERT INTO collate4t1 VALUES('ABC');"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "UNIQUE constraint failed: collate4t1.a") {
		t.Fatalf("column UNIQUE COLLATE NOCASE: got %v, want UNIQUE failure", r.Error)
	}
}

func TestFunc3ReregisterReplacesUDF(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// func3-2.1: the first registration answers.
	db.RegisterFunction("f3", func(args []interface{}) (interface{}, error) {
		return int64(1), nil
	}, 0, 0)
	if r := db.Query("SELECT f3();"); r.Error != nil || r.Rows[0][0] != int64(1) {
		t.Fatalf("first f3() = %v (err %v), want 1", r.Rows, r.Error)
	}
	// func3-2.2 (engine-visible half): re-registering replaces the old one.
	db.RegisterFunction("f3", func(args []interface{}) (interface{}, error) {
		return int64(2), nil
	}, 0, 0)
	if r := db.Query("SELECT f3();"); r.Error != nil || r.Rows[0][0] != int64(2) {
		t.Fatalf("re-registered f3() = %v (err %v), want 2", r.Rows, r.Error)
	}
}
