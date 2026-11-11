// Package exec implements query execution.
//
// This file ports SQLite's planner cost arithmetic (where.c / util.c) for
// the single-table ORDER BY decision between a seek loop and the
// ORDER-BY-serving covering index walk. wherePathSolver prices the temp
// b-tree sorter into every loop that does not deliver the order
// (rCost = LogEstAdd(rUnsort, rSort) + 3) and gives a small bias (-2) to
// loops that do; the arithmetic is LogEst — 10*log2, integer, with C's
// rounding — so the sums reproduce C's verdicts at the probed boundaries
// (the walk wins the 100-row where-6.7 shapes, the seek wins at 100k rows).
package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// sqlite3LogEst converts a row count into its LogEst (an approximation of
// 10*log2(x)); a verbatim port of util.c's sqlite3LogEst.
func sqlite3LogEst(x uint64) int {
	a := [...]int{0, 2, 3, 5, 6, 7, 8, 9}
	y := 40
	if x < 8 {
		if x < 2 {
			return 0
		}
		for x < 8 {
			y -= 10
			x <<= 1
		}
	} else {
		for x > 255 {
			y += 40
			x >>= 4
		}
		for x > 15 {
			y += 10
			x >>= 1
		}
	}
	return a[x&7] + y - 10
}

// logEstAdd adds two LogEst values (the approximate 10*log2 of a+b); a
// verbatim port of util.c's sqlite3LogEstAdd.
func logEstAdd(a, b int) int {
	x := [...]int{
		10, 10, /* 0,1 */
		9, 9, /* 2,3 */
		8, 8, /* 4,5 */
		7, 7, 7, /* 6,7,8 */
		6, 6, 6, /* 9,10,11 */
		5, 5, 5, /* 12-14 */
		4, 4, 4, 4, /* 15-18 */
		3, 3, 3, 3, 3, 3, /* 19-24 */
		2, 2, 2, 2, 2, 2, 2, /* 25-31 */
	}
	if a >= b {
		if a > b+49 {
			return a
		}
		if a > b+31 {
			return a + 1
		}
		return a + x[a-b]
	}
	if b > a+49 {
		return b
	}
	if b > a+31 {
		return b + 1
	}
	return b + x[b-a]
}

// estLog estimates the logarithm of a LogEst value (the sorter's N*log(N)
// factor); a verbatim port of where.c's estLog.
func estLog(n int) int {
	if n <= 10 {
		return 0
	}
	return sqlite3LogEst(uint64(n)) - 33
}

// columnSizeEst mirrors build.c sqlite3AddColumn's per-column width
// estimate: the standard BLOB- and TEXT-affinity typenames carry szEst 5,
// every other declared (or absent) type carries 1.
func columnSizeEst(declType string) int {
	switch strings.ToUpper(strings.TrimSpace(declType)) {
	case "BLOB", "TEXT", "CHAR", "CLOB":
		return 5
	}
	return 1
}

// tableRowWidthLogEst estimates the table's average row width the way
// build.c estimateTableWidth does: the sum of the columns' size estimates
// plus one for the rowid when the table has no INTEGER PRIMARY KEY, times
// 4, through the LogEst.
func (e *SelectEngine) tableRowWidthLogEst(tableName string) int {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return 0
	}
	defs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	w := 0
	for _, cd := range defs {
		w += columnSizeEst(cd.Type)
	}
	if e.ipkAliasColumn(tableName) == "" {
		w++
	}
	return sqlite3LogEst(uint64(w * 4))
}

// indexRowWidthLogEst estimates the index's average row width the way
// build.c estimateIndexWidth does: the key columns' size estimates plus one
// for the trailing rowid of a rowid-table entry, times 4, through the
// LogEst.
func (e *SelectEngine) indexRowWidthLogEst(tableName, idxName string) int {
	cols := e.indexColumns(idxName)
	if len(cols) == 0 {
		return 0
	}
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return 0
	}
	sizes := map[string]int{}
	for _, cd := range e.ctx.ParseColumnDefs(entry.Name, entry.SQL) {
		sizes[strings.ToLower(cd.Name)] = columnSizeEst(cd.Type)
	}
	w := 0
	for _, c := range cols {
		if s, ok := sizes[strings.ToLower(c)]; ok {
			w += s
		} else {
			w++
		}
	}
	if len(e.withoutRowidPKCols(tableName)) == 0 {
		w++ // the trailing rowid
	}
	return sqlite3LogEst(uint64(w * 4))
}

