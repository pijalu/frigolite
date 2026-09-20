package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Ambiguity validation for JOIN operand instances, added/split for the
// FULL-SUITE-DRIFT T26-select tranche (1000-line file-size gate).

// fromOperand is one FROM/JOIN operand instance: ref is the name usable in
// SQL to address it (the alias when present, else the table name), table is
// the underlying real table. Two instances of the SAME table (select1-6.8:
// "FROM test1 as A, test1 as B") are distinct operands, so each contributes
// its columns separately to the ambiguity map.
type fromOperand struct {
	ref   string
	table string
}

// collectFromOperands lists the visible operand instances of a SELECT
// (outermost level only, no derived-table descent).
func collectFromOperands(s *sql.SelectStmt) []fromOperand {
	if s == nil {
		return nil
	}
	var out []fromOperand
	add := func(name, as string) {
		if name == "" && as == "" {
			return
		}
		ref := as
		if ref == "" {
			ref = name
		}
		out = append(out, fromOperand{ref: ref, table: name})
	}
	add(s.From.Name, s.From.As)
	for _, j := range s.Joins {
		add(j.Table.Name, j.Table.As)
	}
	return out
}

// validateAmbiguousColumnRefs rejects unqualified column references that are
// ambiguous across the joined operands (SQLite: "ambiguous column name: X"
// at prepare time). Every operand instance contributes its declared columns
// plus the implicit rowid/_rowid_/oid columns; a bare reference naming a
// column that exists in more than one operand is ambiguous — including two
// instances of the same table under different aliases (select1-6.8/6.8b), or
// a qualified reference naming a duplicated alias (select1-6.8c: two
// operands aliased A). TRUE/FALSE literals and output-column aliases are
// exempt.
func (e *SelectEngine) validateAmbiguousColumnRefs(s *sql.SelectStmt) error {
	names := map[string]bool{}
	collectOuterTableNames(s, names)
	if len(names) == 0 {
		return nil
	}
	mergedCols := map[string]bool{}
	e.collectJoinMergedColumns(s, names, mergedCols)
	colInTables := e.buildAmbiguousColMap(collectFromOperands(s))
	// Derived-table operands (subquery FROM/JOIN) contribute an implicit
	// rowid/_rowid_/oid to the ambiguity map even though they have no real
	// table to resolve — a bare rowid over two derived tables is ambiguous
	// (misc8-3.0: "ambiguous column name: rowid").
	for _, ref := range derivedTableRefs(s) {
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			colInTables[r] = append(colInTables[r], ref)
		}
	}
	checker := ambiguousRefChecker{
		colInTables: colInTables,
		mergedCols:  mergedCols,
		names:       names,
		hasDerived:  selectHasSubqueryOperand(s),
		inCompound:  e.inCompoundMember,
	}
	return checker.checkClauses(s)
}

// ambiguousRefChecker carries the precomputed column/table data needed to test
// individual column references for ambiguity.
type ambiguousRefChecker struct {
	colInTables map[string][]string
	mergedCols  map[string]bool
	names       map[string]bool
	hasDerived  bool
	// inCompound is true while validating a member of a compound SELECT
	// (UNION/INTERSECT/EXCEPT): its ORDER BY is the compound-level ORDER BY
	// and resolves only against result-column names.
	inCompound bool
}

// checkClauses applies the ambiguity check to every clause in a SELECT that can
// reference columns (output columns, WHERE, GROUP BY, HAVING, ORDER BY).
// GROUP BY/HAVING/ORDER BY terms naming an output-column ALIAS are exempt:
// resolve.c resolves those clauses against the output aliases FIRST, so the
// alias reference never reaches the source-column ambiguity check
// (resolver01-1.1: "SELECT 1 AS y FROM t1, t2 ORDER BY y" succeeds even
// though both source tables have a y). WHERE has no alias visibility, so it
// stays strict.
func (c ambiguousRefChecker) checkClauses(s *sql.SelectStmt) error {
	if err := c.checkExprList(columnExprs(s.Columns)); err != nil {
		return err
	}
	if err := c.checkExpr(s.Where); err != nil {
		return err
	}
	aliases := collectSelectAliases(s.Columns)
	if err := c.checkExprListOptAliases(s.GroupBy, aliases); err != nil {
		return err
	}
	if err := c.checkExprOptAliases(s.Having, aliases); err != nil {
		return err
	}
	if len(s.OrderBy) > 0 && (s.Union != nil || c.inCompound) {
		// A COMPOUND select's ORDER BY resolves ONLY against the compound's
		// result-column names (sqlite3Select: the terms never touch any
		// member's FROM scope, so source-column ambiguity cannot apply —
		// tkt3527: ElemView2's "ORDER BY ElemId, InnerCode" over a member
		// "FROM ElemView1 AS Element JOIN ElemView1 AS InnerElem").
		// Exempt bare terms naming a result column (alias or column name).
		return c.checkExprListOptAliases(orderByExprsOf(s.OrderBy),
			collectResultColumnNames(s.Columns))
	}
	for _, ob := range s.OrderBy {
		if err := c.checkExprOptAliases(ob.Expr, aliases); err != nil {
			return err
		}
	}
	return nil
}

// collectResultColumnNames names a compound's result columns: the explicit
// alias when present, else the column-reference name (a qualified ref
// contributes its unqualified name — "InnerElem.ElemCode" is "ElemCode"),
// matching sqlite3Select's compound result-column naming.
func collectResultColumnNames(columns []sql.SelectColumn) map[string]bool {
	names := make(map[string]bool)
	for _, col := range columns {
		if col.As != "" {
			names[strings.ToLower(col.As)] = true
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
			names[strings.ToLower(ref.Name)] = true
		}
	}
	return names
}

