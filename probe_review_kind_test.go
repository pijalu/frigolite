package frigolite

import "testing"

// Area 3/6: numeric-kind parity through the statement template cache.
// ORACLE: INSERT of 8.0 into a no-affinity column stores REAL (typeof real),
// whatever ran before. A template slot first seen with an integer literal
// must not coerce a later float literal (38aaa3a86's gate) — including the
// INSERT VALUES substitution path (same normalized text primes the slot).
func TestReviewTemplateNumericKindParity(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(a)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	// Prime the template with an integer literal, then a float through the
	// SAME normalized text.
	if r := db.Exec("INSERT INTO t VALUES(5)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO t VALUES(8.0)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT typeof(a), a FROM t"))
	t.Logf("insert 5 then 8.0 same template: %s", got)
	if got != "string:integer|int64:5\nstring:real|float64:8\n" {
		t.Fatalf("template INSERT coerced REAL to integer (oracle: real|8.0): %s", got)
	}
}

func TestReviewTemplateSelectKindParity(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE t(b REAL); INSERT INTO t VALUES(1.5),(2.5),(3.5)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for _, q := range []string{
		"SELECT count(*) FROM t WHERE b > 2",
		"SELECT count(*) FROM t WHERE b > 2.2",
		"SELECT 5, typeof(5)",
		"SELECT 5.5, typeof(5.5)",
		"SELECT 5, typeof(5)",
	} {
		rows := probeRows(t, db, q)
		t.Logf("%s -> %s", q, probeDump(rows))
	}
}

// DML-path variant: UPDATE SET with alternating int/float literals through
// one template (SET slot first integer, then float).
func TestReviewTemplateUpdateKindParity(t *testing.T) {
	db := probeOpen(t)
	if r := db.Exec("CREATE TABLE u(k INTEGER PRIMARY KEY, v)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for i := 1; i <= 3; i++ {
		if r := db.Exec("INSERT INTO u(v) VALUES(0)"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := db.Exec("UPDATE u SET v = 7 WHERE k = 1"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("UPDATE u SET v = 7.5 WHERE k = 2"); r.Error != nil {
		t.Fatal(r.Error)
	}
	got := probeDump(probeRows(t, db, "SELECT k, typeof(v), v FROM u ORDER BY k"))
	t.Logf("update kinds:\n%s", got)
	if got != "int64:1|string:integer|int64:7\nint64:2|string:real|float64:7.5\nint64:3|string:integer|int64:0\n" {
		t.Fatalf("update template diverged from oracle: %s", got)
	}
}
