package frigolite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/vtab"
)

// The bestindex native supplement pins the engine side of the virtual-table
// xBestIndex/xFilter contract (sqlite3_index_info) that the TCL bestindex*
// families exercise through the untranspilable `register_tcl_module` harness
// (plan/goals/P7.PLANNER.md T27). A fixture module records what the planner
// offers it and selects plans by mode, mirroring test/bestindex{1,2,4,5,9,
// A,D}.test assertions in engine-visible form.

// biCon is one recorded offered constraint.
type biCon struct {
	Column int
	Op     vtab.IndexConstraintOp
	Usable bool
	IsIn   bool
}

// biRecord is the shared observation sink across all instances a fixture
// module creates (SQLite's test harness stores observations in TCL globals).
type biRecord struct {
	cons       []biCon
	colsUsed   uint64
	orderBy    string
	distinct   int
	sawDefault bool // xBestIndex saw the planner-default estimatedCost
	argv       []interface{}
	filters    int
}

// biFixtureModule is the fixture module (columns a, b, c).
type biFixtureModule struct {
	mode string // plan555 | echo | omit | use | use2 | reject | malfunction | limitclaim
	rec  *biRecord
	rows [][]interface{} // static row set
}

type biFixtureVTab struct {
	mod *biFixtureModule
}

func (m *biFixtureModule) Create([]string) (vtab.VirtualTable, error) {
	return &biFixtureVTab{mod: m}, nil
}
func (m *biFixtureModule) Connect([]string) (vtab.VirtualTable, error) {
	return &biFixtureVTab{mod: m}, nil
}

// BestIndex satisfies vtab.VirtualTable (legacy byte-form; unused).
func (v *biFixtureVTab) BestIndex([]byte) ([]byte, error) { return nil, nil }

// Columns reports the declared schema (bestindex1 xConnect: a, b, c).
func (v *biFixtureVTab) Columns() []string { return []string{"a", "b", "c"} }

// BestIndexPlan implements vtab.PlanBestIndexer (xBestIndex).
func (v *biFixtureVTab) BestIndexPlan(ii *vtab.IndexInfo) error {
	r := v.mod.rec
	r.sawDefault = ii.EstimatedCost == 8.988465674311579e+307 // SQLITE_BIG_DBL/2
	r.colsUsed = ii.ColsUsed
	r.distinct = ii.Distinct
	var ob []string
	for _, o := range ii.OrderBy {
		ob = append(ob, fmt.Sprintf("%d:%v", o.Column, o.Desc))
	}
	r.orderBy = strings.Join(ob, ",")
	r.cons = r.cons[:0]
	for _, c := range ii.Constraints {
		r.cons = append(r.cons, biCon{Column: c.Column, Op: c.Op, Usable: c.Usable, IsIn: c.IsIn})
	}
	switch v.mod.mode {
	case "reject":
		return vtab.ErrVtabConstraint
	case "malfunction":
		// argvIndex 1 then 3: a gap (where.c "xBestIndex malfunction").
		ii.Usage[0].ArgvIndex = 1
		if len(ii.Usage) > 1 {
			ii.Usage[1].ArgvIndex = 3
		}
		return nil
	case "plan555":
		if len(ii.Constraints) == 1 && ii.Constraints[0].Usable {
			ii.IdxNum, ii.IdxStr = 555, "eq!"
			ii.EstimatedCost, ii.EstimatedRows = 0, 1
		} else {
			ii.EstimatedCost, ii.EstimatedRows = 1000000, 0
		}
		return nil
	case "echo":
		// bestindex2's vtab_cmd mirror: idxstr = indexed(a=? AND b=?).
		var parts []string
		for i, c := range ii.Constraints {
			if !c.Usable {
				continue
			}
			ii.Usage[i].ArgvIndex = i + 1
			parts = append(parts, fmt.Sprintf("%s=?", biColName(c.Column)))
		}
		if len(parts) > 0 {
			ii.IdxStr = "indexed(" + strings.Join(parts, " AND ") + ")"
		} else {
			ii.IdxStr = ""
		}
		return nil
	case "omit", "use", "use2":
		for i, c := range ii.Constraints {
			if c.Column == 0 && c.Op == vtab.IndexConstraintEq && c.Usable {
				ii.Usage[i].ArgvIndex = 1
				if v.mod.mode == "omit" {
					ii.Usage[i].Omit = true
				}
			}
		}
		ii.EstimatedCost, ii.EstimatedRows = 10, 10
		return nil
	case "limitclaim":
		n := 0
		for i, c := range ii.Constraints {
			if c.Usable && c.Op != vtab.IndexConstraintLimit && c.Op != vtab.IndexConstraintOffset {
				n++
				ii.Usage[i].ArgvIndex = n
			}
		}
		// Claim the LIMIT/OFFSET aux constraints last (they trail the list).
		for i, c := range ii.Constraints {
			if c.Usable && (c.Op == vtab.IndexConstraintLimit || c.Op == vtab.IndexConstraintOffset) {
				n++
				ii.Usage[i].ArgvIndex = n
				ii.Usage[i].Omit = true
			}
		}
		return nil
	}
	return nil
}

