package frigolite_test

// Typed SET fast lane pins (fleet/r8-update): the point-UPDATE SET lane
// (internal/execdml/update_setlane.go) must produce oracle-identical rows
// for every shape it engages on, and byte-identical rows to the generic
// row-map evaluation for every shape it declines. Expectations transcribed
// from the sqlite3 3.54 oracle (mission gate 5); the parity half drives the
// same statement through the rowid-pinned path (WHERE id = N) and the
// generic scan path (WHERE id = N + 0 — not a constant equality, so
// planDMLSeek declines) and compares the resulting table images.

import (
	"fmt"
	"testing"

	frigo "github.com/pijalu/frigolite"
)

// updlaneDump returns "id|c|r|x|n" cell dumps ordered by id.
func updlaneDump(t *testing.T, db *frigo.DB, table string) []string {
	t.Helper()
	res := db.Query(fmt.Sprintf("SELECT id, c, r, x, n FROM %s ORDER BY id", table))
	if res.Error != nil {
		t.Fatalf("dump %s: %v", table, res.Error)
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		line := fmt.Sprintf("%v", row[0])
		for _, cell := range row[1:] {
			line += "|" + updlaneCell(cell)
		}
		out = append(out, line)
	}
	return out
}

// updlaneCell renders one cell with its storage class, oracle-style.
func updlaneCell(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "null:"
	case int64:
		return fmt.Sprintf("int:%d", t)
	case float64:
		return fmt.Sprintf("real:%v", t)
	case string:
		return fmt.Sprintf("text:%s", t)
	case []byte:
		return fmt.Sprintf("blob:%x", t)
	default:
		return fmt.Sprintf("other:%v", t)
	}
}

// updlaneSeed creates and populates the pin table.
func updlaneSeed(t *testing.T, db *frigo.DB, table string) {
	t.Helper()
	mustExec(t, db, "DROP TABLE IF EXISTS "+table)
	mustExec(t, db, fmt.Sprintf(
		"CREATE TABLE %s(id INTEGER PRIMARY KEY, c INTEGER, r REAL, x TEXT, n)", table))
	stmts := []string{
		fmt.Sprintf("INSERT INTO %s VALUES(1, 5, 5.5, '7', NULL)", table),
		fmt.Sprintf("INSERT INTO %s VALUES(2, 9223372036854775807, 1.0, 'abc', 3)", table),
		fmt.Sprintf("INSERT INTO %s VALUES(3, -9223372036854775808, 2.0, '42', NULL)", table),
		fmt.Sprintf("INSERT INTO %s VALUES(4, NULL, 0.0, NULL, 4)", table),
	}
	for _, s := range stmts {
		mustExec(t, db, s)
	}
}

// TestUpdateSetLaneOracle pins the lane's engaged shapes against sqlite3
// 3.54 oracle outcomes (each want line transcribed from the CLI probe).
func TestUpdateSetLaneOracle(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	updlaneSeed(t, db, "t")

	type step struct {
		sql  string
		id   int64
		want string // expected c|r|x|n cell dump for that row
	}
	steps := []step{
		// int + int stays integer (oracle: integer/6)
		{"UPDATE t SET c=c+1 WHERE id=1", 1, "int:6|real:5.5|text:7|null:"},
		// max-int overflow promotes to REAL (oracle: real/9.2233720368547758e+18)
		{"UPDATE t SET c=c+1 WHERE id=2", 2, "real:9.223372036854776e+18|real:1|text:abc|int:3"},
		// min-int subtraction promotes to REAL (oracle: real/-9.2233720368547758e+18)
		{"UPDATE t SET c=c-1 WHERE id=3", 3, "real:-9.223372036854776e+18|real:2|text:42|null:"},
		// NULL propagates (oracle: null)
		{"UPDATE t SET c=c+1 WHERE id=4", 4, "null:|real:0|null:|int:4"},
		// float literal operand on a REAL column (oracle: real/6.5)
		{"UPDATE t SET r=r+1 WHERE id=1", 1, "int:6|real:6.5|text:7|null:"},
		// TEXT affinity converts the result to text (oracle: text/'8')
		{"UPDATE t SET x=x+1 WHERE id=1", 1, "int:6|real:6.5|text:8|null:"},
		// division by zero is NULL (oracle: null)
		{"UPDATE t SET c=c/0 WHERE id=1", 1, "null:|real:6.5|text:8|null:"},
		// modulo by zero is NULL (oracle: null)
		{"UPDATE t SET r=r%0 WHERE id=1", 1, "null:|null:|text:8|null:"},
		// negated literal in parens (oracle: -(-9.22e18) = 9.22e18 REAL)
		{"UPDATE t SET c=c*(-1) WHERE id=3", 3, "real:9.223372036854776e+18|real:2|text:42|null:"},
		// parenthesized expression (oracle: REAL +1 keeps REAL)
		{"UPDATE t SET c=(c+1) WHERE id=3", 3, "real:9.223372036854776e+18|real:2|text:42|null:"},
		// plain literal store keeps the INTEGER class under INTEGER affinity
		{"UPDATE t SET c=12 WHERE id=1", 1, "int:12|null:|text:8|null:"},
		// column copy moves the raw value (oracle: integer class)
		{"UPDATE t SET c=n WHERE id=4", 4, "int:4|real:0|null:|int:4"},
	}
	for i, st := range steps {
		if res := db.Exec(st.sql); res.Error != nil {
			t.Fatalf("step %d %q: %v", i, st.sql, res.Error)
		}
		res := db.Query(fmt.Sprintf("SELECT c, r, x, n FROM t WHERE id=%d", st.id))
		if res.Error != nil {
			t.Fatalf("step %d select: %v", i, res.Error)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("step %d: row %d missing", i, st.id)
		}
		row := res.Rows[0]
		got := updlaneCell(row[0]) + "|" + updlaneCell(row[1]) + "|" + updlaneCell(row[2]) + "|" + updlaneCell(row[3])
		if got != st.want {
			t.Errorf("step %d %q:\n got %q\nwant %q", i, st.sql, got, st.want)
		}
	}
}

