// Root-split machinery: keeping the splitting root's page number stable,
// rewriting the root as an interior node over the relocated halves, and the
// page-1 special case (the schema b-tree's permanent root).

package btree

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// relocateRootSplit keeps the splitting root's page number stable. The split
// has already rewritten the root as its left segment and allocated the right
// siblings S1..Sk; one more page is allocated as the final child slot so the
// segment contents rotate down one slot (S1←left, Si←R(i-1), new←Rk), and the
// root page is rewritten as an interior node over the relocated children.
func (t *BTree) relocateRootSplit(splits []leafSplitResult) error {
	rootPg, err := t.pager.ReadPage(t.rootPage)
	if err != nil {
		return err
	}
	left := make([]byte, len(rootPg.Data))
	copy(left, rootPg.Data)

	// Final child slot: allocated last so it carries the highest page
	// number, matching btree.c's up-front child allocation order. It is a
	// child of the root (balance_deeper ptrmapPut PTRMAP_BTREE pRoot->pgno,
	// src/btree.c:9028); the split siblings were parented to the root by
	// splitLeafMulti.
	tail, err2 := t.allocBtreeNode(t.rootPage)
	if err2 != nil {
		return err2
	}

	if err := t.rotateRootSplitSegments(left, splits, tail); err != nil {
		return err
	}

	// Children in key order after the rotation; seps[i] separates child i
	// from child i+1 (the split dividers carry their medianKey/medianPayload
	// unchanged).
	children := make([]uint32, 0, len(splits)+1)
	seps := make([]leafSplitResult, 0, len(splits))
	for _, s := range splits {
		children = append(children, s.pageNum)
		seps = append(seps, s)
	}
	children = append(children, tail.PageNum)
	if err := t.repointRelocatedSplitChildren(children); err != nil {
		return err
	}
	return t.writeInteriorRootAt(t.rootPage, children, seps)
}

// rotateRootSplitSegments moves the segment contents down one child slot:
// each split sibling takes the previous segment's content (starting with the
// old root's) and the freshly allocated tail takes the last sibling's.
func (t *BTree) rotateRootSplitSegments(left []byte, splits []leafSplitResult, tail *pager.Page) error {
	prev := left
	for _, s := range splits {
		dst, err := t.pager.ReadPage(s.pageNum)
		if err != nil {
			return err
		}
		cur := make([]byte, len(dst.Data))
		copy(cur, dst.Data)
		copy(dst.Data, prev)
		if err := t.pager.WritePage(dst); err != nil {
			return err
		}
		prev = cur
	}
	copy(tail.Data, prev)
	return t.pager.WritePage(tail)
}

// repointRelocatedSplitChildren re-points every relocated child's references
// at their new owner in the pointer map. The rotation above moved page
// content (and everything it references) between the child slots: S1 now
// holds the old root's children (when the split node was an interior page),
// and each later slot holds the previous segment's content. Every moved
// page's references must be re-pointed (btree.c balance_deeper's ptrmapPut
// over every moved cell's child, src/btree.c:9028, plus ptrmapPutOvflPtr for
// overflow chains at 8783/8025) — setChildPtrmaps covers both: btree
// children of relocated interior pages and the overflow chains of relocated
// leaf cells. Without the interior re-pointing, autovacuum's
// AllocateRootPage relocation later reads a STALE parent for a relocated
// occupant ("parent N does not reference child M", corruptB-3.1.1).
func (t *BTree) repointRelocatedSplitChildren(children []uint32) error {
	for _, ch := range children {
		if t.ptrmapEnabled() {
			cpg, err := t.pager.ReadPage(ch)
			if err != nil {
				return err
			}
			if err := t.setChildPtrmaps(cpg, ch); err != nil {
				return err
			}
		}
	}
	return nil
}

// freeDisplacedInteriorChains is intentionally ABSENT: writeInteriorRootAt's
// caller (relocateRootSplit) first rotates the displaced content VERBATIM
// into the new child slots (rotateRootSplitSegments), so the old page's
// divider cells — and the overflow chains they own — stay LIVE in their new
// home. Freeing them here would hand live chains to the freelist
// (setChildPtrmaps re-parents them instead, btree.c balance_deeper's
// ptrmapPut over every moved cell).

