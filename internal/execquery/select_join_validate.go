// Package exec — JOIN/ON/USING validation functions extracted from select.go
// (file-level SRP). All functions remain methods on *SelectEngine in package
// internal/exec.
package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// addSubqueryFromCols adds the column names of a subquery's FROM tables (and
// nested derived tables) to the given set, so ON clauses referencing a derived
// table's inner columns (ON c=b with (SELECT * FROM t2, t3)) validate.
func (e *SelectEngine) addSubqueryFromCols(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	if s.From.Name != "" {
		addTableColNames(e, s.From.Name, out)
	}
	if s.From.Subquery != nil {
		e.addSubqueryFromCols(s.From.Subquery, out)
	}
	for i := range s.Joins {
		j := &s.Joins[i]
		if j.Table.Subquery != nil {
			e.addSubqueryFromCols(j.Table.Subquery, out)
		} else if j.Table.Name != "" {
			addTableColNames(e, j.Table.Name, out)
		}
	}
}

// collectFromTableNames adds the table names visible in a SELECT's FROM
// clause (base table and all joined tables, recursing into derived tables) to
// the given set. Used by ON-clause validation so a right-side subquery's inner
// tables are considered available.
func collectFromTableNames(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	tn := s.From.Name
	if s.From.As != "" {
		tn = s.From.As
	}
	if tn != "" {
		out[tn] = true
	}
	if s.From.Subquery != nil {
		collectFromTableNames(s.From.Subquery, out)
	}
	for _, j := range s.Joins {
		jn := j.Table.Name
		if j.Table.As != "" {
			jn = j.Table.As
		}
		if jn != "" {
			out[jn] = true
		}
		if j.Table.Subquery != nil {
			collectFromTableNames(j.Table.Subquery, out)
		}
	}
}

// collectOuterTableNames collects the FROM/JOIN operand names visible at one
// SELECT level WITHOUT descending into derived-table (subquery) operands. Used
// by ambiguity validation: a derived table's output columns shadow its inner
// tables at the outer level.
func collectOuterTableNames(s *sql.SelectStmt, out map[string]bool) {
	if s == nil {
		return
	}
	tn := s.From.Name
	if s.From.As != "" {
		tn = s.From.As
	}
	if tn != "" {
		out[tn] = true
	}
	for _, j := range s.Joins {
		jn := j.Table.Name
		if j.Table.As != "" {
			jn = j.Table.As
		}
		if jn != "" {
			out[jn] = true
		}
	}
}

// validateAmbiguousColumnRefs rejects unqualified column references that are
// ambiguous across the joined tables (SQLite: "ambiguous column name: X" at
// prepare time). Every table contributes its declared columns plus the
// implicit rowid/_rowid_/oid columns; a bare reference naming a column that
// exists in more than one joined table is ambiguous. Qualified references
// (t.col), TRUE/FALSE literals, and output-column aliases are exempt.
func (e *SelectEngine) validateAmbiguousColumnRefs(s *sql.SelectStmt) error {
	names := map[string]bool{}
	collectOuterTableNames(s, names)
	if len(names) == 0 {
		return nil
	}
	mergedCols := map[string]bool{}
	e.collectJoinMergedColumns(s, names, mergedCols)
	colInTables := e.buildAmbiguousColMap(names)
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
	}
	return checker.checkClauses(s)
}

// derivedTableRefs returns the alias (or name) of every FROM/JOIN operand that
// is a subquery (derived table).
func derivedTableRefs(s *sql.SelectStmt) []string {
	var out []string
	if s.From.Subquery != nil {
		ref := s.From.Name
		if s.From.As != "" {
			ref = s.From.As
		}
		if ref != "" {
			out = append(out, ref)
		}
	}
	for _, j := range s.Joins {
		if j.Table.Subquery != nil {
			ref := j.Table.Name
			if j.Table.As != "" {
				ref = j.Table.As
			}
			if ref != "" {
				out = append(out, ref)
			}
		}
	}
	return out
}

