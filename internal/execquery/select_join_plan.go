package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func isNaturalJoinType(joinType string) bool {
	return strings.Contains(joinType, "NATURAL")
}

// promoteCorrelatedTVFFrom rewrites a top-level FROM clause whose head is a
// table-valued vtab function with arguments referencing another FROM item
// (tabfunc01-3.1: FROM generate_series(1,x), t1). SQLite resolves TVF
// arguments against every FROM term and runs the referenced table as the
// outer nested-loop cursor, so the function becomes a correlated join
// operand of the referenced table. Returns a rewritten statement copy (the
// original is read-only shared) and ok=true when promotion applied.
func (e *SelectEngine) promoteCorrelatedTVFFrom(s *sql.SelectStmt) (*sql.SelectStmt, bool) {
	if len(s.From.Args) == 0 || !PragmaArgsCorrelated(s.From) || e.outerRow != nil {
		return nil, false
	}
	if _, ok := e.ctx.VTables().Find(strings.ToLower(s.From.Name)); !ok {
		return nil, false
	}
	if len(s.Joins) == 0 {
		return nil, false
	}
	j := s.Joins[0]
	t := j.Table
	// Only a plain comma/cross-joined table can become the outer loop; ON/
	// USING forms and derived or function operands keep their existing path.
	headOK := t.Name != "" && t.Subquery == nil && len(t.Args) == 0 &&
		j.On == nil && len(j.Using) == 0 &&
		(j.CommaJoin || j.JoinType == "" || strings.EqualFold(j.JoinType, "CROSS"))
	if !headOK {
		return nil, false
	}
	ns := *s
	ns.From = t
	joins := make([]sql.JoinClause, 0, len(s.Joins))
	joins = append(joins, sql.JoinClause{JoinType: ",", CommaJoin: true, Table: s.From})
	joins = append(joins, s.Joins[1:]...)
	ns.Joins = joins
	return &ns, true
}

// joinTypeHas reports whether a join type string includes the given type
// keyword (e.g. "NATURAL LEFT" has LEFT).
func joinTypeHas(joinType, typ string) bool {
	return strings.Contains(joinType, typ)
}

