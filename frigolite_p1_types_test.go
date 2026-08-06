package frigolite

import (
	"reflect"
	"strings"
	"testing"
)

// TestP1TypesAffinity covers declared-type → affinity mapping
// (sqlite3Affinity rules): INT→INTEGER, CHAR/CLOB/TEXT→TEXT,
// BLOB→BLOB, REAL/FLOA/DOUB→REAL, else NUMERIC.
func TestP1TypesAffinity(t *testing.T) {
	cases := []struct {
		declared string
		value    string
		want     string // typeof result
	}{
		// INTEGER affinity: '5' → 5 (integer)
		{"INT", "'5'", "integer"},
		{"INTEGER", "5.0", "integer"},
		// TEXT affinity: numbers → text
		{"TEXT", "5", "text"},
		{"CHAR(10)", "5.5", "text"},
		// CLOB has TEXT affinity but blobs stay blobs
		{"CLOB", "x'00'", "blob"},
		// BLOB affinity: no conversion
		{"BLOB", "'5'", "text"},
		// REAL affinity: integer → real
		{"REAL", "5", "real"},
		{"FLOAT", "5", "real"},
		{"DOUBLE", "5", "real"},
		// NUMERIC affinity: '5' → 5, '5.5' → real
		{"NUMERIC", "'5'", "integer"},
		{"NUMERIC", "'5.5'", "real"},
		// No declared type → BLOB affinity (no conversion)
		{"", "'5'", "text"},
	}
	for _, tc := range cases {
		t.Run(tc.declared+"="+tc.value, func(t *testing.T) {
			db := setupDB(t)
			defer db.Close()
			var sql string
			if tc.declared == "" {
				sql = "CREATE TABLE t(c)"
			} else {
				sql = "CREATE TABLE t(c " + tc.declared + ")"
			}
			if res := db.Exec(sql); res.Error != nil {
				t.Fatalf("create %q: %v", sql, res.Error)
			}
			if res := db.Exec("INSERT INTO t VALUES(" + tc.value + ")"); res.Error != nil {
				t.Fatalf("insert %q: %v", tc.value, res.Error)
			}
			got := queryRows(t, db, "SELECT typeof(c) FROM t")
			want := [][]string{{tc.want}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("typeof mismatch: got %v want %v", got, want)
			}
		})
	}
}

