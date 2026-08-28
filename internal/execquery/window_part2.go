// Package execquery implements query execution.
package execquery

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// This file owns window-function execution: computing per-row window values
// for SELECT columns that contain function calls with OVER clauses. The
// window pass runs after WHERE/JOIN/GROUP BY/HAVING have produced the
// intermediate row set, and before the final output rows are built.

// winRow pairs a source row with its original index in the input row set so
// window results computed on a sorted partition can be written back to the
// correct output position.
func (e *SelectEngine) frameOffset(b sql.FrameBound, row RowMap, isStart bool) (int, error) {
	errMsg := "frame ending offset must be a non-negative integer"
	if isStart {
		errMsg = "frame starting offset must be a non-negative integer"
	}
	if b.Expr == nil {
		return 0, fmt.Errorf("%s", errMsg)
	}
	// ROWS frame offsets must be constant non-negative integers evaluated at
	// prepare time; a column reference (e.g. "ROWS BETWEEN d FOLLOWING ...")
	// is rejected even when the column resolves via an outer row.
	if e.exprHasColumnRef(b.Expr) {
		return 0, fmt.Errorf("%s", errMsg)
	}
	v, err := e.ctx.EvalExpr(b.Expr, RowMap{})
	if err != nil {
		return 0, fmt.Errorf("%s", errMsg)
	}
	v = util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("%s", errMsg)
		}
		return int(x), nil
	case float64:
		if x < 0 || x != float64(int64(x)) {
			return 0, fmt.Errorf("%s", errMsg)
		}
		return int(x), nil
	default:
		// SQLite requires a constant non-negative integer for ROWS frame
		// offsets; a non-numeric value is an error.
		return 0, fmt.Errorf("%s", errMsg)
	}
}

// clampFrame clamps raw start/end row positions into valid [0, n] bounds.
func (e *SelectEngine) clampFrame(start, end, n int) (int, int, error) {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > n {
		end = n
	}
	return start, end, nil
}

// rangeFrameBounds computes a RANGE frame's [start, end) indices using ORDER
// BY value peers and offsets.
func (e *SelectEngine) rangeFrameBounds(over *sql.WindowDef, f *sql.WindowFrame, part []winRow, current int) (int, int, error) {
	n := len(part)
	if len(over.OrderBy) == 0 {
		return 0, n, nil
	}
	if !f.Between {
		// Shorthand "RANGE N PRECEDING" == BETWEEN N PRECEDING AND CURRENT ROW.
		start, err := e.rangeBoundIndex(f.Start, over.OrderBy, part, current, true)
		if err != nil {
			return 0, 0, err
		}
		return start, e.rangeEndIndexForCurrent(over.OrderBy, part, current), nil
	}
	// BETWEEN form: compute the numeric value range [lo, hi) once (both bounds
	// contribute their thresholds) and find the in-range rows directly.
	if lo, hi, hiInclusive, ok := e.rangeFrameValueRange(over, f, part, current); ok {
		start, end := e.rangeFrameBoundsInRange(over.OrderBy, part, current, lo, hi, hiInclusive)
		if start > end {
			start = end
		}
		return start, end, nil
	}
	start, err := e.rangeBoundIndex(f.Start, over.OrderBy, part, current, true)
	if err != nil {
		return 0, 0, err
	}
	end, err := e.rangeBoundIndex(f.End, over.OrderBy, part, current, false)
	if err != nil {
		return 0, 0, err
	}
	return e.clampFrame(start, end+1, n)
}