// materializeJoinRight materializes the right-side operand of a join clause:
// a derived subquery, a table-valued pragma function, a CTE, a view, or a
// real table. Returns the row maps, column defs, the (possibly aliased) table
// name, and — for correlated pragmas — the per-left-row right-row index map.
func (e *SelectEngine) materializeJoinRight(s *sql.SelectStmt, join sql.JoinClause, currentMaps []RowMap) ([]RowMap, []sql.ColumnDef, string, []int, error) {
	var rightMaps []RowMap
	var rightDefs []sql.ColumnDef
	var corrLeftIdx []int
	var tableName string

	// Handle derived table (subquery) in JOIN: JOIN (SELECT ...) AS t
	if join.Table.Subquery != nil {
		return e.materializeSubqueryJoin(join)
	}
	if isPragmaTableFunc(join.Table.Name) {
		return e.materializePragmaJoin(join, currentMaps)
	}
	// Correlated table-valued vtab function in a JOIN (json_each(j2.json) AS
	// jx): the argument references a left-side column, so materialize per
	// left row with that row as evaluation context.
	if join.Table.Args != nil && pragmaArgsCorrelated(join.Table) {
		if _, isModule := e.ctx.VTables().Find(strings.ToLower(join.Table.Name)); isModule {
			var merr error
			rightDefs, rightMaps, corrLeftIdx, merr = e.ctx.MaterializeCorrelatedVTabFunc(join.Table, currentMaps, s.Where)
			if merr != nil {
				return nil, nil, "", nil, merr
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			return rightMaps, rightDefs, tableName, corrLeftIdx, nil
		}
	}
	// Function-call syntax on an ordinary relation: SQLite resolve.c's
	// "'%s' is not a function" (tabfunc01-1.25: FROM t0(55)). Genuine
	// table-valued modules and pragma functions were handled above.
	if join.Table.IsTabFunc && !isPragmaTableFunc(join.Table.Name) {
		if _, isModule := e.ctx.VTables().Find(strings.ToLower(join.Table.Name)); !isModule {
			if _, _, terr := e.ctx.FindTable(join.Table.Name); terr == nil {
				return nil, nil, "", nil, fmt.Errorf("'%s' is not a function", join.Table.Name)
			}
			if _, _, verr := e.ctx.FindView(join.Table.Name); verr == nil {
				return nil, nil, "", nil, fmt.Errorf("'%s' is not a function", join.Table.Name)
			}
			// Unknown name: fall through for the normal "no such table".
		}
	}
	// Eponymous / table-valued vtab module operand that schema lookup cannot
	// resolve (FROM t1, carray WHERE carray.pointer = t1.x; tabfunc01-700:
	// FROM t600, carray(inttoptr(...),5)). Materialize via the module
	// registry with WHERE pushdown; non-correlated args concatenate rows.
	if _, isModule := e.ctx.VTables().Find(strings.ToLower(join.Table.Name)); isModule && !isPragmaTableFunc(join.Table.Name) {
		defs, rows, rowids, err := e.ctx.MaterializeVtabTableFunc(join.Table, e.vtabScanOptions(s))
		if err != nil {
			return nil, nil, "", nil, err
		}
		tableName = join.Table.Name
		if join.Table.As != "" {
			tableName = join.Table.As
		}
		// Hidden columns stay out of the row map (never projected by t.*),
		// while native rowids back aa.rowid references (tabfunc01-751:
		// ON aa.rowid=bb.rowid over two carray instances).
		for i, row := range rows {
			m := make(RowMap)
			for j, val := range row {
				if j < len(defs) && !defs[j].Hidden {
					m[defs[j].Name] = val
				}
			}
			if i < len(rowids) {
				m["rowid"] = rowids[i]
			}
			rightMaps = append(rightMaps, m)
		}
		return rightMaps, defs, tableName, nil, nil
	}
	if cteDef, ok := e.findCTE(s, join.Table.Name); ok {
		if join.Table.IsTabFunc {
			return nil, nil, "", nil, fmt.Errorf("'%s' is not a function", join.Table.Name)
		}
		var merr error
		rightDefs, rightMaps, merr = e.materializeCTEForJoin(&cteDef)
		if merr != nil {
			return nil, nil, "", nil, merr
		}
		tableName = join.Table.Name
		if join.Table.As != "" {
			tableName = join.Table.As
		}
		return rightMaps, rightDefs, tableName, corrLeftIdx, nil
	}
	tableEntry, _, tableErr := e.ctx.FindTable(join.Table.Name)
	if tableErr != nil {
		return e.materializeViewJoin(join)
	}
	// A virtual table (RootPage == 0) with a correlated first-column equality
	// constraint in the WHERE (`input = <left.col>`) is materialized per left
	// row (fts3tokenize in a join — fts3tok1 1.13.2).
	if tableEntry.RootPage == 0 && s.Where != nil {
		if col, ok := vtabCorrelatedInput(s.Where); ok {
			defs, maps, leftIdx, merr := e.ctx.MaterializeCorrelatedVTab(tableEntry, col, currentMaps)
			if merr != nil {
				return nil, nil, "", nil, merr
			}
			tableName := join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			return maps, defs, tableName, leftIdx, nil
		}
	}
	// A virtual table operand bound to already-materialized outer aliases by
	// rowid equi-conjuncts (a.rowid=b.rowid AND b.rowid=c.rowid) is seeked
	// per outer row — the engine equivalent of SQLite's omitted unique
	// xBestIndex rowid constraint re-running xFilter per loop iteration
	// (unionvtab.c xBestIndex: EQ -> omit + SQLITE_INDEX_SCAN_UNIQUE).
	if seekMaps, seekDefs, seekName, seekIdx, handled, serr := e.tryVtabRowidSeek(s, join, tableEntry, currentMaps); serr != nil {
		return nil, nil, "", nil, serr
	} else if handled {
		return seekMaps, seekDefs, seekName, seekIdx, nil
	}
	// Real table: parse column defs and scan rows (or materialize a virtual
	// table with RootPage == 0).
	return e.materializeTableJoin(s, join, tableEntry)
}

// materializePragmaJoin builds the right-side row maps for a table-valued
// pragma function join operand (handling correlation when an argument
// references an outer column).
func (e *SelectEngine) materializePragmaJoin(join sql.JoinClause, currentMaps []RowMap) ([]RowMap, []sql.ColumnDef, string, []int, error) {
	var rightMaps []RowMap
	var rightDefs []sql.ColumnDef
	var corrLeftIdx []int
	var tableName string
	// Table-valued pragma function in a JOIN: pragma_table_info('t1') AS t
	// When an argument references an outer (left-side) column, the pragma
	// is materialized per left row with that row as evaluation context
	// (SQLite correlation, e.g. FROM sqlite_schema,
	// pragma_foreign_key_check(name)).
	if pragmaArgsCorrelated(join.Table) {
		var merr error
		rightDefs, rightMaps, corrLeftIdx, merr = e.ctx.MaterializeCorrelatedPragma(join.Table, currentMaps)
		if merr != nil {
			return nil, nil, "", nil, merr
		}
		tableName = join.Table.Name
		if join.Table.As != "" {
			tableName = join.Table.As
		}
	} else {
		defs, rows, err := e.ctx.MaterializePragmaTable(join.Table)
		if err != nil {
			return nil, nil, "", nil, err
		}
		rightDefs = defs
		tableName = join.Table.Name
		if join.Table.As != "" {
			tableName = join.Table.As
		}
		for _, row := range rows {
			rightRowMap := make(RowMap)
			for i, val := range row {
				if i < len(rightDefs) {
					rightRowMap[rightDefs[i].Name] = val
				}
			}
			rightMaps = append(rightMaps, rightRowMap)
		}
	}
	return rightMaps, rightDefs, tableName, corrLeftIdx, nil
}

// buildJoinAutoIndex builds the ephemeral equi-join hash index on the right
// table's join column (for "left.col = right.col" ON patterns). Returns nil
// when the ON clause is not a simple equi-join on an existing right column.
func (e *SelectEngine) buildJoinAutoIndex(effectiveOn sql.Expr, rightMaps []RowMap, rightDefs []sql.ColumnDef, lastTableName, tableName string) map[interface{}][]joinIndexEntry {
	_, rightColName := extractEquiJoinCols(effectiveOn, lastTableName, tableName)
	// Only build the autoindex when the extracted right column actually
	// exists in the right operand's defs. extractEquiJoinCols falls back
	// to assuming unqualified x=y means x-left/y-right, but both may be
	// LEFT columns (SELECT * FROM t3 JOIN t2 ON x=y where t2 has no y);
	// an empty index would wrongly short-circuit the join to zero rows.
	if rightColName == "" || len(rightMaps) == 0 || !rightDefHasColumn(rightDefs, rightColName) {
		return nil
	}
	autoIndex := make(map[interface{}][]joinIndexEntry)
	for ri, rm := range rightMaps {
		if val, ok := rm[rightColName]; ok {
			// Unwrap ColumnValue (and CollatedValue) wrappers so the map
			// key compares by value, not by pointer identity. Normalize
			// numeric text (e.g. '0' vs 0) so affinity-aware equality
			// matches.
			key := joinIndexKey(val)
			autoIndex[key] = append(autoIndex[key], joinIndexEntry{row: rm, idx: ri})
		}
	}
	return autoIndex
}

// prefilterJoinRightOnly pre-filters the right table when the ON references
// ONLY right-table columns (e.g. LEFT JOIN t2 ON t2.x>0): the condition is
// independent of the left row, so filter rightMaps once instead of per left
// row. Returns ok=true when the filter was applied (the caller clears the ON
// clause).
func (e *SelectEngine) prefilterJoinRightOnly(effectiveOn sql.Expr, rightMaps []RowMap, tableName string) ([]RowMap, bool) {
	if effectiveOn == nil || !joinONReferencesOnlyRight(effectiveOn, tableName) {
		return rightMaps, false
	}
	var filtered []RowMap
	for _, rm := range rightMaps {
		// Evaluate against a map that also exposes the right table's
		// qualified keys (t2.x) for view/subquery operands whose rows
		// are keyed unqualified.
		probe := make(RowMap, len(rm)+2)
		for k, v := range rm {
			probe[k] = v
			if tableName != "" && !strings.Contains(k, ".") {
				probe[tableName+"."+k] = v
			}
		}
		if e.evalOnCondition(effectiveOn, probe) {
			filtered = append(filtered, rm)
		}
	}
	return filtered, true
}

// shouldSwapCommaJoin reports whether a two-table comma/cross join should be
// executed with the join (right) table as the OUTER loop instead of the base
// (left) table, matching SQLite's query planner. SQLite prefers to scan the
// table WITHOUT a usable index outer and the indexed table inner (a
// covering-index scan or PRIMARY KEY lookup is cheapest inner). Only plain
// comma/cross joins (no ON, no USING/NATURAL, no LEFT/RIGHT/FULL) qualify.
func (e *SelectEngine) shouldSwapCommaJoin(s *sql.SelectStmt, join sql.JoinClause, leftName, rightName string) bool {
	if !commaJoinSwapEligible(s, join, leftName, rightName) {
		return false
	}
	// One side a view and the other a real table: SQLite materializes the
	// view and, when the WHERE equates a view column to a value or to a
	// column of the other side, builds an automatic index on that view
	// column and scans the real table OUTER (probing the materialized view
	// inner). Swap so the real table becomes the outer loop.
	leftView := e.isViewName(leftName)
	rightView := e.isViewName(rightName)
	if leftView != rightView {
		viewName, tableName := rightName, leftName
		if leftView {
			viewName, tableName = leftName, rightName
		}
		if e.whereEqualityOnViewCol(s.Where, viewName, tableName) {
			return leftView // swap when the view is the left/base
		}
	}
	// The base and join tables must be real tables with column defs.
	leftDefs := e.ctx.ParseColumnDefs(leftName, e.tableCreateSQL(leftName))
	rightDefs := e.ctx.ParseColumnDefs(rightName, e.tableCreateSQL(rightName))
	if len(leftDefs) == 0 || len(rightDefs) == 0 {
		return false
	}
	// Find an equi-join condition (A.col = B.col) between the two tables in
	// the WHERE clause. When exactly one side's column is indexed (PRIMARY
	// KEY / UNIQUE), the indexed side goes INNER, so swap when the BASE
	// (left) table is the indexed side.
	if lc, rc, ok := e.findEquiJoinCols(s.Where, leftName, rightName); ok {
		lIdx := e.columnIndexed(leftDefs, lc, leftName)
		rIdx := e.columnIndexed(rightDefs, rc, rightName)
		if lIdx != rIdx {
			return lIdx
		}
	}
	// No equi-join (or both/neither side indexed): prefer the table without
	// any index as the OUTER loop. Swap when the base has an index and the
	// join table has none.
	leftAny := e.tableHasAnyIndex(leftDefs, leftName)
	rightAny := e.tableHasAnyIndex(rightDefs, rightName)
	if leftAny != rightAny {
		return leftAny
	}
	return false
}

// commaJoinSwapEligible reports whether the join is a plain comma/cross join
// between two named tables with no ON/USING/NATURAL modifiers.
func commaJoinSwapEligible(s *sql.SelectStmt, join sql.JoinClause, leftName, rightName string) bool {
	if len(s.Joins) != 1 {
		return false
	}
	jt := strings.ToUpper(join.JoinType)
	if jt != "" && jt != "CROSS" {
		return false
	}
	if join.On != nil || len(join.Using) > 0 || isNaturalJoinType(join.JoinType) {
		return false
	}
	return leftName != "" && rightName != ""
}

// findEquiJoinCols extracts a simple equality condition "left.col = right.col"
// (either side) between the two joined tables from a WHERE expression. It
// searches inside AND chains and returns the left table's column and the right
// table's column.
func (e *SelectEngine) findEquiJoinCols(where sql.Expr, leftName, rightName string) (string, string, bool) {
	if where == nil {
		return "", "", false
	}
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		if lc, rc, ok := e.findEquiJoinCols(bop.Left, leftName, rightName); ok {
			return lc, rc, true
		}
		return e.findEquiJoinCols(bop.Right, leftName, rightName)
	}
	bop, ok := where.(*sql.BinaryOp)
	if !ok || (bop.Operator != "=" && bop.Operator != "==") {
		return "", "", false
	}
	lc, lok := e.equiSideColumn(bop.Left)
	rc, rok := e.equiSideColumn(bop.Right)
	if !lok || !rok {
		return "", "", false
	}
	// One side must reference the left table, the other the right table.
	lIsLeft := e.colRefTable(bop.Left, leftName)
	rIsLeft := e.colRefTable(bop.Right, leftName)
	if lIsLeft == rIsLeft {
		return "", "", false
	}
	if lIsLeft {
		return lc, rc, true
	}
	return rc, lc, true
}

