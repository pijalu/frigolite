package frigolite_test

import (
	"fmt"
	"path/filepath"
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

// TestSQLiteTempTriggerQualifiedNamePin pins trigger.c sqlite3BeginTrigger's
// TEMP-name rule (trigger7-1.1, oracle-verified against sqlite3 3.54):
//
//   - CREATE TEMP/TEMPORARY TRIGGER schema.name is rejected before the
//     schema is resolved, even when the schema is temp or does not exist.
//   - a quoted or bracketed dotted token is one name, not a qualification.
//   - a non-TEMP CREATE TRIGGER temp.name keeps storing the trigger in the
//     TEMP schema (the isTemp keyword is what triggers this rule).
func TestSQLiteTempTriggerQualifiedNamePin(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if r := db.Exec("CREATE TABLE t1(x, y)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, sql := range []string{
		"CREATE TEMP TRIGGER main.r1 AFTER INSERT ON t1 BEGIN SELECT 1; END",
		"CREATE TEMPORARY TRIGGER temp.r1 AFTER INSERT ON t1 BEGIN SELECT 1; END",
		"CREATE TEMP TRIGGER aux.r1 AFTER INSERT ON t1 BEGIN SELECT 1; END",
	} {
		r := db.Exec(sql)
		if r.Error == nil || !strings.Contains(r.Error.Error(), "temporary trigger may not have qualified name") {
			t.Fatalf("exec %s: got %v, want temporary trigger may not have qualified name", sql, r.Error)
		}
	}
	for _, sql := range []string{
		`CREATE TEMP TRIGGER "r1.x" AFTER INSERT ON t1 BEGIN SELECT 1; END`,
		`CREATE TEMP TRIGGER [r2.x] AFTER INSERT ON t1 BEGIN SELECT 1; END`,
		"CREATE TRIGGER temp.r3 AFTER INSERT ON t1 BEGIN SELECT 1; END",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("exec %s: got %v, want success", sql, r.Error)
		}
	}
	var n int
	if r := db.Exec("SELECT count(*) FROM sqlite_temp_master WHERE type='trigger'"); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		n = int(r.Rows[0][0].(int64))
	}
	if n != 3 {
		t.Fatalf("temp triggers = %d, want 3", n)
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

// TestSQLiteMalformedTriggerOnAttachPin pins the schema-load revalidation of
// stored triggers (triggerupfrom-2.4, oracle-verified against sqlite3 3.54):
//
//   - A trigger stored while its explicit database name matched the trigger's
//     own schema (`FROM main.link` in a database that was main at creation
//     time) becomes malformed when that database is opened under another name:
//     attach.c's fixSelectCb compares the named database with the trigger's
//     own schema index, and a mismatch is
//     "malformed database schema (tr3) - trigger tr3 cannot reference objects
//     in database main".
//   - attachFunc runs sqlite3Init synchronously, so the ATTACH itself fails
//     and the database is NOT left attached (PRAGMA database_list shows only
//     main; the stored table is not reachable).
func TestSQLiteMalformedTriggerOnAttachPin(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "test.db")
	mk, err := frigolite.Open(badPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"CREATE TABLE t1(a INTEGER PRIMARY KEY, b, c)",
		"CREATE TABLE link(f, t)",
		"INSERT INTO link VALUES(5, 2), (20, 10), (2, 1)",
		"CREATE TRIGGER tr3 BEFORE DELETE ON t1 BEGIN " +
			"UPDATE t1 SET b=coalesce(old.b,old.c) FROM main.link WHERE a=t AND old.a=f; END",
	} {
		if r := mk.Exec(sql); r.Error != nil {
			t.Fatalf("setup %s: %v", sql, r.Error)
		}
	}
	if err := mk.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Exec("ATTACH '" + badPath + "' AS yyy")
	want := "malformed database schema (tr3) - trigger tr3 cannot reference objects in database main"
	if r.Error == nil || r.Error.Error() != want {
		t.Fatalf("ATTACH: got %v, want %q", r.Error, want)
	}
	// The failed attach rolls back: yyy is not in database_list and its t1 is
	// not reachable.
	r = db.Query("PRAGMA database_list")
	if r.Error != nil {
		t.Fatalf("database_list after failed attach: %v", r.Error)
	}
	for _, row := range r.Rows {
		if len(row) > 1 && strings.EqualFold(fmt.Sprint(row[1]), "yyy") {
			t.Fatalf("database yyy still attached after failed ATTACH: %v", r.Rows)
		}
	}
	r = db.Query("SELECT * FROM t1")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "no such table: t1") {
		t.Fatalf("SELECT after failed attach: got %v, want no such table: t1", r.Error)
	}
}

