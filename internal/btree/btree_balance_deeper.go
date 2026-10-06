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
	// The balance do-loop's next step runs getAndInitPage on the fresh child
	// (src/btree.c:9104); btreeInitPage with the cell-size check enabled
	// validates every cell pointer of the copied content, so a root whose
	// pointer array was rewritten to point at record bodies is rejected
	// here — the INSERT forcing balance_deeper is where corrupt.test 7.3
	// expects "database disk image is malformed".
	if err := storage.ValidateCellSizeCheck(child.Data, int(t.usableSize), 0); err != nil {
		return nil, err
	}
	if err := t.rewriteRootLeafAsInterior(pg, child.PageNum, coff); err != nil {
		return nil, err
	}
	// The balance do-loop's next iteration balances the child: insert the
	// pending cell there with the normal machinery. The child inherits the
	// root's full cell set, so it is usually overfull itself and splits
	// (btree.c balance_nonroot on the copied child); routing through
	// insertPage keeps the shape general (any root leaf, any page size) and
	// applyChildSplits' right-most-pointer branch wires a split's divider
	// chain to the fresh interior root.
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

// copyLeafRootToChild moves the root leaf's content to the freshly allocated
// child page (copyNodeContent, src/btree.c:8124): two verbatim copies — the
// cell content area at its page-absolute offsets, and the b-tree header plus
// cell pointer array rebuilt at the child's header offset (0 vs page 1's
// 100; cell data offsets are page-absolute so the copied pointer values stay
// valid). Cells are NOT decoded: the copied bytes must reach the child's
// page initialization exactly as they lived on the root, because that
// btreeInitPage — with the cell-size check enabled (ValidateCellSizeCheck in
// the caller) — is what rejects a root whose pointer array was crafted to
// point at record bodies (corrupt.test 7.3). Decoding instead of copying
// would either fail at a different site or re-encode the cells and destroy
// the evidence. Moved cells keep their overflow chains: each chain's first
// page is re-parented to the child (ptrmapPutOvflPtr, src/btree.c:8025).
func (t *BTree) copyLeafRootToChild(child, root *pager.Page, page *storage.BTreePage, coff int) error {
	// Write-intent barrier: the memcpys below overwrite the child's bytes —
	// capture its statement-journal before-image first (the root's own
	// rewrite captures in rewriteRootLeafAsInterior).
	t.pager.PrepareWrite(child)
	usable := int(t.usableSize)
	// memcpy(&aTo[iData], &aFrom[iData], pBt->usableSize-iData): the cell
	// content area, verbatim.
	iData := int(page.CellContent)
	if iData > usable {
		iData = usable
	}
	copy(child.Data[iData:usable], root.Data[iData:usable])
	// memcpy(&aTo[iToHdr], &aFrom[iFromHdr], cellOffset+2*nCell): the leaf
	// b-tree header (8 bytes: type, first freeblock, nCell, content start,
	// fragmented bytes — freeblock chain offsets are page-absolute too) and
	// the cell pointer array.
	hdrLen := 8
	n := hdrLen + 2*int(page.CellCount)
	copy(child.Data[0:n], root.Data[coff:coff+n])
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
	// Write-intent barrier: capture the root's statement-journal before-image
	// before the zeroing wipe.
	t.pager.PrepareWrite(root)
	for i := coff; i < int(t.pageSize); i++ {
		root.Data[i] = 0
	}
	if t.isTable {
		root.Data[coff] = storage.PageTypeInteriorTable
	} else {
		root.Data[coff] = storage.PageTypeInteriorIndex
	}
	binary.BigEndian.PutUint16(root.Data[coff+3:coff+5], 0)                    // nCell = 0
	binary.BigEndian.PutUint16(root.Data[coff+5:coff+7], uint16(t.usableSize)) // content start (zeroPage)
	binary.BigEndian.PutUint32(root.Data[coff+8:coff+12], rightmost)           // rightmost ptr
	return t.pager.WritePage(root)
}
