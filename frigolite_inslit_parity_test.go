package frigolite_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	frigolite "github.com/pijalu/frigolite"
)

// TestInsLitParityCorpus is the kind-parity pin for the INSERT literal fast
// path: every literal class (int, float, text, NULL, blob, hex, negative,
// 2^63-boundary, expressions, multi-row, column lists, upsert assignments,
// RETURNING, trigger bodies, STRICT tables, affinity conversions) executes
// on a FAST engine — one engine, repeated rounds with UNIQUE literals per
// statement, so rounds 1+ serve through the template slot-path live clone
// with the parsed-value stash — and on a CONTROL engine where every
// statement full-parses (round-odd statements spell the table name in upper
// case: SQLite resolves both spellings to the same table, but the template
// key normalizes literals only, so the control never forms a recurring
// template). Every stored row is compared cell for cell: rowid plus
// typeof()/quote() of every column — the exact stored representation.
func TestInsLitParityCorpus(t *testing.T) {
	shapes := []struct {
		name  string
		setup []string
		dump  []string
		stmt  func(r int) string
	}{
		{
			name:  "values_literal_only",
			setup: []string{"CREATE TABLE p1(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p1")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'v%d', NULL, %d, %d)",
					flip(r, "p1"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			name:  "column_list",
			setup: []string{"CREATE TABLE p2(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p2")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s(b, t, id) VALUES(%s, 'c%d', %d)",
					flip(r, "p2"), fmtFloat(2.5+float64(r)), r, r+1)
			},
		},
		{
			name:  "multi_row",
			setup: []string{"CREATE TABLE p3(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p3")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'm%da', NULL, %d, %d), (%d, %s, 'm%db', NULL, %d, %d)",
					flip(r, "p3"), 10+r, fmtFloat(0.5+float64(r)), r, 500+r, 2*r+1,
					11+r, fmtFloat(0.75+float64(r)), r, 501+r, 2*r+2)
			},
		},
		{
			// A literal nested inside an expression diverges the slot table:
			// the template is ineligible and EVERY execution full-parses —
			// on the fast engine too (this shape exercises the refusal, not
			// the stash).
			name:  "expr_item_refusal_shape",
			setup: []string{"CREATE TABLE p4(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p4")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d+1, %s, 'e%d', NULL, %d, %d)",
					flip(r, "p4"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			// Blob literals are not reconstructible from the normalized
			// value: ineligible template, full parse every time.
			name:  "blob_literal_refusal_shape",
			setup: []string{"CREATE TABLE p5(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p5")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'b%d', x'0%d0B', %d, %d)",
					flip(r, "p5"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, (r%9)+1, 1000+7*r, r+1)
			},
		},
		{
			// Hex integer literals keep the generic path (the scan does not
			// extract them; the AST literal refuses the slot gates).
			name:  "hex_literal_refusal_shape",
			setup: []string{"CREATE TABLE p6(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p6")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(0x%X, %s, 'h%d', NULL, %d, %d)",
					flip(r, "p6"), 48+r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			// Negative literals: the '-' stays outside the scanned span, the
			// tuple item is a folded unary minus, the COW walk refuses and
			// the statement full-parses — on both engines identically.
			name:  "negative_literals_refusal_shape",
			setup: []string{"CREATE TABLE p7(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p7")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(-%d, -%s, 'n%d', NULL, -%d, %d)",
					flip(r, "p7"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			// MaxInt64 fits a slot; 2^63 overflows int64 (the scan produces
			// the saturated double, whose spelling is shape-ambiguous) and
			// must fall back to a full parse with the REAL stored value.
			name:  "int64_boundary",
			setup: []string{"CREATE TABLE p8(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("p8")},
			stmt: func(r int) string {
				if r%2 == 0 {
					return fmt.Sprintf("INSERT INTO %s VALUES(9223372036854775807, %s, 'x%d', NULL, 9223372036854775808, %d)",
						flip(r, "p8"), fmtFloat(1.5+0.25*float64(r)), r, r+1)
				}
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'x%d', NULL, 9223372036854775806, %d)",
					flip(r, "p8"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, r+1)
			},
		},
		{
			// Upsert: tuple slots AND a DO UPDATE assignment slot; the DO
			// UPDATE literal must reach the second write on the fast path.
			name: "upsert_literal_assignment",
			setup: []string{
				"CREATE TABLE p9(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)",
				"INSERT INTO p9 VALUES(1, 9.0, 'seed', NULL, 9, 1)",
			},
			dump: []string{dump6("p9")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'u%d', NULL, %d, 1) ON CONFLICT(id) DO UPDATE SET b = %s, t = 'U%d'",
					flip(r, "p9"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, fmtFloat(3.5+0.5*float64(r)), r)
			},
		},
		{
			// RETURNING with no literal in the returned expression is
			// slot-path eligible; the fresh-slice evalTuple reads the stash.
			name:  "returning",
			setup: []string{"CREATE TABLE pa(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("pa")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'r%d', NULL, %d, %d) RETURNING id, a",
					flip(r, "pa"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			// A trigger body INSERT nests at a deeper exec depth and takes
			// the non-pooled evalTuple (fresh slice) — the stash read must
			// hold there too.
			name: "trigger_nested",
			setup: []string{
				"CREATE TABLE pb(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)",
				"CREATE TABLE pblog(src TEXT, v INTEGER)",
				"CREATE TRIGGER pbtg AFTER INSERT ON pb BEGIN INSERT INTO pblog VALUES(new.t, new.a); END",
			},
			dump: []string{dump6("pb"), "SELECT rowid, quote(src), quote(v) FROM pblog ORDER BY rowid"},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 'g%d', NULL, %d, %d)",
					flip(r, "pb"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r, 1000+7*r, r+1)
			},
		},
		{
			// STRICT table: the pre/post-affinity checks run around the
			// affinity wrap the stash feeds.
			name:  "strict_table",
			setup: []string{"CREATE TABLE pc(a INTEGER, b REAL, t TEXT) STRICT"},
			dump:  []string{"SELECT rowid, typeof(a), quote(a), typeof(b), quote(b), typeof(t), quote(t) FROM pc ORDER BY rowid"},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES(%d, %s, 's%d')",
					flip(r, "pc"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), r)
			},
		},
		{
			// Affinity conversions on the fast path: a numeric-looking TEXT
			// literal into INTEGER and REAL columns (converted on store), a
			// non-numeric TEXT literal kept, a no-affinity (bare n) and a
			// BLOB column stored as-is.
			name:  "affinity_conversions",
			setup: []string{"CREATE TABLE pd(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("pd")},
			stmt: func(r int) string {
				return fmt.Sprintf("INSERT INTO %s VALUES('%d', '%s', '900', 'x', %d, %d)",
					flip(r, "pd"), 100+7*r, fmtFloat(1.5+0.25*float64(r)), 1000+7*r, r+1)
			},
		},
	}

	const rounds = 4

	// FAST engine: one engine, all rounds.
	fast, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	for _, sh := range shapes {
		for _, s := range sh.setup {
			if res := fast.Exec(s); res.Error != nil {
				t.Fatalf("%s: fast setup: %v", sh.name, res.Error)
			}
		}
	}
	for r := 0; r < rounds; r++ {
		for _, sh := range shapes {
			if res := fast.Exec(sh.stmt(r)); res.Error != nil {
				t.Fatalf("%s round %d: fast: %v", sh.name, r, res.Error)
			}
		}
	}

	// CONTROL: one engine per shape — the round-odd upper-case spellings
	// keep every execution a cold full parse while accumulating exactly
	// like the fast engine did.
	for _, sh := range shapes {
		ctrl, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sh.setup {
			if res := ctrl.Exec(s); res.Error != nil {
				t.Fatalf("%s: control setup: %v", sh.name, res.Error)
			}
		}
		for r := 0; r < rounds; r++ {
			if res := ctrl.Exec(sh.stmt(r)); res.Error != nil {
				t.Fatalf("%s round %d: control: %v", sh.name, r, res.Error)
			}
		}
		for qi, dq := range sh.dump {
			got := queryDump(t, fast, dq)
			want := queryDump(t, ctrl, dq)
			if len(got) != len(want) {
				t.Fatalf("%s dump %d: fast cells = %d, control cells = %d\nfast=%v\nctrl=%v", sh.name, qi, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s dump %d cell %d: fast = %q, control = %q", sh.name, qi, i, got[i], want[i])
				}
			}
		}
		ctrl.Close()
	}
}

