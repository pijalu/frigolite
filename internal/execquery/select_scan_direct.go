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
		// The prefix walk's exact-count ceiling: a count below it means a
		// requested slot is absent (the record predates an ALTER TABLE ADD
		// COLUMN). An empty slot set (COUNT(*) — no referenced columns) keeps
		// the exact-count walk.
		if len(st.directCols) > 0 {
			st.directSlotCeil = st.directCols[len(st.directCols)-1] + 1
		}
		st.initDirectAffinity()
		st.initBarePassthrough(feed, plan)
	}
}

// initBarePassthrough detects the pure all-bare-refs scan: no WHERE, no feed,
// no joins/aggregate consumers (the bare output branch's own shape), and every
// output column a directly decoded stored slot listed in output order. Only
// that shape decodes straight into the flat output buffer
// (scanRowBarePassthrough): between the decode and the output there is no
// consumer at all.
func (st *scanState) initBarePassthrough(feed *simpleAggFeed, plan scanDecodePlan) {
	st.barePassthrough = feed == nil && plan.whereExpr == nil && !st.hasJoins &&
		st.flatStride == len(st.bareOutIdx) && st.bareOutIdx != nil &&
		sortedSlotsEqual(st.directCols, st.bareOutIdx)
	if st.barePassthrough {
		// An INTEGER PRIMARY KEY rowid-alias fill rides on the direct plan;
		// record where each filled slot lands in the output window.
		for _, slot := range st.directIPKIdx {
			st.ipkPassthroughPos = append(st.ipkPassthroughPos, sortedSlotIndex(st.directCols, slot))
		}
	}
}

// sortedSlotsEqual reports whether the two sorted slot lists are identical.
func sortedSlotsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortedSlotIndex returns the position of slot in the sorted slots slice.
func sortedSlotIndex(slots []int, slot int) int {
	for i, s := range slots {
		if s == slot {
			return i
		}
	}
	return 0 // unreachable: the fill's slots are direct slots
}

// initDirectAffinity restricts the scan's affinity plan to the direct slots
// (see the scanState field docs): the plan's wrap and IPK-fill entries for
// slots the direct read never decodes are dropped. When every plan entry is
// a direct slot the restricted application is identical to affinityPlan.apply.
func (st *scanState) initDirectAffinity() {
	p := st.affPlan
	if p == nil {
		return
	}
	for k, i := range p.wrapIdx {
		if sortedSlotPresent(st.directCols, i) {
			st.directWrapIdx = append(st.directWrapIdx, i)
			st.directWrapAff = append(st.directWrapAff, p.wrapAff[k])
			st.directWrapColl = append(st.directWrapColl, p.wrapColl[k])
		}
	}
	for k, i := range p.ipkIdx {
		if sortedSlotPresent(st.directCols, i) {
			st.directIPKIdx = append(st.directIPKIdx, i)
			st.directIPKAff = append(st.directIPKAff, p.ipkAff[k])
			st.directIPKColl = append(st.directIPKColl, p.ipkColl[k])
		}
	}
}

// sortedSlotPresent reports whether the sorted slots slice contains slot.
func sortedSlotPresent(slots []int, slot int) bool {
	lo, hi := 0, len(slots)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case slots[mid] < slot:
			lo = mid + 1
		case slots[mid] > slot:
			hi = mid - 1
		default:
			return true
		}
	}
	return false
}

// applyDirectAffinity is affinityPlan.apply restricted to the direct slots
// (initDirectAffinity): wrap the planned columns' values in place, then fill
// INTEGER PRIMARY KEY rowid-alias columns whose stored NULL stands for the
// rowid — but only for slots the direct read actually decodes.
func (st *scanState) applyDirectAffinity(values []interface{}, rowID int64) {
	for k, i := range st.directWrapIdx {
		if v := values[i]; v != nil {
			values[i] = wrapPrecomputed(st.directWrapAff[k], st.directWrapColl[k], v)
		}
	}
	for k, i := range st.directIPKIdx {
		if values[i] == nil {
			values[i] = wrapPrecomputed(st.directIPKAff[k], st.directIPKColl[k], rowID)
		}
	}
}

// scanRowBarePassthrough decodes the bare output columns straight into the
// flat output buffer. The pure all-bare-refs scan has no consumer between the
// decode and the output, and every wrapper the pipeline could apply peels to
// the raw decode value at output time (unwrapCollatedValue∘UnwrapColumnValue
// is the identity on raw values, and the INTEGER PRIMARY KEY rowid-alias fill
// peels to the rowid itself), so the reused row's slot round-trip, the
// wrapper application, and the output peel all collapse.
func (st *scanState) scanRowBarePassthrough(payload []byte, rowID int64) error {
	n := len(st.bareOutIdx)
	start := len(st.outValues)
	for i := 0; i < n; i++ {
		st.outValues = append(st.outValues, nil)
	}
	window := st.outValues[start : start+n : start+n]
	count, err := storage.DecodeRecordColumnsPrefix(payload, st.directCols, window)
	if err != nil || count < st.directSlotCeil {
		// A corrupt payload or a record written before ALTER TABLE ADD
		// COLUMN (a requested slot is absent — the prefix walk's count is
		// exact exactly when it is below the requested ceiling): run the
		// historical row pipeline for this row — its decode applies the
		// added column's DEFAULT and its corrupt-payload fallback
		// reproduces the full decode's silent-truncation semantics.
		st.outValues = st.outValues[:start]
		return st.scanRowDecoded(payload, rowID)
	}
	for k := range st.directIPKIdx {
		pos := st.ipkPassthroughPos[k]
		if window[pos] == nil {
			window[pos] = rowID
		}
	}
	st.outRowStarts = append(st.outRowStarts, start)
	return nil
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
// referenced columns, and only when the record has at least one column the
// scan never reads (with zero unread columns the direct walk saves nothing
// over the full decode — every stored value boxes either way).
func (st *scanState) capDirectCols(cols []int) []int {
	if len(cols) > maxDirectDecodeCols || len(cols)+1 > st.activeColCount {
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
	// Prefix walk: the header walk stops after the highest requested slot, so
	// a wide record costs lastCol+1 varint reads instead of its full column
	// count. The count is exact whenever it is below the ceiling; a cap means
	// every requested slot is present and no default can apply to a requested
	// slot (slots beyond the ceiling are never read).
	var count int
	var err error
	if st.directSlotCeil == 0 {
		count, err = storage.DecodeRecordColumns(payload, st.directCols, st.directScratch)
	} else {
		count, err = storage.DecodeRecordColumnsPrefix(payload, st.directCols, st.directScratch)
		if count < st.directSlotCeil {
			// A requested slot is absent (the record predates an ALTER TABLE
			// ADD COLUMN): re-read with the exact-count walk so the default
			// fills exactly the slots beyond the record's true value count.
			count, err = storage.DecodeRecordColumns(payload, st.directCols, st.directScratch)
		}
	}
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
	st.applyDirectAffinity(values, rowID)
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