// rangeFrameValueRange computes the numeric value range [lo, hi] (inclusive
// at lo, exclusive at hi) that the BETWEEN frame covers for the current row.
// PRECEDING bounds extend toward smaller values for ASC (larger for DESC);
// FOLLOWING bounds the reverse. The start bound's offset sets lo, the end
// bound's offset sets hi. Only the first ORDER BY term participates.
func (e *SelectEngine) rangeFrameValueRange(over *sql.WindowDef, f *sql.WindowFrame, part []winRow, current int) (float64, float64, bool, bool) {
	// RANGE frames with multiple ORDER BY terms use the full peer group for
	// CURRENT ROW boundaries; the value-range path only looks at the first
	// term, so fall back to the peer-group index path.
	if len(over.OrderBy) > 1 && (f.Start.Kind == "CURRENT ROW" || f.End.Kind == "CURRENT ROW") {
		return 0, 0, false, false
	}
	curVal, err := e.evalWindowExprValueNoUnwrap(over.OrderBy[0].Expr, part[current].row)
	if err != nil {
		return 0, 0, false, false
	}
	curNum, ok := windowRangeNumeric(curVal)
	if !ok {
		return 0, 0, false, false
	}
	lo := curNum
	hi := curNum
	hiInclusive := true
	// Start bound. UNBOUNDED PRECEDING/FOLLOWING extend to the partition's
	// extremes; numeric offsets adjust lo/hi per the direction.
	switch {
	case f.Start.Kind == "UNBOUNDED PRECEDING" && !over.OrderBy[0].Desc:
		lo = math.Inf(-1)
	case f.Start.Kind == "UNBOUNDED PRECEDING" && over.OrderBy[0].Desc:
		hi = math.Inf(1)
	case f.Start.Kind == "UNBOUNDED FOLLOWING" && !over.OrderBy[0].Desc:
		hi = math.Inf(1)
	case f.Start.Kind == "UNBOUNDED FOLLOWING" && over.OrderBy[0].Desc:
		lo = math.Inf(-1)
	case f.Start.Kind == "CURRENT ROW" && !over.OrderBy[0].Desc:
		lo = curNum
	case f.Start.Kind == "CURRENT ROW" && over.OrderBy[0].Desc:
		hi = curNum
	case f.Start.Kind == "PRECEDING" && !over.OrderBy[0].Desc:
		lo = curNum - e.rangeOffsetVal(f.Start, part[current].row, true, &ok)
	case f.Start.Kind == "PRECEDING" && over.OrderBy[0].Desc:
		hi = curNum + e.rangeOffsetVal(f.Start, part[current].row, true, &ok)
	case f.Start.Kind == "FOLLOWING" && !over.OrderBy[0].Desc:
		lo = curNum + e.rangeOffsetVal(f.Start, part[current].row, true, &ok)
	case f.Start.Kind == "FOLLOWING" && over.OrderBy[0].Desc:
		hi = curNum - e.rangeOffsetVal(f.Start, part[current].row, true, &ok)
	}
	if !ok {
		return 0, 0, false, false
	}
	// End bound.
	switch {
	case f.End.Kind == "UNBOUNDED PRECEDING" && !over.OrderBy[0].Desc:
		hi = math.Inf(-1)
	case f.End.Kind == "UNBOUNDED PRECEDING" && over.OrderBy[0].Desc:
		lo = math.Inf(-1)
	case f.End.Kind == "UNBOUNDED FOLLOWING" && !over.OrderBy[0].Desc:
		hi = math.Inf(1)
	case f.End.Kind == "UNBOUNDED FOLLOWING" && over.OrderBy[0].Desc:
		lo = math.Inf(-1)
	case f.End.Kind == "CURRENT ROW" && !over.OrderBy[0].Desc:
		hi = curNum
	case f.End.Kind == "CURRENT ROW" && over.OrderBy[0].Desc:
		lo = curNum
	case f.End.Kind == "PRECEDING" && !over.OrderBy[0].Desc:
		hi = curNum - e.rangeOffsetVal(f.End, part[current].row, false, &ok)
	case f.End.Kind == "PRECEDING" && over.OrderBy[0].Desc:
		lo = curNum + e.rangeOffsetVal(f.End, part[current].row, false, &ok)
	case f.End.Kind == "FOLLOWING" && !over.OrderBy[0].Desc:
		hi = curNum + e.rangeOffsetVal(f.End, part[current].row, false, &ok)
	case f.End.Kind == "FOLLOWING" && over.OrderBy[0].Desc:
		lo = curNum - e.rangeOffsetVal(f.End, part[current].row, false, &ok)
	}
	if !ok {
		return 0, 0, false, false
	}
	return lo, hi, hiInclusive, true
}

