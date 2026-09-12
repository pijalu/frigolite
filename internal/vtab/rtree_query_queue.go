package vtab

import (
	"fmt"
)

// ---- priority-queue search (rtree.c rtreeSearchPoint machinery) ----
//
// Faithful port of ext/rtree/rtree.c's rtree search: a min-heap of
// RtreeSearchPoint entries ordered by (rScore asc, iLevel asc, insertion
// order) — rtreeSearchPointCompare — driven by rtreeStepToLeaf. The queue is
// what gives 2nd-generation query callbacks (sqlite3_rtree_query_callback)
// their observable row ORDER: the callback scores every cell (rScore) and the
// cursor always expands the lowest-scored pending node first.
//
// This search is ADDITIVE to the proven depth-first walk in rtree_query.go:
// openCursor routes here only when the pushed MATCH marker carries a
// 2nd-generation callback (RtreeGeometry.QueryFn != nil). Plain coordinate
// scans and 1st-generation geometry MATCHes stay on the DFS path, whose row
// order is identical to this queue's when all scores are equal (equal rScore
// → iLevel asc → children-first = depth-first), and which the green rtree
// suites (rtree9's cube/circle) pin byte-for-byte.
//
// Divergence note: C uses this queue for EVERY rtree search. frigolite keeps
// the DFS for score-free searches; a query mixing a 2nd-generation callback
// with other MATCH callbacks (unsupported: frigolite keeps one MATCH marker
// per scan) is the only shape where the two could disagree.

// rtreeSearchPoint mirrors RtreeSearchPoint (rtree.c): one pending node (or
// leaf entry, when iLevel==0) in the search queue. For iLevel>=1, id is the
// node number and iCell the next cell of that node to examine; for iLevel==0,
// id is the PARENT node number and iCell the entry's cell index within it.
type rtreeSearchPoint struct {
	rScore  float64
	id      int64
	iLevel  int
	eWithin int
	iCell   int
	seq     int64 // insertion sequence: FIFO tie-break like C's strict-less heap
}

// rtreeSearchQueue is the cursor's priority queue (RtreeCursor.aPoint plus
// the sPoint fast slot collapsed into one stable binary heap) and the
// per-level occupancy counters (RtreeCursor.anQueue) surfaced to query
// callbacks through RtreeQueryInfo.AnQueue.
type rtreeSearchQueue struct {
	aPoint  []rtreeSearchPoint
	anQueue [RTREE_MAX_DEPTH + 1]uint32
	seq     int64
}

// less mirrors rtreeSearchPointCompare: rScore asc, then iLevel asc; the
// insertion sequence keeps ties FIFO (C's heap uses strict-less swaps, which
// preserves insertion order among equal keys).
func (p *rtreeSearchPoint) less(o *rtreeSearchPoint) bool {
	if p.rScore != o.rScore {
		return p.rScore < o.rScore
	}
	if p.iLevel != o.iLevel {
		return p.iLevel < o.iLevel
	}
	return p.seq < o.seq
}

// newRTreeSearchQueue builds an empty queue (rtreeFilter's memset of
// anQueue happens here).
func newRTreeSearchQueue() *rtreeSearchQueue {
	return &rtreeSearchQueue{}
}

// len reports the number of pending search points.
func (q *rtreeSearchQueue) len() int { return len(q.aPoint) }

// push enqueues one point (rtreeEnqueue + rtreeSearchPointNew): the occupancy
// counter ticks first, then the entry sifts up while strictly less than its
// parent.
func (q *rtreeSearchQueue) push(p rtreeSearchPoint) {
	q.anQueue[p.iLevel]++
	q.seq++
	p.seq = q.seq
	q.aPoint = append(q.aPoint, p)
	i := len(q.aPoint) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !q.aPoint[i].less(&q.aPoint[parent]) {
			break
		}
		q.aPoint[i], q.aPoint[parent] = q.aPoint[parent], q.aPoint[i]
		i = parent
	}
}

