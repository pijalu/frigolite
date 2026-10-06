package exec

// Prepared-statement parameter binding: a BindPlan maps every parameter
// occurrence of one prepared statement to its resolve.c slot so a prepared
// AST can be re-executed with freshly bound values by substituting each
// sql.ParameterExpr marker with a literal expression node (copy-on-write,
// same walker as the template cache — see template_clone.go).
//
// Slot assignment follows sqlite3ExprAssignVarNumber exactly like
// CollectParameterNames: bare "?" takes the next sequential index in AST
// walk order (resolve.c itself walks the expression tree), "?NNN"/":NNN"
// take slot NNN, and named tokens share one slot assigned at first
// appearance. Numbered and named occurrences are cross-checked against the
// node's own token text during substitution, so a walk order that disagreed
// with the source order would refuse the fast path instead of binding the
// wrong slot.

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// BindPlan describes the parameter markers of one prepared statement: occ
// holds the 1-based slot of every sql.ParameterExpr in the clone walker's
// visit order, names maps each lowercased parameter token to its slot, and
// count is the number of distinct slots (the bound-value table length).
// A plan with no occurrences serves a parameterless statement: the prepared
// AST is executed directly with no clone at all.
type BindPlan struct {
	occ   []int
	names map[string]int
	count int
}

// Count returns the number of distinct parameter slots (the length the
// bound-value table must have).
func (p *BindPlan) Count() int {
	if p == nil {
		return 0
	}
	return p.count
}

// BuildBindPlan scans the prepared SQL with the real tokenizer and records
// each parameter occurrence's resolve.c slot. It returns nil when the
// statement list is not a single DML/SELECT statement the bind walker can
// clone (any other shape keeps the caller's fallback path). CollectParameterNames
// validation (slot range) already ran at prepare; a range failure here
// returns nil.
func BuildBindPlan(sqlText string, stmts []sql.Stmt) *BindPlan {
	if len(stmts) != 1 {
		return nil
	}
	switch stmts[0].(type) {
	case *sql.InsertStmt, *sql.SelectStmt, *sql.UpdateStmt, *sql.DeleteStmt:
	default:
		return nil
	}
	t := sql.NewTokenizer(sqlText)
	p := &paramAssigner{}
	var occ []int
	for {
		tok := t.Next()
		if tok.Type == sql.TokenEOF {
			break
		}
		if tok.Type != sql.TokenParam {
			if tok.Type == sql.TokenError {
				return nil // unreachable on parsed SQL; refuse rather than guess
			}
			continue
		}
		slot, ok := p.assignOccurrence(tok.Value)
		if !ok {
			return nil
		}
		occ = append(occ, slot)
	}
	return &BindPlan{occ: occ, names: p.named, count: p.next}
}

// assignOccurrence assigns one parameter token's slot and reports it
// (resolve.c sqlite3ExprAssignVarNumber; ':NNN' is a NUMBERED variable).
func (p *paramAssigner) assignOccurrence(token string) (int, bool) {
	// resolve.c: ':NNN' with an all-digit name is a NUMBERED variable,
	// exactly like '?NNN'.
	numbered := token[0] == '?' && len(token) > 1 ||
		token[0] == ':' && len(token) > 1 && isDigitByte(token[1])
	switch {
	case numbered:
		slot, err := p.assignNumbered(token, strings.TrimLeft(token[1:], ":?"))
		if err != nil {
			return 0, false
		}
		return slot, true
	case token[0] == '?':
		p.assignBare()
		return p.next, true
	default:
		p.assignNamed(token)
		return p.named[strings.ToLower(token)], true
	}
}

// BindStmtValues clones the prepared statement list with the bound values
// substituted for its parameter markers (copy-on-write: only the statement
// root and the expression chain leading to each substituted marker are
// cloned). values must hold one entry per plan slot (unbound slots carry
// nil, SQL NULL). It returns ok=false when the statement cannot serve the
// values (unsupported expression kind, occurrence/slot mismatch); the
// caller then falls back to its text-rendering path. A plan without
// occurrences returns the statement list unchanged — the shared AST is
// immutable during execution.
func BindStmtValues(stmts []sql.Stmt, plan *BindPlan, values []interface{}) ([]sql.Stmt, bool) {
	if plan == nil || len(values) != plan.count {
		return nil, false
	}
	if len(plan.occ) == 0 {
		return stmts, true
	}
	out := make([]sql.Stmt, len(stmts))
	c := exprClone{bind: plan, bindValues: values}
	for i, stmt := range stmts {
		cloned, ok := c.stmt(stmt)
		if !ok {
			return nil, false
		}
		out[i] = cloned
	}
	if c.bindOccI != len(plan.occ) {
		return nil, false
	}
	return out, true
}

