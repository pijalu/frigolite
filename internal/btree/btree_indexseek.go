// Index-key seek: value-ordered lookups over index b-trees (P9.PERF.T3).
//
// The engine's index trees are ordered by VALUE under the index's KeyInfo
// comparator (btree_keyinfo.go: SetIndexKeyInfo installs RecordPayloadCompare,
// and the insert position, interior routing, splits and the DDL rebuild all
// order through it), so a lookup descends the tree: btree_index_lower_bound.go
// carries the shared lower-bound descent, and the walk below remains only for
// trees that carry no installed comparator — the one case where the stored
// order cannot be assumed.

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
// len(probe.Values) fields). On a value-ordered tree the matching entries are
// contiguous, so one lower-bound descent positions the scan and it walks only
// the equal-prefix run; a tree with no installed comparator keeps the
// exhaustive stored-order walk (whose matches the descent could miss).
func (t *BTree) IndexKeyRowIDs(probe *UnpackedIndexKey) ([]int64, error) {
	if err := validIndexProbe(probe); err != nil {
		return nil, err
	}
	if t.keyCompare == nil {
		return t.indexKeyRowIDsByWalk(probe)
	}
	return t.indexKeyRowIDsBySeek(probe)
}

// indexKeyRowIDsBySeek resolves probe with one lower-bound descent followed by
// a forward run over the entries that share the probed prefix (the
// sqlite3BtreeIndexMoveto contract: positioned at the first equal entry, the
// caller continues while the comparison still says equal). The run crosses
// leaf boundaries through the cursor's path stack — equal entries are
// contiguous but not necessarily on one page.
func (t *BTree) indexKeyRowIDsBySeek(probe *UnpackedIndexKey) ([]int64, error) {
	c, err := t.OpenCursorAtRoot()
	if err != nil {
		return nil, err
	}
	found, err := c.indexLowerBoundScan(t.indexProbeCompare(probe))
	if err != nil || !found {
		return nil, err
	}
	return t.collectIndexEqualRun(c, probe)
}

// collectIndexEqualRun reads rowids from the cursor position while the probe
// comparison still reports equality, stepping through the run with Next().
func (t *BTree) collectIndexEqualRun(c *Cursor, probe *UnpackedIndexKey) ([]int64, error) {
	var out []int64
	for {
		rid, equal, err := t.indexRunRowID(c, probe)
		if err != nil || !equal {
			return out, err
		}
		out = append(out, rid)
		ok, err := c.Next()
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
	}
}

// indexRunRowID returns the rowid of the entry at the cursor's position plus
// whether it still matches the probe (the equal run's end condition).
func (t *BTree) indexRunRowID(c *Cursor, probe *UnpackedIndexKey) (int64, bool, error) {
	full, err := t.indexCursorCellPayload(c)
	if err != nil {
		return 0, false, err
	}
	cmp, err := IndexRecordCompare(full, probe)
	if err != nil {
		return 0, false, err
	}
	if cmp != 0 {
		return 0, false, nil // past the equal-prefix run
	}
	rid, err := indexRecordRowID(full)
	if err != nil {
		return 0, false, err
	}
	return rid, true, nil
}