// biColName renders a constraint column index as the fixture schema name
// ("a"/"b"/"c"; -1 is the rowid).
func biColName(col int) string {
	if col < 0 {
		return "rowid"
	}
	return string(rune('a' + col))
}

// biFixtureCursor iterates the static rows, optionally filtered by the
// bound argv (mode omit/use implement filtering; use2 does not).
type biFixtureCursor struct {
	rows [][]interface{}
	pos  int
	mod  *biFixtureModule
}

func (v *biFixtureVTab) Open() (vtab.Cursor, error) {
	return &biFixtureCursor{rows: v.mod.rows, mod: v.mod}, nil
}

// FilterPlan implements vtab.PlanFilterer (xFilter).
func (c *biFixtureCursor) FilterPlan(idxNum int, idxStr string, argv []interface{}) error {
	c.mod.rec.filters++
	c.mod.rec.argv = argv
	if c.mod.mode == "omit" || c.mod.mode == "use" {
		if len(argv) > 0 && argv[0] != nil {
			want := fmt.Sprintf("%v", argv[0])
			var kept [][]interface{}
			for _, row := range c.rows {
				if fmt.Sprintf("%v", row[0]) == want {
					kept = append(kept, row)
				}
			}
			c.rows = kept
		}
	}
	return nil
}

// Next/Column use a 0-based iterator: pos is the NEXT row to serve, so the
// current row is rows[pos-1] after a true Next (vtab Cursor contract:
// Next positions the cursor, Column reads the current row).
func (c *biFixtureCursor) Next() bool {
	c.pos++
	return c.pos-1 < len(c.rows)
}
func (c *biFixtureCursor) Column(idx int) (interface{}, error) {
	row := c.rows[c.pos-1]
	if idx < len(row) {
		return row[idx], nil
	}
	return nil, fmt.Errorf("no column %d", idx)
}
func (c *biFixtureCursor) Close() error { return nil }

// rowsOf renders a query result in TCL space-joined form.
func biRows(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := db.Query(sql)
	if r.Error != nil {
		t.Fatalf("%s: query error: %v", sql, r.Error)
	}
	var parts []string
	for _, row := range r.Rows {
		for _, v := range row {
			if v == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, formatSQLiteValue(v))
			}
		}
	}
	return strings.Join(parts, " ")
}

