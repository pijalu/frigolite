// Package exec implements query execution.
//
// This file holds index-analysis helpers for EXPLAIN QUERY PLAN: resolving
// which index (if any) can satisfy a column reference, expression, or column
// set, and whether an index covers a set of columns. The query-planning
// functions that consume these lookups live in explain_plan.go.
package execquery

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexWhereRe captures the partial-index predicate after the column list.
// Mirrors execdml.IndexWhereRe without importing that package (which would
// create an import cycle since execdml imports execquery).
var indexWhereRe = regexp.MustCompile(`(?is)\)\s*WHERE\s+(.+)$`)

// emptyIndexName is the planner's found-token for an index whose schema name
// is the empty string — CREATE INDEX "" ON ... is legal SQLite
// (tkt-78e04e52ea). Index-lookup helpers signal "no index" with "", which
// would make an empty-named index permanently invisible to every chooser.
// SQLite's C implementation is immune because build.c keeps Index objects on
// a linked list and "not found" is a NULL pointer, never a name comparison;
// the Go planners mirror that distinction by returning this token instead.
// The NUL byte cannot occur in a schema identifier (C-string names), so the
// token never collides with a real index. Renderers and resolve-by-name
// helpers translate the token back with indexSchemaName.
const emptyIndexName = "\x00"

// indexLookupToken maps a found index's schema name to its planner token:
// the empty name becomes the emptyIndexName found-token.
func indexLookupToken(name string) string {
	if name == "" {
		return emptyIndexName
	}
	return name
}

// indexSchemaName maps a planner index token back to its schema name (the
// found-token becomes the real zero-length name).
func indexSchemaName(token string) string {
	if token == emptyIndexName {
		return ""
	}
	return token
}

// findIndexOnCols returns the name of an index on the table whose leading
// columns match the given column list in order (a prefix match), or "" when
// no index qualifies. Used to decide whether ORDER BY / GROUP BY can use an
// index scan instead of a temp b-tree.
func (e *SelectEngine) findIndexOnCols(tableName string, cols []string) string {
	return e.findIndexOnColsForQuery(tableName, cols, nil)
}

// stat1EqualitySelectivity estimates equality range from sqlite_stat1 avg-equal counts.
func (e *SelectEngine) stat1EqualitySelectivity(ref indexedRef) (float64, bool) {
	tokens := e.stat1Tokens(ref.indexName)
	cols := e.indexColumns(ref.indexName)
	pos := indexColPos(cols, ref.colName)
	if pos >= len(tokens)-1 || tokens[0] <= 0 || tokens[pos+1] <= 0 {
		return 0, false
	}
	fraction := float64(tokens[pos+1]) / float64(tokens[0])
	return fraction, true
}

func (e *SelectEngine) indexIsUnordered(indexName string) bool {
	unordered := false
	e.forEachStat1Row(func(rec *storage.Record) bool {
		if len(rec.Values) < 3 || !strings.EqualFold(stat4Text(rec.Values[1]), indexName) {
			return true
		}
		stat, _ := rec.Values[2].(string)
		for _, token := range strings.Fields(stat) {
			if strings.EqualFold(token, "unordered") {
				unordered = true
				return false
			}
		}
		return true
	})
	return unordered
}

func (e *SelectEngine) stat4RangeSelectivity(ref indexedRef, refs []indexedRef, tableName string) (float64, bool, error) {
	if _, err := e.ctx.Schema().FindTable("sqlite_stat4"); err != nil {
		return 0, false, nil
	}
	if ref.constantExpr != nil {
		value, err := e.ctx.EvalExpr(ref.constantExpr, nil)
		if err != nil {
			return 0, false, err
		}
		ref.constant = value
	}
	oneSided := ref.op == ">" || ref.op == ">=" || ref.op == "<" || ref.op == "<="
	if ref.op != "BETWEEN" && !oneSided {
		return 0, false, nil
	}
	if ref.op == "BETWEEN" && (ref.rangeLow == nil || ref.rangeHigh == nil) {
		return 0, false, nil
	}
	if oneSided && ref.constant == nil {
		return 0, false, nil
	}
	cols := e.indexColumns(ref.indexName)
	pos := indexColPos(cols, ref.colName)
	if pos >= len(cols) {
		return 0, false, nil
	}
	prefix := make([]interface{}, pos)
	for i := 0; i < pos; i++ {
		found := false
		for _, candidate := range refs {
			if candidate.indexName == ref.indexName && strings.EqualFold(candidate.colName, cols[i]) && isEqualitySeekOp(candidate.op) {
				prefix[i], found = candidate.constant, true
				break
			}
		}
		if !found {
			return 0, false, nil
		}
	}
	table, err := e.ctx.Schema().FindTable("sqlite_stat4")
	if err != nil {
		return 0, false, nil
	}
	tree := e.ctx.TableBTree("sqlite_stat4", table.RootPage, true)
	defer tree.Close()
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, false, nil
	}
	defer cursor.Close()
	var samples []stat4PlannerSample
	for {
		cell, readErr := cursor.ReadCell()
		if readErr != nil || cell == nil {
			break
		}
		record, decodeErr := storage.DecodeRecord(cell.Payload)
		if decodeErr == nil && len(record.Values) >= 6 && strings.EqualFold(stat4Text(record.Values[1]), ref.indexName) {
			sampleBytes, sampleOK := record.Values[5].([]byte)
			neq, neqOK := parseStat4Counts(record.Values[2])
			nlt, nltOK := parseStat4Counts(record.Values[3])
			if sampleOK && neqOK && nltOK {
				if sampleRecord, sampleErr := storage.DecodeRecord(sampleBytes); sampleErr == nil && len(sampleRecord.Values) > pos {
					samples = append(samples, stat4PlannerSample{values: sampleRecord.Values, neq: neq, nlt: nlt})
				}
			}
		}
		ok, nextErr := cursor.Next()
		if nextErr != nil || !ok {
			break
		}
	}
	total := int64(0)
	if pos == 0 {
		stats := e.stat1Tokens(ref.indexName)
		if len(stats) > 0 {
			total = stats[0]
		}
	} else if len(samples) > 0 && len(samples[0].neq) > pos-1 {
		total = samples[0].neq[pos-1]
	}
	var fraction float64
	var ok bool
	if ref.op == "BETWEEN" {
		fraction, ok = stat4RangeFraction(samples, prefix, pos, ref.rangeLow, ref.rangeHigh, total)
	} else {
		above := ref.op == ">" || ref.op == ">="
		fraction, ok = stat4OneSidedFraction(samples, prefix, pos, ref.constant, total, above)
	}
	return fraction, ok, nil
}

