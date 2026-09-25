package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// ORDER BY resolution and comparison for SELECT execution (split from
// select_columns.go for file-size hygiene): ordinal-term rewriting, row
// value resolution, comparator fallbacks, and collation transfer. Column
// naming and star expansion live in select_columns.go.

// lessRows returns true if row i should come before row j according to ORDER BY.
// resultCols maps ORDER BY aliases/column names to result column positions.
// normalizeOrderByExpr unwraps UnaryOp(+/-) over a NumericLit and strips all
// COLLATE operators so the underlying positional or column-ref operand is
// visible. The term's collation is applied later by compareOrderByValues.
func normalizeOrderByExpr(obExpr sql.Expr) sql.Expr {
	if uo, ok := obExpr.(*sql.UnaryOp); ok && (uo.Operator == "+" || uo.Operator == "-") {
		if num, ok := uo.Operand.(*sql.NumericLit); ok {
			if uo.Operator == "-" {
				return &sql.NumericLit{Value: "-" + num.Value}
			}
			return num
		}
	}
	for {
		prev := obExpr
		obExpr = stripCollate(obExpr)
		if obExpr == prev {
			break
		}
	}
	return obExpr
}

// resolveOrderByValue resolves an ORDER BY expression to a value in the output
// row at position idx. Returns false when neither positional nor column-name
// resolution applies (caller should fall back to evaluating the raw expression).
func resolveOrderByValue(obExpr sql.Expr, rows [][]interface{}, resultCols []string, idx int) (interface{}, bool) {
	if nl, ok := obExpr.(*sql.NumericLit); ok {
		if pos, err := strconv.ParseInt(nl.Value, 10, 64); err == nil && pos >= 1 && pos <= int64(len(rows[idx])) {
			return rows[idx][pos-1], true
		}
	}
	if ref, ok := stripCollate(obExpr).(*sql.ColumnRef); ok {
		if pos := resultColumnIndex(resultCols, ref.Name); pos >= 0 && pos < len(rows[idx]) {
			return rows[idx][pos], true
		}
	}
	return nil, false
}

func (e *SelectEngine) lessRows(orderBy []sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) bool {
	for _, ob := range orderBy {
		cmp := e.compareOrderByTerm(ob, rowMaps, rows, resultCols, i, j)
		if cmp < 0 {
			return true
		}
		if cmp > 0 {
			return false
		}
	}
	return false
}

// resolveOrderByOrdinalTerms rewrites a numeric ORDER BY ordinal term to its
// result column's expression when that expression is a plain unqualified
// column reference. The comparator reads such terms from the row maps, whose
// values carry the column's declared collation — ORDER BY 1 must sort by the
// output column's collation exactly like ORDER BY <name> (select.c
// sqlite3ResolveSortRefs: the sort reference takes the result column's
// collating sequence). Other expressions keep the positional fallback.
//
// Compound selects are exempt: their sort is positional over the merged rows
// by construction (resolve.c resolveCompoundOrderBy converts every term to an
// integer before the sorter runs), and the merged row maps are keyed by the
// compound's OUTPUT column names — a rewrite to the leftmost member's source
// column name would miss those maps whenever an output alias renames the
// column ("SELECT p PX ... UNION ALL SELECT x XX ... ORDER BY PX" resolves PX
// to ordinal 1, which this rewrite would turn into source name "p"; that
// evaluates NULL for every merged row and silently disables the sort,
// tkt2822-6.x). Compound column collations are carried separately by
// applyCompoundOrderByCollations' COLLATE wrapper, which the positional
// fallback honors.
func (e *SelectEngine) resolveOrderByOrdinalTerms(s *sql.SelectStmt, orderBy []sql.OrderByTerm) []sql.OrderByTerm {
	if s == nil || s.Union != nil {
		return orderBy
	}
	changed := false
	out := orderBy
	for k := range orderBy {
		newExpr, rewritten := resolveOrdinalOrderByTerm(s, orderBy[k].Expr)
		if !rewritten {
			continue
		}
		if !changed {
			out = make([]sql.OrderByTerm, len(orderBy))
			copy(out, orderBy)
			changed = true
		}
		out[k].Expr = newExpr
	}
	return out
}

