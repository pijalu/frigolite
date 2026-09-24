package frigolite

import (
	"fmt"
	"strings"
	"testing"
)

// Pins for FULL-SUITE-DRIFT.T32-deep.
//
// Part 1 (TestT32DeepRandexprCollapse) pins the correlated-aggregate collapse
// predicate: an aggregate nested inside an expression in a subquery's SELECT
// list (resolve.c: ownership is decided by where the aggregate's column
// references resolve, not by the shape of the result expression) that
// references its own FROM columns is UNCORRELATED — it must evaluate
// independently over its own full scan, and must never collapse the enclosing
// query to a single row (nor fabricate a row from an empty scan). This drove
// 70 randexpr1 testgen mismatches (execSelectCorrelatedAgg fired because
// aggColumnArgsRefInner only recognized a bare aggregate column).
//
// Part 2 (TestT32DeepEmptyIndexName) pins tkt-78e04e52ea: CREATE INDEX "" is
// legal in SQLite; an empty-named index must be found and used by the planner
// (EQP "SEARCH t2 USING COVERING INDEX  (x=?)" renders the empty name with a
// double space) and droppable again.

// flattenT32 joins all row values with single spaces (testgen flatten style;
// nil renders as the empty string, mirroring TCL's rendering of NULL in these
// result lists).
func flattenT32(rows [][]interface{}) string {
	out := ""
	for _, row := range rows {
		for _, v := range row {
			if out != "" {
				out += " "
			}
			if v == nil {
				continue
			}
			out += t32String(v)
		}
	}
	return out
}