// indexKeyRowIDsByWalk is the exhaustive stored-order leaf walk: it visits
// every leaf once and identifies matches with the KeyInfo record comparator —
// correct on ANY stored order, and per-entry cheap: no DecodeCell allocation,
// no overflow-chain read (only when a comparison is undecided beyond the local
// payload fragment), no full record decode on non-matching entries. Errors
// (unreadable pages, corrupt records) are returned so callers can fall back to
// a full scan rather than silently miss candidates.
func (t *BTree) indexKeyRowIDsByWalk(probe *UnpackedIndexKey) ([]int64, error) {
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
// end-of-tree. On a value-ordered tree (the one every index writer installs,
// SetIndexKeyInfo) the position is reached with the shared lower-bound
// descent; a tree with no installed comparator keeps the stored-order walk.
// Callers continue from the position with Next(), re-comparing entries.
func (c *Cursor) SeekIndexKey(probe *UnpackedIndexKey) (bool, error) {
	if err := c.checkOpen(); err != nil {
		return false, err
	}
	c.clearSavedSeek()
	if err := validIndexProbe(probe); err != nil {
		return false, err
	}
	if c.tx.keyCompare == nil {
		return c.seekIndexKeyByWalk(probe)
	}
	found, err := c.indexLowerBoundScan(c.tx.indexProbeCompare(probe))
	if err != nil {
		return false, err
	}
	if !found {
		c.endOfBTree = true
		return false, nil
	}
	// The lower bound may be an entry that sorts AFTER the probe (no equal
	// entry exists): the seek's verdict is the comparison, not the position.
	full, err := c.tx.indexCursorCellPayload(c)
	if err != nil {
		return false, err
	}
	cmp, err := IndexRecordCompare(full, probe)
	if err != nil {
		return false, err
	}
	if cmp != 0 {
		c.endOfBTree = true
		return false, nil
	}
	return true, nil
}

// indexProbeCompare compares an index cell against an unpacked probe with
// IndexRecordCompare's prefix semantics, reassembling a spilling cell's
// overflow chain first: the descent's routing and the leaf binary search must
// both decide on the same (full) bytes, and a local fragment cannot order a
// record whose comparison runs past it.
func (t *BTree) indexProbeCompare(probe *UnpackedIndexKey) indexCellCompare {
	return func(data []byte, cellOff int, cellType storage.CellType) (int, error) {
		var cell storage.Cell
		if err := storage.DecodeCellInto(data, cellOff, cellType, int(t.usableSize), &cell); err != nil {
			return 0, err
		}
		full, err := t.readOverflow(&cell)
		if err != nil {
			return 0, err
		}
		cmp, err := IndexRecordCompare(full.Payload, probe)
		if errors.Is(err, ErrIndexRecordTruncated) {
			// An already-complete payload that still declares a larger header is
			// corrupt (indexCellCompare's rule).
			return 0, ErrIndexRecordCorrupt
		}
		return cmp, err
	}
}

// seekIndexKeyByWalk is SeekIndexKey's stored-order form, for trees whose
// order the descent cannot assume (no installed comparator).
func (c *Cursor) seekIndexKeyByWalk(probe *UnpackedIndexKey) (bool, error) {
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
	page, err := pg.ParsedBTree(int(t.pageSize), coff)
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
// last record element of every index entry). Only the header and the final
// value are read: no boxes for the leading key columns a full DecodeRecord
// would build, and no heap serial-type slice (a stack buffer serves the
// common ≤16-column index keys; the buffer never escapes this call — append
// spills to the heap beyond it, parse identical). The corrupt-record contract
// is DecodeRecord's, narrowed to what the rowid read can observe: malformed
// header, unknown serial type, value data running past the record, an empty
// element list, or a trailing element that does not decode to an integer
// (a float, like DecodeRecord's int64 unwrap, is corrupt here too).
func indexRecordRowID(payload []byte) (int64, error) {
	var stackTypes [16]uint64
	serialTypes, pos, err := storage.ParseRecordHeaderInto(payload, stackTypes[:0])
	if err != nil || len(serialTypes) == 0 {
		return 0, ErrIndexRecordCorrupt
	}
	last := len(serialTypes) - 1
	for i := 0; i < last; i++ {
		valLen, err := storage.SerialTypeLength(serialTypes[i])
		if err != nil {
			return 0, ErrIndexRecordCorrupt
		}
		pos += int(valLen)
		if pos > len(payload) {
			return 0, ErrIndexRecordCorrupt
		}
	}
	valLen, err := storage.SerialTypeLength(serialTypes[last])
	if err != nil || pos+int(valLen) > len(payload) {
		return 0, ErrIndexRecordCorrupt
	}
	data := payload[pos : pos+int(valLen)]
	switch serialTypes[last] {
	case storage.SerialZero:
		return 0, nil
	case storage.SerialOne:
		return 1, nil
	}
	id, ok := storage.DecodeSerialInt64(serialTypes[last], data)
	if !ok {
		return 0, ErrIndexRecordCorrupt
	}
	return id, nil
}
