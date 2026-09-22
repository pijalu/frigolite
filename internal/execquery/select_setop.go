package execquery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

func (e *SelectEngine) mergeUnionRows(rows [][]interface{}, union *sql.SelectStmt, op sql.SetOp, unionAll bool, colls []string) [][]interface{} {
	unionResult := e.execSelect(union)
	if unionResult.Error != nil {
		return rows
	}
	return e.applySetOp(rows, unionResult.Rows, op, unionAll, colls)
}

// applySetOp combines left and right row sets with a compound set operator.
// colls holds the collation of each result column (from the leftmost member
// of the compound query), used to deduplicate/intersect with SQLite's column
// collation semantics; nil means BINARY for all columns.
func (e *SelectEngine) applySetOp(rows, rightRows [][]interface{}, op sql.SetOp, unionAll bool, colls []string) [][]interface{} {
	switch op {
	case sql.SetUnion:
		if unionAll {
			// UNION ALL: concatenate without dedup
			return append(rows, rightRows...)
		}
		// UNION: deduplicate combined rows; SQLite's temp b-tree emits
		// the unique rows in sorted key order.
		return e.sortSetOpRows(e.dedupeRows(append(rows, rightRows...), colls), colls)
	case sql.SetIntersect:
		// INTERSECT: rows that appear in both sets
		return e.sortSetOpRows(e.intersectRows(rows, rightRows, colls), colls)
	case sql.SetExcept:
		// EXCEPT: rows in left but not in right
		return e.sortSetOpRows(e.exceptRows(rows, rightRows, colls), colls)
	default:
		return append(rows, rightRows...)
	}
}

// sortSetOpRows sorts compound SELECT (UNION/INTERSECT/EXCEPT) output rows
// by their result values, matching SQLite's ephemeral b-tree ordering
// (NULL first, then INTEGER/REAL, then TEXT, then BLOB, with the leftmost
// result column's collation applied to text comparisons).
func (e *SelectEngine) sortSetOpRows(rows [][]interface{}, colls []string) [][]interface{} {
	if len(rows) < 2 {
		return rows
	}
	out := make([][]interface{}, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		n := len(a)
		if len(b) < n {
			n = len(b)
		}
		for k := 0; k < n; k++ {
			coll := ""
			if colls != nil && k < len(colls) {
				coll = colls[k]
			}
			cmp := e.ctx.CompareValuesCollate(util.UnwrapColumnValue(a[k]), util.UnwrapColumnValue(b[k]), coll)
			if cmp != 0 {
				return cmp < 0
			}
		}
		return false
	})
	return out
}

// compoundSelectColCount returns the declared output column count of a
// single SELECT member of a compound query, expanding "*" / "t.*" through
// the schema. It is used to validate that all members of a compound query
// have the same number of result columns (SQLite reports this error at
// prepare time, including under EXPLAIN QUERY PLAN).
// resolveTableColumnNames returns the column names for a FROM/join table
// reference, resolving CTE names before falling back to real tables/views.
// Used by compound column-count validation, which runs before the CTE scope
// is pushed onto e.cteScopes (execSelect pushes CTEs after validation).
func (e *SelectEngine) resolveTableColumnNames(s *sql.SelectStmt, name string) ([]string, error) {
	if cte, ok := e.findCTE(s, name); ok && cte.Select != nil {
		// A CTE that is currently being resolved (a nested anchor width
		// check re-entering through the same body) would report a false
		// circular reference (with2 1.11: the outer i's anchor
		// "SELECT * FROM j" resolves j, whose body references i). Use the
		// declared column list when present; otherwise fall back to the
		// body's own declared width.
		if e.resolvingCTEs[cte.Select] && len(cte.Columns) > 0 {
			return cte.Columns, nil
		}
		// The CTE's output column names come from its SELECT body (a VALUES
		// body exposes column1..columnN).
		res := e.execSelect(cte.Select)
		if res.Error != nil {
			return nil, res.Error
		}
		cols := make([]string, len(res.Columns))
		copy(cols, res.Columns)
		if len(cte.Columns) > 0 {
			for i := 0; i < len(cols) && i < len(cte.Columns); i++ {
				cols[i] = cte.Columns[i]
			}
		}
		return cols, nil
	}
	return e.tableColumnNames(name)
}