// peek returns the front of the queue without removing it.
func (q *rtreeSearchQueue) peek() *rtreeSearchPoint { return &q.aPoint[0] }

// pop removes the front point (rtreeSearchPointPop): the occupancy counter of
// the popped level ticks down and the last entry sifts down from the root.
func (q *rtreeSearchQueue) pop() {
	n := len(q.aPoint) - 1
	q.anQueue[q.aPoint[0].iLevel]--
	q.aPoint[0] = q.aPoint[n]
	q.aPoint = q.aPoint[:n]
	i := 0
	for {
		left, smallest := 2*i+1, i
		if left >= n {
			break
		}
		right := left + 1
		if q.aPoint[left].less(&q.aPoint[smallest]) {
			smallest = left
		}
		if right < n && q.aPoint[right].less(&q.aPoint[smallest]) {
			smallest = right
		}
		if smallest == i {
			break
		}
		q.aPoint[i], q.aPoint[smallest] = q.aPoint[smallest], q.aPoint[i]
		i = smallest
	}
}

// containsID reports whether any PENDING point other than the front carries
// the given node id — the duplicate-child descent guard from rtreeStepToLeaf
// (rtree.c 1684-1688: the same child queued twice is corruption).
func (q *rtreeSearchQueue) containsID(id int64) bool {
	for i := 1; i < len(q.aPoint); i++ {
		if q.aPoint[i].id == id {
			return true
		}
	}
	return false
}

