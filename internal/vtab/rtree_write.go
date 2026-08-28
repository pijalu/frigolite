package vtab

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// RTREE_MINCELLS returns the minimum cell count for a non-root node, following
// rtree.c: ((iNodeSize-4)/nBytesPerCell)/3 (Guttman m = M/3).
func (v *rtreeVTab[T]) minCells() int {
	return ((v.iNodeSize - 4) / v.nBytesPerCell) / 3
}

// ---- geometry (coordinate boxes are generic over T ∈ {float32,int32}) ----

// cellArea returns the N-dimensional volume of the cell's bounding box.
func (v *rtreeVTab[T]) cellArea(p *RtreeCell[T]) float64 {
	area := 1.0
	for ii := 0; ii < v.nDim2; ii += 2 {
		area *= asFloat64(p.aCoord[ii+1]) - asFloat64(p.aCoord[ii])
	}
	return area
}

// cellMargin returns the sum of per-dimension sizes (used by the R*-tree split).
func (v *rtreeVTab[T]) cellMargin(p *RtreeCell[T]) float64 {
	margin := 0.0
	for ii := v.nDim2 - 2; ii >= 0; ii -= 2 {
		margin += asFloat64(p.aCoord[ii+1]) - asFloat64(p.aCoord[ii])
	}
	return margin
}

// cellUnion grows p1 so it also encloses p2 (mutates p1).
func (v *rtreeVTab[T]) cellUnion(p1, p2 *RtreeCell[T]) {
	for ii := 0; ii < v.nDim2; ii += 2 {
		if asFloat64(p2.aCoord[ii]) < asFloat64(p1.aCoord[ii]) {
			p1.aCoord[ii] = p2.aCoord[ii]
		}
		if asFloat64(p2.aCoord[ii+1]) > asFloat64(p1.aCoord[ii+1]) {
			p1.aCoord[ii+1] = p2.aCoord[ii+1]
		}
	}
}

// cellContains reports whether the area of p1 fully contains the area of p2.
func (v *rtreeVTab[T]) cellContains(p1, p2 *RtreeCell[T]) bool {
	for ii := 0; ii < v.nDim2; ii += 2 {
		if asFloat64(p2.aCoord[ii]) < asFloat64(p1.aCoord[ii]) ||
			asFloat64(p2.aCoord[ii+1]) > asFloat64(p1.aCoord[ii+1]) {
			return false
		}
	}
	return true
}

// cellOverlap sums, over aCell[0..nCell-1], the volume of the intersection with p.
func (v *rtreeVTab[T]) cellOverlap(p *RtreeCell[T], aCell []RtreeCell[T], nCell int) float64 {
	overlap := 0.0
	for ii := 0; ii < nCell; ii++ {
		o := 1.0
		for jj := 0; jj < v.nDim2; jj += 2 {
			x1 := math.Max(asFloat64(p.aCoord[jj]), asFloat64(aCell[ii].aCoord[jj]))
			x2 := math.Min(asFloat64(p.aCoord[jj+1]), asFloat64(aCell[ii].aCoord[jj+1]))
			if x2 < x1 {
				o = 0.0
				break
			}
			o *= (x2 - x1)
		}
		overlap += o
	}
	return overlap
}

// coordLess orders two cells by dimension iDim's lower bound (then upper bound).
func (v *rtreeVTab[T]) coordLess(c1, c2 RtreeCell[T], iDim int) bool {
	l1 := asFloat64(c1.aCoord[iDim*2])
	u1 := asFloat64(c1.aCoord[iDim*2+1])
	l2 := asFloat64(c2.aCoord[iDim*2])
	u2 := asFloat64(c2.aCoord[iDim*2+1])
	return l1 < l2 || (l1 == l2 && u1 < u2)
}

