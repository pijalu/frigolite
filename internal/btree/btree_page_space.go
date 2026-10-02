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

func get2(data []byte, off int) int {
	return int(binary.BigEndian.Uint16(data[off : off+2]))
}

func put2(data []byte, off, v int) {
	binary.BigEndian.PutUint16(data[off:off+2], uint16(v))
}

// delSpan is one cell's raw byte range on the page (defragment staging).
type delSpan struct {
	off  int
	size int
}

// freeSpaceOnPage returns the byte range [iStart, iStart+iSize) of one leaf
// page to the page's free space, coalescing with adjacent freeblocks
// (btree.c freeSpace, src/btree.c:1918). hdr is the page's content offset
// (0, or 100 for page 1). An error means the page image is corrupt (the
// caller declines to mutate it).
func freeSpaceOnPage(data []byte, hdr, usableSize, iStart, iSize int) error {
	iEnd := iStart + iSize
	nFrag := 0
	iPtr := hdr + 1
	var iFreeBlk int
	if data[iPtr] == 0 && data[iPtr+1] == 0 {
		iFreeBlk = 0 // freeblock chain is empty
	} else {
		// The chain is in ascending address order: find the first
		// freeblock at or after iStart (its insertion point).
		for {
			iFreeBlk = get2(data, iPtr)
			if iFreeBlk >= iStart {
				break
			}
			if iFreeBlk <= iPtr {
				if iFreeBlk == 0 {
					break
				}
				return storage.ErrMalformedImage
			}
			iPtr = iFreeBlk
		}
		if iFreeBlk > usableSize-4 {
			return storage.ErrMalformedImage
		}
		// Coalesce iFreeBlk onto the end of the freed range.
		if iFreeBlk != 0 && iEnd+3 >= iFreeBlk {
			nFrag = iFreeBlk - iEnd
			if iEnd > iFreeBlk {
				return storage.ErrMalformedImage
			}
			iEnd = iFreeBlk + get2(data, iFreeBlk+2)
			if iEnd > usableSize {
				return storage.ErrMalformedImage
			}
			iSize = iEnd - iStart
			iFreeBlk = get2(data, iFreeBlk)
		}
		// Coalesce iStart onto the end of the preceding freeblock (iPtr
		// is that freeblock when it is not the header pointer).
		if iPtr > hdr+1 {
			iPtrEnd := iPtr + get2(data, iPtr+2)
			if iPtrEnd+3 >= iStart {
				if iPtrEnd > iStart {
					return storage.ErrMalformedImage
				}
				nFrag += iStart - iPtrEnd
				iSize = iEnd - iPtr
				iStart = iPtr
			}
		}
		if nFrag > int(data[hdr+7]) {
			return storage.ErrMalformedImage
		}
		data[hdr+7] -= byte(nFrag)
	}
	if iStart <= get2(data, hdr+5) {
		// The freed range starts at the cell content area's beginning:
		// extend the content area instead of chaining a freeblock.
		if iStart < get2(data, hdr+5) {
			return storage.ErrMalformedImage
		}
		if iPtr != hdr+1 {
			return storage.ErrMalformedImage
		}
		put2(data, hdr+1, iFreeBlk)
		put2(data, hdr+5, iEnd)
		return nil
	}
	// Insert the (possibly coalesced) freeblock into the chain.
	put2(data, iPtr, iStart)
	put2(data, iStart, iFreeBlk)
	put2(data, iStart+2, iSize)
	return nil
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
		// dropCell's emptied-page reset: no freeblocks, no fragments,
		// content area back to the usable end (zeroPage's empty image).
		data[coff+1] = 0
		data[coff+2] = 0
		data[coff+3] = 0
		data[coff+4] = 0
		data[coff+7] = 0
		put2(data, coff+5, int(usableSize))
		page.FirstFree = 0
		page.CellContent = int(usableSize)
		page.FragFree = 0
		return nil
	}
	ptrBase := coff + storage.CellPointerOffset
	for i := idx; i < int(page.CellCount); i++ {
		src := ptrBase + (i+1)*2
		dst := ptrBase + i*2
		data[dst] = data[src]
		data[dst+1] = data[src+1]
	}
	lastPtr := ptrBase + int(page.CellCount)*2
	data[lastPtr] = 0
	data[lastPtr+1] = 0
	put2(data, coff+3, int(page.CellCount))
	return nil
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
	top := page.CellContent
	if top == 0 {
		// Fresh (zero-initialized) page: content starts at the usable end
		// (writeLeafCell's fresh-page convention; the 64KiB wrap is
		// normalized at parse time).
		top = int(usableSize)
	}
	if top > int(usableSize) || gap > top {
		return 0, false, storage.ErrMalformedImage
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
	if gap+2+nByte > top {
		if !defragmentLeafPage(pg, page, coff, usableSize) {
			return 0, false, nil
		}
		top = page.CellContent
		if top == 0 {
			top = int(usableSize) // fresh-page convention (see above)
		}
	}
	if gap+2+nByte > top {
		return 0, false, nil // split needed
	}
	top -= nByte
	put2(data, coff+5, top)
	page.CellContent = top
	return top, true, nil
}

// pageFindSlot searches a page's freeblock chain for a slot of at least
// nByte bytes, removing (or shrinking) the slot it uses (btree.c
// pageFindSlot, src/btree.c:1743). Slots 1-3 bytes larger than the request
// are skipped when the fragmented count would exceed 57. ok=false reports
// no usable slot; an error reports a corrupt chain.
func pageFindSlot(data []byte, hdr, usableSize, nByte, gap int) (off int, ok bool, err error) {
	iAddr := hdr + 1
	pc := get2(data, iAddr)
	maxPC := usableSize - nByte
	for pc <= maxPC {
		size := get2(data, pc+2)
		x := size - nByte
		if x >= 0 {
			if x < 4 {
				if data[hdr+7] > 57 {
					return 0, false, nil // fragmentation budget spent: keep searching
				}
				// Remove the slot; its bytes join the fragmented count.
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
	n := int(page.CellCount)
	ptrBase := coff + storage.CellPointerOffset
	total := 0
	spans := make([]delSpan, n)
	for i := 0; i < n; i++ {
		p := int(storage.CellPointer(data, coff, i, int(usableSize)))
		if p < coff+8+2*n || p >= int(usableSize) {
			return false
		}
		sz, err := storage.TableLeafCellSizeAt(data, p, int(usableSize))
		if err != nil || p+sz > int(usableSize) {
			return false
		}
		spans[i] = delSpan{off: p, size: sz}
		total += sz
	}
	if total > int(usableSize)-coff-storage.CellPointerOffset-2*n {
		return false // cannot happen on a well-formed page
	}
	// Stage the survivor bytes (source and destination ranges overlap).
	arena := make([]byte, total)
	pos := 0
	for _, sp := range spans {
		copy(arena[pos:pos+sp.size], data[sp.off:sp.off+sp.size])
		pos += sp.size
	}
	// Pack from the usable end downward, in pointer-array order, and
	// rewrite the pointers (defragmentPage's repack: cell i's new offset
	// goes back into the pointer array).
	start := int(usableSize)
	arenaPos := 0
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
	for i := coff + 8 + 2*n; i < start; i++ {
		data[i] = 0
	}
	return true
}