type stat4PlannerSample struct {
	values []interface{}
	neq    []int64
	nlt    []int64
}

func stat4Text(value interface{}) string {
	switch text := value.(type) {
	case string:
		return text
	case []byte:
		return string(text)
	default:
		return fmt.Sprint(value)
	}
}

func parseStat4Counts(value interface{}) ([]int64, bool) {
	text := stat4Text(value)
	fields := strings.Fields(text)
	counts := make([]int64, len(fields))
	for i, field := range fields {
		count, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, false
		}
		counts[i] = count
	}
	return counts, len(counts) > 0
}

func stat4RangeFraction(samples []stat4PlannerSample, prefix []interface{}, col int, low, high interface{}, total int64) (float64, bool) {
	matching, base, ok := stat4MatchingSamples(samples, prefix, col)
	if !ok {
		return 0, false
	}
	if total <= 0 {
		return 0, false
	}
	at := func(bound interface{}) float64 {
		return stat4BoundRow(matching, col, base, bound, total)
	}
	lo, hi := at(low), at(high)
	if hi < lo {
		return 0, false
	}
	fraction := (hi - lo) / float64(total)
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1 {
		fraction = 1
	}
	return fraction, true
}

// stat4OneSidedFraction estimates a one-sided range (x > K / x < K) from
// the stat4 histogram: the cumulative sample row count at the bound gives
// the fraction on either side (where.c's stat4 range branch: nNew =
// LogEst(iUpper - iLower), iUpper the table end for a lower bound).
func stat4OneSidedFraction(samples []stat4PlannerSample, prefix []interface{}, col int, bound interface{}, total int64, above bool) (float64, bool) {
	matching, base, ok := stat4MatchingSamples(samples, prefix, col)
	if !ok || total <= 0 {
		return 0, false
	}
	below := stat4BoundRow(matching, col, base, bound, total)
	fraction := below / float64(total)
	if above {
		fraction = 1 - fraction
	}
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1 {
		fraction = 1
	}
	return fraction, true
}

// stat4MatchingSamples filters the samples whose leading prefix (the
// equality-bound columns before the range column) matches, and returns the
// cumulative-row base below the prefix.
func stat4MatchingSamples(samples []stat4PlannerSample, prefix []interface{}, col int) ([]stat4PlannerSample, int64, bool) {
	var matching []stat4PlannerSample
	for _, sample := range samples {
		if len(sample.values) <= col || len(sample.nlt) <= col || len(sample.neq) <= col {
			continue
		}
		matches := true
		for i, value := range prefix {
			if util.CompareValues(sample.values[i], value) != 0 {
				matches = false
				break
			}
		}
		if matches {
			matching = append(matching, sample)
		}
	}
	if len(matching) < 2 {
		return nil, 0, false
	}
	base := int64(0)
	if col > 0 {
		base = matching[0].nlt[col-1]
	}
	return matching, base, true
}

// stat4BoundRow returns the sample-estimated count of rows whose range
// column is BELOW the bound (interpolated through the samples' nlt
// counters). A DESCENDING index stores its samples in descending key
// order — its nlt counts the rows before it in index order (the LARGER
// values) — so the walk reverses (analyzeE-2.8: a>1900 over a DESC
// index seeks on the 10% tail).
func stat4BoundRow(matching []stat4PlannerSample, col int, base int64, bound interface{}, total int64) float64 {
	descending := util.CompareValues(matching[0].values[col], matching[len(matching)-1].values[col]) > 0
	if !descending {
		if util.CompareValues(bound, matching[0].values[col]) <= 0 {
			return 0
		}
		for i := 1; i < len(matching); i++ {
			if util.CompareValues(bound, matching[i].values[col]) <= 0 {
				return float64(matching[i].nlt[col] - base)
			}
		}
		return float64(total)
	}
	if util.CompareValues(bound, matching[0].values[col]) > 0 {
		return float64(total) // the bound tops the largest key: every row is below
	}
	for i := 1; i < len(matching); i++ {
		if util.CompareValues(matching[i].values[col], bound) < 0 {
			// The first sample below the bound: the rows before it in
			// index order are the rows at-or-above the bound.
			return float64(total - (matching[i].nlt[col] - base))
		}
	}
	return 0
}

// findIndexOnColsForQuery is like findIndexOnCols but additionally checks
// that partial indexes are implied by the query's WHERE clause.
func (e *SelectEngine) findIndexOnColsForQuery(tableName string, cols []string, where sql.Expr) string {
	if len(cols) == 0 {
		return ""
	}
	terms := make([]orderTermCol, len(cols))
	for i, c := range cols {
		terms[i] = orderTermCol{col: c}
	}
	return e.findIndexForOrderTerms(tableName, terms, where)
}

// findIndexForOrderTerms resolves the index whose leading columns match the
// ORDER BY terms under where.c's collation rule: each term's EFFECTIVE
// collation (the term's explicit COLLATE, else the table column's declared
// COLLATE, else BINARY) must equal the index column's DECLARED collation
// (the index definition's explicit COLLATE, else the table column's
// declared COLLATE, else BINARY) by NAME. The current registration of a
// collation is irrelevant — a stale stored order (an index last rebuilt
// under a different registration) still satisfies the ORDER BY (e_reindex
// 2.3.x's contract). Partial indexes are only used when the query WHERE
// implies the partial-index predicate.
func (e *SelectEngine) findIndexForOrderTerms(tableName string, terms []orderTermCol, where sql.Expr) string {
	token := ""
	e.forEachOrderServingIndex(tableName, terms, where, func(t string) bool {
		token = t
		return true // the first serving index wins
	})
	return token
}

