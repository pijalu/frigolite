// SPDX-License-Identifier: GPL-3.0-or-later
package execdml

import (
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// parseCreateTableForConstraints parses a stored CREATE TABLE statement so
// the xfer-optimization decision can compare table-level constraints
// (CHECK/FK), STRICT and the WITHOUT ROWID flag. Returns nil when the SQL
// does not parse as an ordinary table (views, virtual tables).
func parseCreateTableForConstraints(createSQL string) *sql.CreateTableStmt {
	stmts, err := parse.ParseSQL(createSQL)
	if err != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct.AsSelect != nil {
		return nil
	}
	return ct
}

// tableChecks collects the CHECK expressions in declaration order: SQLite's
// Table.pCheck flattens column-level and table-level CHECK constraints in
// schema order.
func tableChecks(ct *sql.CreateTableStmt) []sql.Expr {
	var checks []sql.Expr
	for i := range ct.Columns {
		if ct.Columns[i].Check != nil {
			checks = append(checks, ct.Columns[i].Check)
		}
	}
	for _, tc := range ct.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			checks = append(checks, tc.Expr)
		}
	}
	return checks
}

// destIndexesCompatible reports whether every index on the destination table
// has a compatible source index (insert.c's xferCompatibleIndex loop: every
// DEST index needs a SRC counterpart — the src may carry extra indexes).
func (e *DMLExecutor) destIndexesCompatible(destTable, srcTable string, destCols, srcCols []sql.ColumnDef) bool {
	destIdxs, err1 := e.schemaIndexesFor(destTable)
	srcIdxs, err2 := e.schemaIndexesFor(srcTable)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, di := range destIdxs {
		found := false
		for _, si := range srcIdxs {
			if e.xferCompatibleIndex(destCols, srcCols, di, si) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// xferIndexMeta is the parsed shape of one index definition for the
// compatibility comparison (insert.c's Index fields the C reads).
type xferIndexMeta struct {
	unique  bool
	cols    []xferIndexCol
	partIdx string // normalized partial-WHERE text ("" when none)
}

type xferIndexCol struct {
	name    string // column name, or the expression text
	isExpr  bool
	desc    bool
	collate string
}

// schemaIndexesFor returns the parsed explicit indexes of a table (the
// schema's CREATE INDEX entries whose TblName matches).
func (e *DMLExecutor) schemaIndexesFor(table string) ([]xferIndexMeta, error) {
	idxs, err := e.ctx.Schema().FindIndexesForTable(table)
	if err != nil {
		return nil, err
	}
	out := make([]xferIndexMeta, 0, len(idxs))
	for _, e := range idxs {
		if e == nil || e.Type != schema.TypeIndex {
			continue
		}
		out = append(out, parseXferIndexMeta(e.SQL))
	}
	return out, nil
}

// parseXferIndexMeta reduces one CREATE INDEX statement to the fields
// xferCompatibleIndex compares: uniqueness, per-key column/expr + sort
// order + collation, and the normalized partial-WHERE text.
func parseXferIndexMeta(indexSQL string) xferIndexMeta {
	meta := xferIndexMeta{}
	upper := strings.ToUpper(indexSQL)
	meta.unique = strings.Contains(upper, "UNIQUE")
	start := strings.Index(indexSQL, "(")
	if start < 0 {
		return meta
	}
	// The key list ends at the matching close paren; a partial WHERE (if
	// any) follows it.
	depth := 0
	end := -1
	for i := start; i < len(indexSQL); i++ {
		switch indexSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				i = len(indexSQL)
			}
		}
	}
	if end < 0 {
		return meta
	}
	rest := strings.TrimSpace(indexSQL[end+1:])
	if cu := strings.ToUpper(rest); strings.HasPrefix(cu, "WHERE") {
		meta.partIdx = normalizeSQLText(rest)
	}
	for _, c := range splitTopLevel(indexSQL[start+1 : end]) {
		col := parseXferIndexCol(c)
		if col.name == "" {
			continue
		}
		meta.cols = append(meta.cols, col)
	}
	return meta
}

// parseXferIndexCol reduces one key item to its name/expression, sort order
// and collation.
func parseXferIndexCol(item string) xferIndexCol {
	item = strings.TrimSpace(item)
	if item == "" {
		return xferIndexCol{}
	}
	collate := ""
	if ci := strings.Index(strings.ToUpper(item), " COLLATE "); ci >= 0 {
		collate = strings.TrimSpace(item[ci+len(" COLLATE "):])
		item = strings.TrimSpace(item[:ci])
	}
	desc := false
	cu := strings.ToUpper(item)
	if di := strings.LastIndex(cu, " DESC"); di >= 0 && di == len(cu)-len(" DESC") {
		desc = true
		item = strings.TrimSpace(item[:di])
	} else if ai := strings.LastIndex(cu, " ASC"); ai >= 0 && ai == len(cu)-len(" ASC") {
		item = strings.TrimSpace(item[:ai])
	}
	item = strings.Trim(item, "`\"[]")
	out := xferIndexCol{name: item, desc: desc, collate: collate}
	// An expression key contains characters a plain identifier cannot.
	if strings.ContainsAny(item, "()+-*/%<>=|& ") {
		out.isExpr = true
		out.name = normalizeSQLText(item)
	} else {
		out.name = strings.ToLower(item)
	}
	return out
}

// splitTopLevel splits a comma list at top-level parentheses depth.
func splitTopLevel(s string) []string {
	var parts []string
	depth := 0
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, s[last:])
	return parts
}