// splitCollateOperand peels one trailing COLLATE operator off expr: it
// returns the operand and the collation name ("" when expr is not a COLLATE
// wrapper or its operand is not a string literal). A trailing COLLATE
// belongs to the SORT KEY, not the result-column reference: "ORDER BY 1
// COLLATE hex" must sort the resolved column under hex (expr.c keeps the
// COLLATE above the ordinal-resolved expression).
func splitCollateOperand(expr sql.Expr) (sql.Expr, string) {
	if bo, isCollate := expr.(*sql.BinaryOp); isCollate && strings.EqualFold(bo.Operator, "COLLATE") {
		if lit, isLit := bo.Right.(*sql.StringLit); isLit {
			return bo.Left, lit.Value
		}
	}
	return expr, ""
}

// wrapCollate wraps expr in a COLLATE operator over the given name.
func wrapCollate(expr sql.Expr, name string) sql.Expr {
	return &sql.BinaryOp{
		Operator: "COLLATE",
		Left:     expr,
		Right:    &sql.StringLit{Value: name},
	}
}

// resolveOrdinalOrderByTerm rewrites one numeric ORDER BY ordinal term to its
// result column's expression (with collation transfer). Returns the
// replacement expression and whether the term is rewritten.
func resolveOrdinalOrderByTerm(s *sql.SelectStmt, termExpr sql.Expr) (sql.Expr, bool) {
	expr, collateName := splitCollateOperand(termExpr)
	nl, ok := expr.(*sql.NumericLit)
	if !ok {
		return nil, false
	}
	pos, err := strconv.Atoi(nl.Value)
	if err != nil || pos < 1 || pos > len(s.Columns) {
		return nil, false
	}
	// A result column that ITSELF carries COLLATE (`SELECT c2 COLLATE hex
	// ... ORDER BY 1`) donates its collation to the sort key; the term's
	// own COLLATE (if any) takes precedence.
	resultExpr, resultCollate := splitCollateOperand(s.Columns[pos-1].Expr)
	if collateName == "" && resultCollate != "" {
		if _, isRef := resultExpr.(*sql.ColumnRef); !isRef {
			// The result expression is not a bare column (e.g.
			// `(c1||'') COLLATE numeric`): keep the term positional and
			// donate the result column's collation to the sort key.
			return wrapCollate(resultExpr, resultCollate), true
		}
	}
	ref, isRef := resultExpr.(*sql.ColumnRef)
	if !isRef || ref.Table != "" || ref.Name == "*" {
		return nil, false
	}
	if collateName != "" {
		return wrapCollate(ref, collateName), true
	}
	if resultCollate != "" {
		return wrapCollate(ref, resultCollate), true
	}
	return ref, true
}

// compareOrderByTerm compares rows i and j for a single ORDER BY term,
// resolving alias references, applying the column's declared collation via
// the row maps, and falling back to expression evaluation when a value is
// missing from the output row.
func (e *SelectEngine) compareOrderByTerm(ob sql.OrderByTerm, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	positionalIdx := orderByPositionalIndex(&ob, resultCols)
	obExpr := normalizeOrderByExpr(ob.Expr)
	ref, isRef := stripCollate(obExpr).(*sql.ColumnRef)
	if !isRef || ref.Table != "" || ref.Name == "*" {
		return e.compareOrderByFallback(ob, obExpr, rowMaps, rows, resultCols, i, j)
	}
	if positionalIdx >= 0 {
		// Ordinal sort keys read the OUTPUT ROW at the ordinal's position.
		// resolveOrderByRowValues' row-map preference must not apply: the
		// row map is keyed by NAME, and a source column or a stored
		// expression key sharing the result column's rendered name would
		// shadow the actual output value (where6-3.1, window8-1.8.8).
		if positionalIdx < len(rows[i]) && positionalIdx < len(rows[j]) {
			return e.compareOrderByValues(rows[i][positionalIdx], rows[j][positionalIdx], ob)
		}
		return e.compareOrderByFallback(ob, obExpr, rowMaps, rows, resultCols, i, j)
	}
	left, lok := resolveOrderByValue(obExpr, rows, resultCols, i)
	right, rok := resolveOrderByValue(obExpr, rows, resultCols, j)
	left, right = e.resolveOrderByRowValues(ref.Name, obExpr, rowMaps, rows, resultCols, i, j, left, right)
	if !lok || !rok {
		return e.compareOrderByFallback(ob, obExpr, rowMaps, rows, resultCols, i, j)
	}
	return e.compareOrderByValues(left, right, ob)
}