// forEachOrderServingIndex visits every index of the table whose leading key
// columns match the ORDER BY terms (the trailing rowid-column rule of
// wherePathSatisfiesOrderBy included) under the collation rule above. The
// visitor returns true to stop the scan. where.c enumerates every serving
// index as a candidate loop and lets the cost model pick — the visitor keeps
// that enumeration available (the covering-walk preference in
// orderByIndexToken walks all candidates).
func (e *SelectEngine) forEachOrderServingIndex(tableName string, terms []orderTermCol, where sql.Expr, visit func(token string) bool) {
	if len(terms) == 0 {
		return
	}
	// The candidate indexes come from the FROM table's OWNING schema: an
	// attached database's same-named indexes are separate b-trees with
	// their own stored orders (e_reindex 2.4.x: REINDEX main.t1 must not
	// plan aux.t1's index walk through main's entry).
	_, tblCtx, err := e.ctx.FindTable(tableName)
	if err != nil || tblCtx == nil {
		return
	}
	entries, err := tblCtx.Schema.GetEntries("")
	if err != nil {
		return
	}
	// Schema-qualified FROM names ("aux.t1") match entries by the bare
	// object name; the collation lookups resolve the qualified spelling.
	objName := unqualifiedObjectName(tableName)
	// The table's declared per-column collations (the fallback both sides
	// resolve through when neither the term nor the index declares one).
	tableColls := map[string]string{}
	for _, entry := range entries {
		if entry.Type == "table" && strings.EqualFold(entry.Name, objName) {
			for _, def := range e.ctx.ParseColumnDefs(tableName, entry.SQL) {
				tableColls[strings.ToLower(def.Name)] = def.Collate
			}
			break
		}
	}
	declared := func(explicit, col string) string {
		if explicit != "" {
			return explicit
		}
		if c := tableColls[strings.ToLower(col)]; c != "" {
			return c
		}
		return "BINARY"
	}
	for _, entry := range entries {
		if entry.Type != "index" || !strings.EqualFold(entry.TblName, objName) || e.indexIsUnordered(entry.Name) {
			continue
		}
		indexCols := e.indexEntryColumns(entry)
		// where.c's order walk covers the trailing rowid column of a rowid
		// table (nColumn == nKeyCol+1): a final rowid-named ORDER BY term
		// rides the index entry's rowid tail (where-12.2's ORDER BY b, a).
		// The tail needs no collation (XN_ROWID compares numerically) and
		// its direction follows the scan. WITHOUT ROWID tables have no tail.
		keyTerms := terms
		if len(terms) == len(indexCols)+1 && len(e.withoutRowidPKCols(tableName)) == 0 &&
			e.isRowidTailTermName(tableName, terms[len(terms)-1].col) {
			keyTerms = terms[:len(terms)-1]
		}
		if len(indexCols) < len(keyTerms) || !colsPrefixMatch(indexCols, orderTermNames(keyTerms)) {
			continue
		}
		// The index definition's explicit per-column COLLATE; autoindex
		// entries (empty SQL) carry the table constraint's collations —
		// resolved through the table column's declared collation below.
		explicit := sql.ParseIndexColumnCollations(entry.SQL)
		match := true
		for i, term := range keyTerms {
			idxExplicit := ""
			if i < len(explicit) {
				idxExplicit = explicit[i]
			}
			if !strings.EqualFold(declared(idxExplicit, indexCols[i]), declared(term.coll, term.col)) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if entry.SQL != "" && strings.Contains(strings.ToUpper(entry.SQL), " WHERE ") &&
			!(where != nil && e.partialIndexImplied(entry, where)) {
			// A partial index only delivers an order for the rows its
			// predicate admits — the walk must not serve a statement that
			// can emit rows outside it (upsert1-320: ORDER BY b, a over a
			// (b) WHERE b>10 partial index ordered the in-predicate row
			// first and left the rest at the tail).
			continue
		}
		if visit(indexLookupToken(entry.Name)) {
			return
		}
	}
	// Also check the implicit PRIMARY KEY index of a WITHOUT ROWID table,
	// which has no separate schema index entry.
	if pkCols := e.withoutRowidPKCols(tableName); len(pkCols) >= len(terms) && colsPrefixMatch(pkCols, orderTermNames(terms)) {
		visit("PRIMARY KEY")
	}
}

// unqualifiedObjectName strips a leading schema prefix ("aux.t1" -> "t1").
func unqualifiedObjectName(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// orderTermNames renders a term list as plain column names.
func orderTermNames(terms []orderTermCol) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.col
	}
	return out
}

// isRowidTailTermName reports whether a final ORDER BY term names the rowid
// (pseudo-column or the table's INTEGER PRIMARY KEY alias) — the trailing
// implicit column of a rowid-table index entry (where.c's XN_ROWID walk
// position).
func (e *SelectEngine) isRowidTailTermName(tableName, col string) bool {
	if isRowIDName(col) {
		return true
	}
	alias := e.ipkAliasColumn(tableName)
	return alias != "" && strings.EqualFold(col, alias)
}

// indexEntryColumns returns the key column names of a schema index entry,
// resolving sqlite_autoindex_* entries (empty SQL) from the table's UNIQUE /
// PRIMARY KEY constraints.
func (e *SelectEngine) indexEntryColumns(entry *schema.Entry) []string {
	if entry.SQL == "" {
		return e.autoindexColumns(entry.TblName, entry.Name)
	}
	return e.ctx.ParseIndexColumns(entry.SQL)
}

// colsPrefixMatch reports whether the leading entries of indexCols equal cols
// (case-insensitive, in order).
func colsPrefixMatch(indexCols, cols []string) bool {
	for i, c := range cols {
		if !strings.EqualFold(indexCols[i], c) {
			return false
		}
	}
	return true
}

// indexCoversCols reports whether the named index contains every column in
// the list (case-insensitive). A "*" entry requires the index to cover every
// column of the underlying table (a full covering index).
func (e *SelectEngine) indexCoversCols(idx, tableName string, cols []string) bool {
	if idx == "" || len(cols) == 0 {
		return false
	}
	indexCols := e.indexColumns(idx)
	if len(indexCols) == 0 {
		return false
	}
	for _, c := range cols {
		if c == "*" {
			// Every table column must be in the index. The index contains the
			// rowid column implicitly, but a covering scan still needs each
			// named column.
			if !e.indexCoversAllTableCols(tableName, indexCols) {
				return false
			}
			continue
		}
		if !containsFold(indexCols, c) {
			return false
		}
	}
	return true
}

