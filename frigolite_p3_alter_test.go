package frigolite

import (
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.ALTER ALTER TABLE support, written
// BEFORE running the TCL testgen packages (alter, altercol, altercons,
// alterdropcol, altertab, altertrig). Each expectation was verified against
// sqlite3 3.53.4 (via python3) and sqlite3 3.51 CLI as the oracle; the tests
// document the exact SQLite semantics frigolite must match: RENAME TABLE
// updates sqlite_schema SQL of the table itself plus dependent views,
// triggers, and indexes; RENAME COLUMN rewrites references in triggers,
// views, and partial indexes; ADD COLUMN validates CHECK/NOT NULL against
// existing rows; DROP COLUMN fails when the column is referenced by an index
// or is a PRIMARY KEY; and SET/DROP NOT NULL / ADD/DROP CONSTRAINT rewrite
// the CREATE TABLE SQL.

// TestP3Alter is the top-level entry for the P3 ALTER pre-tests. The verify
// command runs it via `go test -run TestP3Alter -count=1 .`
func TestP3Alter(t *testing.T) {
	for _, sub := range []string{
		"RenameTable", "RenameColumn", "AddColumn", "DropColumn",
		"SetDropNotNull", "AddDropConstraint", "Dependencies",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "RenameTable":
				TestP3Alter_RenameTable(t)
			case "RenameColumn":
				TestP3Alter_RenameColumn(t)
			case "AddColumn":
				TestP3Alter_AddColumn(t)
			case "DropColumn":
				TestP3Alter_DropColumn(t)
			case "SetDropNotNull":
				TestP3Alter_SetDropNotNull(t)
			case "AddDropConstraint":
				TestP3Alter_AddDropConstraint(t)
			case "Dependencies":
				TestP3Alter_Dependencies(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// tableSQL returns the stored CREATE TABLE/VIEW/TRIGGER/INDEX sql for a
// schema entry.
func tableSQL(t *testing.T, db *DB, name string) string {
	t.Helper()
	r := db.Query(`SELECT sql FROM sqlite_schema WHERE name='` + name + `'`)
	if r.Error != nil {
		t.Fatalf("query sqlite_schema for %s: %v", name, r.Error)
	}
	if len(r.Rows) == 0 {
		return ""
	}
	if len(r.Rows[0]) == 0 || r.Rows[0][0] == nil {
		return ""
	}
	return formatSQLiteValue(r.Rows[0][0])
}

// execFails asserts that executing sql returns an error containing substr.
func execFails(t *testing.T, db *DB, sql, substr string) {
	t.Helper()
	r := db.Exec(sql)
	if r.Error == nil {
		t.Errorf("expected error containing %q, got success\n  sql: %s", substr, sql)
		return
	}
	if !strings.Contains(r.Error.Error(), substr) {
		t.Errorf("expected error containing %q, got: %v\n  sql: %s", substr, r.Error, sql)
	}
}

// TestP3Alter_RenameTable covers ALTER TABLE t RENAME TO t2: the stored SQL
// is rewritten, data survives, sqlite_sequence is updated, and errors for
// non-existent tables or existing target names.
func TestP3Alter_RenameTable(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b, c)")
	must("INSERT INTO t VALUES(1,2,3)")
	must("ALTER TABLE t RENAME TO t2")

	// Data survives the rename.
	if got := flattenQuery(t, db, "SELECT a, b, c FROM t2"); got != "1 2 3" {
		t.Errorf("data after rename: got [%s], want [1 2 3]", got)
	}
	// Stored SQL uses the new name (quoted like SQLite does).
	got := tableSQL(t, db, "t2")
	if !strings.Contains(got, `"t2"`) {
		t.Errorf("stored SQL after rename: got [%s], want quoted \"t2\"", got)
	}
	// Old name is gone.
	if got := tableSQL(t, db, "t"); got != "" {
		t.Errorf("old table entry still present: [%s]", got)
	}

	// Rename to an existing table fails.
	must("CREATE TABLE t3(x)")
	execFails(t, db, "ALTER TABLE t2 RENAME TO t3", "table or index with this name")

	// Renaming a non-existent table fails.
	execFails(t, db, "ALTER TABLE nope RENAME TO nope2", "no such table")
}

// TestP3Alter_RenameColumn covers RENAME COLUMN with and without the COLUMN
// keyword, plus rewriting of stored CREATE TABLE SQL.
func TestP3Alter_RenameColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b, c)")
	must("INSERT INTO t VALUES(1,2,3)")
	must("ALTER TABLE t RENAME COLUMN b TO b2")
	if got := flattenQuery(t, db, "SELECT a, b2, c FROM t"); got != "1 2 3" {
		t.Errorf("data after rename column: got [%s], want [1 2 3]", got)
	}
	got := tableSQL(t, db, "t")
	if !strings.Contains(got, "b2") || strings.Contains(got, ", b,") {
		t.Errorf("stored SQL after rename column: got [%s]", got)
	}

	// Without the COLUMN keyword (legacy syntax).
	must("ALTER TABLE t RENAME c TO c2")
	if got := flattenQuery(t, db, "SELECT a, b2, c2 FROM t"); got != "1 2 3" {
		t.Errorf("data after legacy rename column: got [%s]", got)
	}

	// Rename to existing column name fails.
	execFails(t, db, "ALTER TABLE t RENAME COLUMN b2 TO a", "duplicate column name")
	// Renaming a missing column fails.
	execFails(t, db, "ALTER TABLE t RENAME COLUMN nope TO x", "no such column")
}

// TestP3Alter_AddColumn covers ADD COLUMN with defaults, NULL default, and
// CHECK/NOT NULL validation against existing rows.
func TestP3Alter_AddColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b)")
	must("INSERT INTO t VALUES(1,2)")

	// Plain ADD COLUMN gives NULL for existing rows.
	must("ALTER TABLE t ADD COLUMN c")
	if got := flattenQuery(t, db, "SELECT a, b, c FROM t"); got != "1 2 NULL" {
		t.Errorf("add plain column: got [%s], want [1 2 NULL]", got)
	}

	// ADD COLUMN with DEFAULT fills existing rows with the default.
	must("ALTER TABLE t ADD COLUMN d DEFAULT 5")
	if got := flattenQuery(t, db, "SELECT a, b, d FROM t"); got != "1 2 5" {
		t.Errorf("add default column: got [%s], want [1 2 5]", got)
	}

	// CHECK constraint is evaluated against existing rows.
	must("CREATE TABLE t2(x)")
	must("INSERT INTO t2 VALUES(1)")
	must("INSERT INTO t2 VALUES(2)")
	// CHECK that succeeds for existing rows is fine.
	must("ALTER TABLE t2 ADD COLUMN y CHECK(x<3)")
	// CHECK that fails for existing rows is rejected.
	execFails(t, db, "ALTER TABLE t2 ADD COLUMN z CHECK(x>0 AND x!=1)", "CHECK constraint failed")

	// NOT NULL without default on a table with rows fails.
	execFails(t, db, "ALTER TABLE t ADD COLUMN e NOT NULL", "Cannot add a NOT NULL column with default value NULL")

	// Duplicate column name fails.
	execFails(t, db, "ALTER TABLE t ADD COLUMN a", "duplicate column name")

	// Default applies to new rows.
	must("INSERT INTO t(a, b) VALUES(10, 20)")
	if got := flattenQuery(t, db, "SELECT a, b, c, d FROM t WHERE a=10"); got != "10 20 NULL 5" {
		t.Errorf("new row with defaults: got [%s], want [10 20 NULL 5]", got)
	}
}

