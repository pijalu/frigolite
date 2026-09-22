package execdml

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// Seek-driven row collection for UPDATE/DELETE (src/where.c point-lookup
// decisioning): a WHERE clause whose conjuncts include an equality on the
// rowid or on the leading column of an index pins the candidate rows, so the
// collection phase visits only those rows instead of scanning the whole table
// (SQLite plans these as "SEARCH ... USING INTEGER PRIMARY KEY (rowid=?)" /
// "USING INDEX ..."). The full WHERE clause is still evaluated on every
// candidate row, so the fast path is a pure narrowing of the candidate set —
// any row it might miss would also fail the equality conjunct and could never
// match the whole AND.

// dmlSeekPlan is a resolved point-lookup plan for one UPDATE/DELETE row
// collection. Exactly one of the fields applies.
type dmlSeekPlan struct {
	// empty marks a WHERE whose equality conjunct provably matches no row
	// (e.g. rowid=5.5, col='abc' on an INTEGER-affinity comparison path):
	// collection returns zero rows.
	empty bool
	// rowid is the pinned rowid when index is nil (rowid = <const>).
	rowid int64
	// index is the driving index when non-nil (col = <const> on the index's
	// leading column); probe holds the comparison candidates for the key
	// value: the column-affinity-applied constant first, the raw constant
	// second (a stored key matching either is a candidate; the full WHERE
	// re-evaluation decides).
	index *indexDef
	probe [2]interface{}
}

// planDMLSeek analyzes a DELETE/UPDATE WHERE clause for a seekable point
// lookup. It returns nil when the statement does not qualify and the caller
// must keep the full table scan. scanName is the effective WHERE qualifier
// (the alias for "UPDATE t AS x"); owning is the table's database context
// (nil falls back to the all-databases index lookup).
func (e *DMLExecutor) planDMLSeek(tableEntry *schema.Entry, colDefs []sql.ColumnDef, where sql.Expr, scanName string, owning *DatabaseContext) *dmlSeekPlan {
	if where == nil || tableEntry == nil {
		return nil
	}
	// WITHOUT ROWID tables are iterated in PRIMARY KEY order (trigger/preupdate
	// order); candidate narrowing through an ordinary index would reorder the
	// collection. Keep the keyed scan there.
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		return nil
	}
	outerCols := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		outerCols[cd.Name] = true
	}
	colIndex := buildColumnIndex(colDefs)
	rowidTable := !execquery.RowHasRowIDColumn(colDefs)
	for _, conj := range splitAndTerms(where) {
		col, val, aff, ok := e.extractEquality(conj, outerCols)
		if !ok || !aff || !dmlQualifierMatches(col, conj, scanName) {
			continue
		}
		// Rowid equality: the pinned row is fetched by a direct b-tree seek.
		if isRowIDName(col) {
			if !rowidTable {
				continue
			}
			return dmlRowidSeekPlan(val)
		}
		plan := e.dmlIndexedSeekPlan(tableEntry, colDefs, colIndex, col, val, owning)
		if plan != nil {
			return plan
		}
	}
	return nil
}

// dmlRowidSeekPlan resolves a `rowid = <const>` conjunct to a plan.
func dmlRowidSeekPlan(val interface{}) *dmlSeekPlan {
	rowid, matches, planned := dmlRowidConst(val)
	if !planned {
		return nil // unhandled constant shape: keep the scan
	}
	if !matches {
		return &dmlSeekPlan{empty: true}
	}
	return &dmlSeekPlan{rowid: rowid}
}

// dmlIndexedSeekPlan resolves a `col = <const>` conjunct to an index-driven
// plan. It returns nil when the conjunct does not qualify (the caller keeps
// scanning for other candidates).
func (e *DMLExecutor) dmlIndexedSeekPlan(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, col string, val interface{}, owning *DatabaseContext) *dmlSeekPlan {
	ci, ok := colIndex[strings.ToLower(col)]
	if !ok || ci < 0 || ci >= len(colDefs) {
		return nil
	}
	if val == nil {
		// col = NULL matches nothing (NULL comparison is UNKNOWN).
		return &dmlSeekPlan{empty: true}
	}
	// An INTEGER PRIMARY KEY column IS the rowid (rowid-alias): the
	// equality pins the btree key exactly like rowid = <const>.
	if isIPKRowidAliasCol(colDefs[ci]) {
		return dmlRowidSeekPlan(val)
	}
	def := e.seekIndexFor(tableEntry.Name, col, ci, colDefs, owning)
	if def == nil {
		return nil
	}
	typ := colDefs[ci].Type
	return &dmlSeekPlan{
		index: def,
		probe: [2]interface{}{
			util.ApplyColumnAffinity(util.UnwrapColumnValue(val), typ),
			util.UnwrapColumnValue(val),
		},
	}
}