// collectDataRowsPQ walks the tree with the rtree.c priority queue, running
// the 2nd-generation query callback (match.QueryFn) against EVERY cell —
// interior bounding boxes and leaf entries — with full RtreeQueryInfo state.
// It returns the matching rows in queue order (rScore-driven; this IS the
// observable result order for a query without ORDER BY, rtreeE-1.4).
func (v *rtreeVTab[T]) collectDataRowsPQ(constraints []rtreeConstraint[T], rowids rtreeRowidSet, match *RtreeGeometry) ([][]interface{}, error) {
	root, err := v.rootAcquire()
	if err != nil {
		return nil, err
	}
	defer v.nodeRelease(root)
	mxLevel := root.depth() + 1

	q := newRTreeSearchQueue()
	info := &RtreeQueryInfo{
		NParam:     len(match.Params),
		AParam:     match.Params,
		ApSqlParam: match.SqlParams,
		ACoord:     make([]float64, v.nDim2),
		AnQueue:    q.anQueue[:],
		NCoord:     v.nDim2,
		MxLevel:    mxLevel,
	}
	q.push(rtreeSearchPoint{rScore: 0, id: 1, iLevel: mxLevel, eWithin: PartlyWithin})

	var out [][]interface{}
	for q.len() > 0 {
		p := q.peek()
		if p.iLevel == 0 {
			// Leaf entry: the cell passed every constraint when its leaf
			// node was expanded; surface id + coordinates.
			out = append(out, v.pqLeafRow(p))
			q.pop()
			continue
		}
		// Expand exactly ONE cell of the front node per iteration, like
		// rtreeStepToLeaf: after a push the loop re-reads the queue front,
		// so a better-scored child is expanded before the parent's next
		// cell (breadth-first interleaving).
		if err := v.pqExpandFront(q, match, info, p, constraints, rowids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pqLeafRow materializes the entry row carried by an iLevel==0 search point:
// the id column followed by the cell's coordinates.
func (v *rtreeVTab[T]) pqLeafRow(p *rtreeSearchPoint) []interface{} {
	node, err := v.nodeAcquire(p.id)
	if err != nil {
		return nil
	}
	cell := v.nodeGetCell(node, p.iCell)
	v.nodeRelease(node)
	row := make([]interface{}, 0, 1+v.nDim2)
	row = append(row, cell.iRowid)
	for j := 0; j < v.nDim2; j++ {
		row = append(row, coordToOut[T](cell.aCoord[j]))
	}
	return row
}

// pqExpandFront examines exactly one cell of the queue's front node per call
// (rtreeStepToLeaf's inner loop): constraints reduce eWithin/rScore,
// NOT_WITHIN cells advance past, and a surviving cell is enqueued as a child
// search point — popping the parent first when its cells are exhausted,
// exactly like C's "POP-S:" ordering.
func (v *rtreeVTab[T]) pqExpandFront(q *rtreeSearchQueue, match *RtreeGeometry, info *RtreeQueryInfo, p *rtreeSearchPoint, constraints []rtreeConstraint[T], rowids rtreeRowidSet) error {
	node, err := v.nodeAcquire(p.id)
	if err != nil {
		return err
	}
	defer v.nodeRelease(node)
	nCell := node.nCell()
	for p.iCell < nCell {
		cell := v.nodeGetCell(node, p.iCell)
		p.iCell++
		rScore, eWithin, cerr := v.pqEvalCell(match, info, p, &cell, constraints, rowids)
		if cerr != nil {
			return cerr
		}
		if eWithin == NotWithin {
			continue
		}
		xLevel := p.iLevel - 1
		xID, xCell := p.id, p.iCell-1
		if xLevel > 0 {
			xID, xCell = cell.iRowid, 0
			if q.containsID(xID) {
				return errCapitalized{"database disk image is malformed"}
			}
		}
		pqPushChild(q, p, nCell, xID, xLevel, xCell, rScore, eWithin)
		return nil
	}
	if p.iCell >= nCell {
		q.pop() // all cells consumed without a push ("POP-Se")
	}
	return nil
}

// pqPushChild enqueues the surviving cell as a child search point
// (rtreeStepToLeaf's tail): the score clamps to zero and the parent pops
// first when its cells are exhausted, exactly like C's "POP-S:" ordering.
func pqPushChild(q *rtreeSearchQueue, p *rtreeSearchPoint, parentNCell int, xID int64, xLevel, xCell int, rScore float64, eWithin int) {
	if p.iCell >= parentNCell {
		q.pop()
	}
	if rScore < 0 {
		rScore = 0 // rtreeStepToLeaf clamps a negative score to zero
	}
	q.push(rtreeSearchPoint{rScore: rScore, id: xID, iLevel: xLevel, eWithin: eWithin, iCell: xCell})
}

// pqEvalCell evaluates every pushed constraint against one cell of the node
// being expanded (rtreeStepToLeaf's constraint loop): the 2nd-generation
// callback runs with the search point's state; coordinate constraints apply
// as nonleaf MBR tests above the leaf level and exact tests at it; rowid
// constraints and `id IN (...)` membership apply at the leaf level only.
// Returns the reduced score (or -1 when no callback ran) and the reduced
// eWithin; NotWithin prunes the cell.
func (v *rtreeVTab[T]) pqEvalCell(match *RtreeGeometry, info *RtreeQueryInfo, p *rtreeSearchPoint, cell *RtreeCell[T], constraints []rtreeConstraint[T], rowids rtreeRowidSet) (float64, int, error) {
	eWithin, rScore, err := v.pqRunCallback(match, info, p, cell)
	if err != nil {
		return 0, NotWithin, err
	}
	if eWithin == NotWithin {
		return rScore, eWithin, nil
	}
	if v.pqCellPasses(p, cell, constraints, rowids) {
		return rScore, eWithin, nil
	}
	return rScore, NotWithin, nil
}

// pqRunCallback drives the 2nd-generation callback against one cell
// (rtreeCallbackConstraint): the callback sees the search point's level,
// parent score and parent visibility; entry cells refresh IRowid. Its
// eWithin and rScore reduce the cell's visibility/score (rScore starts at -1,
// meaning "no callback produced one").
func (v *rtreeVTab[T]) pqRunCallback(match *RtreeGeometry, info *RtreeQueryInfo, p *rtreeSearchPoint, cell *RtreeCell[T]) (int, float64, error) {
	atLeaf := p.iLevel == 1 // this node's cells are entries
	// rtreeCallbackConstraint sets the callback's view of the search point.
	info.ILevel = p.iLevel - 1
	info.RScore = p.rScore
	info.RParentScore = p.rScore
	info.EWithin = p.eWithin
	info.EParentWithin = p.eWithin
	if atLeaf {
		info.IRowid = cell.iRowid // only entry cells refresh iRowid (C parity)
	}
	for i, c := range cell.aCoord {
		info.ACoord[i] = asFloat64(c)
	}
	if err := match.InvokeQuery(info); err != nil {
		return NotWithin, -1, err
	}
	eWithin := FullyWithin
	if info.EWithin < eWithin {
		eWithin = info.EWithin
	}
	rScore := -1.0
	if info.RScore < rScore || rScore < 0 {
		rScore = info.RScore
	}
	return eWithin, rScore, nil
}

// pqCellPasses applies the non-callback constraints: `id IN (...)`
// membership and pushed rowid/coordinate comparisons (leaf-stage for rowid,
// C's leaf/nonleaf semantics for coordinates).
func (v *rtreeVTab[T]) pqCellPasses(p *rtreeSearchPoint, cell *RtreeCell[T], constraints []rtreeConstraint[T], rowids rtreeRowidSet) bool {
	atLeaf := p.iLevel == 1
	if atLeaf && rowids != nil {
		if _, ok := rowids[cell.iRowid]; !ok {
			return false
		}
	}
	for _, con := range constraints {
		switch {
		case con.col == 0:
			// Rowid comparison constraints apply to entries only.
			if atLeaf && !rowidPasses(con.op, cell.iRowid, con.value) {
				return false
			}
		case con.col-1 < len(cell.aCoord):
			if !v.pqCoordPasses(cell.aCoord, con, atLeaf) {
				return false
			}
		}
	}
	return true
}

// pqCoordPasses evaluates one coordinate constraint against the cell's
// bounding box: exact comparison at the leaf level, conservative MBR prune
// above it.
func (v *rtreeVTab[T]) pqCoordPasses(aCoord []T, con rtreeConstraint[T], atLeaf bool) bool {
	ci := con.col - 1
	if atLeaf {
		return pqLeafConstraint(con.op, asFloat64(aCoord[ci]), con.value, isInt32Coord[T]())
	}
	return pqNonleafConstraint(con.op, aCoord, ci, con.value, isInt32Coord[T]())
}

// pqLeafConstraint mirrors rtreeLeafConstraint: an exact coordinate test.
func pqLeafConstraint(op string, xN float64, value interface{}, integerCol bool) bool {
	rv := interfaceToFloat(applyColumnAffinity(value, integerCol))
	switch op {
	case "<":
		return xN < rv
	case "<=":
		return xN <= rv
	case ">":
		return xN > rv
	case ">=":
		return xN >= rv
	case "=", "<>", "!=":
		return xN == rv
	}
	return true
}

// pqNonleafConstraint mirrors rtreeNonleafConstraint: a conservative MBR
// prune. The subtree is skipped only when NO coordinate inside the child
// bounding box could satisfy the constraint.
func pqNonleafConstraint[T coordType](op string, aCoord []T, ci int, value interface{}, integerCol bool) bool {
	lower := asFloat64(aCoord[ci&0xFE])
	upper := asFloat64(aCoord[ci|1])
	rv := interfaceToFloat(applyColumnAffinity(value, integerCol))
	switch op {
	case "=", "<", "<=":
		return rv >= lower && (op != "=" || rv <= upper)
	case ">", ">=":
		return rv <= upper
	}
	return true
}

// interfaceToFloat widens an affinity-adjusted numeric literal to float64.
func interfaceToFloat(v interface{}) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// InvokeQuery runs the 2nd-generation callback; a marker without one reports
// the generic SQL error like SQLite's deserializeGeometry failure path.
func (g *RtreeGeometry) InvokeQuery(info *RtreeQueryInfo) error {
	if g == nil || g.QueryFn == nil {
		return fmt.Errorf("SQL logic error")
	}
	return g.QueryFn(info)
}
