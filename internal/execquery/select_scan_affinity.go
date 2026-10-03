package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// This file owns the scan's affinity-wrap planning: which columns of the
// scanned table receive affinity/collation wrappers so comparison logic
// applies SQLite's affinity and declared-collation rules. Split from
// select_scan.go for file-size hygiene.

// scanTableAffinityCols collects the column names that need affinity wrappers
// from the WHERE clause, SELECT columns, ORDER BY, and join ON/USING/NATURAL
// references (columns compared with affinity must wrap their values).
func (e *SelectEngine) scanTableAffinityCols(s *sql.SelectStmt, colDefs []sql.ColumnDef, needMaps bool) map[string]bool {
	a := &affinityCollector{cols: make(map[string]bool)}
	// Collect column references from the consuming clauses first (WHERE,
	// ORDER BY, GROUP BY, HAVING, joins): their union decides whether the
	// SELECT columns may take the bare-reference exemption below.
	a.collectExprRefs(s.Where)
	for _, ob := range s.OrderBy {
		a.collectExpr(ob.Expr)
	}
	// GROUP BY expressions need affinity/collation wrappers too: grouping a
	// NOCASE column must compare values under that collation (b3's
	// 'abc'/'aBC' group together). A bare term over a no-collation column is
	// exempt — the key computation unwraps the evaluated value and reads the
	// collation marker off it, so only a declared collation needs the
	// wrapper to survive (same exception as the SELECT columns above).
	for _, gb := range s.GroupBy {
		if !skipBareSelectRef(gb, colDefs) {
			a.collectExpr(gb)
		}
	}
	if s.Having != nil {
		a.collectExpr(s.Having)
	}
	// JOIN ON/USING/NATURAL clauses reference columns that need affinity
	// wrappers for the join comparison.
	for i := range s.Joins {
		e.collectJoinAffinity(a, &s.Joins[i], s.From.Name)
	}
	a.collectSelectColumnAffinity(s, colDefs)
	return a.result(colDefs, needMaps)
}

// collectSelectColumnAffinity collects the SELECT output columns' affinity
// references. Expressions like "xt==+xi" need the affinity of xt even when xt
// is not referenced in WHERE/ORDER BY. A bare output column reference
// (SELECT c) is exempt: every output builder peels the wrappers off its slot
// value, so the wrapper never reaches a comparison — skipping it saves one
// allocation per row on the full-scan shapes. The declared-collation
// exception keeps the CollatedValue marker alive for consumers that read the
// marker off the row value (the GROUP BY key computation groups
// 'abc'/'aBC' together through a NOCASE column's declared collation). An
// output ALIAS referenced by a consuming clause (WHERE x='abc' for
// SELECT a AS x) resolves back to its SELECT expression at evaluation time
// through the alias stack, so any collected ref matching an output alias
// cancels the exemption for the whole statement — the underlying column must
// be decoded and wrapped exactly as before.
func (a *affinityCollector) collectSelectColumnAffinity(s *sql.SelectStmt, colDefs []sql.ColumnDef) {
	exemptBare := !aliasReferenced(a.cols, s)
	for _, col := range s.Columns {
		if !(exemptBare && skipBareSelectRef(col.Expr, colDefs)) {
			a.collectExpr(col.Expr)
		}
	}
}

// aliasReferenced reports whether any collected column reference name is one
// of the statement's output aliases. Statements whose columns carry no alias
// skip the map build entirely.
func aliasReferenced(cols map[string]bool, s *sql.SelectStmt) bool {
	for i := range s.Columns {
		if s.Columns[i].As != "" {
			return aliasReferencedSlow(cols, s)
		}
	}
	return false
}

// aliasReferencedSlow builds the alias map and answers the reference check.
func aliasReferencedSlow(cols map[string]bool, s *sql.SelectStmt) bool {
	aliases := selectAliasMap(s)
	if len(aliases) == 0 {
		return false
	}
	for name := range cols {
		if _, isAlias := aliases[name]; isAlias {
			return true
		}
	}
	return false
}

// affinityCollector accumulates column names that need affinity wrappers.
type affinityCollector struct {
	cols    map[string]bool
	seen    bool           // true once any column was collected
	visitFn func(sql.Expr) // the once-materialized method value (visitor)
}