// containsFold reports whether list contains s (case-insensitive).
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// partialIndexWhereColumns returns the column names referenced in the named
// index's partial-index WHERE predicate (empty for a full index). These
// columns are "covered" by the index definition because the index only stores
// rows matching the predicate.
func (e *SelectEngine) partialIndexWhereColumns(idxName string) []string {
	idxName = indexSchemaName(idxName)
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type != "index" || entry.Name != idxName {
			continue
		}
		wm := indexWhereRe.FindStringSubmatch(entry.SQL)
		if wm == nil {
			return nil
		}
		return extractColumnNames(wm[1])
	}
	return nil
}

// sqlKeywords is the set of SQL keyword tokens excluded by extractColumnNames.
var sqlKeywords = map[string]bool{
	"AND": true, "OR": true, "NOT": true, "NULL": true,
	"IS": true, "IN": true, "LIKE": true, "GLOB": true,
}

// extractColumnNames scans a SQL predicate string and returns the column
// identifiers it references.
func extractColumnNames(pred string) []string {
	var cols []string
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		name := token.String()
		token.Reset()
		if isIdentifierStart(name[0]) && !sqlKeywords[strings.ToUpper(name)] {
			cols = append(cols, name)
		}
	}
	for i := 0; i < len(pred); i++ {
		c := pred[i]
		if isIdentChar(c) && (token.Len() > 0 || isIdentifierStart(c)) {
			token.WriteByte(c)
		} else {
			flush()
		}
	}
	flush()
	return cols
}

// isIdentifierStart reports whether c can start a SQL identifier.
func isIdentifierStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// isIdentChar reports whether c can appear in a SQL identifier body.
func isIdentChar(c byte) bool {
	return isIdentifierStart(c) || (c >= '0' && c <= '9') || c == '.'
}

// joinNodeFor emits the plan node for one table in a join: an index SEARCH on
// a join column when the other side is already planned, otherwise an index
// SEARCH on constant predicates when they are selective, otherwise a SCAN. A
// FROM-clause subquery that must be materialized becomes a CO-ROUTINE node
// (body plan nested) followed by a SCAN of the subquery alias; the caller
// splices those sibling nodes into the join plan.
func (e *SelectEngine) joinNodeFor(t queryTable, planned []string, joins []joinRef, s *sql.SelectStmt) []planNode {
	if orNode := e.joinOrNodeFor(t, planned, s); orNode != nil {
		// The operand's loop is a MULTI-INDEX OR (wherecode.c Case 5): one
		// sub-plan per arm in execution order.
		return orNode
	}
	if jr := e.joinSearchRef(joins, planned); jr != nil {
		// Try skip-scan when the join ref matches a non-leading column of an
		// index (e.g. WHERE c=y with index (a,b,c) becomes ANY(a) AND
		// ANY(b) AND c=?).
		if ss := e.joinSearchSkipScanNode(t, s); ss != nil {
			return ss
		}
		return joinSearchNode(t, jr)
	}
	if t.subquery != nil && needsMaterialization(t.subquery) {
		return e.coroutineNode(t)
	}
	return e.joinScanNode(t, s)
}

// joinSearchNode emits the SEARCH node for a join table whose join column is
// already satisfied by a planned table: a real index when the column has one,
// otherwise SQLite's materialized AUTOMATIC COVERING INDEX on the right side
// (e.g. a subquery in the FROM clause).
func joinSearchNode(t queryTable, jr *joinRef) []planNode {
	if jr.indexName != "" {
		if jr.indexName == "INTEGER PRIMARY KEY" {
			return []planNode{{detail: fmt.Sprintf("SEARCH %s USING INTEGER PRIMARY KEY (rowid=?)", t.display)}}
		}
		return []planNode{{detail: fmt.Sprintf("SEARCH %s USING INDEX %s (%s=?)", t.display, indexSchemaName(jr.indexName), jr.col)}}
	}
	// No real index on the join column (e.g. a subquery in the FROM
	// clause): SQLite materializes an automatic index on the right side.
	return []planNode{{detail: fmt.Sprintf("SEARCH %s USING AUTOMATIC COVERING INDEX (%s=?)", t.display, jr.col)}}
}

// joinSearchSkipScanNode emits a skip-scan SEARCH node when the join column
// is satisfied by a planned table but the chosen index has unconstrained
// low-cardinality leading columns (where.c:3517). E.g. t1j JOIN t1 with
// WHERE c=y on index t1abc(a,b,c) becomes SEARCH t1 USING INDEX t1abc
// (ANY(a) AND ANY(b) AND c=?).
func (e *SelectEngine) joinSearchSkipScanNode(t queryTable, s *sql.SelectStmt) []planNode {
	pred := joinWhereExpr(s)
	if pred == nil {
		return nil
	}
	const constrainedNoBest = float64(0)
	ss := e.trySkipScanPlan(t.real, pred, constrainedNoBest)
	if ss == nil {
		return nil
	}
	using := e.indexUsingLabel(t.real, ss.indexName, s)
	return []planNode{{detail: fmt.Sprintf("SEARCH %s USING %s %s", t.display, using, ss.conditions)}}
}

// coroutineNode emits the CO-ROUTINE + SCAN sibling nodes for a FROM-clause
// subquery that must be materialized (a compound or aggregate).
func (e *SelectEngine) coroutineNode(t queryTable) []planNode {
	alias := t.display
	if alias == "" {
		alias = "subquery"
	}
	coroutine := planNode{detail: "CO-ROUTINE " + alias}
	if t.subquery.Union != nil {
		coroutine.children = e.planCompound(t.subquery)
	} else {
		coroutine.children = e.planSelectMember(t.subquery)
	}
	scan := planNode{detail: "SCAN " + alias}
	return []planNode{coroutine, scan}
}