func (e *SelectEngine) compoundSelectColCount(s *sql.SelectStmt) (int, error) {
	count := 0
	for _, col := range s.Columns {
		ref, ok := col.Expr.(*sql.ColumnRef)
		if ok && ref.Name == "*" {
			// Star expansion: count the columns of the referenced table or
			// subquery. When the FROM clause joins multiple tables, count
			// every joined table's columns (SQLite expands * across all
			// tables; USING/NATURAL merges happen at execution time, and the
			// merged column still appears once from the left table).
			n, err := e.compoundStarCount(s, ref)
			if err != nil {
				return 0, err
			}
			count += n
		} else {
			count++
		}
	}
	return count, nil
}

// validateCompoundFromTables resolves each compound member's base FROM table
// right-to-left, reporting the FIRST (rightmost) resolution error — matching
// SQLite's compound name-resolution order (with3 1.0: "SELECT 5 FROM t0 UNION
// SELECT 8 FROM m" reports "no such table: m" before t0). CTE references and
// subquery FROMs are skipped (they resolve through their own paths).
func (e *SelectEngine) validateCompoundFromTables(s *sql.SelectStmt) error {
	if s == nil || s.Union == nil {
		return nil
	}
	// Collect members right-to-left.
	var members []*sql.SelectStmt
	for cur := s; cur != nil; cur = cur.Union {
		members = append(members, cur)
	}
	for i := len(members) - 1; i >= 0; i-- {
		if err := e.resolveCompoundMemberFrom(members[i]); err != nil {
			return err
		}
	}
	return nil
}

// resolveCompoundMemberFrom resolves one compound member's FROM table: CTEs,
// pragma table functions, and views are valid sources; an unresolved name
// surfaces its "no such table" error. The name may be a view (compound member
// FROM can reference a view — unionall2 1.0's vA view body "SELECT * FROM
// v1, ...").
func (e *SelectEngine) resolveCompoundMemberFrom(m *sql.SelectStmt) error {
	if m.From.Name == "" || m.From.Subquery != nil || len(m.From.Args) > 0 {
		return nil
	}
	if _, ok := e.findCTE(m, m.From.Name); ok {
		return nil
	}
	if isPragmaTableFunc(m.From.Name) {
		return nil
	}
	if _, _, err := e.ctx.FindTable(m.From.Name); err != nil {
		if _, _, verr := e.ctx.FindView(m.From.Name); verr != nil {
			return err
		}
	}
	return nil
}

// ValidateCompoundColumnCounts is the exported hook for the DML layer: the
// compound-width error fires during compound generation, ahead of per-row
// INSERT VALUES validation (select4-11.16).
func (e *SelectEngine) ValidateCompoundColumnCounts(s *sql.SelectStmt) error {
	return e.validateCompoundColumnCounts(s)
}

// validateCompoundColumnCounts checks that all members of a compound SELECT
// chain produce the same number of result columns, matching SQLite's
// "SELECTs to the left and right of <OP> do not have the same number of
// result columns" error.
func (e *SelectEngine) validateCompoundColumnCounts(s *sql.SelectStmt) error {
	if s.Union == nil {
		return nil
	}
	headCount, err := e.compoundSelectColCount(s)
	if err != nil {
		return err
	}
	cur := s
	for cur.Union != nil {
		member := cur.Union
		if member.ValuesChain {
			// VALUES members contribute one column per expression.
			for member.Union != nil && member.SetOp == sql.SetUnion && member.UnionAll {
				member = member.Union
			}
			cur = member
			continue
		}
		memberCount, err := e.compoundSelectColCount(member)
		if err != nil {
			return err
		}
		if memberCount != headCount {
			return fmt.Errorf("SELECTs to the left and right of %s do not have the same number of result columns", setOpName(cur.SetOp, cur.UnionAll))
		}
		cur = member
	}
	return nil
}

