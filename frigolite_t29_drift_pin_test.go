package frigolite

import (
	"strconv"
	"strings"
	"testing"
)

// T29 repro: FULL-SUITE-DRIFT.T29-execqfix. Each test mirrors a latent engine
// gap surfaced when the regenerated testgen assertions (3fcb5cea4) became
// active. Expected values are sqlite3-CLI-verified.

func t29Open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir() + "/t29.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func t29Rows(t *testing.T, db *DB, q string) [][]interface{} {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("%s: %v", q, r.Error)
	}
	return r.Rows
}

func t29Render(rows [][]interface{}) string {
	var sb strings.Builder
	for i, row := range rows {
		if i > 0 {
			sb.WriteString(";")
		}
		for j, v := range row {
			if j > 0 {
				sb.WriteString("|")
			}
			switch x := v.(type) {
			case nil:
				sb.WriteString("{}")
			case string:
				sb.WriteString(x)
			case int64:
				sb.WriteString(strconv.FormatInt(x, 10))
			case float64:
				sb.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
			default:
				sb.WriteString("?")
			}
		}
	}
	return sb.String()
}

func t29Exec(t *testing.T, db *DB, q string) error {
	t.Helper()
	res := db.Exec(q)
	return res.Error
}

// 1. count-2.9a: aggregate query with a false HAVING must return zero rows.
func TestT29_HavingFiltersAggregates(t *testing.T) {
	db := t29Open(t)
	if err := t29Exec(t, db, "CREATE TABLE t2(a, b)"); err != nil {
		t.Fatal(err)
	}
	rows := t29Rows(t, db, "SELECT count(*) FROM t2 HAVING count(*)>1")
	if len(rows) != 0 {
		t.Fatalf("HAVING count(*)>1 must filter; got %s", t29Render(rows))
	}
	rows = t29Rows(t, db, "SELECT count(*) FROM t2 HAVING count(*)<10")
	if got := t29Render(rows); got != "0" {
		t.Fatalf("HAVING count(*)<10 must pass [0]; got %s", got)
	}
}

