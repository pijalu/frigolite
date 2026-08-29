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
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// indexWhereRe captures the partial-index predicate after the column list.
// Mirrors execdml.IndexWhereRe without importing that package (which would
// create an import cycle since execdml imports execquery).
var indexWhereRe = regexp.MustCompile(`(?is)\)\s*WHERE\s+(.+)$`)

// findIndexOnCols returns the name of an index on the table whose leading
// columns match the given column list in order (a prefix match), or "" when
// no index qualifies. Used to decide whether ORDER BY / GROUP BY can use an
// index scan instead of a temp b-tree.
func (e *SelectEngine) findIndexOnCols(tableName string, cols []string) string {
	return e.findIndexOnColsForQuery(tableName, cols, nil)
}

// findIndexOnColsForQuery is like findIndexOnCols but additionally checks
// that partial indexes are implied by the query's WHERE clause.
func (e *SelectEngine) findIndexOnColsForQuery(tableName string, cols []string, where sql.Expr) string {
	if len(cols) == 0 {
		return ""
	}
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type != "index" || entry.TblName != tableName {
			continue
		}
		indexCols := e.indexEntryColumns(entry)
		if len(indexCols) >= len(cols) && colsPrefixMatch(indexCols, cols) {
			if where != nil && !e.partialIndexImplied(entry, where) {
				continue
			}
			return entry.Name
		}
	}
	// Also check the implicit PRIMARY KEY index of a WITHOUT ROWID table,
	// which has no separate schema index entry.
	if pkCols := e.withoutRowidPKCols(tableName); len(pkCols) >= len(cols) && colsPrefixMatch(pkCols, cols) {
		return "PRIMARY KEY"
	}
	return ""
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
		return []planNode{{detail: fmt.Sprintf("SEARCH %s USING INDEX %s (%s=?)", t.display, jr.indexName, jr.col)}}
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
	using := "INDEX " + idx
	if idx == "PRIMARY KEY" {
		using = "PRIMARY KEY"
	} else {
		cols := e.indexColumns(idx)
		covered := e.indexCoversAllTableCols(tableName, cols)
		if !covered && s != nil {
			// COVERING when the index covers every column referenced by this
			// SELECT (so the temp table isn't needed).
			covered = e.indexCoversCols(idx, tableName, selectOutputCols(s))
		}
		if covered {
			using = "COVERING INDEX " + idx
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
	colRef, constVal := findColAndConst(binop)
	// Only column-to-constant predicates can drive an index SEARCH;
	// column-to-column predicates are join terms, not constants.
	if colRef == nil || constVal == nil {
		return nil
	}
	idxName := e.findIndexOnColumn(tableName, colRef.Name, where)
	if idxName == "" {
		return nil
	}
	return []indexedRef{{indexName: idxName, colName: colRef.Name, constant: constVal, op: binop.Operator}}
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
	return indexedRef{
		indexName:   idxName,
		colName:     colName,
		constant:    float64(0),
		op:          "BETWEEN",
		selectivity: computeBetweenSelectivity(bt),
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
			return entry.Name
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
			return entry.Name
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
// queryTerms. Parenthesised sub-expressions are recursed into.
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
			return false
		}
	}
	return true
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
