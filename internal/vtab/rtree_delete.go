package vtab

import (
	"fmt"
)

// This file holds the r-tree deletion path (rtree.c rtreeDeleteRowid and its
// helpers: nodeRowidIndex, findLeafNode, deleteCell/fixLeafParent, removeNode
// and the reinsertion pass). Corruption of the shadow tables must surface as
// SQLite's generic malformed-image error at the same points rtree.c reports
// SQLITE_CORRUPT_VTAB.

// nodeRowidIndex returns the index of iRowid's cell within node. rtree.c
// nodeRowidIndex (rtree.c:1440) reports SQLITE_CORRUPT_VTAB when the rowid is
// absent — an internal text must not leak.
func (v *rtreeVTab[T]) nodeRowidIndex(node *rtreeNode[T], iRowid int64) (int, error) {
	nCell := node.nCell()
	for ii := 0; ii < nCell; ii++ {
		if v.nodeGetRowid(node, ii) == iRowid {
			return ii, nil
		}
	}
	return -1, errCapitalized{"database disk image is malformed"}
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
// nodes, else tighten the parent bounding box). rtree.c deleteCell runs
// fixLeafParent (rtree.c:2717-2748) first: the %_parent row of a non-root node
// is mandatory — a missing row is corruption, not a silent no-op fixup
// (rtree8-2.2.2: DELETE after `DELETE FROM t1_parent` must fail).
func (v *rtreeVTab[T]) deleteCell(node *rtreeNode[T], iCell, iHeight int) error {
	var parent *rtreeNode[T]
	if node.iNode != 1 {
		par, ok, err := v.getParent(node.iNode)
		if err != nil {
			return err
		}
		if !ok {
			return errCapitalized{"database disk image is malformed"}
		}
		p, err := v.nodeAcquire(par)
		if err != nil {
			return err
		}
		defer v.nodeRelease(p)
		parent = p
	}
	v.nodeDeleteCell(node, iCell)
	if parent == nil {
		// Root: no ancestor cell to adjust (C's assert(pParent || root)).
		return nil
	}
	if node.nCell() < v.minCells() {
		return v.removeNode(node, iHeight)
	}
	return v.fixBoundingBox(node)
}

// removeNode pulls node out of the tree (deleting its parent cell, cascading if
// the parent also becomes underfull) and schedules its content for reinsertion.
// C only reaches removeNode for nodes with a known parent; a missing %_parent
// row is corruption (the old internal "cannot remove root node" text must not
// leak either).
func (v *rtreeVTab[T]) removeNode(node *rtreeNode[T], iHeight int) error {
	par, ok, err := v.getParent(node.iNode)
	if err != nil {
		return err
	}
	if !ok {
		return errCapitalized{"database disk image is malformed"}
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
