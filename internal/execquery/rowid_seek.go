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
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	srow = &StructRow{Values: rec.Values, Index: colIndex, RowID: realRowID}
	if affinityCols != nil {
		for i := range colDefs {
			if affinityCols[strings.ToLower(colDefs[i].Name)] {
				srow.Values[i] = wrapValueForRowMap(rec.Values[i], colDefs[i])
			}
		}
	}
	return cursor, srow, true, true
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
	// A declared column named rowid/_rowid_/oid shadows the pseudo-column.
	if RowHasRowIDColumn(colDefs) {
		return 0, false, false
	}
	return selectRowidSeekConst(s.Where, tableEntry.Name, s.From.As)
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
// clause: an AND conjunct "rowid = <literal>" (either side, optionally
// table/alias qualified, or wrapped in a unary +/-). Returns planned=false
// when no such conjunct exists or the constant is not a literal (subqueries,
// functions, column references keep the scan), matches=false when the
// constant provably equals no rowid under SQLite's affinity rules
// (non-integral numbers, non-numeric text, blobs, NULL).
func selectRowidSeekConst(where sql.Expr, tableName, alias string) (rowid int64, matches bool, planned bool) {
	for _, conj := range splitAnd(where) {
		bin, ok := unwrapParenExpr(conj).(*sql.BinaryOp)
		if !ok || bin.Operator != "=" {
			continue
		}
		for _, sides := range [2][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
			ref, ok := unwrapParenExpr(sides[0]).(*sql.ColumnRef)
			if !ok || !IsRowIDName(ref.Name) {
				continue
			}
			if ref.Table != "" && !strings.EqualFold(ref.Table, tableName) &&
				(alias == "" || !strings.EqualFold(ref.Table, alias)) {
				continue
			}
			return selectRowidLiteral(unwrapParenExpr(sides[1]))
		}
	}
	return 0, false, false
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
		if v.Operator != "-" && v.Operator != "+" {
			return 0, false, false
		}
		inner := unwrapParenExpr(v.Operand)
		num, ok := inner.(*sql.NumericLit)
		if !ok {
			return 0, false, false
		}
		i, err := strconv.ParseInt(v.Operator+num.Value, 10, 64)
		if err == nil {
			return i, true, true
		}
		f, ferr := strconv.ParseFloat(v.Operator+num.Value, 64)
		if ferr != nil {
			return 0, false, true
		}
		return integralRowid(f)
	case *sql.BlobLit:
		return 0, false, true // blob > integer: never equal
	}
	return 0, false, false
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
