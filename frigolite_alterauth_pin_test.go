package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
	"github.com/pijalu/frigolite/internal/auth"
)

// alterAuthRecorder records the (code, arg1..arg4) tuples the engine reports,
// TCL-fixture style: empty args render as "{}" so the recorded line matches
// the alterauth.test/savepoint.test expectations verbatim.
type alterAuthRecorder struct {
	lines []string
	deny  bool
	denyK func(arg1, arg2 string) bool
}

func (r *alterAuthRecorder) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {
	a := func(s string) string {
		if s == "" {
			return "{}"
		}
		return s
	}
	r.lines = append(r.lines, action.String()+" "+a(arg1)+" "+a(arg2)+" "+a(arg3)+" "+a(arg4))
	if r.deny && (r.denyK == nil || r.denyK(arg1, arg2)) {
		return auth.ResultDeny
	}
	return auth.ResultOK
}

func (r *alterAuthRecorder) reset() { r.lines = nil }

// TestSQLiteAlterAuthPin pins alterauth.test 1.x/2.x: every ALTER TABLE form
// invokes the authorizer once with SQLITE_ALTER_TABLE (dbName, tableName)
// (alter.c sqlite3AuthCheck calls in renameTable/addColumn/renameColumn/
// dropColumn), and SQLITE_DENY fails the statement with "not authorized"
// leaving no side effects. The tcl2go package cannot express the `db auth`
// fixture (proc not transpiled), so the engine-visible contract is pinned
// here and the package is superseded.
func TestSQLiteAlterAuthPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rec := &alterAuthRecorder{}
	db.SetAuthorizer(rec)
	if r := db.Exec("CREATE TABLE t1(a, b, c)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}

	// 1.1: RENAME TO reports (main, t1).
	rec.reset()
	if r := db.Exec("ALTER TABLE t1 RENAME TO t2"); r.Error != nil {
		t.Fatalf("1.1 rename: %v", r.Error)
	}
	if got, want := strings.Join(rec.lines, ";"), "SQLITE_ALTER_TABLE main t1 {} {}"; got != want {
		t.Fatalf("1.1 auth args: got [%s] want [%s]", got, want)
	}

	// 1.2: RENAME COLUMN reports (main, t2).
	rec.reset()
	if r := db.Exec("ALTER TABLE t2 RENAME c TO ccc"); r.Error != nil {
		t.Fatalf("1.2 rename column: %v", r.Error)
	}
	if got, want := strings.Join(rec.lines, ";"), "SQLITE_ALTER_TABLE main t2 {} {}"; got != want {
		t.Fatalf("1.2 auth args: got [%s] want [%s]", got, want)
	}

	// 1.3: ADD COLUMN reports (main, t2).
	rec.reset()
	if r := db.Exec("ALTER TABLE t2 ADD COLUMN d"); r.Error != nil {
		t.Fatalf("1.3 add column: %v", r.Error)
	}
	if got, want := strings.Join(rec.lines, ";"), "SQLITE_ALTER_TABLE main t2 {} {}"; got != want {
		t.Fatalf("1.3 auth args: got [%s] want [%s]", got, want)
	}

	// 2.1-2.3: SQLITE_DENY fails each form with "not authorized" (and the
	// rename/add do not run).
	rec.deny = true
	if r := db.Exec("ALTER TABLE t2 RENAME TO t3"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "not authorized") {
		t.Fatalf("2.1 deny: got %v, want 'not authorized'", r.Error)
	}
	if r := db.Exec("ALTER TABLE t2 ADD COLUMN e"); r.Error == nil ||
		!strings.Contains(r.Error.Error(), "not authorized") {
		t.Fatalf("2.3 deny: got %v, want 'not authorized'", r.Error)
	}
	// The denied operations had no effect.
	master := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if master.Error != nil {
		t.Fatalf("master query: %v", master.Error)
	}
	if len(master.Rows) != 1 {
		t.Fatalf("denied ALTERs changed the schema: %v", master.Rows)
	}
}

// TestSQLiteSavepointAuthPin pins savepoint.test 9.1-9.3: SAVEPOINT, ROLLBACK
// TO, and RELEASE invoke the authorizer with SQLITE_SAVEPOINT and the
// operation name BEGIN / ROLLBACK / RELEASE plus the savepoint name (build.c
// sqlite3Savepoint), before the operation executes.
func TestSQLiteSavepointAuthPin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rec := &alterAuthRecorder{}
	db.SetAuthorizer(rec)
	if r := db.Exec("CREATE TABLE t1(a)"); r.Error != nil {
		t.Fatalf("setup: %v", r.Error)
	}

	type step struct {
		sql  string
		want string
	}
	for _, s := range []step{
		{"SAVEPOINT sp1", "SQLITE_SAVEPOINT BEGIN sp1 {} {}"},
		{"ROLLBACK TO sp1", "SQLITE_SAVEPOINT ROLLBACK sp1 {} {}"},
		{"RELEASE sp1", "SQLITE_SAVEPOINT RELEASE sp1 {} {}"},
	} {
		rec.reset()
		if r := db.Exec(s.sql); r.Error != nil {
			t.Fatalf("%s: %v", s.sql, r.Error)
		}
		if got, want := strings.Join(rec.lines, ";"), s.want; got != want {
			t.Fatalf("%s auth args: got [%s] want [%s]", s.sql, got, want)
		}
	}

	// Denying SAVEPOINT fails with "not authorized" and opens nothing: the
	// following RELEASE must report the savepoint as unknown.
	rec.reset()
	rec.deny = true
	rec.denyK = func(arg1, arg2 string) bool { return arg1 == "BEGIN" }
	if r := db.Exec("SAVEPOINT sp9"); r.Error == nil || !strings.Contains(r.Error.Error(), "not authorized") {
		t.Fatalf("deny SAVEPOINT: got %v, want 'not authorized'", r.Error)
	}
	rec.deny = false
	rec.denyK = nil
	if r := db.Exec("RELEASE sp9"); r.Error == nil || !strings.Contains(r.Error.Error(), "no such savepoint") {
		t.Fatalf("RELEASE after deny: got %v, want 'no such savepoint'", r.Error)
	}
}