// bindSlot consumes the next parameter occurrence and returns the bound
// value: it cross-checks numbered/named tokens against the node's own text
// and reads the plan's slot table. Both substitution forms (bindParam for
// general expression positions, bindParamTuple for VALUES tuples) advance
// the same walk state, exactly once per occurrence.
func (c *exprClone) bindSlot(node *sql.ParameterExpr) (interface{}, bool) {
	if c.bindOccI >= len(c.bind.occ) {
		return nil, false
	}
	slot := c.bind.occ[c.bindOccI]
	if want, checkable := bindTokenSlot(node.Name, c.bind.names); checkable && want != slot {
		return nil, false
	}
	c.bindOccI++
	if slot < 1 || slot > len(c.bindValues) {
		return nil, false
	}
	return c.bindValues[slot-1], true
}

// bindParam substitutes one parameter marker with a literal node carrying
// the bound value (the node is the only carrier outside INSERT VALUES
// tuples). The second return is the InsLitVals stash candidate (see
// bindLiteralStash) — callers outside the tuple path ignore it.
func (c *exprClone) bindParam(node *sql.ParameterExpr) (sql.Expr, interface{}, bool) {
	v, ok := c.bindSlot(node)
	if !ok {
		return nil, nil, false
	}
	return bindLiteralStash(v)
}

// bindParamTuple is the INSERT VALUES-tuple form: when the bound value can
// serve the tuple stash verbatim (bindStashValue), the shared placeholder
// node replaces the literal — the stash IS the substitution, and the node is
// never evaluated (a FormatInt render plus node allocation per slot per
// execution dropped from the dominant insert path). Refusal kinds keep the
// real literal node.
func (c *exprClone) bindParamTuple(node *sql.ParameterExpr) (sql.Expr, interface{}, bool) {
	v, ok := c.bindSlot(node)
	if !ok {
		return nil, nil, false
	}
	if stash, ok := bindStashValue(v); ok {
		return bindStashNode, stash, true
	}
	return bindLiteralStash(v)
}

// bindStashNode is the placeholder expression node for a VALUES-tuple bind
// slot whose value is served from the InsLitVals stash instead of the AST
// (bindParamTuple). The stash entry is by contract identical in kind and
// content to EvalExpr's result for the real literal node (bindStashValue),
// so execdml's tuple evaluation never touches this node. A shared immutable
// NULL keeps the node non-nil for the structural walkers (arity checks, the
// DML validator) while costing no per-execution allocation; if any
// unforeseen path ever evaluated it, a NULL would surface immediately rather
// than a silently wrong value. Only the INSERT VALUES tuple path returns it
// — parameters elsewhere (upsert assignments, RETURNING, WHERE) evaluate
// through a real literal node.
var bindStashNode sql.Expr = &sql.NullLit{}

// bindLiteralStash builds the literal node for one bound value and reports
// whether the value itself can serve as the tuple stash entry. Callers that
// consume the stash (the VALUES-tuple path) may replace the node with
// bindStashNode; callers that discard it must keep the node — it is the
// only carrier of the substitution.
func bindLiteralStash(v interface{}) (sql.Expr, interface{}, bool) {
	node, ok := bindLiteral(v)
	if !ok {
		return nil, nil, false
	}
	stash, ok := bindStashValue(v)
	if !ok {
		return node, nil, true
	}
	return node, stash, true
}

// bindStashValue reports the SQL value execution would evaluate for the
// literal node bindLiteral builds from v: identical kind and content to
// EvalExpr's result for that node (int-family binds parse back to int64, a
// finite float64 parses back to itself through its 'g'/".0" text, a string
// serves a StringLit verbatim). Kinds whose parse-back is ambiguous or
// exotic (unsigned beyond int64, blob, nil/NULL, NaN→NULL) are refused —
// the stash entry stays nil and the node is evaluated as before, exactly
// like the template slot-path gates.
func bindStashValue(v interface{}) (interface{}, bool) {
	switch x := v.(type) {
	case int64, string:
		// Serve the caller's own interface word — re-boxing the concrete
		// value would pay an allocation per slot per execution.
		return v, true
	case float64:
		return bindStashFloat(x)
	case bool:
		if x {
			return int64(1), true
		}
		return int64(0), true
	case int, int8, int16, int32, uint, uint8, uint16, uint32, uint64:
		return bindStashInt(v)
	}
	return nil, false
}

// bindStashFloat stashes a finite float64 verbatim; NaN binds as NULL
// (bindFloatLiteral) and an infinity refuses the node outright — neither
// may be stashed as the float itself.
func bindStashFloat(f float64) (interface{}, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	return f, true
}

// bindStashInt converts any Go integer-kind (or bool) bind to the int64 the
// literal text parses back to. An unsigned value beyond int64 would parse
// back as a REAL — refused (the node is evaluated as before).
func bindStashInt(v interface{}) (interface{}, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), true
		}
		return nil, false
	case uint:
		if uint64(x) <= math.MaxInt64 {
			return int64(x), true
		}
		return nil, false
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	}
	return nil, false
}

