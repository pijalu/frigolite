package execquery

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// Rowid range seek (src/where.c "SEARCH ... USING INTEGER PRIMARY KEY
// (rowid>? AND rowid<?)"): a single-table SELECT whose WHERE conjuncts bound
// the rowid with literal comparisons (BETWEEN, <, >, <=, >=) seeks the range
// start and iterates forward to the exclusive end, evaluating the full WHERE
// per candidate row — the same contract as the equality seek. Only literal
// bounds are planned; any ambiguous shape (OR, subquery/expression bounds,
// non-literal sides) returns planned=false and keeps the scan. The bound
// resolvers mirror SQLite's rowid affinity (vdbemem.c): numbers compare
// numerically (non-integral bounds ceil/floor-normalize onto integer rowids),
// and a text/blob bound the INTEGER affinity cannot convert sorts entirely
// above every integer — a lower bound then matches nothing, an upper bound
// matches everything (verified against the sqlite3 CLI).

// rowidSeekAnalysis is the shared rowid-seek plan extracted from a WHERE
// clause, consumed by both the seek executor (selectRowidSeekRows) and the
// EXPLAIN QUERY PLAN renderer so the plan text and the executed plan cannot
// diverge.
type rowidSeekAnalysis struct {
	eq      bool  // an equality conjunct dominates (rendered "rowid=?")
	eqRowid int64 // the pinned rowid when eq
	eqMatch bool  // false: the equality constant provably equals no rowid
	hasLo   bool  // a lower-range conjunct was seen (rendered "rowid>?")
	hasHi   bool  // an upper-range conjunct was seen (rendered "rowid<?")
	lo, hi  int64 // effective inclusive bounds (loSet/hiSet)
	loSet   bool
	hiSet   bool
	empty   bool // a bound provably matches no row (NULL, text/blob lower, NaN)
	planned bool // false: the shape keeps the full scan
}

// analyzeRowidSeek extracts the rowid seek plan from a WHERE clause: the
// first rowid equality conjunct decides (dominating any ranges); otherwise
// every rowid range conjunct contributes a
// bound. A nil result keeps the scan: no rowid conjunct, or a range conjunct
// whose bound is not a literal (subquery, function, column reference).
func analyzeRowidSeek(where sql.Expr, tableName, alias string, colDefs []sql.ColumnDef) *rowidSeekAnalysis {
	a := &rowidSeekAnalysis{planned: true}
	rangeBad := false
	for _, conj := range splitAnd(where) {
		done, bad := analyzeRowidConjunct(a, unwrapParenExpr(conj), tableName, alias, colDefs)
		if bad {
			rangeBad = true
		}
		if done {
			break
		}
	}
	if a.eq {
		return a // the equality's literal resolution owns planned
	}
	if rangeBad || (!a.hasLo && !a.hasHi) {
		return nil
	}
	return a
}

// analyzeRowidConjunct folds one WHERE conjunct into the analysis. done
// reports the analysis is final (an equality conjunct was found); bad
// reports a range conjunct with a non-literal bound (scan fallback).
func analyzeRowidConjunct(a *rowidSeekAnalysis, conj sql.Expr, tableName, alias string, colDefs []sql.ColumnDef) (done, bad bool) {
	switch c := conj.(type) {
	case *sql.BinaryOp:
		switch c.Operator {
		case "=", "==":
			lit, ok := rowidEqualitySides(c, tableName, alias, colDefs)
			if ok {
				a.eqRowid, a.eqMatch, a.planned = selectRowidLiteral(lit)
				a.eq = true
				return true, false
			}
		case "<", ">", "<=", ">=":
			return false, analyzeRowidRangeBinary(a, c, tableName, alias, colDefs)
		}
	case *sql.Between:
		if c.Negated {
			return false, false // NOT BETWEEN keeps the scan
		}
		return false, analyzeRowidBetween(a, c, tableName, alias, colDefs)
	}
	return false, false
}

// analyzeRowidBetween folds "rowid BETWEEN low AND high" into an inclusive
// lower and upper bound (where.c's AND-pair decomposition of TK_BETWEEN).
func analyzeRowidBetween(a *rowidSeekAnalysis, c *sql.Between, tableName, alias string, colDefs []sql.ColumnDef) bool {
	ref, ok := unwrapParenExpr(c.Operand).(*sql.ColumnRef)
	if !ok || !isRowidSeekRef(ref, tableName, alias, colDefs) {
		return false
	}
	low, okL := resolveRangeBound(c.Low, ">=")
	high, okH := resolveRangeBound(c.High, "<=")
	if !okL || !okH {
		return true // non-literal bound: the scan keeps the conjunct
	}
	applyRangeBound(a, true, low)
	applyRangeBound(a, false, high)
	return false
}