// sortByDimension sorts aIdx (a permutation of [0,nCell)) by cell aCoord[iDim*2]
// then aCoord[iDim*2+1], using aSpare as scratch (faithful merge sort port).
func (v *rtreeVTab[T]) sortByDimension(aIdx []int, nIdx, iDim int, aCell []RtreeCell[T], aSpare []int) {
	if nIdx <= 1 {
		return
	}
	nLeft := nIdx / 2
	nRight := nIdx - nLeft
	v.sortByDimension(aIdx[:nLeft], nLeft, iDim, aCell, aSpare)
	v.sortByDimension(aIdx[nLeft:], nRight, iDim, aCell, aSpare)
	copy(aSpare, aIdx[:nLeft])
	sL := aSpare[:nLeft]
	iLeft, iRight, pos := 0, 0, 0
	for iLeft < nLeft || iRight < nRight {
		if iLeft != nLeft && (iRight == nRight || v.coordLess(aCell[sL[iLeft]], aCell[aIdx[nLeft+iRight]], iDim)) {
			aIdx[pos] = sL[iLeft]
			iLeft++
		} else {
			aIdx[pos] = aIdx[nLeft+iRight]
			iRight++
		}
		pos++
	}
}

// ---- rowid allocation ----

// rtreeNewRowid reserves and returns a fresh entry rowid (rtree.c rtreeNewRowid:
// inserts a NULL row into %_rowid so SQLite assigns the next sequential id, then
// reads it back). We read max(rowid)+1 directly, which yields the same value.
func (v *rtreeVTab[T]) rtreeNewRowid() (int64, error) {
	mx, err := v.maxRowid()
	if err != nil {
		return 0, err
	}
	return mx + 1, nil
}

// ---- ChooseLeaf / AdjustTree ----

// ChooseLeaf descends to the leaf node that should contain cell, following
// SQLite's containment-then-minimal-growth policy (Gutman[84] ChooseLeaf).
func (v *rtreeVTab[T]) ChooseLeaf(cell *RtreeCell[T], iHeight int) (*rtreeNode[T], error) {
	pNode, err := v.rootAcquire()
	if err != nil {
		return nil, err
	}
	for ii := 0; ii < (v.iDepth - iHeight); ii++ {
		nCell := pNode.nCell()
		iBest := int64(0)
		bFound := false
		var fMinArea, fMinGrowth float64
		for iCell := 0; iCell < nCell; iCell++ {
			c := v.nodeGetCell(pNode, iCell)
			if v.cellContains(&c, cell) {
				area := v.cellArea(&c)
				if !bFound || area < fMinArea {
					iBest = c.iRowid
					fMinArea = area
					bFound = true
				}
			}
		}
		if !bFound {
			for iCell := 0; iCell < nCell; iCell++ {
				c := v.nodeGetCell(pNode, iCell)
				area := v.cellArea(&c)
				tmp := c
				v.cellUnion(&tmp, cell)
				growth := v.cellArea(&tmp) - area
				if iCell == 0 || growth < fMinGrowth || (growth == fMinGrowth && area < fMinArea) {
					fMinGrowth = growth
					fMinArea = area
					iBest = c.iRowid
				}
			}
		}
		child, err := v.nodeAcquire(iBest)
		if err != nil {
			v.nodeRelease(pNode)
			return nil, err
		}
		v.nodeRelease(pNode)
		pNode = child
	}
	return pNode, nil
}

// nodeParentIndex returns the index of the cell in parent whose rowid is iNode.
func (v *rtreeVTab[T]) nodeParentIndex(parent *rtreeNode[T], iNode int64) (int, error) {
	nCell := parent.nCell()
	for ii := 0; ii < nCell; ii++ {
		if v.nodeGetRowid(parent, ii) == iNode {
			return ii, nil
		}
	}
	return -1, fmt.Errorf("rtree: parent index not found")
}

// AdjustTree propagates cell's bounding box up the ancestor chain.
func (v *rtreeVTab[T]) AdjustTree(node *rtreeNode[T], cell *RtreeCell[T]) error {
	for {
		par, ok, err := v.getParent(node.iNode)
		if err != nil || !ok {
			return err
		}
		parent, err := v.nodeAcquire(par)
		if err != nil {
			return err
		}
		iCell, err := v.nodeParentIndex(parent, node.iNode)
		if err != nil {
			v.nodeRelease(parent)
			return err
		}
		c := v.nodeGetCell(parent, iCell)
		if !v.cellContains(&c, cell) {
			v.cellUnion(&c, cell)
			v.nodeOverwriteCell(parent, &c, iCell)
		}
		v.nodeRelease(parent)
		node = parent
	}
}

