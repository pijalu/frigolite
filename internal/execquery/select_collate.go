package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// Prepare-time schema-collation resolution (build.c/resolve.c parity).
//
// A schema-declared collation (CREATE TABLE t(c COLLATE name)) must resolve
// against the connection's collation registry for every statement that
// RESOLVES the collation — ORDER BY/GROUP BY sort keys, DISTINCT dedup,
// compound set-operation merge keys, and compound ORDER BY terms inheriting
// a result column's collation. SQLite resolves these at prepare time
// (sqlite3ResolveSelectNames → sqlite3ExprCollSeq → sqlite3LocateCollSeq,
// build.c) and fails with "no such collation sequence: NAME" after a
// close/reopen that did not re-register the collation — even when the table
// is empty. Statements that never need the collation (a bare SELECT *) keep
// succeeding (collate3-2.6/2.12).
//
// Explicit COLLATE operators are validated separately (validateCollateClause
// via validateSelectRowValues), as are WHERE-comparison sides
// (checkWhereCollations).

// validateSchemaCollations raises "no such collation sequence: NAME" for the
// schema-declared collations this SELECT level resolves at compile time.
func (e *SelectEngine) validateSchemaCollations(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	if len(s.OrderBy) == 0 && len(s.GroupBy) == 0 && !s.Distinct && s.Union == nil {
		return nil
	}
	resolve := e.schemaCollationResolver(s)
	if err := e.validateSortKeyCollations(s, resolve); err != nil {
		return err
	}
	return e.validateResultCollations(s)
}

// validateSortKeyCollations checks the ORDER BY and GROUP BY term
// collations. A compound member's trailing ORDER BY belongs to the enclosing
// compound statement (checked through the compound result-column collations
// in validateResultCollations), so members skip their own terms — matching
// checkOrderByAggMisuse's member handling.
func (e *SelectEngine) validateSortKeyCollations(s *sql.SelectStmt, resolve func(sql.ColumnRef) string) error {
	if e.inCompoundMember {
		return nil
	}
	for _, ob := range s.OrderBy {
		if err := e.checkTermCollation(orderTermExpr(s, ob.Expr), resolve); err != nil {
			return err
		}
	}
	for _, g := range s.GroupBy {
		if err := e.checkTermCollation(orderTermExpr(s, g), resolve); err != nil {
			return err
		}
	}
	return nil
}

// checkTermCollation resolves one ORDER BY/GROUP BY term's collation and
// verifies it against the connection's registry.
func (e *SelectEngine) checkTermCollation(expr sql.Expr, resolve func(sql.ColumnRef) string) error {
	if expr == nil {
		return nil
	}
	if name, _ := e.schemaExprCollation(expr, resolve); name != "" {
		return e.ctx.CheckCollationString(name)
	}
	return nil
}

// validateResultCollations checks the collations that drive DISTINCT dedup,
// deduplicating set-operation merges, and compound ORDER BY sort keys: the
// collation of each result column (first compound member with a defined
// collation wins).
func (e *SelectEngine) validateResultCollations(s *sql.SelectStmt) error {
	if !s.Distinct && s.Union == nil {
		return nil
	}
	colls := e.schemaOutputCollations(s)
	if s.Distinct || compoundChainHasDedup(s) {
		for _, c := range colls {
			if c == "" {
				continue
			}
			if err := e.ctx.CheckCollationString(c); err != nil {
				return err
			}
		}
	}
	return e.validateCompoundOrderByCollations(s, colls)
}

// validateCompoundOrderByCollations checks a compound query's ORDER BY
// terms. The parser attaches it to the tail member; it sorts the MERGED
// rows, so each term's effective collation is the result column's (an
// ordinal or bare result-name term), while an explicit COLLATE on the term
// was already validated by validateSelectRowValues.
func (e *SelectEngine) validateCompoundOrderByCollations(s *sql.SelectStmt, colls []string) error {
	if s.Union == nil {
		return nil
	}
	tail := s
	for tail.Union != nil {
		tail = tail.Union
	}
	for _, ob := range tail.OrderBy {
		pos := e.compoundOrderTermPosition(s, ob.Expr)
		if pos >= 1 && pos <= len(colls) && colls[pos-1] != "" {
			if err := e.ctx.CheckCollationString(colls[pos-1]); err != nil {
				return err
			}
		}
	}
	return nil
}

