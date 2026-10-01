package execquery

import (
	"sort"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// Direct column reads for the scan (vdbe.c OP_Column parity): a scan whose
// live consumers touch only a few declared columns decodes exactly those
// columns straight from the cell payload via storage.DecodeRecordColumns,
// instead of boxing the whole record (the full decode's per-row cost is
// dominated by value boxing and slot clearing for columns nobody reads).
//
// Eligibility is the zero-behavior-change contract: every consumer of the
// reused StructRow must read a DIRECT slot. Two statement shapes qualify:
//
//   - simple-aggregate feed mode (st.feed != nil): the feed steps only its
//     compiled slots and the WHERE evaluation reads only the WHERE-referenced
//     columns — both live in the scan decode plan's phase-1 index set. No
//     output rows or row maps exist in feed mode.
//   - all-bare-refs output (st.bareOutIdx != nil, no WHERE): appendRowOutput
//     peels only the bareOutIdx slots, and needMaps/posAgg/aggConsumesRows
//     are excluded (they consume every slot through row maps / clones).
//
// WITHOUT ROWID records (PK-first permutation) and tables with dropped
// columns (storage-position shift) decode through the historical paths: their
// declared slot ordinal differs from the storage position the direct read is
// keyed on. A corrupt payload (DecodeRecordColumns error) falls back to the
// historical per-row decode, reproducing its silent-truncation semantics.

// maxDirectDecodeCols caps the direct read's column list: with this few
// referenced columns the scan skips at least two boxed values per row.
const maxDirectDecodeCols = 3

// initDirectDecode computes st.directCols for an eligible scan, or leaves it
// nil to keep the historical decode paths. plan must be the scan's decode
// plan; feed may be nil.
func (st *scanState) initDirectDecode(feed *simpleAggFeed, plan scanDecodePlan) {
	st.directCols = st.directDecodeCols(feed, plan)
	if st.directCols != nil {
		st.directScratch = make([]interface{}, len(st.directCols))
	}
}

// directDecodeCols returns the scan's directly decoded slots, or nil when the
// historical decode paths must run. Shared exclusions first, then the
// per-shape slot sets.
func (st *scanState) directDecodeCols(feed *simpleAggFeed, plan scanDecodePlan) []int {
	if st.hasJoins || st.isSelectStar {
		return nil
	}
	if hasDroppedColumnDefs(st.colDefs) {
		return nil
	}
	if feed != nil {
		// Feed mode materializes no rows or row maps: needMaps is forced off
		// in newScanState, posAgg requires feed==nil, and aggConsumesRows
		// (true for every aggregate statement) only suppresses per-row output
		// building, which feed mode skips anyway — it cannot make an undecoded
		// slot readable.
		return st.capDirectCols(feedDecodeSlots(plan))
	}
	if st.needMaps || st.posAgg || st.aggConsumesRows {
		return nil
	}
	return st.capDirectCols(bareDecodeSlots(st.s, st.bareOutIdx))
}

// capDirectCols applies the effort guards: at most maxDirectDecodeCols
// referenced columns, and only when the record has columns the scan never
// reads (otherwise the direct walk saves nothing).
func (st *scanState) capDirectCols(cols []int) []int {
	if len(cols) > maxDirectDecodeCols || len(cols)+2 > st.activeColCount {
		return nil
	}
	return cols
}

// feedDecodeSlots returns the feed scan's direct slots: the decode plan's
// phase-1 index set (the statement's referenced columns — the feed's compiled
// argument slots and the WHERE's references among them). A WHERE containing a
// subquery keeps the full decode (correlated references may read any column
// of the outer row), the same gate the lazy decode uses.
func feedDecodeSlots(plan scanDecodePlan) []int {
	if !plan.useLazyDecode {
		return nil
	}
	return sortedIndexSet(plan.whereDecodeIndices)
}

// bareDecodeSlots returns the bare-projection scan's direct slots: the bare
// output columns. Bare scans take the direct read only without a WHERE
// clause: a WHERE keeps the lazy two-phase decode (whose phase-2 refill owns
// the full remaining-columns decode the bare output also reads).
func bareDecodeSlots(s *sql.SelectStmt, bareOutIdx []int) []int {
	if s.Where != nil || bareOutIdx == nil {
		return nil
	}
	return dedupSortedSlots(bareOutIdx)
}

// decodeRowDirect decodes the scan's referenced columns straight from the cell
// payload into the reused StructRow's slots, then applies the same ALTER TABLE
// ADD COLUMN defaults and affinity-plan pipeline (wrapping + INTEGER PRIMARY
// KEY rowid-alias fill) as fillStructRowFromTypes. Undecoded slots are left
// untouched — the eligibility contract guarantees no consumer reads them.
// The next row's decode overwrites every direct slot, so no stale value can
// leak between rows. A corrupt payload returns an error; the caller falls
// back to the historical decode for that row.
func (st *scanState) decodeRowDirect(payload []byte, rowID int64) error {
	count, err := storage.DecodeRecordColumns(payload, st.directCols, st.directScratch)
	if err != nil {
		return err
	}
	sr := st.reuseSRow
	sr.RowID = rowID
	values := sr.Values
	for i, slot := range st.directCols {
		if slot < count {
			values[slot] = st.directScratch[i]
		}
	}
	// Same missing-column default handling as fillStructRowFromTypes: rows
	// written before ALTER TABLE ADD COLUMN need the added column's DEFAULT
	// (only slots beyond the record's value count are filled).
	st.e.applyColumnDefaults(values, st.colDefs, count)
	if st.affPlan != nil {
		st.affPlan.apply(values, rowID)
	}
	return nil
}

// sortedIndexSet returns the index set's keys sorted ascending (non-nil, so
// an empty set still selects the direct path).
func sortedIndexSet(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// dedupSortedSlots returns the output slots sorted ascending with duplicates
// removed (SELECT c, c lists the same slot twice; one read serves both, and
// bareOutputSlots may resolve two columns to the same slot in any order).
func dedupSortedSlots(slots []int) []int {
	out := make([]int, 0, len(slots))
	out = append(out, slots...)
	sort.Ints(out)
	dedup := out[:0]
	for i, s := range out {
		if i > 0 && out[i-1] == s {
			continue
		}
		dedup = append(dedup, s)
	}
	return dedup
}

// hasDroppedColumnDefs reports whether any column definition was removed by
// ALTER TABLE DROP COLUMN (such tables store records without the dropped
// slots, so storage positions no longer match declared ordinals).
func hasDroppedColumnDefs(colDefs []sql.ColumnDef) bool {
	for i := range colDefs {
		if colDefs[i].Dropped {
			return true
		}
	}
	return false
}