// analyzeRowidRangeBinary folds one rowid comparison conjunct into a bound.
// Either operand order is accepted (5 < rowid ≡ rowid > 5). Returns bad=true
// when the comparison side opposite the rowid is not a literal.
func analyzeRowidRangeBinary(a *rowidSeekAnalysis, bin *sql.BinaryOp, tableName, alias string, colDefs []sql.ColumnDef) bool {
	for i, sides := range [2][2]sql.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
		ref, ok := unwrapParenExpr(sides[0]).(*sql.ColumnRef)
		if !ok || !isRowidSeekRef(ref, tableName, alias, colDefs) {
			continue
		}
		op := bin.Operator
		if i == 1 { // rowid on the right: the operator flips
			op = flipRangeOp(op)
		}
		b, ok := resolveRangeBound(sides[1], op)
		if !ok {
			return true
		}
		applyRangeBound(a, op == ">" || op == ">=", b)
		return false
	}
	return false
}

// flipRangeOp mirrors a comparison operator across its operands.
func flipRangeOp(op string) string {
	switch op {
	case "<":
		return ">"
	case ">":
		return "<"
	case "<=":
		return ">="
	case ">=":
		return "<="
	}
	return op
}

// rangeBound is one resolved rowid range bound.
type rangeBound struct {
	never  bool  // the bound provably matches no rowid (empty result)
	always bool  // the bound matches every rowid (no numeric restriction)
	val    int64 // inclusive integral bound (valid unless never/always)
}

// resolveRangeBound resolves a literal range bound under the given (already
// operand-normalized) operator. ok=false marks a non-literal bound —
// subqueries, functions, column references, non-numeric unary operands —
// which keeps the full scan.
func resolveRangeBound(expr sql.Expr, op string) (rangeBound, bool) {
	switch v := unwrapParenExpr(expr).(type) {
	case *sql.NumericLit:
		return numericRangeBound(v.Value, op)
	case *sql.StringLit:
		return textRangeBound(v.Value, op)
	case *sql.NullLit:
		// rowid CMP NULL is NULL for every row: the conjunct matches nothing.
		return rangeBound{never: true}, true
	case *sql.BlobLit:
		return nonNumericRangeBound(op), true
	case *sql.UnaryOp:
		if v.Operator != "-" && v.Operator != "+" {
			return rangeBound{}, false
		}
		num, ok := unwrapParenExpr(v.Operand).(*sql.NumericLit)
		if !ok {
			return rangeBound{}, false
		}
		return numericRangeBound(v.Operator+num.Value, op)
	default:
		return rangeBound{}, false
	}
}

// numericRangeBound resolves a numeric literal bound onto integer rowids.
func numericRangeBound(text, op string) (rangeBound, bool) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return rangeBound{}, false
	}
	return floatRangeBound(f, op)
}

// textRangeBound applies the rowid column's numeric affinity to a text
// bound: exactly well-formed numbers compare numerically, anything else
// sorts above every integer rowid. No whitespace trimming: the bounds must
// never be wider than the engine's own affinity conversion — the WHERE
// re-check on every candidate row uses that conversion, and a wider bound
// would plan rows the re-check then discards (or miss rows it keeps).
func textRangeBound(text, op string) (rangeBound, bool) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nonNumericRangeBound(op), true
	}
	return floatRangeBound(f, op)
}

// nonNumericRangeBound classifies a text/blob bound the INTEGER affinity
// cannot convert: every integer sorts before every text/blob value, so a
// lower bound matches nothing and an upper bound matches everything.
func nonNumericRangeBound(op string) rangeBound {
	if op == ">" || op == ">=" {
		return rangeBound{never: true}
	}
	return rangeBound{always: true}
}

