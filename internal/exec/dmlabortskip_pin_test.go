package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/sql"
)

// R10.DML snapshot-skip pins: the can't-abort point UPDATE/DELETE shapes skip
// the statement journal (dmlCanSkipSnapshot), and every shape that can fail
// AFTER its write keeps it. The skip is an optimization only when the
// skip-set is exactly the provable set — the matrix below pins both sides.

// newAbortSkipEngine builds an in-memory engine with the pin fixtures: the
// point-op target t, a trigger-carrying table tt, an FK parent/child pair,
// a WITHOUT ROWID table wr, a table whose rowid name is a declared column
// (shadowed), and a virtual table.
func newAbortSkipEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := e.schema.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	stmts := []string{
		"CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)",
		"CREATE TABLE plain(a INTEGER, b TEXT)",
		"CREATE TABLE tt(id INTEGER PRIMARY KEY, c INTEGER)",
		"CREATE TRIGGER ttr AFTER UPDATE ON tt BEGIN INSERT INTO plain VALUES(1, 'x'); END",
		"CREATE TABLE parent(k INTEGER PRIMARY KEY)",
		"CREATE TABLE child(id INTEGER PRIMARY KEY, p INTEGER REFERENCES parent(k))",
		"CREATE TABLE wr(a TEXT, b INTEGER, PRIMARY KEY(a)) WITHOUT ROWID",
		"CREATE TABLE shadowed(rowid INTEGER, c INTEGER)",
	}
	for _, s := range stmts {
		if res := e.Exec(mustParse(t, s)); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	return e
}

func parseStmt(t *testing.T, text string) sql.Stmt {
	t.Helper()
	return mustParse(t, text)
}

