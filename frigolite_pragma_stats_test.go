package frigolite

import "testing"

func TestPragmaStatsExposesIndexAndPlanFlags(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t(a INTEGER PRIMARY KEY, b, c)",
		"CREATE UNIQUE INDEX tu ON t(b,c)",
		"CREATE INDEX tn ON t(c)",
		"WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100) INSERT INTO t SELECT x,x,x FROM n",
	} {
		if got := db.Exec(stmt); got.Error != nil {
			t.Fatalf("Exec(%q): %v", stmt, got.Error)
		}
	}
	rows := db.Query("SELECT idx, flgs FROM pragma_stats WHERE tbl='t' AND idx IS NOT NULL ORDER BY idx")
	if rows.Error != nil {
		t.Fatal(rows.Error)
	}
	if len(rows.Rows) != 2 || rows.Rows[0][0] != "tn" || rows.Rows[1][0] != "tu" {
		t.Fatalf("index stats rows = %#v", rows.Rows)
	}
	if got := db.Query("SELECT * FROM t WHERE c=1"); got.Error != nil {
		t.Fatal(got.Error)
	}
	flag := db.Query("SELECT tbl, idx, flgs FROM pragma_stats WHERE tbl='t'")
	if flag.Error != nil || len(flag.Rows) != 3 {
		t.Fatalf("stats rows = %#v, error %v", flag.Rows, flag.Error)
	}
}