// writeInteriorRootHeader writes the interior page header fields and the
// FIRST divider cell (children[0]/seps[0]); the remaining separators are
// appended by addInteriorCellToPage. children[len-1] becomes the rightmost
// pointer.
func (t *BTree) writeInteriorRootHeader(pg *pager.Page, coff int, dst uint32, children []uint32, seps []leafSplitResult) error {
	cellCount := uint16(0)
	contentStart := int(t.pageSize)
	if len(seps) > 0 {
		cellData, derr := t.encodeDividerCell(children[0], seps[0], dst)
		if derr != nil {
			return derr
		}
		contentStart = int(t.pageSize) - len(cellData)
		copy(pg.Data[contentStart:], cellData)
		cellCount = 1
	}
	binary.BigEndian.PutUint16(pg.Data[coff+cellPtrOffset(pg.Data[coff]):], uint16(contentStart))
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], cellCount)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(contentStart))
	binary.BigEndian.PutUint32(pg.Data[coff+8:coff+12], children[len(children)-1]) // rightmostPtr
	return nil
}

// writeInteriorRootAt rewrites page dst as a fresh interior node over the
// ordered children: cell j points at children[j] with separator seps[j], and
// children[len-1] is the rightmost pointer.
func (t *BTree) writeInteriorRootAt(dst uint32, children []uint32, seps []leafSplitResult) error {
	pg, err := t.pager.ReadPage(dst)
	if err != nil {
		return err
	}
	coff := contentOffset(dst)
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	if t.isTable {
		pg.Data[coff] = storage.PageTypeInteriorTable
	} else {
		pg.Data[coff] = storage.PageTypeInteriorIndex
	}
	if err := t.writeInteriorRootHeader(pg, coff, dst, children, seps); err != nil {
		return err
	}
	if err := t.pager.WritePage(pg); err != nil {
		return err
	}
	for i := 1; i < len(seps); i++ {
		if err := t.addInteriorCellToPage(dst, children[i], seps[i], children[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// createInteriorRoot creates an interior page pointing to two children.
func (t *BTree) createInteriorRoot(leftChild uint32, divider leafSplitResult, rightChild uint32) (*pager.Page, error) {
	// The schema b-tree (sqlite_schema) is permanently rooted at page 1:
	// page 1 is the database file header page and cannot be demoted to a
	// child. When its root splits, page 1 becomes an interior page and the
	// split halves are moved to newly allocated pages (SQLite semantics).
	if t.rootPage == 1 {
		return t.createInteriorRootAtPage1(divider, rightChild)
	}
	rootPg, err := t.allocRootpage()
	if err != nil {
		return nil, err
	}
	rootCoff := contentOffset(rootPg.PageNum)

	if t.isTable {
		rootPg.Data[rootCoff] = storage.PageTypeInteriorTable
	} else {
		rootPg.Data[rootCoff] = storage.PageTypeInteriorIndex
	}

	// One cell: {leftChild, divider}
	cellData, err := t.encodeDividerCell(leftChild, divider, rootPg.PageNum)
	if err != nil {
		return nil, err
	}
	cellStart := int(t.usableSize) - len(cellData)
	copy(rootPg.Data[cellStart:], cellData)
	// Full header rewrite: the allocated root may be a cached buffer from
	// an earlier incarnation — freeblock and fragmentation must be reset.
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+1:rootCoff+3], 0) // first freeblock
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+cellPtrOffset(rootPg.Data[rootCoff]):], uint16(cellStart))
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+3:rootCoff+5], 1)
	binary.BigEndian.PutUint16(rootPg.Data[rootCoff+5:rootCoff+7], uint16(cellStart))
	rootPg.Data[rootCoff+7] = 0                                                 // fragmented free bytes
	binary.BigEndian.PutUint32(rootPg.Data[rootCoff+8:rootCoff+12], rightChild) // rightmostPtr

	if err := t.pager.WritePage(rootPg); err != nil {
		return nil, err
	}
	return rootPg, nil
}