// orderByPositionalIndex resolves a numeric ORDER BY term (`ORDER BY 1`) to
// its 0-based output position, rewriting the term to the named result column
// so the comparator's declared-collation resolution sees through SELECT *
// (collate1-3.1: SELECT * FROM t(a COLLATE hex) ORDER BY 1 sorts hex). The
// POSITION survives: an ordinal names the result column AT THAT POSITION
// (resolve.c resolveOrderByTermToExprList returns the iCol-th result
// expression), so when several result columns render the same name
// (t4a.x/t4b.x in where6-3.1, or the two sum(c) OVER (ORDER BY a) windows of
// window8-1.8.8 that differ only in EXCLUDE) a name-based lookup would
// silently redirect the sort key to the FIRST matching column or to a
// row-map value keyed by the shared name. Returns -1 for non-ordinal terms
// (or out-of-range ordinals, which stay untouched).
func orderByPositionalIndex(ob *sql.OrderByTerm, resultCols []string) int {
	nl, isLit := ob.Expr.(*sql.NumericLit)
	if !isLit {
		return -1
	}
	pos, err := strconv.Atoi(nl.Value)
	if err != nil || pos < 1 || pos > len(resultCols) {
		return -1
	}
	ob.Expr = &sql.ColumnRef{Name: resultCols[pos-1]}
	return pos - 1
}

// orderByAliasColumnName resolves an ORDER BY name that is a SELECT-list
// alias to the aliased expression's column name; the second result reports
// whether the name is an alias at all.
func (e *SelectEngine) orderByAliasColumnName(name string) (string, bool) {
	if _, isAlias := e.aliasStackTop(name); !isAlias {
		return "", false
	}
	if aliasExpr, ok := e.resolveAliasRef(name); ok {
		if aliasRef, ok2 := stripCollate(aliasExpr).(*sql.ColumnRef); ok2 && aliasRef.Table == "" {
			return aliasRef.Name, true
		}
	}
	return "", true
}

// resolveOrderByRowValues overrides the output-row values for an unqualified
// column-reference ORDER BY term with the row-map values (which carry the
// column's declared collation), resolving a SELECT-list alias to the aliased
// column first.
func (e *SelectEngine) resolveOrderByRowValues(name string, obExpr sql.Expr, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int, left, right interface{}) (interface{}, interface{}) {
	// A SELECT-list alias in ORDER BY shadows a same-named real column
	// (SQLite: "SELECT z AS x, x AS z FROM d1 ORDER BY z" sorts by the
	// aliased x value, not the real z column).
	resolvedName, isAlias := e.orderByAliasColumnName(name)
	// A plain unqualified column reference carries its declared collation on
	// the row-map value (a CollatedValue marker); the output-row value loses
	// it. Prefer the row map so ORDER BY honors the column's collation
	// (d4.x COLLATE nocase).
	// When the ORDER BY name is a SELECT-list alias whose expression is not a
	// plain column reference (e.g. SELECT substr(m,2) AS m ... ORDER BY m),
	// the row map would wrongly grab a same-named source column; SQLite
	// resolves the bare alias to the output value instead (resolver01-4.1).
	if isAlias && resolvedName == "" {
		return left, right
	}
	rowName := name
	if resolvedName != "" {
		// The name is an alias; the value comes from the aliased column's
		// position in the output row. When the alias itself is an output
		// column, that position WINS and the source-column row-map lookup is
		// skipped: a compound's rebuilt row maps are keyed by output names,
		// so a resolved source name (c62) reads the wrong column and ties a
		// sort that must move rows (selectH-2.1: ORDER BY b over arms
		// "c62 AS b" / "c61 AS b").
		if pos := resultColumnIndex(resultCols, name); pos >= 0 && pos < len(rows[i]) {
			left = rows[i][pos]
			if pos < len(rows[j]) {
				right = rows[j][pos]
			}
			return left, right
		}
		rowName = resolvedName
	}
	if lm, ok := rowMaps[i].Get(rowName); ok {
		left = lm
	}
	if rm, ok := rowMaps[j].Get(rowName); ok {
		right = rm
	}
	return left, right
}

