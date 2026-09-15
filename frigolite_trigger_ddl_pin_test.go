package frigolite_test

import (
	"strings"
	"testing"

	"github.com/pijalu/frigolite"
)

// TestSQLiteTriggerDDLPin pins CREATE TRIGGER DDL error contracts from
// trigger1/trigger4/trigger7 (SQLite trigger.c sqlite3BeginTrigger).
//
// Oracle-verified behaviors (sqlite3 3.51.0):
//   - CREATE TRIGGER on a missing table qualifies the name with the trigger's
//     schema in the error ("no such table: main.X"); a TEMP trigger leaves it
//     unqualified (build.c sqlite3FixSrcList fixes non-temp triggers to their
//     schema; temp triggers are exempt, attach.c fixSelectCb bTemp).
//   - A duplicate trigger name errors "trigger %T already exists" with the
//     name token VERBATIM (quotes preserved); IF NOT EXISTS silently succeeds.
//   - INSTEAD OF on a table / BEFORE/AFTER on a view are rejected.
//   - A schema-qualified trigger name with an unknown database errors
//     "unknown database X" (sqlite3TwoPartName).
func TestSQLiteTriggerDDLPin(t *testing.T) {
	expectErr := func(t *testing.T, sql, want string) {
		t.Helper()
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if must := db.Exec("CREATE TABLE t1(a,b)"); must.Error != nil {
			t.Fatalf("setup: %v", must.Error)
		}
		r := db.Exec(sql)
		if r.Error == nil || !strings.Contains(r.Error.Error(), want) {
			t.Fatalf("exec %s: got %v, want error containing %q", sql, r.Error, want)
		}
	}

	// trigger1-1.1.1: missing ON table in a main trigger names the schema.
	expectErr(t, "CREATE TRIGGER trig UPDATE ON no_such_table BEGIN SELECT 1; END",
		"no such table: main.no_such_table")
	// trigger1-1.1.2: TEMP trigger leaves the name unqualified.
	expectErr(t, "CREATE TEMP TRIGGER trig UPDATE ON no_such_table BEGIN SELECT 1; END",
		"no such table: no_such_table")
	// trigger7-1.1: unknown schema prefix in the trigger name.
	expectErr(t, "CREATE TRIGGER not_a_db.r1 AFTER INSERT ON t1 BEGIN SELECT 1; END",
		"unknown database not_a_db")
	// trigger1-2.2: INSTEAD OF on a table.
	expectErr(t, "CREATE TRIGGER t1t INSTEAD OF UPDATE ON t1 BEGIN DELETE FROM t1; END",
		"cannot create INSTEAD OF trigger on table: t1")
	// trigger1-2.3/2.4: BEFORE/AFTER on a view.
	if db, err := frigolite.Open(":memory:"); err != nil {
		t.Fatal(err)
	} else {
		defer db.Close()
		if must := db.Exec("CREATE TABLE t1(a,b)"); must.Error != nil {
			t.Fatal(must.Error)
		}
		if must := db.Exec("CREATE VIEW v1 AS SELECT * FROM t1"); must.Error != nil {
			t.Fatal(must.Error)
		}
		for _, tc := range []struct{ time, want string }{
			{"BEFORE", "cannot create BEFORE trigger on view: v1"},
			{"AFTER", "cannot create AFTER trigger on view: v1"},
			{"", "cannot create BEFORE trigger on view: v1"},
		} {
			r := db.Exec("CREATE TRIGGER v1t " + tc.time + " UPDATE ON v1 BEGIN DELETE FROM t1; END")
			if r.Error == nil || !strings.Contains(r.Error.Error(), tc.want) {
				t.Fatalf("CREATE TRIGGER v1t %s UPDATE ON v1: got %v, want %q", tc.time, r.Error, tc.want)
			}
		}
	}

	// trigger1-1.2.0..3: duplicate names error with the verbatim name token;
	// IF NOT EXISTS stays silent.
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if must := db.Exec("CREATE TABLE t1(a)"); must.Error != nil {
		t.Fatal(must.Error)
	}
	if must := db.Exec("CREATE TRIGGER tr1 INSERT ON t1 BEGIN SELECT 1; END"); must.Error != nil {
		t.Fatal(must.Error)
	}
	if r := db.Exec("CREATE TRIGGER IF NOT EXISTS tr1 DELETE ON t1 BEGIN SELECT 1; END"); r.Error != nil {
		t.Fatalf("IF NOT EXISTS duplicate: got %v, want silent success", r.Error)
	}
	for _, name := range []string{"tr1", `"tr1"`, "[tr1]"} {
		r := db.Exec("CREATE TRIGGER " + name + " DELETE ON t1 BEGIN SELECT 1; END")
		if r.Error == nil || !strings.Contains(r.Error.Error(), "trigger "+name+" already exists") {
			t.Fatalf("duplicate %s: got %v, want %q", name, r.Error, "trigger "+name+" already exists")
		}
	}
}