// fixBoundingBox recomputes node's bounding box from its cells and updates the
// cell referencing it in its parent (R-tree bottom-up fix).
func (v *rtreeVTab[T]) fixBoundingBox(node *rtreeNode[T]) error {
	par, ok, err := v.getParent(node.iNode)
	if err != nil || !ok {
		return err
	}
	parent, err := v.nodeAcquire(par)
	if err != nil {
		return err
	}
	defer v.nodeRelease(parent)
	nCell := node.nCell()
	box := v.nodeGetCell(node, 0)
	for ii := 1; ii < nCell; ii++ {
		c := v.nodeGetCell(node, ii)
		v.cellUnion(&box, &c)
	}
	box.iRowid = node.iNode
	iCell, err := v.nodeParentIndex(parent, node.iNode)
	if err != nil {
		return err
	}
	v.nodeOverwriteCell(parent, &box, iCell)
	return v.fixBoundingBox(parent)
}

// ---- SplitNode (R*-tree split) ----

// SplitNode redistributes node's cells plus the incoming cell into two nodes
// (Beckman[1990] R*-tree split), then re-links their parents and mappings.
func (v *rtreeVTab[T]) SplitNode(node *rtreeNode[T], cell *RtreeCell[T], iHeight int) error {
	nCell := node.nCell()
	aCell := make([]RtreeCell[T], nCell+1)
	for i := 0; i < nCell; i++ {
		aCell[i] = v.nodeGetCell(node, i)
	}
	v.nodeZero(node)
	aCell[nCell] = *cell
	nCell++

	var pLeft, pRight *rtreeNode[T]
	var leftbbox, rightbbox RtreeCell[T]

	if node.iNode == 1 {
		// Root split: root becomes the parent of two fresh children.
		pRight = v.nodeNew()
		pLeft = v.nodeNew()
		v.iDepth++
		node.setDepth(v.iDepth)
		node.dirty = true
	} else {
		pLeft = node
		pRight = v.nodeNew()
	}

	leftbbox.aCoord = make([]T, v.nDim2)
	rightbbox.aCoord = make([]T, v.nDim2)
	if err := v.splitNodeStartree(aCell, nCell, pLeft, pRight, &leftbbox, &rightbbox); err != nil {
		return err
	}

	if err := v.nodeWrite(pRight); err != nil {
		return err
	}
	if pLeft.iNode == 0 {
		if err := v.nodeWrite(pLeft); err != nil {
			return err
		}
	}

	rightbbox.iRowid = pRight.iNode
	leftbbox.iRowid = pLeft.iNode

	parNodeNo := int64(1)
	if node.iNode != 1 {
		par, ok, err := v.getParent(node.iNode)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("rtree: split node has no parent")
		}
		parNodeNo = par
	}
	parent, err := v.nodeAcquire(parNodeNo)
	if err != nil {
		return err
	}
	defer v.nodeRelease(parent)

	if node.iNode == 1 {
		if err := v.rtreeInsertCell(parent, &leftbbox, iHeight+1); err != nil {
			return err
		}
	} else {
		iCell, err := v.nodeParentIndex(parent, node.iNode)
		if err != nil {
			return err
		}
		v.nodeOverwriteCell(parent, &leftbbox, iCell)
		if err := v.AdjustTree(parent, &leftbbox); err != nil {
			return err
		}
	}
	if err := v.rtreeInsertCell(parent, &rightbbox, iHeight+1); err != nil {
		return err
	}

	newCellIsRight := false
	for i := 0; i < pRight.nCell(); i++ {
		iRowid := v.nodeGetRowid(pRight, i)
		if err := v.updateMapping(iRowid, pRight, iHeight); err != nil {
			return err
		}
		if iRowid == cell.iRowid {
			newCellIsRight = true
		}
	}
	if node.iNode == 1 {
		for i := 0; i < pLeft.nCell(); i++ {
			iRowid := v.nodeGetRowid(pLeft, i)
			if err := v.updateMapping(iRowid, pLeft, iHeight); err != nil {
				return err
			}
		}
	} else if !newCellIsRight {
		if err := v.updateMapping(cell.iRowid, pLeft, iHeight); err != nil {
			return err
		}
	}
	return nil
}

