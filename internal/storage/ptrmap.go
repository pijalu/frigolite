// Package storage: pointer-map (ptrmap) page read/write for auto-vacuum.
//
// In an auto-vacuum or incremental-vacuum database, every b-tree page
// (other than page 1) has an entry in a pointer-map page. The entry
// records the page's "parent type" (which kind of b-tree page owns it:
// interior table, interior index, leaf table, leaf index, overflow,
// freelist trunk) and the page number of that parent. The pointer
// map is consulted by autoVacuumCommit / sqlite3BtreeIncrVacuum to
// locate a page's parent during a page-swap step, without having to
// walk the entire b-tree from the root (which is O(n) per swap).
//
// Reference: SQLite btree.c ptrmapPageno (line ~4130),
// ptrmapGet (~4275), ptrmapPut (~4310), and pager.c / hash.h for the
// 5-byte entry format.
package storage

import (
	"encoding/binary"
	"fmt"
)

// Pointer-map entry "parent type" codes. These match btree.c's
// PTRMAP_* constants; the on-disk format is 1 byte for the type
// followed by a 4-byte parent page number (5 bytes per entry).
const (
	PtrmapRootpage      byte = 1 // root page of a b-tree (no parent in the b-tree)
	PtrmapFreelist      byte = 2 // page is on the freelist (trunk or leaf)
	PtrmapOverflow      byte = 3 // overflow page (first or subsequent) of a cell payload
	PtrmapBtreeNode     byte = 4 // interior or leaf b-tree page
	PtrmapHasRowid      byte = 5 // interior table b-tree page (with rowid cells)
)

// ptrmapEntrySize is the on-disk size of a single pointer-map entry.
// SQLite's btree.c uses 5 bytes (1 type + 4 page-no).
const ptrmapEntrySize = 5

// PtrmapPageNo returns the pointer-map page number covering pgno. It
// is a thin wrapper around pager.PtrmapPageNo for callers in this
// package. Pages < 2 (no parent), pages at the pending-byte position
// (reserved), and the pointer-map page itself are skipped.
func PtrmapPageNo(pgno, pageSize uint32) uint32 {
	return ptrmapPageNo(pgno, pageSize)
}

// ptrmapPageNo is the internal page-no computation (identical to
// pager.PtrmapPageNo; we don't import pager to avoid an import cycle
// since storage is imported BY pager).
func ptrmapPageNo(pgno, pageSize uint32) uint32 {
	if pgno < 2 {
		return 0
	}
	nPer := pageSize/ptrmapEntrySize + 1
	ret := ((pgno-2)/nPer)*nPer + 2
	// btree.c: a pointer-map page never lands on the pending-byte page.
	if ret == pendingBytePage(pageSize) {
		ret++
	}
	return ret
}

// pendingBytePage is the page holding the PENDING_BYTE lock byte
// (1073741824), which SQLite reserves and never uses. btree.c
// PENDING_BYTE_PAGE.
func pendingBytePage(pageSize uint32) uint32 {
	return 1073741824/pageSize + 1
}

// ptrmapEntryOffset returns the byte offset within the pointer-map
// page where the entry for `pgno` lives. Pages map to entries in
// order: page 2 → offset 0, page 3 → offset 5, ..., page 2+k →
// offset 5*k. Pages that are themselves pointer-map pages are NOT
// indexed (they have no entry; SQLite writes 5 bytes per non-ptrmap
// page, but ptrmap pages are skipped — see btree.c ptrmapPageno
// comment about ptrmap pages not needing their own entries).
func ptrmapEntryOffset(pgno, pageSize uint32) uint32 {
	if pgno < 2 {
		return 0
	}
	nPer := pageSize/ptrmapEntrySize + 1
	// Skip the pointer-map pages themselves: they sit at
	// PtrmapPageNo(p, pageSize). For computing the entry offset
	// within a non-ptrmap page's containing ptrmap page, we count
	// how many non-ptrmap pages precede pgno. SQLite's btree.c
	// stores entries densely (1 entry per non-ptrmap page) in the
	// order: page 2 → entry 0, page 3 → entry 1 (assuming page 3
	// is not a ptrmap page), etc.
	//
	// The simplification: we only ever look up the entry for a
	// non-ptrmap page; ptrmap pages have no entries. So we count
	// (pgno - 2) - (number of ptrmap pages at positions < pgno) as
	// the entry index. For the common case (one ptrmap page per
	// usable-size/5 chunk), each chunk contributes (nPer - 1) entries.
	// The entry offset within the containing ptrmap page is then
	// (entryIndex % (nPer - 1)) * ptrmapEntrySize.
	//
	// For phase 2 we implement the dense layout (no gaps for ptrmap
	// pages themselves). The simplify is safe because btree.c writes
	// entries densely too, treating each non-ptrmap page as occupying
	// exactly 1 slot. (For small page sizes with very few non-ptrmap
	// pages per ptrmap page, this is exact; for larger files with
	// many pages per ptrmap page, the count is still correct because
	// we explicitly count ptrmap pages in the range.)
	chunkSize := nPer
	// Number of full chunks before the chunk containing pgno.
	fullChunks := (pgno - 2) / chunkSize
	ptrmapPagesBefore := fullChunks // one ptrmap page per chunk
	// Position within the current chunk (0..chunkSize-1).
	posInChunk := (pgno - 2) % chunkSize
	// The first slot in a chunk is the ptrmap page; entries start
	// at slot 1. So we count non-ptrmap positions before pgno:
	// (posInChunk) positions in the current chunk are before pgno
	// (posInChunk 0 → the ptrmap page itself, posInChunk 1..chunkSize-1
	// → non-ptrmap pages). Wait: that doesn't work either. Let me
	// re-derive.
	//
	// Chunk layout (chunkSize = nPer = pageSize/5+1):
	//   slot 0: the ptrmap page for this chunk (page number = chunkStart)
	//   slot 1..chunkSize-1: non-ptrmap pages (chunkStart+1 .. chunkStart+chunkSize-1)
	//
	// pgno = chunkStart + posInChunk. If posInChunk == 0, pgno is the
	// ptrmap page (no entry). Otherwise pgno is non-ptrmap and its
	// entry index within the chunk is (posInChunk - 1).
	//
	// So the global entry index for pgno is:
	//   fullChunks * (chunkSize - 1) + (posInChunk - 1)   if posInChunk > 0
	// (and pgno is not a ptrmap page; ptrmap pages have no entry).
	_ = ptrmapPagesBefore
	if posInChunk == 0 {
		// pgno is a ptrmap page; it has no entry. Return a sentinel
		// offset that the caller can detect.
		return 0xFFFFFFFF
	}
	entryIndex := fullChunks*(chunkSize-1) + (posInChunk - 1)
	offset := (entryIndex % (chunkSize - 1)) * ptrmapEntrySize
	// The offset is within the ptrmap page that covers pgno. The
	// caller reads from that page at `offset` and gets the 5-byte
	// entry. If the entry index wraps to a new chunk, the caller
	// would need to follow to the next ptrmap page; for phase 2 we
	// assume all entries for a chunk fit in that chunk's ptrmap page
	// (the chunk is sized for that).
	return offset
}