// TestP3Alter_DropColumn covers DROP COLUMN: column removed from stored SQL
// and data, and rejection when the column is a PRIMARY KEY or referenced by
// an index.
func TestP3Alter_DropColumn(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b, c)")
	must("INSERT INTO t VALUES(1,2,3)")
	must("ALTER TABLE t DROP COLUMN b")
	if got := flattenQuery(t, db, "SELECT a, c FROM t"); got != "1 3" {
		t.Errorf("data after drop column: got [%s], want [1 3]", got)
	}
	got := tableSQL(t, db, "t")
	if strings.Contains(got, "b") {
		t.Errorf("stored SQL after drop column still contains b: [%s]", got)
	}
	// Column gone from pragma table_info.
	pi := flattenQuery(t, db, "SELECT name FROM pragma_table_info('t')")
	if pi != "a c" {
		t.Errorf("table_info after drop: got [%s], want [a c]", pi)
	}

	// Dropping a PRIMARY KEY column fails.
	must("CREATE TABLE pk(p INTEGER PRIMARY KEY, q)")
	execFails(t, db, "ALTER TABLE pk DROP COLUMN p", "PRIMARY KEY")

	// Dropping a column referenced by an index fails.
	must("CREATE TABLE idx(i1, i2)")
	must("CREATE INDEX ix ON idx(i2)")
	execFails(t, db, "ALTER TABLE idx DROP COLUMN i2", "no such column")

	// Dropping a missing column fails.
	execFails(t, db, "ALTER TABLE t DROP COLUMN nope", "no such column")

	// Table with single column: dropping the last column fails.
	must("CREATE TABLE solo(s1)")
	execFails(t, db, "ALTER TABLE solo DROP COLUMN s1", "cannot drop")
}

