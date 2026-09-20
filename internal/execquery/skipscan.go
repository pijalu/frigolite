// Package exec implements query execution.
//
// This file holds the skip-scan planner support (where.c WHERE_SKIPSCAN):
// when an index has K unconstrained low-cardinality leading columns but a
// later column IS constrained, the planner considers iterating over each
// distinct value of the leading columns and running an index range scan from
// that prefix. The EQP output renders the unconstrained leading columns as
// ANY(col) (mirroring SQLite's wherecode.c explainIndexRange).
package execquery

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// skipScanMinRepeats is the SQLite threshold (aiRowLogEst[saved_nEq+1] >= 42,
// i.e. log2 >= 4.2, i.e. avg repeats >= 18) below which skip-scan is not
// considered. Below 18, repeated seeks are slower than scanning all rows.
const skipScanMinRepeats = 18

// skipScanPlan is the result of skip-scan detection: which index to use, how
// many leading columns are ANY() (nSkip), and the formatted condition string.
type skipScanPlan struct {
	indexName   string   // chosen index name (or "PRIMARY KEY" for WITHOUT ROWID PK)
	nSkip       int      // number of leading columns to render as ANY(col)
	conditions  string   // formatted "(ANY(c0) AND ANY(c1) AND c2=?)"
	estRows     float64  // estimated number of result rows
	leadingCols []string // the nSkip leading column names (for execution)
	allCols     []string // all columns of the chosen index (for execution)
}

// joinWhereExpr returns the conjunction of the WHERE clause and every JOIN
// ON clause. Returns nil when both are empty. Mirrors the predicates
// joinPredicates() collects for the join planner.
func joinWhereExpr(s *sql.SelectStmt) sql.Expr {
	var parts []sql.Expr
	if s.Where != nil {
		parts = append(parts, s.Where)
	}
	for _, j := range s.Joins {
		if j.On != nil {
			parts = append(parts, j.On)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out = &sql.BinaryOp{Operator: "AND", Left: out, Right: p}
	}
	return out
}

// hasTopLevelOR reports whether the WHERE expression contains an OR at the
// top level (i.e. not nested inside parens that would make it AND-only).
// Skip-scan optimization does not apply inside OR trees (each branch's
// constraints are not simultaneously active), mirroring SQLite's where.c
// where the WHERE_SKIPSCAN branch is reached only via the inner WHERE-AND
// loop, not through OR processing.
func hasTopLevelOR(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *sql.BinaryOp:
		if strings.EqualFold(e.Operator, "OR") {
			return true
		}
		return hasTopLevelOR(e.Left) || hasTopLevelOR(e.Right)
	case *sql.ParenExpr:
		return hasTopLevelOR(e.Expr)
	}
	return false
}

// trySkipScanPlan examines all indexes on tableName for skip-scan candidates:
// an index where the first K columns are unconstrained by WHERE but later
// columns are, with avg repeats at level K+1 (from stat1) >= 18. Returns the
// candidate with the lowest estimated row count.
//
// Mirrors SQLite's skip-scan branch in whereLoopAddBtreeIndex
// (where.c:3517-3554).
func (e *SelectEngine) trySkipScanPlan(tableName string, where sql.Expr, bestEst float64) *skipScanPlan {
	// Skip-scan can be toggled off at runtime via PRAGMA skip_scan = 0 (used
	// by skip-scan tests to verify the alternative plan when the optimization
	// is disabled).
	if e.ctx != nil && !e.ctx.SkipScanEnabled() {
		return nil
	}
	// Skip-scan across OR branches is not implemented: SQLite's where.c
	// OR-optimizes each branch separately, consulting skip-scan inside the
	// per-branch WhereLoop. Our OR-index optimization (internal/execdml/or.go)
	// emits one regular btree lookup per branch without per-branch skip-scan,
	// so we bail on top-level OR here. See skipscan1.test 8.1 (N-A via
	// tools/tcl2go/skiptestfiles.go).
	if hasTopLevelOR(where) {
		return nil
	}
	// Collect column-to-constant WHERE refs and their column names + ops.
	constrainedCols := constrainedColumnNames(where, tableName)
	if len(constrainedCols) == 0 {
		return nil
	}
	colOps := constrainedColOps(where, tableName)

	// Enumerate indexes on this table.
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return nil
	}

	// Use stat1[0] as nRow when available (it represents ANALYZE's row count,
	// not the in-memory cell count). Skip-scan estimates are stat-derived so
	// comparing them against actual row count would always favor skip-scan in
	// small tables.
	var nRow int64
	statRows := e.stat1RowCount(tableName)
	if statRows > 0 {
		nRow = statRows
	} else {
		nRow = e.estimatedRowCount(tableName)
		if nRow <= 0 {
			nRow = 1000000
		}
	}

	best := e.bestSkipScanIndex(entries, tableName, constrainedCols, colOps, nRow)
	if best == nil {
		return nil
	}
	return best
}

