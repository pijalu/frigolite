package frigolite

import (
	"strings"
	"testing"
)

// P3 pre-tests: hand-written tests for G3.FKEY FOREIGN KEY support, written
// BEFORE running the TCL testgen packages (fkey, fkey_). Each expectation was
// verified against sqlite3 3.51 (via the sqlite3 CLI) as the oracle; the tests
// document the exact SQLite semantics frigolite must match: column + table
// REFERENCES forms, parent-key resolution (PK / UNIQUE / implicit), ON
// DELETE/UPDATE actions (CASCADE, SET NULL, SET DEFAULT, RESTRICT, NO ACTION),
// the PRAGMA foreign_keys toggle (default OFF), deferred FK checks at COMMIT,
// PRAGMA defer_foreign_keys, PRAGMA foreign_key_check, self-referential FKs,
// and mismatch / missing-parent errors.

// TestP3FKey is the top-level entry for the P3 FKEY pre-tests. The verify
// command runs it via `go test -run TestP3FKey -count=1 .`
func TestP3FKey(t *testing.T) {
	for _, sub := range []string{
		"Basics", "ParentKey", "Actions", "PragmaToggle",
		"Deferred", "DeferPragma", "FKeyCheck", "SelfRef", "Errors",
	} {
		ok := t.Run(sub, func(t *testing.T) {
			switch sub {
			case "Basics":
				TestP3FKey_Basics(t)
			case "ParentKey":
				TestP3FKey_ParentKey(t)
			case "Actions":
				TestP3FKey_Actions(t)
			case "PragmaToggle":
				TestP3FKey_PragmaToggle(t)
			case "Deferred":
				TestP3FKey_Deferred(t)
			case "DeferPragma":
				TestP3FKey_DeferPragma(t)
			case "FKeyCheck":
				TestP3FKey_FKeyCheck(t)
			case "SelfRef":
				TestP3FKey_SelfRef(t)
			case "Errors":
				TestP3FKey_Errors(t)
			}
		})
		if !ok {
			t.Fail()
		}
	}
}

// TestP3FKey_Basics covers column-level and table-level REFERENCES forms and
// the immediate child-direction FK check (INSERT/UPDATE with no parent row).
func TestP3FKey_Basics(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	must(t, db, "CREATE TABLE p(a PRIMARY KEY, b);")
	must(t, db, "CREATE TABLE c1(x REFERENCES p(a), y);")      // column-level, explicit parent col
	must(t, db, "CREATE TABLE c2(x REFERENCES p, y);")          // column-level, implicit parent PK
	must(t, db, "CREATE TABLE c3(x, y, FOREIGN KEY (x) REFERENCES p(a));") // table-level
	if err := db.Exec("INSERT INTO c1 VALUES(1, 1)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("insert c1 with missing parent: got %v, want FK error", err)
	}
	if err := db.Exec("INSERT INTO c2 VALUES(1, 1)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("insert c2 with missing parent: got %v, want FK error", err)
	}
	if err := db.Exec("INSERT INTO c3 VALUES(1, 1)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("insert c3 with missing parent: got %v, want FK error", err)
	}
	must(t, db, "INSERT INTO p VALUES(1, 'one');")
	must(t, db, "INSERT INTO c1 VALUES(1, 2);")
	must(t, db, "INSERT INTO c2 VALUES(1, 2);")
	must(t, db, "INSERT INTO c3 VALUES(1, 2);")
	// NULL FK values are always valid.
	must(t, db, "INSERT INTO c1 VALUES(NULL, 3);")
	// UPDATE to a missing parent fails.
	if err := db.Exec("UPDATE c1 SET x = 9 WHERE y = 2").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("update c1 to missing parent: got %v, want FK error", err)
	}
	// Parent DELETE with children (NO ACTION default) fails.
	if err := db.Exec("DELETE FROM p WHERE a = 1").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("delete parent with children: got %v, want FK error", err)
	}
}

