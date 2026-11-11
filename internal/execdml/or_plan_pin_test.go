package execdml_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
)

// SQLITE_TEST's sqlite3_search_count over the OR-plan's shapes (whereD's
// do_searchcount_test family). The MULTI-INDEX OR loop's per-arm counts,
// all pinned against the 3.51.0 testfixture bytecode:
//
//	an index arm counts 1 seek + its successful Nexts (the boundary step
//	    onto the range's successor succeeds — the beyond[] model) + one
//	    deferred moveto per row it emits whose OWN index does not cover
//	    the output columns (vdbe.c:3022's aAltMap redirect serves covered
//	    reads from the index cursor without finishing the moveto —
//	    per-arm, not per-plan: 6.3.2's x1a arm lacks c, its x1bc arm
//	    covers it);
//	a rowid-equality arm runs OP_SeekRowid: no seek count, no Next loop,
//	    no moveto (the table cursor is positioned directly) — C renders it
//	    "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)";
//	a rowid-range arm seeks the table b-tree (counted) and walks it (the
//	    boundary Next counts when any rowid lies past the bound), with no
//	    movetos;
//	an uncorrelated subquery bound as a branch constraint is evaluated
//	    exactly once, at plan-build time (C's OP_Once + register probe) —
//	    the subquery's own seek + row fetch land in the count;
//	a consumed constraint never re-runs per candidate row (the residual
//	    filter holds only the terms the plan did not compile into its
//	    seek).
//
// The rows pin the arm order too: the union walks the branches in source
// order, so 6.5.4 (`c=11 OR rowid=2`) emits the c-arm rows first.
func TestPinOrPlanSearchCounts(t *testing.T) {
	e := exec.NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := e.Schema().Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE t3(a, b, c)`,
		`CREATE UNIQUE INDEX i3 ON t3(a, b)`,
		`INSERT INTO t3 VALUES(1, 'one', 'i')`,
		`INSERT INTO t3 VALUES(3, 'three', 'iii')`,
		`INSERT INTO t3 VALUES(6, 'six', 'vi')`,
		`INSERT INTO t3 VALUES(2, 'two', 'ii')`,
		`INSERT INTO t3 VALUES(4, 'four', 'iv')`,
		`INSERT INTO t3 VALUES(5, 'five', 'v')`,
		`CREATE TABLE t4(x PRIMARY KEY, y)`,
		`INSERT INTO t4 VALUES('a', 'one')`,
		`INSERT INTO t4 VALUES('b', 'two')`,
		`CREATE TABLE x1(a, b, c, d, e)`,
		`CREATE INDEX x1a ON x1(a)`,
		`CREATE INDEX x1bc ON x1(b, c)`,
		`CREATE INDEX x1cd ON x1(c, d)`,
		`INSERT INTO x1 VALUES(1, 2, 3, 4, 'A')`,
		`INSERT INTO x1 VALUES(5, 6, 7, 8, 'B')`,
		`INSERT INTO x1 VALUES(9, 10, 11, 12, 'C')`,
		`INSERT INTO x1 VALUES(13, 14, 15, 16, 'D')`,
		`CREATE TABLE t1(a, b TEXT, c)`,
		`CREATE INDEX i1 ON t1(a)`,
		`CREATE INDEX i2 ON t1(b)`,
		`INSERT INTO t1 VALUES(1, 'one', 'I')`,
		`INSERT INTO t1 VALUES(2, 'two', 'II')`,
		`INSERT INTO t1 VALUES(3, 'three', 'III')`,
		`INSERT INTO t1 VALUES(4, 'four', 'IV')`,
		`INSERT INTO t1 VALUES(5, 'five', 'V')`,
		`INSERT INTO t1 VALUES(6, 'six', 'VI')`,
		`INSERT INTO t1 VALUES(7, 'seven', 'VII')`,
		`INSERT INTO t1 VALUES(8, 'eight', 'VIII')`,
		`INSERT INTO t1 VALUES(9, 'nine', 'IX')`,
		`INSERT INTO t1 VALUES(10, 'ten', 'X')`,
	} {
		stmts, err := parse.ParseSQL(s)
		if err != nil {
			t.Fatalf("%s: parse: %v", s, err)
		}
		for _, st := range stmts {
			if res := e.Exec(st); res.Error != nil {
				t.Fatalf("%s: exec: %v", s, res.Error)
			}
		}
	}
	probe := func(sqlText string) (rows string, search int64) {
		stmts, err := parse.ParseSQL(sqlText)
		if err != nil {
			t.Fatalf("%s: parse: %v", sqlText, err)
		}
		var last *exec.Result
		for _, st := range stmts {
			last = e.Exec(st)
			if last.Error != nil {
				t.Fatalf("%s: exec: %v", sqlText, last.Error)
			}
		}
		return fmt.Sprint(last.Rows), e.SearchCount()
	}
	type pin struct {
		sql    string
		rows   string
		search int64
	}
	pins := []pin{
		// whereD-3.1/3.2: the covered pair vs the moveto pair.
		{`SELECT a, b FROM t3 WHERE (a=1 AND b='one') OR (a=2 AND b='two')`, "[[1 one] [2 two]]", 4},
		{`SELECT a, c FROM t3 WHERE (a=1 AND b='one') OR (a=2 AND b='two')`, "[[1 i] [2 ii]]", 6},
		// whereD-3.4.1: the t4 unique-x probe: seek + row fetch, no Next.
		{`SELECT y FROM t4 WHERE x='a'`, "[[one]]", 2},
		// whereD-3.4.2/3.4.3/3.4.4: the subquery arm — bound once at
		// plan-build, its seek + moveto land once; the OR arms keep their
		// Next loops.
		{`SELECT a, b FROM t3 WHERE (a=1 AND b=(SELECT y FROM t4 WHERE x='a')) OR (a=2 AND b='two')`, "[[1 one] [2 two]]", 6},
		{`SELECT a, b FROM t3 WHERE (a=2 AND b='two') OR (a=1 AND b=(SELECT y FROM t4 WHERE x='a'))`, "[[2 two] [1 one]]", 6},
		{`SELECT a, b FROM t3 WHERE (a=2 AND b=(SELECT y FROM t4 WHERE x='b')) OR (a=1 AND b=(SELECT y FROM t4 WHERE x='a'))`, "[[2 two] [1 one]]", 8},
		// whereD-3.5.1/3.5.2: the rowid arm contributes nothing; the moveto
		// is per-arm (the covered output vs the c read).
		{`SELECT a, b FROM t3 WHERE (a=1 AND b='one') OR rowid=4`, "[[1 one] [2 two]]", 2},
		{`SELECT a, c FROM t3 WHERE (a=1 AND b='one') OR rowid=4`, "[[1 i] [2 ii]]", 3},
		// whereD-6.2.x: both arms cover c / neither covers e.
		{`SELECT e FROM x1 WHERE b=2 OR c=7`, "[[A] [B]]", 6},
		{`SELECT c FROM x1 WHERE b=2 OR c=7`, "[[3] [7]]", 4},
		// whereD-6.3.x: the per-arm moveto (x1a lacks c; rowid never
		// fetches).
		{`SELECT e FROM x1 WHERE a=1 OR b=10`, "[[A] [C]]", 6},
		{`SELECT c FROM x1 WHERE a=1 OR b=10`, "[[3] [11]]", 5},
		{`SELECT rowid FROM x1 WHERE a=1 OR b=10`, "[[1] [3]]", 4},
		// whereD-6.4.x: BETWEEN arms with per-arm coverage.
		{`SELECT a FROM x1 WHERE b BETWEEN 1 AND 4 OR c BETWEEN 8 AND 12`, "[[1] [9]]", 6},
		{`SELECT b, c FROM x1 WHERE b BETWEEN 1 AND 4 OR c BETWEEN 8 AND 12`, "[[2 3] [10 11]]", 5},
		{`SELECT rowid, c FROM x1 WHERE b BETWEEN 1 AND 4 OR c BETWEEN 8 AND 12`, "[[1 3] [3 11]]", 4},
		// whereD-6.5.x: the rowid arm's position (source order) and its
		// zero contribution.
		{`SELECT a FROM x1 WHERE rowid = 2 OR c=11`, "[[5] [9]]", 3},
		{`SELECT d FROM x1 WHERE rowid = 2 OR c=11`, "[[8] [12]]", 2},
		{`SELECT d FROM x1 WHERE c=11 OR rowid = 2`, "[[12] [8]]", 2},
		{`SELECT a FROM x1 WHERE c=11 OR rowid = 2`, "[[9] [5]]", 3},
		// whereD-6.6.x: three arms — one moveto from the x1a arm only.
		{`SELECT rowid FROM x1 WHERE a=1 OR b=6 OR c=11`, "[[1] [2] [3]]", 6},
		{`SELECT c FROM x1 WHERE a=1 OR b=6 OR c=11`, "[[3] [7] [11]]", 7},
		{`SELECT c FROM x1 WHERE c=11 OR a=1 OR b=6`, "[[11] [3] [7]]", 7},
		{`SELECT c FROM x1 WHERE b=6 OR c=11 OR a=1`, "[[7] [11] [3]]", 7},
		// where8-1.10: the residual read of an uncovered column fires the
		// moveto per candidate — row 10 (rejected by `c != 'X'`) counts.
		{`SELECT c FROM t1 WHERE (a >= 9 AND c != 'X') OR b <= 'eight'`, "[[IX] [VIII]]", 7},
		// where8-1.4a: the duplicate row 10 in the GLOB arm does NOT count
		// (its output read is skipped by the RowSetTest; the arm has no
		// residual reads).
		{`SELECT c FROM t1 WHERE a > 8 OR b GLOB 't*'`, "[[IX] [X] [III] [II]]", 10},
	}
	for _, p := range pins {
		e.ResetSearchCount()
		rows, search := probe(p.sql)
		if rows != p.rows || search != p.search {
			t.Errorf("%s\n  got  rows=%s search=%d\n  want rows=%s search=%d",
				p.sql, rows, search, p.rows, p.search)
		}
	}
}