// bestSkipScanIndex enumerates the table's candidate indexes (schema indexes
// plus the implicit PRIMARY KEY of a WITHOUT ROWID table) and returns the
// skip-scan plan with the lowest estimated row count. The stat1 row for a
// WITHOUT ROWID PK uses the table name (or sqlite_autoindex_<tbl>_1) as idx;
// it is looked up via the schema rather than hard-coding "PRIMARY KEY".
func (e *SelectEngine) bestSkipScanIndex(entries []*schema.Entry, tableName string, constrainedCols map[string]bool, colOps map[string]string, nRow int64) *skipScanPlan {
	best := e.bestSchemaSkipScan(entries, tableName, constrainedCols, colOps, nRow)
	if pkCols := e.withoutRowidPKCols(tableName); len(pkCols) >= 2 {
		pkStatName := e.primaryKeyStatName(tableName)
		plan := e.skipScanForColumnsWithStatName("PRIMARY KEY", pkCols, constrainedCols, colOps, nRow, pkStatName)
		if plan != nil && (best == nil || plan.estRows < best.estRows) {
			best = plan
		}
	}
	return best
}

// bestSchemaSkipScan enumerates the schema's declared indexes on the table
// and returns the skip-scan plan with the lowest estimated row count.
func (e *SelectEngine) bestSchemaSkipScan(entries []*schema.Entry, tableName string, constrainedCols map[string]bool, colOps map[string]string, nRow int64) *skipScanPlan {
	var best *skipScanPlan
	for _, entry := range entries {
		if entry.Type != "index" || entry.TblName != tableName {
			continue
		}
		var cols []string
		if entry.SQL == "" {
			cols = e.autoindexColumns(entry.TblName, entry.Name)
		} else {
			cols = e.ctx.ParseIndexColumns(entry.SQL)
		}
		if len(cols) < 2 {
			continue
		}
		if e.statHasNoSkipScan(entry.Name) {
			continue
		}
		plan := e.skipScanForColumns(entry.Name, cols, constrainedCols, colOps, nRow)
		if plan != nil && (best == nil || plan.estRows < best.estRows) {
			best = plan
		}
	}
	return best
}

// skipScanForColumns tests one candidate index for skip-scan. cols is the
// index column list (in order); constrainedCols is the set of columns that
// appear in WHERE as column-to-constant comparisons. Returns the skip-scan
// plan when (1) the first nSkip cols are NOT in constrainedCols, (2) the
// next col IS constrained, (3) stat1 says avg repeats at level nSkip+1 is
// >= 18, and (4) the noskipscan hint is absent.
func (e *SelectEngine) skipScanForColumns(idxName string, cols []string, constrainedCols map[string]bool, colOps map[string]string, nRow int64) *skipScanPlan {
	return e.skipScanForColumnsWithStatName(idxName, cols, constrainedCols, colOps, nRow, idxName)
}