// TestP3FKey_ParentKey covers parent-key resolution: an FK must reference the
// parent's PRIMARY KEY or a UNIQUE index; a partial UNIQUE index does not
// qualify (foreign key mismatch), and a missing parent table is reported.
func TestP3FKey_ParentKey(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	// REFERENCES t (implicit) targets the parent PK.
	must(t, db, "CREATE TABLE pp(a TEXT PRIMARY KEY); CREATE TABLE cc(x REFERENCES pp);")
	must(t, db, "INSERT INTO pp VALUES('k'); INSERT INTO cc VALUES('k');")
	// A full UNIQUE index qualifies as a parent key.
	must(t, db, "CREATE TABLE uu(x, y); CREATE UNIQUE INDEX uu_x ON uu(x); CREATE TABLE cu(z REFERENCES uu(x));")
	must(t, db, "INSERT INTO uu VALUES(1, 2); INSERT INTO cu VALUES(1);")
	// A partial UNIQUE index does NOT qualify: inserting into the child
	// reports "foreign key mismatch".
	must(t, db, "CREATE TABLE p1(x, y); CREATE UNIQUE INDEX p1x ON p1(x) WHERE y<2; CREATE TABLE c1(a REFERENCES p1(x));")
	if err := db.Exec("INSERT INTO c1 VALUES(1)").Error; err == nil || !strings.Contains(err.Error(), `foreign key mismatch - "c1" referencing "p1"`) {
		t.Errorf("partial unique index parent: got %v, want foreign key mismatch", err)
	}
	// A column UNIQUE constraint qualifies.
	must(t, db, "CREATE TABLE uu2(x UNIQUE); CREATE TABLE cu2(z REFERENCES uu2(x));")
	must(t, db, "INSERT INTO uu2 VALUES(5); INSERT INTO cu2 VALUES(5);")
}

// TestP3FKey_Actions covers the ON DELETE / ON UPDATE actions: CASCADE, SET
// NULL, SET DEFAULT, RESTRICT, and NO ACTION.
func TestP3FKey_Actions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	must(t, db, "CREATE TABLE p(a PRIMARY KEY, b); CREATE TABLE cc(x REFERENCES p ON DELETE CASCADE, y);")
	must(t, db, "INSERT INTO p VALUES(1,'a'),(2,'b'); INSERT INTO cc VALUES(1,1),(1,2),(2,3);")
	must(t, db, "DELETE FROM p WHERE a = 1;")
	if got := flattenQuery(t, db, "SELECT x FROM cc"); got != "2" {
		t.Errorf("ON DELETE CASCADE: got [%s], want [2]", got)
	}
	// SET NULL
	must(t, db, "CREATE TABLE p2(a PRIMARY KEY); CREATE TABLE c2(x REFERENCES p2 ON DELETE SET NULL);")
	must(t, db, "INSERT INTO p2 VALUES(1); INSERT INTO c2 VALUES(1);")
	must(t, db, "DELETE FROM p2 WHERE a=1;")
	if got := flattenQuery(t, db, "SELECT x FROM c2"); got != "NULL" {
		t.Errorf("ON DELETE SET NULL: got [%s], want [NULL]", got)
	}
	// SET DEFAULT: the action sets the FK column to its DEFAULT; when the
	// default has no matching parent row the DELETE fails (the resulting
	// child violates the FK).
	must(t, db, "CREATE TABLE p3(a PRIMARY KEY); CREATE TABLE c3(x DEFAULT 9 REFERENCES p3 ON DELETE SET DEFAULT);")
	must(t, db, "INSERT INTO p3 VALUES(1); INSERT INTO c3 VALUES(1);")
	if err := db.Exec("DELETE FROM p3 WHERE a=1").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("ON DELETE SET DEFAULT to missing parent: got %v, want FK error", err)
	}
	// A DEFAULT that does have a matching parent row applies.
	must(t, db, "CREATE TABLE p3b(a PRIMARY KEY); CREATE TABLE c3b(x DEFAULT 9 REFERENCES p3b ON DELETE SET DEFAULT);")
	must(t, db, "INSERT INTO p3b VALUES(1),(9); INSERT INTO c3b VALUES(1);")
	must(t, db, "DELETE FROM p3b WHERE a=1;")
	if got := flattenQuery(t, db, "SELECT x FROM c3b"); got != "9" {
		t.Errorf("ON DELETE SET DEFAULT: got [%s], want [9]", got)
	}
	// RESTRICT blocks the parent delete.
	must(t, db, "CREATE TABLE p4(a PRIMARY KEY); CREATE TABLE c4(x REFERENCES p4 ON DELETE RESTRICT);")
	must(t, db, "INSERT INTO p4 VALUES(1); INSERT INTO c4 VALUES(1);")
	if err := db.Exec("DELETE FROM p4").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("ON DELETE RESTRICT: got %v, want FK error", err)
	}
	// ON UPDATE CASCADE propagates the new key to children.
	must(t, db, "CREATE TABLE p5(a PRIMARY KEY); CREATE TABLE c5(x REFERENCES p5 ON UPDATE CASCADE);")
	must(t, db, "INSERT INTO p5 VALUES(1); INSERT INTO c5 VALUES(1);")
	must(t, db, "UPDATE p5 SET a = 10 WHERE a = 1;")
	if got := flattenQuery(t, db, "SELECT x FROM c5"); got != "10" {
		t.Errorf("ON UPDATE CASCADE: got [%s], want [10]", got)
	}
}