// isHiddenColumnDef reports whether a column definition is hidden (a HIDDEN
// virtual-table column or an internal __hidden__-prefixed column). Hidden
// columns are excluded from bare * expansion and PRAGMA table_info but remain
// readable by explicit column references.
func IsHiddenColumnDef(cd sql.ColumnDef) bool {
	return cd.Hidden || strings.HasPrefix(cd.Name, "__hidden__")
}

// viewSelectColumnNames returns the result column names of a view by parsing
// its stored SELECT body and deriving column names.
func (e *SelectEngine) viewSelectColumnNames(entry *schema.Entry) ([]string, error) {
	sqlStr := entry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return nil, fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	selectSQL := strings.TrimSpace(sqlStr[idx+3:])
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return nil, fmt.Errorf("exec: view parse error: %v", err)
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		// Prefer the declared column list, then typed defs (which expand
		// stars); fall back to derived names.
		if declared := ViewDeclaredColumns(entry.SQL); len(declared) > 0 {
			return declared, nil
		}
		if defs := e.ctx.ViewColumnDefsFromSelect(sel); len(defs) > 0 {
			names := make([]string, len(defs))
			for i, cd := range defs {
				names[i] = cd.Name
			}
			return names, nil
		}
		return e.ctx.ViewColumnNames(sel), nil
	}
	return nil, fmt.Errorf("exec: view does not contain SELECT")
}

// validateViewBody parses a view's stored SELECT body and runs the same
// compile-time validations a real query would (compound column counts,
// expression checks). It returns the first error found, or nil.
func (e *SelectEngine) validateViewBody(entry *schema.Entry) error {
	sqlStr := entry.SQL
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS")
	if idx < 0 {
		return fmt.Errorf("exec: invalid view SQL: %s", sqlStr)
	}
	selectSQL := strings.TrimSpace(sqlStr[idx+3:])
	stmts, err := parse.ParseSQL(selectSQL)
	if err != nil || len(stmts) == 0 {
		return fmt.Errorf("exec: view parse error: %v", err)
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		if err := e.validateCompoundColumnCounts(sel); err != nil {
			return err
		}
		if err := e.validateSelectExprs(sel); err != nil {
			return err
		}
	}
	return nil
}

// selectOutputCollations returns the collation of each output column of a
// compound SELECT: SQLite takes, per result column, the FIRST compound member
// (left-to-right) whose expression has a DEFINED collation (including BINARY;
// a bare literal has no collation and lets the search continue). A later
// member's explicit COLLATE therefore wins when earlier members are bare
// literals (e.g. `SELECT 'abc' UNION SELECT 'ABC' COLLATE nocase` is NOCASE),
// while a column declared COLLATE binary stops the search at BINARY. Returns
// "" per column when no member defines a collation.
func (e *SelectEngine) selectOutputCollations(s *sql.SelectStmt) []string {
	if s == nil {
		return nil
	}
	colls := make([]string, 0, len(s.Columns))
	for ci := range s.Columns {
		coll := ""
		for cur := s; cur != nil; cur = cur.Union {
			if ci >= len(cur.Columns) {
				break
			}
			if c, defined := e.memberColumnCollation(cur, cur.Columns[ci]); defined {
				coll = c
				break
			}
		}
		colls = append(colls, coll)
	}
	return colls
}