// compareOrderByFallback compares rows i and j for an ORDER BY term that is
// not a simple unqualified column reference (or whose output-row values are
// missing): evaluate the expression against the row maps.
func (e *SelectEngine) compareOrderByFallback(ob sql.OrderByTerm, obExpr sql.Expr, rowMaps []RowMap, rows [][]interface{}, resultCols []string, i, j int) int {
	// Resolve a non-column ORDER BY expression by its rendered text: when it
	// matches a result column name (e.g. "10+sum(a) OVER (ORDER BY a)" is the
	// SELECT-list expression), use the output row value — the window pass
	// already computed it.
	if pos := resultColumnIndex(resultCols, sql.ExprString(stripCollate(obExpr))); pos >= 0 {
		left, right := interface{}(nil), interface{}(nil)
		if i < len(rows) && pos < len(rows[i]) {
			left = rows[i][pos]
		}
		if j < len(rows) && pos < len(rows[j]) {
			right = rows[j][pos]
		}
		return e.compareOrderByValues(left, right, ob)
	}
	left, lok := resolveOrderByValue(obExpr, rows, resultCols, i)
	right, rok := resolveOrderByValue(obExpr, rows, resultCols, j)
	if !lok {
		// A window function expression in ORDER BY (e.g. RANK() OVER w) is a
		// distinct AST node from its SELECT-list twin; the window pass stores
		// its computed value under the rendered expression key.
		if v, ok := rowMaps[i].Get(sql.ExprString(ob.Expr)); ok {
			left = v
		} else {
			left, _ = e.ctx.EvalExpr(ob.Expr, combinedOutputRowMap(rowMaps[i], resultCols, rowAt(rows, i)))
		}
	}
	if !rok {
		if v, ok := rowMaps[j].Get(sql.ExprString(ob.Expr)); ok {
			right = v
		} else {
			right, _ = e.ctx.EvalExpr(ob.Expr, combinedOutputRowMap(rowMaps[j], resultCols, rowAt(rows, j)))
		}
	}
	return e.compareOrderByValues(left, right, ob)
}

// combinedOutputRowMap merges a source row map with the output row's values
// keyed by result column name, so names inside ORDER BY expressions resolve
// against SELECT-list aliases when no source column matches (SQLite resolves
// ORDER BY names against the result set; filter1-4.2's ORDER BY (h+1.0)
// needs the alias h). SOURCE values shadow same-named output aliases: inside
// an ORDER BY expression a name that is also a source column resolves to the
// column, not the alias (resolver01-4.1's ORDER BY lower(m) sorts by t4.m,
// not by the alias m).
func combinedOutputRowMap(src RowMap, resultCols []string, row []interface{}) RowMap {
	m := make(RowMap, len(src)+len(resultCols))
	for ci, cn := range resultCols {
		if cn != "" && ci < len(row) {
			m[cn] = row[ci]
		}
	}
	for k, v := range src {
		m[k] = v
	}
	return m
}

// rowAt returns rows[idx] or nil when out of range.
func rowAt(rows [][]interface{}, idx int) []interface{} {
	if idx < 0 || idx >= len(rows) {
		return nil
	}
	return rows[idx]
}

// compareOrderByValues compares two values for an ORDER BY term, applying
// the term's direction and explicit NULLS FIRST/LAST rules. SQLite defaults:
// NULLs sort first for ASC, last for DESC; explicit NULLS FIRST/LAST win.
// The ORDER BY term's explicit COLLATE (e.g. "ORDER BY x COLLATE nocase")
// is applied when the compared values do not already carry a collation
// marker (e.g. positional/alias ORDER BY terms resolve to output values).
// nullOrderByCmp returns the comparison result when one or both ORDER BY
// values are NULL. Returns (0, false) when neither value is NULL.
func nullOrderByCmp(leftNull, rightNull bool, ob sql.OrderByTerm) (int, bool) {
	if !leftNull && !rightNull {
		return 0, false
	}
	if leftNull && rightNull {
		return 0, true
	}
	nullsFirst := ob.NullsFirst
	if ob.NullsLast {
		nullsFirst = false
	}
	if !ob.NullsFirst && !ob.NullsLast {
		nullsFirst = !ob.Desc
	}
	if leftNull {
		if nullsFirst {
			return -1, true
		}
		return 1, true
	}
	if nullsFirst {
		return 1, true
	}
	return -1, true
}

func (e *SelectEngine) compareOrderByValues(left, right interface{}, ob sql.OrderByTerm) int {
	if cmp, isNull := nullOrderByCmp(execexpr.IsSQLNull(left), execexpr.IsSQLNull(right), ob); isNull {
		return cmp
	}
	if coll := e.orderBySortCollation(left, right, ob); coll != "" {
		// An explicit COLLATE in the ORDER BY term (or one inherited from a
		// SELECT-list alias) overrides the column's declared collation:
		// SQLite sorts ORDER BY x COLLATE binary with BINARY even when the
		// column is declared COLLATE nocase. Apply the term's collation to
		// the raw values unconditionally here.
		lc, _ := extractValue(left)
		rc, _ := extractValue(right)
		cmp := e.ctx.CompareValuesCollate(lc, rc, coll)
		if ob.Desc {
			cmp = -cmp
		}
		return cmp
	}
	cmp := e.ctx.CompareValuesWithCollate(left, right)
	if ob.Desc {
		cmp = -cmp
	}
	return cmp
}

