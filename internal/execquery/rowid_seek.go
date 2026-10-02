package execquery

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/value"
)

// Seek-driven SELECT (src/where.c "SEARCH ... USING INTEGER PRIMARY KEY"):
// a single-table SELECT whose WHERE conjuncts pin the rowid — equality or
// literal range (rowid_range.go) — reads the candidates through direct
// b-tree seeks instead of scanning. The full WHERE clause is still evaluated
// on every candidate row, so the result set equals the scan's — a row missed
// by the seek bounds would also fail a rowid conjunct and could never match
// the whole AND. The gate mirrors the DML seek (internal/execdml/seek.go);
// the two packages cannot share code (layered opposite directions).

// selectRowidSeekRows resolves a rowid-pinned SELECT to its rows through the
// equality or range seek. handled=false falls back to the full scan: any
// gate miss, seek anomaly, or evaluation error (the scan re-evaluates and
// surfaces it identically).
func (e *SelectEngine) selectRowidSeekRows(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, tree *btree.BTree, feed *simpleAggFeed) (allRows [][]interface{}, allRowMaps []RowMap, handled bool) {
	a := e.selectRowidSeekPlan(s, tableEntry, colDefs)
	if a == nil || !a.planned {
		return nil, nil, false
	}
	needMaps := SelectNeedsRowMaps(e, s, tableEntry.Name)
	if !a.eq {
		return e.selectRowidRangeRows(s, tree, colDefs, a, needMaps, feed)
	}
	if !a.eqMatch {
		return [][]interface{}{}, nil, true
	}
	cursor, srow, found, ok := e.fetchSeekStructRow(s, tree, a.eqRowid, colDefs, needMaps)
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
	// Feed mode: step the single candidate row; the result comes from the
	// statement's feed (finishSimpleAggFeed in execRealTableSelect).
	if feed != nil {
		if err := feed.step(srow.Values, srow.RowID); err != nil {
			return nil, nil, false // the scan fallback re-evaluates and surfaces it
		}
		return nil, nil, true
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
	colIndex := e.seekColIndexFor(colDefs)
	srow = e.structRowFromRecord(rec.Values, len(rec.Values), colDefs, realRowID, affinityCols, colIndex)
	return cursor, srow, true, true
}

// seekColIndexFor returns the column-name → slot index for colDefs, memoized
// per SelectEngine and guarded by the schema fingerprint (DDL invalidates it)
// plus the colDefs slice identity — the same pattern the DML executor's
// columnIndexFor uses. The hot point-SELECT path rebuilt this identical map
// on every statement; the built map is read-only afterwards (StructRow.Get
// and the range iterator only look up), so sharing it across statements is
// safe.
func (e *SelectEngine) seekColIndexFor(colDefs []sql.ColumnDef) map[string]int {
	if len(colDefs) == 0 {
		return buildSeekColIndex(colDefs)
	}
	fp := uint64(0)
	if sm := e.ctx.Schema(); sm != nil {
		fp = sm.SchemaFingerprint()
	}
	if e.seekCICache != nil && e.seekCIFingerprint == fp && e.seekCIDefs == &colDefs[0] && e.seekCILen == len(colDefs) {
		return e.seekCICache
	}
	m := buildSeekColIndex(colDefs)
	e.seekCIFingerprint, e.seekCIDefs, e.seekCILen, e.seekCICache = fp, &colDefs[0], len(colDefs), m
	return m
}

// buildSeekColIndex builds the column-name → slot index the seek path's
// StructRow.Index uses (declared-name keys; StructRow.Get falls back to a
// case-insensitive scan). Purely a function of colDefs: range iteration
// builds it once for the whole loop.
func buildSeekColIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	return colIndex
}

// structRowFromRecord builds a seek-path StructRow through the same per-row
// pipeline the table scan applies (fillStructRowFromTypes): dropped-column
// re-alignment, ALTER TABLE ADD COLUMN defaults, affinity wrappers on the
// referenced columns, and the INTEGER PRIMARY KEY rowid-alias substitution
// (the alias is stored as NULL in the record; both the alias seek's WHERE
// re-check and the output read the alias value from here).
func (e *SelectEngine) structRowFromRecord(values []interface{}, valueCount int, colDefs []sql.ColumnDef, rowID int64, affinityCols map[string]bool, colIndex map[string]int) *StructRow {
	return e.seekStructRowPhaseOne(values, valueCount, colDefs, rowID, affinityCols, colIndex, ipkAliasIndices(colDefs))
}