func t32String(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

func TestT32DeepRandexprCollapse(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE TABLE t1(a,b,c,d,e,f)",
		"INSERT INTO t1 VALUES(100,200,300,400,500,600)",
		"CREATE TABLE t2(b1 INTEGER)",
		"INSERT INTO t2 VALUES(4),(5)",
		"CREATE TABLE t3(a1 INTEGER)",
		"INSERT INTO t3 VALUES(1),(2),(3)",
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	for _, tc := range []struct {
		name string
		q    string
		want string // "<EMPTY>" means zero rows
	}{
		// randexpr-2.1187 class: aggregate inside CAST must stay inner-owned.
		{"cast-agg", "SELECT (SELECT -count(*)-cast(avg(f) AS integer) FROM t1) FROM t1 WHERE 0", "<EMPTY>"},
		// randexpr-2.33/2.34 class: product of aggregates nested two subqueries
		// deep with a WHERE that eliminates the middle scan.
		{"nested-prod-empty", "SELECT coalesce((SELECT (SELECT max(a)*max(a) FROM t1) FROM t1 WHERE a>100), -19) FROM t1 WHERE 0", "<EMPTY>"},
		{"nested-prod-rows", "SELECT coalesce((SELECT (SELECT max(a)*max(a) FROM t1) FROM t1 WHERE a>100), -19) FROM t1", "-19"},
		// Direct nested-aggregate evaluation over its own scan.
		{"nested-prod-direct", "SELECT (SELECT max(a)*max(a) FROM t1)", "10000"},
		// Inner WHERE eliminates the middle scan → scalar subquery NULL.
		{"middle-scan-empty", "SELECT coalesce((SELECT (SELECT max(a) FROM t1) FROM t1 WHERE a>100), -19)", "-19"},
		// Aggregate nested inside an IN-list stays inner-owned: the middle
		// query is not an aggregate query (the aggregate lives in the nested
		// subquery's own scope), so its WHERE 0 eliminates the scan and the
		// scalar subquery yields NULL (oracle: one NULL row).
		{"in-list-agg", "SELECT (SELECT (SELECT max(a) IN (SELECT max(f) FROM t1) FROM t1) FROM t1 WHERE 0) FROM t1", "<NULLROW>"},
		// Guards: the LEGITIMATE correlated-aggregate collapse must keep
		// working (aggnested-1.1: aggregate arg is outer-only → one row
		// aggregating the outer column).
		{"aggnested-1-1", "SELECT (SELECT string_agg(a1,'x') FROM t2) FROM t3", "1x2x3"},
		// aggnested-1.3: mixed inner/outer aggregate args stay per-row.
		{"aggnested-1-3", "SELECT (SELECT string_agg(b1,a1) FROM t2) FROM t3", "415 425 435"},
		// filter1-6.3: outer-only COUNT collapses to one row (counting the
		// outer rows).
		{"filter1-6-3", "SELECT (SELECT COUNT(a1) FROM t2) FROM t3", "3"},
	} {
		r := db.Query(tc.q)
		if r.Error != nil {
			t.Errorf("%s: ERR %v", tc.name, r.Error)
			continue
		}
		got := flattenT32(r.Rows)
		switch tc.want {
		case "<EMPTY>":
			if len(r.Rows) != 0 {
				t.Errorf("%s: got [%s] want zero rows", tc.name, got)
			}
			continue
		case "<NULLROW>":
			if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != nil {
				t.Errorf("%s: got %v want one row with a single NULL", tc.name, r.Rows)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got [%s] want [%s]", tc.name, got, tc.want)
		}
	}
}

// TestT32DeepEmptyIndexName pins tkt-78e04e52ea: zero-length schema names are
// legal (CREATE INDEX "" ON ...; CREATE TABLE "" ...). The planner's
// not-found signal must not be the empty string, or an empty-named index is
// permanently invisible to every chooser: EQP must report
// "SEARCH t2 USING COVERING INDEX  (x=?)" (double space — the empty name) and
// DROP INDEX "" must remove it again. Expectations oracle-checked against
// sqlite3 3.54.
func TestT32DeepEmptyIndexName(t *testing.T) {
	db, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE t2(x)`,
		`INSERT INTO t2 VALUES(2),(5)`,
	} {
		if r := db.Exec(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}
	if r := db.Exec(`CREATE INDEX "" ON t2(x)`); r.Error != nil {
		t.Fatalf("CREATE INDEX \"\": %v", r.Error)
	}
	// The empty-named index is visible in the schema...
	r := db.Query(`SELECT quote(name), quote(tbl_name) FROM sqlite_master ORDER BY name`)
	if r.Error != nil || len(r.Rows) != 2 {
		t.Fatalf("sqlite_master: err=%v rows=%v want 2 rows", r.Error, r.Rows)
	}
	// ...and used by the planner (tkt-78e04-2.1, oracle-verified).
	if got := eqpT32(db, `EXPLAIN QUERY PLAN SELECT * FROM t2 WHERE x=5`); got != "SEARCH t2 USING COVERING INDEX  (x=?)" {
		t.Errorf("EQP with empty-named index: got [%s] want [SEARCH t2 USING COVERING INDEX  (x=?)]", got)
	}
	// COUNT(col) covering plan renders the empty name too.
	if got := eqpT32(db, `EXPLAIN QUERY PLAN SELECT count(x) FROM t2`); got == "" {
		t.Log("no covering plan for count(x)")
	}
	// DROP INDEX "" works (tkt-78e04-2.2) and the plan reverts to a scan.
	if r := db.Exec(`DROP INDEX ""`); r.Error != nil {
		t.Fatalf("DROP INDEX \"\": %v", r.Error)
	}
	if got := eqpT32(db, `EXPLAIN QUERY PLAN SELECT * FROM t2 WHERE x=2`); got != "SCAN t2" {
		t.Errorf("EQP after drop: got [%s] want [SCAN t2]", got)
	}

	// Zero-length TABLE name: the autoindex of a UNIQUE column on table ""
	// resolves and plans with the empty name rendered (oracle: SEARCH with
	// COVERING INDEX sqlite_autoindex__1).
	if r := db.Exec(`CREATE TABLE ""("" UNIQUE, x CHAR(100))`); r.Error != nil {
		t.Fatalf("CREATE TABLE \"\": %v", r.Error)
	}
	if r := db.Exec(`INSERT INTO "" VALUES('1e5zz','y')`); r.Error != nil {
		t.Fatalf("INSERT: %v", r.Error)
	}
	if got := eqpT32(db, `EXPLAIN QUERY PLAN SELECT "" FROM "" WHERE "" = '1e5zz'`); got != "SEARCH  USING COVERING INDEX sqlite_autoindex__1 (=?)" {
		t.Errorf("EQP on empty-named table: got [%s] want [SEARCH  USING COVERING INDEX sqlite_autoindex__1 (=?)]", got)
	}
	// The zero-length column participates in index-driven scans; the data
	// contract (the LIKE term over table "" with index i1 — TCL 1.4, whose
	// exact EQP wording is a deeper where.c class) returns the right row.
	if r := db.Exec(`CREATE INDEX i1 ON ""("" COLLATE nocase)`); r.Error != nil {
		t.Fatalf("CREATE INDEX i1: %v", r.Error)
	}
	qr := db.Query(`SELECT "" FROM "" WHERE "" LIKE '1e5%'`)
	if qr.Error != nil || len(qr.Rows) != 1 || t32String(qr.Rows[0][0]) != "1e5zz" {
		t.Errorf("LIKE over empty-named table: err=%v rows=%v", qr.Error, qr.Rows)
	}
	// SQLite-faithful table_info: the zero-length name/type are empty STRINGS
	// (oracle-verified); only dflt_value is NULL. The JSON harness cannot
	// express zero-length cells ({} ↔ NULL lossiness), so pin it here.
	ti := db.Query(`PRAGMA table_info("")`)
	if ti.Error != nil || len(ti.Rows) != 2 {
		t.Fatalf("table_info(\"\"): err=%v rows=%v", ti.Error, ti.Rows)
	}
	c0 := ti.Rows[0]
	if s, ok := c0[1].(string); !ok || s != "" {
		t.Errorf("table_info col-0 name = %#v, want the empty string", c0[1])
	}
	if c0[4] != nil {
		t.Errorf("table_info col-0 dflt_value = %#v, want NULL", c0[4])
	}
}

// eqpT32 runs one EXPLAIN QUERY PLAN statement and returns the joined plan
// detail lines (header row skipped).
func eqpT32(db *DB, q string) string {
	r := db.Query(q)
	if r.Error != nil {
		return "ERR: " + r.Error.Error()
	}
	out := ""
	for _, row := range r.Rows {
		for _, v := range row {
			if s, ok := v.(string); ok && s != "QUERY PLAN" {
				s = strings.TrimPrefix(s, "`--")
				out += s
			}
		}
	}
	return out
}