// skipScanForColumnsWithStatName is the internal worker; pass a different
// statName (e.g. for a WITHOUT ROWID table's implicit PRIMARY KEY whose
// stat1 idx column is the table name itself).
// skipScanSkipCount determines the number of leading index columns the
// skip-scan iterates over, or -1 when the shape does not apply. Two modes:
//  1. K unconstrained leading cols + 1 constrained trailing col (basic).
//  2. K constrained leading cols + >=1 unconstrained col + 1 constrained
//     trailing col (2014-08-20 addition; needed for skipscan3.test 1.3:
//     `a=1 AND c=32` -> ANY(a) AND ANY(b) AND c=? where the leading col a IS
//     constrained and acts as a single-value iteration variable).
//
// Mode 2 is tried first (more permissive), then mode 1.
func skipScanSkipCount(cols []string, constrainedCols map[string]bool) int {
	if constrainedCols[strings.ToLower(cols[0])] {
		return skipScanSkipCountConstrainedHead(cols, constrainedCols)
	}
	// Mode 1: standard skip-scan.
	nSkip := 0
	for nSkip < len(cols)-1 && !constrainedCols[strings.ToLower(cols[nSkip])] {
		nSkip++
	}
	if nSkip == 0 {
		return -1
	}
	// Need the NEXT column to be constrained for skip-scan to apply.
	if !constrainedCols[strings.ToLower(cols[nSkip])] {
		return -1
	}
	return nSkip
}

// skipScanSkipCountConstrainedHead is mode 2: count constrained leading cols,
// then require >=1 unconstrained col between the leading constraint and the
// next constrained col. The "next constrained col" can be at position
// len(cols)-1 (the last col), which then becomes the skip-scan tail.
func skipScanSkipCountConstrainedHead(cols []string, constrainedCols map[string]bool) int {
	nSkip := 0
	for nSkip < len(cols)-1 && constrainedCols[strings.ToLower(cols[nSkip])] {
		nSkip++
	}
	if nSkip < len(cols)-1 {
		start := nSkip
		for nSkip = start; nSkip < len(cols) && !constrainedCols[strings.ToLower(cols[nSkip])]; nSkip++ {
		}
		if nSkip == start || nSkip >= len(cols) {
			return -1
		}
	}
	// Need the NEXT column to be constrained for skip-scan to apply.
	if !constrainedCols[strings.ToLower(cols[nSkip])] {
		return -1
	}
	return nSkip
}

func (e *SelectEngine) skipScanForColumnsWithStatName(idxName string, cols []string, constrainedCols map[string]bool, colOps map[string]string, nRow int64, statName string) *skipScanPlan {
	nSkip := skipScanSkipCount(cols, constrainedCols)
	if nSkip < 0 {
		return nil
	}
	// Read stat1: stat[i] = avg rows per distinct value of prefix length i+1.
	// SQLite's skip-scan activation runs recursively: at each depth K (where
	// K = saved_nEq = saved_nSkip, taking values 0, 1, ..., nSkip-1), the check
	// is `aiRowLogEst[K+1] >= 42` which in raw counts is `stat[K+1] >= 18`. All
	// depths must pass, so require every stat[1..nSkip] >= 18.
	statTokens := e.stat1Tokens(statName)
	if len(statTokens) < nSkip+1 {
		return nil
	}
	for i := 1; i <= nSkip; i++ {
		if statTokens[i] < skipScanMinRepeats {
			return nil
		}
	}
	// Estimate result rows. Skip-scan iterates over distinct leading-nSkip
	// tuples (count = stat[nSkip-1]/stat[nSkip]) and seeks an index range per
	// iteration; per-iteration result = stat[nSkip+1] avg rows.
	// For stat = [10000, 5000, 2000, 10], nSkip=1: 2 iters * 2000 = 4000.
	var estRows float64
	if statTokens[nSkip] > 0 {
		nIter := float64(statTokens[nSkip-1]) / float64(statTokens[nSkip])
		estRows = nIter * float64(statTokens[nSkip+1])
	} else {
		estRows = float64(nRow) * 0.5
	}

	// Compare against the regular (non-skip) btree index cost. The unconstrained
	// leading prefix means a regular index lookup on col K has cost stat[K] rows
	// (avg rows per distinct K value, across all leading prefixes; in SQLite's
	// LogEst this is `aiRowLogEst[K] - aiRowLogEst[0]` ≈ log(stat[K]) since
	// aiRowLogEst[0] = log(nRow) is shared with nOut init at line 3187). Skip-scan
	// only fires when its estRows is strictly less than the regular cost (in row
	// counts), matching SQLite's whereLoopInsert winner-take-all comparison.
	regularEst := float64(statTokens[nSkip])
	if estRows >= regularEst {
		return nil
	}

	// Format EQP condition: "(ANY(c0) AND ANY(c1) AND c_nSkip op ? ...)"
	conds := formatSkipScanConditions(cols, nSkip, colOps)
	return &skipScanPlan{
		indexName:   idxName,
		nSkip:       nSkip,
		conditions:  conds,
		estRows:     estRows,
		leadingCols: cols[:nSkip],
		allCols:     cols,
	}
}