// orderByExprsOf extracts the expression list of ORDER BY terms.
func orderByExprsOf(obs []sql.OrderByTerm) []sql.Expr {
	out := make([]sql.Expr, 0, len(obs))
	for _, ob := range obs {
		out = append(out, ob.Expr)
	}
	return out
}

// checkExprListOptAliases applies the ambiguity check to a list of clause
// expressions, skipping bare references that name an output-column alias.
func (c ambiguousRefChecker) checkExprListOptAliases(exprs []sql.Expr, aliases map[string]bool) error {
	for _, expr := range exprs {
		if err := c.checkExprOptAliases(expr, aliases); err != nil {
			return err
		}
	}
	return nil
}

// checkExprOptAliases applies the ambiguity check to a clause expression. A
// term that IS an output-column alias (a bare unqualified column reference,
// possibly wrapped in COLLATE) is skipped: resolve.c resolves such a term
// against the output aliases first (resolver01-1.1/2.1). Any other operator
// over that name ("+y" — resolver01-3.1) is an expression whose operand
// resolves against the SOURCE columns, so the ambiguity check applies.
func (c ambiguousRefChecker) checkExprOptAliases(expr sql.Expr, aliases map[string]bool) error {
	if expr == nil {
		return nil
	}
	if ref := aliasTermColumnRef(expr); ref != nil && ref.Table == "" && aliases[strings.ToLower(ref.Name)] {
		return nil
	}
	return c.checkExpr(expr)
}

// aliasTermColumnRef returns the column reference when the expression is a
// bare column reference, looking through ParenExpr and COLLATE wrappers;
// nil for every other shape.
func aliasTermColumnRef(expr sql.Expr) *sql.ColumnRef {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return v
	case *sql.ParenExpr:
		return aliasTermColumnRef(v.Expr)
	case *sql.BinaryOp:
		// COLLATE parses as a BinaryOp wrapping the term (rule 187).
		if strings.EqualFold(v.Operator, "COLLATE") {
			return aliasTermColumnRef(v.Left)
		}
	}
	return nil
}

// columnExprs extracts the expression slice from a SELECT's output columns.
func columnExprs(cols []sql.SelectColumn) []sql.Expr {
	var exprs []sql.Expr
	for _, col := range cols {
		exprs = append(exprs, col.Expr)
	}
	return exprs
}

// checkExprList applies the ambiguity check to a list of expressions.
func (c ambiguousRefChecker) checkExprList(exprs []sql.Expr) error {
	for _, expr := range exprs {
		if err := c.checkExpr(expr); err != nil {
			return err
		}
	}
	return nil
}

// checkExpr walks a single expression and returns the first ambiguity error.
func (c ambiguousRefChecker) checkExpr(expr sql.Expr) error {
	if expr == nil {
		return nil
	}
	var checkErr error
	WalkExprFull(expr, func(e2 sql.Expr) {
		if checkErr != nil {
			return
		}
		ref, ok := e2.(*sql.ColumnRef)
		if !ok || ref.Name == "*" {
			return
		}
		checkErr = c.checkColumnRef(ref)
	})
	return checkErr
}

// checkColumnRef tests a single column reference for ambiguity or unknown
// qualifier.
func (c ambiguousRefChecker) checkColumnRef(ref *sql.ColumnRef) error {
	if ref.Table != "" {
		return c.checkQualifiedRef(ref)
	}
	if strings.EqualFold(ref.Name, "TRUE") || strings.EqualFold(ref.Name, "FALSE") {
		return nil
	}
	l := strings.ToLower(ref.Name)
	if c.mergedCols[l] {
		return nil
	}
	if len(c.colInTables[l]) > 1 {
		return fmt.Errorf("ambiguous column name: %s", ref.Name)
	}
	return nil
}

// checkQualifiedRef validates that a qualified reference (t.col) names a
// visible table. When a derived table is present in the FROM/JOIN operands,
// the qualifier may name a table inside the derived table (resolved at
// execution), so the check is skipped. NEW/OLD trigger references are exempt.
// A schema-qualified reference (main.t4.a) matches an operand written either
// way (main.t4 or t4) — both spellings are candidates, mirroring SQLite's
// db-qualified name resolution (selectD-2.4).
// countQualifierInstances counts how many FROM operands (each counted once)
// provide col under any of the given qualifiers, and whether any match
// exists at all.
func (c ambiguousRefChecker) countQualifierInstances(col string, qualifiers []string) (instances int, found bool) {
	for tblCol, refs := range c.colInTables {
		if !strings.EqualFold(tblCol, col) {
			continue
		}
		for _, rn := range refs {
			for _, ql := range qualifiers {
				if strings.EqualFold(rn, ql) {
					found = true
					instances++
					break // count each operand instance once
				}
			}
		}
	}
	return instances, found
}

func (c ambiguousRefChecker) checkQualifiedRef(ref *sql.ColumnRef) error {
	if c.hasDerived {
		return nil
	}
	q := strings.ToLower(ref.Table)
	if q == "new" || q == "old" {
		return nil
	}
	qualifiers := []string{q}
	if dot := strings.IndexByte(q, '.'); dot >= 0 {
		qualifiers = append(qualifiers, q[dot+1:])
	}
	instances, found := c.countQualifierInstances(ref.Name, qualifiers)
	if instances > 1 {
		// A qualifier naming a DUPLICATED alias is ambiguous
		// (select1-6.8c: "FROM test1 as A, test1 as A").
		return fmt.Errorf("ambiguous column name: %s.%s", ref.Table, ref.Name)
	}
	if found {
		return nil
	}
	for tn := range c.names {
		for _, ql := range qualifiers {
			if strings.EqualFold(tn, ql) {
				return nil
			}
		}
	}
	return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
}