// dmlQualifierMatches reports whether an equality conjunct's column reference
// is unqualified or qualified by the effective scan name (the table or its
// alias); a foreign qualifier (a FROM-joined table) does not constrain this
// table's rows.
func dmlQualifierMatches(col string, conj sql.Expr, scanName string) bool {
	bin, ok := unwrapParen(conj).(*sql.BinaryOp)
	if !ok {
		return false
	}
	for _, side := range []sql.Expr{bin.Left, bin.Right} {
		if cr, ok := unwrapParen(side).(*sql.ColumnRef); ok && strings.EqualFold(cr.Name, col) {
			if cr.Table == "" || strings.EqualFold(cr.Table, scanName) {
				return true
			}
		}
	}
	return false
}

// isRowIDName reports whether a column reference names the rowid pseudo-column.
func isRowIDName(lower string) bool {
	switch strings.ToLower(lower) {
	case "rowid", "_rowid_", "oid":
		return true
	}
	return false
}

// dmlRowidConst converts an equality constant to the pinned rowid. It returns
// planned=false for value shapes the conversion does not handle (the caller
// keeps the scan), matches=false when no rowid can equal the constant
// (non-integral numbers, non-numeric text, blobs, NULL — SQLite's affinity
// rules make the equality provably false against an integer rowid).
func dmlRowidConst(v interface{}) (rowid int64, matches bool, planned bool) {
	switch n := execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(v)).(type) {
	case int64:
		return n, true, true
	case int:
		return int64(n), true, true
	case float64:
		if inRowidRange(n) {
			return int64(n), true, true
		}
		return 0, false, true
	case string:
		return dmlRowidConstFromText(n)
	case nil:
		return 0, false, true // rowid = NULL matches nothing
	}
	return 0, false, false
}

// inRowidRange reports whether an integral float lies in the int64 rowid
// range.
func inRowidRange(f float64) bool {
	return f == math.Trunc(f) && f >= -9.223372036854776e18 && f < 9.223372036854776e18
}

// dmlRowidConstFromText converts a text constant to a matching rowid.
// Non-numeric or non-integral text never equals an integer rowid.
func dmlRowidConstFromText(n string) (rowid int64, matches bool, planned bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
	if err != nil {
		return 0, false, true // non-numeric text never equals an integer rowid
	}
	if inRowidRange(f) {
		return int64(f), true, true
	}
	return 0, false, true
}

// seekIndexFor finds the index driving a col = <const> candidate lookup: a
// full (non-partial) index on the table whose FIRST key is the plain column,
// resolving through the owning database context when known (a same-named
// table in an attached database must not borrow another schema's index).
// Only BINARY-collated keys qualify: the engine's index b-trees are stored in
// serial-type byte order, so the candidate scan compares decoded key VALUES;
// a NOCASE/RTRIM key could hold case variants the value comparison would
// miss, so those keep the full scan.
func (e *DMLExecutor) seekIndexFor(tableName, col string, colIdx int, colDefs []sql.ColumnDef, owning *DatabaseContext) *indexDef {
	if cdColl := strings.ToUpper(colDefs[colIdx].Collate); cdColl != "" && cdColl != "BINARY" {
		return nil
	}
	ctxs := []*DatabaseContext{owning}
	if owning == nil {
		for _, dbCtx := range e.ctx.Databases() {
			ctxs = append(ctxs, dbCtx)
		}
	}
	for _, ctx := range ctxs {
		if ctx == nil {
			continue
		}
		for _, def := range e.indexDefsIn(ctx, tableName) {
			if dmlIndexProbeEligible(def, col) {
				d := def
				return &d
			}
		}
	}
	return nil
}

// dmlIndexProbeEligible reports whether an index can drive a col = <const>
// candidate scan: full (non-partial), plain unqualified leading key column,
// BINARY collation.
func dmlIndexProbeEligible(def indexDef, col string) bool {
	if def.Where != "" || len(def.Cols) == 0 {
		return false
	}
	if !strings.EqualFold(def.Cols[0], col) {
		return false
	}
	// Expression or qualified first keys cannot be probed by value.
	if strings.ContainsAny(def.Cols[0], "(.") {
		return false
	}
	coll := seekKeyCollation(def.SQL, 0)
	return coll == "" || coll == "BINARY"
}

