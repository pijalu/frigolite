package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// emitAggregateDistinctRows renders the resetAccumulator OP_OpenEphemeral
// rows for DISTINCT aggregates (select.c:6788-6807). C opens one unordered
// b-tree per distinct aggregate whose argument cannot be delivered in order
// by the chosen scan; codeDistinct later uses it for the Found/IdxInsert pair.
func (e *SelectEngine) emitAggregateDistinctRows(prog *explainProgram, s *sql.SelectStmt) {
	cursor := int64(0)
	for _, arg := range aggregateDistinctArgs(s) {
		if e.aggregateDistinctOrdered(s, arg) {
			continue
		}
		prog.emit("OpenEphemeral", cursor, 0, 0, "k(1,B)", 8, "nColumn=0")
		cursor++
	}
}

// aggregateDistinctArgs returns the argument of every DISTINCT aggregate in
// the statement's expressions (one per AggInfo_func, C's iDistinct>=0 set).
func aggregateDistinctArgs(s *sql.SelectStmt) []sql.Expr {
	var args []sql.Expr
	add := func(expr sql.Expr) {
		WalkExprFull(expr, func(node sql.Expr) {
			call, ok := node.(*sql.FuncCall)
			if ok && call.Distinct && len(call.Args) == 1 {
				args = append(args, call.Args[0])
			}
		})
	}
	for _, col := range s.Columns {
		add(col.Expr)
	}
	for _, g := range s.GroupBy {
		add(g)
	}
	add(s.Having)
	for _, ob := range s.OrderBy {
		add(ob.Expr)
	}
	return args
}

// aggregateDistinctOrdered reports whether the scan can deliver the DISTINCT
// argument in order (where.c's WHERE_DISTINCT_ORDERED for DISTINCTBY): a
// rowid argument is unique, and a plain column led by an index after its
// equality-bound prefix needs no dedup b-tree.
func (e *SelectEngine) aggregateDistinctOrdered(s *sql.SelectStmt, arg sql.Expr) bool {
	ref := aggregateDistinctColumn(arg)
	if ref == nil {
		return false // an expression argument cannot be index-ordered here
	}
	if isRowIDName(strings.ToLower(ref.Name)) {
		return true
	}
	for _, t := range e.collectQueryTables(s) {
		if t.subquery != nil || !aggregateDistinctRefMatches(ref, t) {
			continue
		}
		if distinctColumnIsConstant(s, ref, t) {
			return true // C's isDistinctRedundant: one possible DISTINCT value
		}
		group, ok := distinctGroupColumns(s, t)
		if ok && e.indexServesDistinctColumn(t.real, ref.Name, e.distinctEqualityColumns(s, t), group, s.Where) {
			return true
		}
	}
	return false
}

// aggregateDistinctColumn unwraps parentheses and an explicit COLLATE on a
// DISTINCT aggregate's single argument.
func aggregateDistinctColumn(expr sql.Expr) *sql.ColumnRef {
	for {
		switch v := expr.(type) {
		case *sql.ParenExpr:
			expr = v.Expr
		case *sql.BinaryOp:
			if !strings.EqualFold(v.Operator, "COLLATE") {
				return nil
			}
			expr = v.Left
		case *sql.ColumnRef:
			return v
		default:
			return nil
		}
	}
}

func aggregateDistinctRefMatches(ref *sql.ColumnRef, t queryTable) bool {
	if ref.Table == "" {
		return true
	}
	name := unqualifiedObjectName(ref.Table)
	return strings.EqualFold(name, t.display) || strings.EqualFold(name, t.real)
}

// distinctColumnIsConstant reports whether WHERE pins the DISTINCT argument
// to a constant (where.c isDistinctRedundant): one value can never need a
// dedup b-tree. A column=column term does not qualify.
func distinctColumnIsConstant(s *sql.SelectStmt, ref *sql.ColumnRef, t queryTable) bool {
	for _, conj := range splitAnd(s.Where) {
		bin, ok := conj.(*sql.BinaryOp)
		if !ok || !(strings.EqualFold(bin.Operator, "=") || strings.EqualFold(bin.Operator, "IS")) {
			continue
		}
		for _, pair := range [][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
			col, ok := pair[0].(*sql.ColumnRef)
			if ok && aggregateDistinctRefMatches(col, t) &&
				strings.EqualFold(col.Name, ref.Name) {
				if _, anotherCol := pair[1].(*sql.ColumnRef); !anotherCol {
					return true
				}
			}
		}
	}
	return false
}

