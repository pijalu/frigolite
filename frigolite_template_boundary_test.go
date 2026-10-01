package frigolite

import (
	"strconv"
	"testing"

	"github.com/pijalu/frigolite/internal/util"
)

// TestTemplateBoundaryLiteralParity drives the int64 boundary literal family
// (minInt64/maxInt64 and their +-1 spellings, with and without a unary
// minus) through the template cache in an order that seeds templates and
// then HITS them with different values, and requires every result to match
// the sqlite3 oracle executing the same statement fresh.
//
// The statements are bare literals/arithmetic (not tointeger/toreal) because
// the macOS system sqlite3 lacks those functions; the statement shapes — and
// therefore the template keys and AST shapes — are identical.
//
// The trap this pins: one normalized key (e.g. "SELECT -?")
// serves statements whose parsed ASTs differ in shape — the parser FOLDS the
// unary minus of a 2^63-magnitude literal into the literal itself
// ("-9223372036854775808" is a single NumericLit, not UnaryOp{'-'}), while
// every smaller magnitude parses as UnaryOp{'-'} plus a positive literal.
// Substitution into a stored template must reproduce exactly what a fresh
// parse of the current statement yields; anything else corrupts sign or
// kind (the first same-kind gate attempt flipped -2147483648 to +2147483648
// and turned tointeger(-9223372036854775808) into NULL here).
func TestTemplateBoundaryLiteralParity(t *testing.T) {
	foldedSequence := []string{
		// Shape "SELECT ?" — plain positive boundary family. The first
		// statement seeds digit-only slots; the rest substitute.
		"SELECT 9223372036854775807",
		"SELECT 9223372036854775806",
		"SELECT 2147483649",
		// Shape "SELECT -?" — the folded seed. Later statements share the
		// key but parse as UnaryOp: substitution must refuse.
		"SELECT -9223372036854775808",
		"SELECT -9223372036854775807",
		"SELECT -2147483649",
		"SELECT -2147483648",
		// The REAL spelling of the same shape: same key, same float64 2^63
		// value as the folded statement — must refuse and parse to a REAL.
		"SELECT -9223372036854775808.0",
		// Two-literal shapes: substitution must hit for digit-only slots and
		// refuse the folded seed.
		"SELECT 9223372036854775807 - 1",
		"SELECT 9223372036854775807 - 2",
		"SELECT -9223372036854775808 - 1",
		"SELECT -2147483648 - 1",
		// Hex spellings: lossy extraction (value 0, residual "x…" in the
		// key) must never poison a template; folded hex refuses too.
		"SELECT 0x1F",
		"SELECT 0x1E",
		"SELECT -0x1F",
		// Kind visibility for the fold and the substituted boundary.
		"SELECT typeof(-9223372036854775808)",
		"SELECT typeof(9223372036854775807)",
		"SELECT quote(-2147483648)",
	}
	// Unary-op shapes whose FIRST statement is NOT folded: here substitution
	// must actually take the template path (unary minus preserved), so a
	// separate engine keeps the folded statements above from seeding these
	// keys.
	unarySequence := []string{
		// UnaryOp{'-'} over integer literals: seed small, hit at the
		// boundaries.
		"SELECT -1",
		"SELECT -2147483648",
		"SELECT -9223372036854775807",
		"SELECT -9223372036854775808", // float64 2^63 value: refuse, fresh parse folds
		// UnaryOp{'-'} over REAL literals: seed small, hit boundary; the
		// exact 2^63 double refuses (ambiguous spelling), fresh parse.
		"SELECT -1.5",
		"SELECT -2.5",
		"SELECT -9223372036854775808.0",
		// Kind of the substituted unary-integer boundary (hit path).
		"SELECT typeof(-2147483648)",
		"SELECT quote(-9223372036854775807)",
		// Unary plus never folds: substitution must serve varying values.
		"SELECT +9223372036854775807",
		"SELECT +9223372036854775806",
	}

	dbFolded, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dbFolded.Close()
	dbUnary, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dbUnary.Close()
	unarySet := make(map[string]bool, len(unarySequence))
	for _, s := range unarySequence {
		unarySet[s] = true
	}
	all := append(append([]string{}, foldedSequence...), unarySequence...)
	for _, s := range all {
		target := dbFolded
		if unarySet[s] {
			target = dbUnary
		}
		if r := target.Query(s); r.Error != nil {
			t.Fatalf("%s: %v", s, r.Error)
		}
	}

	// Every statement must equal the oracle's fresh execution.
	for _, s := range all {
		want := oracleRows(t, s)
		source := dbFolded
		if unarySet[s] {
			source = dbUnary
		}
		got := queryRows(t, source, s)
		if len(got) != len(want) {
			t.Fatalf("%s: rows %v, oracle %v", s, got, want)
		}
		for i := range want {
			if len(got[i]) != len(want[i]) {
				t.Fatalf("%s: row %v, oracle %v", s, got[i], want[i])
			}
			for j := range want[i] {
				if !sameCell(got[i][j], want[i][j]) {
					t.Fatalf("%s: cell %q, oracle %q", s, got[i][j], want[i][j])
				}
			}
		}
	}
}

