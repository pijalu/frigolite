package execquery

import (
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// Table-scan row materialization, split out of select_scan.go for the
// file-size gate: the per-row decode -> WHERE -> output pipeline shared by
// the page-batch walker and the cursor loop.

func (st *scanState) scanRow(payload []byte, rowID int64, tableOffset int64, tableOffsetOK bool) error {
	st.rowsVisited++
	// sqlite_offset's OP_Offset source: this scan's table b-tree cursor
	// position (the row's cell payload start). Cleared state survives — the
	// scan entry saved and restores the outer scan's values.
	st.e.ctx.SetScanRowOffsets(tableOffset, tableOffsetOK, 0, false, nil)
	if st.barePassthrough {
		return st.scanRowBarePassthrough(payload, rowID)
	}
	return st.scanRowDecoded(payload, rowID)
}

// scanRowDecoded runs the general decode → WHERE → feed/output pipeline for
// one scanned cell.

func (st *scanState) scanRowDecoded(payload []byte, rowID int64) error {
	passesWhere, filtered, err := st.decodeAndFilterRow(payload, rowID)
	if err != nil {
		return err
	}
	if filtered {
		return nil
	}
	if st.hasJoins || passesWhere {
		if st.feed != nil {
			// Simple-aggregate feed: step from the decoded values; no output
			// rows or row maps (execSelectPostScan builds the result).
			if err := st.feed.step(st.reuseSRow.Values, rowID); err != nil {
				return err
			}
		} else if err := st.appendRowOutput(); err != nil {
			return err
		}
	}
	return nil
}

// scanState holds the per-scan configuration and output accumulators for
// scanTableRows, keeping the scan loop body small and low-complexity.
type scanState struct {
	e        *SelectEngine
	s        *sql.SelectStmt
	colDefs  []sql.ColumnDef
	hasJoins bool
	// rowsVisited counts the rows this scan stepped (SQLite's
	// SQLITE_STMTSTATUS_FULLSCAN_STEP counts the rows advanced past, i.e.
	// one less than the rows visited).
	rowsVisited  int64
	affinityCols map[string]bool
	// affPlan precomputes the per-column affinity/collation wrapping for the
	// scan (nil exactly when affinityCols is nil, preserving the old gate).
	affPlan *affinityPlan
	// ipkFillIdx holds the INTEGER PRIMARY KEY rowid-alias column indices
	// (always computed; drives the phase-2 rowid refill in lazy decode).
	ipkFillIdx []int
	reuseSRow  *StructRow
	// wrOrder permutes PK-first index-leaf records back to declared order
	// (nil for rowid tables and legacy table-leaf WR roots). Set by
	// execSelectScanPhase via initWROrder; decodeRowFull applies it.
	wrOrder []int
	// whereExpr is the WHERE clause this scan evaluates: s.Where with
	// index-usable LIKE/GLOB terms decorated with their synthesized prefix
	// range (select_like_opt.go). Identical to s.Where when no term qualifies.
	whereExpr     sql.Expr
	useLazyDecode bool
	// whereDecodeIndices/remainingDecodeIndices are the phase-1/phase-2
	// decode sets (the map plan's sets materialized by position — the decode
	// loop's per-column test is a slice index).
	whereDecodeIndices     []bool
	remainingDecodeIndices []bool
	isSelectStar           bool
	activeColCount         int
	needMaps               bool
	// bareOutIdx, when non-nil, lists the StructRow slot of every output
	// column of an all-bare-refs SELECT ("SELECT a, b FROM t"): the output
	// row is built by peeling the reused StructRow's slots directly,
	// skipping the per-column EvalExpr dispatch (appendRowOutput).
	bareOutIdx []int
	// feed, when non-nil, is the statement's simple-aggregate feed: surviving
	// rows step it (phase-1 decoded values only) and no output rows or row
	// maps are materialized; execSelectPostScan builds the aggregate result.
	feed *simpleAggFeed
	// aggConsumesRows marks statements whose output is rebuilt from the
	// scanned row maps by the aggregate / GROUP BY / window passes, so a
	// per-row output row built during the scan would be discarded work.
	aggConsumesRows bool
	// posAgg marks scans running in positional-aggregate mode: surviving rows
	// are retained as StructRow clones in aggRows (index-addressed, shared
	// column index) instead of per-row RowMaps. The aggregate passes read
	// them through the Row interface; the consumers that demand name-keyed
	// maps (window pass, nested aggregates, correlated outer sets) materialize
	// them lazily. Gated to exactly the shapes whose scan output only the
	// aggregate passes consume (see allowPositionalAggRows).
	posAgg bool
	// aggRows accumulates the positional rows of a posAgg scan; aggArena
	// chunks the per-row value slices (cloneReuseSRow) so retention costs one
	// small allocation per row.
	aggRows  []Row
	aggArena []interface{}
	// serialTypesBuf is the scan's reusable record-header type buffer
	// (parseRecordSerialTypesInto), reused across all rows of the scan.
	serialTypesBuf []uint64
	// directCols, when non-nil, selects the direct column-read decode
	// (decodeRowDirect): the sorted declared slots whose values the scan's
	// consumers read, decoded straight from the cell payload each row via
	// storage.DecodeRecordColumns. Eligibility in select_scan_direct.go.
	directCols    []int
	directScratch []interface{}
	// directWrap/directIPK are the affinity plan's application restricted to
	// the direct slots: every consumer of the reused StructRow reads a DIRECT
	// slot (the eligibility contract), so plan entries for undecoded slots are
	// dead work — an INTEGER PRIMARY KEY rowid-alias fill for a slot nobody
	// reads allocated a wrapper per row. Semantics match affinityPlan.apply on
	// the slots that survive the restriction (wrap skips NULL, fill replaces a
	// stored NULL with the rowid).
	directWrapIdx  []int
	directWrapAff  []rune
	directWrapColl []string
	directIPKIdx   []int
	directIPKAff   []rune
	directIPKColl  []string
	// directSlotCeil is max(directCols)+1: the prefix header walk's returned
	// count is exact exactly when it is below this ceiling; at or above it
	// every requested slot is present in the record.
	directSlotCeil int
	// barePassthrough marks the pure all-bare-refs scan (initBarePassthrough):
	// decode lands straight in the flat output buffer, skipping the reused
	// row's slot round-trip and the output unwrap.
	barePassthrough   bool
	ipkPassthroughPos []int
	// output accumulators
	outValues    []interface{}
	outRowStarts []int
	// flatStride is the per-row slot count of the flat output buffer: the
	// star column count for SELECT * scans, the bare-ref count for
	// all-bare-refs scans (a statement is one or the other — a star column
	// never qualifies as a bare ref); 0 leaves the rows to buildResultRows'
	// active-column default (star scans).
	flatStride  int
	nonStarRows [][]interface{}
	allRowMaps  []RowMap
}

// allowPositionalAggRows reports whether the caller (execSelectScanPhase)
// permits positional-aggregate retention for this scan: no WITHOUT ROWID PK
// reorder (it sorts the scan's row maps), no schema-table post-filter (it
// consumes the maps), no join (the join pass rebuilds maps), and no
// outer/correlated aggregate context (execSelectOuterAgg /
// execSelectCorrelatedAgg consume the maps first).
func (e *SelectEngine) allowPositionalAggRows(s *sql.SelectStmt, tableEntry *schema.Entry, withoutRowidPKCols []string) bool {
	if len(withoutRowidPKCols) > 0 || IsSchemaTable(tableEntry.Name) {
		return false
	}
	if len(s.Joins) > 0 || s.From.Subquery != nil {
		return false
	}
	return e.outerRow == nil && len(e.OuterRows()) == 0
}

// newScanState builds the scan configuration and reusable buffers for a table
// scan. The StructRow and flat output buffers are reused across all rows to
// avoid per-row allocation. allowPosAgg is the caller's positional-retention
// permission (allowPositionalAggRows); the scan adds its own statement-shape
// gates on top.