// bindStashFor readies the clone's InsLitVals to the Values shape for the
// bind-mode tuple stash, reusing a recycled tenant's arrays when the shape
// matches (the same once-per-tenant allocation rule as the template slot
// path's stashTarget). Shape mismatches — a tenant last used by a different
// statement — allocate fresh.
func bindStashFor(clone *sql.InsertStmt, values [][]sql.Expr) [][]interface{} {
	if len(clone.InsLitVals) == len(values) {
		same := true
		for ti, tuple := range values {
			if cap(clone.InsLitVals[ti]) < len(tuple) || len(clone.InsLitVals[ti]) != len(tuple) {
				same = false
				break
			}
		}
		if same {
			return clone.InsLitVals
		}
	}
	stash := make([][]interface{}, len(values))
	for ti, tuple := range values {
		stash[ti] = make([]interface{}, len(tuple))
	}
	clone.InsLitVals = stash
	return stash
}

// bindTokenSlot re-derives a parameter token's slot from its own text:
// "?NNN"/":NNN" carry their slot, a named token resolves through the
// plan's name table; a bare "?" has no self-describing slot (checkable
// false — its sequential assignment is the clone walk order itself).
func bindTokenSlot(token string, names map[string]int) (int, bool) {
	if len(token) > 1 && (token[0] == '?' || token[0] == ':') && isAllDigitRun(token[1:]) {
		n := 0
		for i := 1; i < len(token); i++ {
			n = n*10 + int(token[i]-'0')
		}
		return n, true
	}
	if slot, ok := names[strings.ToLower(token)]; ok {
		return slot, true
	}
	return 0, false
}

// isAllDigitRun reports whether s is a non-empty run of ASCII digits.
func isAllDigitRun(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigitByte(s[i]) {
			return false
		}
	}
	return true
}

// bindLiteral builds the literal expression node for one bound value. The
// node kind follows the value's Go kind the way a fresh parse of the
// rendered literal would produce (the template-cache kind gates: an
// integral REAL stays REAL, an int stays INTEGER). Values with no literal
// equivalent refuse the substitution (ok=false), keeping the caller's
// fallback path authoritative for exotic types.
func bindLiteral(v interface{}) (sql.Expr, bool) {
	switch x := v.(type) {
	case nil:
		return &sql.NullLit{}, true
	case bool:
		return bindBoolLiteral(x), true
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(x, 10)}, true
	case int, int8, int16, int32:
		return bindSignedLiteral(v)
	case uint64, uint, uint8, uint16, uint32:
		return bindUnsignedLiteral(v)
	case float64:
		return bindFloatLiteral(x)
	case string:
		return &sql.StringLit{Value: x}, true
	case []byte:
		return bindBlobLiteral(x)
	}
	return nil, false
}

// bindBoolLiteral renders a Go bool as SQLite's integer truth value.
func bindBoolLiteral(b bool) sql.Expr {
	if b {
		return &sql.NumericLit{Value: "1"}
	}
	return &sql.NumericLit{Value: "0"}
}

// bindSignedLiteral renders any Go signed integer kind as an INTEGER literal.
func bindSignedLiteral(v interface{}) (sql.Expr, bool) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int8:
		n = int64(x)
	case int16:
		n = int64(x)
	case int32:
		n = int64(x)
	default:
		return nil, false
	}
	return &sql.NumericLit{Value: strconv.FormatInt(n, 10)}, true
}

// bindUnsignedLiteral renders any Go unsigned integer kind as an INTEGER
// literal.
func bindUnsignedLiteral(v interface{}) (sql.Expr, bool) {
	var n uint64
	switch x := v.(type) {
	case uint64:
		n = x
	case uint:
		n = uint64(x)
	case uint8:
		n = uint64(x)
	case uint16:
		n = uint64(x)
	case uint32:
		n = uint64(x)
	default:
		return nil, false
	}
	return &sql.NumericLit{Value: strconv.FormatUint(n, 10)}, true
}

// bindFloatLiteral renders a float64 preserving the REAL kind: an integral
// float needs the trailing ".0" ('g' drops the decimal point), NaN renders
// as NULL per SQLite semantics, and an infinity has no numeric literal (the
// fallback's fmt.Sprint path keeps its historical behavior).
func bindFloatLiteral(f float64) (sql.Expr, bool) {
	if math.IsNaN(f) {
		return &sql.NullLit{}, true
	}
	if math.IsInf(f, 0) {
		return nil, false
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return &sql.NumericLit{Value: s}, true
}

// bindBlobLiteral renders a BLOB literal, copying the bytes: the cloned AST
// may outlive the caller's buffer (the engine holds record values), and a
// host-side mutation must not corrupt a stored row.
func bindBlobLiteral(b []byte) (sql.Expr, bool) {
	buf := make([]byte, len(b))
	copy(buf, b)
	return &sql.BlobLit{Value: buf}, true
}
