package execquery

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// Seek-driven single-row SELECT (src/where.c "SEARCH ... USING INTEGER
// PRIMARY KEY (rowid=?)"): a single-table SELECT whose WHERE conjuncts pin
// the rowid to a constant reads that one row through a direct b-tree seek
// instead of scanning. The full WHERE clause is still evaluated on the
// candidate row, so the result set equals the scan's — a row missed by the
// seek would also fail the rowid= conjunct and could never match the whole
// AND. The gate mirrors the DML seek (internal/execdml/seek.go); the two
// packages cannot share code (layered opposite directions).

// selectRowidSeekRows resolves a rowid-pinned SELECT to its (0 or 1) rows.
// handled=false falls back to the full scan: any gate miss, seek anomaly, or
// evaluation error (the scan re-evaluates and surfaces it identically).
func (e *SelectEngine) selectRowidSeekRows(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, tree *btree.BTree) (allRows [][]interface{}, allRowMaps []RowMap, handled bool) {
	rowid, matches, planned := e.selectRowidSeekGate(s, tableEntry, colDefs)
	if !planned {
		return nil, nil, false
	}
	needMaps := SelectNeedsRowMaps(e, s, tableEntry.Name)
	if !matches {
		return [][]interface{}{}, nil, true
	}
	cursor, srow, found, ok := e.fetchSeekStructRow(s, tree, rowid, colDefs, needMaps)
	if !ok {
		return nil, nil, false
	}
	if !found {
		return [][]interface{}{}, nil, true
	}
	pass, err := e.RowPassesWhere(s.Where, srow, cursor)
	if err != nil {
		return nil, nil, false // the scan fallback re-evaluates and surfaces it
	}
	if !pass {
		return [][]interface{}{}, nil, true
	}
	return e.seekRowOutput(s, colDefs, srow, true, needMaps)
}

// fetchSeekStructRow seeks the pinned row and builds its affinity-wrapped
// StructRow. found=false with ok=true means the rowid is absent (empty
// result); ok=false falls back to the scan.
func (e *SelectEngine) fetchSeekStructRow(s *sql.SelectStmt, tree *btree.BTree, rowid int64, colDefs []sql.ColumnDef, needMaps bool) (cursor *btree.Cursor, srow *StructRow, found, ok bool) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil, false, false
	}
	found, err = cursor.SeekToRowID(rowid)
	if err != nil {
		return nil, nil, false, false
	}
	if !found {
		return cursor, nil, false, true
	}
	payload, realRowID, err := cursor.ReadCellData()
	if err != nil {
		return nil, nil, false, false
	}
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil {
		return nil, nil, false, false
	}
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	srow = e.structRowFromRecord(rec.Values, len(rec.Values), colDefs, realRowID, affinityCols)
	return cursor, srow, true, true
}

// structRowFromRecord builds a seek-path StructRow through the same per-row
// pipeline the table scan applies (fillStructRowFromTypes): dropped-column
// re-alignment, ALTER TABLE ADD COLUMN defaults, affinity wrappers on the
// referenced columns, and the INTEGER PRIMARY KEY rowid-alias substitution
// (the alias is stored as NULL in the record; both the alias seek's WHERE
// re-check and the output read the alias value from here).
func (e *SelectEngine) structRowFromRecord(values []interface{}, valueCount int, colDefs []sql.ColumnDef, rowID int64, affinityCols map[string]bool) *StructRow {
	// Rows written before ALTER TABLE ADD COLUMN store fewer values than the
	// table now declares: pad to the declared width so every colDefs slot
	// exists (the scan path allocates the full width up front).
	if len(values) < len(colDefs) {
		padded := make([]interface{}, len(colDefs))
		copy(padded, values)
		values = padded
	}
	shiftDroppedColumns(values, colDefs)
	e.applyColumnDefaults(values, colDefs, valueCount)
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	srow := &StructRow{Values: values, Index: colIndex, RowID: rowID}
	// Wrap the referenced columns, skipping stored NULLs exactly like the
	// scan's affinityPlan.apply — the INTEGER PRIMARY KEY rowid-alias column
	// is stored as NULL and must reach the substitution below unwrapped.
	if affinityCols != nil {
		for i := range colDefs {
			if affinityCols[strings.ToLower(colDefs[i].Name)] && values[i] != nil {
				srow.Values[i] = wrapValueForRowMap(values[i], colDefs[i])
			}
		}
	}
	for _, i := range ipkAliasIndices(colDefs) {
		if srow.Values[i] == nil {
			srow.Values[i] = wrapAffinityCollated(colDefs[i], rowID)
		}
	}
	return srow
}

// selectRowidSeekGate runs the eligibility checks and extracts the pinned
// rowid. planned=false keeps the scan.
func (e *SelectEngine) selectRowidSeekGate(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (rowid int64, matches bool, planned bool) {
	if s.Where == nil || tableEntry == nil {
		return 0, false, false
	}
	// Single real table only: joins, FROM subqueries, views, INDEXED BY, and
	// system tables keep the scan (an INDEXED BY clause forces the named
	// plan; schema tables have post-scan filtering the seek path bypasses).
	if len(s.Joins) > 0 || s.From.Name == "" || s.From.Subquery != nil ||
		s.From.IndexedBy != "" || s.From.EmptyName || IsSchemaTable(tableEntry.Name) {
		return 0, false, false
	}
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return 0, false, false
	}
	return selectRowidSeekConst(s.Where, tableEntry.Name, s.From.As, colDefs)
}