// floatRangeBound normalizes a numeric bound onto the inclusive integer
// rowid range: > b starts at floor(b)+1, >= b at ceil(b), < b ends at
// ceil(b)-1, <= b at floor(b). Bounds beyond the int64 range collapse to
// never/always.
func floatRangeBound(f float64, op string) (rangeBound, bool) {
	if math.IsNaN(f) {
		return rangeBound{never: true}, true
	}
	lower := op == ">" || op == ">="
	if f >= i64MaxF {
		// No rowid lies at or above 2^63: a lower bound there matches
		// nothing, an upper bound everything.
		if lower {
			return rangeBound{never: true}, true
		}
		return rangeBound{always: true}, true
	}
	if f < i64MinF {
		// Below -2^63 the mirror: a lower bound always, an upper never.
		if lower {
			return rangeBound{always: true}, true
		}
		return rangeBound{never: true}, true
	}
	if lower {
		return lowerRangeBound(f, op), true
	}
	return upperRangeBound(f, op), true
}

// i64MaxF and i64MinF are ±2^63, the int64 range edges as float64.
const (
	i64MaxF = 9223372036854775808.0
	i64MinF = -9223372036854775808.0
)

// lowerRangeBound resolves an in-range lower bound (">" exclusive, ">="
// inclusive) to its inclusive integer start.
func lowerRangeBound(f float64, op string) rangeBound {
	if op == ">=" {
		f = math.Ceil(f)
		if f >= i64MaxF {
			return rangeBound{never: true}
		}
		return rangeBound{val: int64(f)}
	}
	return rangeBound{val: int64(math.Floor(f)) + 1}
}

// upperRangeBound resolves an in-range upper bound ("<" exclusive, "<="
// inclusive) to its inclusive integer end.
func upperRangeBound(f float64, op string) rangeBound {
	if op == "<=" {
		return rangeBound{val: int64(math.Floor(f))}
	}
	f = math.Ceil(f)
	if f <= i64MinF {
		return rangeBound{never: true}
	}
	return rangeBound{val: int64(f) - 1}
}

// applyRangeBound folds a resolved bound into the analysis: never bounds
// empty the plan, always bounds only contribute the rendered constraint,
// numeric bounds tighten (lower: max, upper: min).
func applyRangeBound(a *rowidSeekAnalysis, lower bool, b rangeBound) {
	if lower {
		a.hasLo = true
		switch {
		case b.never:
			a.empty = true
		case b.always:
		case !a.loSet || b.val > a.lo:
			a.lo, a.loSet = b.val, true
		}
		return
	}
	a.hasHi = true
	switch {
	case b.never:
		a.empty = true
	case b.always:
	case !a.hiSet || b.val < a.hi:
		a.hi, a.hiSet = b.val, true
	}
}