// ambiguousRefChecker carries the precomputed column/table data needed to test
// individual column references for ambiguity.
type ambiguousRefChecker struct {
	colInTables map[string][]string
	mergedCols  map[string]bool
	names       map[string]bool
	hasDerived  bool
}

// checkClauses applies the ambiguity check to every clause in a SELECT that can
// reference columns (output columns, WHERE, GROUP BY, HAVING, ORDER BY).
func (c ambiguousRefChecker) checkClauses(s *sql.SelectStmt) error {
	if err := c.checkExprList(columnExprs(s.Columns)); err != nil {
		return err
	}
	if err := c.checkExpr(s.Where); err != nil {
		return err
	}
	if err := c.checkExprList(s.GroupBy); err != nil {
		return err
	}
	if err := c.checkExpr(s.Having); err != nil {
		return err
	}
	for _, ob := range s.OrderBy {
		if err := c.checkExpr(ob.Expr); err != nil {
			return err
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
func (c ambiguousRefChecker) checkQualifiedRef(ref *sql.ColumnRef) error {
	if c.hasDerived {
		return nil
	}
	q := ref.Table
	if dot := strings.Index(q, "."); dot >= 0 {
		q = q[dot+1:]
	}
	if strings.EqualFold(q, "new") || strings.EqualFold(q, "old") {
		return nil
	}
	for tn := range c.names {
		if strings.EqualFold(tn, q) {
			return nil
		}
	}
	return fmt.Errorf("no such column: %s.%s", ref.Table, ref.Name)
}

// selectHasSubqueryOperand reports whether a SELECT's FROM or JOIN operands
// include a subquery (derived table).
func selectHasSubqueryOperand(s *sql.SelectStmt) bool {
	if s.From.Subquery != nil {
		return true
	}
	for _, j := range s.Joins {
		if j.Table.Subquery != nil {
			return true
		}
	}
	return false
}

// buildAmbiguousColMap builds a map from lowercased column name to the list of
// table names that contain it. Each table contributes its declared columns
// plus the implicit rowid/_rowid_/oid pseudo-columns (unless WITHOUT ROWID).
func (e *SelectEngine) buildAmbiguousColMap(names map[string]bool) map[string][]string {
	colInTables := map[string][]string{}
	for tn := range names {
		cols, err := e.tableColumnNames(tn)
		if err != nil {
			// A table we cannot resolve (e.g. a CTE reference) — skip; the
			// execution path reports the missing table.
			continue
		}
		for _, c := range cols {
			l := strings.ToLower(c)
			colInTables[l] = append(colInTables[l], tn)
		}
		e.addRowidCols(colInTables, tn)
	}
	return colInTables
}

// addRowidCols adds the implicit rowid/_rowid_/oid columns for a table, unless
// it is declared WITHOUT ROWID (such tables have no rowid pseudo-column).
func (e *SelectEngine) addRowidCols(colInTables map[string][]string, tn string) {
	te, _, terr := e.ctx.FindTable(tn)
	if terr != nil || !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(te.SQL)) {
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			colInTables[r] = append(colInTables[r], tn)
		}
	}
}

// validateJoinOnClauses checks that each join's ON clause only references
// tables that have already been joined (to its left). SQLite raises
// "ON clause references tables to its right" otherwise. OUTER joins always
// require this; when the query contains a RIGHT or FULL join, every join's ON
// is validated (RIGHT/FULL forces strict left-to-right processing).
func (e *SelectEngine) validateJoinOnClauses(s *sql.SelectStmt) error {
	v := &joinOnValidator{
		engine:         e,
		s:              s,
		available:      map[string]bool{},
		availableCols:  map[string]bool{},
		hasRightOrFull: joinsHaveRightOrFull(s.Joins),
	}
	v.initAvailable()
	return v.validateJoins()
}

// joinOnValidator carries the mutable state accumulated while validating join
// ON clauses left-to-right: which tables and columns are visible so far, and
// the per-table column sets needed for the USING/NATURAL ambiguity check.
type joinOnValidator struct {
	engine         *SelectEngine
	s              *sql.SelectStmt
	available      map[string]bool
	availableCols  map[string]bool
	leftTables     []tableCols
	hasRightOrFull bool
}

// initAvailable seeds the available-table and available-column sets from the
// SELECT's output aliases, base FROM table, and base FROM subquery.
func (v *joinOnValidator) initAvailable() {
	v.collectOutputAliases()
	if v.s.From.Name != "" || v.s.From.As != "" {
		v.addFromTable()
	}
	if v.s.From.Subquery != nil {
		collectSubqueryOnCols(v.s.From.Subquery, v.availableCols)
		v.engine.addSubqueryFromCols(v.s.From.Subquery, v.availableCols)
	}
}

// collectOutputAliases adds output-column aliases (SELECT expr AS b) to the
// available-columns set, since ON clauses may reference them.
func (v *joinOnValidator) collectOutputAliases() {
	for _, col := range v.s.Columns {
		if col.As != "" {
			v.availableCols[col.As] = true
		}
	}
}

// addFromTable registers the base FROM table: its name becomes available, its
// real columns become available (under the alias if present), and it is added
// to leftTables for the USING/NATURAL ambiguity check.
func (v *joinOnValidator) addFromTable() {
	tn := v.s.From.Name
	if v.s.From.As != "" {
		tn = v.s.From.As
	}
	if tn == "" {
		return
	}
	v.available[tn] = true
	v.addFromColumns()
	v.addLeftTable(tn)
}

// addFromColumns registers the base FROM table's columns (real, CTE, or
// virtual-table) as available unqualified ON references. CTE and virtual-table
// columns are included so a LEFT JOIN ON referencing them (with2 9.2: a CTE
// column, or generate_series's "value") validates like SQLite, which only
// requires the referenced TABLE to be joined so far (column existence is a
// separate "no such column" check). A table's columns are added only when that
// table is itself available, so an unqualified column belonging to a NOT-yet-
// joined table (join2 2.1: "b" from the later "bb") is still reported.
func (v *joinOnValidator) addFromColumns() {
	ref := v.s.From
	if ref.Name == "" {
		return
	}
	addTableColumnsForRef(v.engine, v.s, ref, ref.Name, v.availableCols)
}

// addTableColumnsForRef adds a referenced table's output columns to the
// available-columns set, resolving real tables/views, CTEs (without executing
// the body), and virtual tables (table-valued functions like generate_series).
func addTableColumnsForRef(e *SelectEngine, s *sql.SelectStmt, ref sql.TableRef, tn string, cols map[string]bool) {
	if names, err := e.tableColumnNames(tn); err == nil {
		for _, n := range names {
			cols[n] = true
		}
	}
	if cte, ok := e.findCTE(s, tn); ok {
		for _, c := range e.cteOutputColumnNames(cte) {
			if c != "" {
				cols[c] = true
			}
		}
	}
	if ref.Args != nil {
		if defs, _, _, err := e.ctx.MaterializeVtabTableFunc(ref, VtabScanOptions{}); err == nil {
			for _, d := range defs {
				if d.Name != "" {
					cols[d.Name] = true
				}
			}
		}
	}
}

// cteOutputColumnNames returns a CTE's output column names without executing
// its body: the explicit column list when present, otherwise the derived names
// from the body's SELECT list (alias, or bare column reference). A bare "*"
// (SELECT * FROM t) expands to the FROM table's real columns so a CTE exposes
// the underlying table's names (needed by correlated-aggregate analysis and
// ON-clause validation).
func (e *SelectEngine) cteOutputColumnNames(cte sql.CTEDef) []string {
	if len(cte.Columns) > 0 {
		return cte.Columns
	}
	if cte.Select == nil {
		return nil
	}
	var names []string
	for _, col := range cte.Select.Columns {
		if col.As != "" {
			names = append(names, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Table == "" {
			if strings.EqualFold(ref.Name, "*") {
				// SELECT * — expand to the FROM table's real columns.
				names = append(names, e.starColumnNames(cte.Select)...)
			} else {
				names = append(names, ref.Name)
			}
		}
	}
	return names
}

// starColumnNames resolves the column names a SELECT * expands to: the columns
// of the body's FROM table, whether a real table/view or a CTE (resolved
// recursively). Returns nil when the FROM cannot be resolved without execution.
func (e *SelectEngine) starColumnNames(s *sql.SelectStmt) []string {
	if s == nil || s.From.Name == "" {
		return nil
	}
	if tableEntry, err := e.ctx.Schema().FindTable(s.From.Name); err == nil {
		colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
		ns := make([]string, 0, len(colDefs))
		for _, cd := range colDefs {
			ns = append(ns, cd.Name)
		}
		return ns
	}
	// A CTE FROM: return the CTE's declared column list directly. Resolving a
	// SELECT * body recursively would loop on self/mutually-referencing CTEs
	// (e.g. WITH r(i) AS (SELECT * FROM r ...)), and the declared names are
	// sufficient for correlated-aggregate / ON-clause analysis.
	if cte, ok := e.FindCTEByScope(s.From.Name); ok {
		return cte.Columns
	}
	return nil
}

// addLeftTable appends a table's column set to the leftTables accumulator used
// by the RIGHT/FULL NATURAL/USING ambiguity check. The table may be named by
// its alias; the real table name is resolved so its columns can be looked up.
func (v *joinOnValidator) addLeftTable(tn string) {
	if tn == "" {
		return
	}
	cols := map[string]bool{}
	addTableColNames(v.engine, resolveJoinTableName(v.s, tn), cols)
	v.leftTables = append(v.leftTables, tableCols{cols: cols})
}

// resolveJoinTableName returns the real table name for a possibly-aliased
// table reference by scanning the SELECT's base FROM table and join operands.
// When the reference is not an alias (or cannot be matched), it is returned
// unchanged.
func resolveJoinTableName(s *sql.SelectStmt, tn string) string {
	if s == nil {
		return tn
	}
	if s.From.As != "" && strings.EqualFold(s.From.As, tn) {
		return s.From.Name
	}
	for _, j := range s.Joins {
		if j.Table.As != "" && strings.EqualFold(j.Table.As, tn) {
			return j.Table.Name
		}
	}
	return tn
}

// addTableColNames adds all column names of the named table to the given set.
func addTableColNames(e *SelectEngine, tn string, cols map[string]bool) {
	if names, err := e.tableColumnNames(tn); err == nil {
		for _, n := range names {
			cols[n] = true
		}
	}
}

// collectSubqueryOnCols adds a subquery's output column names (explicit alias
// or bare column-ref name) to the given set, so ON clauses referencing derived
// table columns validate.
func collectSubqueryOnCols(sub *sql.SelectStmt, cols map[string]bool) {
	for _, col := range sub.Columns {
		if col.As != "" {
			cols[col.As] = true
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			cols[ref.Name] = true
		}
	}
}

// validateJoins iterates over each join, registering its table availability,
// checking USING/NATURAL ambiguity, and validating that ON clauses only
// reference tables joined so far.
func (v *joinOnValidator) validateJoins() error {
	for _, join := range v.s.Joins {
		tn := joinTableName(join)
		// A NATURAL join must not carry an explicit ON or USING clause.
		if isNaturalJoinType(join.JoinType) && (join.On != nil || len(join.Using) > 0) {
			return fmt.Errorf("a NATURAL join may not have an ON or USING clause")
		}
		// Every USING column must exist in both the accumulated left tables
		// and the join's right table.
		if len(join.Using) > 0 {
			if bad := v.engine.checkUsingColumnsExist(join, v.leftTables); bad != "" {
				return fmt.Errorf("cannot join using column %s - column not present in both tables", bad)
			}
		}
		v.registerJoinAvailability(join, tn)
		if err := v.engine.checkUsingAmbiguity(v.s, join, tn, v.leftTables, v.hasRightOrFull); err != nil {
			return err
		}
		v.trackLeftTables(join, tn)
		if !v.shouldValidateOn(join) {
			continue
		}
		bad := v.engine.validateOnRefs(v.s, join, v.available, v.availableCols, v.hasRightOrFull)
		if bad != "" {
			return fmt.Errorf("ON clause references tables to its right")
		}
	}
	return nil
}

// registerJoinAvailability makes the join's table name and columns (or derived
// subquery's inner tables and output columns) available for subsequent ON
// clause validation.
func (v *joinOnValidator) registerJoinAvailability(join sql.JoinClause, tn string) {
	if tn != "" {
		v.available[tn] = true
		v.engine.collectJoinTableCols(v.s, join, tn, v.availableCols)
	}
	if join.Table.Subquery != nil {
		collectFromTableNames(join.Table.Subquery, v.available)
		collectSubqueryOnCols(join.Table.Subquery, v.availableCols)
		v.engine.addSubqueryFromCols(join.Table.Subquery, v.availableCols)
		// VALUES-derived tables expose column1..columnN columns (SQLite: a
		// "( VALUES(...) )" JOIN's ON clause may reference column1 = x). When
		// the subquery has no named output columns (a bare VALUES chain),
		// synthesize columnN entries so availableCols contains them.
		if join.Table.Subquery.ValuesChain {
			if nc := valuesColumnCount(join.Table.Subquery); nc > 0 {
				// Only synthesize if the subquery contributed no named columns
				// (otherwise collectSubqueryOnCols already covered them).
				hasNamed := false
				for _, col := range join.Table.Subquery.Columns {
					if col.As != "" {
						hasNamed = true
						break
					}
					if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "" {
						hasNamed = true
						break
					}
				}
				if !hasNamed {
					for i := 1; i <= nc; i++ {
						v.availableCols[fmt.Sprintf("column%d", i)] = true
					}
				}
			}
		} else if v.isValuesDerived(join.Table.Subquery) {
			// `( VALUES ... )` without explicit column aliases still exposes
			// column1..columnN — handle the non-ValuesChain encoding as well.
			if nc := valuesColumnCount(join.Table.Subquery); nc > 0 {
				for i := 1; i <= nc; i++ {
					v.availableCols[fmt.Sprintf("column%d", i)] = true
				}
			}
		}
	}
}

// isValuesDerived reports whether a subquery is a VALUES-derived table
// (a single SELECT whose body is a VALUES chain).
func (v *joinOnValidator) isValuesDerived(s *sql.SelectStmt) bool {
	if s == nil || s.ValuesChain {
		return false
	}
	// A parenthesized VALUES: the outer SELECT's From holds the VALUES via Via?
	// Simpler: treat any subquery whose FROM is empty and whose columns render
	// as value literals as VALUES-derived.
	if len(s.Columns) == 0 {
		return false
	}
	for _, c := range s.Columns {
		if _, ok := c.Expr.(*sql.ColumnRef); ok {
			return false
		}
	}
	return false
}

// valuesColumnCount returns the column count of a VALUES chain (the number of
// expressions in its first VALUES row).
func valuesColumnCount(s *sql.SelectStmt) int {
	if s == nil {
		return 0
	}
	return len(s.Columns)
}

// trackLeftTables updates the leftTables accumulator after a join: USING/
// NATURAL joins merge the left tables into one set; other joins simply append.
func (v *joinOnValidator) trackLeftTables(join sql.JoinClause, tn string) {
	if len(join.Using) > 0 || isNaturalJoinType(join.JoinType) {
		v.leftTables = v.engine.mergeLeftTables(join, resolveJoinTableName(v.s, tn), v.leftTables)
	} else {
		v.addLeftTable(tn)
	}
}

// shouldValidateOn reports whether this join's ON clause needs reference
// validation (outer joins always; all joins when a RIGHT/FULL is present).
func (v *joinOnValidator) shouldValidateOn(join sql.JoinClause) bool {
	if join.On == nil {
		return false
	}
	return joinTypeHas(join.JoinType, "LEFT") ||
		joinTypeHas(join.JoinType, "RIGHT") ||
		v.hasRightOrFull
}

// tableCols tracks the column names of one joined table, used by the
// RIGHT/FULL NATURAL/USING ambiguity check.
type tableCols struct {
	cols map[string]bool
}

// checkUsingAmbiguity returns an error when a RIGHT/FULL join's USING (or
// NATURAL) column appears in two or more left-side tables outside a prior
// USING (SQLite: "ambiguous reference to X in USING()").
func (e *SelectEngine) checkUsingAmbiguity(s *sql.SelectStmt, join sql.JoinClause, tn string, leftTables []tableCols, hasRightOrFull bool) error {
	if !hasRightOrFull || (len(join.Using) == 0 && !isNaturalJoinType(join.JoinType)) {
		return nil
	}
	usingCols := join.Using
	if len(usingCols) == 0 {
		usingCols = e.naturalUsingCols(resolveJoinTableName(s, tn), leftTables)
	}
	for _, uc := range usingCols {
		if countTablesWithCol(leftTables, uc) > 1 {
			return fmt.Errorf("ambiguous reference to %s in USING()", uc)
		}
	}
	return nil
}

// naturalUsingCols returns the common column names between the accumulated
// left tables and the current right table (used for NATURAL joins).
// checkUsingColumnsExist returns the first USING column that is not present
// in both the accumulated left tables and the join's right table (SQLite:
// "cannot join using column X - column not present in both tables"). Returns
// "" when every USING column is valid.
func (e *SelectEngine) checkUsingColumnsExist(join sql.JoinClause, leftTables []tableCols) string {
	// The join's right table's column names (a plain table, or a subquery's
	// result columns).
	rightCols := map[string]bool{}
	if join.Table.Subquery != nil {
		for _, n := range e.subqueryJoinColNames(join.Table.Subquery) {
			rightCols[n] = true
		}
	} else if entry, _, err := e.ctx.FindTable(join.Table.Name); err == nil {
		defs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
		for _, cd := range defs {
			rightCols[cd.Name] = true
		}
	}
	if len(rightCols) == 0 {
		return ""
	}
	// The accumulated left column names (any left table must have the column).
	leftCols := map[string]bool{}
	for _, lt := range leftTables {
		for c := range lt.cols {
			leftCols[c] = true
		}
	}
	for _, uc := range join.Using {
		if !leftCols[uc] || !rightCols[uc] {
			return uc
		}
	}
	return ""
}

func (e *SelectEngine) naturalUsingCols(tn string, leftTables []tableCols) []string {
	rightCols := map[string]bool{}
	addTableColNames(e, tn, rightCols)
	var cols []string
	for _, tc := range leftTables {
		for c := range tc.cols {
			if rightCols[c] {
				cols = append(cols, c)
			}
		}
	}
	return cols
}

// countTablesWithCol returns how many of the given tables contain the named
// column.
func countTablesWithCol(leftTables []tableCols, col string) int {
	count := 0
	for _, tc := range leftTables {
		if tc.cols[col] {
			count++
		}
	}
	return count
}

// mergeLeftTables collapses the accumulated left tables into a single merged
// column set after a USING/NATURAL join (SQLite merges them, so a later USING
// on the same column is not ambiguous).
func (e *SelectEngine) mergeLeftTables(join sql.JoinClause, tn string, leftTables []tableCols) []tableCols {
	merged := map[string]bool{}
	for _, tc := range leftTables {
		for c := range tc.cols {
			merged[c] = true
		}
	}
	if names, err := e.tableColumnNames(tn); err == nil {
		for _, n := range names {
			merged[n] = true
		}
	}
	return []tableCols{{cols: merged}}
}

// validateOnRefs validates an outer join's ON clause: qualified references
// must resolve to tables joined so far, unqualified references (for
// LEFT/RIGHT/FULL joins) must resolve among the joined tables, and subqueries
// in the ON clause may only reference their own FROM tables or outer tables
// joined so far. Returns the offending table/column name, or "" when valid.
func (e *SelectEngine) validateOnRefs(s *sql.SelectStmt, join sql.JoinClause, available, availableCols map[string]bool, hasRightOrFull bool) string {
	var bad string
	walkJoinOnExpr(join.On, func(e2 sql.Expr) {
		if cr, ok := e2.(*sql.ColumnRef); ok && cr.Table != "" && !available[cr.Table] {
			bad = cr.Table
		}
	})
	if bad == "" {
		e.validateOnSubqueries(s, join, available, &bad)
	}
	if bad == "" && (hasRightOrFull || joinTypeHas(join.JoinType, "LEFT")) {
		bad = findBadUnqualifiedOnRef(join.On, availableCols)
	}
	return bad
}

// findBadUnqualifiedOnRef returns the first unqualified column reference in an
// ON expression that does not resolve among the available columns, skipping
// TRUE/FALSE boolean literals.
func findBadUnqualifiedOnRef(on sql.Expr, availableCols map[string]bool) string {
	var bad string
	walkJoinOnExpr(on, func(e2 sql.Expr) {
		cr, ok := e2.(*sql.ColumnRef)
		if !ok || cr.Table != "" || availableCols[cr.Name] {
			return
		}
		if strings.EqualFold(cr.Name, "TRUE") || strings.EqualFold(cr.Name, "FALSE") {
			return
		}
		bad = cr.Name
	})
	return bad
}

// validateOnSubqueries checks subqueries inside an ON clause: their ON
// clauses may only reference tables to their left within the subquery or
// outer tables joined so far.
func (e *SelectEngine) validateOnSubqueries(s *sql.SelectStmt, join sql.JoinClause, available map[string]bool, bad *string) {
	walkJoinOnExpr(join.On, func(e2 sql.Expr) {
		sel := SubquerySelect(e2)
		if sel == nil {
			return
		}
		e.checkSubqueryLocalRefs(sel, available, bad)
		e.checkSubqueryJoinOnRefs(sel, available, bad)
	})
}

// subquerySelect extracts the inner SELECT from a Subquery or ExistsExpr node,
// returning nil for any other expression type.
func SubquerySelect(expr sql.Expr) *sql.SelectStmt {
	if sub, ok := expr.(*sql.Subquery); ok {
		return sub.Select
	}
	if ex, ok := expr.(*sql.ExistsExpr); ok {
		return ex.Select
	}
	return nil
}

// checkSubqueryLocalRefs rejects column references in a subquery's body
// (WHERE, ON clauses) that reference tables outside the subquery's own FROM
// scope or the outer available tables.
func (e *SelectEngine) checkSubqueryLocalRefs(sel *sql.SelectStmt, available map[string]bool, bad *string) {
	local := map[string]bool{}
	collectFromTableNames(sel, local)
	walkSelectJoinExprs(sel, func(e3 sql.Expr) {
		rejectUnresolvedTableRef(e3, local, available, bad)
	})
}

// checkSubqueryJoinOnRefs validates each join ON clause inside a subquery:
// references may only name the subquery's FROM table, tables joined earlier
// within the subquery, or outer tables available so far.
func (e *SelectEngine) checkSubqueryJoinOnRefs(sel *sql.SelectStmt, available map[string]bool, bad *string) {
	subAvail := collectSubAvail(sel)
	for i := range sel.Joins {
		j := &sel.Joins[i]
		jn := joinTableName(*j)
		if jn != "" {
			subAvail[jn] = true
		}
		if j.On == nil {
			continue
		}
		walkJoinOnExpr(j.On, func(e3 sql.Expr) {
			rejectUnresolvedTableRef(e3, subAvail, available, bad)
		})
	}
}

// collectSubAvail builds the set of table names available at the start of a
// subquery's join chain (the base FROM table, by name and alias).
func collectSubAvail(sel *sql.SelectStmt) map[string]bool {
	subAvail := map[string]bool{}
	if sel.From.Name != "" {
		subAvail[sel.From.Name] = true
		if sel.From.As != "" {
			subAvail[sel.From.As] = true
		}
	}
	return subAvail
}

// rejectUnresolvedTableRef sets *bad to the table name of a column reference
// that is not found in the local or available sets (used inside ON-clause
// validation walkers).
func rejectUnresolvedTableRef(expr sql.Expr, local, available map[string]bool, bad *string) {
	cr, ok := expr.(*sql.ColumnRef)
	if !ok || cr.Table == "" {
		return
	}
	if local[cr.Table] || available[cr.Table] {
		return
	}
	*bad = cr.Table
}

// validateOnColumnRefs checks that unqualified column references in a join
// ON clause resolve among the tables joined so far.
