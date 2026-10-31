package execexpr_test

// Native pin for the P-C6 function auxiliary data (sqlite3_get_auxdata /
// sqlite3_set_auxdata). It drives the engine's Exec path with a Go port of
// test_func.c's test_auxdata() and mirrors the func.test 13.x expectations
// (SQLite 3.54), plus the statement-teardown, destructor, error-path, and
// 32-bit-mask argument rules.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/exec"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/vtab"
)

// auxTextOf renders one argument as sqlite3_value_text would; ok=false for
// NULL (test_auxdata skips NULL arguments entirely — test_func.c:198).
func auxTextOf(v interface{}) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case []byte:
		return string(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	default:
		return fmt.Sprint(x), true
	}
}

// newBareEngine builds an initialized in-memory engine.
func newBareEngine(t *testing.T) *exec.Engine {
	t.Helper()
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	e := exec.NewEngine(pg)
	if err := e.Databases()["MAIN"].Schema.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	return e
}

// newAuxEngine builds an in-memory engine with t4(a,b) holding two rows and
// test_auxdata registered (the test_func.c:185 port: per argument, '1' when
// get_auxdata returns non-NULL — asserting the stored text equals the
// current argument text — else '0', then set a copy of the argument text).
func newAuxEngine(t *testing.T) *exec.Engine {
	t.Helper()
	e := newBareEngine(t)
	e.Functions().RegisterContext("test_auxdata", func(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
		out := make([]byte, len(args)*2)
		for i, arg := range args {
			z, ok := auxTextOf(arg)
			if !ok {
				continue
			}
			if prev := ctx.GetAuxData(i); prev != nil {
				out[i*2] = '1'
				if prev.(string) != z {
					return nil, fmt.Errorf("test_auxdata: stored %q != arg %q", prev, z)
				}
			} else {
				out[i*2] = '0'
			}
			ctx.SetAuxData(i, z, nil)
			out[i*2+1] = ' '
		}
		return string(out[:len(args)*2-1]), nil
	}, 1, -1)
	execSQL(t, e, "CREATE TABLE t4(a, b)",
		"INSERT INTO t4 VALUES('abc', 'def')",
		"INSERT INTO t4 VALUES('ghi', 'jkl')")
	return e
}

// execSQL runs each statement and fails the test on the first error.
func execSQL(t *testing.T, e *exec.Engine, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		parsed, err := parse.ParseSQL(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		for _, st := range parsed {
			if res := e.Exec(st); res.Error != nil {
				t.Fatalf("exec %q: %v", s, res.Error)
			}
		}
	}
}

// queryRows runs one SELECT and returns the rendered first-column rows.
func queryRows(t *testing.T, e *exec.Engine, sel string) []string {
	t.Helper()
	parsed, err := parse.ParseSQL(sel)
	if err != nil {
		t.Fatalf("parse %q: %v", sel, err)
	}
	res := e.Exec(parsed[0])
	if res.Error != nil {
		t.Fatalf("exec %q: %v", sel, res.Error)
	}
	out := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		out[i] = fmt.Sprint(row[0])
	}
	return out
}

// queryRowsAllCols renders every row with all its columns space-joined (the
// TCL execsql flattening).
func queryRowsAllCols(t *testing.T, e *exec.Engine, sel string) []string {
	t.Helper()
	parsed, err := parse.ParseSQL(sel)
	if err != nil {
		t.Fatalf("parse %q: %v", sel, err)
	}
	res := e.Exec(parsed[0])
	if res.Error != nil {
		t.Fatalf("exec %q: %v", sel, res.Error)
	}
	out := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		cells := make([]string, len(row))
		for j, c := range row {
			cells[j] = fmt.Sprint(c)
		}
		out[i] = strings.Join(cells, " ")
	}
	return out
}

// TestPinAuxDataConstantArgPersists is func.test 13.1/13.3: a constant
// argument's slot is fresh on the first row ('0') and reused afterwards
// ('1'); the same single-row call without FROM is '0'.
func TestPinAuxDataConstantArgPersists(t *testing.T) {
	e := newAuxEngine(t)
	if got := queryRows(t, e, "SELECT test_auxdata('hello world')"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("13.1 got %v, want [0]", got)
	}
	got := queryRows(t, e, "SELECT test_auxdata('hello world') FROM t4")
	want := []string{"0", "1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("13.3 got %v, want %v", got, want)
	}
}

// TestPinAuxDataTwoConstants is func.test 13.4: both arguments constant →
// both slots persist across rows.
func TestPinAuxDataTwoConstants(t *testing.T) {
	e := newAuxEngine(t)
	got := queryRows(t, e, "SELECT test_auxdata('hello world', 123) FROM t4")
	want := []string{"0 0", "1 1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("13.4 got %v, want %v", got, want)
	}
}

// TestPinAuxDataNonConstantArgFresh is func.test 13.5/13.6: a column (or any
// non-constant) argument gets a fresh slot every row; a folded constant
// expression ('hello'||'world') persists like a literal.
func TestPinAuxDataNonConstantArgFresh(t *testing.T) {
	e := newAuxEngine(t)
	for _, sel := range []string{
		"SELECT test_auxdata('hello world', a) FROM t4",
		"SELECT test_auxdata('hello'||'world', a) FROM t4",
	} {
		got := queryRows(t, e, sel)
		want := []string{"0 0", "1 0"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s got %v, want %v", sel, got, want)
		}
	}
}