// rangeOffsetVal evaluates a RANGE frame bound's numeric offset, setting ok to
// false on error (callers bail out of the value-range path).
func (e *SelectEngine) rangeOffsetVal(b sql.FrameBound, row RowMap, isStart bool, ok *bool) float64 {
	v, err := e.rangeFrameOffset(b, row, isStart)
	if err != nil {
		*ok = false
		return 0
	}
	return v
}

// rangeFrameBoundsInRange finds the [start, end) indices of rows whose value
// is in [lo, hi) within the sorted partition, using a two-pointer scan. NULL
// ORDER BY values are their own peer group: they sort at the partition's start
// (NULLS FIRST / ASC default) or end (NULLS LAST / DESC default) and are in
// frame only when the frame's boundary on their side is unbounded.
func (e *SelectEngine) rangeFrameBoundsInRange(orderBy []sql.OrderByTerm, part []winRow, current int, lo, hi float64, hiInclusive bool) (int, int) {
	n := len(part)
	inRange := func(num float64) bool {
		if num < lo {
			return false
		}
		if hiInclusive {
			return num <= hi
		}
		return num < hi
	}
	nullsAtStart := !(orderBy[0].NullsLast || (orderBy[0].Desc && !orderBy[0].NullsFirst))
	nullsInFrame := false
	if nullsAtStart {
		// NULLs at the partition start: in frame when the START bound is
		// unbounded (ASC: lo=-Inf; DESC: hi=+Inf).
		nullsInFrame = (!orderBy[0].Desc && math.IsInf(lo, -1)) || (orderBy[0].Desc && math.IsInf(hi, 1))
	} else {
		// NULLs at the partition end: in frame when the END bound is
		// unbounded (ASC: hi=+Inf; DESC: lo=-Inf).
		nullsInFrame = (!orderBy[0].Desc && math.IsInf(hi, 1)) || (orderBy[0].Desc && math.IsInf(lo, -1))
	}
	start := n
	for i := 0; i < n; i++ {
		v, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[i].row)
		if err != nil {
			continue
		}
		num, isNum := windowRangeNumeric(v)
		if !isNum {
			if nullsInFrame {
				start = i
			} else {
				continue
			}
		}
		if isNum && inRange(num) {
			start = i
		}
		if start != n {
			break
		}
	}
	end := start
	for i := start; i < n; i++ {
		v, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[i].row)
		if err != nil {
			continue
		}
		num, isNum := windowRangeNumeric(v)
		if !isNum {
			if nullsInFrame {
				end = i + 1
			}
			continue
		}
		if inRange(num) {
			end = i + 1
		}
	}
	return start, end
}

// rangeEndIndexForCurrent returns the index one past the last peer of the
// current row (RANGE ... AND CURRENT ROW includes all peer rows).
func (e *SelectEngine) rangeEndIndexForCurrent(orderBy []sql.OrderByTerm, part []winRow, current int) int {
	end := current + 1
	for end < len(part) && e.winRowsArePeers(orderBy, part[current], part[end]) {
		end++
	}
	return end
}