// normalizeSQLText collapses whitespace and lowercases for the textual
// comparisons the decision makes on partial WHERE clauses and expression
// keys (sqlite3ExprCompare ignores spelling case and whitespace).
func normalizeSQLText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// xferCompatibleIndex compares two parsed index shapes with insert.c's
// xferCompatibleIndex: equal key count, equal uniqueness (onError), equal
// column ordinals/expressions, equal sort orders and collations, equal
// partial WHERE.
func (e *DMLExecutor) xferCompatibleIndex(destCols, srcCols []sql.ColumnDef, dest, src xferIndexMeta) bool {
	if len(dest.cols) != len(src.cols) || dest.unique != src.unique {
		return false
	}
	for i := range dest.cols {
		d, s := dest.cols[i], src.cols[i]
		if d.isExpr != s.isExpr || d.desc != s.desc {
			return false
		}
		// Index key columns compare by their ORDINAL in the owning table
		// (insert.c's aiColumn comparison — insert4-6.3's t2(x ASC, y ASC)
		// is compatible with t3(a, b) because the ordinals match).
		if !d.isExpr {
			dx, dOk := columnOrdinal(destCols, d.name)
			sy, sOk := columnOrdinal(srcCols, s.name)
			if dOk && sOk {
				if dx != sy {
					return false
				}
			} else if !strings.EqualFold(d.name, s.name) {
				return false
			}
		} else if !strings.EqualFold(d.name, s.name) {
			return false
		}
		if !strings.EqualFold(d.collate, s.collate) {
			return false
		}
	}
	return dest.partIdx == src.partIdx
}

