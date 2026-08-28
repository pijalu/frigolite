// Package exec implements query execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func literalForValue(v interface{}) sql.Expr {
	switch n := util.UnwrapColumnValue(v).(type) {
	case nil:
		return &sql.NullLit{}
	case int64:
		return &sql.NumericLit{Value: fmt.Sprintf("%d", n)}
	case float64:
		return &sql.NumericLit{Value: fmt.Sprintf("%g", n)}
	case string:
		return &sql.StringLit{Value: n}
	case []byte:
		return &sql.BlobLit{Value: n}
	default:
		return &sql.NullLit{}
	}
}

// collectViewUpdatePairs builds the matched (old,new) pairs for an UPDATE on
// a view, applying the WHERE clause per row.
func (e *DMLExecutor) collectViewUpdatePairs(s *sql.UpdateStmt, rows [][]interface{}, viewCols []string) ([]viewUpdatePair, error) {
	var pairs []viewUpdatePair
	var evalRows []RowMap
	var evalRowOwners []RowMap
	// First pass: collect all matched eval rows (one per JOIN combination for
	// UPDATE ... FROM, per SQLite's INSTEAD OF trigger firing semantics).
	for _, rowVals := range rows {
		oldRow := buildViewOldRow(rowVals, viewCols)
		matchedRows, matched, err := e.applyViewWhere(s, oldRow)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		for _, evalRow := range matchedRows {
			evalRows = append(evalRows, evalRow)
			evalRowOwners = append(evalRowOwners, oldRow)
		}
	}
	// Second pass: when a SET expression contains a window function, compute it
	// over the matched eval rows (window1 73.4: UPDATE view SET c=nth_value(15,2)
	// OVER() FROM ... computes nth_value over all 9 joined rows → c=15 each).
	winVals, err := e.computeViewSetWindowValues(s, evalRows)
	if err != nil {
		return nil, err
	}
	for i, evalRow := range evalRows {
		oldRow := evalRowOwners[i]
		newRow, err := e.applyViewSetAssignments(s, oldRow, evalRow, winVals, i)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, viewUpdatePair{oldRow: oldRow, newRow: newRow})
	}
	return pairs, nil
}

// computeViewSetWindowValues computes the window functions appearing in a
// view UPDATE's SET expressions over the matched eval rows. Returns a map from
// each window FuncCall (by pointer) to its per-row values, or nil when no SET
// expression contains a window function.
func (e *DMLExecutor) computeViewSetWindowValues(s *sql.UpdateStmt, evalRows []RowMap) (map[*sql.FuncCall][]interface{}, error) {
	var winNodes []*sql.FuncCall
	seen := make(map[*sql.FuncCall]bool)
	for _, a := range s.Assignments {
		execquery.WalkExprFull(a.Value, func(n sql.Expr) {
			fn, ok := n.(*sql.FuncCall)
			if ok && fn.Over != nil && !seen[fn] {
				seen[fn] = true
				winNodes = append(winNodes, fn)
			}
		})
	}
	if len(winNodes) == 0 {
		return nil, nil
	}
	vals := make(map[*sql.FuncCall][]interface{}, len(winNodes))
	for _, fn := range winNodes {
		v, err := e.ctx.ComputeWindowValues(fn, nil, evalRows)
		if err != nil {
			return nil, err
		}
		vals[fn] = v
	}
	return vals, nil
}

// orderUpdateViewPairs applies UPDATE ... ORDER BY to a view's trigger row
// pairs (ordering by the old row values).
func orderUpdateViewPairs(e *DMLExecutor, s *sql.UpdateStmt, pairs []viewUpdatePair) []viewUpdatePair {
	if len(s.OrderBy) == 0 {
		return pairs
	}
	oldRows := make([]RowMap, len(pairs))
	for i, p := range pairs {
		oldRows[i] = p.oldRow
	}
	e.sortDeleteRows(oldRows, s.OrderBy)
	order := make(map[string]int, len(oldRows)) // oldRow pointer -> pair index
	for i, p := range pairs {
		order[fmt.Sprintf("%p", p.oldRow)] = i
	}
	reordered := make([]viewUpdatePair, 0, len(pairs))
	for _, or := range oldRows {
		reordered = append(reordered, pairs[order[fmt.Sprintf("%p", or)]])
	}
	return reordered
}