// joinScanNode emits the plan node for a join table with no satisfied join
// predicate: a skip-scan when an index's unconstrained leading cols have low
// cardinality, else an index SEARCH on selective constant predicates, else a
// SCAN.
func (e *SelectEngine) joinScanNode(t queryTable, s *sql.SelectStmt) []planNode {
	nRow := e.estimatedRowCount(t.real)
	est := float64(nRow)
	idx, conds := e.bestIndexForQuery(t.real, s.Where, &est)
	// Skip-scan: when an index's leading cols are unconstrained but a later
	// col is constrained (e.g. WHERE c=y in a join), the planner iterates
	// each distinct leading prefix. For joins we still consider skip-scan
	// when its conditions are met (mirrors SQLite where.c:3517).
	pred := joinWhereExpr(s)
	if pred != nil {
		if ss := e.trySkipScanPlan(t.real, pred, est); ss != nil {
			using := e.indexUsingLabel(t.real, ss.indexName, s)
			return []planNode{{detail: fmt.Sprintf("SEARCH %s USING %s %s", t.display, using, ss.conditions)}}
		}
	}
	if idx != "" && (idx == "PRIMARY KEY" || est < float64(nRow)*0.10) {
		using := e.indexUsingLabel(t.real, idx, s)
		if conds != "" {
			return []planNode{{detail: fmt.Sprintf("SEARCH %s USING %s %s", t.display, using, conds)}}
		}
		return []planNode{{detail: fmt.Sprintf("SEARCH %s USING %s", t.display, using)}}
	}
	return []planNode{{detail: "SCAN " + t.display}}
}

// indexUsingLabel renders the index-usage phrase of a SEARCH/SCAN plan node:
// "PRIMARY KEY" for the implicit WITHOUT ROWID storage index, "COVERING INDEX
// <name>" when the index covers the SELECT's output columns (rowvalue 34.5;
// SQLite uses COVERING INDEX when no temp table is needed to resolve the
// output), or "INDEX <name>". When s is nil, falls back to all-table-cols.
func (e *SelectEngine) indexUsingLabel(tableName, idx string, s *sql.SelectStmt) string {
	// idx carries the planner token: translate it for display, while the
	// resolve helpers below (indexColumns, indexCoversCols) accept the token
	// directly — the empty name must stay distinguishable from "not found".
	name := indexSchemaName(idx)
	using := "INDEX " + name
	if idx == "PRIMARY KEY" {
		using = "PRIMARY KEY"
	} else {
		cols := e.indexColumns(idx)
		// A WITHOUT ROWID index implicitly carries the PRIMARY KEY columns
		// (they lead every index entry), so an index that covers the PK
		// covers the whole row (index7-2.2: t2(a,b PRIMARY KEY) WITHOUT
		// rowid with index t2a1(a) gives "SEARCH t2 USING COVERING INDEX
		// t2a1 (a=?)" for "SELECT * FROM t2 WHERE a=5").
		if pk := e.withoutRowidPKCols(tableName); len(pk) > 0 {
			cols = append(append([]string{}, cols...), pk...)
		}
		covered := e.indexCoversAllTableCols(tableName, cols)
		if !covered && s != nil {
			// COVERING when the index covers every column referenced by this
			// SELECT (so the temp table isn't needed).
			covered = e.indexCoversCols(idx, tableName, selectOutputCols(s))
		}
		if covered {
			using = "COVERING INDEX " + name
		}
	}
	return using
}

// collectIndexedRefs walks a WHERE expression and returns one indexedRef per
// predicate that can drive an index scan: column-to-constant comparisons and
// BETWEEN on an indexed column, plus LIKE patterns that satisfy the LIKE
// optimization. Returns nil when no predicate is indexed.
// The full WHERE expression is passed to findIndexOnColumn so partial-index
// implication can be checked.
func collectIndexedRefs(expr sql.Expr, tableName string, e *SelectEngine) []indexedRef {
	var refs []indexedRef
	walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			refs = append(refs, collectBinaryRef(binop, tableName, e, expr)...)
		}
	})
	walkExpr(expr, func(e2 sql.Expr) {
		if bt, ok := e2.(*sql.Between); ok {
			if ref, ok := collectBetweenRef(bt, tableName, e, expr); ok {
				refs = append(refs, ref)
			}
		}
	})
	return refs
}

// collectBinaryRef collects the indexed ref for one BinaryOp predicate: the
// LIKE optimization or a column-to-constant comparison.
// where is the full query WHERE, used to check partial-index implication.
func collectBinaryRef(binop *sql.BinaryOp, tableName string, e *SelectEngine, where sql.Expr) []indexedRef {
	if binop.Operator == "LIKE" {
		return collectLikeRef(binop, tableName, e, where)
	}
	colRef, constVal, constExpr := findColAndConstExpr(binop, e)
	flip := false
	if colRef == nil || constVal == nil && constExpr == nil {
		// A correlated bound: the comparison's own-table column side pairs
		// with a bound that resolves through the enclosing outer rows (the
		// subquery re-executes per outer row — where.c compiles the seek
		// with the outer loop's register). The bound is a runtime constant
		// for planning; its VALUE binds at execution.
		var bound sql.Expr
		colRef, bound, flip = e.seekRefSides(binop, tableName)
		if colRef == nil {
			return nil
		}
		constExpr = bound
	} else if _, leftIsCol := binop.Left.(*sql.ColumnRef); !leftIsCol {
		// findColAndConstExpr took the RIGHT operand as the column: the
		// operator flips (5 > d means d < 5) — the executor's term
		// extraction flips the same way, so the (column, operator) pairs
		// must agree.
		flip = true
	}
	op := binop.Operator
	if flip {
		op = flipComparisonOp(op)
	}
	idxName := e.findIndexOnColumn(tableName, colRef.Name, where)
	if idxName == "" {
		return nil
	}
	if !e.seekCollationAgrees(tableName, idxName, colRef.Name, binop) {
		return nil
	}
	return []indexedRef{{indexName: idxName, colName: colRef.Name, constant: constVal, constantExpr: constExpr, op: op}}
}

