// Package btree: pointer-map maintenance helpers for the auto-vacuum
// page-swap machinery (split from btree_vacuum.go to stay within the
// repository's file-size budget).
//
// These helpers rewrite pointer-map entries after a page move and walk
// b-tree/overflow chains to locate the parent of a relocated page.
package btree

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// setChildPtrmaps rewrites the pointer-map entries for every child
// and every overflow page of `pgNo`, setting parent=pgNo. Called by
// RelocatePage after a page has been moved to a new slot so that
// future vacuum steps can find the moved page as their parent.
//
// For interior pages: the first 4 bytes of each cell is the
// left-child page number; the rightmost-child is the 4-byte value
// at pc+8. For leaf pages: each cell's overflow page (if any) is a
// chain — write ptrmap for the first overflow page in the chain.
// Interior pages don't have overflow chains (the divider key is
// inlined).
//
// Reference: src/btree.c::setChildPtrmaps (~line 6490).
func (t *BTree) setChildPtrmaps(pg *pager.Page, pgNo uint32) error {
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return err
	}
	if page.PageType == storage.PageTypeInteriorTable || page.PageType == storage.PageTypeInteriorIndex {
		return t.setChildPtrmapsInterior(pg, pgNo, coff, page)
	}
	// Leaf page: walk each cell's overflow chain.
	cellType, ok := leafChainCellType(page.PageType)
	if !ok {
		return nil
	}
	return t.setChildPtrmapsLeaf(pg, pgNo, coff, page, cellType)
}

// setChildPtrmapsInterior re-points the ptrmap entry of every interior cell's
// left child (and the rightmost child) at pgNo.
func (t *BTree) setChildPtrmapsInterior(pg *pager.Page, pgNo uint32, coff int, page *storage.BTreePage) error {
	ptrBase := coff + cellPtrOffset(page.PageType) - 8
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.pageSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		if child == 0 {
			continue
		}
		if err := t.pager.WritePtrmap(child, storage.PtrmapBtree, pgNo); err != nil {
			return err
		}
	}
	rmp := binary.BigEndian.Uint32(pg.Data[coff+8 : coff+12])
	if rmp == 0 {
		return nil
	}
	return t.pager.WritePtrmap(rmp, storage.PtrmapBtree, pgNo)
}

// leafChainCellType maps a leaf page type to its cell encoding; ok=false for
// non-leaf pages (nothing to walk).
func leafChainCellType(pageType byte) (storage.CellType, bool) {
	switch pageType {
	case storage.PageTypeLeafTable:
		return storage.CellTableLeaf, true
	case storage.PageTypeLeafIndex:
		return storage.CellIndexLeaf, true
	}
	return 0, false
}

// setChildPtrmapsLeaf re-points the ptrmap entry of each cell's overflow
// chain head at pgNo.
func (t *BTree) setChildPtrmapsLeaf(pg *pager.Page, pgNo uint32, coff int, page *storage.BTreePage, cellType storage.CellType) error {
	for i := 0; i < int(page.CellCount); i++ {
		ptrBase := coff + cellPtrOffset(page.PageType) - 8
		cellOff := int(storage.CellPointer(pg.Data, ptrBase, i, int(t.usableSize)))
		if cellOff+4 > len(pg.Data) {
			continue
		}
		c, err := storage.DecodeCell(pg.Data, cellOff, cellType, int(t.usableSize))
		if err != nil || c.Overflow == 0 {
			continue
		}
		if err := t.pager.WritePtrmap(c.Overflow, storage.PtrmapOverflow1, pgNo); err != nil {
			return err
		}
	}
	return nil
}

// schemaCursor opens a cursor on the schema btree (rooted at page 1).
// Used by collectSchemaRoots to walk the schema even when the BTree
// handle's rootPage is a user-table root.
func (t *BTree) schemaCursor() (*Cursor, error) {
	savedRoot := t.rootPage
	t.rootPage = 1
	defer func() { t.rootPage = savedRoot }()
	return t.OpenCursor()
}

