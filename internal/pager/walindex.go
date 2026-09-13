package pager

// walindex.go — the wal-index ("-shm") byte layout codec and hash-table
// engine, ported from SQLite src/wal.c (P7.WAL-G7 slice 1).
//
// The wal-index is a shared-memory file ("test.db-shm") whose first 32KiB
// page carries the double-buffered wal-index header, the checkpoint info and
// the first pgno→frame hash table; further hash tables live on subsequent
// 32KiB pages. Layout reference (src/wal.c, struct WalIndexHdr/WalCkptInfo and
// the 136-byte header schematic, L306-475):
//
//	WalIndexHdr (48 bytes), copies at offsets 0 and 48 (little-endian):
//	   0: iVersion          u32  3007000 (WALINDEX_MAX_VERSION)
//	   4: unused            u32
//	   8: iChange           u32  (incremented each transaction)
//	  12: isInit            u8   (1 once initialized)
//	  13: bigEndCksum       u8   (WAL frame checksum byte order flag)
//	  14: szPage            u16  (1 == 65536; else page size)
//	  16: mxFrame           u32  (index of last valid frame)
//	  20: nPage             u32  (db size in pages)
//	  24: aFrameCksum[2]    u32  (of the last commit frame)
//	  32: aSalt[2]          u32  (copies of the WAL header salts)
//	  40: aCksum[2]         u32  (checksum over bytes [0:40])
//	WalCkptInfo (40 bytes) at offset 96:
//	  96: nBackfill         u32
//	 100: aReadMark[5]      u32  (READMARK_NOT_USED = 0xffffffff)
//	 120: aLock[8]          (lock bytes — never read/written as data)
//	 128: nBackfillAttempted u32
//	 132: notUsed0          u32
//
// Page 0 layout: [0:136) header, [136:16384) aPgno (4062 u32), [16384:32768)
// aHash (8192 u16). Pages 1..N: [0:16384) aPgno (4096 u32), [16384:32768)
// aHash (8192 u16). Slot value N refers to frame iZero+N.

import (
	"encoding/binary"
	"fmt"
)

// Wal-index layout constants (src/wal.c L278-498, L615-628).
const (
	// WalIndexPageSize is WALINDEX_PGSZ: the size of one wal-index page.
	WalIndexPageSize = 32768
	// WalIndexHdrSize is WALINDEX_HDR_SIZE: two WalIndexHdr copies (48 each)
	// plus WalCkptInfo (40 bytes).
	WalIndexHdrSize = 136
	// WalIndexLockOffset is WALINDEX_LOCK_OFFSET — the first shm lock byte
	// (WalCkptInfo.aLock), reserved for the lock protocol (slice 2).
	WalIndexLockOffset = 120
	// WalIndexMaxVersion is WALINDEX_MAX_VERSION.
	WalIndexMaxVersion = 3007000
	// walIndexHdrOff0 / walIndexHdrOff1 are the two header copies' offsets.
	walIndexHdrOff0 = 0
	walIndexHdrOff1 = 48
	// walIndexCkptOff is the WalCkptInfo offset (48*2).
	walIndexCkptOff = 96
	// WalHashtableNPage is HASHTABLE_NPAGE (must be a power of two).
	WalHashtableNPage = 4096
	// WalHashtableNPageOne is HASHTABLE_NPAGE_ONE: frames indexed by page 0's
	// hash table (4096 minus the 34 u32 consumed by the wal-index header).
	WalHashtableNPageOne = WalHashtableNPage - WalIndexHdrSize/4
	// WalHashtableNSlot is HASHTABLE_NSLOT (must be a power of two).
	WalHashtableNSlot = WalHashtableNPage * 2
	// WalHashtableHash1 is HASHTABLE_HASH_1 (should be prime).
	WalHashtableHash1 = 383
	// WalNReader is WAL_NREADER: the number of read marks.
	WalNReader = 5
	// ReadmarkNotUsed is READMARK_NOT_USED.
	ReadmarkNotUsed = 0xffffffff
)

// WalIndexHdr is the Go mirror of the C WalIndexHdr struct (wal.c L306-330).
// It is the per-connection cached copy (C's pWal->hdr) of the shared header.
type WalIndexHdr struct {
	IVersion    uint32
	Unused      uint32
	IChange     uint32
	IsInit      bool
	BigEndCksum bool
	SzPage      uint16 // 1 == 65536, else the database page size
	MxFrame     uint32
	NPage       uint32
	AFrameCksum [2]uint32
	ASalt       [2]uint32
	ACksum      [2]uint32
}

// Equal reports whether two headers are byte-identical (C's memcmp of
// WalIndexHdr in walIndexTryHdr / walTryBeginRead / sqlite3WalBeginWriteTransaction).
func (h *WalIndexHdr) Equal(o *WalIndexHdr) bool {
	return *h == *o
}