// equiSideColumn returns the column name of an equality operand if it is a
// (possibly qualified) column reference.
func (e *SelectEngine) equiSideColumn(expr sql.Expr) (string, bool) {
	ref, ok := expr.(*sql.ColumnRef)
	if !ok {
		return "", false
	}
	return ref.Name, true
}

// colRefTable reports whether a column reference is qualified with the given
// table name (or its alias), case-insensitively.
func (e *SelectEngine) colRefTable(expr sql.Expr, tableName string) bool {
	ref, ok := expr.(*sql.ColumnRef)
	if !ok {
		return false
	}
	if ref.Table == "" {
		return false
	}
	base := ref.Table
	if dot := strings.Index(base, "."); dot >= 0 {
		base = base[dot+1:]
	}
	return strings.EqualFold(base, tableName)
}

// tableHasAnyIndex reports whether a table has any PRIMARY KEY or UNIQUE index
// usable by the planner for an inner-loop scan.
func (e *SelectEngine) tableHasAnyIndex(defs []sql.ColumnDef, tableName string) bool {
	for _, cd := range defs {
		if isIPKRowidAliasCol(cd) || cd.PrimaryKey || cd.Unique {
			return true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableName, e.tableCreateSQL(tableName)) {
		if tc.Type == sql.ConstraintPrimaryKey || tc.Type == sql.ConstraintUnique {
			return true
		}
	}
	return len(e.ctx.UniqueIndexColumns(tableName)) > 0
}

// tableCreateSQL returns the CREATE TABLE SQL for a table name ("" when not
// found), used by join-planning helpers.
func (e *SelectEngine) tableCreateSQL(tableName string) string {
	if entry, _, err := e.ctx.FindTable(tableName); err == nil {
		return entry.SQL
	}
	return ""
}

// isViewName reports whether name refers to a view (not a real table).
func (e *SelectEngine) isViewName(name string) bool {
	if name == "" {
		return false
	}
	if _, _, err := e.ctx.FindTable(name); err == nil {
		return false
	}
	_, _, err := e.ctx.FindView(name)
	return err == nil
}

// whereEqualityOnViewCol reports whether the WHERE clause contains an equality
// ("=" or "==") whose operand is a column of the view (qualified with the
// view name, or unqualified when it is not a column of the other table). This
// mirrors the condition under which SQLite builds an automatic index on a
// materialized view's column and swaps the join so the real table scans outer.
func (e *SelectEngine) whereEqualityOnViewCol(where sql.Expr, viewName, tableName string) bool {
	if where == nil {
		return false
	}
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		return e.whereEqualityOnViewCol(bop.Left, viewName, tableName) ||
			e.whereEqualityOnViewCol(bop.Right, viewName, tableName)
	}
	bop, ok := where.(*sql.BinaryOp)
	if !ok || (bop.Operator != "=" && bop.Operator != "==") {
		return false
	}
	return e.equalitySideIsViewCol(bop.Left, viewName, tableName) ||
		e.equalitySideIsViewCol(bop.Right, viewName, tableName)
}