// rowidSeekConstraints renders the planner constraint text the way SQLite
// renders an INTEGER PRIMARY KEY seek: "(rowid=?)" for the equality, the
// lower bound before the upper bound otherwise (rowid>=X normalizes to
// "rowid>?", rowid<=Y to "rowid<?" — the integer-key normalization of
// whereLoopToString, verified against the sqlite3 CLI).
func rowidSeekConstraints(a *rowidSeekAnalysis) string {
	if a.eq {
		return "(rowid=?)"
	}
	var parts []string
	if a.hasLo {
		parts = append(parts, "rowid>?")
	}
	if a.hasHi {
		parts = append(parts, "rowid<?")
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// rangeSeekRow is the per-iteration state of the range seek loop: the
// prepared decode plan plus the accumulated output. The decode buffer and
// StructRow are allocated once and reused across steps (the scan's
// reuseSRow discipline): every consumer either copies the values it keeps
// (output rows, row maps, aggregate steps) or evaluates them before the next
// row decodes.
type rangeSeekRow struct {
	e        *SelectEngine
	s        *sql.SelectStmt
	cursor   *btree.Cursor
	colDefs  []sql.ColumnDef
	colIndex map[string]int
	whereIdx map[int]bool
	restIdx  map[int]bool
	ipkIdx   []int
	needMaps bool
	// affWrapIdx lists the column indices the affinity wrapping loop covers
	// (precomputed from the affinity map; replaces a per-row ToLower walk).
	affWrapIdx []int
	// feed, when non-nil, is the statement's simple-aggregate feed: passing
	// rows step it instead of decoding the remaining columns and
	// materializing output rows / row maps.
	feed *simpleAggFeed

	srow   *StructRow
	values []interface{}
	// serialTypes is the iterator's reusable record-header type buffer
	// (parseRecordSerialTypesInto); consumed within each row's decode.
	serialTypes []uint64

	rows [][]interface{}
	maps []RowMap
}

// selectRowidRangeRows resolves a rowid-range SELECT by seeking the range
// start and iterating forward past the inclusive end, applying the full
// WHERE to every candidate. Rows go through the scan's lazy two-phase decode
// (fillStructRowFromTypes): phase 1 decodes only the WHERE-referenced
// columns, phase 2 refills the rest for rows that pass. handled=false falls
// back to the full scan (seek or evaluation anomaly); the returned rows are
// exactly the scan's.
func (e *SelectEngine) selectRowidRangeRows(s *sql.SelectStmt, tree *btree.BTree, colDefs []sql.ColumnDef, a *rowidSeekAnalysis, needMaps bool, feed *simpleAggFeed) ([][]interface{}, []RowMap, bool) {
	if a.empty {
		return [][]interface{}{}, nil, true
	}
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil, false
	}
	if a.loSet {
		// SeekToRowID positions at lo when present, else at the first larger
		// rowid (the insertion point) — the inclusive range start either way.
		if _, err := cursor.SeekToRowID(a.lo); err != nil {
			return nil, nil, false
		}
	}
	// Feed mode produces no rows or row maps: the statement's aggregate
	// result is built from the feed after the loop.
	if feed != nil {
		needMaps = false
	}
	affinityCols := e.scanTableAffinityCols(s, colDefs, needMaps)
	colIndex := buildSeekColIndex(colDefs)
	whereIdx, restIdx := scanLazyDecodeIndices(colDefs, colIndex, affinityCols)
	// Feed mode reads raw values for the aggregate steps: wrap only the
	// WHERE-referenced columns (their wrappers feed the WHERE evaluation).
	wrapCols := affinityCols
	if feed != nil {
		wrapCols = e.whereReferencedAffinityCols(s.Where)
	}
	it := &rangeSeekRow{
		e:          e,
		s:          s,
		cursor:     cursor,
		colDefs:    colDefs,
		colIndex:   colIndex,
		whereIdx:   whereIdx,
		restIdx:    restIdx,
		ipkIdx:     ipkAliasIndices(colDefs),
		needMaps:   needMaps,
		affWrapIdx: affinityWrapIndices(colDefs, wrapCols),
		feed:       feed,
		values:     make([]interface{}, len(colDefs)),
		srow:       &StructRow{Index: colIndex},
	}
	if !it.run(a) {
		return nil, nil, false
	}
	// PRAGMA reverse_unordered_selects reverses the rowid range walk like it
	// reverses the plain scan (select_scan's shouldReverse: top-level,
	// no ORDER BY; where.c WHERE_REVERSE applies to the rowid loop too).
	if it.e.ctx.ReverseUnordered() && len(s.OrderBy) == 0 && it.e.selectDepth == 1 {
		reverseInterfaces(it.rows)
		reverseRowMaps(it.maps)
	}
	return it.rows, it.maps, true
}

// affinityWrapIndices lists the column indices whose values receive the
// affinity/collation wrapper (seekStructRowPhaseOne's per-column
// strings.ToLower(colDefs[i].Name) walk, hoisted out of the row loop).
func affinityWrapIndices(colDefs []sql.ColumnDef, affinityCols map[string]bool) []int {
	if affinityCols == nil {
		return nil
	}
	var idx []int
	for i := range colDefs {
		if affinityCols[strings.ToLower(colDefs[i].Name)] {
			idx = append(idx, i)
		}
	}
	return idx
}

// whereReferencedAffinityCols collects the affinity set restricted to the
// WHERE clause's references (the same collector semantics the full scan
// affinity walk applies to s.Where, including subquery bodies). Feed mode
// wraps only these: the WHERE evaluation consumes the wrappers, the feed
// steps raw values.
func (e *SelectEngine) whereReferencedAffinityCols(where sql.Expr) map[string]bool {
	a := &affinityCollector{cols: make(map[string]bool)}
	a.collectExprRefs(where)
	if !a.seen {
		return nil
	}
	return a.cols
}

// aggFeedWrapCols is the scan's feed-mode wrapping set: the WHERE-referenced
// columns plus the INTEGER PRIMARY KEY rowid-alias columns. The alias fill is
// a VALUE substitution (the record stores NULL for the alias; readers
// substitute the rowid — btree.c IPK semantics), and the scan's affinity plan
// is what performs it on the full-decode (no-WHERE) path, so the alias columns
// must stay in the plan even though nothing wraps them for comparison.
func (e *SelectEngine) aggFeedWrapCols(where sql.Expr, colDefs []sql.ColumnDef) map[string]bool {
	cols := e.whereReferencedAffinityCols(where)
	if cols == nil {
		cols = make(map[string]bool)
	}
	for i := range colDefs {
		if isIPKRowidAliasCol(colDefs[i]) {
			cols[colDefs[i].Name] = true
		}
	}
	return cols
}

// run iterates the seeked range, emitting every row that passes the full
// WHERE. ok=false falls back to the scan (a read or evaluation anomaly the
// scan re-evaluates and surfaces identically).
func (it *rangeSeekRow) run(a *rowidSeekAnalysis) bool {
	for !it.cursor.AtEnd() {
		payload, rowID, err := it.cursor.ReadCellData()
		if err != nil {
			return it.cursor.AtEnd() // advanced off the end between rows: clean EOF
		}
		if a.hiSet && rowID > a.hi {
			return true
		}
		done, ok := it.step(payload, rowID)
		if !ok || done {
			return ok
		}
	}
	return true
}

// step processes the current row: phase-1 decode, WHERE re-check, then either
// a simple-aggregate step (feed mode — no row materialization) or the
// phase-2 refill of the passing row plus its output. done=true reports
// iteration finished cleanly (Next ran past the last entry). ok=false falls
// back to the scan.
func (it *rangeSeekRow) step(payload []byte, rowID int64) (done, ok bool) {
	if !it.decodePhaseOne(payload, rowID) {
		return false, false
	}
	pass, err := it.e.RowPassesWhere(it.s.Where, it.srow, it.cursor)
	if err != nil {
		return false, false // the scan fallback re-evaluates and surfaces it
	}
	if pass {
		switch {
		case it.feed != nil:
			// The feed reads only phase-1-decoded columns (every statement
			// reference is in the WHERE-referenced index set); no refill, no
			// output rows or row maps.
			if err := it.feed.step(it.srow.Values, rowID); err != nil {
				return false, false // the scan fallback re-evaluates and surfaces it
			}
		case !it.refill(it.srow, payload) || !it.emit(it.srow):
			return false, false
		}
	}
	more, err := it.cursor.Next()
	if err != nil {
		return false, false
	}
	return !more, true
}

// decodePhaseOne decodes one table-leaf cell's WHERE-referenced columns into
// the REUSED phase-1 StructRow (the scan's fillStructRowFromTypes pipeline:
// dropped-column re-alignment, ALTER TABLE ADD COLUMN defaults, affinity
// wrappers on the decoded columns, INTEGER PRIMARY KEY rowid-alias
// substitution). All slots are cleared first, so a record shorter than the
// declared width leaves the tail slots nil exactly like a fresh buffer.
func (it *rangeSeekRow) decodePhaseOne(payload []byte, rowID int64) bool {
	var err error
	var dataStart int
	it.serialTypes, dataStart, err = parseRecordSerialTypesInto(payload, it.serialTypes[:0])
	if err != nil {
		return false
	}
	values := it.values
	for i := range values {
		values[i] = nil
	}
	storage.DecodeRecordValuesFromTypes(payload, dataStart, values, it.serialTypes, it.whereIdx)
	it.e.fillSeekRowPhaseOne(values, len(it.serialTypes), it.srow, it.colDefs, rowID, it.affWrapIdx, it.ipkIdx)
	return true
}

// refill decodes the remaining (not WHERE-referenced) columns of a row that
// passed the WHERE — the scan's fillStructRowRemainingFromTypes —
// re-applying defaults and the rowid-alias substitution the second decode
// re-read as stored NULL. ok=false marks a corrupt record (scan fallback).
func (it *rangeSeekRow) refill(srow *StructRow, payload []byte) bool {
	var err error
	var dataStart int
	it.serialTypes, dataStart, err = parseRecordSerialTypesInto(payload, it.serialTypes[:0])
	if err != nil {
		return false
	}
	storage.DecodeRecordValuesFromTypes(payload, dataStart, srow.Values, it.serialTypes, it.restIdx)
	it.e.applyColumnDefaults(srow.Values, it.colDefs, len(it.serialTypes))
	for _, i := range it.ipkIdx {
		if srow.Values[i] == nil {
			srow.Values[i] = wrapAffinityCollated(it.colDefs[i], srow.RowID)
		}
	}
	return true
}

// emit appends one passing row's output (and row map when needed).
func (it *rangeSeekRow) emit(srow *StructRow) bool {
	out, outMaps, ok := it.e.seekRowOutput(it.s, it.colDefs, srow, true, it.needMaps)
	if !ok {
		return false
	}
	it.rows = append(it.rows, out[0])
	if it.needMaps {
		it.maps = append(it.maps, outMaps[0])
	}
	return true
}