// seekRefSides splits a comparison into its own-table column side and its
// runtime-constant bound side (either orientation): a literal/parameter or
// deterministic expression bound (findColAndConstExpr's shapes), or a
// column reference that resolves OUTSIDE the statement being planned (a
// correlated bound). nil when neither orientation yields the pair.
func (e *SelectEngine) seekRefSides(b *sql.BinaryOp, tableName string) (*sql.ColumnRef, sql.Expr, bool) {
	for i, sides := range [2][2]sql.Expr{{b.Left, b.Right}, {b.Right, b.Left}} {
		colRef, ok := sides[0].(*sql.ColumnRef)
		if !ok || colRef.Name == "*" || !e.seekRefIsOwnColumn(colRef, tableName) {
			continue
		}
		bound := sides[1]
		if extractConst(bound) != nil || deterministicConstExpr(bound, e) != nil {
			return colRef, bound, i == 1
		}
		if e.seekRefIsOuterColumn(bound, tableName) {
			return colRef, bound, i == 1
		}
	}
	return nil, nil, false
}

// seekCollationAgrees reports whether the index named idxName orders its
// colName column by the comparison's effective collation — C's rule
// (whereLoopAddBtree only takes a term whose collation matches the index
// column's sqlite3IndexCollSeq). The comparison's effective collation is an
// explicit COLLATE on either operand, else the column-operand side's declared
// collation (sqlite3BinaryCompareCollSeq); the index column's is the index
// DDL's explicit COLLATE, else the column's declared collation. An index
// ordering a NOCASE column by BINARY cannot serve a=x (existsexpr-4.4.2's
// recreated index: the seek would compare under the wrong collation and
// miss every entry). The IPK/PK tokens are numeric — no collation applies.
func (e *SelectEngine) seekCollationAgrees(tableName, idxName, colName string, binop *sql.BinaryOp) bool {
	if idxName == "PRIMARY KEY" || idxName == "INTEGER PRIMARY KEY" {
		return true
	}
	seek := collateWrapperName(binop.Left)
	if seek == "" {
		seek = e.tableColumnCollationForRef(binop.Left, tableName)
	}
	if seek == "" {
		seek = collateWrapperName(binop.Right)
	}
	if seek == "" {
		seek = e.tableColumnCollationForRef(binop.Right, tableName)
	}
	if seek == "" {
		return true
	}
	return strings.EqualFold(seek, e.indexColumnCollation(tableName, idxName, colName))
}

// tableColumnCollationForRef returns the declared collation of the column
// when ref names one of the planned table's columns (either orientation of
// the comparison qualifies); "" when ref is not an own-table column.
func (e *SelectEngine) tableColumnCollationForRef(ref sql.Expr, tableName string) string {
	cr, ok := ref.(*sql.ColumnRef)
	if !ok || !e.seekRefIsOwnColumn(cr, tableName) {
		return ""
	}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return ""
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if strings.EqualFold(cd.Name, cr.Name) {
			return strings.ToUpper(cd.Collate)
		}
	}
	return ""
}

// seekRefIsOwnColumn reports whether the reference names a column of the
// statement being planned (the FROM table's token — its alias when the FROM
// term carries one — or one of the table's columns for an unqualified name).
func (e *SelectEngine) seekRefIsOwnColumn(ref *sql.ColumnRef, tableName string) bool {
	if ref.Table != "" {
		tok := strings.ToLower(schemaStrippedName(ref.Table))
		own := strings.ToLower(schemaStrippedName(e.currentScanTable))
		return own != "" && tok == own
	}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return false
	}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		if strings.EqualFold(cd.Name, ref.Name) {
			return true
		}
	}
	return false
}

// seekRefIsOuterColumn reports whether the reference is a column that does
// NOT resolve inside the statement being planned (an enclosing scope's
// column — the correlated bound's source). A reference resolving to any
// FROM operand of the current statement (a join sibling's column) is NOT a
// correlated bound: its value is only computable at that operand's own loop
// level, not at this table's seek.
func (e *SelectEngine) seekRefIsOuterColumn(expr sql.Expr, tableName string) bool {
	ref, ok := expr.(*sql.ColumnRef)
	if !ok || ref.Name == "*" {
		return false
	}
	if ref.Table != "" {
		if e.stmtOperandTokens != nil {
			tok := strings.ToLower(schemaStrippedName(ref.Table))
			for _, t := range e.stmtOperandTokens {
				if tok == t {
					return false
				}
			}
			return true
		}
		return !e.seekRefIsOwnColumn(ref, tableName)
	}
	if e.stmtOperandCols != nil {
		return !e.stmtOperandCols[strings.ToLower(ref.Name)]
	}
	return !e.seekRefIsOwnColumn(ref, tableName)
}

// collectLikeRef collects the indexed ref for a LIKE predicate when its
// constant pattern has a non-wildcard prefix and the column's index collation
// matches the LIKE comparison (the LIKE optimization). The pattern may be
// wrapped in an explicit COLLATE (x LIKE 'abc%' COLLATE nocase parses with
// the pattern inside a COLLATE BinaryOp); the explicit COLLATE on the LIKE
// operand does not affect index selection (SQLite uses the column/index
// collation).
// where is the full query WHERE, used to check partial-index implication.
func collectLikeRef(binop *sql.BinaryOp, tableName string, e *SelectEngine, where sql.Expr) []indexedRef {
	colRef, ok := binop.Left.(*sql.ColumnRef)
	if !ok {
		return nil
	}
	pattern, ok := likePatternConst(binop.Right)
	if !ok {
		return nil
	}
	prefix, ok := likePrefix(pattern, binop.Escape, binop.HasEscape)
	if !ok || prefix == "" {
		return nil
	}
	idxName := e.findIndexOnColumn(tableName, colRef.Name, where)
	if idxName == "" {
		return nil
	}
	coll := e.indexColumnCollation(tableName, idxName, colRef.Name)
	if !e.likeIndexCompatible(coll) {
		return nil
	}
	return []indexedRef{{
		indexName:   idxName,
		colName:     colRef.Name,
		constant:    prefix,
		op:          "LIKE",
		selectivity: estimateLikePrefixSelectivity(prefix),
	}}
}