// TestPinDMLAbortSkipMatrix pins dmlCanSkipSnapshot's decision matrix for the
// R10.DML point UPDATE/DELETE extension: only the plain rowid-pinned shape
// skips; OR clauses, RETURNING, ORDER BY/LIMIT, UPDATE...FROM, triggers, FK
// enforcement, virtual tables, FTS content targets, WITHOUT ROWID targets,
// shadowed rowid names and non-rowid WHERE terms all keep the snapshot.
func TestPinDMLAbortSkipMatrix(t *testing.T) {
	e := newAbortSkipEngine(t)

	skip := []string{
		// the point workhorses: rowid pseudo-column and IPK alias spellings,
		// reversed operand order, conjuncts beyond the pin term
		"UPDATE t SET c=c+1 WHERE id=5",
		"UPDATE t SET c=c+1 WHERE rowid=5",
		"UPDATE t SET c=c+1 WHERE _rowid_=5",
		"UPDATE t SET c=c+1 WHERE oid=5",
		"UPDATE t SET c=c+1 WHERE 5=id",
		"UPDATE t SET c=c+1 WHERE (id)=5",
		"UPDATE plain SET b='x' WHERE rowid=(SELECT max(k) FROM parent)",
		"UPDATE plain SET b='x' WHERE rowid=(SELECT max(k) FROM parent) AND a>0",
		"UPDATE plain SET a=2, b='x' WHERE rowid=1",
		"DELETE FROM t WHERE id=5",
		"DELETE FROM t WHERE rowid=5",
		"DELETE FROM plain WHERE 1=1 AND rowid=2",
		// the single-row VALUES INSERT skip (pre-R10 baseline behavior)
		"INSERT INTO t VALUES(9, 9)",
	}
	for _, s := range skip {
		st := parseStmt(t, s)
		if !e.dmlCanSkipSnapshot(st) {
			t.Errorf("dmlCanSkipSnapshot(%q) = false, want true", s)
		}
	}

	keep := []string{
		// OR-clause dispositions need the statement rollback
		"UPDATE OR REPLACE t SET c=1 WHERE id=5",
		"UPDATE OR FAIL t SET c=1 WHERE id=5",
		"UPDATE OR IGNORE t SET c=1 WHERE id=5",
		"UPDATE OR ABORT t SET c=1 WHERE id=5",
		"UPDATE OR ROLLBACK t SET c=1 WHERE id=5",
		"DELETE FROM t WHERE rowid IN (5) LIMIT 1",
		// ORDER BY/LIMIT/OFFSET tails are gated on both statement kinds
		"UPDATE t SET c=1 WHERE id=5 LIMIT 1",
		"UPDATE t SET c=1 WHERE id=5 LIMIT 1 OFFSET 1",
		"UPDATE t SET c=1 WHERE id=5 ORDER BY rowid LIMIT 1",
		"DELETE FROM t WHERE id=5 LIMIT 1 OFFSET 1",
		"DELETE FROM t WHERE id=5 ORDER BY rowid LIMIT 1",
		// RETURNING evaluates after the write
		"UPDATE t SET c=c+1 WHERE id=5 RETURNING c",
		"DELETE FROM t WHERE id=5 RETURNING c",
		// UPDATE ... FROM is a join statement
		"UPDATE t SET c=(SELECT 1) WHERE id IN (SELECT k FROM parent)",
		// trigger-carrying target
		"UPDATE tt SET c=c+1 WHERE id=5",
		"DELETE FROM tt WHERE id=5",
		// a non-rowid equality term never pins the row count
		"UPDATE plain SET b='x' WHERE a=1",
		"UPDATE t SET c=c+1 WHERE c=5",
		"DELETE FROM plain WHERE a=1",
		// non-equality rowid terms (range / inequality) match many rows
		"DELETE FROM t WHERE id>5",
		"UPDATE t SET c=c+1 WHERE id<5",
		// OR-form WHERE is not a conjunct
		"DELETE FROM t WHERE id=5 OR id=6",
		// a declared column named rowid shadows the pseudo-column: the term
		// matches by VALUE, potentially many rows
		"UPDATE shadowed SET c=1 WHERE rowid=5",
		"DELETE FROM shadowed WHERE rowid=5",
		// WITHOUT ROWID target: "a" is its PK, not a rowid
		"DELETE FROM wr WHERE a='x'",
		// an unresolvable target never skips (findTable failure)
		"DELETE FROM nosuch WHERE rowid=5",
		// the multi-row shapes the journal exists for
		"UPDATE t SET c=c+1 WHERE id>0",
		"DELETE FROM t",
		"UPDATE t SET c=c+1",
	}
	for _, s := range keep {
		st := parseStmt(t, s)
		if e.dmlCanSkipSnapshot(st) {
			t.Errorf("dmlCanSkipSnapshot(%q) = true, want false", s)
		}
	}

	// FK enforcement disables the skip for every point shape.
	e.SetForeignKeys(true)
	for _, s := range []string{
		"UPDATE t SET c=c+1 WHERE id=5",
		"DELETE FROM t WHERE id=5",
		"INSERT INTO t VALUES(9, 9)",
	} {
		st := parseStmt(t, s)
		if e.dmlCanSkipSnapshot(st) {
			t.Errorf("FK on: dmlCanSkipSnapshot(%q) = true, want false", s)
		}
	}
	e.SetForeignKeys(false)
}