// dump6 reads a 6-column table rowwise: rowid plus typeof/quote of each
// column (quote gives the exact stored bytes).
func dump6(table string) string {
	return fmt.Sprintf(`SELECT rowid,
		typeof(a), quote(a), typeof(b), quote(b), typeof(t), quote(t),
		typeof(n), quote(n), typeof(k), quote(k)
		FROM %s ORDER BY rowid`, table)
}

// TestBindLitParityCorpus is the kind-parity pin for the prepared-statement
// bind path (the bind-mode twin of TestInsLitParityCorpus): one FAST engine
// runs every shape through db.Prepare + Stmt.Exec with Go-typed arguments
// (int64/int/negative/float integral+fractional/string/NULL/bool/blob/named
// ?NNN params, multi-row, upsert assignments, RETURNING, trigger bodies,
// STRICT tables, affinity conversions, uint64-beyond-int64 and NaN refusal
// shapes), each statement executed twice per round so the recycled tenant's
// literal nodes are rewritten in place with the stash refreshed. A CONTROL
// engine accumulates the identical rows through literal db.Exec text. Every
// stored row is compared cell for cell (rowid plus typeof()/quote()).
func TestBindLitParityCorpus(t *testing.T) {
	type bindShape struct {
		name  string
		setup []string
		dump  []string
		sql   string // the prepared form (parameter markers)
		ctrl  string // the control form: the same statement with %s per marker
		args  func(r, half int) []interface{}
	}
	shapes := []bindShape{
		{
			name:  "values_params",
			setup: []string{"CREATE TABLE q1(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("q1")},
			sql:   "INSERT INTO q1 VALUES(?, ?, ?, ?, ?, ?)",
			ctrl:  "INSERT INTO q1 VALUES(%s, %s, %s, %s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{int64(h + 100 + 7*r), 1.5 + 0.25*float64(r), "v" + fmt.Sprint(half) + fmt.Sprint(r), nil, []byte{byte(r % 9), 0x0B}, int64(h + r + 1)}
			},
		},
		{
			name:  "go_int_kinds",
			setup: []string{"CREATE TABLE q2(a INTEGER, b INTEGER, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("q2")},
			sql:   "INSERT INTO q2 VALUES(?1, ?2, ?3, ?4, ?5, ?6)",
			ctrl:  "INSERT INTO q2 VALUES(%s, %s, %s, %s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{-(h + 300 + r), 7 + r, "i" + fmt.Sprint(half) + fmt.Sprint(r), nil, int32(1000 + r), uint64(h + r + 1)}
			},
		},
		{
			name:  "bool_and_named",
			setup: []string{"CREATE TABLE q3(f INTEGER, t TEXT, id INTEGER PRIMARY KEY)"},
			dump:  []string{"SELECT rowid, typeof(f), quote(f), quote(t) FROM q3 ORDER BY rowid"},
			sql:   "INSERT INTO q3(f, t, id) VALUES(:flag, @txt, $id)",
			ctrl:  "INSERT INTO q3(f, t, id) VALUES(%s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{(r+half)%2 == 0, "b" + fmt.Sprint(half) + fmt.Sprint(r), int64(h + r + 1)}
			},
		},
		{
			name:  "multi_row_params",
			setup: []string{"CREATE TABLE q4(a INTEGER, t TEXT, id INTEGER PRIMARY KEY)"},
			dump:  []string{"SELECT rowid, typeof(a), quote(a), quote(t) FROM q4 ORDER BY rowid"},
			sql:   "INSERT INTO q4 VALUES(?, ?, ?), (?, ?, ?)",
			ctrl:  "INSERT INTO q4 VALUES(%s, %s, %s), (%s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{h + 10 + r, "ma" + fmt.Sprint(half) + fmt.Sprint(r), int64(h + 2*r + 1), h + 11 + r, "mb" + fmt.Sprint(half) + fmt.Sprint(r), int64(h + 2*r + 2)}
			},
		},
		{
			// The DO UPDATE assignment is not a tuple slot: its parameters
			// must evaluate through the rewritten nodes (no stash), with the
			// tuple slots stashed in the same statement.
			name: "upsert_param_assignment",
			setup: []string{
				"CREATE TABLE q5(a INTEGER, t TEXT, id INTEGER PRIMARY KEY)",
				"INSERT INTO q5 VALUES(1, 'seed', 1)",
			},
			dump: []string{"SELECT rowid, quote(a), quote(t) FROM q5 ORDER BY rowid"},
			sql:  "INSERT INTO q5 VALUES(?, ?, 1) ON CONFLICT(id) DO UPDATE SET t = ?",
			ctrl: "INSERT INTO q5 VALUES(%s, %s, 1) ON CONFLICT(id) DO UPDATE SET t = %s",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{h + 100 + r, "u" + fmt.Sprint(half) + fmt.Sprint(r), "U" + fmt.Sprint(half) + fmt.Sprint(r)}
			},
		},
		{
			name:  "returning_params",
			setup: []string{"CREATE TABLE q6(a INTEGER, id INTEGER PRIMARY KEY)"},
			dump:  []string{"SELECT rowid, quote(a) FROM q6 ORDER BY rowid"},
			sql:   "INSERT INTO q6 VALUES(?, ?) RETURNING id, a",
			ctrl:  "INSERT INTO q6 VALUES(%s, %s) RETURNING id, a",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{h + 200 + r, int64(h + r + 1)}
			},
		},
		{
			name: "trigger_nested_params",
			setup: []string{
				"CREATE TABLE q7(a INTEGER, t TEXT, id INTEGER PRIMARY KEY)",
				"CREATE TABLE q7log(src TEXT, v INTEGER)",
				"CREATE TRIGGER q7tg AFTER INSERT ON q7 BEGIN INSERT INTO q7log VALUES(new.t, new.a); END",
			},
			dump: []string{
				"SELECT rowid, quote(a), quote(t) FROM q7 ORDER BY rowid",
				"SELECT rowid, quote(src), quote(v) FROM q7log ORDER BY rowid",
			},
			sql:  "INSERT INTO q7 VALUES(?, ?, ?)",
			ctrl: "INSERT INTO q7 VALUES(%s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{h + 300 + r, "g" + fmt.Sprint(half) + fmt.Sprint(r), int64(h + r + 1)}
			},
		},
		{
			name:  "strict_table_params",
			setup: []string{"CREATE TABLE q8(a INTEGER, b REAL, t TEXT) STRICT"},
			dump:  []string{"SELECT rowid, typeof(a), quote(a), typeof(b), quote(b), typeof(t), quote(t) FROM q8 ORDER BY rowid"},
			sql:   "INSERT INTO q8 VALUES(?, ?, ?)",
			ctrl:  "INSERT INTO q8 VALUES(%s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				return []interface{}{h + 'x' + r, 0.5 + float64(r), "s" + fmt.Sprint(half) + fmt.Sprint(r)}
			},
		},
		{
			name:  "affinity_conversions_params",
			setup: []string{"CREATE TABLE q9(a INTEGER, b REAL, t TEXT, n, k BLOB, id INTEGER PRIMARY KEY)"},
			dump:  []string{dump6("q9")},
			sql:   "INSERT INTO q9 VALUES(?, ?, ?, ?, ?, ?)",
			ctrl:  "INSERT INTO q9 VALUES(%s, %s, %s, %s, %s, %s)",
			args: func(r, half int) []interface{} {
				// numeric-looking TEXT into INTEGER/REAL columns (converted on
				// store), non-numeric TEXT kept, bare/BLOB columns as-is.
				h := half * 100000
				return []interface{}{fmt.Sprint(h + 400 + r), fmt.Sprintf("%v", 1.5+0.25*float64(r)), "900", "x", int64(1000 + r), int64(h + r + 1)}
			},
		},
		{
			// uint64 beyond int64 and NaN must keep the generic bind form
			// (no stash entry): uint64 stores as REAL, NaN as NULL.
			name:  "refusal_kinds",
			setup: []string{"CREATE TABLE qa(a, b, id INTEGER PRIMARY KEY)"},
			dump:  []string{"SELECT rowid, typeof(a), quote(a), typeof(b), quote(b) FROM qa ORDER BY rowid"},
			sql:   "INSERT INTO qa VALUES(?, ?, ?)",
			ctrl:  "INSERT INTO qa VALUES(%s, %s, %s)",
			args: func(r, half int) []interface{} {
				h := half * 100000
				if (r+half)%2 == 0 {
					return []interface{}{uint64(18446744073709551615), math.NaN(), int64(h + r + 1)}
				}
				return []interface{}{uint64(9223372036854775806), 2.5, int64(h + r + 1)}
			},
		},
	}

	const rounds = 4

	fast, err := frigolite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	for _, sh := range shapes {
		for _, s := range sh.setup {
			if res := fast.Exec(s); res.Error != nil {
				t.Fatalf("%s: fast setup: %v", sh.name, res.Error)
			}
		}
	}
	for r := 0; r < rounds; r++ {
		for _, sh := range shapes {
			st, err := fast.Prepare(sh.sql)
			if err != nil {
				t.Fatalf("%s round %d: prepare: %v", sh.name, r, err)
			}
			for half := 0; half < 2; half++ {
				if res := st.Exec(sh.args(r, half)...); res.Error != nil {
					t.Fatalf("%s round %d half %d: fast: %v", sh.name, r, half, res.Error)
				}
			}
			st.Close()
		}
	}

	for _, sh := range shapes {
		ctrl, err := frigolite.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sh.setup {
			if res := ctrl.Exec(s); res.Error != nil {
				t.Fatalf("%s: control setup: %v", sh.name, res.Error)
			}
		}
		for r := 0; r < rounds; r++ {
			for half := 0; half < 2; half++ {
				args := sh.args(r, half)
				lits := make([]interface{}, len(args))
				for i, a := range args {
					lits[i] = sqlLiteralText(a)
				}
				if res := ctrl.Exec(fmt.Sprintf(sh.ctrl, lits...)); res.Error != nil {
					t.Fatalf("%s round %d half %d: control: %v", sh.name, r, half, res.Error)
				}
			}
		}
		for qi, dq := range sh.dump {
			got := queryDump(t, fast, dq)
			want := queryDump(t, ctrl, dq)
			if len(got) != len(want) {
				t.Fatalf("%s dump %d: fast cells = %d, control cells = %d\nfast=%v\nctrl=%v", sh.name, qi, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s dump %d cell %d: fast = %q, control = %q", sh.name, qi, i, got[i], want[i])
				}
			}
		}
		ctrl.Close()
	}
}

