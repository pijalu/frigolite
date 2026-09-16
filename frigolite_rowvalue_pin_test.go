// Pin tests for SQLite row-value semantics (rowvalue.test engine-visible
// contract): multi-clause paren-set UPDATE assignments inside triggers.
package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// TestRowvaluePin_MultiParenSetUpdate covers rowvalue.test 16.3-16.5:
// UPDATE ... SET (c,d) = (...), (e,b) = (...) inside a trigger — every SET
// clause must apply (SQLite evaluates all setlist entries).
func TestRowvaluePin_MultiParenSetUpdate(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mustExec := func(sql string) {
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("exec error: %v\nsql: %s", res.Error, sql)
		}
	}
	query := func(sql string) string {
		r := db.Query(sql)
		if r.Error != nil {
			t.Fatalf("query error: %v\nsql: %s", r.Error, sql)
		}
		var parts []string
		for _, row := range r.Rows {
			for _, v := range row {
				if v == nil {
					parts = append(parts, "NULL")
				} else {
					parts = append(parts, strings.TrimSpace(fmt.Sprint(v)))
				}
			}
		}
		return strings.Join(parts, " ")
	}

	mustExec("CREATE TABLE t16c(a, b, c, d, e);")
	mustExec("INSERT INTO t16c VALUES(1, 'a', 'b', 'c', 'd');")
	mustExec(`CREATE TRIGGER t16c1 AFTER INSERT ON t16c BEGIN
    UPDATE t16c SET (c, d) = (SELECT 'A', 'B'), (e, b) = (SELECT 'C', 'D')
      WHERE a = new.a-1;
  END;`)

	// Plain UPDATE (no trigger path) with two paren-set clauses.
	mustExec("UPDATE t16c SET (c, d) = (SELECT 'X', 'Y'), (e, b) = (SELECT 'Z', 'W') WHERE a = 1;")
	if got, want := query("SELECT * FROM t16c"), "1 W X Y Z"; got != want {
		t.Errorf("plain two-clause paren-set UPDATE:\n got: %s\nwant: %s", got, want)
	}

	// Restore and go through the INSERT trigger (rowvalue 16.4).
	mustExec("DROP TRIGGER t16c1; DELETE FROM t16c;")
	mustExec("INSERT INTO t16c VALUES(1, 'a', 'b', 'c', 'd');")
	mustExec(`CREATE TRIGGER t16c1 AFTER INSERT ON t16c BEGIN
    UPDATE t16c SET (c, d) = (SELECT 'A', 'B'), (e, b) = (SELECT 'C', 'D')
      WHERE a = new.a-1;
  END;`)
	mustExec("INSERT INTO t16c VALUES(2, 'w', 'x', 'y', 'z');")
	if got, want := query("SELECT * FROM t16c"), "1 D A B C 2 w x y z"; got != want {
		t.Errorf("trigger two-clause paren-set UPDATE (rowvalue 16.4):\n got: %s\nwant: %s", got, want)
	}

	// rowvalue 16.5: recursive trigger with two paren-set clauses.
	mustExec("DROP TRIGGER t16c1;")
	mustExec("PRAGMA recursive_triggers = 1;")
	mustExec("INSERT INTO t16c VALUES(3, 'i', 'ii', 'iii', 'iv');")
	mustExec(`CREATE TRIGGER t16c1 AFTER UPDATE ON t16c WHEN new.a>1 BEGIN
    UPDATE t16c SET (e, d) = (
      SELECT b, c FROM t16c WHERE a = new.a-1
    ), (c, b) = (
      SELECT d, e FROM t16c WHERE a = new.a-1
    ) WHERE a = new.a-1;
  END;`)
	mustExec("UPDATE t16c SET a=a WHERE a=3;")
	if got, want := query("SELECT * FROM t16c"), "1 C B A D 2 z y x w 3 i ii iii iv"; got != want {
		t.Errorf("recursive trigger two-clause paren-set UPDATE (rowvalue 16.5):\n got: %s\nwant: %s", got, want)
	}
}