// seekRowOutput builds the single row's output (SELECT * flat path or
// buildOutputRow projection) plus its row map when needed.
func (e *SelectEngine) seekRowOutput(s *sql.SelectStmt, colDefs []sql.ColumnDef, srow *StructRow, affinity, needMaps bool) ([][]interface{}, []RowMap, bool) {
	if len(s.Columns) == 1 {
		if ref, ok := s.Columns[0].Expr.(*sql.ColumnRef); ok && ref.Name == "*" && ref.Table == "" {
			star := appendScanStarValues(nil, colDefs, srow.Values, affinity)
			rows := [][]interface{}{star}
			var maps []RowMap
			if needMaps {
				maps = []RowMap{StructRowToMap(srow)}
			}
			return rows, maps, true
		}
	}
	row, err := e.buildOutputRow(s.Columns, colDefs, srow)
	if err != nil {
		return nil, nil, false
	}
	rows := [][]interface{}{row}
	var maps []RowMap
	if needMaps {
		maps = []RowMap{StructRowToMap(srow)}
	}
	return rows, maps, true
}

// selectRowidSeekConst extracts a rowid-pinning constant from the WHERE
// clause: an AND conjunct "rowid = <literal>" — where the rowid reference is
// either the rowid/_rowid_/oid pseudo-column or the table's INTEGER PRIMARY
// KEY rowid-alias column (isRowidSeekRef) — with either operand order,
// optionally table/alias qualified, or wrapped in a unary +/-. Returns
// planned=false when no such conjunct exists or the constant is not a
// literal (subqueries, functions, column references keep the scan),
// matches=false when the constant provably equals no rowid under SQLite's
// affinity rules (non-integral numbers, non-numeric text, blobs, NULL).
func selectRowidSeekConst(where sql.Expr, tableName, alias string, colDefs []sql.ColumnDef) (rowid int64, matches bool, planned bool) {
	for _, conj := range splitAnd(where) {
		bin, ok := unwrapParenExpr(conj).(*sql.BinaryOp)
		if !ok || (bin.Operator != "=" && bin.Operator != "==") {
			continue
		}
		for _, sides := range [2][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
			ref, ok := unwrapParenExpr(sides[0]).(*sql.ColumnRef)
			if !ok || !isRowidSeekRef(ref, tableName, alias, colDefs) {
				continue
			}
			return selectRowidLiteral(unwrapParenExpr(sides[1]))
		}
	}
	return 0, false, false
}

// isRowidSeekRef reports whether a column reference designates the table's
// rowid for seek planning: the rowid/_rowid_/oid pseudo-column — blocked
// when a declared column of the same name shadows it (RowHasRowIDColumn) —
// or the INTEGER PRIMARY KEY rowid-alias column, qualified by the table
// name or its FROM alias. The same predicate drives the SELECT seek
// executor and the EXPLAIN QUERY PLAN renderer so the two cannot diverge
// (the alias keeps seeking under a shadow: intpkey tables with a declared
// rowid column still SEARCH by the alias, only the pseudo-column scans).
func isRowidSeekRef(ref *sql.ColumnRef, tableName, alias string, colDefs []sql.ColumnDef) bool {
	if ref.Table != "" && !strings.EqualFold(ref.Table, tableName) &&
		(alias == "" || !strings.EqualFold(ref.Table, alias)) {
		return false
	}
	if IsRowIDName(ref.Name) {
		return !RowHasRowIDColumn(colDefs)
	}
	if cd, ok := findColDefByName(colDefs, ref.Name); ok {
		return isIPKRowidAliasCol(cd)
	}
	return false
}

// selectRowidLiteral converts a literal expression to the pinned rowid.
func selectRowidLiteral(expr sql.Expr) (rowid int64, matches bool, planned bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		if i, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			return i, true, true
		}
		f, err := strconv.ParseFloat(v.Value, 64)
		if err != nil {
			return 0, false, true
		}
		return integralRowid(f)
	case *sql.StringLit:
		return rowidFromNumericText(v.Value)
	case *sql.NullLit:
		return 0, false, true // rowid = NULL matches nothing
	case *sql.UnaryOp:
		return signedRowidLiteral(v)
	case *sql.BlobLit:
		return 0, false, true // blob > integer: never equal
	}
	return 0, false, false
}

// signedRowidLiteral interprets a +/- signed numeric literal as a rowid
// comparison target.
func signedRowidLiteral(v *sql.UnaryOp) (int64, bool, bool) {
	if v.Operator != "-" && v.Operator != "+" {
		return 0, false, false
	}
	inner := unwrapParenExpr(v.Operand)
	num, ok := inner.(*sql.NumericLit)
	if !ok {
		return 0, false, false
	}
	if i, err := strconv.ParseInt(v.Operator+num.Value, 10, 64); err == nil {
		return i, true, true
	}
	f, ferr := strconv.ParseFloat(v.Operator+num.Value, 64)
	if ferr != nil {
		return 0, false, true
	}
	return integralRowid(f)
}

// rowidFromNumericText applies the rowid column's numeric affinity to text:
// well-formed numbers convert (integral ones pin a rowid); anything else
// never equals an integer rowid.
func rowidFromNumericText(text string) (int64, bool, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, false, true
	}
	return integralRowid(f)
}

// integralRowid maps a numeric constant to a rowid: only integral values
// within int64 range can equal an integer rowid.
func integralRowid(f float64) (int64, bool, bool) {
	if f == math.Trunc(f) && f >= -9.223372036854776e18 && f < 9.223372036854776e18 {
		return int64(f), true, true
	}
	return 0, false, true
}

// unwrapParenExpr peels parentheses from an expression node.
func unwrapParenExpr(expr sql.Expr) sql.Expr {
	for {
		if p, ok := expr.(*sql.ParenExpr); ok {
			expr = p.Expr
			continue
		}
		return expr
	}
}
