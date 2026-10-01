package frigolite

// Prepared-statement bind tests: error surfaces, integration with DML
// features (triggers, RETURNING, upsert, INSERT...SELECT, transactions),
// binding-state lifecycle, and the concurrency contract (a Stmt is not
// safe for concurrent use; the DB's own methods remain usable alongside).

import (
	"fmt"
	"sync"
	"testing"
)

// TestStmtPrepareErrors pins the prepare-time error surfaces.
func TestStmtPrepareErrors(t *testing.T) {
	db := bindTestDB(t)

	if _, err := db.Prepare("SELECT 1; SELECT 2"); err == nil {
		t.Fatal("multi-statement prepare must fail")
	} else if err.Error() != "frigolite: prepared statements support exactly one SQL statement, got 2" {
		t.Fatalf("multi-statement error: %v", err)
	}
	if _, err := db.Prepare("SELECT );"); err == nil {
		t.Fatal("syntax error prepare must fail")
	}
	// Out-of-range bind on a prepared statement.
	st, err := db.Prepare("SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Bind(2, int64(1)); err == nil {
		t.Fatal("out-of-range bind must fail")
	}
	if err := st.BindNamed("nope", int64(1)); err == nil {
		t.Fatal("unknown named bind must fail")
	}
}

// TestStmtClosedErrors pins the closed-statement behavior of the bind API.
func TestStmtClosedErrors(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(a)")
	st, err := db.Prepare("INSERT INTO t VALUES(?)")
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(int64(1)); r.Error != nil {
		t.Fatal(r.Error)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if r := st.Exec(int64(2)); r.Error == nil || r.Error.Error() != "statement is closed" {
		t.Fatalf("exec after close: %v", r.Error)
	}
	if r := st.Query(int64(2)); r.Error == nil || r.Error.Error() != "statement is closed" {
		t.Fatalf("query after close: %v", r.Error)
	}
	// DB.Close refuses while a statement is unfinalized.
	st2, err := db.Prepare("SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if err := db.Close(); err == nil {
		t.Fatal("close with unfinalized statement must fail")
	}
}

// TestStmtArgCountMismatch pins the variadic count gate.
func TestStmtArgCountMismatch(t *testing.T) {
	db := bindTestDB(t)
	st, err := db.Prepare("SELECT ? + ?")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, args := range [][]interface{}{
		{int64(1)},
		{int64(1), int64(2), int64(3)},
	} {
		if r := st.Query(args...); r.Error == nil ||
			r.Error.Error() != fmt.Sprintf("frigolite: expected %d arguments, got %d", 2, len(args)) {
			t.Fatalf("args %d: %v", len(args), r.Error)
		}
	}
	// Zero-arg call still works: unbound parameters evaluate to NULL.
	if r := st.Query(); r.Error != nil || r.Rows[0][0] != nil {
		t.Fatalf("unbound: %v %v", r.Error, r.Rows)
	}
}

// TestStmtBindOverrideLifecycle covers Bind()/Exec(args) override semantics:
// call arguments win for that call and persist as the statement's bindings
// (sqlite3_reset retains bindings too).
func TestStmtBindOverrideLifecycle(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(a, b)")
	st, err := db.Prepare("INSERT INTO t VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.BindInt64(1, 10); err != nil {
		t.Fatal(err)
	}
	if err := st.BindText(2, "keep"); err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(); r.Error != nil { // uses Bind-API values
		t.Fatal(r.Error)
	}
	if r := st.Exec(int64(20), "override"); r.Error != nil { // call args win
		t.Fatal(r.Error)
	}
	if r := st.Exec(); r.Error != nil { // override persists
		t.Fatal(r.Error)
	}
	if err := st.ClearBindings(); err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(); r.Error != nil { // cleared → NULLs
		t.Fatal(r.Error)
	}

	q := db.Query("SELECT a, b FROM t ORDER BY rowid")
	want := "[[10 keep] [20 override] [20 override] [<nil> <nil>]]"
	if got := fmt.Sprint(q.Rows); got != want {
		t.Fatalf("lifecycle rows: got %v, want %s", got, want)
	}
}

// TestStmtIntegrationRoundTrip drives INSERT/UPDATE/DELETE/SELECT through
// one prepared statement family in a transaction.
func TestStmtIntegrationRoundTrip(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)")
	bindExec(t, db, "BEGIN")

	bindRoundTripLoad(t, db)
	bindRoundTripMutate(t, db)
	bindExec(t, db, "COMMIT")
	bindRoundTripVerify(t, db)
	bindRollbackPhase(t, db)
}

