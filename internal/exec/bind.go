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

// bindParam substitutes one parameter marker: it consumes the next
// occurrence slot, cross-checks numbered/named tokens against the node's
// own text, and builds the literal node for the slot's bound value.
func (c *exprClone) bindParam(node *sql.ParameterExpr) (sql.Expr, bool) {
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
	return bindLiteral(c.bindValues[slot-1])
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