// exprsStructurallyEqual compares two expressions with sqlite3ExprCompare's
// semantics for the decision: equal structure, equal operators, equal
// literal tokens. Column references compare by their ORDINAL in the owning
// table (sqlite3ExprCompare assumes both sides belong to the same schema
// table when iTab<0: `b>a` over (a,b) equals `y>x` over (x,y)), falling
// back to the name when either side does not resolve. Node kinds the
// compare does not implement count as DIFFERENT (the transfer is
// disabled), matching sqlite3ExprCompare's non-zero return.
func (e *DMLExecutor) exprsStructurallyEqual(destCols, srcCols []sql.ColumnDef, a, b sql.Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch x := a.(type) {
	case *sql.ColumnRef:
		y, ok := b.(*sql.ColumnRef)
		if !ok {
			return false
		}
		dx, dOk := columnOrdinal(destCols, x.Name)
		sy, sOk := columnOrdinal(srcCols, y.Name)
		if dOk && sOk {
			return dx == sy
		}
		// A reference that does not resolve to a declared column (SQLite's
		// rowid in a CHECK constraint — insert4-12.x) has no ordinal to
		// match: sqlite3ExprCompare's conservative verdict is DIFFERENT,
		// which disables the transfer (12.2 expects 0 where 3.1's plain
		// column checks expect 1).
		return false
	case *sql.NumericLit:
		y, ok := b.(*sql.NumericLit)
		return ok && strings.EqualFold(strings.TrimSpace(x.Value), strings.TrimSpace(y.Value))
	case *sql.StringLit:
		y, ok := b.(*sql.StringLit)
		return ok && x.Value == y.Value
	case *sql.NullLit:
		_, ok := b.(*sql.NullLit)
		return ok
	case *sql.BlobLit:
		y, ok := b.(*sql.BlobLit)
		return ok && string(x.Value) == string(y.Value)
	case *sql.ParenExpr:
		y, ok := b.(*sql.ParenExpr)
		return ok && e.exprsStructurallyEqual(destCols, srcCols, x.Expr, y.Expr)
	case *sql.BinaryOp:
		y, ok := b.(*sql.BinaryOp)
		return ok && strings.EqualFold(x.Operator, y.Operator) &&
			e.exprsStructurallyEqual(destCols, srcCols, x.Left, y.Left) &&
			e.exprsStructurallyEqual(destCols, srcCols, x.Right, y.Right)
	case *sql.UnaryOp:
		y, ok := b.(*sql.UnaryOp)
		return ok && strings.EqualFold(x.Operator, y.Operator) &&
			e.exprsStructurallyEqual(destCols, srcCols, x.Operand, y.Operand)
	case *sql.IsNull:
		y, ok := b.(*sql.IsNull)
		return ok && e.exprsStructurallyEqual(destCols, srcCols, x.Operand, y.Operand)
	case *sql.IsNotNull:
		y, ok := b.(*sql.IsNotNull)
		return ok && e.exprsStructurallyEqual(destCols, srcCols, x.Operand, y.Operand)
	case *sql.InList:
		y, ok := b.(*sql.InList)
		if !ok || x.Negated != y.Negated || len(x.List) != len(y.List) {
			return false
		}
		if !e.exprsStructurallyEqual(destCols, srcCols, x.Operand, y.Operand) {
			return false
		}
		for i := range x.List {
			if !e.exprsStructurallyEqual(destCols, srcCols, x.List[i], y.List[i]) {
				return false
			}
		}
		return true
	case *sql.Between:
		y, ok := b.(*sql.Between)
		if !ok || x.Negated != y.Negated {
			return false
		}
		return e.exprsStructurallyEqual(destCols, srcCols, x.Operand, y.Operand) &&
			e.exprsStructurallyEqual(destCols, srcCols, x.Low, y.Low) &&
			e.exprsStructurallyEqual(destCols, srcCols, x.High, y.High)
	case *sql.FuncCall:
		y, ok := b.(*sql.FuncCall)
		if !ok || len(x.Args) != len(y.Args) {
			return false
		}
		if !strings.EqualFold(x.Name, y.Name) {
			return false
		}
		for i := range x.Args {
			if !e.exprsStructurallyEqual(destCols, srcCols, x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// columnOrdinal resolves a column name to its declared ordinal.
func columnOrdinal(cols []sql.ColumnDef, name string) (int, bool) {
	for i := range cols {
		if strings.EqualFold(cols[i].Name, name) {
			return i, true
		}
	}
	return 0, false
}