// schemaCollationResolver builds the declared-collation resolver for a
// SELECT's FROM scope: unqualified column names resolve against the
// concatenation of every FROM/join table's collation map; qualified names
// resolve against their own table's map (alias-qualified included). Names
// keep their schema-declared case — SQLite reports the collation name as
// declared ("no such collation sequence: string_compare").
func (e *SelectEngine) schemaCollationResolver(s *sql.SelectStmt) func(sql.ColumnRef) string {
	unqualified := make(map[string]string)
	byTable := make(map[string]map[string]string)
	add := func(name, alias string) {
		m := e.schemaTableCollations(name, byTable)
		if m == nil {
			return
		}
		for k, v := range m {
			unqualified[k] = v
		}
		if alias != "" {
			byTable[strings.ToLower(alias)] = m
		}
	}
	add(s.From.Name, s.From.As)
	for _, j := range s.Joins {
		add(j.Table.Name, j.Table.As)
	}
	return func(ref sql.ColumnRef) string {
		if ref.Table != "" {
			if m, ok := byTable[strings.ToLower(ref.Table)]; ok {
				return m[strings.ToLower(ref.Name)]
			}
			return ""
		}
		return unqualified[strings.ToLower(ref.Name)]
	}
}

// schemaTableCollations returns a table's lowercased-column → declared
// collation map, computing and caching it in cache.
func (e *SelectEngine) schemaTableCollations(name string, cache map[string]map[string]string) map[string]string {
	if name == "" {
		return nil
	}
	key := strings.ToLower(name)
	if m, ok := cache[key]; ok {
		return m
	}
	m := make(map[string]string)
	cache[key] = m
	entry, _, err := e.ctx.FindTable(name)
	if err != nil || entry == nil {
		return m
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if cd.Collate != "" {
			m[strings.ToLower(cd.Name)] = cd.Collate
		}
	}
	return m
}

// schemaOutputCollations returns the case-preserved collation of each result
// column of a (possibly compound) SELECT: the FIRST chain member whose
// expression has a defined collation wins (selectOutputCollations parity,
// keeping the schema-declared letter case for error reporting).
func (e *SelectEngine) schemaOutputCollations(s *sql.SelectStmt) []string {
	colls := make([]string, 0, len(s.Columns))
	for ci := range s.Columns {
		colls = append(colls, e.schemaColumnCollation(s, ci))
	}
	return colls
}

// schemaColumnCollation resolves the collation of result column ci by
// scanning the compound chain for the first member that defines one.
func (e *SelectEngine) schemaColumnCollation(s *sql.SelectStmt, ci int) string {
	coll := ""
	for cur := s; cur != nil && ci < len(cur.Columns); cur = cur.Union {
		resolve := e.schemaCollationResolver(cur)
		name, defined := e.schemaMemberColumnCollation(cur.Columns[ci], resolve)
		if defined {
			return name
		}
		if coll == "" {
			coll = name
		}
	}
	return coll
}

// schemaMemberColumnCollation returns one compound member's output-column
// collation and whether it is defined at all (BINARY counts as defined): an
// explicit COLLATE on the expression wins, then a bare column reference's
// declared table collation; a bare literal defines nothing.
func (e *SelectEngine) schemaMemberColumnCollation(col sql.SelectColumn, resolve func(sql.ColumnRef) string) (string, bool) {
	if name, explicit := e.schemaExprCollation(col.Expr, resolve); name != "" {
		return name, true
	} else if explicit {
		// An explicit COLLATE binary is still a defined BINARY collation.
		return "", true
	}
	ref, ok := col.Expr.(*sql.ColumnRef)
	if !ok || ref.Name == "*" || ref.Table != "" {
		return "", false
	}
	return resolve(*ref), true
}

// schemaExprCollation computes the compile-time collation of an expression
// like execexpr.ExprCollation, but resolves column references through the
// FROM tables' declared collations via resolve (SQLite expr.c
// sqlite3ExprCollSeq). The boolean reports whether the collation is
// "explicit" (propagates from a COLLATE operator), mirroring ExprCollation.
func (e *SelectEngine) schemaExprCollation(expr sql.Expr, resolve func(sql.ColumnRef) string) (string, bool) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return e.schemaBinaryCollation(v, resolve)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if c, _ := e.schemaExprCollation(arg, resolve); c != "" {
				return c, false
			}
		}
		return "", false
	case *sql.CaseExpr:
		return e.schemaCaseCollation(v, resolve)
	case *sql.UnaryOp:
		// UPLUS propagates the operand's collation (SQLite TK_UPLUS).
		return e.schemaExprCollation(v.Operand, resolve)
	case *sql.ColumnRef:
		if v.Name == "*" {
			return "", false
		}
		return resolve(*v), false
	}
	return "", false
}

