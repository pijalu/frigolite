package btree

// In-page free-space accounting, ported from btree.c (src/btree.c
// freeSpace / dropCell / allocateSpace / pageFindSlot / defragmentPage).
// A leaf-cell removal does NOT defragment the page (btree.c dropCell): the
// cell's bytes return to the page's freeblock chain (or the fragmented-byte
// count when smaller than 4 bytes), and the next insert reuses that space
// (allocateSpace: freeblock first-fit, else defragment, else the content
// area). The engine's earlier convention compacted a page on every cell
// delete — O(pageSize) per point delete — which this replaces with the
// same O(1) accounting SQLite performs.
//
// Freeblock wire format (SQLite file format §B-tree Pages): a 2-byte
// next-freeblock offset (0 ends the chain, kept in ascending address
// order), then a 2-byte size in bytes including this 4-byte header. The
// chain head lives in header bytes 1-2; header byte 7 counts fragmented
// bytes (free snippets smaller than 4 bytes). All offsets are relative to
// the page start (page 1: after the 100-byte database header), matching
// contentOffset.

import (
	"encoding/binary"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

// delSpan is one cell's raw byte range on the page (defragment staging).
type delSpan struct {
	off  int
	size int
}

func get2(data []byte, off int) int {
	return int(binary.BigEndian.Uint16(data[off : off+2]))
}

func put2(data []byte, off, v int) {
	binary.BigEndian.PutUint16(data[off:off+2], uint16(v))
}

// freeSpacePos locates a freed range's insertion point in a page's
// freeblock chain: iPtr is the address of the pointer to the successor,
// iFreeBlk the successor itself (0 when the range lands at the chain end).
type freeSpacePos struct {
	iPtr     int
	iFreeBlk int
}

// freeSpaceLocate walks the ascending-address chain for the first freeblock
// at or after iStart (btree.c freeSpace's insertion-point scan). An out-of-
// order or oversized link is a corrupt image.
func freeSpaceLocate(data []byte, hdr, usableSize, iStart int) (freeSpacePos, error) {
	iPtr := hdr + 1
	if data[iPtr] == 0 && data[iPtr+1] == 0 {
		return freeSpacePos{iPtr: iPtr}, nil // empty chain
	}
	for {
		iFreeBlk := get2(data, iPtr)
		if iFreeBlk >= iStart {
			if iFreeBlk > usableSize-4 {
				return freeSpacePos{}, storage.ErrMalformedImage
			}
			return freeSpacePos{iPtr: iPtr, iFreeBlk: iFreeBlk}, nil
		}
		if iFreeBlk <= iPtr {
			if iFreeBlk == 0 {
				return freeSpacePos{iPtr: iPtr}, nil
			}
			return freeSpacePos{}, storage.ErrMalformedImage
		}
		iPtr = iFreeBlk
	}
}

// freeSpaceCoalesce folds the freed range [iStart, iEnd) into its chain
// neighbors: the successor freeblock when it directly follows, and the
// preceding freeblock when it directly ends at iStart (btree.c freeSpace's
// two coalescing steps). The absorbed seams return as fragmented bytes.
func freeSpaceCoalesce(data []byte, hdr, usableSize int, pos freeSpacePos, iStart, iSize, iEnd int) (start, size, end, next, nFrag int, err error) {
	next = pos.iFreeBlk
	if next != 0 && iEnd+3 >= next {
		nFrag = next - iEnd
		if iEnd > next {
			return 0, 0, 0, 0, 0, storage.ErrMalformedImage
		}
		iEnd = next + get2(data, next+2)
		if iEnd > usableSize {
			return 0, 0, 0, 0, 0, storage.ErrMalformedImage
		}
		iSize = iEnd - iStart
		next = get2(data, next)
	}
	if pos.iPtr > hdr+1 {
		iPtrEnd := pos.iPtr + get2(data, pos.iPtr+2)
		if iPtrEnd+3 >= iStart {
			if iPtrEnd > iStart {
				return 0, 0, 0, 0, 0, storage.ErrMalformedImage
			}
			nFrag += iStart - iPtrEnd
			iSize = iEnd - pos.iPtr
			iStart = pos.iPtr
		}
	}
	return iStart, iSize, iEnd, next, nFrag, nil
}

// freeSpaceOnPage returns the byte range [iStart, iStart+iSize) of one leaf
// page to the page's free space, coalescing with adjacent freeblocks
// (btree.c freeSpace, src/btree.c:1918). hdr is the page's content offset
// (0, or 100 for page 1). An error means the page image is corrupt (the
// caller declines to mutate it).
func freeSpaceOnPage(data []byte, hdr, usableSize, iStart, iSize int) error {
	iEnd := iStart + iSize
	pos, err := freeSpaceLocate(data, hdr, usableSize, iStart)
	if err != nil {
		return err
	}
	next := pos.iFreeBlk
	if data[hdr+1] != 0 || data[hdr+2] != 0 {
		// Non-empty chain: coalesce with the touched neighbors and fold
		// their seams into the fragmented count.
		iStart, iSize, iEnd, next, nFrag, err := freeSpaceCoalesce(data, hdr, usableSize, pos, iStart, iSize, iEnd)
		if err != nil {
			return err
		}
		if nFrag > int(data[hdr+7]) {
			return storage.ErrMalformedImage
		}
		data[hdr+7] -= byte(nFrag)
		if iStart <= get2(data, hdr+5) {
			return freeSpaceExtendContent(data, hdr, pos.iPtr, iStart, iEnd, next)
		}
		freeSpaceLink(data, hdr, pos.iPtr, iStart, iSize, next)
		return nil
	}
	if iStart <= get2(data, hdr+5) {
		return freeSpaceExtendContent(data, hdr, pos.iPtr, iStart, iEnd, next)
	}
	freeSpaceLink(data, hdr, pos.iPtr, iStart, iSize, next)
	return nil
}

// freeSpaceExtendContent handles a freed range that begins exactly at the
// cell content area's start: the area grows upward instead of chaining a
// freeblock (legal only at the chain head).
func freeSpaceExtendContent(data []byte, hdr, iPtr, iStart, iEnd, next int) error {
	if iStart < get2(data, hdr+5) {
		return storage.ErrMalformedImage
	}
	if iPtr != hdr+1 {
		return storage.ErrMalformedImage
	}
	put2(data, hdr+1, next)
	put2(data, hdr+5, iEnd)
	return nil
}

// freeSpaceLink chains a (possibly coalesced) freeblock at iStart: its
// predecessor's pointer (at iPtr) aims at it, and it carries the successor
// and its own size.
func freeSpaceLink(data []byte, hdr, iPtr, iStart, iSize, next int) {
	put2(data, iPtr, iStart)
	put2(data, iStart, next)
	put2(data, iStart+2, iSize)
}

// dropCellFromLeafPage removes the leaf cell at pointer-array index idx —
// the cell's bytes were already validated to occupy [cellOff, cellOff+sz) —
// without defragmenting the page (btree.c dropCell, src/btree.c:7228): the
// bytes return to the free space, the pointer slot is unlinked, and an
// emptied page is reinitialized (nFree reset, content pointer at the
// usable end). The parsed header struct is kept in sync.
func dropCellFromLeafPage(pg *pager.Page, page *storage.BTreePage, coff, idx, cellOff, sz int, usableSize uint32) error {
	data := pg.Data
	if cellOff+sz > int(usableSize) {
		return storage.ErrMalformedImage
	}
	if err := freeSpaceOnPage(data, coff, int(usableSize), cellOff, sz); err != nil {
		return err
	}
	page.CellCount--
	if page.CellCount == 0 {
		dropCellResetEmptyPage(data, page, coff, usableSize)
		return nil
	}
	dropCellShiftPointers(data, coff, idx, int(page.CellCount))
	put2(data, coff+3, int(page.CellCount))
	return nil
}

// dropCellResetEmptyPage applies dropCell's emptied-page reset: no
// freeblocks, no fragments, content area back to the usable end (zeroPage's
// empty image).
func dropCellResetEmptyPage(data []byte, page *storage.BTreePage, coff int, usableSize uint32) {
	for i := 1; i <= 4; i++ {
		data[coff+i] = 0
	}
	data[coff+7] = 0
	put2(data, coff+5, int(usableSize))
	page.FirstFree = 0
	page.CellContent = int(usableSize)
	page.FragFree = 0
}

// dropCellShiftPointers unlinks the deleted cell's pointer slot: slots after
// idx shift down one, and the vacated tail slot zeroes.
func dropCellShiftPointers(data []byte, coff, idx, cellCount int) {
	ptrBase := coff + storage.CellPointerOffset
	for i := idx; i < cellCount; i++ {
		src := ptrBase + (i+1)*2
		dst := ptrBase + i*2
		data[dst] = data[src]
		data[dst+1] = data[src+1]
	}
	lastPtr := ptrBase + cellCount*2
	data[lastPtr] = 0
	data[lastPtr+1] = 0
}

// allocateSpaceOnPage finds room for nByte cell bytes on one leaf page and
// returns the offset the bytes must be written at (btree.c allocateSpace +
// pageFindSlot, src/btree.c:1743/1836). Precedence: a freeblock slot big
// enough (whole-chain first fit, tiny remainders to the fragmented count),
// then the gap between the cell-pointer array and the content area after a
// defragment when that gap alone is too small. ok=false reports the page
// cannot hold the cell even after defragmentation — the caller must split.
// The parsed header struct is kept in sync with any header mutation.
func allocateSpaceOnPage(pg *pager.Page, page *storage.BTreePage, coff, nByte int, usableSize uint32) (off int, ok bool, err error) {
	data := pg.Data
	gap := coff + storage.CellPointerOffset + 2*int(page.CellCount)
	top, err := leafContentTop(page, coff, int(usableSize))
	if err != nil {
		return 0, false, err
	}
	// Freeblock search — btree.c consults the chain whenever one exists
	// and the pointer array has headroom for the cell's new pointer, even
	// when the content-area gap would also fit (keeping allocations out of
	// the gap preserves it for cell pointers).
	if gap+2 <= top && (data[coff+1] != 0 || data[coff+2] != 0) {
		var found bool
		off, found, err = pageFindSlot(data, coff, int(usableSize), nByte, gap)
		if err != nil {
			return 0, false, err
		}
		if found {
			return off, true, nil
		}
	}
	// No slot fit: when the content-area gap alone cannot take the cell,
	// fold every freeblock and fragment into it (defragmentPage) first.
	top, ok, err = allocateFromContentGap(pg, page, coff, nByte, int(usableSize), gap, top)
	if err != nil || !ok {
		return 0, false, err
	}
	top -= nByte
	put2(data, coff+5, top)
	page.CellContent = top
	return top, true, nil
}

// leafContentTop unwraps the page's cell-content-area start (the fresh-page
// convention: a zeroed page's first cell ends at the usable end; the 64KiB
// wrap is normalized at parse time).
func leafContentTop(page *storage.BTreePage, coff, usableSize int) (int, error) {
	top := page.CellContent
	if top == 0 {
		top = usableSize
	}
	if top > usableSize || coff+storage.CellPointerOffset+2*int(page.CellCount) > top {
		return 0, storage.ErrMalformedImage
	}
	return top, nil
}

// allocateFromContentGap decides whether the cell fits the content-area gap,
// defragmenting first when the gap alone is too small. ok=false means the
// page must split.
func allocateFromContentGap(pg *pager.Page, page *storage.BTreePage, coff, nByte, usableSize, gap, top int) (int, bool, error) {
	if coff+storage.CellPointerOffset+2*int(page.CellCount)+2+nByte <= top {
		return top, true, nil
	}
	if !defragmentLeafPage(pg, page, coff, uint32(usableSize)) {
		return 0, false, nil
	}
	top = page.CellContent
	if top == 0 {
		top = usableSize // fresh-page convention (see leafContentTop)
	}
	if coff+storage.CellPointerOffset+2*int(page.CellCount)+2+nByte > top {
		return 0, false, nil
	}
	return top, true, nil
}

// pageFindSlot searches a page's freeblock chain for a slot of at least
// nByte bytes, removing (or shrinking) the slot it uses (btree.c
// pageFindSlot, src/btree.c:1743). ok=false reports no usable slot; an
// error reports a corrupt chain.
func pageFindSlot(data []byte, hdr, usableSize, nByte, gap int) (off int, ok bool, err error) {
	iAddr := hdr + 1
	pc := get2(data, iAddr)
	maxPC := usableSize - nByte
	for pc <= maxPC {
		x := get2(data, pc+2) - nByte
		if x >= 0 {
			return pageUseSlot(data, hdr, gap, iAddr, pc, x, maxPC)
		}
		// Too small: the next pointer lives IN this slot.
		iAddr = pc
		pc = get2(data, iAddr)
		if pc <= iAddr {
			if pc != 0 {
				return 0, false, storage.ErrMalformedImage
			}
			return 0, false, nil
		}
	}
	if pc > maxPC+nByte-4 {
		return 0, false, storage.ErrMalformedImage
	}
	return 0, false, nil
}

// pageUseSlot consumes the freeblock at pc for an nByte allocation with
// x bytes to spare (btree.c pageFindSlot's two use shapes): a remainder
// under 4 bytes joins the fragmented count and the slot leaves the chain;
// a larger slot shrinks in place and the allocation comes off its end.
func pageUseSlot(data []byte, hdr, gap, iAddr, pc, x, maxPC int) (off int, ok bool, err error) {
	if x < 4 {
		if data[hdr+7] > 57 {
			return 0, false, nil // fragmentation budget spent
		}
		// Remove the slot from the chain; its bytes join the fragmented
		// count.
		data[iAddr] = data[pc]
		data[iAddr+1] = data[pc+1]
		data[hdr+7] += byte(x)
		if pc <= gap {
			return 0, false, storage.ErrMalformedImage
		}
		return pc, true, nil
	}
	if x+pc > maxPC {
		return 0, false, storage.ErrMalformedImage
	}
	// Shrink the slot to its remainder; allocate from its end.
	put2(data, pc+2, x)
	if pc+x <= gap {
		return 0, false, storage.ErrMalformedImage
	}
	return pc + x, true, nil
}

// defragmentLeafPage packs a leaf page's cells contiguously from the usable
// end (btree.c defragmentPage's full-repack path, src/btree.c:1613): cells
// move through a staging buffer so overlapping ranges are safe, the pointer
// array is rewritten in order, the freeblock chain and fragmented count
// clear, and the gap between pointer array and content area is zeroed. A
// page with no freeblocks and no fragments is left untouched (defrag is a
// no-op there). ok=false declines pages whose cells cannot be sized (the
// caller falls back to the split path, which rewrites the page wholesale).
func defragmentLeafPage(pg *pager.Page, page *storage.BTreePage, coff int, usableSize uint32) (ok bool) {
	data := pg.Data
	if page.FirstFree == 0 && page.FragFree == 0 {
		// Cells already packed (the engine's layouts never strand content
		// bytes without accounting): nothing to fold.
		return true
	}
	if page.PageType != storage.PageTypeLeafTable {
		// Freeblocks only arise on table leaves (dropCell is the only
		// producer); a foreign image that carries them on an index leaf
		// is left for the split path's wholesale rewrite.
		return false
	}
	spans, ok := leafCellSpans(data, page, coff, usableSize)
	if !ok {
		return false
	}
	defragmentPackSpans(data, page, coff, usableSize, spans)
	return true
}

// leafCellSpans measures every cell's raw byte range on the page (the
// defragment staging input). ok=false declines malformed images.
func leafCellSpans(data []byte, page *storage.BTreePage, coff int, usableSize uint32) ([]delSpan, bool) {
	n := int(page.CellCount)
	if total := totalSpanBound(n, coff, usableSize); total < 0 {
		return nil, false
	}
	spans := make([]delSpan, n)
	bound := int(usableSize)
	for i := 0; i < n; i++ {
		p := int(storage.CellPointer(data, coff, i, int(usableSize)))
		if p < coff+8+2*n || p >= bound {
			return nil, false
		}
		sz, err := storage.TableLeafCellSizeAt(data, p, int(usableSize))
		if err != nil || p+sz > bound {
			return nil, false
		}
		spans[i] = delSpan{off: p, size: sz}
	}
	return spans, true
}

// totalSpanBound reports the byte budget every packed cell set must fit
// (negative when the pointer array already exceeds it — a corrupt page).
func totalSpanBound(n, coff int, usableSize uint32) int {
	return int(usableSize) - coff - storage.CellPointerOffset - 2*n
}

// defragmentPackSpans stages the spans through an arena and packs them from
// the usable end downward, rewriting the pointer array and clearing the
// free-space accounting (defragmentPage's repack + closing memset).
func defragmentPackSpans(data []byte, page *storage.BTreePage, coff int, usableSize uint32, spans []delSpan) {
	total := 0
	for _, sp := range spans {
		total += sp.size
	}
	arena := make([]byte, total)
	pos := 0
	for _, sp := range spans {
		copy(arena[pos:pos+sp.size], data[sp.off:sp.off+sp.size])
		pos += sp.size
	}
	start := int(usableSize)
	arenaPos := 0
	ptrBase := coff + storage.CellPointerOffset
	for i, sp := range spans {
		start -= sp.size
		copy(data[start:start+sp.size], arena[arenaPos:arenaPos+sp.size])
		arenaPos += sp.size
		put2(data, ptrBase+i*2, start)
	}
	page.CellContent = start
	put2(data, coff+5, start)
	data[coff+1] = 0 // first freeblock: folded into the content area
	data[coff+2] = 0
	data[coff+7] = 0 // fragmented bytes: folded into the content area
	page.FirstFree = 0
	page.FragFree = 0
	// The gap between the pointer array and the content area holds no
	// live bytes after the pack (defragmentPage's closing memset).
	for i := coff + 8 + 2*len(spans); i < start; i++ {
		data[i] = 0
	}
}