// sqlite_schema. The schema btree's cells are (type, name, tblname,
// rootpage, sql) records. We open a cursor on the schema btree
// (rooted at page 1) and walk every cell, decoding just enough of
// the record header to extract the rootpage int64.
func (t *BTree) collectSchemaRoots() ([]uint32, error) {
	// Open the schema btree directly (it's always at page 1), regardless
	// of what this BTree handle's rootPage is. The previous guard
	// `if t.rootPage != 1 { return nil }` made the function a no-op for
	// user-table BTree handles, which broke findParentByWalk in
	// maybeRebalanceAfterDelete (autovacuum-9.5: no roots enumerated
	// → user-table leaves reported as orphans → balanceNonroot never
	// called → FreePage never called → autovacuum never shrinks the
	// file).
	cur, err := t.schemaCursor()
	if err != nil {
		return nil, err
	}
	var roots []uint32
	for {
		payload, _, err := cur.ReadCellData()
		if err != nil {
			break
		}
		root, ok := decodeSchemaRootpage(payload)
		if ok && root > 1 {
			roots = append(roots, root)
		}
		ok2, err := cur.Next()
		if err != nil || !ok2 {
			break
		}
	}
	return roots, nil
}

// decodeSchemaRootpage extracts the 4th field of a sqlite_schema
// record (the rootpage int64). The header is: 1+ varint headerSize
// followed by hdrSize-1 varint serial types; the data follows at
// byte hdrSize.
func decodeSchemaRootpage(payload []byte) (uint32, bool) {
	if len(payload) < 2 {
		return 0, false
	}
	hdrSize, n := binary.Uvarint(payload)
	if n <= 0 || hdrSize == 0 || int(hdrSize) > len(payload) {
		return 0, false
	}
	headerEnd := int(hdrSize)
	dataPos, rootLen, ok := schemaDataPos(payload, n, headerEnd)
	if !ok {
		return 0, false
	}
	if rootLen == 0 || dataPos+rootLen > len(payload) {
		return 0, false
	}
	root, ok := decodeSchemaInt(payload, dataPos, rootLen)
	if !ok {
		return 0, false
	}
	if root < 0 || root > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(root), true
}

// schemaDataPos walks the four serial-type varints of a sqlite_schema record
// header (fields: type, name, tbl_name, rootpage — each start offset is the
// previous field's varint width plus the field index) and returns the
// data-section start offset together with the rootpage field's length.
func schemaDataPos(payload []byte, n, headerEnd int) (dataPos, rootLen int, ok bool) {
	dataPos = headerEnd
	for i := 0; i < 4; i++ {
		size, w, fok := schemaFieldSize(payload, n+i, headerEnd)
		if !fok {
			return 0, 0, false
		}
		n = w
		if i < 3 {
			dataPos += size
		} else {
			rootLen = size
		}
	}
	return dataPos, rootLen, true
}

// schemaFieldSize reads one serial-type varint in a sqlite_schema record
// header (the varint at payload[start:headerEnd]) and returns the field's
// encoded data length together with the varint's width.
func schemaFieldSize(payload []byte, start, headerEnd int) (size, width int, ok bool) {
	code, w := binary.Uvarint(payload[start:headerEnd])
	if w <= 0 {
		return 0, 0, false
	}
	b, err := storage.SerialTypeLength(code)
	if err != nil {
		return 0, 0, false
	}
	return int(b), w, true
}

// decodeSchemaInt decodes a big-endian signed integer of serial length
// 1/2/3/4/6/8 at dataPos; ok=false for any other length.
func decodeSchemaInt(payload []byte, dataPos, rootLen int) (int64, bool) {
	var root int64
	switch rootLen {
	case 1:
		root = int64(int8(payload[dataPos]))
	case 2:
		root = int64(int16(binary.BigEndian.Uint16(payload[dataPos:])))
	case 3:
		v := uint32(payload[dataPos])<<16 | uint32(payload[dataPos+1])<<8 | uint32(payload[dataPos+2])
		if v&0x800000 != 0 {
			v |= 0xFF000000
		}
		root = int64(int32(v))
	case 4:
		root = int64(int32(binary.BigEndian.Uint32(payload[dataPos:])))
	case 6:
		v := uint64(payload[dataPos])<<40 | uint64(payload[dataPos+1])<<32 | uint64(payload[dataPos+2])<<24 |
			uint64(payload[dataPos+3])<<16 | uint64(payload[dataPos+4])<<8 | uint64(payload[dataPos+5])
		if v&0x800000000000 != 0 {
			v |= 0xFF00000000000000
		}
		root = int64(v)
	case 8:
		root = int64(binary.BigEndian.Uint64(payload[dataPos:]))
	default:
		return 0, false
	}
	return root, true
}