// formatSkipScanConditions builds the EQP "(ANY(c0) AND ... ANY(cN-1) AND
// cN op ?)" condition string for a skip-scan plan. nSkip is the count of
// leading columns rendered as ANY(col); columns at position >= nSkip that
// appear in colOps are rendered with the matching operator (`=` for =,
// `>? AND <?` for BETWEEN, `IN` rendered as `=?` since the EQP doesn't list
// the full IN list). Columns without a constraint are omitted.
func formatSkipScanConditions(cols []string, nSkip int, colOps map[string]string) string {
	var parts []string
	for i, col := range cols {
		if i < nSkip {
			parts = append(parts, fmt.Sprintf("ANY(%s)", col))
			continue
		}
		op := colOps[strings.ToLower(col)]
		switch op {
		case "BETWEEN":
			parts = append(parts, fmt.Sprintf("%s>? AND %s<?", col, col))
		case "IN":
			parts = append(parts, fmt.Sprintf("%s=?", col))
		case "=":
			parts = append(parts, fmt.Sprintf("%s=?", col))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// constrainedColumnNames returns the set of column names that appear in WHERE
// as the left operand of a column-to-constant comparison (e.g. `b=345`).
// Used by skip-scan detection to identify the prefix of unconstrained cols.
// UnaryOp children (e.g. `+a=1`) are not considered to constrain the column.
func constrainedColumnNames(where sql.Expr, tableName string) map[string]bool {
	out := map[string]bool{}
	walkConstrainedCols(where, tableName, false, func(col, _ string) { out[col] = true })
	return out
}

// constrainedColOps maps column name -> operator (the first one found) for
// WHERE predicates. Used by skip-scan EQP formatting to render BETWEEN/IN/LIKE
// with the correct shape (e.g. `c>? AND c<?` for BETWEEN c 6 AND 7).
func constrainedColOps(where sql.Expr, tableName string) map[string]string {
	out := map[string]string{}
	walkConstrainedCols(where, tableName, true, func(col, op string) { out[col] = op })
	return out
}

// walkConstrainedCols visits every column-to-constant constraint in where,
// invoking mark with the lower-cased column name and its operator ("=",
// "BETWEEN", "IN"). recurseUnary selects whether UnaryOp subtrees descend:
// Unary + / - on a column reference (e.g. `+a=1`) is a no-op numeric coercion
// that does not constrain the column for skip-scan purposes (SQLite's
// skipscan3.test 1.2: `+a=1 AND c=32` becomes `(ANY(a) AND ANY(b) AND c=?)`),
// so the name collector skips those subtrees while the operator collector
// descends.
func walkConstrainedCols(where sql.Expr, tableName string, recurseUnary bool, mark func(col, op string)) {
	if where == nil {
		return
	}
	switch e := where.(type) {
	case *sql.BinaryOp:
		if e.Operator == "AND" || e.Operator == "OR" {
			walkConstrainedCols(e.Left, tableName, recurseUnary, mark)
			walkConstrainedCols(e.Right, tableName, recurseUnary, mark)
			return
		}
		if constraintOperandCol(e, tableName, "=", mark) {
			return
		}
		walkConstrainedCols(e.Left, tableName, recurseUnary, mark)
		walkConstrainedCols(e.Right, tableName, recurseUnary, mark)
	case *sql.Between:
		constrainedOperandCol(e.Operand, tableName, "BETWEEN", mark)
	case *sql.InList:
		constrainedOperandCol(e.Operand, tableName, "IN", mark)
	case *sql.ParenExpr:
		walkConstrainedCols(e.Expr, tableName, recurseUnary, mark)
	case *sql.UnaryOp:
		if recurseUnary {
			walkConstrainedCols(e.Operand, tableName, recurseUnary, mark)
		}
	}
}

// constraintOperandCol marks a comparison's constraint column when either
// side (left preferred) is a table-local column reference. Reports whether
// the constraint was consumed.
func constraintOperandCol(bin *sql.BinaryOp, tableName, op string, mark func(col, opv string)) bool {
	for _, side := range []sql.Expr{bin.Left, bin.Right} {
		if col, ok := side.(*sql.ColumnRef); ok {
			if col.Table == "" || strings.EqualFold(col.Table, tableName) {
				mark(strings.ToLower(col.Name), op)
				return true
			}
		}
	}
	return false
}

// constrainedOperandCol marks a BETWEEN/IN operand column when it is a
// table-local column reference.
func constrainedOperandCol(expr sql.Expr, tableName, op string, mark func(col, opv string)) {
	if col, ok := expr.(*sql.ColumnRef); ok {
		if col.Table == "" || strings.EqualFold(col.Table, tableName) {
			mark(strings.ToLower(col.Name), op)
		}
	}
}

// stat1Tokens returns the integer tokens of the sqlite_stat1 entry for the
// given index (or for the PRIMARY KEY of a WITHOUT ROWID table when idxName
// is "PRIMARY KEY"). Returns nil when no stat1 row is found.
func (e *SelectEngine) stat1Tokens(idxName string) []int64 {
	var tokens []int64
	e.forEachStat1Row(func(rec *storage.Record) bool {
		if len(rec.Values) < 3 {
			return true
		}
		idxStr, ok := rec.Values[1].(string)
		if !ok || !strings.EqualFold(idxStr, idxName) {
			return true
		}
		statStr, ok := rec.Values[2].(string)
		if !ok {
			return true
		}
		for _, f := range strings.Fields(statStr) {
			// Skip tokens containing '=' (e.g. "sz=20", "noskipscan").
			if strings.Contains(f, "=") {
				continue
			}
			n, err := strconv.ParseInt(f, 10, 64)
			if err == nil {
				tokens = append(tokens, n)
			}
		}
		return false
	})
	return tokens
}

// statHasNoSkipScan reports whether the sqlite_stat1 stat string for the
// given index contains the "noskipscan" hint (skip-scan optimization
// disabled).
func (e *SelectEngine) statHasNoSkipScan(idxName string) bool {
	var found bool
	e.forEachStat1Row(func(rec *storage.Record) bool {
		if len(rec.Values) < 3 {
			return true
		}
		idxStr, ok := rec.Values[1].(string)
		if !ok || !strings.EqualFold(idxStr, idxName) {
			return true
		}
		statStr, ok := rec.Values[2].(string)
		if !ok {
			return true
		}
		found = strings.Contains(strings.ToLower(statStr), "noskipscan")
		return false
	})
	return found
}

// primaryKeyStatName returns the sqlite_stat1 idx name used by a WITHOUT
// ROWID table's PRIMARY KEY. The naming depends on whether the PK has an
// associated autoindex (from UNIQUE constraints) or just the PK columns.
// For a plain WITHOUT ROWID PK, the stat1 idx column is the table name;
// for a sqlite_autoindex entry it is "sqlite_autoindex_<table>_N". We
// return either; the stat1 lookup tries both.
func (e *SelectEngine) primaryKeyStatName(tableName string) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return tableName
	}
	// Prefer an autoindex (autoindex_* entries on this table).
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName && entry.SQL == "" {
			return entry.Name
		}
	}
	return tableName
}

// withoutRowidPKCols returns the WITHOUT ROWID PRIMARY KEY column list for a
// table, or nil when the table isn't WITHOUT ROWID.
func (e *SelectEngine) withoutRowidPKCols(tableName string) []string {
	entry, err := e.ctx.Schema().FindTable(tableName)
	if err != nil || entry == nil {
		return nil
	}
	if !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return nil
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	pkCols := e.ctx.WithoutRowidPKColumns(entry.Name, entry, colDefs, false)
	if len(pkCols) == 0 {
		return nil
	}
	out := make([]string, len(pkCols))
	for i, c := range pkCols {
		out[i] = c.Name
	}
	return out
}