// equalitySideIsViewCol reports whether an equality operand is a column
// reference that resolves to the view: qualified with the view name, or
// unqualified when the name is not a column of the other (real) table.
func (e *SelectEngine) equalitySideIsViewCol(expr sql.Expr, viewName, tableName string) bool {
	ref, ok := expr.(*sql.ColumnRef)
	if !ok {
		return false
	}
	if ref.Table != "" {
		return strings.EqualFold(ref.Table, viewName)
	}
	// Unqualified: resolve against the other table's columns. If the name is
	// not one of the table's columns, it must be the view's.
	names, err := e.tableColumnNames(tableName)
	if err != nil {
		return true
	}
	for _, n := range names {
		if strings.EqualFold(n, ref.Name) {
			return false
		}
	}
	return true
}

// joinIndexEntry pairs a right row with its index in rightMaps, used by the
// ephemeral equi-join hash index so RIGHT/FULL joins can track which right
// rows matched.
type joinIndexEntry struct {
	row RowMap
	idx int
}

// joinIndexKey unwraps a row value's ColumnValue and CollatedValue wrappers
// and normalizes it for use as an ephemeral equi-join hash key. A NOCASE
// collation lowercases the key so 'ABC' and 'abc' match.
func joinIndexKey(v interface{}) interface{} {
	coll := ""
	if cv, ok := v.(*CollatedValue); ok {
		coll = cv.Collation
		v = cv.Value
	}
	key := normalizeJoinKey(util.UnwrapColumnValue(v))
	if coll == "NOCASE" {
		if s, ok := key.(string); ok {
			return strings.ToLower(s)
		}
		if b, ok := key.([]byte); ok {
			return strings.ToLower(string(b))
		}
	}
	return key
}