func TestNativeBestIndex_plan555(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := &biRecord{}
	db.RegisterVtabModule("bi555", &biFixtureModule{mode: "plan555", rec: rec, rows: [][]interface{}{{"abc", 1, "x"}, {"def", 2, "y"}}})
	if res := db.Exec("CREATE VIRTUAL TABLE x1 USING bi555;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// bestindex1-1.1: usable EQ plan echoes idxnum/idxstr through EQP.
	if got := biRows(t, db, "EXPLAIN QUERY PLAN SELECT * FROM x1 WHERE a='abc'"); !strings.Contains(got, "SCAN x1 VIRTUAL TABLE INDEX 555:eq!") {
		t.Errorf("eqp 1.1: got [%s]", got)
	}
	// bestindex1-1.2: IN maps to EQ (same plan string).
	if got := biRows(t, db, "EXPLAIN QUERY PLAN SELECT * FROM x1 WHERE a IN ('abc','def')"); !strings.Contains(got, "SCAN x1 VIRTUAL TABLE INDEX 555:eq!") {
		t.Errorf("eqp 1.2: got [%s]", got)
	}
	if len(rec.cons) != 1 || rec.cons[0].Column != 0 || rec.cons[0].Op != vtab.IndexConstraintEq || !rec.cons[0].Usable || !rec.cons[0].IsIn {
		t.Errorf("1.2 constraint: got %+v", rec.cons)
	}
	if !rec.sawDefault {
		t.Errorf("xBestIndex did not observe planner-default estimatedCost")
	}
}

func TestNativeBestIndex_omitUseRows(t *testing.T) {
	rows := [][]interface{}{{"abc", 1, "x"}, {"def", 2, "y"}, {"abc", 3, "z"}}
	for _, mode := range []string{"omit", "use", "use2"} {
		db, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		rec := &biRecord{}
		db.RegisterVtabModule("bifilter", &biFixtureModule{mode: mode, rec: rec, rows: rows})
		if res := db.Exec("CREATE VIRTUAL TABLE t1 USING bifilter;"); res.Error != nil {
			t.Fatal(res.Error)
		}
		// bestindex2: omit (module filters, core skips re-check), use
		// (module filters AND core re-checks), use2 (module ignores the
		// constraint; the core re-check must still produce the right rows).
		got := biRows(t, db, "SELECT b FROM t1 WHERE a='abc'")
		if want := "1 3"; got != want {
			t.Errorf("%s: got [%s] want [%s]", mode, got, want)
		}
		if rec.filters != 1 {
			t.Errorf("%s: xFilter calls = %d", mode, rec.filters)
		}
		if mode != "use2" && (len(rec.argv) == 0 || fmt.Sprintf("%v", rec.argv[0]) != "abc") {
			t.Errorf("%s: argv = %v", mode, rec.argv)
		}
		db.Close()
	}
}

func TestNativeBestIndex_constraintEnumeration(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := &biRecord{}
	db.RegisterVtabModule("biecho", &biFixtureModule{mode: "echo", rec: rec})
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING biecho;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// bestindexA: enumeration order + op codes; a reusable normalization
	// keeps the column on the left; (b+1)=? is not a plain column term.
	_ = biRows(t, db, "SELECT * FROM t1 WHERE a=1 AND b>2 AND (c+1)=3")
	want := []biCon{
		{Column: 0, Op: vtab.IndexConstraintEq, Usable: true},
		{Column: 1, Op: vtab.IndexConstraintGt, Usable: true},
	}
	if len(rec.cons) != 2 {
		t.Errorf("enumeration: got %+v, want exactly the two plain-column terms", rec.cons)
	} else {
		for i, c := range want {
			if rec.cons[i] != c {
				t.Errorf("constraint %d: got %+v want %+v", i, rec.cons[i], c)
			}
		}
	}
	// Reversed operand order normalizes to column-on-left with the
	// operator mirrored (5<a ⇔ a>5 — where.c exprAnalyze commutation).
	_ = biRows(t, db, "SELECT * FROM t1 WHERE 5<a")
	if len(rec.cons) != 1 || rec.cons[0].Column != 0 || rec.cons[0].Op != vtab.IndexConstraintGt {
		t.Errorf("reversed operand: got %+v", rec.cons)
	}
	// IN offers EQ with the IN flag.
	_ = biRows(t, db, "SELECT * FROM t1 WHERE a IN (1,2)")
	if len(rec.cons) != 1 || rec.cons[0].Op != vtab.IndexConstraintEq || !rec.cons[0].IsIn {
		t.Errorf("IN: got %+v", rec.cons)
	}
}

func TestNativeBestIndex_limitOffsetConstraints(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := &biRecord{}
	db.RegisterVtabModule("bilimit", &biFixtureModule{mode: "limitclaim", rec: rec,
		rows: [][]interface{}{{"a", 1, "x"}, {"b", 2, "y"}, {"c", 3, "z"}, {"d", 4, "w"}}})
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING bilimit;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	got := biRows(t, db, "SELECT b FROM t1 WHERE a>'a' LIMIT 2 OFFSET 1")
	if want := "3 4"; got != want {
		t.Errorf("limit/offset rows: got [%s] want [%s]", got, want)
	}
	// sqlite3WhereAddLimit: OFFSET then LIMIT aux constraints trail the list.
	n := len(rec.cons)
	if n < 2 || rec.cons[n-2].Op != vtab.IndexConstraintOffset || rec.cons[n-1].Op != vtab.IndexConstraintLimit {
		t.Errorf("aux constraints: got %+v", rec.cons[n-2:])
	}
}

func TestNativeBestIndex_orderByDistinct(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := &biRecord{}
	db.RegisterVtabModule("biob", &biFixtureModule{mode: "echo", rec: rec})
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING biob;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// bestindex9: orderby + distinct observations.
	_ = biRows(t, db, "SELECT * FROM t1 ORDER BY a")
	if rec.orderBy != "0:false" {
		t.Errorf("orderby a: got [%s]", rec.orderBy)
	}
	_ = biRows(t, db, "SELECT * FROM t1 ORDER BY a DESC, b")
	if rec.orderBy != "0:true,1:false" {
		t.Errorf("orderby a DESC, b: got [%s]", rec.orderBy)
	}
	// Non-vtab ORDER BY terms disqualify the aOrderBy offer.
	_ = biRows(t, db, "SELECT * FROM t1 ORDER BY a+1")
	if rec.orderBy != "" {
		t.Errorf("orderby a+1: got [%s]", rec.orderBy)
	}
	_ = biRows(t, db, "SELECT DISTINCT a FROM t1")
	if rec.distinct != 2 {
		t.Errorf("distinct flag: got %d want 2", rec.distinct)
	}
	_ = biRows(t, db, "SELECT a FROM t1 GROUP BY a")
	if rec.distinct != 1 {
		t.Errorf("groupby flag: got %d want 1", rec.distinct)
	}
}

func TestNativeBestIndex_colUsed(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rec := &biRecord{}
	db.RegisterVtabModule("bimask", &biFixtureModule{mode: "echo", rec: rec})
	if res := db.Exec("CREATE VIRTUAL TABLE x1 USING bimask;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	// bestindexD: colUsed mask matches the columns the statement uses.
	_ = biRows(t, db, "SELECT a, c FROM x1 WHERE b=1")
	want := uint64(0b111)
	if rec.colsUsed != want {
		t.Errorf("colUsed a,c+b: got %b want %b", rec.colsUsed, want)
	}
	_ = biRows(t, db, "SELECT * FROM x1")
	want = uint64(0b111)
	if rec.colsUsed != want {
		t.Errorf("colUsed *: got %b want %b", rec.colsUsed, want)
	}
	_ = biRows(t, db, "SELECT b FROM x1")
	want = uint64(0b010)
	if rec.colsUsed != want {
		t.Errorf("colUsed b: got %b want %b", rec.colsUsed, want)
	}
}

func TestNativeBestIndex_rejectAndMalfunction(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// SQLITE_CONSTRAINT from xBestIndex rejects the plan; the statement
	// still runs with the plain materialization + core WHERE re-check.
	rec := &biRecord{}
	db.RegisterVtabModule("bireject", &biFixtureModule{mode: "reject", rec: rec,
		rows: [][]interface{}{{"abc", 1, "x"}, {"def", 2, "y"}}})
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING bireject;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := biRows(t, db, "SELECT b FROM t1 WHERE a='abc'"); got != "1" {
		t.Errorf("rejected plan rows: got [%s] want [1]", got)
	}
	// A gap in argvIndex is an xBestIndex malfunction (where.c:4366).
	db2, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.RegisterVtabModule("bimal", &biFixtureModule{mode: "malfunction", rec: &biRecord{}})
	if res := db2.Exec("CREATE VIRTUAL TABLE t1 USING bimal;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	r := db2.Query("SELECT * FROM t1 WHERE a=1 AND b=2")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "xBestIndex malfunction") {
		t.Errorf("malfunction: got %v", r.Error)
	}
}

func TestNativeBestIndex_inExpansion(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// omit mode implements filtering: one xFilter per IN value, streams
	// concatenated in list order (where.c IN-to-EQ parity).
	rec := &biRecord{}
	db.RegisterVtabModule("biin", &biFixtureModule{mode: "omit", rec: rec,
		rows: [][]interface{}{{"abc", 1, "x"}, {"def", 2, "y"}, {"abc", 3, "z"}, {"ghi", 4, "w"}}})
	if res := db.Exec("CREATE VIRTUAL TABLE t1 USING biin;"); res.Error != nil {
		t.Fatal(res.Error)
	}
	if got := biRows(t, db, "SELECT b FROM t1 WHERE a IN ('def','abc')"); got != "2 1 3" {
		t.Errorf("IN stream order: got [%s] want [2 1 3]", got)
	}
	if rec.filters != 2 {
		t.Errorf("xFilter calls = %d want 2", rec.filters)
	}
}