// createInteriorRootAtPage1 converts page 1 (the schema b-tree root, which
// must remain the root because it is the file header page) into an interior
// page after a split. The split's lower half currently stored in page 1 is
// moved to a newly allocated leaf so page 1 becomes a pure interior page
// pointing to both halves.
func (t *BTree) createInteriorRootAtPage1(divider leafSplitResult, rightChild uint32) (*pager.Page, error) {
	pg1, err := t.pager.ReadPage(1)
	if err != nil {
		return nil, err
	}

	// Move page 1's current content to a fresh page. Page 1's b-tree
	// content lives at offset 100 (after the file header) while a normal
	// page's content starts at offset 0, so the b-tree header and cell
	// pointer array are relocated; the cell data offsets are absolute
	// positions within the page and copy verbatim. The relocated page
	// keeps its ORIGINAL page type: this path runs both for the classic
	// first leaf split (page 1 holds leaf content) AND when a split
	// bubbles up from an interior child (page 1 then holds INTERIOR
	// entries plus its rightmost pointer). Relocating an interior page 1
	// as a leaf orphaned that entire subtree — every row under it vanished
	// from scans and seeks while its cells stayed on the now-unreferenced
	// pages (fts4opt 2.x churn: blockids went "missing" after DELETE FROM
	// + regrowth crossed the second root split).
	oldType := pg1.Data[contentOffset(1)]
	interior := oldType == storage.PageTypeInteriorTable || oldType == storage.PageTypeInteriorIndex
	oldHdrLen := 8
	if interior {
		oldHdrLen = 12 // interior pages carry the rightmost pointer at 8..12
	}
	// The relocated lower half becomes a child of page 1, which stays the
	// schema b-tree's root (balance_deeper semantics on the header page).
	newLeft, err := t.allocBtreeNode(1)
	if err != nil {
		return nil, err
	}
	hdrLen := oldHdrLen

	copy(newLeft.Data, pg1.Data)
	copy(newLeft.Data[0:hdrLen], pg1.Data[100:100+hdrLen])
	n := int(binary.BigEndian.Uint16(pg1.Data[103:105]))
	copy(newLeft.Data[hdrLen:hdrLen+2*n], pg1.Data[100+hdrLen:100+hdrLen+2*n])
	newLeft.Data[0] = oldType
	if err := t.pager.WritePage(newLeft); err != nil {
		return nil, err
	}
	// The relocated content's children and overflow chains (leaf cells AND
	// divider cells) still carry page 1 as their ptrmap parent, but the
	// content now lives in newLeft — re-point every reference
	// (btree.c balance_deeper's ptrmapPut / ptrmapPutOvflPtr over the moved
	// page's cells).
	if t.ptrmapEnabled() {
		if err := t.setChildPtrmaps(newLeft, newLeft.PageNum); err != nil {
			return nil, err
		}
	}

	// Convert page 1 into an interior page: one cell {newLeft, divider}
	// and rightmostChild = rightChild. Keep the 100-byte file header.
	rootCoff := contentOffset(1)
	if t.isTable {
		pg1.Data[rootCoff] = storage.PageTypeInteriorTable
	} else {
		pg1.Data[rootCoff] = storage.PageTypeInteriorIndex
	}
	for i := rootCoff + 1; i < int(t.pageSize); i++ {
		pg1.Data[i] = 0
	}
	cellData, err := t.encodeDividerCell(newLeft.PageNum, divider, 1)
	if err != nil {
		return nil, err
	}
	cellStart := int(t.pageSize) - len(cellData)
	copy(pg1.Data[cellStart:], cellData)
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+cellPtrOffset(pg1.Data[rootCoff]):], uint16(cellStart))
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+3:rootCoff+5], 1)
	binary.BigEndian.PutUint16(pg1.Data[rootCoff+5:rootCoff+7], uint16(cellStart))
	binary.BigEndian.PutUint32(pg1.Data[rootCoff+8:rootCoff+12], rightChild) // rightmostPtr

	if err := t.pager.WritePage(pg1); err != nil {
		return nil, err
	}
	return pg1, nil
}