// TestP3Alter_SetDropNotNull covers ALTER COLUMN SET/DROP NOT NULL rewriting
// of the CREATE TABLE SQL and constraint enforcement on SET.
func TestP3Alter_SetDropNotNull(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b, c)")
	must("ALTER TABLE t ALTER c SET NOT NULL")
	got := tableSQL(t, db, "t")
	if !strings.Contains(got, "c NOT NULL") {
		t.Errorf("after SET NOT NULL stored SQL: [%s]", got)
	}
	// NULL insert into c now fails.
	execFails(t, db, "INSERT INTO t(a, b, c) VALUES(1,2,NULL)", "NOT NULL constraint failed")

	// DROP NOT NULL removes the constraint.
	must("ALTER TABLE t ALTER c DROP NOT NULL")
	got = tableSQL(t, db, "t")
	if strings.Contains(got, "NOT NULL") {
		t.Errorf("after DROP NOT NULL stored SQL: [%s]", got)
	}
	must("INSERT INTO t(a, b, c) VALUES(1,2,NULL)")

	// SET NOT NULL fails when existing rows contain NULL.
	must("CREATE TABLE t2(x, y)")
	must("INSERT INTO t2 VALUES(1, NULL)")
	execFails(t, db, "ALTER TABLE t2 ALTER y SET NOT NULL", "constraint failed")

	// ALTER COLUMN keyword variant.
	must("CREATE TABLE t3(m, n)")
	must("ALTER TABLE t3 ALTER COLUMN n SET NOT NULL")
	if got := tableSQL(t, db, "t3"); !strings.Contains(got, "n NOT NULL") {
		t.Errorf("after ALTER COLUMN SET NOT NULL stored SQL: [%s]", got)
	}
}

// TestP3Alter_AddDropConstraint covers ADD CONSTRAINT / DROP CONSTRAINT with
// named CHECK constraints.
func TestP3Alter_AddDropConstraint(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	must("CREATE TABLE t(a, b)")
	must("ALTER TABLE t ADD CONSTRAINT cc CHECK(a>0)")
	got := tableSQL(t, db, "t")
	if !strings.Contains(got, "CONSTRAINT cc CHECK") {
		t.Errorf("after ADD CONSTRAINT stored SQL: [%s]", got)
	}
	// The constraint is enforced on new rows.
	execFails(t, db, "INSERT INTO t(a, b) VALUES(-1, 2)", "CHECK constraint failed")

	// ADD CONSTRAINT validates existing rows too: adding a constraint that
	// an existing row violates fails.
	must("CREATE TABLE tbad(x, y)")
	must("INSERT INTO tbad VALUES(-1, 2)")
	execFails(t, db, "ALTER TABLE tbad ADD CONSTRAINT cc CHECK(x>0)", "constraint failed")
	must("INSERT INTO t(a, b) VALUES(7, 2)")

	// DROP CONSTRAINT removes it.
	must("ALTER TABLE t DROP CONSTRAINT cc")
	got = tableSQL(t, db, "t")
	if strings.Contains(got, "CHECK") {
		t.Errorf("after DROP CONSTRAINT stored SQL: [%s]", got)
	}
	must("INSERT INTO t(a, b) VALUES(-5, 2)")

	// Dropping a non-existent constraint fails.
	execFails(t, db, "ALTER TABLE t DROP CONSTRAINT nope", "no such constraint")
}

