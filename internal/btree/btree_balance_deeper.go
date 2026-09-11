package btree

// Root-leaf overflow resolution: btree.c balance_deeper (src/btree.c:9010).
//
// A root leaf can be unable to hold a cell whose local/overflow split is
// mandated by the file-format formula (storage.LocalPayloadSize): page 1 —
// the sqlite_schema root — loses 100 bytes of usable area to the database
// file header, so a fully-local cell of up to maxLocal bytes fits the
// formula but not page 1's content area. SQLite never shrinks such a
// cell's local portion below the formula: balance() detects the overfull
// ROOT (src/btree.c:9115-9129) and runs balance_deeper, which allocates a
// fresh child leaf, copies the root's content into it (copyNodeContent)
// and rewrites the root as an interior page whose rightmost pointer is the
// child (zeroPage(pRoot, flags & ~PTF_LEAF)). The pending cell then lands
// on the child — whose full-size area always fits the largest legal cell —
// and the balance do-loop's next iteration balances the child.

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// balanceDeeperRootLeaf promotes an overfull root leaf through
// balance_deeper and inserts the pending cell into the fresh child.
// parentPgno must be 0 (the caller only routes root leaves here).
// It returns no splits: the tree is fully wired when it returns.
func (t *BTree) balanceDeeperRootLeaf(pg *pager.Page, page *storage.BTreePage, newCell *storage.Cell, cellData []byte, coff int) ([]leafSplitResult, error) {
	// Fresh child leaf parented to the root (allocateBtreePage called with
	// pRoot->pgno so the ptrmap entry is PTRMAP_BTREE parent=pRoot->pgno,
	// src/btree.c:9028).
	child, err := t.allocBtreeNode(pg.PageNum)
	if err != nil {
		return nil, err
	}
	if err := t.copyLeafRootToChild(child, pg, page, coff); err != nil {
		return nil, err
	}
	if err := t.rewriteRootLeafAsInterior(pg, child.PageNum, coff); err != nil {
		return nil, err
	}
	// The balance do-loop's next iteration balances the child: insert the
	// pending cell there with the normal machinery. A fresh child always
	// has room for a cell the root could not hold, so splits never actually
	// occur on this insert; routing through insertPage keeps the shape
	// general (any root leaf, any page size) and applyChildSplits' right-
	// most-pointer branch would wire a split's divider chain to the fresh
	// interior root.
	splits, err := t.insertPage(child.PageNum, pg.PageNum, newCell)
	if err != nil {
		return nil, err
	}
	if len(splits) > 0 {
		rootPage, perr := storage.ParsePage(pg.Data, int(t.pageSize), coff)
		if perr != nil {
			return nil, perr
		}
		if aerr := t.applyChildSplits(pg, rootPage, child.PageNum, splits); aerr != nil {
			return nil, aerr
		}
	}
	return nil, nil
}

// copyLeafRootToChild moves the root leaf's existing cells to the freshly
// allocated child page (copyNodeContent, src/btree.c:9021). The cell set
// fit the root's (header-reduced) area, so it fits the child's full-size
// area. writeLeafHalf rebuilds the b-tree header and cell pointer array at
// the child's content offset (0 vs page 1's 100); cell data offsets are
// page-absolute and copy verbatim. Moved cells take their overflow chains:
// each chain's first page is re-parented to the child (ptrmapPutOvflPtr,
// src/btree.c:8025).
func (t *BTree) copyLeafRootToChild(child, root *pager.Page, page *storage.BTreePage, coff int) error {
	cellType := storage.CellTableLeaf
	if !t.isTable {
		cellType = storage.CellIndexLeaf
	}
	var moved []splitEntry
	for i := uint16(0); i < page.CellCount; i++ {
		cellOff := int(storage.CellPointer(root.Data, coff, int(i), int(t.pageSize)))
		c, err := storage.DecodeCell(root.Data, cellOff, cellType, int(t.usableSize))
		if err != nil {
			return err
		}
		moved = append(moved, splitEntry{cell: c, cellData: storage.EncodeCell(c)})
	}
	child.Data[0] = root.Data[coff] // the child inherits the leaf page type
	if err := writeLeafHalf(child, 0, moved, int(t.pageSize)); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(child.Data[int(t.pageSize)-4:int(t.pageSize)], 0) // no right sibling
	if err := t.pager.WritePage(child); err != nil {
		return err
	}
	return t.reparentPageOverflowChains(child.PageNum)
}

// rewriteRootLeafAsInterior zeroes the root leaf's b-tree content and
// reinstalls it as an interior page whose single pointer is the rightmost
// child (zeroPage(pRoot, flags & ~PTF_LEAF) + put4byte right-child,
// src/btree.c:9037-9039). The rewrite starts at the b-tree header offset,
// so page 1 keeps its 100-byte database file header.
func (t *BTree) rewriteRootLeafAsInterior(root *pager.Page, rightmost uint32, coff int) error {
	for i := coff; i < int(t.pageSize); i++ {
		root.Data[i] = 0
	}
	if t.isTable {
		root.Data[coff] = storage.PageTypeInteriorTable
	} else {
		root.Data[coff] = storage.PageTypeInteriorIndex
	}
	binary.BigEndian.PutUint16(root.Data[coff+3:coff+5], 0)                   // nCell = 0
	binary.BigEndian.PutUint16(root.Data[coff+5:coff+7], uint16(t.pageSize)) // content start
	binary.BigEndian.PutUint32(root.Data[coff+8:coff+12], rightmost)          // rightmost ptr
	return t.pager.WritePage(root)
}