// seekKeyCollation returns the explicit COLLATE of the idx-th key of a CREATE
// INDEX statement ("" when none).
func seekKeyCollation(indexSQL string, idx int) string {
	colText := indexColumnListText(indexSQL)
	if colText == "" {
		return ""
	}
	colls := parseIndexKeyCollations(colText)
	if idx < len(colls) {
		return strings.ToUpper(colls[idx])
	}
	return ""
}

// seekCandidateRowIDs resolves a plan to the ascending list of candidate
// rowids. ok=false means the candidate lookup itself failed (unreadable index
// pages, unexpected payload shapes) and the caller must fall back to the
// full scan.
func (e *DMLExecutor) seekCandidateRowIDs(tableName string, rootPage uint32, plan *dmlSeekPlan) (rowIDs []int64, ok bool) {
	if plan.empty {
		return nil, true
	}
	if plan.index == nil {
		return []int64{plan.rowid}, true
	}
	return e.scanIndexCandidates(plan)
}

// scanIndexCandidates resolves the plan's probe keys through the driving
// index's b-tree (P9.PERF.T3): each probe value becomes a
// btree.UnpackedIndexKey under the index's KeyInfo and btree.IndexKeyRowIDs
// collects candidate rowids with record-format comparisons — value-order
// semantics, so an INTEGER probe also matches a stored REAL with the same
// value (the old byte-encoding prefilter required equal encodings and could
// miss that pair). The walk is order-agnostic (stored byte order scatters
// value-equal entries), and any comparator/page error falls back to the
// full scan via ok=false.
func (e *DMLExecutor) scanIndexCandidates(plan *dmlSeekPlan) (rowIDs []int64, ok bool) {
	idxTree := btree.NewBTree(plan.index.Ctx.Pager, plan.index.RootPage, false)
	seen := make(map[int64]bool)
	for _, probe := range dmlIndexSeekProbes(plan) {
		ids, err := idxTree.IndexKeyRowIDs(probe)
		if err != nil {
			return nil, false
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				rowIDs = append(rowIDs, id)
			}
		}
	}
	// The table scan visits rows in rowid order; sort the candidates so the
	// trigger/preupdate/LIMIT order is unchanged (index byte order does not
	// imply rowid order).
	sortInt64Ascending(rowIDs)
	return rowIDs, true
}

// dmlIndexSeekProbes builds the record-format probe keys for a plan: the
// affinity-applied constant first, the raw constant second (deduplicated
// when the two compare equal). The KeyInfo carries the index's leading-key
// collation (BINARY for every currently eligible index — see
// dmlIndexProbeEligible) and ASC sort order.
func dmlIndexSeekProbes(plan *dmlSeekPlan) []*btree.UnpackedIndexKey {
	ki := btree.NewKeyInfo(1, []string{seekKeyCollation(plan.index.SQL, 0)}, nil)
	var probes []*btree.UnpackedIndexKey
	for _, p := range plan.probe {
		if p == nil {
			continue
		}
		v := execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(p))
		if len(probes) == 1 && util.CompareValues(probes[0].Values[0], v) == 0 {
			continue // the affinity-applied and raw constants are value-equal
		}
		probes = append(probes, btree.NewUnpackedIndexKey(ki, []interface{}{v}))
	}
	return probes
}

func sortInt64Ascending(v []int64) {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
}

// fetchSeekRow reads one candidate row by rowid through a direct b-tree seek.
// Returns (nil, false, nil) when the rowid is absent, and a non-nil error
// when the tree read fails (caller falls back to the scan).
func (e *DMLExecutor) fetchSeekRow(tree *btree.BTree, tableName string, rootPage uint32, createSQL string, colDefs []sql.ColumnDef, rowID int64) (RowMap, bool, error) {
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, false, err
	}
	found, err := cursor.SeekToRowID(rowID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	payload, realRowID, err := cursor.ReadCellData()
	if err != nil {
		return nil, false, err
	}
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil {
		return nil, false, nil
	}
	e.ctx.RemapWRRecordToDeclared(rec, createSQL, colDefs)
	row := e.ctx.BuildRowMap(rec, colDefs, realRowID)
	row[trueRowidKey] = realRowID
	return row, true, nil
}