// rangeBoundIndex resolves one RANGE frame bound. isStart selects the start
// (UNBOUNDED PRECEDING) or end (UNBOUNDED FOLLOWING) unbounded semantics.
func (e *SelectEngine) rangeBoundIndex(b sql.FrameBound, orderBy []sql.OrderByTerm, part []winRow, current int, isStart bool) (int, error) {
	n := len(part)
	switch b.Kind {
	case "UNBOUNDED PRECEDING":
		return 0, nil
	case "UNBOUNDED FOLLOWING":
		return n - 1, nil
	case "CURRENT ROW":
		if isStart {
			// RANGE ... AND CURRENT ROW: start at the first peer of current.
			s := current
			for s > 0 && e.winRowsArePeers(orderBy, part[current], part[s-1]) {
				s--
			}
			return s, nil
		}
		return e.rangeEndIndexForCurrent(orderBy, part, current) - 1, nil
	case "PRECEDING", "FOLLOWING":
		off, err := e.rangeFrameOffset(b, part[current].row, isStart)
		if err != nil {
			return 0, err
		}
		return e.rangeOffsetIndex(orderBy, part, current, b.Kind, off, isStart)
	}
	return 0, nil
}

// rangeFrameOffset evaluates a RANGE frame bound's offset expression for the
// current row. SQLite RANGE offsets accept any non-negative NUMBER (integers
// and fractions like 4.5); non-numeric or negative values are errors. isStart
// selects the start/end error message ("frame starting/ending offset must be a
// non-negative number").
func (e *SelectEngine) rangeFrameOffset(b sql.FrameBound, row RowMap, isStart bool) (float64, error) {
	errMsg := "frame ending offset must be a non-negative number"
	if isStart {
		errMsg = "frame starting offset must be a non-negative number"
	}
	if b.Expr == nil {
		return 0, fmt.Errorf("%s", errMsg)
	}
	v, err := e.ctx.EvalExpr(b.Expr, row)
	if err != nil {
		return 0, fmt.Errorf("%s", errMsg)
	}
	v = util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("%s", errMsg)
		}
		return float64(x), nil
	case float64:
		if x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, fmt.Errorf("%s", errMsg)
		}
		return x, nil
	default:
		// SQLite accepts a numeric TEXT value (e.g. '2.0') as a RANGE offset;
		// non-numeric text ('' , '2.0x') is an error.
		if s, ok := v.(string); ok {
			s = strings.TrimSpace(s)
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				if f >= 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
					return f, nil
				}
			}
		}
		return 0, fmt.Errorf("%s", errMsg)
	}
}