// splitNodeStartree performs the R*-tree axis/margin/overlap evaluation and
// assigns the cells to pLeft/pRight, filling their bounding boxes.
func (v *rtreeVTab[T]) splitNodeStartree(aCell []RtreeCell[T], nCell int, pLeft, pRight *rtreeNode[T], pBboxLeft, pBboxRight *RtreeCell[T]) error {
	aSorted := make([][]int, v.nDim)
	aSpare := make([]int, nCell)
	for ii := 0; ii < v.nDim; ii++ {
		aSorted[ii] = make([]int, nCell)
		for k := 0; k < nCell; k++ {
			aSorted[ii][k] = k
		}
		v.sortByDimension(aSorted[ii], nCell, ii, aCell, aSpare)
	}

	iBestDim := 0
	iBestSplit := 0
	fBestMargin := 0.0
	minCells := v.minCells()

	for ii := 0; ii < v.nDim; ii++ {
		margin := 0.0
		fBestOverlap := 0.0
		fBestArea := 0.0
		iBestLeft := 0
		for nLeft := minCells; nLeft <= nCell-minCells; nLeft++ {
			var left, right RtreeCell[T]
			left.aCoord = make([]T, v.nDim2)
			right.aCoord = make([]T, v.nDim2)
			v.copyCell(&left, &aCell[aSorted[ii][0]])
			v.copyCell(&right, &aCell[aSorted[ii][nCell-1]])
			for k := 1; k < nCell-1; k++ {
				if k < nLeft {
					v.cellUnion(&left, &aCell[aSorted[ii][k]])
				} else {
					v.cellUnion(&right, &aCell[aSorted[ii][k]])
				}
			}
			margin += v.cellMargin(&left)
			margin += v.cellMargin(&right)
			overlap := v.cellOverlap(&left, []RtreeCell[T]{right}, 1)
			area := v.cellArea(&left) + v.cellArea(&right)
			if nLeft == minCells || overlap < fBestOverlap || (overlap == fBestOverlap && area < fBestArea) {
				iBestLeft = nLeft
				fBestOverlap = overlap
				fBestArea = area
			}
		}
		if ii == 0 || margin < fBestMargin {
			iBestDim = ii
			fBestMargin = margin
			iBestSplit = iBestLeft
		}
	}

	v.copyCell(pBboxLeft, &aCell[aSorted[iBestDim][0]])
	v.copyCell(pBboxRight, &aCell[aSorted[iBestDim][iBestSplit]])
	for ii := 0; ii < nCell; ii++ {
		target := pLeft
		bbox := pBboxLeft
		if ii >= iBestSplit {
			target = pRight
			bbox = pBboxRight
		}
		c := aCell[aSorted[iBestDim][ii]]
		v.nodeInsertCell(target, &c)
		v.cellUnion(bbox, &c)
	}
	return nil
}

// copyCell copies c into dst (without sharing the slice).
func (v *rtreeVTab[T]) copyCell(dst, src *RtreeCell[T]) {
	dst.iRowid = src.iRowid
	dst.aCoord = make([]T, v.nDim2)
	copy(dst.aCoord, src.aCoord)
}

// updateMapping records the (rowid/nodeno) or (child/parent) mapping for iRowid.
func (v *rtreeVTab[T]) updateMapping(iRowid int64, node *rtreeNode[T], iHeight int) error {
	if iHeight == 0 {
		return v.setRowidMapping(iRowid, node.iNode)
	}
	return v.setParent(iRowid, node.iNode)
}

// rtreeInsertCell inserts cell into node (a subtree iHeight high). If node is
// full it triggers a SplitNode; otherwise it adjusts ancestry and writes the
// rowid/parent mapping for the inserted cell.
func (v *rtreeVTab[T]) rtreeInsertCell(node *rtreeNode[T], cell *RtreeCell[T], iHeight int) error {
	if node.nCell() >= v.maxCells() {
		return v.SplitNode(node, cell, iHeight)
	}
	v.nodeInsertCell(node, cell)
	if err := v.AdjustTree(node, cell); err != nil {
		return err
	}
	if iHeight == 0 {
		if err := v.setRowidMapping(cell.iRowid, node.iNode); err != nil {
			return err
		}
		return v.storeAuxColumns(cell.iRowid)
	}
	return v.setParent(cell.iRowid, node.iNode)
}