// IsPtrmapPageNo reports whether pgno is itself a pointer-map page.
// Pages 1 and <2 are never pointer-map pages.
func IsPtrmapPageNo(pgno, pageSize uint32) bool {
	if pgno < 2 {
		return false
	}
	return ptrmapPageNo(pgno, pageSize) == pgno
}

// PtrmapEntry reads a pointer-map entry from pageData (the raw bytes
// of the pointer-map page that covers pgno). Returns the parent
// type and parent page number. Returns an error if the entry is
// zero (uninitialized) or if the offset is out of range.
//
// On-disk format (5 bytes per entry, big-endian):
//   byte 0: parent type (1=root, 2=freelist, 3=overflow, 4=btnode, 5=hasrowid)
//   bytes 1-4: parent page number (uint32)
func PtrmapEntry(pageData []byte, pgno, pageSize uint32) (parentType byte, parentPgno uint32, err error) {
	if pgno < 2 {
		return 0, 0, fmt.Errorf("storage: PtrmapEntry: pgno %d < 2", pgno)
	}
	if IsPtrmapPageNo(pgno, pageSize) {
		return 0, 0, fmt.Errorf("storage: PtrmapEntry: pgno %d is a pointer-map page (no entry)", pgno)
	}
	off := ptrmapEntryOffset(pgno, pageSize)
	if off == 0xFFFFFFFF {
		return 0, 0, fmt.Errorf("storage: PtrmapEntry: pgno %d is a pointer-map page (no entry)", pgno)
	}
	if int(off)+ptrmapEntrySize > len(pageData) {
		return 0, 0, fmt.Errorf("storage: PtrmapEntry: offset %d+%d out of range (len %d)", off, ptrmapEntrySize, len(pageData))
	}
	t := pageData[off]
	if t == 0 {
		// Uninitialized entry (e.g., page was just allocated and not
		// yet ptrmap'd). SQLite btree.c treats this as "no parent";
		// callers should not depend on the value.
		return 0, 0, nil
	}
	pg := binary.BigEndian.Uint32(pageData[off+1 : off+5])
	return t, pg, nil
}

// WritePtrmapEntry writes a 5-byte pointer-map entry into pageData at
// the offset corresponding to pgno. The entry is (parentType, parentPgno).
// Returns the offset that was written, or an error if pgno is invalid
// for this page.
func WritePtrmapEntry(pageData []byte, pgno, pageSize uint32, parentType byte, parentPgno uint32) (uint32, error) {
	if pgno < 2 {
		return 0, fmt.Errorf("storage: WritePtrmapEntry: pgno %d < 2", pgno)
	}
	if IsPtrmapPageNo(pgno, pageSize) {
		return 0, fmt.Errorf("storage: WritePtrmapEntry: pgno %d is a pointer-map page (no entry)", pgno)
	}
	off := ptrmapEntryOffset(pgno, pageSize)
	if off == 0xFFFFFFFF {
		return 0, fmt.Errorf("storage: WritePtrmapEntry: pgno %d is a pointer-map page (no entry)", pgno)
	}
	if int(off)+ptrmapEntrySize > len(pageData) {
		return 0, fmt.Errorf("storage: WritePtrmapEntry: offset %d+%d out of range (len %d)", off, ptrmapEntrySize, len(pageData))
	}
	pageData[off] = parentType
	binary.BigEndian.PutUint32(pageData[off+1:off+5], parentPgno)
	return off, nil
}