// sameCell compares one result cell against the oracle's text. REAL values
// are compared by re-rendering the parsed oracle value through frigolite's
// SQLite-format real-to-text conversion: the system sqlite3 (3.54) renders
// doubles with shortest-round-trip digits, while frigolite mirrors the
// classic %.15g rendering — same value, different digit count.
func sameCell(got, want string) bool {
	if got == want {
		return true
	}
	w, err := strconv.ParseFloat(want, 64)
	if err != nil {
		return false
	}
	g, gerr := strconv.ParseFloat(got, 64)
	if gerr != nil {
		return false
	}
	return util.FormatSQLiteReal(g) == util.FormatSQLiteReal(w)
}

// TestTemplateBoundaryLiteralExpectation pins the exact boundary values
// without the oracle (so the pin holds even in CLI-less environments): the
// substituted template executes the CURRENT statement's literal with the
// CURRENT statement's kind, and the folded minInt64 keeps its sign.
func TestTemplateBoundaryLiteralExpectation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT tointeger(9223372036854775807)", "9223372036854775807"},
		{"SELECT tointeger(9223372036854775806)", "9223372036854775806"},
		{"SELECT tointeger(-9223372036854775808)", "-9223372036854775808"},
		{"SELECT tointeger(-9223372036854775807)", "-9223372036854775807"},
		{"SELECT tointeger(-2147483649)", "-2147483649"},
		{"SELECT tointeger(-2147483648)", "-2147483648"},
		{"SELECT tointeger(-2147483648 - 1)", "-2147483649"},
		{"SELECT tointeger(-1)", "-1"},
		{"SELECT tointeger(-2147483648)", "-2147483648"}, // template hit on the -1 seed
		{"SELECT toreal(-1.5)", "-1.5"},
		{"SELECT toreal(-2.5)", "-2.5"},
		{"SELECT toreal(-9223372036854775808)", "{}"}, // fold -> lossy int64->real -> NULL
		{"SELECT tointeger(0x1F)", "31"},
		{"SELECT tointeger(-0x1F)", "-31"},
		{"SELECT typeof(tointeger(-9223372036854775808))", "integer"},
		{"SELECT typeof(toreal(-2.5))", "real"},
	}
	// Seed the shared shapes in an order that maximizes template hits.
	for _, c := range cases {
		if r := db.Query(c.sql); r.Error != nil {
			t.Fatalf("%s: %v", c.sql, r.Error)
		}
	}
	// Second pass: every statement now either hits a template or full-parses;
	// results must be identical to the first pass.
	for _, c := range cases {
		got := queryRows(t, db, c.sql)
		if len(got) != 1 || len(got[0]) != 1 {
			t.Fatalf("%s: rows %v", c.sql, got)
		}
		if got[0][0] != c.want {
			t.Fatalf("%s = %q, want %q", c.sql, got[0][0], c.want)
		}
	}
}