// ---- deletion ----

func (v *rtreeVTab[T]) nodeRowidIndex(node *rtreeNode[T], iRowid int64) (int, error) {
	nCell := node.nCell()
	for ii := 0; ii < nCell; ii++ {
		if v.nodeGetRowid(node, ii) == iRowid {
			return ii, nil
		}
	}
	return -1, fmt.Errorf("rtree: rowid not found in node")
}

// findLeafNode returns the leaf node currently holding iRowid's entry.
func (v *rtreeVTab[T]) findLeafNode(iRowid int64) (*rtreeNode[T], error) {
	nodeNo, ok, err := v.getRowidNode(iRowid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("rtree: rowid %d not found", iRowid)
	}
	return v.nodeAcquire(nodeNo)
}

// deleteCell removes cell iCell from node and fixes the tree (remove underfull
// nodes, else tighten the parent bounding box).
func (v *rtreeVTab[T]) deleteCell(node *rtreeNode[T], iCell, iHeight int) error {
	v.nodeDeleteCell(node, iCell)
	par, ok, err := v.getParent(node.iNode)
	if err != nil || !ok {
		return err
	}
	parent, err := v.nodeAcquire(par)
	if err != nil {
		return err
	}
	defer v.nodeRelease(parent)
	if node.nCell() < v.minCells() {
		return v.removeNode(node, iHeight)
	}
	return v.fixBoundingBox(node)
}

// removeNode pulls node out of the tree (deleting its parent cell, cascading if
// the parent also becomes underfull) and schedules its content for reinsertion.
func (v *rtreeVTab[T]) removeNode(node *rtreeNode[T], iHeight int) error {
	par, ok, err := v.getParent(node.iNode)
	if err != nil || !ok {
		return fmt.Errorf("rtree: cannot remove root node")
	}
	parent, err := v.nodeAcquire(par)
	if err != nil {
		return err
	}
	iCell, err := v.nodeParentIndex(parent, node.iNode)
	if err != nil {
		v.nodeRelease(parent)
		return err
	}
	if err := v.deleteCell(parent, iCell, iHeight+1); err != nil {
		v.nodeRelease(parent)
		return err
	}
	v.nodeRelease(parent)

	if _, err := v.module.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE nodeno=%d", v.shadow("node"), node.iNode)); err != nil {
		return err
	}
	if err := v.delParent(node.iNode); err != nil {
		return err
	}
	// Repurpose iNode to carry the subtree height for reinsertion; mark dead so
	// nodeFlush never persists this in-memory copy.
	node.dead = true
	node.iNode = int64(iHeight)
	node.parent = v.deleted
	v.deleted = node
	return nil
}

// reinsertNodeContent re-inserts every cell of a removed node into the tree.
func (v *rtreeVTab[T]) reinsertNodeContent(node *rtreeNode[T]) error {
	nCell := node.nCell()
	height := int(node.iNode)
	for i := 0; i < nCell; i++ {
		cell := v.nodeGetCell(node, i)
		pInsert, err := v.ChooseLeaf(&cell, height)
		if err != nil {
			return err
		}
		if err := v.rtreeInsertCell(pInsert, &cell, height); err != nil {
			v.nodeRelease(pInsert)
			return err
		}
		v.nodeRelease(pInsert)
	}
	return nil
}