// PageSize decodes hdr.szPage the way walIndexTryHdr does
// (szPage&0xfe00 + (szPage&1)<<16): the value 1 means 65536.
func (h *WalIndexHdr) PageSize() uint32 {
	return uint32(h.SzPage&0xfe00) + uint32(h.SzPage&0x0001)<<16
}

// setPageSizeForHdr encodes a page size into hdr.szPage the way
// walIndexRecover does: ((szPage&0xff00) | (szPage>>16)) — the value 1
// encodes 65536.
func setPageSizeForHdr(h *WalIndexHdr, szPage uint32) {
	h.SzPage = uint16(szPage&0xff00) | uint16(szPage>>16)
}

// walIndexHdrChecksum computes the header checksum over its first 40 bytes
// (everything before aCksum) with walChecksumBytes(1, ...) — native = little
// endian byte order, zero seed (wal.c walIndexWriteHdr / walIndexTryHdr).
func walIndexHdrChecksum(buf []byte) (uint32, uint32) {
	return WalChecksumBytes(false, buf[:40], 0, 0)
}

// EncodeWalIndexHdr serializes h into buf[0:48] (little-endian; the aCksum
// field must already be set by the caller).
func EncodeWalIndexHdr(h *WalIndexHdr, buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:], h.IVersion)
	binary.LittleEndian.PutUint32(buf[4:], h.Unused)
	binary.LittleEndian.PutUint32(buf[8:], h.IChange)
	buf[12] = b2u8(h.IsInit)
	buf[13] = b2u8(h.BigEndCksum)
	binary.LittleEndian.PutUint16(buf[14:], h.SzPage)
	binary.LittleEndian.PutUint32(buf[16:], h.MxFrame)
	binary.LittleEndian.PutUint32(buf[20:], h.NPage)
	binary.LittleEndian.PutUint32(buf[24:], h.AFrameCksum[0])
	binary.LittleEndian.PutUint32(buf[28:], h.AFrameCksum[1])
	binary.LittleEndian.PutUint32(buf[32:], h.ASalt[0])
	binary.LittleEndian.PutUint32(buf[36:], h.ASalt[1])
	binary.LittleEndian.PutUint32(buf[40:], h.ACksum[0])
	binary.LittleEndian.PutUint32(buf[44:], h.ACksum[1])
}

// DecodeWalIndexHdr parses the 48-byte header copy at buf[0:48].
func DecodeWalIndexHdr(buf []byte) WalIndexHdr {
	var h WalIndexHdr
	h.IVersion = binary.LittleEndian.Uint32(buf[0:])
	h.Unused = binary.LittleEndian.Uint32(buf[4:])
	h.IChange = binary.LittleEndian.Uint32(buf[8:])
	h.IsInit = buf[12] != 0
	h.BigEndCksum = buf[13] != 0
	h.SzPage = binary.LittleEndian.Uint16(buf[14:])
	h.MxFrame = binary.LittleEndian.Uint32(buf[16:])
	h.NPage = binary.LittleEndian.Uint32(buf[20:])
	h.AFrameCksum[0] = binary.LittleEndian.Uint32(buf[24:])
	h.AFrameCksum[1] = binary.LittleEndian.Uint32(buf[28:])
	h.ASalt[0] = binary.LittleEndian.Uint32(buf[32:])
	h.ASalt[1] = binary.LittleEndian.Uint32(buf[36:])
	h.ACksum[0] = binary.LittleEndian.Uint32(buf[40:])
	h.ACksum[1] = binary.LittleEndian.Uint32(buf[44:])
	return h
}

// WalCkptInfo is the Go mirror of the C WalCkptInfo struct (wal.c L361-378):
// checkpoint bookkeeping shared through the wal-index header.
type WalCkptInfo struct {
	NBackfill          uint32
	AReadMark          [WalNReader]uint32
	NBackfillAttempted uint32
	NotUsed0           uint32
}

// EncodeWalCkptInfo serializes info into buf at the WalCkptInfo offset (96).
// The 8 aLock bytes (120..127) are never written as data.
func EncodeWalCkptInfo(info *WalCkptInfo, buf []byte) {
	off := walIndexCkptOff
	binary.LittleEndian.PutUint32(buf[off:], info.NBackfill)
	for i, m := range info.AReadMark {
		binary.LittleEndian.PutUint32(buf[off+4+4*i:], m)
	}
	// [120:128) aLock: reserved for the lock protocol, left untouched.
	binary.LittleEndian.PutUint32(buf[off+32:], info.NBackfillAttempted)
	binary.LittleEndian.PutUint32(buf[off+36:], info.NotUsed0)
}