// TestP3FKey_PragmaToggle covers PRAGMA foreign_keys (default OFF; ON enables
// enforcement).
func TestP3FKey_PragmaToggle(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	if got := flattenQuery(t, db, "PRAGMA foreign_keys"); got != "0" {
		t.Errorf("foreign_keys default: got [%s], want [0]", got)
	}
	// With foreign_keys OFF, orphans are allowed.
	must(t, db, "CREATE TABLE p(a PRIMARY KEY); CREATE TABLE c(x REFERENCES p);")
	must(t, db, "INSERT INTO c VALUES(1);")
	// Turning it ON does not backfill; new violations are enforced.
	must(t, db, "PRAGMA foreign_keys=ON;")
	if err := db.Exec("INSERT INTO c VALUES(2)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("insert with foreign_keys ON: got %v, want FK error", err)
	}
	if got := flattenQuery(t, db, "PRAGMA foreign_keys"); got != "1" {
		t.Errorf("foreign_keys after ON: got [%s], want [1]", got)
	}
}

// TestP3FKey_Deferred covers DEFERRABLE INITIALLY DEFERRED constraints: the
// violation is reported at COMMIT, not at the statement.
func TestP3FKey_Deferred(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	must(t, db, "CREATE TABLE p(x INTEGER PRIMARY KEY);")
	must(t, db, "CREATE TABLE c(z INTEGER REFERENCES p(x) DEFERRABLE INITIALLY DEFERRED);")
	// In autocommit the statement's implicit COMMIT checks the deferred FK.
	if err := db.Exec("INSERT INTO c VALUES(1)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("autocommit deferred insert: got %v, want FK error", err)
	}
	// Inside an explicit transaction the insert succeeds; COMMIT fails.
	must(t, db, "BEGIN;")
	must(t, db, "INSERT INTO c VALUES(1);")
	if err := db.Exec("COMMIT").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("deferred COMMIT: got %v, want FK error", err)
	}
	// The failed COMMIT leaves the transaction open; roll back.
	must(t, db, "ROLLBACK;")
	// A transaction that repairs the violation commits cleanly.
	must(t, db, "BEGIN; INSERT INTO c VALUES(2); INSERT INTO p VALUES(2); COMMIT;")
}

// TestP3FKey_DeferPragma covers PRAGMA defer_foreign_keys: with it ON, even
// immediate constraints are deferred to COMMIT; it resets at COMMIT/ROLLBACK.
func TestP3FKey_DeferPragma(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	must(t, db, "CREATE TABLE p(a PRIMARY KEY); CREATE TABLE c(x REFERENCES p);")
	must(t, db, "BEGIN; PRAGMA defer_foreign_keys=ON;")
	if got := flattenQuery(t, db, "PRAGMA defer_foreign_keys"); got != "1" {
		t.Errorf("defer_foreign_keys inside txn: got [%s], want [1]", got)
	}
	// The immediate FK is deferred: the INSERT succeeds inside the txn.
	must(t, db, "INSERT INTO c VALUES(1);")
	if err := db.Exec("COMMIT").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("deferred COMMIT with defer pragma: got %v, want FK error", err)
	}
	must(t, db, "ROLLBACK;")
	if got := flattenQuery(t, db, "PRAGMA defer_foreign_keys"); got != "0" {
		t.Errorf("defer_foreign_keys after ROLLBACK: got [%s], want [0]", got)
	}
}