// TestP1TypesInsertCoercion covers insert-time affinity application:
// '5'→INTEGER col stores 5; 3.0→TEXT col stores '3.0'; blob→TEXT.
func TestP1TypesInsertCoercion(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	runSQL(t, db, "CREATE TABLE t(i INTEGER, r REAL, te TEXT)")
	runSQL(t, db, "INSERT INTO t VALUES('5', 3.0, x'6869')")
	got := queryRows(t, db, "SELECT i, typeof(i), r, typeof(r), te, hex(te) FROM t")
	want := [][]string{{"5", "integer", "3.0", "real", "x'6869'", "6869"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coercion mismatch:\n got %v\nwant %v", got, want)
	}
	// TEXT affinity converts numeric values to text but leaves blobs as blobs.
	runSQL(t, db, "CREATE TABLE t2(te TEXT)")
	runSQL(t, db, "INSERT INTO t2 VALUES(5), (x'00')")
	got = queryRows(t, db, "SELECT typeof(te), hex(te) FROM t2")
	want = [][]string{{"text", "35"}, {"blob", "00"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("text affinity mismatch:\n got %v\nwant %v", got, want)
	}
	// NUMERIC affinity: a column with no declared type (BLOB affinity) keeps
	// '3.0' as text; a NUMERIC-declared column canonicalizes to integer 3.
	runSQL(t, db, "CREATE TABLE n(c)")
	runSQL(t, db, "INSERT INTO n VALUES('3.0')")
	got = queryRows(t, db, "SELECT c, typeof(c) FROM n")
	want = [][]string{{"3.0", "text"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blob-affinity coercion mismatch:\n got %v\nwant %v", got, want)
	}
	runSQL(t, db, "CREATE TABLE n2(c NUMERIC)")
	runSQL(t, db, "INSERT INTO n2 VALUES('3.0')")
	got = queryRows(t, db, "SELECT c, typeof(c) FROM n2")
	want = [][]string{{"3", "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("numeric canonicalization mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesCast covers CAST to each target type, incl. NUMERIC and
// TYPEOF after CAST.
func TestP1TypesCast(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	got := queryRows(t, db, "SELECT CAST('123' AS INTEGER), CAST('12.7' AS INTEGER), CAST('abc' AS INTEGER)")
	want := [][]string{{"123", "12", "0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cast int mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT CAST('12.5' AS REAL), CAST('abc' AS REAL), CAST(7 AS REAL)")
	want = [][]string{{"12.5", "0.0", "7.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cast real mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT CAST(123 AS TEXT), CAST(1.5 AS TEXT), typeof(CAST(1.5 AS TEXT))")
	want = [][]string{{"123", "1.5", "text"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cast text mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT CAST('3.5' AS NUMERIC), typeof(CAST('3.5' AS NUMERIC)), CAST('3' AS NUMERIC), typeof(CAST('3' AS NUMERIC))")
	want = [][]string{{"3.5", "real", "3", "integer"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cast numeric mismatch:\n got %v\nwant %v", got, want)
	}
	// CAST affinity in comparison: CAST('abc' AS NUMERIC) = 0 > '-1'
	got = queryRows(t, db, "SELECT CAST('abc' AS NUMERIC) > '-1'")
	want = [][]string{{"1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cast compare mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesTypedef covers TYPEOF for each storage class.
func TestP1TypesTypedef(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	got := queryRows(t, db, "SELECT typeof(1), typeof(1.5), typeof('x'), typeof(x'00'), typeof(NULL)")
	want := [][]string{{"integer", "real", "text", "blob", "null"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("typeof mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesIntPKey covers INTEGER PRIMARY KEY rowid aliasing and the
// datatype-mismatch rejection of non-integer values.
func TestP1TypesIntPKey(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	runSQL(t, db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b)")
	runSQL(t, db, "INSERT INTO t VALUES(5, 'five')")
	runSQL(t, db, "INSERT INTO t VALUES('123', 'text-int')")
	runSQL(t, db, "INSERT INTO t VALUES(3.0, 'real-int')")
	// rowid aliasing: the PK column IS the rowid
	got := queryRows(t, db, "SELECT rowid, a FROM t ORDER BY rowid")
	want := [][]string{{"3", "3"}, {"5", "5"}, {"123", "123"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intpkey rowid mismatch:\n got %v\nwant %v", got, want)
	}
	// Non-integer values are rejected with datatype mismatch
	for _, bad := range []string{"'x'", "3.5", "''"} {
		res := db.Exec("INSERT INTO t VALUES(" + bad + ", 'bad')")
		if res.Error == nil || !strings.Contains(res.Error.Error(), "datatype mismatch") {
			t.Errorf("insert %s: expected datatype mismatch, got %v", bad, res.Error)
		}
	}
	// NULL auto-assigns the next rowid
	runSQL(t, db, "INSERT INTO t(b) VALUES('auto')")
	got = queryRows(t, db, "SELECT a FROM t WHERE b='auto'")
	want = [][]string{{"124"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intpkey auto rowid mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesIntReal covers the intreal() test function: a REAL with an
// integer value that renders as N.0, typeof 'real', compares numerically.
func TestP1TypesIntReal(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	got := queryRows(t, db, "SELECT intreal(5), typeof(intreal(9)), intreal(5)=5, 6=intreal(6)")
	want := [][]string{{"5.0", "real", "1", "1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intreal mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT 'a'||intreal(11)||'z'")
	want = [][]string{{"a11.0z"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intreal concat mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT max(1.0,intreal(2),3.0), max(1,intreal(2),3), max(1,intreal(4),3)")
	want = [][]string{{"3.0", "3", "4.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("intreal max mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesNull covers NULL storage class: IS NULL, typeof, aggregates skip.
func TestP1TypesNull(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	runSQL(t, db, "CREATE TABLE t(a)")
	runSQL(t, db, "INSERT INTO t VALUES(NULL), (1), (NULL)")
	got := queryRows(t, db, "SELECT a FROM t WHERE a IS NULL")
	want := [][]string{{"{}"}, {"{}"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IS NULL mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT typeof(a) FROM t WHERE a IS NULL LIMIT 1")
	want = [][]string{{"null"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("typeof NULL mismatch:\n got %v\nwant %v", got, want)
	}
	// Aggregates skip NULLs
	got = queryRows(t, db, "SELECT count(a), count(*), sum(a) FROM t")
	want = [][]string{{"1", "3", "1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aggregate NULL skip mismatch:\n got %v\nwant %v", got, want)
	}
	// NULLs are distinct in UNIQUE constraints
	runSQL(t, db, "CREATE TABLE u(a, b, c, UNIQUE(b,c) ON CONFLICT IGNORE)")
	runSQL(t, db, "INSERT INTO u VALUES(1,1,1)")
	runSQL(t, db, "INSERT INTO u VALUES(2,NULL,1)")
	runSQL(t, db, "INSERT INTO u VALUES(3,NULL,1)")
	runSQL(t, db, "INSERT INTO u VALUES(4,1,1)") // ignored (conflict)
	got = queryRows(t, db, "SELECT a FROM u")
	want = [][]string{{"1"}, {"2"}, {"3"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unique NULL distinct mismatch:\n got %v\nwant %v", got, want)
	}
}

// TestP1TypesRowidFloatBoundary covers rowid comparisons with REAL values at
// the int64 boundary (2^63 clamps to MaxInt64, -2^63 to MinInt64).
func TestP1TypesRowidFloatBoundary(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	runSQL(t, db, "CREATE TABLE t1(x)")
	runSQL(t, db, "INSERT INTO t1(rowid,x) VALUES(-9223372036854775808, 'min-int'), (0, 'zero'), (9223372036854775807, 'max-int')")
	got := queryRows(t, db, "SELECT x FROM t1 WHERE rowid = +9223372036854775807.0")
	want := [][]string{{"max-int"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rowid +2^63 mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT x FROM t1 WHERE rowid = +9223372036854775808.0")
	want = [][]string{{"max-int"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rowid 2^63 mismatch:\n got %v\nwant %v", got, want)
	}
	got = queryRows(t, db, "SELECT x FROM t1 WHERE rowid = -9223372036854775809.0")
	want = [][]string{{"min-int"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rowid -2^63 mismatch:\n got %v\nwant %v", got, want)
	}
	// Beyond the boundary: no match
	got = queryRows(t, db, "SELECT x FROM t1 WHERE rowid = +9223372036854777856.0")
	if len(got) != 0 {
		t.Errorf("rowid +2^63+2048: expected no rows, got %v", got)
	}
}