// TestSQLiteTriggerBodyErrorPin pins fire-time trigger body error contracts
// from trigger4/triggerB (SQLite compiles trigger subprograms with the firing
// statement, so body errors surface before the statement takes effect).
//
// Oracle-verified behaviors:
//   - a trigger body statement whose table no longer exists reports the
//     trigger's schema-qualified name ("no such table: main.test2",
//     trigger4-3.3, oracle-confirmed).
//   - an unrecognized qualified column in the body surfaces "no such column:
//     wen.x" when the trigger fires, BEFORE the firing statement reports its
//     own constraint failure (triggerB-2.1: oracle errors on the column, not
//     on the row's UNIQUE collision).
func TestSQLiteTriggerBodyErrorPin(t *testing.T) {
	// trigger4-3.3: INSTEAD OF trigger whose body updates a dropped table.
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE test1(id integer primary key, a)",
		"CREATE TABLE test2(id integer, b)",
		"CREATE VIEW test AS SELECT test1.id AS id, a AS a, b AS b FROM test1 JOIN test2 ON test2.id = test1.id",
		"CREATE TRIGGER U_test INSTEAD OF UPDATE ON test BEGIN UPDATE test1 SET a=NEW.a WHERE id=NEW.id; UPDATE test2 SET b=NEW.b WHERE id=NEW.id; END",
		"INSERT INTO test VALUES(1,2,3)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("exec %s: %v", s, r.Error)
		}
	}
	if r := db.Exec("DROP TABLE test2"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Exec("UPDATE test SET a=222 WHERE id=1")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: main.test2") {
		t.Fatalf("update via view trigger: got %v, want no such table: main.test2", r.Error)
	}

	// triggerB-2.1/2.2: unrecognized column references in the body fire the
	// "no such column" error, ahead of any statement-side constraint error.
	db2, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, s := range []string{
		"CREATE TABLE x(x INTEGER PRIMARY KEY, y INT NOT NULL)",
		"INSERT INTO x(y) VALUES(1)",
		"INSERT INTO x(y) VALUES(1)",
	} {
		if r := db2.Exec(s); r.Error != nil {
			t.Fatalf("exec %s: %v", s, r.Error)
		}
	}
	r = db2.Exec("CREATE TRIGGER ty AFTER INSERT ON x BEGIN SELECT wen.x; END; INSERT INTO x VALUES(1,2)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column: wen.x") {
		t.Fatalf("triggerB-2.1: got %v, want no such column: wen.x", r.Error)
	}
	r = db2.Exec("CREATE TRIGGER tz AFTER UPDATE ON x BEGIN SELECT dlo.x; END; UPDATE x SET y=y+1")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column: dlo.x") {
		t.Fatalf("triggerB-2.2: got %v, want no such column: dlo.x", r.Error)
	}

	// Compile-time semantics: the trigger program is validated with the
	// firing statement, so the body error surfaces even when zero rows are
	// affected (oracle: UPDATE of an empty table still errors), and a valid
	// NEW.col body reference keeps working.
	db3, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	if r := db3.Exec("CREATE TABLE e(a); CREATE TRIGGER te AFTER UPDATE ON e BEGIN SELECT wen.x; END"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r = db3.Exec("UPDATE e SET a=1")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such column: wen.x") {
		t.Fatalf("empty-table fire: got %v, want no such column: wen.x", r.Error)
	}
	if r := db3.Exec("CREATE TRIGGER tn AFTER INSERT ON e BEGIN SELECT new.a; END; INSERT INTO e VALUES(7)"); r.Error != nil {
		t.Fatalf("NEW.col body ref: %v", r.Error)
	}
}
