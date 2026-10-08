// Result finalization: the DISTINCT / ORDER BY / LIMIT / compound-merge
// stage shared by every SELECT dispatch path.

package execquery

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/sql"
)

// finalizeSelectResult applies DISTINCT, ORDER BY, LIMIT, and UNION.
func (e *SelectEngine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []RowMap) *Result {
	// A pending aggregate Step failure (e.g. zipfile's "out of memory"
	// raised inside a wrapping scalar expression whose plumbing drops the
	// per-expression error) outranks any computed rows: surface it now.
	if e.aggPendingErr != nil {
		err := e.aggPendingErr
		e.aggPendingErr = nil
		return &Result{Error: err}
	}
	// select.c sqlite3SelectCallback: a result set wider than
	// SQLITE_LIMIT_COLUMN errors "too many columns in result set"
	// (sqllimits1-17.0: nested SELECT *,*,* expansion). The flag is set by
	// buildColumnNames for any SELECT level (subqueries included).
	if e.resultTooWide {
		e.resultTooWide = false
		return &Result{Error: fmt.Errorf("too many columns in result set")}
	}
	// The collation of each result column of a compound query comes from the
	// leftmost SELECT member (SQLite's compound column collation rule).
	// DISTINCT, the compound merge, and ORDER BY resolution are the collation
	// list's only consumers; a statement with none of them (the dominant
	// point-lookup shape) skips the per-column walk entirely.
	colls := e.finalResultCollations(s)
	if s.Distinct {
		result.Rows, rowMaps = e.distinctRows(result.Rows, rowMaps, colls, s)
	}
	// Handle UNION before ORDER BY (ORDER BY on compound SELECT applies to the merged result).
	orderBy := s.OrderBy
	limit := s.Limit
	offset := s.Offset
	if s.Union != nil {
		var mergeErr error
		result.Rows, orderBy, limit, offset, mergeErr = e.mergeCompoundChain(result.Rows, s, colls, len(result.Columns))
		if mergeErr != nil {
			return &Result{Error: mergeErr}
		}
		// The head's rowMaps only cover its own rows; rebuild them from the
		// merged result so ORDER BY can resolve columns across all members.
		rowMaps = rebuildRowMapsFromRows(result.Rows, result.Columns)
	}
	if len(orderBy) > 0 {
		resolved, rerr := e.resolveFinalOrderBy(s, orderBy, result.Columns, colls)
		if rerr != nil {
			return &Result{Error: rerr}
		}
		if serr := e.sortRowsWithMaps(result, resolved, rowMaps, s); serr != nil {
			return &Result{Error: serr}
		}
	}
	lExpr, oExpr, lerr := e.evalLimitOffsetExprs(limit, offset)
	if lerr != nil {
		return &Result{Error: lerr}
	}
	result.Rows = applyLimitOffset(result.Rows, lExpr, oExpr)
	return result
}

// finalResultCollations returns the result collations the finalization stage
// needs (DISTINCT, the compound merge, ORDER BY resolution) — and nil for a
// statement with none of them (the dominant point-lookup shape), so the
// per-column walk is skipped entirely.
func (e *SelectEngine) finalResultCollations(s *sql.SelectStmt) []string {
	if !s.Distinct && s.Union == nil && len(s.OrderBy) == 0 {
		return nil
	}
	return e.selectOutputCollations(s)
}
