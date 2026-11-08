package execquery_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
)

// SQLITE_TEST's sqlite3_search_count over the index-driven walks
// (where8-B card). The counter increments are exactly the VDBE's
// (vdbe.c:4973, next_tail; sqlite3VdbeFinishMoveto):
//
//	+1 per OP_SeekLT/LE/GE/GT execution — unconditional, the seek counts
//	    even when it finds nothing;
//	+1 per successful OP_Next/OP_Prev positioning — the final step onto
//	    EOF fails and does not count, while the final step onto the range's
//	    boundary successor SUCCEEDS and does;
//	+1 per deferred table-row moveto (the index-driven row fetch) — issued
//	    per candidate entry before the residual WHERE runs, so filtered
//	    candidates count too;
//	OP_Rewind/OP_Last/OP_SeekRowid are NOT counted (an upper-bounded range
//	    with no start bound opens its loop with OP_Rewind).
//
// where8-1.8 (`SELECT c FROM t1 WHERE a > 1 AND c LIKE 'I%'` over the
// where8-1.1 fixture: 9 rows a=2..10, i1(a)) is the oracle anchor: "18
// searches. 9 on the index cursor and 9 on the table cursor" — 1 seek + 8
// successful Nexts + 9 movetos. where8-1.12.1 (`a IN(1, 2, 3) OR a = 5`)
// wants 14: four OR branches (per IN value 1 seek + 1 moveto + 1 boundary
// Next; the a=5 branch the same) plus the IN value loop's 2 successful
// ephemeral Nexts. Both leaves were red before this card: the executor
// full-scanned (scan 9) wherever the blanket LIKE gate disconnected it from
// the planner's SEARCH plan, and the index walks counted nothing at all.
func TestPinIndexSeekWalkSearchCounts(t *testing.T) {
	e := newSeekWalkPinEngine(t)
	probe := func(sqlText string) (rows string, scan, sort, search int64) {
		t.Helper()
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
		var flat []string
		for _, row := range last.Rows {
			for _, v := range row {
				if v == nil {
					flat = append(flat, "NULL")
				} else {
					flat = append(flat, fmt.Sprint(v))
				}
			}
		}
		return fmt.Sprint(flat), e.StmtStatusFullScanStep(), e.StmtStatusSort(), e.SearchCount()
	}
	reset := func() {
		e.ResetSearchCount()
		e.ResetStmtStatus()
	}

	type pin struct {
		sql    string
		rows   string
		scan   int64
		sort   int64
		search int64
	}
	pins := []pin{
		// where8-1.8 exactly: the plan is SEARCH t1 USING INDEX i1 (a>?),
		// the LIKE conjunct rides along as a residual filter.
		{`SELECT c FROM t1 WHERE a > 1 AND c LIKE 'I%'`, "[II III IV IX]", 0, 0, 18},
		// where8-1.12.1 exactly: the IN arms plan as per-value equality
		// branches; the value loops' ephemeral Nexts complete the count.
		{`SELECT c FROM t1 WHERE a IN(1, 2, 3) OR a = 5`, "[I II III V]", 0, 0, 14},
		// The same walk without the residual filter: 1 seek + 8 Nexts +
		// 9 movetos.
		{`SELECT c FROM t1 WHERE a > 1`, "[II III IV V VI VII VIII IX X]", 0, 0, 18},
		// A covering loop opens no table cursor: 1 seek + 8 Nexts, no
		// movetos.
		{`SELECT a FROM t1 WHERE a > 1`, "[2 3 4 5 6 7 8 9 10]", 0, 0, 9},
		// An upper-bounded range starts at OP_Rewind (not counted): 4
		// movetos + 4 successful Nexts (the last steps onto the boundary
		// successor a=5 and succeeds).
		{`SELECT c FROM t1 WHERE a < 5`, "[I II III IV]", 0, 0, 8},
		// A single-row equality seek: 1 seek + 1 boundary Next + 1 moveto.
		{`SELECT c FROM t1 WHERE a = 5`, "[V]", 0, 0, 3},
		// A rowid probe is OP_SeekRowid: not counted.
		{`SELECT c FROM t1 WHERE rowid = 5`, "[V]", 0, 0, 0},
		// A LIKE conjunct that does NOT drive the plan (the planner picks
		// i1 for a=3): the seek honors the plan, the LIKE stays residual.
		{`SELECT c FROM t1 WHERE b LIKE 't%' AND a = 3`, "[III]", 0, 0, 3},
		// A complete GLOB term driving the plan keeps the regular scan (the
		// scan-local optimizer's exact matcher-call elision — the
		// sqlite3_like_count contract), so its count is the full scan's.
		{`SELECT c FROM t1 WHERE a GLOB '5'`, "[V]", 9, 0, 9},
		// An IN arm beside a non-IN arm: two eq branches (3 each) + the
		// 2-value loop's 1 ephemeral Next.
		{`SELECT c FROM t1 WHERE a IN(1, 2) OR b = 'nine'`, "[I II IX]", 0, 0, 10},
	}
	for _, p := range pins {
		reset()
		rows, scan, sort, search := probe(p.sql)
		if rows != p.rows || scan != p.scan || sort != p.sort || search != p.search {
			t.Errorf("%s\n  got  rows=%s scan=%d sort=%d search=%d\n  want rows=%s scan=%d sort=%d search=%d",
				p.sql, rows, scan, sort, search, p.rows, p.scan, p.sort, p.search)
		}
	}
}

// newSeekWalkPinEngine builds an in-memory engine holding the where8-1.1
// fixture: t1(a, b TEXT, c) with i1(a) and i2(b), ten rows.
func newSeekWalkPinEngine(t *testing.T) *exec.Engine {
	t.Helper()
	e := exec.NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := e.Schema().Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	for _, s := range []string{
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
	return e
}
