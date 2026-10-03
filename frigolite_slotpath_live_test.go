package frigolite_test

import (
	"fmt"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// TestPinSlotPathNestedDepths pins the live-clone substitution's per-depth
// isolation: an outer same-shape statement stream (template hit → live clone
// at depth 0) interleaves with a trigger body firing the SAME shape per row
// (its own live clone at a deeper exec depth). The trigger rows must carry
// exactly the values each firing substituted — a shared or cross-rewritten
// live clone would copy one row's literals into another's insert.
func TestPinSlotPathNestedDepths(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")
	must("CREATE TABLE log(src INTEGER)")
	must("CREATE TRIGGER tg AFTER INSERT ON t BEGIN INSERT INTO log VALUES(new.c); END")
	must("CREATE TRIGGER tgu AFTER UPDATE ON t BEGIN INSERT INTO log VALUES(new.c); END")
	// Interleave outer INSERTs (depth-0 template hits) with trigger-body
	// INSERTs (deeper-depth template hits on the SAME shape).
	for i := 1; i <= 40; i++ {
		must(fmt.Sprintf("INSERT INTO t VALUES(%d, %d)", i, i*3))
	}
	r := db.Query("SELECT COUNT(*), MIN(src), MAX(src), SUM(src) FROM log")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	wantSum := 0
	for i := 1; i <= 40; i++ {
		wantSum += i * 3
	}
	row := r.Rows[0]
	if row[0] != int64(40) || row[1] != int64(3) || row[2] != int64(120) || row[3] != int64(wantSum) {
		t.Fatalf("log = %v, want [40 3 120 %d]", row, wantSum)
	}
	// Now the same interleaving through UPDATE-driven trigger rows: the outer
	// UPDATE template and the trigger body's INSERT template rewrite their
	// own live clones.
	for i := 1; i <= 40; i++ {
		must(fmt.Sprintf("UPDATE t SET c = %d WHERE id = %d", i*7, i))
	}
	r = db.Query("SELECT COUNT(*), SUM(src) FROM log WHERE src % 7 = 0")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	// 5 insert-phase rows (c = i*3 with i in {7,14,21,28,35}) plus the 40
	// update-phase rows (c = i*7).
	if r.Rows[0][0] != int64(45) {
		t.Fatalf("update-driven log rows = %v, want 45", r.Rows[0][0])
	}
}

// TestPinSlotPathCTECoexistence pins that a template whose WITH body and
// main clause both carry literal slots substitutes each slot with its own
// value through the live-clone path (the CTE-clones-first walk order), and
// that a UNION chain's members stay distinct clones.
func TestPinSlotPathCTECoexistence(t *testing.T) {
	db, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(sql string) {
		t.Helper()
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	must("CREATE TABLE u(a INTEGER, b TEXT)")
	for round := 0; round < 12; round++ {
		// WITH body literal (1000*round) precedes the INSERT SELECT's
		// literals in source order.
		must(fmt.Sprintf("WITH s(x) AS (SELECT %d) INSERT INTO u SELECT x, 'r%d' FROM s", round*1000, round))
		r := db.Query(fmt.Sprintf("SELECT b FROM u WHERE a = %d", round*1000))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if len(r.Rows) != 1 || r.Rows[0][0] != fmt.Sprintf("r%d", round) {
			t.Fatalf("round %d: %v", round, r.Rows)
		}
	}
	if r := db.Query("SELECT COUNT(*) FROM u"); r.Error != nil || r.Rows[0][0] != int64(12) {
		t.Fatalf("u count = %v", r.Rows)
	}
	// UNION chain through the live path: head and member slots substitute
	// independently across rounds.
	for round := 0; round < 12; round++ {
		r := db.Query(fmt.Sprintf("SELECT %d AS v UNION SELECT %d", round, round+100))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if len(r.Rows) != 2 {
			t.Fatalf("round %d: %v", round, r.Rows)
		}
	}
}