// TestUpdateSetLaneParity drives every lane shape (engaged and declined)
// through the rowid-pinned path and the generic scan path in two identical
// tables; the resulting images must match cell for cell. The generic path
// is forced with "id = N + 0" (not a constant rowid equality, so the seek
// planner declines and the scan pipeline runs the row-map evaluation).
func TestUpdateSetLaneParity(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Each entry: point-path SQL and its generic-scan twin (id = N + 0
	// defeats the constant-equality seek, forcing the row-map pipeline).
	type pair struct{ point, scan string }
	var stmts []pair
	for _, tmpl := range []string{
		"UPDATE %s SET c=c+1 WHERE id=2",
		"UPDATE %s SET c=c*2 WHERE id=2",
		"UPDATE %s SET c=c/2 WHERE id=2",
		"UPDATE %s SET c=c%%3 WHERE id=2",
		"UPDATE %s SET c=c-1 WHERE id=3",
		"UPDATE %s SET c=c+1.5 WHERE id=3",
		"UPDATE %s SET c=(c+1) WHERE id=3",
		"UPDATE %s SET c=c*(-1) WHERE id=3",
		"UPDATE %s SET c=c+1 WHERE id=4",      // NULL propagate
		"UPDATE %s SET x=x+1 WHERE id=1",      // text coercion + TEXT affinity
		"UPDATE %s SET r=r+1 WHERE id=1",      // REAL column
		"UPDATE %s SET c=c/0 WHERE id=1",      // div by zero
		"UPDATE %s SET c=rowid+c WHERE id=2",  // rowid operand
		"UPDATE %s SET c=id+c WHERE id=2",     // IPK alias operand
		"UPDATE %s SET c=12 WHERE id=2",       // literal store
		"UPDATE %s SET c=n WHERE id=2",        // column copy
		"UPDATE %s SET c=c+1, n=c WHERE id=2", // second RHS sees the original row
		"UPDATE %s SET c=c+1, c=c*10 WHERE id=2",
	} {
		point := fmt.Sprintf(tmpl, "pa")
		scan := fmt.Sprintf(tmpl, "pb")
		// Replace the pinned equality with an equivalent expression that is
		// no longer a constant rowid equality (the planner declines it).
		for n := 1; n <= 4; n++ {
			old := fmt.Sprintf("WHERE id=%d", n)
			repl := fmt.Sprintf("WHERE id=%d+0", n)
			if len(scan) >= len(old) && scan[len(scan)-len(old):] == old {
				scan = scan[:len(scan)-len(old)] + repl
				break
			}
		}
		stmts = append(stmts, pair{point, scan})
	}
	for i, st := range stmts {
		updlaneSeed(t, db, "pa")
		updlaneSeed(t, db, "pb")
		if res := db.Exec(st.point); res.Error != nil {
			t.Fatalf("stmt %d point %q: %v", i, st.point, res.Error)
		}
		if res := db.Exec(st.scan); res.Error != nil {
			t.Fatalf("stmt %d scan %q: %v", i, st.scan, res.Error)
		}
		want, got := updlaneDump(t, db, "pa"), updlaneDump(t, db, "pb")
		if len(got) != len(want) {
			t.Fatalf("stmt %d %q: row count %d vs %d", i, st.point, len(got), len(want))
		}
		for r := range want {
			if got[r] != want[r] {
				t.Errorf("stmt %d %q (scan %q):\n row %d got  %s\n row %d want %s",
					i, st.point, st.scan, r, got[r], r, want[r])
			}
		}
	}
}
