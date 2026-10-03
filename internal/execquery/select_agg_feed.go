package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// Simple-aggregate feed (src/vdbe.c OP_AggStep / OP_AggFinal): a SELECT whose
// output columns are bare aggregate calls from the numeric/counter family
// (COUNT/SUM/AVG/TOTAL over a plain column reference, or COUNT with no
// argument — COUNT(*)) and that carries no GROUP BY, HAVING, window
// functions, or DISTINCT aggregates, accumulates each aggregate directly from
// the scan's decoded row values. The generic path materializes a RowMap and a
// full output row for EVERY input row only to feed aggregate Step calls; the
// feed skips both, keeping per-row work at decode + WHERE + Step. Accumulation
// uses the registry's own Aggregator implementations, so the semantics (NULL
// skipping, TEXT numeric classification, int64→float SUM promotion,
// Kahan-Babuška-Neumaier compensation, empty-input values, "integer overflow")
// are exactly the generic path's. Every other statement shape fails the
// compile guards and keeps the generic path.

// feedRowidSlot marks an aggregate argument that reads the row's rowid (the
// rowid/_rowid_/oid pseudo-column reference, legal when no declared column
// shadows it).
const feedRowidSlot = -1

// simpleAggFeed is the statement-scoped feed: one compiled call per output
// column, in output order. It lives on the SelectEngine for the duration of
// one execRealTableSelect (saved/restored like outerRows) because the row
// loops (rowid seek, range seek, table scan) consume it from engine state.
// group, when non-nil, is the grouped-aggregate partition (select_agg_groupfeed.go):
// the statement is a GROUP BY feed and step routes to it.
type simpleAggFeed struct {
	calls []aggFeedCall
	group *groupFeedPartition
	// scratch carries each step's one-element argument slice. Reused across
	// steps and calls: the Step implementations never retain the argument
	// slice (aggFeedCall's contract), so one buffer serves the whole feed.
	scratch [1]interface{}
}

// aggFeedCall is one output column's aggregate: the registry aggregator plus
// where its per-row argument comes from. countStar steps with no argument
// (COUNT(*) counts rows); slot == feedRowidSlot reads the rowid; any other
// slot reads the decoded row value at that colDefs index. slot-based calls
// are restricted to COUNT/SUM/AVG/TOTAL, whose Step implementations never
// retain the argument slice (a reused one-element scratch buffer is safe).
type aggFeedCall struct {
	agg       function.Aggregator
	slot      int
	countStar bool
}