// rangeOffsetIndex returns the partition index that starts the peer group at
// the RANGE boundary: rows whose first ORDER BY value is exactly off (a
// numeric offset) before/after the current row's value. SQLite RANGE offsets
// are value-based (numeric difference on the first ORDER BY term), not
// row-count based.
func (e *SelectEngine) rangeOffsetIndex(orderBy []sql.OrderByTerm, part []winRow, current int, dir string, off float64, isStart bool) (int, error) {
	if len(orderBy) == 0 {
		return 0, nil
	}
	curVal, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[current].row)
	if err != nil {
		return 0, nil
	}
	// A non-numeric current value: the RANGE offset frame is just the current
	// row's peer group (SQLite: RANGE offsets only apply to numeric values).
	curNum, curOK := windowRangeNumeric(curVal)
	if !curOK {
		start := e.rangePeerStart(orderBy, part, current)
		if isStart {
			return start, nil
		}
		end := start
		for end+1 < len(part) && e.winRowsArePeers(orderBy, part[current], part[end+1]) {
			end++
		}
		return end, nil
	}

	// Compute the value range [lo, hi] this bound includes. PRECEDING bounds
	// reach values before the current row (smaller for ASC, larger for DESC);
	// FOLLOWING bounds reach values after it.
	lo := curNum
	hi := curNum
	switch {
	case dir == "PRECEDING" && !orderBy[0].Desc:
		lo = curNum - off
	case dir == "PRECEDING" && orderBy[0].Desc:
		hi = curNum + off
	case dir == "FOLLOWING" && !orderBy[0].Desc:
		hi = curNum + off
	case dir == "FOLLOWING" && orderBy[0].Desc:
		lo = curNum - off
	}

	// The frame excludes the current row at the side the bound closes: a
	// PRECEDING end excludes values >= cur, a FOLLOWING start excludes values
	// <= cur. Adjust the inclusive bounds accordingly.
	//   START of a PRECEDING bound: [cur-off, cur]  (current included)
	//   END   of a PRECEDING bound: [cur-off, cur)  (current excluded)
	//   START of a FOLLOWING bound: (cur, cur+off]  (current excluded)
	//   END   of a FOLLOWING bound: [cur, cur+off]  (current included)
	n := len(part)
	if isStart {
		// The START boundary is the LOWEST index whose value is in the range,
		// scanning down from the current row. A FOLLOWING start excludes the
		// current row, so scanning begins one row past it.
		startAt := current
		lowest := current
		if dir == "FOLLOWING" {
			startAt = current + 1
			lowest = current + 1
		}
		if startAt >= n {
			startAt = n - 1
		}
		if lowest >= n {
			lowest = n - 1
		}
		for i := startAt; i >= 0; i-- {
			v, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[i].row)
			if err != nil {
				continue
			}
			num, isNum := windowRangeNumeric(v)
			if !isNum {
				break
			}
			inRange := num >= lo && num <= hi
			if dir == "FOLLOWING" {
				// The current row is excluded: values must be strictly greater.
				inRange = num > curNum && num <= hi
			}
			if inRange {
				lowest = i
			} else {
				break
			}
		}
		return lowest, nil
	}
	// The END boundary is the HIGHEST index whose value is in the range. A
	// PRECEDING end excludes the current row (scan down from current-1); a
	// FOLLOWING end includes it (scan up from current). The PRECEDING
	// threshold is INCLUSIVE: ASC ends at values >= cur-off, DESC at values
	// >= cur+off.
	if dir == "PRECEDING" {
		threshold := curNum - off
		if orderBy[0].Desc {
			threshold = curNum + off
		}
		last := current - 1
		for last >= 0 {
			v, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[last].row)
			if err != nil {
				last--
				continue
			}
			num, isNum := windowRangeNumeric(v)
			if !isNum {
				last--
				continue
			}
			if num >= threshold {
				break
			}
			last--
		}
		return last, nil
	}
	// FOLLOWING end: scan up from the current row (current included).
	last := current
	for last+1 < n {
		v, err := e.evalWindowExprValueNoUnwrap(orderBy[0].Expr, part[last+1].row)
		if err != nil {
			break
		}
		num, isNum := windowRangeNumeric(v)
		if !isNum {
			break
		}
		if num >= lo && num <= hi {
			last++
		} else {
			break
		}
	}
	return last, nil
}

// rangePeerStart returns the index of the first row in the current row's
// ORDER BY peer group (used when RANGE offsets cannot apply numerically).
func (e *SelectEngine) rangePeerStart(orderBy []sql.OrderByTerm, part []winRow, current int) int {
	s := current
	for s > 0 && e.winRowsArePeers(orderBy, part[current], part[s-1]) {
		s--
	}
	return s
}

// windowRangeNumeric converts a value to a float for RANGE offset arithmetic;
// returns false for non-numeric values (SQLite: only numeric ORDER BY values
// participate in RANGE offset frames).
func windowRangeNumeric(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case *util.ColumnValue:
		return windowRangeNumeric(util.UnwrapColumnValue(x))
	default:
		return 0, false
	}
}

// evalWindowExprValueNoUnwrap evaluates an expression against a row without
// unwrapping collated wrappers.
func (e *SelectEngine) evalWindowExprValueNoUnwrap(expr sql.Expr, row RowMap) (interface{}, error) {
	v, err := e.ctx.EvalExpr(expr, row)
	if err != nil {
		return nil, err
	}
	return unwrapCollatedValue(v), nil
}