// memberColumnCollation returns the collation of one compound member's output
// column (BINARY counts as defined) and whether the column has a defined
// collation at all. An explicit COLLATE on the expression wins, then a column
// reference's declared table collation (its default is BINARY). A bare
// literal has no defined collation.
func (e *SelectEngine) memberColumnCollation(m *sql.SelectStmt, col sql.SelectColumn) (string, bool) {
	if exprColl, explicit := execexpr.ExprCollation(col.Expr); exprColl != "" {
		return exprColl, true
	} else if explicit && isCollateExpr(col.Expr) {
		// An explicit COLLATE binary is still a defined BINARY collation.
		return "", true
	}
	ref, ok := col.Expr.(*sql.ColumnRef)
	if !ok || ref.Name == "*" || ref.Table != "" {
		return "", false
	}
	// Resolve the column's declared collation from the FROM table (or a
	// joined table whose column matches); its absence means BINARY.
	colByName := make(map[string]string)
	if m.From.Name != "" {
		e.collectTableCollations(m.From.Name, colByName)
	}
	for _, join := range m.Joins {
		e.collectTableCollations(join.Table.Name, colByName)
	}
	if coll := colByName[strings.ToLower(ref.Name)]; coll != "" {
		return coll, true
	}
	// A real column reference always has a defined collation (BINARY by
	// default).
	return "", true
}

// isCollateExpr reports whether expr is a top-level COLLATE operator.
func isCollateExpr(e sql.Expr) bool {
	b, ok := e.(*sql.BinaryOp)
	return ok && strings.EqualFold(b.Operator, "COLLATE")
}

// collectTableCollations adds the non-BINARY declared collations of a table's
// columns to colByName (lowercased column name → uppercased collation).
func (e *SelectEngine) collectTableCollations(tableName string, colByName map[string]string) {
	if tableName == "" {
		return
	}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, cd := range colDefs {
		if cd.Collate != "" && !strings.EqualFold(cd.Collate, "BINARY") {
			colByName[strings.ToLower(cd.Name)] = strings.ToUpper(cd.Collate)
		}
	}
}

// orderByTermCollation returns the COLLATE name applied to an ORDER BY term
// expression (e.g. "ORDER BY x COLLATE nocase"), or "" for BINARY.
func orderByTermCollation(e sql.Expr) string {
	c, _ := execexpr.ExprCollation(e)
	return c
}

// stripCollate removes a top-level COLLATE operator from an expression,
// returning the underlying operand (the value to evaluate).
func stripCollate(e sql.Expr) sql.Expr {
	if b, ok := e.(*sql.BinaryOp); ok && strings.EqualFold(b.Operator, "COLLATE") {
		return b.Left
	}
	return e
}

// setOpName returns the SQL keyword for a compound set operator, used in
// error messages about mismatched result column counts.
func setOpName(op sql.SetOp, unionAll bool) string {
	switch op {
	case sql.SetUnion:
		if unionAll {
			return "UNION ALL"
		}
		return "UNION"
	case sql.SetIntersect:
		return "INTERSECT"
	case sql.SetExcept:
		return "EXCEPT"
	default:
		return "UNION"
	}
}

// mergeCompoundChain iterates the compound (UNION/INTERSECT/EXCEPT) chain
// starting from s, evaluating each member and applying its set operator.
// Returns the merged rows and the trailing ORDER BY/LIMIT/OFFSET (attached to
// the last compound member by the parser). colCount validates member width.
func (e *SelectEngine) mergeCompoundChain(rows [][]interface{}, s *sql.SelectStmt, colls []string, colCount int) ([][]interface{}, []sql.OrderByTerm, sql.Expr, sql.Expr, error) {
	cur := s
	for cur.Union != nil {
		member := cur.Union
		if member.ValuesChain {
			memberResult := e.execValuesGroup(member)
			if memberResult.Error != nil {
				return nil, nil, nil, nil, memberResult.Error
			}
			rows = e.applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll, colls)
			for member.Union != nil && member.SetOp == sql.SetUnion && member.UnionAll {
				member = member.Union
			}
			cur = member
			continue
		}
		memberCopy := *member
		memberCopy.Union = nil
		// A compound member never applies the compound's trailing ORDER BY/
		// LIMIT/OFFSET to itself: the parser attaches those to the tail member
		// as the compound-level clauses, and they belong to the MERGED result
		// (extracted below from the tail). Executing the tail with its own
		// LIMIT truncated that arm pre-merge (tkt-38cb5df375: "... UNION ALL
		// SELECT 9 FROM (...) LIMIT 1" dropped arm-2 rows instead of limiting
		// the union to one row).
		memberCopy.OrderBy = nil
		memberCopy.Limit = nil
		memberCopy.Offset = nil
		prevCompound := e.inCompoundMember
		e.inCompoundMember = true
		memberResult := e.execSelect(&memberCopy)
		e.inCompoundMember = prevCompound
		if memberResult.Error != nil {
			return nil, nil, nil, nil, memberResult.Error
		}
		if len(memberResult.Columns) != colCount {
			return nil, nil, nil, nil, fmt.Errorf("SELECTs to the left and right of %s do not have the same number of result columns", setOpName(cur.SetOp, cur.UnionAll))
		}
		rows = e.applySetOp(rows, memberResult.Rows, cur.SetOp, cur.UnionAll, colls)
		cur = member
	}
	orderBy, limit, offset := compoundTrailingClauses(cur)
	return rows, orderBy, limit, offset, nil
}