// skipBareSelectRef reports whether a SELECT output column's affinity
// collection can be skipped: the expression is a bare, unqualified, non-star,
// non-keyword column reference resolving (case-insensitively) to a column
// with no non-BINARY declared collation. Only that shape's wrapper never
// reaches a comparison — every output builder peels the wrappers off its slot
// value — so collecting it costs a per-row wrapper allocation for nothing.
// All other expressions keep the historical collect-everything behavior.
func skipBareSelectRef(expr sql.Expr, colDefs []sql.ColumnDef) bool {
	ref, ok := unwrapParenExpr(expr).(*sql.ColumnRef)
	if !ok || ref.Table != "" || groupByKeywordName(ref.Name) {
		return false
	}
	if ref.Name == "*" {
		// A star keeps its historical collection: the collector's seen flag
		// drives appendScanStarValues's output unwrap (the IPK rowid-alias
		// fill leaves a wrapper in the star's slots).
		return false
	}
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, ref.Name) {
			coll := colDefs[i].Collate
			return coll == "" || strings.EqualFold(coll, "BINARY")
		}
	}
	return false // unresolved name: keep the historical wrapper
}

// collectExpr collects column references from a single expression,
// descending into subquery SELECT bodies (their WHERE and output columns)
// so outer scans wrap the columns a correlated subquery references. This
// mirrors the original collectExprRefs helper the engine used before the
// query extraction. The visitor closure is materialized once per collector
// (visitNodeCached), not once per clause.
func (a *affinityCollector) collectExpr(expr sql.Expr) {
	if expr == nil {
		return
	}
	WalkExprFull(expr, a.visitor())
}

// visitor materializes the per-node collector as a method value exactly once
// per collector instance.
func (a *affinityCollector) visitor() func(sql.Expr) {
	if a.visitFn == nil {
		a.visitFn = a.visitNode
	}
	return a.visitFn
}

// visitNode is the per-node collector. It is installed as a single method
// value per collector (one allocation) and shared across every clause walk
// the statement runs.
func (a *affinityCollector) visitNode(e sql.Expr) {
	if cr, ok := e.(*sql.ColumnRef); ok {
		a.cols[cr.Name] = true
		a.seen = true
	}
	a.collectSubqueryCols(e)
}

// collectSubqueryCols descends into subquery and EXISTS bodies, collecting
// column references from their WHERE and output columns.
func (a *affinityCollector) collectSubqueryCols(e sql.Expr) {
	if sub, ok := e.(*sql.Subquery); ok && sub.Select != nil {
		a.collectSelectBodyCols(sub.Select)
	}
	if ex, ok := e.(*sql.ExistsExpr); ok && ex.Select != nil {
		a.collectSelectBodyCols(ex.Select)
	}
}

// collectSelectBodyCols collects affinity columns from a subquery's WHERE
// and result columns.
func (a *affinityCollector) collectSelectBodyCols(sel *sql.SelectStmt) {
	if sel.Where != nil {
		a.collectExpr(sel.Where)
	}
	for _, col := range sel.Columns {
		a.collectExpr(col.Expr)
	}
}

// collectExprRefs collects column references from one or more expressions.
func (a *affinityCollector) collectExprRefs(expr sql.Expr) {
	if expr != nil {
		a.collectExpr(expr)
	}
}

// add marks a column name as needing affinity.
func (a *affinityCollector) add(name string) {
	a.cols[name] = true
	a.seen = true
}

// addAll marks all names as needing affinity.
func (a *affinityCollector) addAll(names []string) {
	for _, n := range names {
		a.add(n)
	}
}

// result returns the accumulated affinity set, or nil when nothing was
// collected and needMaps is false. When needMaps is true but nothing was
// collected, all columns need affinity (maps may be used downstream).
func (a *affinityCollector) result(colDefs []sql.ColumnDef, needMaps bool) map[string]bool {
	if a.seen {
		return a.cols
	}
	if !needMaps {
		return nil
	}
	for _, cd := range colDefs {
		a.cols[cd.Name] = true
	}
	return a.cols
}

// collectJoinAffinity collects affinity-requiring columns from a join's ON,
// USING, and (for NATURAL joins) the common columns of both tables.
func (e *SelectEngine) collectJoinAffinity(a *affinityCollector, j *sql.JoinClause, fromTable string) {
	if j.On != nil {
		a.collectExpr(j.On)
	}
	for _, uc := range j.Using {
		a.add(uc)
	}
	if !isNaturalJoinType(j.JoinType) {
		return
	}
	// NATURAL joins compare all common columns; mark the join table's columns
	// and, conservatively, the base FROM table's columns with the same names.
	if names, err := e.tableColumnNames(j.Table.Name); err == nil {
		a.addAll(names)
	}
	if fromTable != "" {
		if names, err := e.tableColumnNames(fromTable); err == nil {
			a.addAll(names)
		}
	}
}

// fastEvalComparison attempts to evaluate a simple BinaryOp comparison
// (ColumnRef OP Literal or Literal OP ColumnRef) without going through the
// full evalExpr → evalComplexExpr → evalBinaryOp chain. Returns (result, true)
// if the fast path was taken, or (false, false) to fall through to the slow path.
