package storage

// Oversize cell validation: btree.c btreeCellSizeCheck (src/btree.c:2173).
//
// btreeInitPage runs this validation when the SQLITE_CellSizeCk flag is set
// (PRAGMA cell_size_check; the SQLITE_ENABLE_OVERSIZE_CELL_CHECK build that
// the SQLite test suite targets enables it by default, main.c:3461). Every
// entry of the cell pointer array must address a cell that starts at or
// after the end of the pointer array (iCellFirst) and whose bytes fit the
// page (pc + size <= usableSize). A page whose pointer array was rewritten
// to point at record bodies only passes while the targeted bytes still parse
// as a small in-bounds cell — once a later UPDATE overwrites those bytes the
// next page initialization detects the corruption (test/corrupt.test
// corrupt-7.x).

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pijalu/frigolite/internal/util"
)

// ErrMalformedImage is SQLite's SQLITE_CORRUPT message ("database disk image
// is malformed", btree.c SQLITE_CORRUPT_BKPT).
var ErrMalformedImage = errors.New("database disk image is malformed")

// ValidateCellSizeCheck validates the cell pointer array of the b-tree page
// at pageData[contentOffset:]; it returns ErrMalformedImage when any cell
// pointer or cell size violates the page bounds, mirroring the btreeInitPage
// cell-size-check path. contentOffset is 100 for page 1, 0 otherwise.
func ValidateCellSizeCheck(pageData []byte, pageSize int, contentOffset int) error {
	if len(pageData) < contentOffset+8 {
		// Partial/synthetic page buffers (unit-test headers) are out of
		// scope, matching validatePageHeader.
		return nil
	}
	pageType := pageData[contentOffset]
	leaf := pageType == PageTypeLeafIndex || pageType == PageTypeLeafTable
	cellOffset := contentOffset + 8
	if !leaf {
		cellOffset += 4
	}
	nCell := int(binary.BigEndian.Uint16(pageData[contentOffset+3 : contentOffset+5]))
	iCellFirst := cellOffset + 2*nCell
	iCellLast := pageSize - 4
	if !leaf {
		iCellLast--
	}
	for i := 0; i < nCell; i++ {
		pos := cellOffset + 2*i
		if pos+2 > len(pageData) {
			return ErrMalformedImage
		}
		pc := int(binary.BigEndian.Uint16(pageData[pos : pos+2]))
		if pc < iCellFirst || pc > iCellLast {
			return ErrMalformedImage
		}
		sz, err := cellSizeCheckAt(pageData, pc, pageSize, pageType)
		if err != nil || pc+sz > pageSize {
			return ErrMalformedImage
		}
	}
	return nil
}

// cellSizeCheckAt computes the on-page byte size of the cell at offset pc
// (btree.c xCellSize / btreeParseCellPtr). The error return reports a cell
// whose varints or overflow pointer run off the page buffer.
func cellSizeCheckAt(pageData []byte, pc int, pageSize int, pageType byte) (int, error) {
	if pc < 0 || pc >= len(pageData) {
		return 0, fmt.Errorf("storage: cell offset %d outside page", pc)
	}
	switch pageType {
	case PageTypeLeafTable:
		return tableLeafCellSize(pageData[pc:], pageSize)
	case PageTypeInteriorTable:
		return tableInteriorCellSize(pageData[pc:])
	case PageTypeLeafIndex:
		return indexLeafCellSize(pageData[pc:], pageSize)
	case PageTypeInteriorIndex:
		return indexInteriorCellSize(pageData[pc:], pageSize)
	default:
		return 0, fmt.Errorf("storage: unknown page type 0x%02x", pageType)
	}
}

// tableLeafCellSize sizes a table-leaf cell: payload-size varint + rowid
// varint + local payload (+ 4-byte overflow pointer).
func tableLeafCellSize(cell []byte, pageSize int) (int, error) {
	plen, n1 := util.GetVarint(cell)
	if n1 < 1 || n1 >= len(cell) {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	_, n2 := util.GetVarint(cell[n1:])
	if n2 < 1 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	local := LocalPayloadSize(int(plen), pageSize, CellTableLeaf)
	sz := n1 + n2 + local
	if local < int(plen) {
		sz += 4
	}
	return sz, nil
}

// tableInteriorCellSize sizes a table-interior cell: 4-byte left-child
// pointer + rowid varint.
func tableInteriorCellSize(cell []byte) (int, error) {
	if len(cell) <= 4 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	_, n := util.GetVarint(cell[4:])
	if n < 1 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	return 4 + n, nil
}

// indexLeafCellSize sizes an index-leaf cell: payload-size varint + local
// payload (+ 4-byte overflow pointer).
func indexLeafCellSize(cell []byte, pageSize int) (int, error) {
	plen, n := util.GetVarint(cell)
	if n < 1 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	local := LocalPayloadSize(int(plen), pageSize, CellIndexLeaf)
	sz := n + local
	if local < int(plen) {
		sz += 4
	}
	return sz, nil
}

// indexInteriorCellSize sizes an index-interior cell: 4-byte left-child
// pointer + payload-size varint + local payload (+ 4-byte overflow pointer).
func indexInteriorCellSize(cell []byte, pageSize int) (int, error) {
	if len(cell) <= 4 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	plen, n := util.GetVarint(cell[4:])
	if n < 1 {
		return 0, fmt.Errorf("storage: truncated cell")
	}
	local := LocalPayloadSize(int(plen), pageSize, CellIndexInterior)
	sz := 4 + n + local
	if local < int(plen) {
		sz += 4
	}
	return sz, nil
}
