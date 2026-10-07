package frigolite_test

import (
	"testing"

	frigo "github.com/pijalu/frigolite"
)

// TestPinAnalyzeStatRowInsert pins the ANALYZE stat-row write path: the
// engine-internal sqlite_stat1 insert passes no column defs, so the
// insert-shape memo declines and the cold path must resolve a minimal shape
// (r10-insscan regression: nil deref in insertRowSh).
func TestPinAnalyzeStatRowInsert(t *testing.T) {
	db, err := frigo.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sql := range []string{
		"CREATE TABLE t1(a, b)",
		"INSERT INTO t1 VALUES(1, 'x')",
		"INSERT INTO t1 VALUES(2, 'y')",
		"CREATE INDEX i1 ON t1(a)",
		"ANALYZE",
		"SELECT * FROM sqlite_stat1",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	res := db.Query("SELECT tbl, idx FROM sqlite_stat1 ORDER BY tbl, idx")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) == 0 {
		t.Fatal("sqlite_stat1 empty after ANALYZE")
	}
	// Second ANALYZE after DDL churn (the autoinc.test-class sequence).
	for _, sql := range []string{
		"CREATE TABLE t2(c)",
		"INSERT INTO t2 VALUES(9)",
		"DROP TABLE t2",
		"ANALYZE",
	} {
		if r := db.Exec(sql); r.Error != nil {
			t.Fatalf("%s: %v", sql, r.Error)
		}
	}
	if res := db.Query("SELECT count(*) FROM sqlite_stat1"); res.Error != nil || len(res.Rows) == 0 {
		t.Fatalf("post-DDL ANALYZE: %v %v", res.Error, res.Rows)
	}
}