// groupsFrameBounds computes a GROUPS frame's [start, end) indices: peer
// groups are counted as single units.
func (e *SelectEngine) groupsFrameBounds(over *sql.WindowDef, f *sql.WindowFrame, part []winRow, current int) (int, int, error) {
	n := len(part)
	if len(part) == 0 {
		return 0, 0, nil
	}
	// Precompute group boundaries: groupStart[i] = start index of i's peer group.
	groupStart, groupEnd := e.computePeerGroups(over.OrderBy, part)
	if !f.Between {
		// Shorthand "GROUPS N PRECEDING" == BETWEEN N PRECEDING AND CURRENT ROW.
		off, err := e.evalConstGroupOffset(f.Start)
		if err != nil {
			return 0, 0, err
		}
		start := e.groupsOffsetIndex(groupStart, current, "PRECEDING", off, true)
		return start, groupEnd[current] + 1, nil
	}
	start, err := e.groupsBoundIndex(f.Start, groupStart, groupEnd, current, true)
	if err != nil {
		return 0, 0, err
	}
	end, err := e.groupsBoundIndex(f.End, groupStart, groupEnd, current, false)
	if err != nil {
		return 0, 0, err
	}
	// A START bound beyond the partition (or before it for PRECEDING) means
	// the frame is empty.
	if start < 0 {
		return 0, 0, nil
	}
	return e.clampFrame(start, end+1, n)
}

// computePeerGroups fills groupStart/groupEnd for every row index: the
// inclusive bounds of its ORDER BY peer group.
func (e *SelectEngine) computePeerGroups(orderBy []sql.OrderByTerm, part []winRow) ([]int, []int) {
	n := len(part)
	groupStart := make([]int, n)
	groupEnd := make([]int, n)
	// With no ORDER BY, every row is a peer (rank() OVER () is 1 for all).
	if len(orderBy) == 0 {
		for k := range part {
			groupStart[k] = 0
			groupEnd[k] = n - 1
		}
		return groupStart, groupEnd
	}
	i := 0
	for i < n {
		j := i + 1
		for j < n && e.winRowsArePeers(orderBy, part[i], part[j]) {
			j++
		}
		for k := i; k < j; k++ {
			groupStart[k] = i
			groupEnd[k] = j - 1
		}
		i = j
	}
	return groupStart, groupEnd
}

// groupsBoundIndex resolves one GROUPS frame bound to a row index.
func (e *SelectEngine) groupsBoundIndex(b sql.FrameBound, groupStart, groupEnd []int, current int, isStart bool) (int, error) {
	n := len(groupStart)
	switch b.Kind {
	case "UNBOUNDED PRECEDING":
		return 0, nil
	case "UNBOUNDED FOLLOWING":
		return n - 1, nil
	case "CURRENT ROW":
		if isStart {
			return groupStart[current], nil
		}
		return groupEnd[current], nil
	case "PRECEDING", "FOLLOWING":
		// The offset expression is a constant; evaluate it against the
		// current row (offsets in GROUPS frames are constants).
		off, err := e.evalConstGroupOffset(b)
		if err != nil {
			return 0, err
		}
		idx := e.groupsOffsetIndex(groupStart, current, b.Kind, off, isStart)
		if idx < 0 {
			return idx, nil
		}
		// For an END bound, return the LAST row of the target group (the
		// frame includes the whole group).
		if !isStart {
			return groupEnd[idx], nil
		}
		return idx, nil
	}
	return 0, nil
}

// evalConstGroupOffset evaluates a GROUPS frame bound offset (which SQLite
// requires to be a constant non-negative integer).
func (e *SelectEngine) evalConstGroupOffset(b sql.FrameBound) (int, error) {
	if b.Expr == nil {
		return 0, fmt.Errorf("frame offset must be a non-negative integer")
	}
	// Evaluate the expression against an empty row: constants only.
	v, err := e.ctx.EvalExpr(b.Expr, RowMap{})
	if err != nil {
		return 0, fmt.Errorf("frame offset must be a non-negative integer")
	}
	v = util.UnwrapColumnValue(unwrapCollatedValue(v))
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return 0, fmt.Errorf("frame offset must be a non-negative integer")
		}
		return int(x), nil
	case float64:
		if x < 0 || x != float64(int64(x)) {
			return 0, fmt.Errorf("frame offset must be a non-negative integer")
		}
		return int(x), nil
	default:
		return 0, fmt.Errorf("frame offset must be a non-negative integer")
	}
}