// TestP3FKey_FKeyCheck covers PRAGMA foreign_key_check and the
// pragma_foreign_key_check table function.
func TestP3FKey_FKeyCheck(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	// foreign_key_check runs regardless of the foreign_keys setting.
	must(t, db, "CREATE TABLE p1(a INTEGER PRIMARY KEY); CREATE TABLE c1(x INTEGER PRIMARY KEY references p1);")
	must(t, db, "INSERT INTO p1 VALUES(88),(89);")
	must(t, db, "INSERT INTO c1 VALUES(90),(87),(88);")
	if got := flattenQuery(t, db, "PRAGMA foreign_key_check"); got != "c1 87 p1 0 c1 90 p1 0" {
		t.Errorf("foreign_key_check: got [%s], want [c1 87 p1 0 c1 90 p1 0]", got)
	}
	if got := flattenQuery(t, db, "PRAGMA foreign_key_check(c1)"); got != "c1 87 p1 0 c1 90 p1 0" {
		t.Errorf("foreign_key_check(c1): got [%s], want [c1 87 p1 0 c1 90 p1 0]", got)
	}
	if got := flattenQuery(t, db, "SELECT * FROM pragma_foreign_key_check('c1')"); got != "c1 87 p1 0 c1 90 p1 0" {
		t.Errorf("pragma_foreign_key_check('c1'): got [%s], want [c1 87 p1 0 c1 90 p1 0]", got)
	}
}

// TestP3FKey_SelfRef covers self-referential FKs: a row may satisfy its own
// reference, and parent actions apply to the same table.
func TestP3FKey_SelfRef(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	must(t, db, "CREATE TABLE t1(x INTEGER PRIMARY KEY, parent REFERENCES t1 ON DELETE CASCADE);")
	must(t, db, "INSERT INTO t1 VALUES(1, NULL), (2, 1), (3, 2);")
	// Deleting row 1 cascades to rows 2 and 3.
	must(t, db, "DELETE FROM t1 WHERE x = 1;")
	if got := flattenQuery(t, db, "SELECT x FROM t1"); got != "" {
		t.Errorf("self-ref cascade: got [%s], want []", got)
	}
	// A self-referential table-level FK whose parent key is not UNIQUE/PK
	// reports a foreign key mismatch at use.
	must(t, db, "CREATE TABLE t2(c1 PRIMARY KEY, c2, FOREIGN KEY(c1) REFERENCES t2(c2));")
	if err := db.Exec("INSERT INTO t2 VALUES(10000, 20000)").Error; err == nil || !strings.Contains(err.Error(), "foreign key mismatch") {
		t.Errorf("self-ref non-unique parent: got %v, want foreign key mismatch", err)
	}
	// A self-referential FK on a unique column: the row may be its own parent.
	must(t, db, "CREATE TABLE t3(c1 PRIMARY KEY, c2 UNIQUE, FOREIGN KEY(c1) REFERENCES t3(c2));")
	must(t, db, "INSERT INTO t3 VALUES(20000, 20000);")
	if err := db.Exec("INSERT INTO t3 VALUES(30000, 40000)").Error; err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("self-ref row not its own parent: got %v, want FK error", err)
	}
}

// TestP3FKey_Errors covers the exact error messages SQLite reports for FK
// schema problems: a missing parent table and a mismatched parent key.
func TestP3FKey_Errors(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	must(t, db, "PRAGMA foreign_keys=ON;")
	// Missing parent table: "no such table: main.<name>" at use time.
	must(t, db, "CREATE TABLE t9(a REFERENCES nosuchtable, b);")
	if err := db.Exec("INSERT INTO t9 VALUES(1, 2)").Error; err == nil || !strings.Contains(err.Error(), "no such table: main.nosuchtable") {
		t.Errorf("missing parent: got %v, want no such table", err)
	}
	// Referenced parent column that is not a PK/UNIQUE key: foreign key
	// mismatch.
	must(t, db, "CREATE TABLE p(x, y); CREATE TABLE c(a REFERENCES p(x));")
	if err := db.Exec("INSERT INTO c VALUES(1)").Error; err == nil || !strings.Contains(err.Error(), `foreign key mismatch - "c" referencing "p"`) {
		t.Errorf("non-unique parent column: got %v, want foreign key mismatch", err)
	}
}