// 2. collate4-4.10/4.13/4.14: scalar max(X,Y) uses the leftmost argument's
// collating sequence (column collations included).
func TestT29_MinMaxArgCollation(t *testing.T) {
	db := t29Open(t)
	db.RegisterCollation("TEXT", func(a, b string) int { return strings.Compare(a, b) })
	db.RegisterCollation("numeric", func(a, b string) int {
		if a == b {
			return 0
		}
		af, aerr := strconv.ParseFloat(a, 64)
		bf, berr := strconv.ParseFloat(b, 64)
		if aerr == nil && berr == nil {
			if af < bf {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if err := t29Exec(t, db, "CREATE TABLE collate4t1(a COLLATE TEXT, b COLLATE NUMERIC)"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"INSERT INTO collate4t1 VALUES('11', '101')",
		"INSERT INTO collate4t1 VALUES('101', '11')",
	} {
		if err := t29Exec(t, db, s); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ q, want string }{
		{"SELECT max(a, b) FROM collate4t1", "11;11"},
		{"SELECT max(b, a) FROM collate4t1", "101;101"},
		{"SELECT max(b, '101') FROM collate4t1", "101;101"},
		{"SELECT max('101', b) FROM collate4t1", "101;101"},
	}
	for _, tc := range cases {
		if got := t29Render(t29Rows(t, db, tc.q)); got != tc.want {
			t.Errorf("%s: got [%s] want [%s]", tc.q, got, tc.want)
		}
	}
}

// 3. memdb-6.6: ORDER BY c DESC with an index on (c) must produce
// descending rowid within equal keys (reverse index scan).
func TestT29_OrderByIndexDescTies(t *testing.T) {
	db := t29Open(t)
	for _, s := range []string{
		"CREATE TABLE t2(a,b,c)",
		"INSERT INTO t2 VALUES(1,2,1)",
		"INSERT INTO t2 VALUES(2,3,2)",
		"INSERT INTO t2 VALUES(3,4,1)",
		"INSERT INTO t2 VALUES(4,5,4)",
		"CREATE INDEX i2 ON t2(c)",
	} {
		if err := t29Exec(t, db, s); err != nil {
			t.Fatal(err)
		}
	}
	if got := t29Render(t29Rows(t, db, "SELECT a FROM t2 ORDER BY c DESC")); got != "4;2;3;1" {
		t.Fatalf("ORDER BY c DESC: got [%s] want [4|2;3;1 -> 4;2;3;1]", got)
	}
}

// 4. trigger2-6.1f/g/h: the outer statement's conflict resolution overrides
// the trigger body's ON CONFLICT clause.
func TestT29_TriggerConflictOverride(t *testing.T) {
	db := t29Open(t)
	for _, s := range []string{
		"CREATE TABLE tbl (a primary key, b, c)",
		"CREATE TRIGGER ai_tbl AFTER INSERT ON tbl BEGIN\n" +
			"  INSERT OR IGNORE INTO tbl values (new.a, 0, 0);\n" +
			"END",
	} {
		if err := t29Exec(t, db, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := t29Exec(t, db, "INSERT INTO tbl values (1, 2, 3)"); err != nil {
		t.Fatal(err)
	}
	// OR ABORT: trigger conflict -> error, statement rolled back.
	err := t29Exec(t, db, "INSERT OR ABORT INTO tbl values (2, 2, 3)")
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: tbl.a") {
		t.Fatalf("OR ABORT: want UNIQUE error, got %v", err)
	}
	if got := t29Render(t29Rows(t, db, "SELECT * FROM tbl")); got != "1|2|3" {
		t.Fatalf("after OR ABORT: got [%s] want [1|2|3]", got)
	}
	// OR FAIL: error, but statement's own prior changes are kept.
	err = t29Exec(t, db, "INSERT OR FAIL INTO tbl values (2, 2, 3)")
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: tbl.a") {
		t.Fatalf("OR FAIL: want UNIQUE error, got %v", err)
	}
	if got := t29Render(t29Rows(t, db, "SELECT * FROM tbl")); got != "1|2|3;2|2|3" {
		t.Fatalf("after OR FAIL: got [%s] want [1|2|3;2|2|3]", got)
	}
	// OR REPLACE: the trigger's insert runs with REPLACE and swaps the row.
	if err := t29Exec(t, db, "INSERT OR REPLACE INTO tbl values (2, 2, 3)"); err != nil {
		t.Fatal(err)
	}
	if got := t29Render(t29Rows(t, db, "SELECT * FROM tbl")); got != "1|2|3;2|0|0" {
		t.Fatalf("after OR REPLACE: got [%s] want [1|2|3;2|0|0]", got)
	}
}

// 5. alter3-5.x: CREATE TABLE AS SELECT stores the expanded column-list SQL;
// ALTER TABLE ADD COLUMN rewrites it and bumps schema_version.
func TestT29_CTASSqlTextAndAlter(t *testing.T) {
	db := t29Open(t)
	dir := t.TempDir()
	for _, s := range []string{
		"CREATE TABLE t1(a, b)",
		"INSERT INTO t1 VALUES(1, 'one')",
		"INSERT INTO t1 VALUES(2, 'two')",
		"ATTACH '" + dir + "/test2.db' AS aux",
		"CREATE TABLE aux.t1 AS SELECT * FROM t1",
		"PRAGMA aux.schema_version = 30",
	} {
		if err := t29Exec(t, db, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if got := t29Render(t29Rows(t, db, "SELECT sql FROM aux.sqlite_master")); got != "CREATE TABLE t1(a,b)" {
		t.Fatalf("CTAS stored sql: got [%s] want [CREATE TABLE t1(a,b)]", got)
	}
	if err := t29Exec(t, db, "ALTER TABLE aux.t1 ADD COLUMN c VARCHAR(128)"); err != nil {
		t.Fatal(err)
	}
	if got := t29Render(t29Rows(t, db, "SELECT sql FROM aux.sqlite_master")); got != "CREATE TABLE t1(a,b, c VARCHAR(128))" {
		t.Fatalf("after ADD COLUMN: got [%s] want [CREATE TABLE t1(a,b, c VARCHAR(128))]", got)
	}
	if got := t29Render(t29Rows(t, db, "SELECT * FROM aux.t1")); got != "1|one|{};2|two|{}" {
		t.Fatalf("aux.t1 rows: got [%s] want [1|one|{};2|two|{}]", got)
	}
	if got := t29Render(t29Rows(t, db, "SELECT * FROM t1")); got != "1|one;2|two" {
		t.Fatalf("main t1 rows: got [%s] want [1|one;2|two]", got)
	}
}
