package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/vtab"
)

// echoContractDB opens a connection with the echo module registered
// (register_echo_module parity: the module is per-connection and absent by
// default).
func echoContractDB(t *testing.T) *frigolite.DB {
	t.Helper()
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.RegisterEchoModule()
	return db
}

// TestSQLiteEchoVtabLifecyclePin pins the echo module's constructor contract
// (src/test8.c via vtab.c vtabCallConstructor) and the CREATE VIRTUAL TABLE
// error ordering (SQLite raises reserved-name, IF-NOT-EXISTS and already-
// exists errors before the module lookup; oracle-verified):
//
//	CREATE VIRTUAL TABLE t1 USING echo            → module registered on this
//	connection but the constructor never declares a schema:
//	"vtable constructor did not declare schema: t1"
//
// plus the xUpdate error wrap ("echo-vtab-error: %s", test8.c echoError) and
// the text-rowid "datatype mismatch" (vtab1-15.4).
func TestSQLiteEchoVtabConstructorPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Without registration the module is unknown on this connection.
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING echo;"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "no such module: echo") {
		t.Fatalf("unregistered echo: got %v", res.Error)
	}
	db.RegisterEchoModule()

	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING echo;"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "vtable constructor did not declare schema: t1") {
		t.Fatalf("no-schema constructor: got %v", res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING echo(no_such_table);"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "vtable constructor failed: t1") {
		t.Fatalf("missing-source constructor: got %v", res.Error)
	}
	if res := db.Exec("CREATE VIRTUAL TABLE sqlite_master USING echo;"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "object name reserved for internal use: sqlite_master") {
		t.Fatalf("reserved name: got %v", res.Error)
	}

	// IF NOT EXISTS turns an existing same-name object into a no-op, even a
	// real table (vtab1-1.8.2; oracle: succeeds even with an unknown module).
	mustExec := func(sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	mustExec("CREATE TABLE treal(a, b, c)")
	if res := db.Exec("CREATE VIRTUAL TABLE treal USING echo(treal);"); res.Error == nil ||
		!strings.Contains(res.Error.Error(), "table treal already exists") {
		t.Fatalf("duplicate name: got %v", res.Error)
	}
	mustExec("CREATE VIRTUAL TABLE IF NOT EXISTS treal USING echo(treal)")

	// xUpdate failures report through the module's error prefix
	// (vtab1.12-2: "echo-vtab-error: UNIQUE constraint failed: c.a").
	mustExec("CREATE TABLE b(a, b, c); CREATE TABLE c(a UNIQUE, b, c);")
	mustExec("INSERT INTO b VALUES(1,'A','B'); INSERT INTO b VALUES(2,'C','D'); INSERT INTO b VALUES(3,'E','F');")
	mustExec("INSERT INTO c VALUES(3,'G','H'); CREATE VIRTUAL TABLE echo_c USING echo(c);")
	res := db.Exec("INSERT INTO echo_c SELECT * FROM b")
	if res.Error == nil || res.Error.Error() != "echo-vtab-error: UNIQUE constraint failed: c.a" {
		t.Fatalf("echo write-through wrap: got %v", res.Error)
	}
	// The failed statement is atomic: c keeps only its original row.
	r := db.Query("SELECT * FROM c")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(3) {
		t.Fatalf("c after failed echo insert: %v %v", r.Rows, r.Error)
	}

	// A non-integer explicit rowid is a datatype mismatch (vtab1-15.4).
	mustExec("CREATE TABLE t9(a, b, c); CREATE VIRTUAL TABLE echo_t1 USING echo(t9);")
	if res := db.Exec("INSERT INTO echo_t1(rowid) VALUES('new rowid');"); res.Error == nil ||
		res.Error.Error() != "datatype mismatch" {
		t.Fatalf("text rowid: got %v", res.Error)
	}
}

