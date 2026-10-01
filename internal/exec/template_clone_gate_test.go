package exec

import (
	"math"
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// parseGateTemplate parses one statement as a template and normalizes a
// second one, returning the cached values normalizeSQL extracted for it.
func parseGateTemplate(t *testing.T, template, stmt string) []interface{} {
	t.Helper()
	if _, err := parse.ParseSQL(template); err != nil {
		t.Fatalf("parse template %q: %v", template, err)
	}
	_, values, _ := normalizeSQLScratch(stmt, nil, nil)
	if len(values) == 0 {
		t.Fatalf("normalize %q: no literal values", stmt)
	}
	return values
}

// cloneLiterals clones template with values and returns the substituted text
// of the first NumericLit/StringLit walked, for value/kind assertions.
func cloneLiterals(t *testing.T, template string, values []interface{}) ([]string, bool) {
	t.Helper()
	stmts, err := parse.ParseSQL(template)
	if err != nil {
		t.Fatalf("parse %q: %v", template, err)
	}
	cloned, ok := cloneStmtsWithValues(stmts, values)
	if !ok {
		return nil, false
	}
	var lits []string
	walk := func(e sql.Expr) {
		switch v := e.(type) {
		case *sql.NumericLit:
			lits = append(lits, v.Value)
		case *sql.StringLit:
			lits = append(lits, v.Value)
		}
	}
	for _, s := range cloned {
		if sel, ok := s.(*sql.SelectStmt); ok {
			for _, col := range sel.Columns {
				switch x := col.Expr.(type) {
				case *sql.FuncCall:
					for _, a := range x.Args {
						walk(a)
					}
				default:
					walk(col.Expr)
				}
			}
		}
	}
	return lits, true
}

// TestTemplateCloneGateSameKind pins that the numeric gate ALLOWS same-kind
// varying-value substitution: integer value into a decimal slot, REAL value
// into a REAL slot (decimal point kept), string into any slot. A regression
// to the spelling-equality gate would refuse every different value and
// silently forfeit the template path (the 24%-of-Prepare re-parse cost).
func TestTemplateCloneGateSameKind(t *testing.T) {
	cases := []struct {
		name     string
		template string
		stmt     string
		wantLits []string
	}{
		{"int-slot-varying-value", "SELECT tointeger(9223372036854775807)", "SELECT tointeger(9223372036854775806)", []string{"9223372036854775806"}},
		{"int-slot-plain-value", "SELECT tointeger(5)", "SELECT tointeger(2147483648)", []string{"2147483648"}},
		{"int-slot-leading-zeros", "SELECT tointeger(007)", "SELECT tointeger(9)", []string{"9"}},
		{"real-slot-varying-value", "SELECT toreal(1.5)", "SELECT toreal(2.5)", []string{"2.5"}},
		{"real-slot-integer-valued-real", "SELECT toreal(8.0)", "SELECT toreal(5.0)", []string{"5.0"}},
		{"real-slot-exponent", "SELECT toreal(1e2)", "SELECT toreal(3.5)", []string{"3.5"}},
		{"string-slot", "SELECT lower('ABC')", "SELECT lower('xyz')", []string{"xyz"}},
		{"quoted-slot-serves-int", "SELECT lower('ABC')", "SELECT lower(7)", []string{"7"}},
	}
	for _, tc := range cases {
		values := parseGateTemplate(t, tc.template, tc.stmt)
		lits, ok := cloneLiterals(t, tc.template, values)
		if !ok {
			t.Fatalf("%s: clone refused values %v (gate must allow same-kind substitution)", tc.name, values)
		}
		if len(lits) != len(tc.wantLits) {
			t.Fatalf("%s: literals %v, want %v", tc.name, lits, tc.wantLits)
		}
		for i := range lits {
			if lits[i] != tc.wantLits[i] {
				t.Fatalf("%s: literal %q, want %q", tc.name, lits[i], tc.wantLits[i])
			}
		}
	}
}

// TestTemplateCloneGateRefusals pins every shape the numeric gate must
// refuse: folded unary-minus slots (the parser folds -2^63 and -hex into the
// literal), hex slots (lossy extraction), the shape-ambiguous exact 2^63
// double, non-finite REALs, and REAL values into integer slots.
func TestTemplateCloneGateRefusals(t *testing.T) {
	cases := []struct {
		name     string
		template string
		stmt     string
	}{
		{"folded-minint64-slot", "SELECT tointeger(-9223372036854775808)", "SELECT tointeger(-9223372036854775807)"},
		{"folded-minint64-slot-float", "SELECT tointeger(-9223372036854775808)", "SELECT tointeger(-9223372036854775808)"},
		{"folded-hex-slot", "SELECT tointeger(-0x1F)", "SELECT tointeger(-0x1E)"},
		{"hex-slot", "SELECT tointeger(0x1F)", "SELECT tointeger(0x1F)"},
		{"hex-digit-E-slot", "SELECT tointeger(0xE8)", "SELECT tointeger(0xE8)"},
		{"twoPow63-into-real-slot", "SELECT toreal(9223372036854775808.0)", "SELECT toreal(9223372036854775808)"},
		{"infinite-real", "SELECT toreal(1.5)", "SELECT toreal(1e999)"},
		{"real-into-int-slot", "SELECT tointeger(5)", "SELECT tointeger(1.5)"},
		{"overflow-into-int-slot", "SELECT tointeger(5)", "SELECT tointeger(9223372036854775808)"},
		{"int-into-real-slot", "SELECT toreal(1.5)", "SELECT toreal(5)"},
	}
	for _, tc := range cases {
		values := parseGateTemplate(t, tc.template, tc.stmt)
		if _, ok := cloneLiterals(t, tc.template, values); ok {
			t.Fatalf("%s: clone accepted values %v (gate must refuse)", tc.name, values)
		}
	}
}

// TestTemplateCloneGateFloatSlotText pins floatSlotText's REAL-kind
// preservation and the twoPow63 constant.
func TestTemplateCloneGateFloatSlotText(t *testing.T) {
	if got := floatSlotText(5); got != "5.0" {
		t.Fatalf("floatSlotText(5) = %q, want 5.0", got)
	}
	if got := floatSlotText(9.5); got != "9.5" {
		t.Fatalf("floatSlotText(9.5) = %q, want 9.5", got)
	}
	if got := floatSlotText(1e100); got != "1e+100" {
		t.Fatalf("floatSlotText(1e100) = %q, want 1e+100", got)
	}
	if twoPow63 != math.Ldexp(1, 63) {
		t.Fatalf("twoPow63 = %v", twoPow63)
	}
	if !isDecimalSlot("9223372036854775807") || isDecimalSlot("-1") || isDecimalSlot("0x1F") || isDecimalSlot("1.5") || isDecimalSlot("") {
		t.Fatal("isDecimalSlot misclassifies")
	}
	if !isRealSlot("1.5") || !isRealSlot("1e2") || isRealSlot("0xE8") || isRealSlot("-9223372036854775808") || isRealSlot("5") {
		t.Fatal("isRealSlot misclassifies")
	}
}