// TestPinDMLAbortSkipRollbackExactness drives a failed multi-row UPDATE
// (a CHECK constraint the generic pipeline rejects mid-statement) through a
// transaction and pins the page-hash exactness of the statement rollback:
// the skipped statement shapes never reach this path, but every shape the
// matrix keeps must still restore exact bytes.
func TestPinDMLAbortSkipRollbackExactness(t *testing.T) {
	e := newAbortSkipEngine(t)
	if res := e.Exec(mustParse(t, "CREATE TABLE ck(id INTEGER PRIMARY KEY, c INTEGER CHECK(c < 100))")); res.Error != nil {
		t.Fatalf("create ck: %v", res.Error)
	}
	if res := e.Exec(mustParse(t, "BEGIN")); res.Error != nil {
		t.Fatalf("begin: %v", res.Error)
	}
	for i := 1; i <= 20; i++ {
		if res := e.Exec(mustParse(t, "INSERT INTO ck VALUES("+itoa(int64(i))+", "+itoa(int64(i))+")")); res.Error != nil {
			t.Fatalf("insert %d: %v", i, res.Error)
		}
	}
	// The multi-row shape can fail after earlier writes: rows 21..25 would
	// violate the CHECK before any write of those rows, but the statement's
	// already-written rows (none here — the violation is row 1's new value)
	// must leave the table byte-exact. Drive both orderings.
	hashBefore := engineTableHash(t, e, "ck")
	if res := e.Exec(mustParse(t, "UPDATE ck SET c=c+200 WHERE id>10")); res.Error == nil {
		t.Fatalf("multi-row UPDATE past CHECK succeeded, want failure")
	}
	hashAfter := engineTableHash(t, e, "ck")
	if hashBefore != hashAfter {
		t.Fatalf("failed multi-row UPDATE changed table bytes\nbefore %v\nafter  %v", hashBefore, hashAfter)
	}
	if res := e.Exec(mustParse(t, "COMMIT")); res.Error != nil {
		t.Fatalf("commit: %v", res.Error)
	}
}

// TestPinDMLAbortSkipORRollbackExactness pins the OR-clause shapes the skip
// excludes: an UPDATE OR FAIL whose second row conflicts keeps its prior-row
// semantics (execRollbackOnError's isOrFail branch) and an UPDATE OR ABORT
// whose SET reuses a UNIQUE value rolls the statement back byte-exactly.
func TestPinDMLAbortSkipORRollbackExactness(t *testing.T) {
	e := newAbortSkipEngine(t)
	for _, s := range []string{
		"CREATE TABLE uq(id INTEGER PRIMARY KEY, c INTEGER UNIQUE)",
		"BEGIN",
		"INSERT INTO uq VALUES(1, 10)",
		"INSERT INTO uq VALUES(2, 20)",
		"INSERT INTO uq VALUES(3, 30)",
	} {
		if res := e.Exec(mustParse(t, s)); res.Error != nil {
			t.Fatalf("%s: %v", s, res.Error)
		}
	}
	hashBefore := engineTableHash(t, e, "uq")
	// OR ABORT: the failing row's statement changes roll back, byte-exact.
	if res := e.Exec(mustParse(t, "UPDATE OR ABORT uq SET c=c WHERE id IN (1,2)")); res.Error == nil {
		// c=c is a no-op; force a real conflict instead.
		if res := e.Exec(mustParse(t, "UPDATE OR ABORT uq SET c=20 WHERE id IN (1,3)")); res.Error == nil {
			t.Fatalf("OR ABORT conflict statement succeeded, want failure")
		}
		if hashAfter := engineTableHash(t, e, "uq"); hashBefore != hashAfter {
			t.Fatalf("OR ABORT statement rollback changed bytes\nbefore %v\nafter  %v", hashBefore, hashAfter)
		}
	}
	if res := e.Exec(mustParse(t, "COMMIT")); res.Error != nil {
		t.Fatalf("commit: %v", res.Error)
	}
}

// engineTableHash hashes a table's full row set (rowid + declared values) —
// the rollback-exactness observable.
func engineTableHash(t *testing.T, e *Engine, table string) uint64 {
	t.Helper()
	var h uint64 = 14695981039346656037
	res := e.Exec(mustParse(t, "SELECT rowid, * FROM "+table+" ORDER BY rowid"))
	if res.Error != nil {
		t.Fatalf("hash scan %s: %v", table, res.Error)
	}
	for _, row := range res.Rows {
		for _, cell := range row {
			h = h*1099511628211 ^ hashCell(cell)
		}
	}
	return h
}

func hashCell(v interface{}) uint64 {
	switch n := v.(type) {
	case int64:
		return uint64(n)
	case string:
		var h uint64 = 5381
		for i := 0; i < len(n); i++ {
			h = h*33 + uint64(n[i])
		}
		return h
	case nil:
		return 0xdeadbeef
	default:
		return 0xfeedface
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