// rtreeDeleteRowid removes the entry iDelete from the r-tree and rebalances the
// tree (mirrors rtree.c rtreeDeleteRowid).
func (v *rtreeVTab[T]) rtreeDeleteRowid(iDelete int64) error {
	root, err := v.rootAcquire()
	if err != nil {
		return err
	}
	defer v.nodeRelease(root)

	leaf, err := v.findLeafNode(iDelete)
	if err != nil {
		return err
	}
	iCell, err := v.nodeRowidIndex(leaf, iDelete)
	if err != nil {
		v.nodeRelease(leaf)
		return err
	}
	if err := v.deleteCell(leaf, iCell, 0); err != nil {
		v.nodeRelease(leaf)
		return err
	}
	v.nodeRelease(leaf)

	if err := v.delRowidMapping(iDelete); err != nil {
		return err
	}

	// Shrink the tree height when the root has a single child.
	if v.iDepth > 0 && root.nCell() == 1 {
		childNo := v.nodeGetRowid(root, 0)
		child, err := v.nodeAcquire(childNo)
		if err != nil {
			return err
		}
		if err := v.removeNode(child, v.iDepth-1); err != nil {
			v.nodeRelease(child)
			return err
		}
		v.nodeRelease(child)
		v.iDepth--
		root.setDepth(v.iDepth)
		root.dirty = true
	}

	for p := v.deleted; p != nil; {
		next := p.parent
		if err := v.reinsertNodeContent(p); err != nil {
			return err
		}
		p = next
	}
	v.deleted = nil
	return nil
}

// ---- RowUpdater (xUpdate) ----

// rtreeRowidFromValues extracts the rowid column value (column 0) as int64,
// applying SQLite's coercion: REAL truncates toward zero, TEXT contributes its
// numeric prefix ('4xxx' → 4, 'six' → 0 → auto-assign), NULL/other → 0.
//
// The zero result doubles as the auto-assign marker on the INSERT path only;
// delete/update paths must use rtreeRequiredRowid because rowid 0 is a legal
// stored key there (SQLite rowids start at 0 for explicit inserts).
func rtreeRowidFromValues(values []interface{}) int64 {
	if len(values) == 0 {
		return 0
	}
	switch x := values[0].(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		return int64(rtreeNumericPrefix(x))
	}
	return 0
}

// rtreeRequiredRowid extracts the rowid for delete/update paths: only an
// absent or SQL NULL first column counts as "no rowid"; an actual integer 0
// is the legitimate key of an explicitly inserted row.
func rtreeRequiredRowid(values []interface{}) (int64, error) {
	if len(values) == 0 || values[0] == nil {
		return 0, fmt.Errorf("rtree: cannot mutate entry without rowid")
	}
	return rtreeRowidFromValues(values), nil
}

// buildCellFromValues translates a VALUES tuple into an RtreeCell, validating
// the per-dimension min<=max constraint. It reports the rowid (0 => autogen).
func (v *rtreeVTab[T]) buildCellFromValues(values []interface{}) (int64, bool, RtreeCell[T], error) {
	var cell RtreeCell[T]
	if len(values) < 1+v.nDim2 {
		return 0, false, cell, fmt.Errorf("rtree: table %s has %d columns but %d values supplied", v.name, 1+v.nDim2, len(values))
	}
	rowid := rtreeRowidFromValues(values)
	// Auto-assign happens ONLY for an explicit NULL id (rtree.c rtreeUpdate);
	// any other value — including 0 and negatives — is a real rowid after
	// numeric-prefix coercion.
	hasRowid := len(values) > 0 && values[0] != nil
	if !hasRowid {
		rowid = 0
	}
	cell.aCoord = make([]T, v.nDim2)
	for i := 0; i < v.nDim2; i += 2 {
		lo := toCoord[T](values[1+i])
		hi := toCoord[T](values[1+i+1])
		if asFloat64(lo) > asFloat64(hi) {
			// SQLite wording (rtree.c rtreeInsertPoint): "rtree constraint
			// failed: t1.(x1<=x2)".
			return 0, false, cell, fmt.Errorf("rtree constraint failed: %s.(%s<=%s)",
				v.name, v.columns[1+i], v.columns[1+i+1])
		}
		cell.aCoord[i] = lo
		cell.aCoord[i+1] = hi
	}
	return rowid, hasRowid, cell, nil
}