// groupsOffsetIndex returns the row index of the peer group off groups before
// (PRECEDING) or after (FOLLOWING) the current row's group.
func (e *SelectEngine) groupsOffsetIndex(groupStart []int, current int, dir string, off int, isStart bool) int {
	n := len(groupStart)
	if off == 0 {
		return groupStart[current]
	}
	if dir == "PRECEDING" {
		// Walk back off groups. target starts at the current group's start;
		// each step moves to the previous group's start.
		target := groupStart[current]
		for i := 0; i < off; i++ {
			if target <= 0 {
				target = -1
				break
			}
			target = groupStart[target-1]
		}
		if target < 0 {
			// For an END (PRECEDING) bound, going before the partition means the
			// frame is empty; for a START bound it clamps to the first group.
			if !isStart {
				return -1
			}
			return 0
		}
		return target
	}
	// FOLLOWING: the group containing the current row's groupStart is the
	// group of index currentGroup. The group after it (off groups later) is
	// found by walking the groupStart boundaries forward.
	currentGroup := groupStart[current]
	// Find the start of the next group after currentGroup.
	target := currentGroup
	for i := 0; i <= off; i++ {
		// Find the end of the current target group.
		end := target
		for end+1 < n && groupStart[end+1] == target {
			end++
		}
		if i == off {
			return target
		}
		if end+1 >= n {
			// Going beyond the partition: an END bound clamps to the last
			// group; a START bound means the frame is empty.
			if !isStart {
				return n - 1
			}
			return -1
		}
		target = groupStart[end+1]
	}
	if target >= n {
		if !isStart {
			return n - 1
		}
		return -1
	}
	return target
}

// winRowsArePeers reports whether two rows are ORDER BY peers (equal on every
// ORDER BY term). An empty orderBy means there is no ORDER BY, so the whole
// partition is one peer group (SQLite: RANGE CURRENT ROW without ORDER BY
// frames the entire partition, and EXCLUDE GROUP removes it).
func (e *SelectEngine) winRowsArePeers(orderBy []sql.OrderByTerm, a, b winRow) bool {
	if len(orderBy) == 0 {
		return true
	}
	for _, ob := range orderBy {
		vi, errI := e.ctx.EvalExpr(ob.Expr, a.row)
		vj, errJ := e.ctx.EvalExpr(ob.Expr, b.row)
		if errI != nil || errJ != nil {
			continue
		}
		// Extract the raw value and its declared collation (a COLLATE-wrapped
		// column value carries its collation; the ORDER BY term's own collation
		// is the fallback).
		vi, collI := execexpr.ExtractValue(vi)
		vj, collJ := execexpr.ExtractValue(vj)
		coll := collI
		if coll == "" {
			coll = orderByTermCollation(ob.Expr)
		}
		_ = collJ
		if e.ctx.CompareValuesCollate(vi, vj, coll) != 0 {
			return false
		}
	}
	return true
}