// compoundTrailingClauses extracts ORDER BY / LIMIT / OFFSET from the last
// compound member (the parser attaches trailing clauses there, like SQLite).
func compoundTrailingClauses(last *sql.SelectStmt) ([]sql.OrderByTerm, sql.Expr, sql.Expr) {
	var orderBy []sql.OrderByTerm
	var limit, offset sql.Expr
	if len(last.OrderBy) > 0 {
		orderBy = last.OrderBy
	}
	limit = last.Limit
	offset = last.Offset
	return orderBy, limit, offset
}

// execValuesGroup evaluates a VALUES-select head together with its internal
// tuple chain (one UNION ALL node per tuple) as a single row set.
func (e *SelectEngine) execValuesGroup(head *sql.SelectStmt) *Result {
	memberCopy := *head
	memberCopy.Union = nil
	// Values-group members never carry the compound's trailing clauses; clear
	// them on the copies so a trailing-LIMIT tail tuple cannot self-apply.
	memberCopy.OrderBy = nil
	memberCopy.Limit = nil
	memberCopy.Offset = nil
	res := e.execSelect(&memberCopy)
	if res.Error != nil {
		return res
	}
	cur := head
	for cur.Union != nil && cur.SetOp == sql.SetUnion && cur.UnionAll {
		next := cur.Union
		nextCopy := *next
		nextCopy.Union = nil
		nextCopy.OrderBy = nil
		nextCopy.Limit = nil
		nextCopy.Offset = nil
		nres := e.execSelect(&nextCopy)
		if nres.Error != nil {
			return nres
		}
		res.Rows = append(res.Rows, nres.Rows...)
		cur = next
	}
	return res
}

// dedupeRows removes duplicate rows using CompareValues-based keys. colls
// holds the collation of each column (nil → BINARY). SQLite's UNION temp
// b-tree keeps the LAST row inserted for a duplicate key, so a later row
// replaces an earlier equal row (e.g. 'abc' COLLATE nocase UNION 'ABC' yields
// 'ABC').
func (e *SelectEngine) dedupeRows(rows [][]interface{}, colls []string) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}
	last := make(map[string]int)
	for i, row := range rows {
		key := rowKey(row, colls)
		last[key] = i
	}
	seen := make(map[string]bool)
	var result [][]interface{}
	for i, row := range rows {
		key := rowKey(row, colls)
		if last[key] != i || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, row)
	}
	return result
}

