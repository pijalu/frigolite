// Index-key seek: value-ordered lookups over index b-trees (P9.PERF.T3).
//
// The engine's index trees are stored in raw payload byte order (see
// btree_keyinfo.go), so a binary value seek is unsound: value-equal entries
// are NOT byte-contiguous. The seek here therefore walks the tree's leaves
// in stored order and identifies matches with the KeyInfo record comparator
// — correct on ANY stored order, and per-entry cheap: no DecodeCell
// allocation, no overflow-chain read (only when a comparison is undecided
// beyond the local payload fragment), no full record decode on
// non-matching entries. The walk shape is also the seam a future
// value-ordered storage tranche replaces with a binary descent: SeekIndexKey
// keeps the sqlite3BtreeIndexMoveto contract (cursor positioned at the first
// equal entry), so its body — not its callers — is what changes.

package btree

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// indexSeekMaxDepth caps interior descent depth (corrupt child cycles).
const indexSeekMaxDepth = 64

// IndexKeyRowIDs returns the trailing rowids of every index entry whose
// probed key fields compare equal to probe (IndexRecordCompare == 0 over
// len(probe.Values) fields). It walks every leaf in stored order — on
// today's byte-ordered trees matches are not contiguous, so the walk is
// exhaustive by design; errors (unreadable pages, corrupt records) are
// returned so callers can fall back to a full scan rather than silently
// miss candidates.
func (t *BTree) IndexKeyRowIDs(probe *UnpackedIndexKey) ([]int64, error) {
	if err := validIndexProbe(probe); err != nil {
		return nil, err
	}
	var out []int64
	_, err := t.walkIndexLeaves(t.rootPage, 0, nil, func(data []byte, pageNum uint32, coff, cellIdx int, _ []cursorPathEntry) (bool, error) {
		cellOff := int(storage.CellPointer(data, coff, cellIdx, int(t.pageSize)))
		cmp, payload, err := t.indexCellCompare(data, cellOff, probe)
		if err != nil || cmp != 0 {
			return false, err
		}
		rid, err := indexRecordRowID(payload)
		if err != nil {
			return false, err
		}
		out = append(out, rid)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SeekIndexKey positions the cursor at the FIRST index entry whose probed
// key fields compare equal to probe (the sqlite3BtreeIndexMoveto contract),
// returning true; with no match it returns false and leaves the cursor at
// end-of-tree. On today's byte-ordered trees the position is reached by a
// stored-order walk; once index trees become value-ordered the same
// signature binary-descends. Callers continue from the position with
// Next(), re-comparing entries (matches may be interleaved with non-matches
// until the storage order is value order).
func (c *Cursor) SeekIndexKey(probe *UnpackedIndexKey) (bool, error) {
	if err := validIndexProbe(probe); err != nil {
		return false, err
	}
	found := false
	_, err := c.tx.walkIndexLeaves(c.tx.rootPage, 0, nil, func(data []byte, pageNum uint32, coff, cellIdx int, path []cursorPathEntry) (bool, error) {
		cellOff := int(storage.CellPointer(data, coff, cellIdx, int(c.tx.pageSize)))
		cmp, _, err := c.tx.indexCellCompare(data, cellOff, probe)
		if err != nil || cmp != 0 {
			return false, err
		}
		c.pageNum = pageNum
		c.cellIdx = cellIdx
		c.endOfBTree = false
		c.clearPageCache()
		c.path = append(c.path[:0], path...)
		found = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	if !found {
		c.endOfBTree = true
	}
	return found, nil
}

// validIndexProbe rejects probes the seek cannot honor.
func validIndexProbe(probe *UnpackedIndexKey) error {
	if probe == nil || probe.KeyInfo == nil || len(probe.Values) == 0 {
		return fmt.Errorf("btree: index seek requires a KeyInfo-carrying probe with at least one value")
	}
	return nil
}

// walkIndexLeaves visits every cell of every index leaf reachable from
// pageNum, depth-first in stored order. path carries the cursorPathEntry
// chain from the root to the current page (root first, immediate parent
// last; childIdx 0..CellCount-1 = a cell's left child, CellCount = the
// rightmost pointer — navigateToNextChild's convention, so a cursor can
// resume from a recorded position). fn returns stop=true to end the walk.
// Unreadable or unexpected pages are errors (a seek must not silently skip
// subtrees and miss candidates).
func (t *BTree) walkIndexLeaves(pageNum uint32, depth int, path []cursorPathEntry, fn func(data []byte, leafNum uint32, coff, cellIdx int, path []cursorPathEntry) (bool, error)) (bool, error) {
	if depth > indexSeekMaxDepth {
		return false, fmt.Errorf("btree: interior page chain too deep")
	}
	pg, err := t.pager.ReadPage(pageNum)
	if err != nil {
		return false, err
	}
	coff := contentOffset(pg.PageNum)
	page, err := storage.ParsePage(pg.Data, int(t.pageSize), coff)
	if err != nil {
		return false, err
	}
	switch page.PageType {
	case storage.PageTypeLeafIndex:
		return walkLeafIndexCells(pg, page, coff, pageNum, path, fn)
	case storage.PageTypeInteriorIndex:
		return t.walkIndexInterior(pg, page, coff, depth, path, fn)
	default:
		return false, fmt.Errorf("btree: unexpected page type 0x%02x for index seek", page.PageType)
	}
}

// walkLeafIndexCells invokes fn for each cell of one index leaf page.
func walkLeafIndexCells(pg *pager.Page, page *storage.BTreePage, coff int, leafNum uint32, path []cursorPathEntry, fn func(data []byte, leafNum uint32, coff, cellIdx int, path []cursorPathEntry) (bool, error)) (bool, error) {
	for i := 0; i < int(page.CellCount); i++ {
		stop, err := fn(pg.Data, leafNum, coff, i, path)
		if err != nil || stop {
			return stop, err
		}
	}
	return false, nil
}

// walkIndexInterior descends an index interior page's children in order
// (cell left children 0..CellCount-1, then the rightmost pointer), pushing
// this page onto the cursor path for each descent.
func (t *BTree) walkIndexInterior(pg *pager.Page, page *storage.BTreePage, coff, depth int, path []cursorPathEntry, fn func(data []byte, leafNum uint32, coff, cellIdx int, path []cursorPathEntry) (bool, error)) (bool, error) {
	for i := 0; i < int(page.CellCount); i++ {
		cellOff := int(storage.CellPointer(pg.Data, coff+cellPtrOffset(page.PageType)-8, i, int(t.pageSize)))
		if cellOff < 0 || cellOff+4 > len(pg.Data) {
			return false, fmt.Errorf("database disk image is malformed")
		}
		child := binary.BigEndian.Uint32(pg.Data[cellOff : cellOff+4])
		stop, err := t.walkIndexLeaves(child, depth+1, append(path, cursorPathEntry{pageNum: pg.PageNum, childIdx: i}), fn)
		if err != nil || stop {
			return stop, err
		}
	}
	if page.RightmostPtr == 0 {
		return false, nil
	}
	rightmost := append(path, cursorPathEntry{pageNum: pg.PageNum, childIdx: int(page.CellCount)})
	return t.walkIndexLeaves(page.RightmostPtr, depth+1, rightmost, fn)
}

// indexCellCompare compares the index cell at cellOff against the probe and
// returns (result, the payload the decision was made on). The comparison
// runs against the cell's LOCAL payload fragment; ErrIndexRecordTruncated
// (comparison undecided beyond the fragment) re-runs against the reassembled
// full payload — the only case that reads overflow pages.
func (t *BTree) indexCellCompare(data []byte, cellOff int, probe *UnpackedIndexKey) (int, []byte, error) {
	local, fullLen, ovfl, err := t.indexCellLocalPayload(data, cellOff)
	if err != nil {
		return 0, nil, err
	}
	cmp, err := IndexRecordCompare(local, probe)
	if err == nil {
		return cmp, local, nil
	}
	if !errors.Is(err, ErrIndexRecordTruncated) {
		return 0, nil, err
	}
	full, err := t.fullIndexCellPayload(local, fullLen, ovfl)
	if err != nil {
		return 0, nil, err
	}
	cmp, err = IndexRecordCompare(full, probe)
	if errors.Is(err, ErrIndexRecordTruncated) {
		// Already-complete payload still declaring a larger header: corrupt.
		return 0, nil, ErrIndexRecordCorrupt
	}
	if err != nil {
		return 0, nil, err
	}
	return cmp, full, nil
}

// indexCellLocalPayload parses an index-leaf cell header at cellOff without
// allocating: the payload-length varint, the LOCAL payload slice (a view
// into the page buffer, storage.LocalPayloadSize-clamped like
// decodeIndexLeafCell) and, when the body spills, the first overflow page.
func (t *BTree) indexCellLocalPayload(data []byte, cellOff int) (local []byte, fullLen int, ovfl uint32, err error) {
	if cellOff < 0 || cellOff >= len(data) {
		return nil, 0, 0, fmt.Errorf("database disk image is malformed")
	}
	plen, n := util.GetVarint(data[cellOff:])
	if n == 0 || int64(plen) < 0 {
		return nil, 0, 0, ErrIndexRecordCorrupt
	}
	pos := cellOff + n
	fullLen = int(plen)
	localLen := storage.LocalPayloadSize(fullLen, int(t.usableSize), storage.CellIndexLeaf)
	if pos+localLen > len(data) {
		localLen = len(data) - pos
	}
	if localLen < 0 {
		return nil, 0, 0, fmt.Errorf("database disk image is malformed")
	}
	local = data[pos : pos+localLen]
	if localLen < fullLen {
		if pos+localLen+4 > len(data) {
			return nil, 0, 0, fmt.Errorf("database disk image is malformed")
		}
		ovfl = binary.BigEndian.Uint32(data[pos+localLen : pos+localLen+4])
	}
	return local, fullLen, ovfl, nil
}

// fullIndexCellPayload reassembles a spilling cell's payload through its
// overflow chain (btree_overflow readOverflow). A cell that declares more
// payload than it holds locally with no overflow pointer is malformed.
func (t *BTree) fullIndexCellPayload(local []byte, fullLen int, ovfl uint32) ([]byte, error) {
	if ovfl == 0 {
		if fullLen > len(local) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		return local, nil
	}
	cell := &storage.Cell{
		Type:       storage.CellIndexLeaf,
		Payload:    local,
		PayloadLen: fullLen,
		LocalLen:   len(local),
		Overflow:   ovfl,
	}
	full, err := t.readOverflow(cell)
	if err != nil {
		return nil, err
	}
	return full.Payload, nil
}

// indexRecordRowID decodes an index record's trailing rowid element (the
// last record element of every index entry).
func indexRecordRowID(payload []byte) (int64, error) {
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil || len(rec.Values) == 0 {
		return 0, ErrIndexRecordCorrupt
	}
	id, ok := util.UnwrapColumnValue(rec.Values[len(rec.Values)-1]).(int64)
	if !ok {
		return 0, ErrIndexRecordCorrupt
	}
	return id, nil
}