// TestSQLiteEchoVtabBestIndexMalfunctionPin pins the xBestIndex malfunction
// contract (where.c:4366 "%s.xBestIndex malfunction" via test8.c
// echoBestIndex's echo_module_ignore_usable handling): when the TCL-side
// flag makes the echo module claim constraints whose usable flag is 0, the
// planner names the virtual table in the error. The join's ON terms are the
// offered constraints, and the left (outer-loop) operand plans first, so
// FROM ab NATURAL JOIN bc reports ab and the reverse reports bc
// (vtab6-11.4.1/11.4.2).
func TestSQLiteEchoVtabBestIndexMalfunctionPin(t *testing.T) {
	db := echoContractDB(t)
	mustExec := func(sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	mustExec("CREATE TABLE ab_r(a, b); CREATE TABLE bc_r(b, c);")
	mustExec("CREATE VIRTUAL TABLE ab USING echo(ab_r); CREATE VIRTUAL TABLE bc USING echo(bc_r);")
	mustExec("INSERT INTO ab VALUES(1, 2); INSERT INTO bc VALUES(2, 3);")

	r := db.Query("SELECT a, b, c FROM ab NATURAL JOIN bc")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(1) {
		t.Fatalf("natural join: %v %v", r.Error, r.Rows)
	}

	mustExec("CREATE INDEX ab_i ON ab_r(b); CREATE INDEX bc_i ON bc_r(b);")
	vtab.TclVarSet("echo_module_ignore_usable", "", "1")
	defer vtab.TclVarSet("echo_module_ignore_usable", "", "")
	if res := db.Exec("SELECT a, b, c FROM ab NATURAL JOIN bc"); res.Error == nil ||
		res.Error.Error() != "ab.xBestIndex malfunction" {
		t.Fatalf("malfunction (ab first): got %v", res.Error)
	}
	if res := db.Exec("SELECT a, b, c FROM bc NATURAL JOIN ab"); res.Error == nil ||
		res.Error.Error() != "bc.xBestIndex malfunction" {
		t.Fatalf("malfunction (bc first): got %v", res.Error)
	}
	vtab.TclVarSet("echo_module_ignore_usable", "", "")
	r = db.Query("SELECT a, b, c FROM ab NATURAL JOIN bc")
	if r.Error != nil || len(r.Rows) != 1 {
		t.Fatalf("natural join after flag cleared: %v %v", r.Error, r.Rows)
	}
}

// authRecorder is the vtab3.test auth proc: a recording authorizer with a
// deny counter.
type authRecorder struct {
	log   []string
	failN int
}

// Authorize implements auth.Authorizer with vtab3.test's semantics: filtered
// actions pass, the action is logged, the counter decrements, and hitting
// zero denies.
func (a *authRecorder) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {
	switch action {
	case auth.ActionRead, auth.ActionUpdate, auth.ActionSelect, auth.ActionPragma:
		return auth.ResultOK
	}
	a.log = append(a.log, action.String(), arg1, arg2, arg3, arg4)
	a.failN--
	if a.failN == 0 {
		return auth.ResultDeny
	}
	return auth.ResultOK
}

// TestSQLiteVtabAuthorizerPin pins the authorizer actions of vtab DDL
// (vtab3-1.2/1.3): CREATE VIRTUAL TABLE raises SQLITE_INSERT on sqlite_master
// then SQLITE_CREATE_VTABLE with the module name; DROP TABLE of a vtab raises
// SQLITE_DELETE sqlite_master, SQLITE_DROP_VTABLE, SQLITE_DELETE <vtab>,
// SQLITE_DELETE sqlite_master. A deny leaves the schema unchanged
// (vtab3-1.4/1.7).
func TestSQLiteVtabAuthorizerPin(t *testing.T) {
	db := echoContractDB(t)
	mustExec := func(sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	mustExec("CREATE TABLE elephant(name VARCHAR(32), color VARCHAR(16), age INTEGER, UNIQUE(name, color))")

	rec := &authRecorder{}
	db.SetAuthorizer(rec)

	// vtab3-1.2: CREATE trace.
	mustExec("CREATE VIRTUAL TABLE pachyderm USING echo(elephant)")
	want := []string{
		"SQLITE_INSERT", "sqlite_master", "", "main", "",
		"SQLITE_CREATE_VTABLE", "pachyderm", "echo", "main", "",
	}
	if strings.Join(rec.log, "|") != strings.Join(want, "|") {
		t.Fatalf("create trace:\n got %v\nwant %v", rec.log, want)
	}

	// vtab3-1.3: DROP trace.
	rec.log = nil
	mustExec("DROP TABLE pachyderm")
	want = []string{
		"SQLITE_DELETE", "sqlite_master", "", "main", "",
		"SQLITE_DROP_VTABLE", "pachyderm", "echo", "main", "",
		"SQLITE_DELETE", "pachyderm", "", "main", "",
		"SQLITE_DELETE", "sqlite_master", "", "main", "",
	}
	if strings.Join(rec.log, "|") != strings.Join(want, "|") {
		t.Fatalf("drop trace:\n got %v\nwant %v", rec.log, want)
	}

	// vtab3-1.4: deny on the first action — the vtab is not created.
	rec.log = nil
	rec.failN = 1
	if res := db.Exec("CREATE VIRTUAL TABLE pachyderm USING echo(elephant)"); res.Error == nil ||
		res.Error.Error() != "not authorized" {
		t.Fatalf("denied create: got %v", res.Error)
	}
	r := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if r.Error != nil || len(r.Rows) != 1 || r.Rows[0][0] != "elephant" {
		t.Fatalf("schema after denied create: %v %v", r.Rows, r.Error)
	}
}
