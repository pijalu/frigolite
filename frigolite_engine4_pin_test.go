package frigolite_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// Engine4 pins for the §5d regeneration-sync oracle bugs. Each test pins one
// sqlite3-verified contract that a testgen package (trigger1, misc1, alter,
// without_rowid3) depends on.

// flatRows renders a Query result the way the tcl2go flatten() helper does
// (cells space-joined within a row, rows newline-joined).
func flatRows(q *frigolite.Result) string {
	rows := make([]string, 0, len(q.Rows))
	for _, r := range q.Rows {
		cells := make([]string, 0, len(r))
		for _, c := range r {
			cells = append(cells, fmt.Sprintf("%v", c))
		}
		rows = append(rows, strings.Join(cells, " "))
	}
	return strings.Join(rows, "\n")
}

// TestEngine4TempTriggerScoping pins trigger1-10.4: a TEMP trigger is bound to
// the specific table (and schema) it was created ON (trigger.c pTabSchema).
// INSERT INTO temp.t4 fires exactly ONE temp trigger — the one ON temp.t4 —
// never the temp triggers ON main.t4 or aux.t4.
func TestEngine4TempTriggerScoping(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Each trigger inserts its own name so the firing set is observable.
	// (The aux.t4 leg of trigger1-10.x is covered by the testgen suite; the
	// main/temp pair pins the same pTabSchema scoping semantics.)
	steps := []string{
		"CREATE TABLE main.t4(a, b, c)",
		"CREATE TABLE temp.t4(a, b, c)",
		"CREATE TABLE log(name)",
		"CREATE TEMP TRIGGER trig1 AFTER INSERT ON main.t4 BEGIN INSERT INTO log VALUES('trig1'); END",
		"CREATE TEMP TRIGGER trig2 AFTER INSERT ON temp.t4 BEGIN INSERT INTO log VALUES('trig2'); END",
		"INSERT INTO main.t4 VALUES(1, 2, 3)",
		"INSERT INTO temp.t4 VALUES(4, 5, 6)",
	}
	for _, s := range steps {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	q := db.Query("SELECT name FROM log")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if got, want := flatRows(q), "trig1\ntrig2"; got != want {
		t.Fatalf("temp trigger scoping: got [%s] want [%s]", got, want)
	}
}

// TestEngine4SumIntegerResult pins misc1-2.2/2.3: sum() classifies each
// input with sqlite3_value_numeric_type (func.c sumStep), so numeric text in
// a TEXT column feeds the exact integer path and sumFinalize returns an
// INTEGER (result_int64) unless a non-integer input or an int64 overflow
// promoted the accumulator to the Kahan-Babuška-Neumaier double sum. BLOB
// and non-numeric TEXT count as 0.0 (sqlite3_value_double) — still counted,
// so the result is REAL, never NULL.
func TestEngine4SumIntegerResult(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if r := db.Exec("CREATE TABLE t(x)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(8), (6), (4), (3)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	q := db.Query("SELECT sum(x), typeof(sum(x)) FROM t")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if v, ok := q.Rows[0][0].(int64); !ok || v != 21 || q.Rows[0][1] != "integer" {
		t.Fatalf("sum of integers: got %v %v want 21 integer", q.Rows[0][0], q.Rows[0][1])
	}

	// misc1-2.2: a TEXT column holding numeric text sums as INTEGER.
	if r := db.Exec("DELETE FROM t; INSERT INTO t VALUES(2), (6)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	q = db.Query("SELECT sum(x), typeof(sum(x)) FROM t")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if v, ok := q.Rows[0][0].(int64); !ok || v != 8 || q.Rows[0][1] != "integer" {
		t.Fatalf("sum over numeric text: got %v %v want 8 integer", q.Rows[0][0], q.Rows[0][1])
	}

	// A REAL input promotes the accumulator: 1.5 + 2 = 3.5 real.
	if r := db.Exec("DELETE FROM t; INSERT INTO t VALUES(1.5), (2)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	q = db.Query("SELECT sum(x), typeof(sum(x)) FROM t")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if v, ok := q.Rows[0][0].(float64); !ok || v != 3.5 || q.Rows[0][1] != "real" {
		t.Fatalf("sum with real input: got %v %v want 3.5 real", q.Rows[0][0], q.Rows[0][1])
	}

	// Non-numeric text and BLOBs contribute 0.0 but are counted: sum over
	// '1','2.0','abc',x'4142' is 3.0 real, avg is 3.0/4 (oracle-verified).
	if r := db.Exec("DELETE FROM t; INSERT INTO t VALUES('1'),('2.0'),('abc'),(x'4142')"); r.Error != nil {
		t.Fatal(r.Error)
	}
	q = db.Query("SELECT sum(x), typeof(sum(x)), total(x), avg(x) FROM t")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	row := q.Rows[0]
	if v, ok := row[0].(float64); !ok || v != 3 || row[1] != "real" {
		t.Fatalf("sum over mixed text/blob: got %v %v want 3 real", row[0], row[1])
	}
	if v, ok := row[2].(float64); !ok || v != 3 {
		t.Fatalf("total over mixed text/blob: got %v want 3", row[2])
	}
	if v, ok := row[3].(float64); !ok || v != 0.75 {
		t.Fatalf("avg over mixed text/blob: got %v want 0.75", row[3])
	}

	// An int64 overflow promotes to the double sum; an unabsorbed overflow
	// raises "integer overflow" at finalize (func.c sumFinalize), while a
	// later non-integer input absorbs it into a REAL result (oracle: sum
	// over 2^63-1, 1 errors; over 2^63-1, 1, 2.5 yields real 9.223372e18).
	// A single max-int64 input never overflows: sum stays INTEGER.
	if r := db.Exec("DELETE FROM t; INSERT INTO t VALUES(9223372036854775807)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if q := db.Query("SELECT typeof(sum(x)) FROM t"); q.Error != nil || q.Rows[0][0] != "integer" {
		t.Fatalf("single max-int64 sum: got %v/%v want integer", q.Rows, q.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if q := db.Query("SELECT sum(x) FROM t"); q.Error == nil || !strings.Contains(q.Error.Error(), "integer overflow") {
		t.Fatalf("unabsorbed overflow: got %v want integer overflow error", q.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(2.5)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if q := db.Query("SELECT typeof(sum(x)) FROM t"); q.Error != nil || q.Rows[0][0] != "real" {
		t.Fatalf("absorbed overflow: got %v/%v want real", q.Rows, q.Error)
	}
}

// TestEngine4DropTempTableCascadesTriggers pins alter-3.3.8: DROP TABLE drops
// the table's triggers from the same schema — including a TEMP table's TEMP
// triggers (build.c sqlite3CodeDropTable walks sqlite3TriggerList and drops
// each from its owning schema). temp.sqlite_master must be empty afterwards.
func TestEngine4DropTempTableCascadesTriggers(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"CREATE TEMP TABLE tbl3(a, b, c)",
		"CREATE TEMP TRIGGER trig1 AFTER INSERT ON tbl3 BEGIN SELECT 1; END",
		"CREATE TEMP TRIGGER trig2 AFTER UPDATE ON tbl3 BEGIN SELECT 1; END",
		"DROP TABLE temp.tbl3",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	q := db.Query("SELECT name, type, tbl_name FROM temp.sqlite_master")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if got, want := flatRows(q), ""; got != want {
		t.Fatalf("temp.sqlite_master after DROP TABLE: got [%s] want []", got)
	}
}

// TestEngine4FkRestrictBeforeAfterTrigger pins without_rowid3-12.2.2-12.2.4:
// an ON DELETE RESTRICT FK violation aborts the DELETE before the AFTER
// DELETE trigger runs (fk.c: the RESTRICT action subprogram sits between
// OP_Delete and the after-trigger program), so the trigger's repair-insert
// never happens. An immediate NO ACTION FK instead runs as an
// end-of-statement check, so the trigger's repair-insert CAN satisfy it —
// and the deleted rows must really be gone (a WITHOUT ROWID table whose PK
// carries a collation must not silently skip the delete and leave the
// trigger's inserts as duplicates).
func TestEngine4FkRestrictBeforeAfterTrigger(t *testing.T) {
	dir := t.TempDir()
	db, err := frigolite.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE t1(x COLLATE NOCASE PRIMARY KEY) WITHOUT rowid",
		"CREATE TRIGGER tt1 AFTER DELETE ON t1 WHEN EXISTS (SELECT 1 FROM t2 WHERE old.x = y) BEGIN INSERT INTO t1 VALUES(old.x); END",
		"CREATE TABLE t2(y REFERENCES t1)",
		"INSERT INTO t1 VALUES('A')",
		"INSERT INTO t1 VALUES('B')",
		"INSERT INTO t2 VALUES('a')",
		"INSERT INTO t2 VALUES('b')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	// 12.2.2: the NO ACTION delete succeeds — the trigger's re-inserted
	// parent keys satisfy the end-of-statement check, and the rows are
	// deleted exactly once (no duplicates).
	if r := db.Exec("DELETE FROM t1"); r.Error != nil {
		t.Fatalf("NO ACTION delete: %v", r.Error)
	}
	if q := db.Query("SELECT * FROM t1; "); q.Error != nil {
		t.Fatal(q.Error)
	} else if got, want := flatRows(q), "A\nB"; got != want {
		t.Fatalf("after NO ACTION delete: got [%s] want [A B]", got)
	}
	// 12.2.3/12.2.4: the RESTRICT delete errors and leaves the tables
	// untouched — the AFTER trigger never fired.
	for _, s := range []string{
		"DROP TABLE t2",
		"CREATE TABLE t2(y REFERENCES t1 ON DELETE RESTRICT)",
		"INSERT INTO t2 VALUES('a')",
		"INSERT INTO t2 VALUES('b')",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Exec("DELETE FROM t1"); r.Error == nil {
		t.Fatalf("DELETE with RESTRICT violation: expected error")
	}
	q := db.Query("SELECT * FROM t1; SELECT * FROM t2")
	if q.Error != nil {
		t.Fatal(q.Error)
	}
	if got, want := flatRows(q), "A\nB\na\nb"; got != want {
		t.Fatalf("after RESTRICT delete: got [%s] want [%s]", got, want)
	}
}