// distinctEqualityColumns collects the columns held constant by WHERE for
// one FROM operand: an index's equality prefix over these columns still
// delivers the following columns in order (GROUP BY is a prefix, not an
// equality binding, and is handled by distinctGroupColumns).
func (e *SelectEngine) distinctEqualityColumns(s *sql.SelectStmt, t queryTable) map[string]bool {
	eq := make(map[string]bool)
	for _, conj := range splitAnd(s.Where) {
		bin, ok := conj.(*sql.BinaryOp)
		if !ok || !(strings.EqualFold(bin.Operator, "=") || strings.EqualFold(bin.Operator, "IS")) {
			continue
		}
		e.addDistinctEqualitySide(bin.Left, t, eq)
		e.addDistinctEqualitySide(bin.Right, t, eq)
	}
	return eq
}

// distinctGroupColumns returns the plain GROUP BY columns of one FROM
// operand; ok is false when a group term is an expression or belongs to
// another operand (no index prefix can then be proven for this table).
func distinctGroupColumns(s *sql.SelectStmt, t queryTable) ([]string, bool) {
	out := make([]string, 0, len(s.GroupBy))
	for _, g := range s.GroupBy {
		ref, ok := g.(*sql.ColumnRef)
		if !ok || !aggregateDistinctRefMatches(ref, t) {
			return nil, false
		}
		out = append(out, strings.ToLower(ref.Name))
	}
	return out, true
}

func (e *SelectEngine) addDistinctEqualitySide(expr sql.Expr, t queryTable, eq map[string]bool) {
	ref, ok := expr.(*sql.ColumnRef)
	if ok && aggregateDistinctRefMatches(ref, t) {
		eq[strings.ToLower(ref.Name)] = true
	}
}

// indexServesDistinctColumn reports whether an index on tableName has the
// DISTINCT column immediately after its equality-bound prefix.
func (e *SelectEngine) indexServesDistinctColumn(tableName, col string, eq map[string]bool, group []string, where sql.Expr) bool {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return false
	}
	obj := unqualifiedObjectName(tableName)
	for _, ent := range entries {
		if ent.Type != "index" || !strings.EqualFold(ent.TblName, obj) ||
			!e.indexEntryUsableForDistinct(ent, where) {
			continue
		}
		cols := e.indexEntryColumns(ent)
		i := e.skipDistinctEqPrefix(cols, eq)
		if !distinctGroupPrefix(cols, group, &i) {
			continue
		}
		if i < len(cols) && strings.EqualFold(cols[i], col) {
			return true
		}
	}
	return false
}

// indexEntryUsableForDistinct excludes unordered and unproven partial
// indexes from the DISTINCT order check.
func (e *SelectEngine) indexEntryUsableForDistinct(ent *schema.Entry, where sql.Expr) bool {
	if e.indexIsUnordered(ent.Name) {
		return false
	}
	partial := strings.Contains(strings.ToUpper(ent.SQL), " WHERE ")
	return !partial || (where != nil && e.partialIndexImplied(ent, where))
}

// skipDistinctEqPrefix advances past index columns fixed by WHERE.
func (e *SelectEngine) skipDistinctEqPrefix(cols []string, eq map[string]bool) int {
	i := 0
	for i < len(cols) && eq[strings.ToLower(cols[i])] {
		i++
	}
	return i
}

// distinctGroupPrefix advances over the GROUP BY prefix in declared order.
func distinctGroupPrefix(cols, group []string, at *int) bool {
	for _, g := range group {
		if *at >= len(cols) || !strings.EqualFold(cols[*at], g) {
			return false
		}
		*at++
	}
	return true
}