// compileSimpleAggFeed extracts the simple-aggregate feed for a real-table
// SELECT, or nil when the statement keeps the generic aggregate path.
func (e *SelectEngine) compileSimpleAggFeed(s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *simpleAggFeed {
	if s == nil || tableEntry == nil || len(s.Columns) == 0 || !e.aggFeedStatementEligible(s, tableEntry) {
		return nil
	}
	feed := &simpleAggFeed{}
	for _, col := range s.Columns {
		call, ok := e.compileAggFeedColumn(col, s, colDefs)
		if !ok {
			return nil
		}
		feed.calls = append(feed.calls, call)
	}
	return feed
}

// aggFeedStatementEligible reports the statement-level guards: one real
// rowid table, no GROUP BY/HAVING/DISTINCT/window/correlated-agg context,
// and no scan-order effect the feed cannot reproduce. Every false branch
// names the semantic hazard it excludes.
func (e *SelectEngine) aggFeedStatementEligible(s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	return aggFeedSingleRowidTable(e, s, tableEntry) && e.aggFeedEvaluationEligible(s)
}

// aggFeedSingleRowidTable reports the FROM-shape guards: the feed steps one
// plain rowid table's rows (no compound chain, joins, FROM subquery, schema
// table, or WITHOUT ROWID storage).
func aggFeedSingleRowidTable(e *SelectEngine, s *sql.SelectStmt, tableEntry *schema.Entry) bool {
	// The feed produces the single collapsed row and leaves compound merging
	// to finalizeSelectResult via the generic path's contract.
	if s.Union != nil || s.From.Subquery != nil || s.From.Name == "" || len(s.Joins) > 0 {
		return false
	}
	if len(s.GroupBy) > 0 || s.Having != nil || s.Distinct {
		return false
	}
	if IsSchemaTable(tableEntry.Name) {
		return false
	}
	// WITHOUT ROWID tables remap PK-first records inside the scan (wrOrder);
	// the feed reads declared-order slots from the rowid-table pipeline only.
	return !e.ctx.TableIsWithoutRowidEntry(tableEntry)
}

// aggFeedEvaluationEligible reports the evaluation-context guards: the
// generic path must not reorder or reroute the feed's row stream.
func (e *SelectEngine) aggFeedEvaluationEligible(s *sql.SelectStmt) bool {
	// reverse_unordered_selects feeds the generic path its rows in reverse; a
	// compensated float sum is order-sensitive in the last ulp, so the feed
	// only runs when the reversal would not apply.
	if e.ctx.ReverseUnordered() && len(s.OrderBy) == 0 && e.selectDepth == 1 {
		return false
	}
	// A WHERE-driven index scan reorders surviving rows into index key order
	// after the scan; the feed has no rows to reorder.
	if e.indexScanOrderIndex(s) != "" {
		return false
	}
	// Correlated outer contexts route aggregates to the outer rows
	// (execSelectOuterAgg / execSelectCorrelatedAgg run first).
	if e.outerRow != nil || len(e.OuterRows()) > 0 {
		return false
	}
	return !e.hasSubqueryWithCorrelatedAgg(s.Columns) && !e.selectHasWindowFuncs(s.Columns)
}

// compileAggFeedColumn compiles one output column into a feed call. The
// column must be an aggregate registry call with no DISTINCT, FILTER, or
// ORDER BY and no nested aggregate (all of which evaluate through dedicated
// generic paths).
func (e *SelectEngine) compileAggFeedColumn(col sql.SelectColumn, s *sql.SelectStmt, colDefs []sql.ColumnDef) (aggFeedCall, bool) {
	fn, ok := col.Expr.(*sql.FuncCall)
	if !ok {
		return aggFeedCall{}, false
	}
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		return aggFeedCall{}, false
	}
	if fn.Distinct || fn.Filter != nil || len(fn.OrderBy) > 0 {
		return aggFeedCall{}, false
	}
	if nested := e.findAggNestedAggregates(fn); nested != "" {
		return aggFeedCall{}, false
	}
	return compileAggFeedCall(fn, reg, s, colDefs)
}

// compileAggFeedCall compiles one aggregate output column. Only the
// numeric/counter family takes the feed: COUNT with zero or one argument,
// SUM/AVG/TOTAL with exactly one. The single argument must be a plain column
// reference (unqualified or qualified by the FROM table/alias) designating a
// stored declared column or the rowid pseudo-column. MIN/MAX single-argument
// reduce under the argument's collation (evalMinMaxAggregate) and
// GROUP_CONCAT and every other aggregate is order- or context-sensitive, so
// they fall back.
func compileAggFeedCall(fn *sql.FuncCall, reg *function.Func, s *sql.SelectStmt, colDefs []sql.ColumnDef) (aggFeedCall, bool) {
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		return compileAggFeedCount(fn, reg, s, colDefs)
	case "SUM", "AVG", "TOTAL":
		if len(fn.Args) != 1 {
			return aggFeedCall{}, false
		}
	default:
		return aggFeedCall{}, false
	}
	return aggFeedColumnArg(fn.Args[0], reg, s, colDefs)
}

// compileAggFeedCount compiles COUNT: no argument (COUNT(*)) or one column
// reference. COUNT(*) parses as a single star argument (the expression
// evaluator produces the non-nil "*" marker for it, so the generic counter
// counts every row); the feed steps it as a no-argument row count.
func compileAggFeedCount(fn *sql.FuncCall, reg *function.Func, s *sql.SelectStmt, colDefs []sql.ColumnDef) (aggFeedCall, bool) {
	switch {
	case len(fn.Args) == 0:
		return aggFeedCall{agg: reg.AggregateFn(), countStar: true}, true
	case len(fn.Args) == 1:
		if ref, ok := unwrapParenExpr(fn.Args[0]).(*sql.ColumnRef); ok && ref.Name == "*" {
			if ref.Table != "" && !aggFeedQualifierMatches(ref.Table, s) {
				return aggFeedCall{}, false
			}
			return aggFeedCall{agg: reg.AggregateFn(), countStar: true}, true
		}
		return aggFeedColumnArg(fn.Args[0], reg, s, colDefs)
	default:
		return aggFeedCall{}, false
	}
}