// normalizeJoinKey converts a value to a canonical key for the ephemeral
// equi-join hash index: numeric text (e.g. "0", "1.5") becomes its numeric
// value so a text '0' right key matches an integer 0 left key (SQLite's
// affinity-aware equality). Non-numeric values are returned unchanged.
func normalizeJoinKey(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
		return t
	case []byte:
		return normalizeJoinKey(string(t))
	default:
		return v
	}
}

// joinONReferencesOnlyRight reports whether every column reference in a join
// ON expression is qualified to the RIGHT table (none unqualified, none to
// the left or other tables). Such a condition is independent of the left row
// and can be pre-filtered once on the right rows.
func joinONReferencesOnlyRight(on sql.Expr, rightName string) bool {
	if on == nil {
		return false
	}
	onlyRight := true
	walkJoinOnExpr(on, func(e sql.Expr) {
		cr, ok := e.(*sql.ColumnRef)
		if !ok {
			return
		}
		if cr.Table == "" || !strings.EqualFold(cr.Table, rightName) {
			onlyRight = false
		}
	})
	return onlyRight
}

// naturalJoinCommonCols returns the set of column names common to the left
// and right table definitions (used to merge columns in NATURAL JOIN output).
func naturalJoinCommonCols(leftDefs, rightDefs []sql.ColumnDef) map[string]bool {
	rightNames := make(map[string]bool)
	for _, cd := range rightDefs {
		rightNames[cd.Name] = true
	}
	common := make(map[string]bool)
	for _, cd := range leftDefs {
		if rightNames[cd.Name] {
			common[cd.Name] = true
		}
	}
	return common
}

