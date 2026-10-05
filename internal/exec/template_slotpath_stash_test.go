package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// TestPinSlotPathInsertStash pins the INSERT tuple value stash: the slot
// table records which slots are VALUES-tuple items, apply writes the parsed
// values into the clone's InsLitVals in tuple/item position (nil for
// non-literal items), and the text rewrite and the stash stay in sync across
// successive substitutions of the same live clone.
func TestPinSlotPathInsertStash(t *testing.T) {
	stmts, perr := parseForStashTest("INSERT INTO t VALUES(1, 'x', NULL, 2.5, 7)")
	if perr != nil {
		t.Fatal(perr)
	}
	slots := collectTemplateSlots(stmts)
	if slots == nil {
		t.Fatal("literal-only INSERT must collect a slot table")
	}
	tupCount := 0
	for _, enc := range slots.tupSlot {
		if enc >= 0 {
			tupCount++
		}
	}
	if tupCount != 4 {
		t.Fatalf("tupSlot tuple entries = %d, want 4 (NULL is not a slot)", tupCount)
	}
	// apply mutates the AST in place; the test treats stmts as the live clone.
	vals := []interface{}{int64(42), "y", 8.25, int64(43)}
	if !slots.validateValues(vals) {
		t.Fatalf("validateValues(%v) = false", vals)
	}
	if !slots.apply(stmts[0], vals) {
		t.Fatal("apply failed")
	}
	ins := stmts[0].(*sql.InsertStmt)
	if ins.InsLitVals == nil {
		t.Fatal("apply did not build InsLitVals")
	}
	want := []interface{}{int64(42), "y", nil, 8.25, int64(43)}
	got := ins.InsLitVals[0]
	if len(got) != len(want) {
		t.Fatalf("InsLitVals = %v, want shape %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("InsLitVals[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// The rewritten literal text must match the stashed value's canonical
	// rendering (the eval-parity contract).
	if lit := ins.Values[0][0].(*sql.NumericLit); lit.Value != "42" {
		t.Fatalf("literal text = %q, want 42", lit.Value)
	}
	// A second substitution of the same live clone refreshes every slot
	// entry — a stale entry from the previous statement must not survive.
	vals2 := []interface{}{int64(5), "z", 1.5, int64(7)}
	if !slots.validateValues(vals2) || !slots.apply(stmts[0], vals2) {
		t.Fatal("second apply failed")
	}
	want2 := []interface{}{int64(5), "z", nil, 1.5, int64(7)}
	for i := range want2 {
		if ins.InsLitVals[0][i] != want2[i] {
			t.Fatalf("refresh: InsLitVals[%d] = %v, want %v", i, ins.InsLitVals[0][i], want2[i])
		}
	}
}

// TestPinSlotPathMixedSlotsNoStash pins that a template whose slots extend
// beyond the VALUES tuples (here an upsert DO UPDATE assignment) still
// stashes the TUPLE slots, and the tuple stash coexists with non-tuple
// slots served through the node rewrite only.
func TestPinSlotPathMixedSlotsNoStash(t *testing.T) {
	stmts, perr := parseForStashTest("INSERT INTO t VALUES(1, 'x', NULL, 2.5, 7) ON CONFLICT(id) DO UPDATE SET c = 9")
	if perr != nil {
		t.Fatal(perr)
	}
	slots := collectTemplateSlots(stmts)
	if slots == nil {
		t.Fatal("upsert INSERT with literals must collect a slot table")
	}
	tupCount, nonTup := 0, 0
	for _, enc := range slots.tupSlot {
		if enc >= 0 {
			tupCount++
		} else {
			nonTup++
		}
	}
	if tupCount != 4 || nonTup != 1 {
		t.Fatalf("tuple/non-tuple slots = %d/%d, want 4/1", tupCount, nonTup)
	}
	vals := []interface{}{int64(42), "y", 8.25, int64(43), int64(77)}
	if !slots.validateValues(vals) {
		t.Fatal("validateValues refused the upsert corpus")
	}
	if !slots.apply(stmts[0], vals) {
		t.Fatal("apply failed")
	}
	ins := stmts[0].(*sql.InsertStmt)
	got := ins.InsLitVals[0]
	if got[0] != int64(42) || got[1] != "y" || got[3] != 8.25 {
		t.Fatalf("tuple stash = %v, want [42 y nil 8.25 43]", got)
	}
	// The non-tuple slot (the DO UPDATE literal) must carry the rewritten
	// text but no stash entry of its own.
	if asg := ins.OnConflict.Assignments[0].Value.(*sql.NumericLit); asg.Value != "77" {
		t.Fatalf("DO UPDATE literal text = %q, want 77", asg.Value)
	}
}

// TestPinSlotPathStashValuesFirstBuild pins the first-build form: stashValues
// fills the stash of a freshly COW-built clone (no node rewrite), and a
// slot table without tuple slots reports nothing to stash.
func TestPinSlotPathStashValuesFirstBuild(t *testing.T) {
	stmts, perr := parseForStashTest("INSERT INTO t VALUES(1, 'x', NULL, 2.5, 7)")
	if perr != nil {
		t.Fatal(perr)
	}
	slots := collectTemplateSlots(stmts)
	c := exprClone{values: []interface{}{int64(42), "y", 8.25, int64(43)}}
	out, ok := c.stmt(stmts[0])
	if !ok {
		t.Fatal("COW clone refused")
	}
	if !slots.stashValues(out, c.values) {
		t.Fatal("stashValues failed on the COW clone")
	}
	ins := out.(*sql.InsertStmt)
	if ins.InsLitVals[0][0] != int64(42) || ins.InsLitVals[0][1] != "y" || ins.InsLitVals[0][3] != 8.25 {
		t.Fatalf("first-build stash = %v", ins.InsLitVals[0])
	}
	// A SELECT template has no tuple slots: nothing to stash, no failure.
	selStmts, perr2 := parseForStashTest("SELECT 1, 'a' FROM t WHERE id = 3")
	if perr2 != nil {
		t.Fatal(perr2)
	}
	selSlots := collectTemplateSlots(selStmts)
	if selSlots == nil {
		t.Fatal("SELECT literals must collect a slot table")
	}
	if !selSlots.stashValues(selStmts[0], []interface{}{int64(1), "a", int64(3)}) {
		t.Fatal("stashValues with no tuple slots must be a no-op success")
	}
}

// parseForStashTest parses one statement for the stash pins.
func parseForStashTest(s string) ([]sql.Stmt, error) {
	stmts, perr := parse.ParseSQL(s)
	if perr != nil {
		return nil, perr
	}
	return stmts, nil
}