// aggFeedColumnArg compiles one aggregate argument: a plain column reference
// designating the rowid pseudo-column (no declared column shadows it) or a
// stored declared column of the scanned table.
func aggFeedColumnArg(arg sql.Expr, reg *function.Func, s *sql.SelectStmt, colDefs []sql.ColumnDef) (aggFeedCall, bool) {
	ref, ok := unwrapParenExpr(arg).(*sql.ColumnRef)
	if !ok || ref.Name == "*" {
		return aggFeedCall{}, false
	}
	if ref.Table != "" && !aggFeedQualifierMatches(ref.Table, s) {
		return aggFeedCall{}, false
	}
	if IsRowIDName(ref.Name) {
		if RowHasRowIDColumn(colDefs) {
			return aggFeedCall{}, false // a declared column shadows the pseudo-column
		}
		return aggFeedCall{agg: reg.AggregateFn(), slot: feedRowidSlot}, true
	}
	slot, ok := aggFeedArgSlot(colDefs, ref.Name)
	if !ok {
		return aggFeedCall{}, false
	}
	return aggFeedCall{agg: reg.AggregateFn(), slot: slot}, true
}

// aggFeedQualifierMatches reports whether a reference's qualifier names the
// scanned FROM table or its alias.
func aggFeedQualifierMatches(table string, s *sql.SelectStmt) bool {
	return strings.EqualFold(table, s.From.Name) ||
		(s.From.As != "" && strings.EqualFold(table, s.From.As))
}

// aggFeedArgSlot resolves an unqualified column reference to its colDefs
// slot (SQLite column names are case-insensitive). Dropped columns have no
// on-disk slot and generated columns are computed, so both fall back to the
// generic path.
func aggFeedArgSlot(colDefs []sql.ColumnDef, name string) (int, bool) {
	for i := range colDefs {
		cd := &colDefs[i]
		if cd.Dropped || cd.Generated != nil {
			continue
		}
		if strings.EqualFold(cd.Name, name) {
			return i, true
		}
	}
	return 0, false
}

// step accumulates one decoded row into every compiled call. The argument
// unwrapping mirrors evalAggCallArgs exactly (util.UnwrapColumnValue then
// unwrapCollatedValue), so the aggregate receives the same raw scalar it
// would receive through the row-map path. countStar steps with no argument.
// A grouped feed (GROUP BY partition) owns the whole step.
func (f *simpleAggFeed) step(values []interface{}, rowID int64) error {
	if f.group != nil {
		return f.group.step(values, rowID)
	}
	for ci := range f.calls {
		c := &f.calls[ci]
		if c.countStar {
			if err := c.agg.Step(nil); err != nil {
				return err
			}
			continue
		}
		var raw interface{}
		if c.slot == feedRowidSlot {
			raw = rowID
		} else {
			raw = values[c.slot]
		}
		f.scratch[0] = unwrapCollatedValue(util.UnwrapColumnValue(raw))
		if err := c.agg.Step(f.scratch[:1]); err != nil {
			return err
		}
	}
	return nil
}

// finishSimpleAggFeed consumes the statement's feed after its row loop ran,
// building the single aggregate output row through the registry Final calls
// and the shared finalize path. A zero-row input lands here too: each Final
// returns the same empty-input value the generic path's emptyAggValue yields
// (COUNT 0, TOTAL 0.0, SUM/AVG NULL). A grouped feed builds one output row
// per GROUP instead (finishGroupedAggFeed).
func (e *SelectEngine) finishSimpleAggFeed(s *sql.SelectStmt, feed *simpleAggFeed, colDefs []sql.ColumnDef) *Result {
	if feed.group != nil {
		return e.finishGroupedAggFeed(s, feed, colDefs)
	}
	columns := e.buildColumnNames(s.Columns, colDefs, s)
	outRow := make([]interface{}, len(feed.calls))
	for i := range feed.calls {
		v, err := feed.calls[i].agg.Final()
		if err != nil {
			// Mirror evalAggFuncCall's Final-error handling: park the error
			// and surface it as the statement result (func-37.x).
			e.aggPendingErr = err
			return &Result{Error: err}
		}
		outRow[i] = v
	}
	// The generic evalAggregates route finalizes its single aggregate row the
	// same way (ORDER BY/LIMIT/compound merging apply to the one-row result).
	return e.finalizeSelectResult(&Result{Columns: columns, Rows: [][]interface{}{outRow}}, s, nil)
}