// InsertRow implements vtab.RowUpdater: inserts one rtree entry, returning its
// rowid. It rebuilds the node cache for the operation and persists all changes.
func (v *rtreeVTab[T]) InsertRow(values []interface{}) (int64, error) {
	v.newNodeCache()
	v.deleted = nil

	rowid, hasRowid, cell, err := v.buildCellFromValues(values)
	if err != nil {
		return 0, err
	}
	if hasRowid {
		if _, exists, _ := v.getRowidNode(rowid); exists {
			return 0, &UniqueConstraintError{Table: v.name, Column: v.columns[0], RowID: rowid}
		}
		cell.iRowid = rowid
	} else {
		rowid, err = v.rtreeNewRowid()
		if err != nil {
			return 0, err
		}
		cell.iRowid = rowid
	}

	leaf, err := v.ChooseLeaf(&cell, 0)
	if err != nil {
		return 0, err
	}
	v.pendingAux = auxFromValues(values, 1+v.nDim2)
	if err := v.rtreeInsertCell(leaf, &cell, 0); err != nil {
		v.nodeRelease(leaf)
		return 0, err
	}
	v.nodeRelease(leaf)
	if err := v.nodeFlush(); err != nil {
		return 0, err
	}
	return rowid, nil
}

// DeleteRow implements vtab.RowUpdater.
func (v *rtreeVTab[T]) DeleteRow(oldValues []interface{}) error {
	rowid, err := rtreeRequiredRowid(oldValues)
	if err != nil {
		return fmt.Errorf("rtree: cannot delete entry without rowid")
	}
	v.newNodeCache()
	v.deleted = nil
	if err := v.rtreeDeleteRowid(rowid); err != nil {
		return err
	}
	return v.nodeFlush()
}

// UpdateRow implements vtab.RowUpdater: delete the old entry, insert the new
// one. A move onto an existing rowid raises UniqueConstraintError before any
// mutation (SQLite checks the target uniqueness first under all OR modes).
func (v *rtreeVTab[T]) UpdateRow(oldValues, newValues []interface{}) error {
	rowid, rerr := rtreeRequiredRowid(oldValues)
	if rerr != nil {
		return fmt.Errorf("rtree: cannot update entry without rowid")
	}
	v.newNodeCache()
	v.deleted = nil

	_, hasRowid, cell, err := v.buildCellFromValues(newValues)
	if err != nil {
		return err
	}
	newID := rowid // default: keep the old rowid when id not reassigned
	if hasRowid {
		if u := rtreeRowidFromValues(newValues); u != newID {
			if _, exists, _ := v.getRowidNode(u); exists {
				return &UniqueConstraintError{Table: v.name, Column: v.columns[0], RowID: u}
			}
			newID = u
		}
	}
	cell.iRowid = newID

	if err := v.rtreeDeleteRowid(rowid); err != nil {
		return err
	}
	leaf, err := v.ChooseLeaf(&cell, 0)
	if err != nil {
		return err
	}
	v.pendingAux = auxFromValues(newValues, 1+v.nDim2)
	if err := v.rtreeInsertCell(leaf, &cell, 0); err != nil {
		v.nodeRelease(leaf)
		return err
	}
	v.nodeRelease(leaf)
	return v.nodeFlush()
}

// auxFromValues returns values[start:] when non-empty, else nil.
func auxFromValues(values []interface{}, start int) []interface{} {
	if len(values) > start {
		return append([]interface{}(nil), values[start:]...)
	}
	return nil
}

// storeAuxColumns persists the pending auxiliary-column values into the
// %_rowid row (rtree.c stores only coordinates in %_node; aux columns live
// exclusively in the mapping table).
func (v *rtreeVTab[T]) storeAuxColumns(rowid int64) error {
	aux := v.pendingAux
	v.pendingAux = nil
	if len(aux) == 0 {
		return nil
	}
	sets := make([]string, len(aux))
	for i, val := range aux {
		sets[i] = fmt.Sprintf("a%d=%s", i, rtreeSQLLiteral(val))
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE rowid=%d",
		v.shadow("rowid"), strings.Join(sets, ","), rowid)
	_, err := v.module.db.ExecSQL(sql)
	return err
}

// rtreeSQLLiteral renders a SQL value for shadow-table text statements.
func rtreeSQLLiteral(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + fmt.Sprintf("%x", x) + "'"
	}
	return "NULL"
}