// intersectRows returns rows that exist in both a and b (INTERSECT).
// colls holds the collation of each column (nil → BINARY).
func (e *SelectEngine) intersectRows(a, b [][]interface{}, colls []string) [][]interface{} {
	if len(a) == 0 || len(b) == 0 {
		return [][]interface{}{}
	}
	// Build set of b rows
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row, colls)] = true
	}
	// The surviving representative of each distinct key is arm a's LAST
	// row for it (SQLite's INTERSECT keeps the accumulator's final entry —
	// collate5-2.3.x: INTERSECT of 'a'/'A' yields 'A', the later of the
	// nocase-equal rows).
	last := make(map[string]int)
	for i, row := range a {
		last[rowKey(row, colls)] = i
	}
	var result [][]interface{}
	for i, row := range a {
		key := rowKey(row, colls)
		if bSet[key] && last[key] == i {
			result = append(result, row)
		}
	}
	return result
}

// exceptRows returns rows in a that are not in b (EXCEPT).
// colls holds the collation of each column (nil → BINARY).
func (e *SelectEngine) exceptRows(a, b [][]interface{}, colls []string) [][]interface{} {
	if len(a) == 0 {
		return [][]interface{}{}
	}
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row, colls)] = true
	}
	// The surviving representative of each distinct key is the LAST row
	// inserted (SQLite's EXCEPT temp b-tree overwrites on duplicate keys —
	// collate5-2.2.1: EXCEPT yields 'N', the later of the nocase-equal
	// 'n'/'N').
	last := make(map[string]int)
	for i, row := range a {
		key := rowKey(row, colls)
		if bSet[key] {
			continue
		}
		last[key] = i
	}
	var result [][]interface{}
	for i, row := range a {
		key := rowKey(row, colls)
		if bSet[key] {
			continue
		}
		if last[key] != i {
			continue
		}
		result = append(result, row)
	}
	return result
}

// rowKey creates a deduplication key for a row using CompareValues-based
// serialization. This is more robust than fmt.Sprintf because it handles
// type equivalence (int64(1) == float64(1.0) per SQLite affinity).
// colls holds the collation of each column (nil → BINARY); string keys are
// normalized by their column's collation so compound set operators and
// DISTINCT compare with the column's declared collation, matching SQLite.
func rowKey(row []interface{}, colls []string) string {
	parts := make([]string, len(row))
	for i, v := range row {
		raw, coll := extractValue(v)
		if v == nil || raw == nil {
			parts[i] = "\x00"
			continue
		}
		if coll == "" && colls != nil && i < len(colls) {
			coll = colls[i]
		}
		parts[i] = rowValueKey(raw, coll)
	}
	return strings.Join(parts, "\x00")
}

// rowValueKey renders one dedup key component by value type. Numeric keys
// unify INTEGER and REAL so 1 and 1.0 deduplicate (SQLite compares them as
// equal); an integral float formats without a decimal point to match the
// int64 key of the same value, while a fractional float keeps its distinct
// form. A lazy zero blob deduplicates against a materialized blob of the same
// content (distinct-4.1: x'0000000000' and zeroblob(5) are one DISTINCT row).
func rowValueKey(raw interface{}, coll string) string {
	switch x := raw.(type) {
	case int64:
		return "n:" + strconv.FormatInt(x, 10)
	case float64:
		// (see rowValueKey doc)
		if x == float64(int64(x)) {
			return "n:" + strconv.FormatInt(int64(x), 10)
		}
		return "n:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "s:" + normalizeForKey(x, coll)
	case []byte:
		return "b:" + string(x)
	case value.ZeroBlob:
		return "b:" + string(x.Bytes())
	default:
		return "o:" + fmt.Sprintf("%v", util.UnwrapColumnValue(raw))
	}
}

// normalizeForKey applies a collation's normalization to a string for use as
// a deduplication/set-operator key.
func normalizeForKey(s, collation string) string {
	switch strings.ToUpper(collation) {
	case "NOCASE":
		return strings.ToUpper(s)
	case "RTRIM":
		return strings.TrimRight(s, " ")
	default:
		return s
	}
}

// viewDeclaredColumns extracts the optional declared column list from a
// stored view SQL string ("CREATE VIEW v(c0, c1) AS SELECT ...").
// Returns nil when the view has no declared column list.