// bindRoundTripLoad inserts 100 bound rows in one transaction segment.
func bindRoundTripLoad(t *testing.T, db *DB) {
	t.Helper()
	ins, err := db.Prepare("INSERT INTO t(id, v) VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	for i := 1; i <= 100; i++ {
		if r := ins.Exec(int64(i), fmt.Sprintf("v%d", i)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
}

// bindRoundTripMutate updates one bound row and deletes a bound range.
func bindRoundTripMutate(t *testing.T, db *DB) {
	t.Helper()
	upd, err := db.Prepare("UPDATE t SET v = ? WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	if r := upd.Exec("patched", int64(50)); r.Error != nil || r.Changes != 1 {
		t.Fatalf("update: %v %d", r.Error, r.Changes)
	}
	upd.Close()

	del, err := db.Prepare("DELETE FROM t WHERE id > ?")
	if err != nil {
		t.Fatal(err)
	}
	if r := del.Exec(int64(90)); r.Error != nil || r.Changes != 10 {
		t.Fatalf("delete: %v %d", r.Error, r.Changes)
	}
	del.Close()
}

// bindRoundTripVerify checks the committed row set.
func bindRoundTripVerify(t *testing.T, db *DB) {
	t.Helper()
	sel, err := db.Prepare("SELECT count(*), sum(id) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer sel.Close()
	r := sel.Query()
	if r.Rows[0][0] != int64(90) || r.Rows[0][1] != int64(int64(1+90)*90/2) {
		t.Fatalf("round trip: %v", r.Rows)
	}
}

// bindRollbackPhase pins that bound work inside a rolled-back transaction
// disappears.
func bindRollbackPhase(t *testing.T, db *DB) {
	t.Helper()
	bindExec(t, db, "BEGIN")
	bad, err := db.Prepare("INSERT INTO t(id, v) VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	if r := bad.Exec(int64(999), "temp"); r.Error != nil {
		t.Fatal(r.Error)
	}
	bad.Close()
	bindExec(t, db, "ROLLBACK")
	if n := db.Query("SELECT count(*) FROM t WHERE id = 999").Rows[0][0]; n != int64(0) {
		t.Fatalf("rollback left %v", n)
	}
}

// TestStmtTriggerAndReturning covers trigger firing and RETURNING clauses
// over bound statements.
func TestStmtTriggerAndReturning(t *testing.T) {
	db := bindTestDB(t)
	bindTriggerPhase(t, db)
	bindReturningPhase(t, db)
}

// bindTriggerPhase fires an AFTER INSERT trigger from a bound statement.
func bindTriggerPhase(t *testing.T, db *DB) {
	t.Helper()
	bindExec(t, db, "CREATE TABLE t(a INTEGER PRIMARY KEY, b)")
	bindExec(t, db, "CREATE TABLE log(a, b, tag)")
	bindExec(t, db, "CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.a, new.b, 'ins'); END")

	st, err := db.Prepare("INSERT INTO t VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Exec(int64(1), "one"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := st.Exec(int64(2), "two"); r.Error != nil {
		t.Fatal(r.Error)
	}
	st.Close()
	if n := db.Query("SELECT count(*) FROM log").Rows[0][0]; n != int64(2) {
		t.Fatalf("trigger rows: %v", n)
	}
}

// bindReturningPhase reads RETURNING rows from bound INSERT/UPDATE.
func bindReturningPhase(t *testing.T, db *DB) {
	t.Helper()
	ret, err := db.Prepare("INSERT INTO t VALUES(?, ?) RETURNING a, b")
	if err != nil {
		t.Fatal(err)
	}
	defer ret.Close()
	r := ret.Query(int64(3), "three")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(3) || r.Rows[0][1] != "three" {
		t.Fatalf("returning: %v %v", r.Error, r.Rows)
	}

	upd, err := db.Prepare("UPDATE t SET b = ? WHERE a = ? RETURNING b")
	if err != nil {
		t.Fatal(err)
	}
	defer upd.Close()
	r = upd.Query("TWO", int64(2))
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "TWO" {
		t.Fatalf("update returning: %v %v", r.Error, r.Rows)
	}
}

// TestStmtUpsertAndInsertSelect covers the upsert and INSERT...SELECT clone
// paths with bound values.
func TestStmtUpsertAndInsertSelect(t *testing.T) {
	db := bindTestDB(t)
	bindExec(t, db, "CREATE TABLE t(k INTEGER PRIMARY KEY, v)")
	bindExec(t, db, "CREATE TABLE src(k, v)")

	src, err := db.Prepare("INSERT INTO src VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if r := src.Exec(int64(i), fmt.Sprintf("s%d", i)); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	src.Close()

	// Bound INSERT ... SELECT.
	isel, err := db.Prepare("INSERT INTO t SELECT k, v FROM src WHERE k > ?")
	if err != nil {
		t.Fatal(err)
	}
	if r := isel.Exec(int64(3)); r.Error != nil || r.Changes != 2 {
		t.Fatalf("insert-select: %v %d", r.Error, r.Changes)
	}
	isel.Close()

	// Bound upsert: second pass flips to DO UPDATE with the bound value.
	up, err := db.Prepare("INSERT INTO t VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v")
	if err != nil {
		t.Fatal(err)
	}
	if r := up.Exec(int64(4), "s4-again"); r.Error != nil {
		t.Fatal(r.Error)
	}
	up.Close()
	if got := db.Query("SELECT v FROM t WHERE k = 4").Rows[0][0]; got != "s4-again" {
		t.Fatalf("upsert: %v", got)
	}
	if got := db.Query("SELECT count(*) FROM t").Rows[0][0]; got != int64(2) {
		t.Fatalf("upsert count: %v", got)
	}
}

// TestStmtConcurrentWithOtherStatements documents the concurrency contract:
// one goroutine drives a Stmt while the database family serves other
// statements from a second connection. A Stmt itself is NOT safe for
// concurrent use by multiple goroutines (bind and step state are
// unsynchronized, like sqlite3_stmt without the serialized threading mode).
func TestStmtConcurrentWithOtherStatements(t *testing.T) {
	db1, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, db := range []*DB{db1, db2} {
		if r := db.Exec("CREATE TABLE t(a INTEGER PRIMARY KEY, v)"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}

	st, err := db1.Prepare("INSERT INTO t VALUES(?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // prepared-statement loop
		defer wg.Done()
		bindConcurrentStmtLoop(t, st)
	}()
	go func() { // other statements on the connection family
		defer wg.Done()
		bindConcurrentQueryLoop(t, db2)
	}()
	wg.Wait()
	if n := db1.Query("SELECT count(*) FROM t").Rows[0][0]; n != int64(300) {
		t.Fatalf("concurrent loop rows: %v", n)
	}
}

// bindConcurrentStmtLoop runs the prepared statement's bound Exec loop.
func bindConcurrentStmtLoop(t *testing.T, st *Stmt) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if r := st.Exec(int64(i), fmt.Sprintf("s%d", i)); r.Error != nil {
			t.Errorf("stmt exec: %v", r.Error)
			return
		}
	}
}

// bindConcurrentQueryLoop runs plain statements on the sibling connection.
func bindConcurrentQueryLoop(t *testing.T, db2 *DB) {
	t.Helper()
	for i := 0; i < 300; i++ {
		r := db2.Query(fmt.Sprintf("SELECT count(*) FROM t WHERE a > %d", i))
		if r.Error != nil {
			t.Errorf("other query: %v", r.Error)
			return
		}
	}
}