// orderBySortCollation resolves the collation an ORDER BY term's values
// compare under: the term's explicit COLLATE first, then a SELECT-list
// alias's collation, then a collation marker carried by the compared values,
// then the schema resolver's declared column collation.
func (e *SelectEngine) orderBySortCollation(left, right interface{}, ob sql.OrderByTerm) string {
	if coll := orderByTermCollation(ob.Expr); coll != "" {
		return coll
	}
	if coll := e.aliasOrderByCollation(ob.Expr); coll != "" {
		return coll
	}
	// A bare column term with no explicit COLLATE sorts by the column's
	// DECLARED collation (expr.c sqlite3ExprCollSeq → TK_COLUMN): the scan
	// wraps declared-collation columns in CollatedValue markers, so an
	// unwrapped-order term inherits the marker's collation (reindex-2.6:
	// ORDER BY a with a TEXT PRIMARY KEY COLLATE c1 sorts reverse). When
	// the output values carry no marker (SELECT * rows), the schema
	// resolver supplies the declared collation (collate1-3.1: a COLLATE hex
	// column sorts ORDER BY 1 numerically).
	if _, c := extractValue(left); c != "" {
		return c
	}
	if _, c := extractValue(right); c != "" {
		return c
	}
	return e.declaredOrderByCollation(ob.Expr)
}

// aliasOrderByCollation resolves the collation of an ORDER BY term that
// names a SELECT-list alias (e.g. ORDER BY y where the SELECT is "SELECT x
// AS y FROM d4"): it inherits the aliased expression's collation
// (resolve.c transfers the result-set expression's collation to the alias
// reference), explicit COLLATE first and then the schema-declared column
// collation (collate8-1.11: SELECT a AS x FROM t1 ORDER BY "x" sorts by a's
// declared COLLATE nocase). A unary + over the alias reference keeps the
// collation (collate8-1.15: ORDER BY +x).
func (e *SelectEngine) aliasOrderByCollation(obExpr sql.Expr) string {
	aliasExpr, ok := e.orderTermAliasExpr(obExpr)
	if !ok {
		return ""
	}
	if coll := orderByTermCollation(aliasExpr); coll != "" {
		return coll
	}
	if e.obCollationResolver != nil {
		if c, _ := e.schemaExprCollation(aliasExpr, e.obCollationResolver); c != "" {
			return c
		}
	}
	return ""
}

// declaredOrderByCollation resolves a bare column term's schema-declared
// collation. Qualified references (ORDER BY main.t.a) resolve through the
// per-table collation map; unqualified through the merged one.
func (e *SelectEngine) declaredOrderByCollation(obExpr sql.Expr) string {
	if e.obCollationResolver == nil {
		return ""
	}
	if ref, ok := normalizeOrderByExpr(obExpr).(*sql.ColumnRef); ok {
		return e.obCollationResolver(*ref)
	}
	return ""
}

// eponymousModuleResolvable reports whether name resolves to an eponymous
// vtab module's implicit FROM source (prepare-time name resolution for
// scalar subqueries).
func (e *SelectEngine) eponymousModuleResolvable(name string) bool {
	lower := strings.ToLower(name)
	if dot := strings.LastIndex(lower, "."); dot >= 0 {
		lower = lower[dot+1:]
	}
	module, ok := e.ctx.VTables().Find(lower)
	return ok && vtab.ModuleIsEponymous(module)
}

// orderTermAliasExpr resolves an ORDER BY term that names a SELECT-list
// alias, unwrapping the COLLATE and unary+ wrappers SQLite strips before
// alias resolution (ORDER BY "x", ORDER BY [x], ORDER BY +x all resolve the
// alias x). Returns the aliased expression and true when the term is an
// unqualified alias reference.
func (e *SelectEngine) orderTermAliasExpr(obExpr sql.Expr) (sql.Expr, bool) {
	expr := obExpr
	for {
		switch v := expr.(type) {
		case *sql.UnaryOp:
			if v.Operator != "+" {
				return nil, false
			}
			expr = v.Operand
		case *sql.BinaryOp:
			if !strings.EqualFold(v.Operator, "COLLATE") {
				return nil, false
			}
			expr = v.Left
		default:
			ref, ok := expr.(*sql.ColumnRef)
			if !ok || ref.Table != "" || ref.Name == "*" {
				return nil, false
			}
			return e.aliasStackTop(ref.Name)
		}
	}
}
