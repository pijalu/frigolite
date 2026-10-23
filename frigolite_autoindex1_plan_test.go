package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

func TestAutoindexPlannerBuildsJoinIndexFromStatistics(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a,b)",
		"INSERT INTO t1 VALUES(1,11),(2,22),(3,33),(4,44),(5,55),(6,66),(7,77),(8,88)",
		"CREATE TABLE t2(c,d)",
		"INSERT INTO t2 SELECT a,900+b FROM t1",
		"PRAGMA automatic_index=ON",
		"ANALYZE",
		"UPDATE sqlite_stat1 SET stat='10000' WHERE tbl='t2'",
		"ANALYZE sqlite_master",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatalf("%s: %v", stmt, result.Error)
		}
	}
	result := db.Query("EXPLAIN QUERY PLAN SELECT b,d FROM t1 CROSS JOIN t2 ON (c=a)")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	var plan strings.Builder
	for _, row := range result.Rows {
		for _, value := range row {
			plan.WriteString(fmt.Sprint(value))
			plan.WriteByte(' ')
		}
	}
	if !strings.Contains(strings.ToUpper(plan.String()), "AUTOMATIC COVERING INDEX") {
		t.Fatalf("plan = %q, want automatic covering index", plan.String())
	}
}

func TestAutoindexPlannerUsesAutoindexForViewJoinAndCompositePK(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE pk_t(a INT, b INT, x INT, PRIMARY KEY(a,b)) WITHOUT ROWID",
		"CREATE TABLE va(c INT PRIMARY KEY, i INT)",
		"CREATE TABLE vb(i INT PRIMARY KEY, d INT)",
		"CREATE VIEW vv(c,d) AS SELECT c,d FROM va NATURAL JOIN vb",
		"INSERT INTO pk_t VALUES(1,2,90),(1,3,91),(1,4,92)",
		"INSERT INTO va VALUES(3,93),(4,94),(5,95)",
		"INSERT INTO vb VALUES(91,11),(92,22),(93,33),(94,44),(95,55)",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatalf("%s: %v", stmt, result.Error)
		}
	}
	result := db.Query("EXPLAIN QUERY PLAN SELECT pk_t.*,vv.* FROM vv LEFT JOIN pk_t ON b=c ORDER BY +b")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	var plan strings.Builder
	for _, row := range result.Rows {
		for _, value := range row {
			plan.WriteString(fmt.Sprint(value))
			plan.WriteByte(' ')
		}
	}
	if !strings.Contains(strings.ToUpper(plan.String()), "SEARCH PK_T USING AUTOMATIC COVERING INDEX") {
		t.Fatalf("plan = %q, want automatic covering index on composite-PK join", plan.String())
	}
}

func TestAutoindexPartialIndexOrderForWhereConstrainedCommaJoin(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE t1(a,b)",
		"INSERT INTO t1 VALUES(123,'abc'),(234,'def'),(234,'ghi'),(345,'jkl')",
		"CREATE TABLE t2(x,y)",
		"INSERT INTO t2 VALUES(987,'zyx'),(654,'wvu'),(987,'rqp')",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatalf("%s: %v", stmt, result.Error)
		}
	}
	result := db.Query("SELECT *, '|' FROM t1, t2 WHERE a=234 AND x=987 ORDER BY +b")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	got := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		got = append(got, fmt.Sprint(row[1], " ", row[3]))
	}
	want := []string{"def rqp", "def zyx", "ghi rqp", "ghi zyx"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("join rows = %v, want %v", got, want)
	}
}

func TestAutoindexPlannerUsesLeftJoinEquality(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE outer_t(k)",
		"CREATE TABLE inner_t(k, payload)",
		"INSERT INTO outer_t VALUES(1)",
		"INSERT INTO inner_t VALUES(1, 'x')",
	} {
		if result := db.Exec(stmt); result.Error != nil {
			t.Fatalf("%s: %v", stmt, result.Error)
		}
	}
	result := db.Query("EXPLAIN QUERY PLAN SELECT inner_t.* FROM outer_t LEFT JOIN inner_t ON outer_t.k=inner_t.k")
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	var plan strings.Builder
	for _, row := range result.Rows {
		for _, value := range row {
			plan.WriteString(fmt.Sprint(value))
			plan.WriteByte(' ')
		}
	}
	if !strings.Contains(strings.ToUpper(plan.String()), "SEARCH INNER_T USING AUTOMATIC COVERING INDEX") {
		t.Fatalf("plan = %q, want automatic covering index on LEFT JOIN RHS", plan.String())
	}
}