// schemaBinaryCollation resolves the collation of a binary operator:
// COLLATE yields its (explicit) collation; "||" takes the right operand's
// collation when explicit, else the left's; other operators have none.
func (e *SelectEngine) schemaBinaryCollation(v *sql.BinaryOp, resolve func(sql.ColumnRef) string) (string, bool) {
	if strings.EqualFold(v.Operator, "COLLATE") {
		return collateOperandName(v.Right), true
	}
	if strings.EqualFold(v.Operator, "||") {
		if rc, rx := e.schemaExprCollation(v.Right, resolve); rx {
			return rc, true
		}
		if lc, _ := e.schemaExprCollation(v.Left, resolve); lc != "" {
			return lc, false
		}
	}
	return "", false
}

// schemaCaseCollation resolves a CASE expression's collation: the first
// THEN branch with a collation, else the ELSE branch's.
func (e *SelectEngine) schemaCaseCollation(v *sql.CaseExpr, resolve func(sql.ColumnRef) string) (string, bool) {
	for _, w := range v.Whens {
		if c, _ := e.schemaExprCollation(w.Then, resolve); c != "" {
			return c, false
		}
	}
	if v.Else != nil {
		return e.schemaExprCollation(v.Else, resolve)
	}
	return "", false
}

// collateOperandName returns the collation name operand of a COLLATE
// operator (a string literal), or "".
func collateOperandName(expr sql.Expr) string {
	if lit, ok := expr.(*sql.StringLit); ok {
		return strings.ToUpper(lit.Value)
	}
	return ""
}

// orderTermExpr resolves an ORDER BY/GROUP BY term to the expression whose
// collation SQLite resolves for it: an ordinal maps to that result column's
// expression; a bare name maps to the select-list alias' expression when one
// matches (resolve.c alias precedence); otherwise the term itself.
func orderTermExpr(s *sql.SelectStmt, expr sql.Expr) sql.Expr {
	expr = stripCollate(expr)
	if nl, ok := expr.(*sql.NumericLit); ok {
		if n, err := strconv.Atoi(nl.Value); err == nil && n >= 1 && n <= len(s.Columns) {
			return s.Columns[n-1].Expr
		}
		return nil
	}
	if ref, ok := expr.(*sql.ColumnRef); ok && ref.Table == "" {
		for _, col := range s.Columns {
			if col.As != "" && strings.EqualFold(col.As, ref.Name) {
				return col.Expr
			}
		}
	}
	return expr
}

// compoundChainHasDedup reports whether any set operation along the compound
// chain deduplicates (UNION, INTERSECT, EXCEPT — every op except UNION ALL).
// Only deduplicating merges resolve the result-column collations.
func compoundChainHasDedup(s *sql.SelectStmt) bool {
	for cur := s; cur != nil && cur.Union != nil; cur = cur.Union {
		if !(cur.SetOp == sql.SetUnion && cur.UnionAll) {
			return true
		}
	}
	return false
}

// compoundOrderTermPosition returns the 1-based result column a compound
// ORDER BY term sorts (an ordinal, or a bare name matching a result column),
// or 0 when the term sorts by its own expression.
func (e *SelectEngine) compoundOrderTermPosition(s *sql.SelectStmt, term sql.Expr) int {
	inner := stripCollate(term)
	if nl, ok := inner.(*sql.NumericLit); ok {
		if n, err := strconv.Atoi(nl.Value); err == nil && n >= 1 {
			return n
		}
		return 0
	}
	if ref, ok := inner.(*sql.ColumnRef); ok && ref.Table == "" && ref.Name != "*" {
		for i, name := range e.compoundResultColumnNames(s) {
			if strings.EqualFold(name, ref.Name) {
				return i + 1
			}
		}
	}
	return 0
}

// compoundResultColumnNames returns the head member's result column names in
// order, expanding a bare "*" through the FROM source.
func (e *SelectEngine) compoundResultColumnNames(s *sql.SelectStmt) []string {
	if names, ok := e.expandedStarColumnNames(s); ok {
		return names
	}
	names := make([]string, 0, len(s.Columns))
	for _, col := range s.Columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name != "*" {
			names = append(names, ref.Name)
			continue
		}
		if col.As != "" {
			names = append(names, col.As)
		} else {
			names = append(names, "")
		}
	}
	return names
}

// expandedStarColumnNames resolves a bare "SELECT * FROM source" column
// list through the FROM source (table, CTE, or view). ok is false when the
// head member is not a bare star.
func (e *SelectEngine) expandedStarColumnNames(s *sql.SelectStmt) ([]string, bool) {
	if len(s.Columns) != 1 {
		return nil, false
	}
	ref, ok := s.Columns[0].Expr.(*sql.ColumnRef)
	if !ok || ref.Name != "*" || s.From.Name == "" {
		return nil, false
	}
	names, err := e.resolveTableColumnNames(s, s.From.Name)
	if err != nil {
		return nil, false
	}
	return names, true
}