// generateNaturalJoinOn creates an ON expression for a NATURAL JOIN by finding
// all common column names between left and right table definitions and creating
// equality conditions: col = col AND col2 = col2 ...
// If no common columns exist, NATURAL JOIN behaves as a CROSS JOIN (nil ON).
func (e *SelectEngine) generateNaturalJoinOn(leftDefs, rightDefs []sql.ColumnDef, leftName, rightName string) sql.Expr {
	rightNames := make(map[string]bool)
	for _, cd := range rightDefs {
		rightNames[cd.Name] = true
	}
	var onExpr sql.Expr
	for _, cd := range leftDefs {
		if rightNames[cd.Name] {
			// Generate an equality whose LEFT side is UNQUALIFIED: in a chained
			// natural join the merged column (stored unqualified in the combined
			// row map) is the value from whichever side supplied it, while the
			// qualified left-table name can be NULL for rows that only matched
			// on a deeper table (t1 FULL JOIN t2 FULL JOIN t3 must match
			// t2-only rows of the first join against t3 on the merged id).
			eq := &sql.BinaryOp{
				Left:     &sql.ColumnRef{Name: cd.Name},
				Right:    &sql.ColumnRef{Table: rightName, Name: cd.Name},
				Operator: "=",
			}
			if onExpr == nil {
				onExpr = eq
			} else {
				onExpr = &sql.BinaryOp{Left: onExpr, Right: eq, Operator: "AND"}
			}
		}
	}
	return onExpr
}