// DecodeWalCkptInfo parses the WalCkptInfo at buf's offset 96.
func DecodeWalCkptInfo(buf []byte) WalCkptInfo {
	var info WalCkptInfo
	off := walIndexCkptOff
	info.NBackfill = binary.LittleEndian.Uint32(buf[off:])
	for i := range info.AReadMark {
		info.AReadMark[i] = binary.LittleEndian.Uint32(buf[off+4+4*i:])
	}
	info.NBackfillAttempted = binary.LittleEndian.Uint32(buf[off+32:])
	info.NotUsed0 = binary.LittleEndian.Uint32(buf[off+36:])
	return info
}

// b2u8 converts a bool to its 0/1 byte form (C's u8 boolean fields).
func b2u8(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// walIndexHash ports walHash (wal.c L1132): hash of a page number into the
// 8192-slot hash table.
func walIndexHash(iPage uint32) int {
	return int(iPage*WalHashtableHash1) & (WalHashtableNSlot - 1)
}

// walIndexNextHash ports walNextHash (wal.c L1137): linear probe step.
func walIndexNextHash(iPriorHash int) int {
	return (iPriorHash + 1) & (WalHashtableNSlot - 1)
}

// walFramePageOf ports walFramePage (wal.c L1197): the wal-index page (32KiB
// units) whose hash table indexes WAL frame iFrame.
func walFramePageOf(iFrame uint32) int {
	return int(iFrame+WalHashtableNPage-WalHashtableNPageOne-1) / WalHashtableNPage
}

// walHashLoc describes one page's pgno→frame mapping region — the Go mirror
// of C's WalHashLoc (wal.c L1146-1153). All accessors go through the
// underlying 32KiB page buffer (no unsafe aliasing).
type walHashLoc struct {
	page  []byte // 32768-byte wal-index page
	iHash int    // wal-index page number (0 = header page)
	iZero uint32 // one less than the frame number of the first indexed frame
}

// aPgnoOff/aPgnoCount are the byte offset and entry count of the aPgno array
// on a wal-index page (page 0 starts after the 136-byte header).
func walHashLocApgno(iHash int) (off, count int) {
	if iHash == 0 {
		return WalIndexHdrSize, WalHashtableNPageOne
	}
	return 0, WalHashtableNPage
}

// newWalHashLoc builds the hash-location view of page iHash. The page buffer
// must already be materialized (WalIndex.page).
func newWalHashLoc(page []byte, iHash int) walHashLoc {
	loc := walHashLoc{page: page, iHash: iHash}
	if iHash == 0 {
		loc.iZero = 0
	} else {
		loc.iZero = WalHashtableNPageOne + uint32(iHash-1)*WalHashtableNPage
	}
	return loc
}

// pgno returns aPgno[idx] (idx is 0-based within this table's mapping
// region): the database page number of frame iZero+idx+1.
func (l *walHashLoc) pgno(idx int) uint32 {
	off, _ := walHashLocApgno(l.iHash)
	return binary.LittleEndian.Uint32(l.page[off+4*idx:])
}

// setPgno sets aPgno[idx].
func (l *walHashLoc) setPgno(idx int, v uint32) {
	off, _ := walHashLocApgno(l.iHash)
	binary.LittleEndian.PutUint32(l.page[off+4*idx:], v)
}

// slot returns aHash[i] (i in [0, 8192)): the 1-based index (relative to
// iZero) of the frame occupying hash slot i, or 0 when the slot is free.
func (l *walHashLoc) slot(i int) uint16 {
	return binary.LittleEndian.Uint16(l.page[walHashApgnoEnd+2*i:])
}

// setSlot sets aHash[i].
func (l *walHashLoc) setSlot(i int, v uint16) {
	binary.LittleEndian.PutUint16(l.page[walHashApgnoEnd+2*i:], v)
}

// zeroMapping zeroes the whole mapping region (aPgno + aHash) of the page —
// walIndexAppend's "first entry in this hash table" reset (wal.c L1310-1316).
func (l *walHashLoc) zeroMapping() {
	off, _ := walHashLocApgno(l.iHash)
	for i := range l.page[off:walHashApgnoEnd] {
		l.page[off+i] = 0
	}
	for i := range l.page[walHashApgnoEnd:] {
		l.page[walHashApgnoEnd+i] = 0
	}
}

// walHashApgnoEnd is the byte offset where aHash starts on every wal-index
// page: HASHTABLE_NPAGE u32 entries = 16384 bytes (page 0's aPgno starts at
// 136 and holds 4062 entries, ending at exactly 16384 — walHashGet computes
// aHash as &aPgno[HASHTABLE_NPAGE] on the unshifted page pointer).
const walHashApgnoEnd = WalHashtableNPage * 4

// errWalCorrupt is the wal-index corruption error (SQLITE_CORRUPT family).
var errWalCorrupt = fmt.Errorf("database disk image is malformed")