// limitUpdateViewPairs applies UPDATE ... LIMIT/OFFSET to a view's trigger
// row pairs (keeping only the LIMIT window).
func limitUpdateViewPairs(e *DMLExecutor, s *sql.UpdateStmt, pairs []viewUpdatePair) ([]viewUpdatePair, error) {
	if s.Limit == nil {
		return pairs, nil
	}
	oldRows := make([]RowMap, len(pairs))
	for i, p := range pairs {
		oldRows[i] = p.oldRow
	}
	oldRows, lerr := e.limitDeleteRows(oldRows, &sql.DeleteStmt{Where: s.Where, OrderBy: s.OrderBy, Limit: s.Limit, Offset: s.Offset})
	if lerr != nil {
		return nil, lerr
	}
	keep := make(map[string]bool, len(oldRows))
	for _, or := range oldRows {
		keep[fmt.Sprintf("%p", or)] = true
	}
	var limited []viewUpdatePair
	for _, p := range pairs {
		if keep[fmt.Sprintf("%p", p.oldRow)] {
			limited = append(limited, p)
		}
	}
	return limited, nil
}

func (e *DMLExecutor) collectUpdateChanges(tableName string, rootPage uint32, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt, deferSetEval bool) ([]updateChange, error) {
	tree := e.dmlTableBTree(tableName, rootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, fmt.Errorf("exec: cursor error: %w", err)
	}

	// Set the current scan table so table-qualified column references
	// ("t1.a") in the WHERE clause resolve to the row map.
	prevScan := e.ctx.CurrentScanTable()
	e.ctx.SetCurrentScanTable(tableName)
	defer func() { e.ctx.SetCurrentScanTable(prevScan) }()

	var changes []updateChange
	var rowMaps []RowMap
	for {
		// SQLITE_TEST interrupt countdown: one op per row examined
		// (src/vdbe.c per-opcode decrement of sqlite3_interrupt_count).
		if err := e.ctx.CheckProgress(); err != nil {
			return nil, err
		}
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		ch, matchRow, matched, err := e.matchUpdateRow(s, cell, rec, colIndex, colDefs, row, deferSetEval)
		if err != nil {
			return nil, err
		}
		if matched {
			changes = append(changes, *ch)
			rowMaps = append(rowMaps, matchRow)
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	// Apply UPDATE ... ORDER BY ... LIMIT (a SQLite extension). SQLite's
	// semantics (EVIDENCE-OF: R-10927-26133): the ORDER BY clause is used
	// only to determine which rows fall within the LIMIT; the order in which
	// rows are modified is NOT influenced by ORDER BY (rows are processed in
	// natural rowid order).
	changes = e.applyUpdateOrderLimit(changes, rowMaps, s)
	return changes, nil
}

// applyUpdateOrderLimit applies UPDATE ... ORDER BY ... LIMIT: sort a copy of
// the changes by ORDER BY (evaluated on the rows' original values), keep only
// the LIMIT window, then restore the natural rowid order for the survivors.
func (e *DMLExecutor) applyUpdateOrderLimit(changes []updateChange, rowMaps []RowMap, s *sql.UpdateStmt) []updateChange {
	if len(s.OrderBy) == 0 && s.Limit == nil {
		return changes
	}
	sorted := make([]updateChange, len(changes))
	copy(sorted, changes)
	sortedRows := make([]RowMap, len(rowMaps))
	copy(sortedRows, rowMaps)
	if len(s.OrderBy) > 0 {
		e.sortUpdateChanges(sorted, sortedRows, s.OrderBy)
	}
	if s.Limit != nil {
		sorted = e.limitUpdateChanges(sorted, s)
	}
	// Keep only the sorted-LIMIT survivors, then reorder them back to
	// natural rowid order (the order they were scanned in).
	if len(sorted) == len(changes) {
		return changes
	}
	keep := make(map[int64]bool, len(sorted))
	for _, c := range sorted {
		keep[c.rowID] = true
	}
	var limited []updateChange
	for _, c := range changes {
		if keep[c.rowID] {
			limited = append(limited, c)
		}
	}
	return limited
}

// matchUpdateRow evaluates the WHERE clause for one target row and, if it
// matches, builds the change. For UPDATE ... FROM the WHERE/SET evaluate
// against the joined row (the target is updated once per matching join row,
// using the first match's SET values).
func (e *DMLExecutor) matchUpdateRow(s *sql.UpdateStmt, cell *storage.Cell, rec *storage.Record, colIndex map[string]int, colDefs []sql.ColumnDef, row RowMap, deferSetEval bool) (*updateChange, RowMap, bool, error) {
	if s.From.Name != "" {
		return e.matchUpdateFromRow(s, cell, rec, colIndex, colDefs, row, deferSetEval)
	}
	match, err := e.rowMatchesWhere(s.Where, row)
	if err != nil {
		return nil, nil, false, err
	}
	if !match {
		return nil, nil, false, nil
	}
	ch, err := e.buildUpdateChange(cell, rec, colIndex, colDefs, s, row, deferSetEval)
	if err != nil {
		return nil, nil, false, err
	}
	return ch, row, true, nil
}

// matchUpdateFromRow matches a target row against the UPDATE ... FROM joined
// rows: each join combination is a candidate, and the first match's SET
// values are used for the row.
func (e *DMLExecutor) matchUpdateFromRow(s *sql.UpdateStmt, cell *storage.Cell, rec *storage.Record, colIndex map[string]int, colDefs []sql.ColumnDef, row RowMap, deferSetEval bool) (*updateChange, RowMap, bool, error) {
	joined, err := e.joinUpdateFromRows(s, row)
	if err != nil {
		return nil, nil, false, err
	}
	for _, jrow := range joined {
		match, err := e.rowMatchesWhere(s.Where, jrow)
		if err != nil {
			return nil, nil, false, err
		}
		if match {
			ch, err := e.buildUpdateChange(cell, rec, colIndex, colDefs, s, jrow, deferSetEval)
			if err != nil {
				return nil, nil, false, err
			}
			return ch, jrow, true, nil
		}
	}
	return nil, nil, false, nil
}

// joinUpdateFromRows reads the UPDATE ... FROM tables and returns combined
// row maps (target row + each FROM table row) for the target row. The
// combined row keys include both the target table's columns and the FROM
// tables' columns (qualified and unqualified).
// joinUpdateFromRows reads the UPDATE ... FROM tables and returns combined
// row maps (target row + each FROM table row) for the target row. The
// combined row keys include both the target table's columns and the FROM
// tables' columns (qualified and unqualified).
// JoinUpdateFromRows builds the combined row maps for an UPDATE ... FROM's
// target row (exposed through DMLContext so the FTS executor can evaluate
// WHERE/SET against the joined columns — fts4upfrom 1.x).
func (e *DMLExecutor) JoinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error) {
	return e.joinUpdateFromRows(s, targetRow)
}

func (e *DMLExecutor) joinUpdateFromRows(s *sql.UpdateStmt, targetRow RowMap) ([]RowMap, error) {
	// Qualify the target row with its own table name so WHERE/SET expressions
	// can reference it by table name (UPDATE t2 SET ... FROM chng WHERE
	// chng.a = t2.a). Without the qualified keys, t2.a does not resolve and
	// the join never matches.
	qualifyUpdateRow(targetRow, s.Table)
	// Join the FROM tables among THEMSELVES (the target row is not a join
	// operand — its columns must stay NULL-free and independent, e.g. a
	// RIGHT JOIN's unmatched rows keep the target's real values). The
	// accumulator starts as a single empty row; the primary FROM table is a
	// comma join (CROSS), and each FromJoins entry applies its own JOIN type
	// (INNER/LEFT/RIGHT/FULL/CROSS) with its ON condition.
	acc := []RowMap{nil}
	if s.From.Name != "" || s.From.Subquery != nil {
		rows, err := e.readUpdateFromTable(s, s.From)
		if err != nil {
			return nil, err
		}
		acc = crossJoinUpdateRows(acc, [][]RowMap{rows})
		if len(acc) == 0 {
			return nil, nil
		}
	}
	for _, jc := range s.FromJoins {
		rightRows, err := e.readUpdateFromTable(s, jc.Table)
		if err != nil {
			return nil, err
		}
		acc = e.applyUpdateFromJoin(jc, acc, rightRows)
	}
	// Merge the target row into every joined row so WHERE/SET can reference
	// the target's columns (unqualified and t5.a qualified). The target wins
	// on unqualified name conflicts (mergeRowMaps keeps the base).
	out := make([]RowMap, 0, len(acc))
	for _, jrow := range acc {
		out = append(out, mergeRowMaps(targetRow, jrow))
	}
	return out, nil
}

// applyUpdateFromJoin applies one UPDATE ... FROM JOIN clause to the
// accumulated rows. NATURAL joins derive the common-column equality from the
// row maps; other joins use the ON condition. Returns the joined rows.
func (e *DMLExecutor) applyUpdateFromJoin(jc sql.JoinClause, acc, rightRows []RowMap) []RowMap {
	joinType := strings.ToUpper(jc.JoinType)
	isNatural := strings.Contains(joinType, "NATURAL")
	if isNatural {
		joinType = strings.TrimSpace(strings.TrimPrefix(joinType, "NATURAL"))
		if joinType == "" {
			joinType = "INNER"
		}
	}
	matchRow := func(left, right RowMap) bool {
		if isNatural {
			return e.naturalJoinRowMatch(left, right)
		}
		return e.joinRowMatchesOn(jc.On, mergeRowMaps(left, right))
	}
	switch joinType {
	case "LEFT":
		return e.updateLeftJoin(acc, rightRows, matchRow)
	case "RIGHT":
		return e.updateRightJoin(acc, rightRows, matchRow)
	case "FULL":
		return e.updateFullJoin(acc, rightRows, matchRow)
	case "CROSS", "INNER", "", "JOIN":
		return e.updateInnerJoin(acc, rightRows, jc.On, matchRow)
	default:
		// Unknown join type: fall back to a cross join so rows still resolve.
		return e.updateInnerJoin(acc, rightRows, nil, func(_, _ RowMap) bool { return true })
	}
}

// updateLeftJoin keeps every left row, matched right rows merged, and
// unmatched left rows with NULL right columns.
func (e *DMLExecutor) updateLeftJoin(acc, rightRows []RowMap, match func(RowMap, RowMap) bool) []RowMap {
	var next []RowMap
	for _, left := range acc {
		matched := false
		for _, right := range rightRows {
			if match(left, right) {
				next = append(next, mergeRowMaps(left, right))
				matched = true
			}
		}
		if !matched {
			next = append(next, left)
		}
	}
	return next
}

// updateRightJoin keeps every right row, matched left rows merged, and
// unmatched right rows with NULL left columns.
func (e *DMLExecutor) updateRightJoin(acc, rightRows []RowMap, match func(RowMap, RowMap) bool) []RowMap {
	var next []RowMap
	for _, right := range rightRows {
		matched := false
		for _, left := range acc {
			if match(left, right) {
				next = append(next, mergeRowMaps(left, right))
				matched = true
			}
		}
		if !matched {
			next = append(next, right)
		}
	}
	return next
}

// updateFullJoin keeps matched rows plus unmatched rows from both sides.
func (e *DMLExecutor) updateFullJoin(acc, rightRows []RowMap, match func(RowMap, RowMap) bool) []RowMap {
	var next []RowMap
	seenLeft := make(map[int]bool, len(acc))
	for _, right := range rightRows {
		matched := false
		for li, left := range acc {
			if match(left, right) {
				next = append(next, mergeRowMaps(left, right))
				matched = true
				seenLeft[li] = true
			}
		}
		if !matched {
			next = append(next, right)
		}
	}
	for li, left := range acc {
		if !seenLeft[li] {
			next = append(next, left)
		}
	}
	return next
}

// updateInnerJoin keeps rows that match the ON condition (or all rows when
// the join has no ON condition / is a CROSS join).
func (e *DMLExecutor) updateInnerJoin(acc, rightRows []RowMap, on sql.Expr, match func(RowMap, RowMap) bool) []RowMap {
	var next []RowMap
	for _, left := range acc {
		for _, right := range rightRows {
			merged := mergeRowMaps(left, right)
			if on == nil || match(left, right) {
				next = append(next, merged)
			}
		}
	}
	return next
}

// naturalJoinRowMatch reports whether two rows match under a NATURAL join:
// every column name present in both (unqualified, excluding rowid aliases)
// has an equal value.
func (e *DMLExecutor) naturalJoinRowMatch(left, right RowMap) bool {
	for k, lv := range left {
		if isUpdateRowIDKey(k) || strings.Contains(k, ".") {
			continue
		}
		rv, ok := right[k]
		if !ok {
			continue
		}
		if util.CompareValues(lv, rv) != 0 {
			return false
		}
	}
	return true
}

// isUpdateRowIDKey reports whether a row-map key is a rowid alias (rowid,
// oid, _rowid_) that NATURAL joins must ignore.
func isUpdateRowIDKey(k string) bool {
	return strings.EqualFold(k, "rowid") || strings.EqualFold(k, "oid") || strings.EqualFold(k, "_rowid_")
}

// joinRowMatchesOn evaluates a JOIN ON condition against a merged row.
func (e *DMLExecutor) joinRowMatchesOn(on sql.Expr, row RowMap) bool {
	if on == nil {
		return true
	}
	match, err := e.ctx.EvalBool(on, row)
	return err == nil && match
}

// readUpdateFromTable reads every row of one UPDATE ... FROM table into row
// maps. Qualified keys (alias.col) are added so SET/WHERE can reference them
// by table name. A reference with no name yields nil rows.
func (e *DMLExecutor) readUpdateFromTable(s *sql.UpdateStmt, ref sql.TableRef) ([]RowMap, error) {
	if ref.Subquery != nil {
		return e.readUpdateFromSubquery(ref)
	}
	if ref.Name == "" {
		return nil, nil
	}
	// A FROM table may be a VIEW (upfrom1-5.1) or a WITH-clause CTE
	// (upfrom2-3.1): materialize its rows.
	if _, _, _, terr := e.resolveUpdateFromTable(s, ref); terr != nil {
		if viewEntry, _, verr := e.ctx.FindView(ref.Name); verr == nil {
			return e.readUpdateFromView(ref, viewEntry)
		}
		if cte, ok := e.ctx.FindCTEByName(ref.Name); ok {
			return e.readUpdateFromCTE(ref, cte)
		}
	}
	entry, colDefs, fromCtx, err := e.resolveUpdateFromTable(s, ref)
	if err != nil {
		return nil, err
	}
	alias := ref.As
	if alias == "" {
		alias = entry.Name
	}
	// An FTS virtual table in the FROM clause has no btree (RootPage 0); its
	// rows come from the in-memory FTS index (fts4upfrom 1.x: UPDATE ft SET
	// b=o.c FROM ft AS o — the FROM alias o scans the same FTS table).
	if ftsTable, ok := e.ctx.FTSTables()[entry.Name]; ok {
		return e.scanUpdateFromFTS(ftsTable, colDefs, alias)
	}
	return e.scanUpdateFromTable(entry, colDefs, fromCtx, alias)
}

// scanUpdateFromFTS reads every row of an FTS virtual table (used as an
// UPDATE ... FROM operand) into row maps qualified with the alias (fts4upfrom
// 1.x). The docid is exposed as rowid/docid; each user column under its name.
func (e *DMLExecutor) scanUpdateFromFTS(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, alias string) ([]RowMap, error) {
	cols := ftsTable.ColumnNames()
	var rows []RowMap
	for _, docID := range ftsTable.AllRowsMap() {
		doc := ftsTable.GetDoc(docID)
		if doc == nil {
			continue
		}
		m := make(RowMap)
		m["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		m["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		m[alias+".rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		for i, cd := range colDefs {
			if i >= len(cols) {
				break
			}
			var v interface{} = nil
			if i < len(doc.Columns) {
				v = doc.Columns[i]
			}
			m[cd.Name] = &util.ColumnValue{Value: v}
			m[alias+"."+cd.Name] = &util.ColumnValue{Value: v}
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// scanUpdateFromTable reads every row of an UPDATE ... FROM table's btree into
// row maps qualified with the table alias.
func (e *DMLExecutor) scanUpdateFromTable(entry *schema.Entry, colDefs []sql.ColumnDef, fromCtx *DatabaseContext, alias string) ([]RowMap, error) {
	tree := e.updateFromTree(entry, fromCtx)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, err
	}
	var rows []RowMap
	for {
		cell, rerr := cursor.ReadCell()
		if rerr != nil || cell == nil {
			break
		}
		rec, derr := storage.DecodeRecord(cell.Payload)
		if derr != nil || rec == nil {
			break
		}
		fm := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		qualifyUpdateRow(fm, alias)
		rows = append(rows, fm)
		ok, nerr := cursor.Next()
		if nerr != nil || !ok {
			break
		}
	}
	return rows, nil
}

// readUpdateFromView materializes an UPDATE ... FROM view's result rows into
// row maps keyed by the view's column names, qualified with the view alias.
func (e *DMLExecutor) readUpdateFromView(ref sql.TableRef, viewEntry *schema.Entry) ([]RowMap, error) {
	viewResult := e.ctx.ExecSelectView(viewEntry)
	if viewResult.Error != nil {
		return nil, viewResult.Error
	}
	viewCols := viewResult.Columns
	if len(viewCols) == 0 {
		return nil, nil
	}
	// A CREATE VIEW with an explicit column list (CREATE VIEW changes(k, v)
	// AS VALUES(...)) declares the output names; the SELECT body's
	// auto-generated names (column1/column2 for VALUES) must be replaced so
	// WHERE/SET can reference k/v (fts4upfrom 1.x).
	declared := execquery.ViewDeclaredColumns(viewEntry.SQL)
	if len(declared) == len(viewCols) {
		viewCols = declared
	}
	alias := ref.As
	if alias == "" {
		alias = viewEntry.Name
	}
	var rows []RowMap
	for _, rowVals := range viewResult.Rows {
		fm := make(RowMap, len(viewCols))
		for i, c := range viewCols {
			if i < len(rowVals) {
				fm[c] = rowVals[i]
			}
		}
		qualifyUpdateRow(fm, alias)
		rows = append(rows, fm)
	}
	return rows, nil
}

// readUpdateFromCTE materializes an UPDATE ... FROM CTE's result rows into
// row maps keyed by the CTE's column names, qualified with the CTE name.
func (e *DMLExecutor) readUpdateFromCTE(ref sql.TableRef, cte sql.CTEDef) ([]RowMap, error) {
	if cte.Select == nil {
		return nil, nil
	}
	cteResult := e.ctx.ExecSelect(cte.Select)
	if cteResult.Error != nil {
		return nil, cteResult.Error
	}
	cols := cteResult.Columns
	// Apply the CTE's declared column list (WITH input(k,v) AS ...).
	if len(cte.Columns) > 0 {
		cols = cte.Columns
	}
	if len(cols) == 0 {
		return nil, nil
	}
	alias := ref.As
	if alias == "" {
		alias = cte.Name
	}
	var rows []RowMap
	for _, rowVals := range cteResult.Rows {
		fm := make(RowMap, len(cols))
		for i, c := range cols {
			if i < len(rowVals) {
				fm[c] = rowVals[i]
			}
		}
		qualifyUpdateRow(fm, alias)
		rows = append(rows, fm)
	}
	return rows, nil
}

// readUpdateFromSubquery materializes an UPDATE ... FROM subquery's result
// rows into row maps keyed by the subquery's output column names, qualified
// with the subquery alias (upfrom4-520).
func (e *DMLExecutor) readUpdateFromSubquery(ref sql.TableRef) ([]RowMap, error) {
	if ref.Subquery == nil {
		return nil, nil
	}
	subResult := e.ctx.ExecSelect(ref.Subquery)
	if subResult.Error != nil {
		return nil, subResult.Error
	}
	cols := subResult.Columns
	if len(cols) == 0 {
		return nil, nil
	}
	alias := ref.As
	if alias == "" {
		alias = "_subq"
	}
	var rows []RowMap
	for _, rowVals := range subResult.Rows {
		fm := make(RowMap, len(cols))
		for i, c := range cols {
			if i < len(rowVals) {
				fm[c] = rowVals[i]
			}
		}
		qualifyUpdateRow(fm, alias)
		rows = append(rows, fm)
	}
	return rows, nil
}

// resolveUpdateFromTable resolves one UPDATE ... FROM table reference in the
// modified table's context first (a trigger body's unqualified references
// resolve in the trigger's schema, e.g. an aux trigger's FROM mmm → aux.mmm),
// then falls back to the normal temp/main-first resolution.
func (e *DMLExecutor) resolveUpdateFromTable(s *sql.UpdateStmt, ref sql.TableRef) (*schema.Entry, []sql.ColumnDef, *DatabaseContext, error) {
	var entry *schema.Entry
	var err error
	if e.currentDMLCtx != nil && !strings.Contains(ref.Name, ".") {
		if ent, cerr := e.currentDMLCtx.Schema.FindTable(ref.Name); cerr == nil {
			entry = ent
		}
	}
	if entry == nil {
		entry, _, err = e.ctx.FindTable(ref.Name)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	var fromCtx *DatabaseContext
	if !strings.Contains(ref.Name, ".") && e.currentDMLCtx != nil {
		fromCtx = e.currentDMLCtx
	} else {
		_, fc, ferr := e.ctx.FindTable(ref.Name)
		if ferr == nil {
			fromCtx = fc
		}
	}
	return entry, colDefs, fromCtx, nil
}

// updateFromTree builds the btree for an UPDATE ... FROM table, using the
// resolved context's pager when known.