// generateUsingJoinOn builds ON col = col for each USING column. The refs are
// unqualified (like NATURAL JOIN) so filterUsingColumns/collectUsingColumns can
// recognize and merge them (the column resolution uses currentScanTable).
func (e *SelectEngine) generateUsingJoinOn(cols []string, leftName, rightName string) sql.Expr {
	var onExpr sql.Expr
	for _, col := range cols {
		// The LEFT side is UNQUALIFIED: in a chained join the immediate left
		// "table" may be a JOIN result whose merged column is stored under
		// the plain name, while the qualified name (e.g. dual.id for
		// t3 JOIN dual FULL JOIN t4 USING(id)) may not exist. The merged
		// column always resolves via the unqualified key. When the right side
		// is a derived table without an alias (rightName == ""), its column
		// is also unqualified.
		right := &sql.ColumnRef{Table: rightName, Name: col}
		if rightName == "" {
			right = &sql.ColumnRef{Name: col}
		}
		eq := &sql.BinaryOp{
			Left:     &sql.ColumnRef{Name: col},
			Right:    right,
			Operator: "=",
		}
		if onExpr == nil {
			onExpr = eq
		} else {
			onExpr = &sql.BinaryOp{Left: onExpr, Right: eq, Operator: "AND"}
		}
	}
	return onExpr
}