// collectBetweenRef collects the indexed ref for a BETWEEN predicate on an
// indexed column or expression index key (e.g. datetime(b) BETWEEN ... with
// an index on datetime(b)).
// where is the full query WHERE, used to check partial-index implication.
func collectBetweenRef(bt *sql.Between, tableName string, e *SelectEngine, where sql.Expr) (indexedRef, bool) {
	idxName := ""
	colName := ""
	if colRef, ok := bt.Operand.(*sql.ColumnRef); ok {
		colName = colRef.Name
		idxName = e.findIndexOnColumn(tableName, colRef.Name, where)
	} else {
		// Expression operand: match against an expression-index key.
		exprSQL := sql.ExprString(bt.Operand)
		colName = exprSQL
		idxName = e.findIndexOnExpr(tableName, exprSQL)
	}
	if idxName == "" {
		return indexedRef{}, false
	}
	low, lowErr := e.ctx.EvalExpr(bt.Low, nil)
	high, highErr := e.ctx.EvalExpr(bt.High, nil)
	if lowErr != nil || highErr != nil {
		low, high = nil, nil
	}
	return indexedRef{
		indexName:   idxName,
		colName:     colName,
		constant:    float64(0),
		op:          "BETWEEN",
		selectivity: computeBetweenSelectivity(bt),
		rangeLow:    low,
		rangeHigh:   high,
	}, true
}

// findIndexOnColumn returns the name of an index on the table that contains
// colName as a key column, or "" when none does. A WITHOUT ROWID table's
// PRIMARY KEY is its implicit storage index; a reference to a PK column
// returns the "PRIMARY KEY" marker the plan formatters render.
//
// When where is non-nil, partial indexes (CREATE INDEX ... WHERE ...) are only
// considered when the query's WHERE clause implies the partial-index predicate.
func (e *SelectEngine) findIndexOnColumn(tableName, colName string, where ...sql.Expr) string {
	if e.isWithoutRowidPKColumn(tableName, colName) {
		return "PRIMARY KEY"
	}
	if entry, _, err := e.ctx.FindTable(tableName); err == nil {
		for _, def := range e.ctx.ParseColumnDefs(tableName, entry.SQL) {
			if strings.EqualFold(def.Name, colName) && isIPKRowidAliasCol(def) {
				return "INTEGER PRIMARY KEY"
			}
		}
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	var whereExpr sql.Expr
	if len(where) > 0 {
		whereExpr = where[0]
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName && indexEntryContains(e.indexEntryColumns(entry), colName) {
			if whereExpr != nil && !e.partialIndexImplied(entry, whereExpr) {
				continue
			}
			return indexLookupToken(entry.Name)
		}
	}
	return ""
}

// indexEntryContains reports whether an index's key columns include colName
// (case-insensitive).
func indexEntryContains(indexCols []string, colName string) bool {
	for _, ic := range indexCols {
		if strings.EqualFold(ic, colName) {
			return true
		}
	}
	return false
}

// findIndexOnExpr matches an expression (rendered as SQL text) against the
// key expressions of every index on the table. This lets the planner use
// expression indexes (e.g. CREATE INDEX i ON t(datetime(b))) for a WHERE
// predicate whose operand is the same expression. Returns the index name or "".
func (e *SelectEngine) findIndexOnExpr(tableName, exprSQL string) string {
	if exprSQL == "" {
		return ""
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName && entry.SQL != "" && indexKeyMatches(entry.SQL, exprSQL) {
			return indexLookupToken(entry.Name)
		}
	}
	return ""
}

// indexKeyMatches reports whether an index's CREATE SQL contains a key
// expression equal to exprSQL (case-insensitive, trimmed).
func indexKeyMatches(indexSQL, exprSQL string) bool {
	colText := indexColumnListText(indexSQL)
	if colText == "" {
		return false
	}
	for _, ke := range parseIndexKeyCols(colText) {
		if strings.EqualFold(strings.TrimSpace(ke), exprSQL) {
			return true
		}
	}
	return false
}

// readStatSZs reads the sqlite_stat1 table and returns a map of index name -> sz value.
func (e *SelectEngine) readStatSZs() map[string]int {
	szMap := make(map[string]int)
	e.forEachStat1Row(func(rec *storage.Record) bool {
		// Use positional access: values are [tbl, idx, stat]
		if len(rec.Values) >= 3 {
			if idxStr, ok := rec.Values[1].(string); ok {
				if statStr, ok := rec.Values[2].(string); ok {
					szMap[idxStr] = parseStatSZ(statStr)
				}
			}
		}
		return true
	})
	return szMap
}

// forEachStat1Row calls fn for each decoded row of sqlite_stat1 until fn
// returns false or the table cannot be read. It is the shared cursor walk for
// readStatSZs and stat1RowCount.
func (e *SelectEngine) forEachStat1Row(fn func(rec *storage.Record) bool) {
	statEntry, err := e.ctx.Schema().FindTable("sqlite_stat1")
	if err != nil {
		return
	}
	tree := e.ctx.TableBTree("sqlite_stat1", statEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			return
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return
		}
		if !fn(rec) {
			return
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return
		}
	}
}

// partialIndexImplied reports whether the query's WHERE clause implies the
// partial-index predicate in idxEntry.SQL. A full index (no WHERE clause) is
// always implied. For partial indexes, the implication check mirrors
// SQLite's whereUsablePartialIndex:
//   - For AND conjuncts in the index predicate, ALL must be implied.
//   - For OR disjuncts, at least ONE branch must be fully implied.
//
// Each leaf term (e.g. a<100) is implied when it appears verbatim as a
// conjunct of the query's WHERE clause.
func (e *SelectEngine) partialIndexImplied(idxEntry *schema.Entry, where sql.Expr) bool {
	if idxEntry.SQL == "" {
		return true
	}
	wm := indexWhereRe.FindStringSubmatch(idxEntry.SQL)
	if wm == nil {
		return true // full index, no predicate
	}
	pred := strings.TrimSpace(wm[1])
	if pred == "" {
		return true
	}
	queryTerms := queryAndTerms(where)
	if len(queryTerms) == 0 {
		return false
	}
	return indexPredImplied(pred, queryTerms)
}