// TestP3Alter_Dependencies covers dependency management: RENAME TABLE and
// RENAME COLUMN update views, triggers, and indexes that reference the table.
func TestP3Alter_Dependencies(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}
	// RENAME COLUMN rewrites references inside triggers.
	must("CREATE TABLE t(a, b, c)")
	must("INSERT INTO t VALUES(1,2,3)")
	must("CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT new.b; END")
	must("ALTER TABLE t RENAME COLUMN b TO b2")
	tr := tableSQL(t, db, "tr")
	if !strings.Contains(tr, "new.b2") {
		t.Errorf("trigger after rename column: [%s], want reference to new.b2", tr)
	}

	// RENAME COLUMN rewrites partial index WHERE clauses.
	must("CREATE INDEX idx ON t(c) WHERE c>0")
	must("ALTER TABLE t RENAME COLUMN c TO c2")
	ix := tableSQL(t, db, "idx")
	if !strings.Contains(ix, "c2") || strings.Contains(ix, "WHERE c>") {
		t.Errorf("partial index after rename column: [%s]", ix)
	}

	// RENAME COLUMN rewrites view references.
	must("CREATE VIEW v AS SELECT a, b2 FROM t")
	must("ALTER TABLE t RENAME COLUMN b2 TO be")
	vw := tableSQL(t, db, "v")
	if !strings.Contains(vw, "be") {
		t.Errorf("view after rename column: [%s], want reference to be", vw)
	}

	// RENAME TABLE rewrites references inside triggers and views.
	must("CREATE TRIGGER tr2 AFTER INSERT ON t BEGIN SELECT new.a; END")
	must("ALTER TABLE t RENAME TO t2")
	tr2 := tableSQL(t, db, "tr2")
	if !strings.Contains(tr2, "t2") {
		t.Errorf("trigger after rename table: [%s], want reference to t2", tr2)
	}
	vw2 := tableSQL(t, db, "v")
	if !strings.Contains(vw2, "t2") {
		t.Errorf("view after rename table: [%s], want reference to t2", vw2)
	}

	// Views referencing the renamed table still work.
	if got := flattenQuery(t, db, "SELECT a, be FROM v"); got != "1 2" {
		t.Errorf("view query after rename: got [%s], want [1 2]", got)
	}

	// Trigger still fires and works.
	must("CREATE TABLE log(x)")
	must("CREATE TRIGGER logtr AFTER INSERT ON t2 BEGIN INSERT INTO log VALUES(new.a); END")
	must("INSERT INTO t2(a) VALUES(42)")
	if got := flattenQuery(t, db, "SELECT x FROM log"); got != "42" {
		t.Errorf("trigger after rename fired wrong: got [%s], want [42]", got)
	}
}

// TestP3Alter_RenameParenSetTrigger covers ALTER TABLE rename operations on
// triggers whose bodies use SQLite's row-value UPDATE assignment
// (SET (c,d)=(a,b)). SQLite stores the CREATE TRIGGER text verbatim (the
// parser's paren-set rewriter must not leak into sqlite_schema) and rename
// rewrites column references inside the preserved text (altertab2-4.x).
func TestP3Alter_RenameParenSetTrigger(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	must := func(sql string) {
		t.Helper()
		r := db.Exec(sql)
		if r.Error != nil {
			t.Fatalf("exec: %v\n  sql: %s", r.Error, sql)
		}
	}

	must(`CREATE TABLE t1(a,b,c,d,e,f)`)
	must(`CREATE TRIGGER r1 AFTER INSERT ON t1 WHEN new.a NOT NULL BEGIN
    UPDATE t1 SET (c,d)=(a,b);
  END`)

	// SQLite stores the trigger SQL verbatim, preserving the paren-set form.
	got := tableSQL(t, db, "r1")
	want := `CREATE TRIGGER r1 AFTER INSERT ON t1 WHEN new.a NOT NULL BEGIN
    UPDATE t1 SET (c,d)=(a,b);
  END`
	if got != want {
		t.Errorf("stored trigger SQL:\n  got:  %q\n  want: %q", got, want)
	}

	// RENAME TO rewrites both the ON clause and the body table reference.
	must(`ALTER TABLE t1 RENAME TO t1x`)
	got = tableSQL(t, db, "r1")
	want = `CREATE TRIGGER r1 AFTER INSERT ON "t1x" WHEN new.a NOT NULL BEGIN
    UPDATE "t1x" SET (c,d)=(a,b);
  END`
	if got != want {
		t.Errorf("trigger SQL after RENAME TO:\n  got:  %q\n  want: %q", got, want)
	}

	// RENAME COLUMN rewrites column references inside the preserved text.
	must(`ALTER TABLE t1x RENAME a TO aaa`)
	got = tableSQL(t, db, "r1")
	want = `CREATE TRIGGER r1 AFTER INSERT ON "t1x" WHEN new.aaa NOT NULL BEGIN
    UPDATE "t1x" SET (c,d)=(aaa,b);
  END`
	if got != want {
		t.Errorf("trigger SQL after RENAME COLUMN a:\n  got:  %q\n  want: %q", got, want)
	}

	must(`ALTER TABLE t1x RENAME d TO ddd`)
	got = tableSQL(t, db, "r1")
	want = `CREATE TRIGGER r1 AFTER INSERT ON "t1x" WHEN new.aaa NOT NULL BEGIN
    UPDATE "t1x" SET (c,ddd)=(aaa,b);
  END`
	if got != want {
		t.Errorf("trigger SQL after RENAME COLUMN d:\n  got:  %q\n  want: %q", got, want)
	}
}