// buildCombinedRowMap creates a combined row map from left and right join sides.
// It stores values under both unqualified names and table-prefixed names so that
// qualified column references (e.g., "data.id") resolve correctly for both sides.
func (e *SelectEngine) buildCombinedRowMap(leftMap, rightMap RowMap, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	// When the left side is itself a joined compound, its map already carries
	// qualified keys for every operand table; re-prefixing the unqualified
	// keys with a single table name would fabricate garbage keys (e.g.
	// dual.a for t1's column a) that leak into t.* star expansion.
	compound := leftMapHasForeignQualifiedKeys(leftMap, leftTableName)
	// Copy the left map. Keys already qualified (containing a '.') are
	// copied as-is; unqualified keys (from the base table scan) get the
	// left-table prefix added once. Re-qualifying keys that are already
	// qualified would produce garbage keys like "t1.t2.a".
	for k, v := range leftMap {
		combined[k] = v
		if !compound && !strings.Contains(k, ".") && k != "rowid" {
			qk := leftTableName + "." + k
			if _, exists := combined[qk]; !exists {
				combined[qk] = v
			}
		}
	}
	for k, v := range rightMap {
		// Prefix unqualified keys with the right table name; keys already
		// qualified (e.g. a view operand's dual.dummy) are copied as-is.
		// Prefixing them again would fabricate garbage keys like
		// dual.dual.dummy that leak into t.* star expansion.
		if !strings.Contains(k, ".") {
			combined[tableName+"."+k] = v
		}
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	// The left table's qualified rowid key (t1.rowid) must carry the LEFT
	// table's own rowid. Fabricating it from the bare "rowid" key is only
	// valid for a single base-table scan: in a chained join the bare key
	// holds the FIRST table's rowid (each right alias's bare key keeps the
	// existing first-table value), so overwriting the immediate-left alias's
	// qualified key from it would corrupt b.rowid from the third alias on.
	// In the compound case the correct key already exists — it was set when
	// that alias was the right side of the previous join.
	if !compound && leftTableName != "" {
		combined[leftTableName+".rowid"] = leftMap["rowid"]
	}
	return combined
}

// leftMapHasForeignQualifiedKeys reports whether the left row map already
// carries qualified keys (X.col) whose prefix is not the immediate-left table
// name — i.e. the left side is a compound of several joined tables rather than
// a single base-table scan.
func leftMapHasForeignQualifiedKeys(leftMap RowMap, leftTableName string) bool {
	for k := range leftMap {
		if dot := strings.Index(k, "."); dot > 0 && k[:dot] != leftTableName {
			return true
		}
	}
	return false
}

// evalOnCondition evaluates a JOIN ON condition against a combined row map.
func (e *SelectEngine) evalOnCondition(on sql.Expr, row Row) bool {
	if on == nil {
		return true
	}
	match, err := e.ctx.EvalBool(on, row)
	return err == nil && match
}

// filterUsingColumns filters right-side column definitions to exclude columns
// that are part of a USING clause. The USING clause generates equality conditions
// in the ON expression, and those columns should appear only once in the result.
func (e *SelectEngine) filterUsingColumns(rightDefs []sql.ColumnDef, on sql.Expr, naturalCols map[string]bool, usingJoin bool) []sql.ColumnDef {
	if !usingJoin || (on == nil && len(naturalCols) == 0) {
		return rightDefs
	}
	// Collect column names referenced in USING equality conditions.
	usingCols := make(map[string]bool)
	collectUsingColumns(on, usingCols)
	for c := range naturalCols {
		usingCols[c] = true
	}
	if len(usingCols) == 0 {
		return rightDefs
	}
	var filtered []sql.ColumnDef
	for _, cd := range rightDefs {
		if usingCols[cd.Name] {
			continue // skip — this column is merged by USING
		}
		filtered = append(filtered, cd)
	}
	return filtered
}

// rightDefHasColumn reports whether defs contains a column named name
// (case-insensitive), including prefixed defs (table.col).
func rightDefHasColumn(defs []sql.ColumnDef, name string) bool {
	for _, cd := range defs {
		if strings.EqualFold(cd.Name, name) ||
			strings.HasSuffix(strings.ToLower(cd.Name), "."+strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// prefixRightColDefs prefixes right-table column names with the table name
// when they conflict with columns already in the left table. This ensures
// that * expansion resolves values using qualified keys (table.col) from
// the combined row map, avoiding incorrect resolution to the left table's values.
func (e *SelectEngine) prefixRightColDefs(rightDefs, leftDefs []sql.ColumnDef, tableName string) []sql.ColumnDef {
	// Build set of left-column names for quick conflict detection.
	leftNames := make(map[string]bool)
	for _, cd := range leftDefs {
		leftNames[cd.Name] = true
	}
	needsPrefix := false
	for _, cd := range rightDefs {
		if leftNames[cd.Name] {
			needsPrefix = true
			break
		}
	}
	if !needsPrefix {
		return rightDefs
	}
	named := make([]sql.ColumnDef, len(rightDefs))
	for i, cd := range rightDefs {
		named[i] = cd
		if leftNames[cd.Name] {
			named[i].Name = tableName + "." + cd.Name
		}
	}
	return named
}

// buildLeftJoinRow creates a row for LEFT JOIN when no match is found.
func (e *SelectEngine) buildLeftJoinRow(leftMap RowMap, rightDefs []sql.ColumnDef, tableName, leftTableName string) RowMap {
	combined := make(RowMap)
	for k, v := range leftMap {
		combined[k] = v
		if leftTableName != "" && !strings.Contains(k, ".") && k != "rowid" {
			qk := leftTableName + "." + k
			if _, exists := combined[qk]; !exists {
				combined[qk] = v
			}
		}
	}
	// Preserve the left table's qualified rowid key (t1.rowid) so qualified
	// rowid references resolve even when the right side has no match. The
	// matched-row path (buildCombinedRowMap) sets this; the unmatched
	// LEFT JOIN row must too. As there: only for a single base-table scan —
	// a compound left's bare "rowid" holds the FIRST table's rowid, and the
	// immediate-left alias's qualified key is already correct from the
	// previous join.
	if leftTableName != "" && !leftMapHasForeignQualifiedKeys(leftMap, leftTableName) {
		if v, ok := leftMap["rowid"]; ok {
			combined[leftTableName+".rowid"] = v
		}
	}
	for _, cd := range rightDefs {
		combined[tableName+"."+cd.Name] = nil
		if _, exists := combined[cd.Name]; !exists {
			combined[cd.Name] = nil
		}
	}
	return combined
}

// hasAggregates checks if any SELECT column uses an aggregate function.
