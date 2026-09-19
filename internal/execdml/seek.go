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
		if n == math.Trunc(n) && n >= -9.223372036854776e18 && n < 9.223372036854776e18 {
			return int64(n), true, true
		}
		return 0, false, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false, true // non-numeric text never equals an integer rowid
		}
		if f == math.Trunc(f) && f >= -9.223372036854776e18 && f < 9.223372036854776e18 {
			return int64(f), true, true
		}
		return 0, false, true
	case nil:
		return 0, false, true // rowid = NULL matches nothing
	}
	return 0, false, false
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

// scanIndexCandidates walks the driving index's entries, collecting the
// rowids whose first key value matches the probe (see seekCandidateRowIDs).
func (e *DMLExecutor) scanIndexCandidates(plan *dmlSeekPlan) (rowIDs []int64, ok bool) {
	idxTree := btree.NewBTree(plan.index.Ctx.Pager, plan.index.RootPage, false)
	cursor, err := idxTree.OpenCursor()
	if err != nil {
		return nil, false
	}
	rowIDs, ok = walkIndexForCandidates(cursor, newDMLKeyProbes(plan), plan)
	if !ok {
		return nil, false
	}
	// The table scan visits rows in rowid order; sort the candidates so the
	// trigger/preupdate/LIMIT order is unchanged (index byte order does not
	// imply rowid order).
	sortInt64Ascending(rowIDs)
	return rowIDs, true
}

// walkIndexForCandidates iterates the index entries, collecting the rowids of
// entries whose first key value matches the probe.
func walkIndexForCandidates(cursor *btree.Cursor, probeKeys []dmlKeyProbe, plan *dmlSeekPlan) ([]int64, bool) {
	var rowIDs []int64
	seen := make(map[int64]bool)
	for {
		payload, _, err := cursor.ReadCellData()
		if err != nil {
			return nil, false
		}
		if dmlPayloadKeyMatches(payload, probeKeys) {
			var ok bool
			rowIDs, ok = dmlAppendCandidate(payload, plan, seen, rowIDs)
			if !ok {
				return nil, false
			}
		}
		next, err := cursor.Next()
		if err != nil {
			return nil, false
		}
		if !next {
			break
		}
	}
	return rowIDs, true
}

// dmlAppendCandidate decodes one index entry and, when its first key value
// matches the probe, appends the entry's rowid. ok=false signals an
// undecodable payload (the caller falls back to the full scan).
func dmlAppendCandidate(payload []byte, plan *dmlSeekPlan, seen map[int64]bool, rowIDs []int64) ([]int64, bool) {
	rid, matched, scanOK := dmlMatchIndexPayload(payload, plan, seen)
	if !scanOK {
		return nil, false
	}
	if matched {
		rowIDs = append(rowIDs, rid)
	}
	return rowIDs, true
}

// dmlProbeMatches reports whether an index key value matches either probe
// candidate (affinity-applied or raw constant). The full WHERE re-evaluation
// remains the exact filter; this only needs to be a superset.
func dmlProbeMatches(keyVal interface{}, probe [2]interface{}) bool {
	k := util.UnwrapColumnValue(keyVal)
	if k == nil {
		return false
	}
	for _, p := range probe {
		if p == nil {
			continue
		}
		if util.CompareValues(k, execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(p))) == 0 {
			return true
		}
	}
	return false
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

// dmlKeyProbe is the record-element encoding of an index probe value: the
// serial type varint and body bytes a stored key with the same value must
// carry (record element encodings are canonical per value).
type dmlKeyProbe struct {
	st   uint64
	body []byte
	ok   bool
}

// newDMLKeyProbe encodes one value into a single-element record and extracts
// its element encoding.
func newDMLKeyProbe(v interface{}) dmlKeyProbe {
	payload, err := storage.EncodeRecord([]interface{}{v})
	if err != nil {
		return dmlKeyProbe{}
	}
	hdrSize, n1 := util.GetVarint(payload)
	if n1 == 0 || int(hdrSize) > len(payload) {
		return dmlKeyProbe{}
	}
	st, n2 := util.GetVarint(payload[n1:])
	if n2 == 0 {
		return dmlKeyProbe{}
	}
	keyLen, err := storage.SerialTypeLength(st)
	if err != nil {
		return dmlKeyProbe{}
	}
	bodyStart := int(hdrSize)
	if bodyStart+int(keyLen) != len(payload) {
		return dmlKeyProbe{}
	}
	return dmlKeyProbe{st: st, body: payload[bodyStart:], ok: true}
}

// dmlPayloadKeyMatches reports whether an index cell payload's first element
// encoding matches any probe candidate (a superset of value equality).
func dmlPayloadKeyMatches(payload []byte, probes []dmlKeyProbe) bool {
	hdrSize, n1 := util.GetVarint(payload)
	if n1 == 0 {
		return true // undecodable shape: let the full decode decide
	}
	st, n2 := util.GetVarint(payload[n1:])
	if n2 == 0 {
		return true
	}
	keyLen, err := storage.SerialTypeLength(st)
	if err != nil {
		return true
	}
	bodyStart := int(hdrSize)
	if bodyStart+int(keyLen) > len(payload) {
		return true
	}
	for _, kp := range probes {
		if kp.st == st && len(kp.body) == int(keyLen) && string(kp.body) == string(payload[bodyStart:bodyStart+int(keyLen)]) {
			return true
		}
	}
	return false
}

// dmlMatchIndexPayload decodes one index entry and reports its rowid when the
// first key value matches the probe. scanOK=false signals an unreadable
// payload (the caller falls back to the full scan); matched=false with
// scanOK=true is a plain miss (rid is 0).
func dmlMatchIndexPayload(payload []byte, plan *dmlSeekPlan, seen map[int64]bool) (rid int64, matched, scanOK bool) {
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil || len(rec.Values) < 2 {
		return 0, false, false
	}
	if !dmlProbeMatches(rec.Values[0], plan.probe) {
		return 0, false, true
	}
	if id, ok := util.UnwrapColumnValue(rec.Values[len(rec.Values)-1]).(int64); ok && !seen[id] {
		seen[id] = true
		return id, true, true
	}
	return 0, false, true
}

// newDMLKeyProbes builds the byte-level prefilter probes for a plan: equal
// values always produce the same record element encoding (serial type + body
// bytes), so an entry whose first element's encoding differs from BOTH probe
// candidates cannot match and is skipped without decoding the record.
func newDMLKeyProbes(plan *dmlSeekPlan) []dmlKeyProbe {
	probeKeys := make([]dmlKeyProbe, 0, 2)
	for _, p := range plan.probe {
		if p == nil {
			continue
		}
		if kp := newDMLKeyProbe(execexpr.UnwrapCollatedValue(util.UnwrapColumnValue(p))); kp.ok {
			probeKeys = append(probeKeys, kp)
		}
	}
	return probeKeys
}