// indexPredImplied checks whether every term in the index predicate is implied
// by the query's conjunctive terms. Top-level OR branches are each checked;
// at least one must be fully implied.
func indexPredImplied(pred string, queryTerms map[string]bool) bool {
	orBranches := splitTopLevel(pred, " OR ")
	if len(orBranches) > 1 {
		for _, br := range orBranches {
			if indexPredImplied(br, queryTerms) {
				return true
			}
		}
		return false
	}
	return allAndTermsImplied(pred, queryTerms)
}

// allAndTermsImplied checks that every AND conjunct in pred is implied by
// queryTerms. Parenthesised sub-expressions are recursed into. An "X IS NOT
// NULL" conjunct is implied when any query constraint on X uses a comparison
// with a non-NULL operand (where.c exprImpliesNonNullRow: an EQ/LT/LE/GT/GE/
// NE/IN/BETWEEN/LIKE/GLOB constraint on the column guarantees it is not NULL
// in every surviving row — index6-2.2 "WHERE a=5" implies the partial index
// predicate "a IS NOT NULL").
func allAndTermsImplied(pred string, queryTerms map[string]bool) bool {
	for _, t := range splitTopLevel(pred, " AND ") {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
			if !indexPredImplied(t[1:len(t)-1], queryTerms) {
				return false
			}
			continue
		}
		if _, ok := queryTerms[normaliseTerm(t)]; !ok {
			if col, isNonNull := nonNullTermColumn(t); isNonNull && nonNullImpliedByTerms(col, queryTerms) {
				continue
			}
			return false
		}
	}
	return true
}

// nonNullTermColumn matches an "X IS NOT NULL" predicate term and returns X
// (qualified form preserved), or reports that the term is not of that shape.
func nonNullTermColumn(term string) (string, bool) {
	f := strings.Fields(term)
	if len(f) == 4 && strings.EqualFold(f[1], "IS") && strings.EqualFold(f[2], "NOT") &&
		strings.EqualFold(f[3], "NULL") {
		return f[0], true
	}
	return "", false
}

// nonNullImpliedByTerms reports whether any query conjunct constrains col
// with an operator whose surviving rows cannot hold NULL for that column.
func nonNullImpliedByTerms(col string, queryTerms map[string]bool) bool {
	for term := range queryTerms {
		f := strings.Fields(term)
		if len(f) < 2 || !strings.EqualFold(f[0], col) {
			continue
		}
		switch strings.ToUpper(f[1]) {
		case "=", "==", "<", "<=", ">", ">=", "<>", "!=", "IN", "BETWEEN", "LIKE", "GLOB":
			return true
		}
	}
	return false
}

// splitTopLevel splits a SQL predicate string on the given separator at
// parenthesis depth 0 (case-insensitive). Used for " AND " and " OR ".
func splitTopLevel(pred, sep string) []string {
	var parts []string
	depth := 0
	start := 0
	sepUpper := strings.ToUpper(sep)
	i := 0
	for i < len(pred) {
		switch pred[i] {
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth == 0 && i+len(sep) <= len(pred) {
				rest := strings.ToUpper(pred[i : i+len(sep)])
				if rest == sepUpper {
					parts = append(parts, strings.TrimSpace(pred[start:i]))
					start = i + len(sep)
					i += len(sep)
					continue
				}
			}
		}
		i++
	}
	parts = append(parts, strings.TrimSpace(pred[start:]))
	return parts
}

// queryAndTerms returns the set of normalised top-level AND conjuncts of a
// query WHERE expression.
func queryAndTerms(where sql.Expr) map[string]bool {
	result := make(map[string]bool)
	if where == nil {
		return result
	}
	var visit func(e sql.Expr)
	visit = func(ex sql.Expr) {
		if be, ok := ex.(*sql.BinaryOp); ok && be.Operator == "AND" {
			visit(be.Left)
			visit(be.Right)
			return
		}
		result[normaliseTerm(sql.ExprString(ex))] = true
	}
	visit(where)
	return result
}

// normaliseTerm collapses whitespace, ensures single spaces around comparison
// operators, and canonicalises operand order (column name first, constant
// second) so "a=1", "a = 1", and "1=a" all compare equal.
func normaliseTerm(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	for _, op := range []string{"<=", ">=", "<>", "!=", "=", "<", ">", " IS "} {
		if i := strings.Index(s, op); i >= 0 {
			before := strings.TrimSpace(s[:i])
			after := strings.TrimSpace(s[i+len(op):])
			opTrim := strings.TrimSpace(op)
			// The parser renders a unary minus as "- <n>" (ExprString's
			// spacing); the DDL text carries "-<n>" — fold the prefix so
			// y = - 20111000111 matches y = -20111000111 (index9-2.2). The
			// fold is operand-local: a binary minus is never scanned as an
			// operator, so "a - 5" never reaches this path.
			before = foldUnarySignPrefix(before)
			after = foldUnarySignPrefix(after)
			// Canonicalise: put the column identifier first.
			if isIdentifier(before) && !isIdentifier(after) {
				return before + " " + opTrim + " " + after
			}
			if isIdentifier(after) && !isIdentifier(before) {
				return after + " " + flipOp(opTrim) + " " + before
			}
			return before + " " + opTrim + " " + after
		}
	}
	return s
}

// foldUnarySignPrefix joins a leading unary minus/plus back onto its
// operand: "- 5" -> "-5" (the ExprString unary rendering vs the DDL text).
func foldUnarySignPrefix(s string) string {
	for _, sign := range []string{"- ", "+ "} {
		if strings.HasPrefix(s, sign) {
			return s[:1] + s[2:]
		}
	}
	return s
}

// isIdentifier reports whether s is a bare SQL identifier (column name).
func isIdentifier(s string) bool {
	if s == "" || !isIdentifierStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return true
}

// flipOp reverses the direction of a comparison operator so that swapping
// operand order preserves the semantic: < ↔ >, <= ↔ >=, = stays, != stays.
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case ">":
		return "<"
	case "<=":
		return ">="
	case ">=":
		return "<="
	default:
		return op // =, <>, !=, IS are symmetric
	}
}