// computeAggregateWindow fills window results for an aggregate window
// function (sum/min/max/count/avg/group_concat/total/median/etc.) by stepping
// the registered aggregate over each row's frame.
func (e *SelectEngine) computeAggregateWindow(fn *sql.FuncCall, over *sql.WindowDef, part []winRow, results []interface{}) error {
	reg, found := e.ctx.Functions().Find(fn.Name)
	if !found || reg.Type != function.TypeAggregate {
		// A non-aggregate scalar used as a window function: evaluate it per
		// row (SQLite errors for non-window functions used with OVER, but
		// scalar functions with OVER evaluate per row).
		for _, wr := range part {
			v, err := e.ctx.EvalExpr(fn, wr.row)
			if err != nil {
				results[wr.origIdx] = nil
				continue
			}
			results[wr.origIdx] = util.UnwrapColumnValue(unwrapCollatedValue(v))
		}
		return nil
	}

	// Validate nested aggregates inside the window function (skipped in GROUP
	// BY window mode, where the inner aggregate is the group's output column).
	if e.windowGroupOutputs == nil {
		if nested := e.findAggNestedAggregates(fn); nested != "" {
			return fmt.Errorf("misuse of aggregate function %s()", nested)
		}
	}

	for i, wr := range part {
		start, end, err := e.frameBounds(over, part, i)
		if err != nil {
			return err
		}
		if end < start {
			end = start
		}

		agg := reg.AggregateFn()
		var frameRows []RowMap
		if fn.Distinct {
			frameRows = e.dedupeAggRowsWindow(fn, part, start, end)
		} else {
			frameRows = make([]RowMap, 0, end-start)
			for j := start; j < end; j++ {
				if e.windowExcludesRow(over, i, j, part) {
					continue
				}
				if !e.aggRowPassesFilter(fn, part[j].row) {
					continue
				}
				frameRows = append(frameRows, part[j].row)
			}
		}
		// Aggregate ORDER BY terms apply within the frame.
		if len(fn.OrderBy) > 0 {
			frameRows = e.sortRowMapsByOrderBy(fn.OrderBy, frameRows)
		}
		for _, r := range frameRows {
			if err := agg.Step(e.windowEvalAggArgs(fn, r)); err != nil {
				return err
			}
		}
		result, _ := agg.Final()
		results[wr.origIdx] = result
	}
	return nil
}

// windowEvalAggArgs evaluates a window aggregate function's arguments against
// one row. In GROUP BY window mode, an argument that is itself an aggregate
// matching a GROUP BY output column (e.g. sum(b) in sum(sum(b)) OVER ...)
// resolves to the row's output-column value instead of being re-aggregated.
func (e *SelectEngine) windowEvalAggArgs(fn *sql.FuncCall, row RowMap) []interface{} {
	args := make([]interface{}, len(fn.Args))
	for i, arg := range fn.Args {
		resolved := false
		if e.windowGroupOutputs != nil {
			if af, ok := arg.(*sql.FuncCall); ok {
				name := sql.ExprString(af)
				// A nested aggregate precomputed over the aggregate input rows
				// (storeWindowNestedAggs) is stored under its expression-string
				// key (e.g. "sum(a)" in min(sum(a)) OVER ()); prefer it.
				if v, exists := row.Get(name); exists {
					args[i] = util.UnwrapColumnValue(v)
					resolved = true
				}
				for _, cn := range e.windowGroupOutputs {
					if strings.EqualFold(name, cn) {
						if v, exists := row.Get(cn); exists {
							args[i] = util.UnwrapColumnValue(v)
							resolved = true
							break
						}
					}
				}
				// Resolve by the SELECT-list alias (e.g. max(z) AS m).
				if !resolved {
					for _, sc := range e.windowGroupCols {
						if sc.As != "" && strings.EqualFold(sql.ExprString(sc.Expr), name) {
							if v, exists := row.Get(sc.As); exists {
								args[i] = util.UnwrapColumnValue(v)
								resolved = true
								break
							}
						}
					}
				}
			}
		}
		if resolved {
			continue
		}
		v, err := e.ctx.EvalExpr(arg, row)
		if err != nil {
			args[i] = nil
		} else {
			args[i] = util.UnwrapColumnValue(v)
		}
	}
	return args
}

// storeWindowNestedAggs precomputes plain aggregates nested inside window
// function columns (arguments and OVER clause) over the given input rows and
// stores each under its expression-string key in the destination row map. This
// makes SELECT min(sum(a)) OVER () FROM t work like SQLite: the inner sum(a)
// is a regular aggregate over the whole input (one row per group) and the
// window runs over those rows.