// TestSQLiteTriggerGrammarRestrictionsPin pins the parse-time restrictions
// SQLite's grammar applies to trigger bodies (parse.y trnm/tridxby), oracle-
// verified against sqlite3 3.54:
//
//   - A qualified INSERT/UPDATE/DELETE target inside a trigger body is a
//     parse error for non-temp triggers ("qualified table names are not
//     allowed on INSERT, UPDATE, and DELETE statements within triggers"),
//     while a TEMP trigger may write to a fully qualified target — and firing
//     it performs the cross-database write.
//   - INDEXED BY / NOT INDEXED on UPDATE/DELETE is rejected for every trigger,
//     including TEMP, and the grammar error precedes index resolution (a
//     non-existent index name still reports the clause restriction).
func TestSQLiteTriggerGrammarRestrictionsPin(t *testing.T) {
	newDB := func(t *testing.T) *frigolite.DB {
		t.Helper()
		db, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if r := db.Exec("ATTACH ':memory:' AS aux; CREATE TABLE aux.t2(x); CREATE TABLE t1(a)"); r.Error != nil {
			t.Fatal(r.Error)
		}
		return db
	}
	qualified := "qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers"
	for _, body := range []string{
		"INSERT INTO aux.t2 VALUES(new.a)",
		"UPDATE aux.t2 SET x=1",
		"DELETE FROM aux.t2",
	} {
		db := newDB(t)
		r := db.Exec("CREATE TRIGGER r1 AFTER INSERT ON t1 BEGIN " + body + "; END")
		if r.Error == nil || !strings.Contains(r.Error.Error(), qualified) {
			t.Fatalf("non-temp %s: got %v, want %q", body, r.Error, qualified)
		}
	}

	// TEMP trigger: qualified target allowed, and the cross-database write
	// actually happens when the trigger fires.
	db := newDB(t)
	if r := db.Exec("CREATE TEMP TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO aux.t2 VALUES(new.a); END; INSERT INTO t1 VALUES(7)"); r.Error != nil {
		t.Fatalf("temp qualified insert: %v", r.Error)
	}
	r := db.Query("SELECT x FROM aux.t2")
	if r.Error != nil || len(r.Rows) != 1 || fmt.Sprint(r.Rows[0][0]) != "7" {
		t.Fatalf("temp trigger write: rows=%v err=%v, want one row 7", r.Rows, r.Error)
	}

	indexErr := "the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers"
	notIndexedErr := "the NOT INDEXED clause is not allowed on UPDATE or DELETE statements within triggers"
	for _, tc := range []struct{ sql, want string }{
		{"CREATE TRIGGER r2 AFTER INSERT ON t1 BEGIN UPDATE t1 INDEXED BY nosuch SET a=1; END", indexErr},
		{"CREATE TEMP TRIGGER r3 AFTER INSERT ON t1 BEGIN UPDATE t1 INDEXED BY nosuch SET a=1; END", indexErr},
		{"CREATE TRIGGER r4 AFTER INSERT ON t1 BEGIN DELETE FROM t1 NOT INDEXED; END", notIndexedErr},
		{"CREATE TEMP TRIGGER r5 AFTER INSERT ON t1 BEGIN UPDATE t1 NOT INDEXED SET a=1; END", notIndexedErr},
	} {
		db := newDB(t)
		res := db.Exec(tc.sql)
		if res.Error == nil || !strings.Contains(res.Error.Error(), tc.want) {
			t.Fatalf("grammar %q: got %v, want %q", tc.sql, res.Error, tc.want)
		}
	}
}