// queryResultColumnCount counts the SELECT's output columns the way C's
// pEList->nExpr does: a "*" expands to every column of the FROM table.
func (e *SelectEngine) queryResultColumnCount(t queryTable, s *sql.SelectStmt) int {
	n := 0
	for _, c := range s.Columns {
		if ref, ok := c.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			entry, _, err := e.ctx.FindTable(t.real)
			if err != nil || entry == nil {
				n++
				continue
			}
			n += len(e.ctx.ParseColumnDefs(entry.Name, entry.SQL))
			continue
		}
		n++
	}
	return n
}

// limitRowCount returns the statement's LIMIT as a row count when it is a
// constant expression, ok=false otherwise (C's iLimit only caps the sorter
// cost for a known limit).
func (e *SelectEngine) limitRowCount(s *sql.SelectStmt) (int64, bool) {
	if s == nil || s.Limit == nil {
		return 0, false
	}
	if lit, isLit := s.Limit.(*sql.NumericLit); isLit {
		if v, err := strconv.ParseInt(strings.TrimSpace(lit.Value), 10, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// orderServingWalkCheaper runs wherePathSolver's cost comparison between
// the seek loop (which does not deliver the ORDER BY — its path pays the
// sorter) and the ORDER-BY-serving covering index walk (which delivers it
// and takes the no-sort bias). The formulas are where.c's: the seek's
// rRun = LogEstAdd(rLogSize, nOut + 1 + (15*szIdxRow)/szTabRow) plus the
// table-lookup term (nOut + 16) when the index does not cover the query,
// and the sorter's rSort = LogEst(nRowEst) + LogEst(cols) + estLog(nRow)
// with the LIMIT cap (+10) under WHERE_USE_LIMIT.
func (e *SelectEngine) orderServingWalkCheaper(t queryTable, s *sql.SelectStmt, seek scanLoop, walkToken string) bool {
	nRow := int64(seek.nRow)
	if nRow <= 0 {
		return false
	}
	est := int64(seek.bestEst)
	if est <= 0 {
		est = 1
	}
	rSize := sqlite3LogEst(uint64(nRow))
	rSeek := sqlite3LogEst(uint64(est))
	szTab := e.tableRowWidthLogEst(t.real)
	szIdx := e.indexRowWidthLogEst(t.real, seek.token)
	if szTab <= 0 {
		szTab = 1
	}
	// The seek loop (whereLoopAddBtree's constrained branch): one key
	// search plus stepping nOut entries.
	rSeekRun := logEstAdd(rSize, rSeek+1+(15*szIdx)/szTab)
	if !e.indexCoversQuery(t, s, seek.token) {
		// Not WHERE_IDX_ONLY: every output row pays a table lookup.
		rSeekRun = logEstAdd(rSeekRun, rSeek+16)
	}
	// The sorter (whereSortingCost, nothing pre-sorted: isOrdered==0).
	nCol := sqlite3LogEst(uint64((e.queryResultColumnCount(t, s)+59)/30))
	rSort := rSeek + nCol
	if lim, ok := e.limitRowCount(s); ok {
		rSort += 10 // TUNING: extra 2.0x if using LIMIT
		nSort := rSeek
		if lim < int64(nSort) {
			// C compares the plain LIMIT count against the LogEst row
			// estimate and assigns the count verbatim (where.c:5455).
			nSort = int(lim)
		}
		rSort += estLog(nSort)
	} else {
		rSort += estLog(rSeek)
	}
	seekCost := logEstAdd(rSeekRun, rSort) + 3
	// The delivering walk (whereLoopAddBtree's full-index-scan branch,
	// covering: rRun = rSize + 1 + (15*szIdxRow)/szTabRow) and the
	// no-sort bias (where.c:5849's rUnsort -= 2).
	walkRun := rSize + 1 + (15*e.indexRowWidthLogEst(t.real, walkToken))/szTab
	return walkRun-2 < seekCost
}