// TestPinAuxDataDistinctCallSites: the same function called at two sites in
// one statement keeps DISTINCT slots (C keys by OP_Function instruction
// address): each site starts '0' and persists independently to '1'. The two
// columns flatten to "0 0" / "1 1" per row.
func TestPinAuxDataDistinctCallSites(t *testing.T) {
	e := newAuxEngine(t)
	got := queryRowsAllCols(t, e, "SELECT test_auxdata('x'), test_auxdata('x') FROM t4")
	want := []string{"0 0", "1 1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("two call sites got %v, want %v (a shared slot would render 0 1 on row 1)", got, want)
	}
}

// TestPinAuxDataParameterIsConstant is func.test 13.8.4: a bound $parameter
// is compile-time constant (TK_VARIABLE), so its slot persists across rows
// while the column argument stays fresh.
func TestPinAuxDataParameterIsConstant(t *testing.T) {
	e := newAuxEngine(t)
	vtab.TclVarSet("v", "", "one")
	defer vtab.TclVarReset()
	got := queryRows(t, e, "SELECT test_auxdata($v, a) FROM t4")
	want := []string{"0 0", "1 0"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("13.8.4 got %v, want %v", got, want)
	}
}

// TestPinAuxDataStatementResetDiscards is func.test 13.8.1-3: statement
// teardown destroys every auxdata slot, so a re-executed statement starts
// at '0' again.
func TestPinAuxDataStatementResetDiscards(t *testing.T) {
	e := newAuxEngine(t)
	want := []string{"0", "1"}
	for i := 0; i < 3; i++ {
		got := queryRows(t, e, "SELECT test_auxdata('constant') FROM t4")
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("run %d got %v, want %v", i, got, want)
		}
	}
}

// TestPinAuxDataGetAfterSet: within one invocation get-after-set sees the
// value (C's json.c parse cache relies on this; the post-call release only
// runs after the function returns).
func TestPinAuxDataGetAfterSet(t *testing.T) {
	e := newBareEngine(t)
	e.Functions().RegisterContext("aux_setget", func(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
		ctx.SetAuxData(0, "cached", nil)
		if ctx.GetAuxData(0) == nil {
			return "0", nil
		}
		return "1", nil
	}, 0, 0)
	if got := queryRows(t, e, "SELECT aux_setget()"); len(got) != 1 || got[0] != "1" {
		t.Fatalf("got %v, want [1]", got)
	}
}

// TestPinAuxDataDestructorLifecycle: destructors run on slot replacement
// (every row re-sets a constant-argument slot) and at statement teardown
// (C closeAllCursors); an erroring call also releases the site's
// non-constant slots.
func TestPinAuxDataDestructorLifecycle(t *testing.T) {
	e := newBareEngine(t)
	var destroyed int
	e.Functions().RegisterContext("aux_hold", func(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
		ctx.SetAuxData(0, "v", func(interface{}) { destroyed++ })
		return "v", nil
	}, 1, 1)
	execSQL(t, e, "CREATE TABLE t5(x)", "INSERT INTO t5 VALUES(1)", "INSERT INTO t5 VALUES(2)")
	// Row 1 creates the slot; row 2 replaces it (one destructor); the next
	// statement's teardown fires the remaining one.
	if got := queryRows(t, e, "SELECT aux_hold('k') FROM t5"); len(got) != 2 {
		t.Fatalf("rows: %v", got)
	}
	if destroyed != 1 {
		t.Fatalf("after 2 rows: destroyed=%d, want 1 (replacement destructor)", destroyed)
	}
	execSQL(t, e, "SELECT 1")
	if destroyed != 2 {
		t.Fatalf("after statement reset: destroyed=%d, want 2 (teardown destructor)", destroyed)
	}

	// Error path: setting a non-constant-argument slot then failing still
	// releases the slot (C's isError!=0 branch runs the same release).
	destroyed = 0
	e.Functions().RegisterContext("aux_err", func(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
		ctx.SetAuxData(0, "v", func(interface{}) { destroyed++ })
		return nil, fmt.Errorf("aux_err boom")
	}, 1, 1)
	parsed, err := parse.ParseSQL("SELECT aux_err(x) FROM t5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := e.Exec(parsed[0])
	if res.Error == nil || !strings.Contains(res.Error.Error(), "aux_err boom") {
		t.Fatalf("error = %v, want aux_err boom", res.Error)
	}
	if destroyed != 1 {
		t.Fatalf("erroring call destroyed=%d, want 1 (post-call release)", destroyed)
	}
}

// TestPinAuxDataBeyond32Args: C's P1 mask is 32-bit, so slots at arguments
// >= 32 never survive a call even when the argument is constant
// (vdbeaux.c:3702 iAuxArg>31).
func TestPinAuxDataBeyond32Args(t *testing.T) {
	e := newAuxEngine(t)
	e.Functions().RegisterContext("aux33", func(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
		// Read-then-set, like test_auxdata: reading after setting in the
		// same call would observe the just-stored value.
		d0 := boolDigit(ctx.GetAuxData(0) != nil)
		d32 := boolDigit(ctx.GetAuxData(32) != nil)
		for i := range args {
			ctx.SetAuxData(i, fmt.Sprintf("slot%d", i), nil)
		}
		return fmt.Sprintf("%d %d", d0, d32), nil
	}, 33, 33)
	lits := make([]string, 33)
	for i := range lits {
		lits[i] = fmt.Sprintf("'c%d'", i)
	}
	got := queryRows(t, e, "SELECT aux33("+strings.Join(lits, ", ")+") FROM t4")
	want := []string{"0 0", "1 0"} // arg 0 persists; arg 32 never does
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("33-arg call got %v, want %v", got, want)
	}
}

// boolDigit renders a C test_auxdata digit.
func boolDigit(b bool) int {
	if b {
		return 1
	}
	return 0
}