// sqlLiteralText renders one bound Go value as the SQL literal a control
// statement spells it with (the same literal bindLiteral's node carries).
func sqlLiteralText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		h := make([]byte, 0, len(x)*2+3)
		h = append(h, 'x', '\'')
		const hexdigits = "0123456789ABCDEF"
		for _, b := range x {
			h = append(h, hexdigits[b>>4], hexdigits[b&0xF])
		}
		return string(append(h, '\''))
	case float64:
		// bindFloatLiteral renders NaN as NULL and refuses infinities.
		if math.IsNaN(x) {
			return "NULL"
		}
		if math.IsInf(x, 0) {
			if x > 0 {
				return "9e999"
			}
			return "-9e999"
		}
		return fmtFloat(x)
	default:
		return fmt.Sprintf("%d", v)
	}
}

// fmtFloat renders a float the way the SQL text spells it (the corpus needs
// stable statement text, not Go's %g shortest form everywhere).
func fmtFloat(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	// Trim trailing zeros but keep one decimal (stays a REAL literal).
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	if i > 0 && s[i-1] == '.' {
		i++
	}
	return s[:i]
}

// flip returns the lower-case table spelling on even rounds and the
// upper-case spelling on odd rounds (see the control-engine note in
// TestInsLitParityCorpus).
func flip(r int, name string) string {
	if r%2 == 0 {
		return name
	}
	upper := []byte(name)
	for i := range upper {
		if upper[i] >= 'a' && upper[i] <= 'z' {
			upper[i] -= 32
		}
	}
	return string(upper)
}

// queryDump renders every result cell as its string (quote/typeof cells are
// TEXT by construction; integers render via the driver's formatting).
func queryDump(t *testing.T, db *frigolite.DB, q string) []string {
	t.Helper()
	r := db.Query(q)
	if r.Error != nil {
		t.Fatalf("%s: %v", q, r.Error)
	}
	out := make([]string, 0, len(r.Rows)*8)
	for _, row := range r.Rows {
		for _, c := range row {
			switch v := c.(type) {
			case nil:
				out = append(out, "<nil>")
			case []byte:
				out = append(out, "blob:"+string(v))
			default:
				out = append(out, fmt.Sprintf("%v", v))
			}
		}
	}
	return out
}
