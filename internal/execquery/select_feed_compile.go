// Statement-local aggregate-feed compilation for a real-table SELECT.

package execquery

import (
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// compileStatementAggFeed builds the simple-aggregate feed (OP_AggStep parity):
// a bare COUNT/SUM/AVG/TOTAL select over one real rowid table accumulates
// straight from the row loop's decoded values. The grouped-aggregate feed
// extends the same machinery to GROUP BY statements whose output is bare
// aggregates and GROUP BY term projections (select_agg_groupfeed.go). The feed
// stays a statement-LOCAL value handed to the seek/scan loops and consumed by
// finishSimpleAggFeed — never engine state, so a nested statement (a WHERE
// subquery) can neither step nor consume an enclosing statement's feed.
//
// Both feeds produce a non-nil result only from aggregate columns (or, grouped,
// bare GROUP BY projections), so a statement with neither an aggregate nor a
// GROUP BY term skips the eligibility walk entirely — the point-SELECT shape
// pays nothing for the feed machinery.
func (e *SelectEngine) compileStatementAggFeed(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *simpleAggFeed {
	if !e.hasAggregatesCachedStmt(s) && len(s.GroupBy) == 0 {
		return nil
	}
	if feed := e.compileSimpleAggFeed(s, tableEntry, colDefs); feed != nil {
		return feed
	}
	if len(s.GroupBy) == 0 {
		return nil
	}
	return e.compileGroupedAggFeed(s, tableEntry, colDefs)
}
