package exec

import (
	"math"
	"reflect"
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// slotPathParityCase is one corpus entry: a template statement plus the
// value sequences of three same-shape statements (the first builds the live
// clone, the rest rewrite it in place).
type slotPathParityCase struct {
	name     string
	template string
	stmts    [3]string // same normalized shape, differing literals
	values   [3][]interface{}
}

// slotPathParityCorpus covers the statement families and expression shapes
// the slot-path form serves: point INSERT/UPDATE/DELETE/SELECT, CTE bodies,
// unary-minus literals, expression trees, mixed literal kinds in one
// statement, ORDER BY + single-slot LIMIT, and an upsert ON CONFLICT chain.
var slotPathParityCorpus = []slotPathParityCase{
	{
		name:     "insert-values",
		template: "INSERT INTO t VALUES(1, 'a', 2.5)",
		stmts: [3]string{
			"INSERT INTO t VALUES(1, 'a', 2.5)",
			"INSERT INTO t VALUES(2, 'b', 3.5)",
			"INSERT INTO t VALUES(3000, 'ccc', 0.25)",
		},
		values: [3][]interface{}{
			{int64(1), "a", 2.5},
			{int64(2), "b", 3.5},
			{int64(3000), "ccc", 0.25},
		},
	},
	{
		name:     "update-point",
		template: "UPDATE t SET c = c + 1 WHERE id = 10",
		stmts: [3]string{
			"UPDATE t SET c = c + 1 WHERE id = 10",
			"UPDATE t SET c = c + 1 WHERE id = 20",
			"UPDATE t SET c = c + 1 WHERE id = -30",
		},
		values: [3][]interface{}{
			{int64(1), int64(10)},
			{int64(1), int64(20)},
			{int64(1), int64(30)}, // the unary minus is structural; the literal is 30
		},
	},
	{
		name:     "delete-two-term",
		template: "DELETE FROM t WHERE k = 20 AND n = 200",
		stmts: [3]string{
			"DELETE FROM t WHERE k = 20 AND n = 200",
			"DELETE FROM t WHERE k = 21 AND n = 210",
			"DELETE FROM t WHERE k = 22 AND n = 220",
		},
		values: [3][]interface{}{
			{int64(20), int64(200)},
			{int64(21), int64(210)},
			{int64(22), int64(220)},
		},
	},
	{
		name:     "select-point",
		template: "SELECT v FROM t WHERE id = 5 AND v = 'x'",
		stmts: [3]string{
			"SELECT v FROM t WHERE id = 5 AND v = 'x'",
			"SELECT v FROM t WHERE id = 6 AND v = 'y'",
			"SELECT v FROM t WHERE id = 7 AND v = 8",
		},
		values: [3][]interface{}{
			{int64(5), "x"},
			{int64(6), "y"},
			{int64(7), int64(8)},
		},
	},
	{
		name:     "insert-cte-first",
		template: "WITH s(x) AS (SELECT 50 UNION ALL SELECT x + 1 FROM s WHERE x < 5) INSERT INTO t SELECT x, 'r', x * 2 FROM s",
		stmts: [3]string{
			"WITH s(x) AS (SELECT 50 UNION ALL SELECT x + 1 FROM s WHERE x < 5) INSERT INTO t SELECT x, 'r', x * 2 FROM s",
			"WITH s(x) AS (SELECT 60 UNION ALL SELECT x + 1 FROM s WHERE x < 6) INSERT INTO t SELECT x, 'r', x * 2 FROM s",
			"WITH s(x) AS (SELECT 70 UNION ALL SELECT x + 1 FROM s WHERE x < 7) INSERT INTO t SELECT x, 'r', x * 2 FROM s",
		},
		values: [3][]interface{}{
			{int64(50), int64(1), int64(5), "r", int64(2)},
			{int64(60), int64(1), int64(6), "r", int64(2)},
			{int64(70), int64(1), int64(7), "r", int64(2)},
		},
	},
	{
		name:     "select-order-limit",
		template: "SELECT x FROM t ORDER BY x LIMIT 3",
		stmts: [3]string{
			"SELECT x FROM t ORDER BY x LIMIT 3",
			"SELECT x FROM t ORDER BY x LIMIT 4",
			"SELECT x FROM t ORDER BY x LIMIT 5",
		},
		values: [3][]interface{}{
			{int64(3)},
			{int64(4)},
			{int64(5)},
		},
	},
	{
		name:     "update-expression-set",
		template: "UPDATE t SET n = n * 2 + 1, v = 'u' WHERE k = 4",
		stmts: [3]string{
			"UPDATE t SET n = n * 2 + 1, v = 'u' WHERE k = 4",
			"UPDATE t SET n = n * 2 + 1, v = 'w' WHERE k = 5",
			"UPDATE t SET n = n * 2 + 1, v = 'z' WHERE k = 6",
		},
		values: [3][]interface{}{
			{int64(2), int64(1), "u", int64(4)},
			{int64(2), int64(1), "w", int64(5)},
			{int64(2), int64(1), "z", int64(6)},
		},
	},
	{
		name:     "upsert-onconflict",
		template: "INSERT INTO t VALUES(1, 'a', 2) ON CONFLICT(k) DO UPDATE SET v = 'dup' WHERE t.n < 9",
		stmts: [3]string{
			"INSERT INTO t VALUES(1, 'a', 2) ON CONFLICT(k) DO UPDATE SET v = 'dup' WHERE t.n < 9",
			"INSERT INTO t VALUES(2, 'b', 3) ON CONFLICT(k) DO UPDATE SET v = 'dup2' WHERE t.n < 8",
			"INSERT INTO t VALUES(3, 'c', 4) ON CONFLICT(k) DO UPDATE SET v = 'dup3' WHERE t.n < 7",
		},
		values: [3][]interface{}{
			{int64(1), "a", int64(2), "dup", int64(9)},
			{int64(2), "b", int64(3), "dup2", int64(8)},
			{int64(3), "c", int64(4), "dup3", int64(7)},
		},
	},
}

// TestPinSlotPathParityWithCOW pins the slot-path substitution's
// byte-equivalence: for every corpus entry, rewriting the live clone's
// literal leaves produces an AST structurally identical to what the COW
// walker produces for the same values (which is what a fresh parse of the
// statement text produces — pinned separately by the template-clone
// behavioral tests).
func TestPinSlotPathParityWithCOW(t *testing.T) {
	for _, tc := range slotPathParityCorpus {
		t.Run(tc.name, func(t *testing.T) {
			templateAST, err := parse.ParseSQL(tc.template)
			if err != nil {
				t.Fatalf("parse template: %v", err)
			}
			slots := collectTemplateSlots(templateAST)
			if slots == nil {
				t.Fatalf("%s: template unexpectedly slot-path ineligible", tc.name)
			}
			// Values must line up with the normalizer's extraction (the same
			// count the engine would see at a hit).
			for i, stmt := range tc.stmts {
				_, values, _ := normalizeSQLScratch(stmt, nil, nil)
				if len(values) != len(tc.values[i]) {
					t.Fatalf("%s stmt %d: normalize extracted %v, corpus says %v", tc.name, i, values, tc.values[i])
				}
				for k := range values {
					if values[k] != tc.values[i][k] {
						t.Fatalf("%s stmt %d: value %d = %v, corpus says %v", tc.name, i, k, values[k], tc.values[i][k])
					}
				}
			}
			// Build the live clone with the first statement's values (the
			// engine's standalone COW form), then rewrite for stmts 1 and 2
			// and compare against the COW walk each time.
			c := exprClone{values: tc.values[0]}
			live := make([]sql.Stmt, len(templateAST))
			for i, st := range templateAST {
				out, ok := c.stmt(st)
				if !ok {
					t.Fatalf("%s: COW build refused", tc.name)
				}
				live[i] = out
			}
			for i := range tc.stmts {
				if !slots.validateValues(tc.values[i]) {
					t.Fatalf("%s stmt %d: slot gate refused values %v", tc.name, i, tc.values[i])
				}
				slots.apply(live[0], tc.values[i])
				cow := exprClone{values: tc.values[i]}
				ref := make([]sql.Stmt, len(templateAST))
				for k, st := range templateAST {
					out, ok := cow.stmt(st)
					if !ok {
						t.Fatalf("%s stmt %d: COW reference refused", tc.name, i)
					}
					ref[k] = out
				}
				if !reflect.DeepEqual(live, ref) {
					t.Fatalf("%s stmt %d: slot-path AST diverged from COW reference", tc.name, i)
				}
				// The template AST itself must stay untouched throughout:
				// still DeepEqual to a fresh parse of the same text.
				if fresh, err := parse.ParseSQL(tc.template); err != nil || !reflect.DeepEqual(templateAST, fresh) {
					t.Fatalf("%s: template AST mutated", tc.name)
				}
			}
		})
	}
}

// TestPinSlotPathRefusalTemplatesAreCOWOnly pins that every shape the COW
// walker refuses (folded unary-minus integer slots, hex slots, dual-slot
// LIMIT/OFFSET, blob and RAISE literals, INSERT tuples with nested literals)
// is slot-path ineligible — hits take the COW path and refuse identically.
func TestPinSlotPathRefusalTemplatesAreCOWOnly(t *testing.T) {
	cases := []string{
		"SELECT tointeger(-9223372036854775808)",                            // folded minInt64
		"SELECT tointeger(0x1F)",                                            // hex slot
		"SELECT x FROM t LIMIT 3 OFFSET 2",                                  // dual-slot limit/offset
		"SELECT length(x'4142')",                                            // blob literal
		"INSERT INTO t VALUES(1 + 1, 'a')",                                  // nested literal in a tuple expression
		"DELETE FROM t WHERE k = 1 LIMIT 1 OFFSET 1",                        // dual-slot limit/offset
		"WITH c(x) AS (SELECT 1 UNION SELECT 2) SELECT x FROM c LIMIT 2, 3", // comma limit
	}
	for _, template := range cases {
		ast, err := parse.ParseSQL(template)
		if err != nil {
			t.Fatalf("parse %q: %v", template, err)
		}
		if slots := collectTemplateSlots(ast); slots != nil {
			t.Errorf("%q: expected slot-path ineligibility, got %d slots", template, len(slots.paths))
		}
	}
}

// TestPinSlotPathMultiStatementTemplateIsCOWOnly pins that a multi-statement
// batch template never takes the live-clone form (two same-template
// statements of one batch substitute up front and must coexist).
func TestPinSlotPathMultiStatementTemplateIsCOWOnly(t *testing.T) {
	ast, err := parse.ParseSQL("UPDATE t SET c = 1 WHERE id = 2; UPDATE t SET c = 3 WHERE id = 4")
	if err != nil {
		t.Fatal(err)
	}
	if slots := collectTemplateSlots(ast); slots != nil {
		t.Fatalf("multi-statement template got %d slots, want nil", len(slots.paths))
	}
}

// TestPinSlotPathValueGates pins the slot classes' value gates against the
// COW gates: an int into a REAL slot refuses, a float into an int/string
// slot refuses, the 2^63 double and non-finite floats refuse REAL slots,
// and a quoted value serves any slot.
func TestPinSlotPathValueGates(t *testing.T) {
	mk := func(classes ...slotClass) *templateSlots {
		ts := &templateSlots{classes: classes}
		for range classes {
			ts.paths = append(ts.paths, slotPath{})
		}
		return ts
	}
	if mk(slotInt).validateValues([]interface{}{int64(5)}) != true {
		t.Error("int into int slot refused")
	}
	if mk(slotInt).validateValues([]interface{}{5.0}) {
		t.Error("float into int slot accepted")
	}
	if mk(slotReal).validateValues([]interface{}{int64(5)}) {
		t.Error("int into real slot accepted")
	}
	if !mk(slotReal).validateValues([]interface{}{5.0}) {
		t.Error("float into real slot refused")
	}
	if mk(slotReal).validateValues([]interface{}{twoPow63}) {
		t.Error("2^63 double into real slot accepted")
	}
	if mk(slotReal).validateValues([]interface{}{math.Inf(1)}) {
		t.Error("infinite real into real slot accepted")
	}
	if !mk(slotStr).validateValues([]interface{}{"x"}) {
		t.Error("string into string slot refused")
	}
	if mk(slotStr).validateValues([]interface{}{5.0}) {
		t.Error("float into string slot accepted")
	}
	if !mk(slotStr).validateValues([]interface{}{int64(7)}) {
		t.Error("int into string slot refused (quoted slot serves any)")
	}
	if !mk(slotInsAny).validateValues([]interface{}{int64(7)}) ||
		!mk(slotInsAny).validateValues([]interface{}{7.5}) ||
		!mk(slotInsAny).validateValues([]interface{}{"x"}) {
		t.Error("INSERT tuple slot refused a value insertValue accepts")
	}
	if mk(slotInt).validateValues([]interface{}{int64(5), int64(6)}) {
		t.Error("count mismatch accepted")
	}
	if mk(slotInt).validateValues([]interface{}{nil}) {
		t.Error("nil value accepted")
	}
}