// seekStructRowPhaseOne assembles a phase-1 seek-path StructRow: pad short
// records to the declared width, re-align dropped columns, apply added-column
// defaults, wrap the decoded columns' values (skipping stored NULLs exactly
// like the scan's affinityPlan.apply), and substitute the rowid into the
// INTEGER PRIMARY KEY rowid-alias columns.
func (e *SelectEngine) seekStructRowPhaseOne(values []interface{}, valueCount int, colDefs []sql.ColumnDef, rowID int64, affinityCols map[string]bool, colIndex map[string]int, ipkIdx []int) *StructRow {
	// Rows written before ALTER TABLE ADD COLUMN store fewer values than the
	// table now declares: pad to the declared width so every colDefs slot
	// exists (the scan path allocates the full width up front).
	if len(values) < len(colDefs) {
		padded := make([]interface{}, len(colDefs))
		copy(padded, values)
		values = padded
	}
	srow := &StructRow{Index: colIndex}
	e.fillSeekRowPhaseOne(values, valueCount, srow, colDefs, rowID, affinityWrapIndices(colDefs, affinityCols), ipkIdx)
	return srow
}

// fillSeekRowPhaseOne applies seekStructRowPhaseOne's pipeline IN PLACE to a
// reused StructRow and decode buffer (the range loop allocates neither per
// row): dropped-column re-alignment, added-column defaults, the precomputed
// affinity wrap indices' wrappers (skipping stored NULLs exactly like the
// scan's affinityPlan.apply), and the INTEGER PRIMARY KEY rowid-alias
// substitution.
func (e *SelectEngine) fillSeekRowPhaseOne(values []interface{}, valueCount int, srow *StructRow, colDefs []sql.ColumnDef, rowID int64, affWrapIdx []int, ipkIdx []int) {
	shiftDroppedColumns(values, colDefs)
	e.applyColumnDefaults(values, colDefs, valueCount)
	srow.Values = values
	srow.RowID = rowID
	for _, i := range affWrapIdx {
		if values[i] != nil {
			srow.Values[i] = wrapValueForRowMap(values[i], colDefs[i])
		}
	}
	for _, i := range ipkIdx {
		if srow.Values[i] == nil {
			srow.Values[i] = wrapAffinityCollated(colDefs[i], rowID)
		}
	}
}

// selectRowidSeekPlan runs the eligibility checks and extracts the shared
// rowid-seek plan (equality pin or literal range bounds). A nil result keeps
// the scan.
func (e *SelectEngine) selectRowidSeekPlan(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *rowidSeekAnalysis {
	if s.Where == nil || tableEntry == nil {
		return nil
	}
	// Single real table only: joins, FROM subqueries, views, INDEXED BY, and
	// system tables keep the scan (an INDEXED BY clause forces the named
	// plan; schema tables have post-scan filtering the seek path bypasses).
	if len(s.Joins) > 0 || s.From.Name == "" || s.From.Subquery != nil ||
		s.From.IndexedBy != "" || s.From.EmptyName || IsSchemaTable(tableEntry.Name) {
		return nil
	}
	if e.ctx.HasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil
	}
	return analyzeRowidSeek(s.Where, tableEntry.Name, s.From.As, colDefs)
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

// rowidEqualitySides matches an equality conjunct whose rowid-side reference
// (either operand order, "=" or "==") designates the table's rowid. Returns
// the literal side when matched.
func rowidEqualitySides(bin *sql.BinaryOp, tableName, alias string, colDefs []sql.ColumnDef) (sql.Expr, bool) {
	for _, sides := range [2][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
		ref, ok := unwrapParenExpr(sides[0]).(*sql.ColumnRef)
		if !ok || !isRowidSeekRef(ref, tableName, alias, colDefs) {
			continue
		}
		return unwrapParenExpr(sides[1]), true
	}
	return nil, false
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

// rowidFromNumericText applies the rowid column's numeric affinity to text
// through value.NumericText (SQLite's applyNumericAffinity full-string
// rule): leading/trailing whitespace is skipped and a well-formed number
// converts — a pure integer pins its rowid EXACTLY (int64, no float
// rounding near 2^63), an integral real pins the same rowid, and anything
// else ('5000abc', '5000.0abc', ”, '0x10') never equals an integer rowid.
func rowidFromNumericText(text string) (int64, bool, bool) {
	kind, iv, fv := value.NumericText(text)
	switch kind {
	case value.IntNumeric:
		return iv, true, true
	case value.RealNumeric:
		return integralRowid(fv)
	default:
		return 0, false, true
	}
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
