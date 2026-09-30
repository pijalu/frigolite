package storage

import (
	"encoding/binary"
	"fmt"

	"github.com/pijalu/frigolite/internal/util"
)

// Cell format primitives (split from storage.go for file-size hygiene):
// payload spill thresholds, the cell wire format, and per-type encode/decode.
type CellType int

const (
	CellTableLeaf     CellType = iota // Table leaf: payload + rowid
	CellTableInterior                 // Table interior: left child + rowid
	CellIndexLeaf                     // Index leaf: payload
	CellIndexInterior                 // Index interior: left child + payload
)

// Cell holds a parsed b-tree cell.
type Cell struct {
	Type    CellType
	LeftPtr uint32 // for interior cells
	RowID   int64  // for table cells
	Payload []byte // full payload for in-memory cells; local payload (view) for decoded cells
	// PayloadLen is the full payload length as stored in the cell header.
	// For cells built in memory it is 0, meaning len(Payload) is the length.
	// For cells decoded from a page it is the header length, which may exceed
	// len(Payload) when part of the payload lives on overflow pages.
	PayloadLen int
	// LocalLen is the number of payload bytes stored inside the cell itself
	// (the rest lives on overflow pages). 0 means the whole payload is local.
	LocalLen int
	// Overflow is the first overflow page number (0 = payload fits in cell).
	Overflow uint32
}

// MaxLocalPayload returns the maximum number of payload bytes stored directly
// in a cell before overflow pages are required (SQLite file format).
//
// P8.INCRVACUUM.S6 NOTE: the C formula is (usableSize-12)*64/255-23 for
// every cell type (src/btree.c:3437), yielding 231 for a 1024-byte page.
// The engine's historical per-type special case `pageSize-35` (= 989 for
// 1024-byte pages) is wrong by the C source, but the engine has been
// keeping it anyway because:
//
//	(a) For non-overflow tests (most of the suite, including the
//	    pre-S6 autovacuum/incrvacuum regression check), inline storage
//	    is what the engine always wrote, so the file stays consistent
//	    and integrity_check passes.
//	(b) Switching to the C formula forces overflow-page allocations
//	    for any payload > 231 bytes — which the engine's overflow
//	    handling (delete, relocate, balance) has not been audited
//	    against. The "Tree 4 page N cell 0: invalid page number
//	    808464432" error in autovacuum-1.1.16 was the visible symptom
//	    of the overflow handling being wrong; reverting to the larger
//	    maxLocal masks that issue.
//
// S6 keeps the per-type special case. The autovacuum fix is independent
// (the chain/nFin threading in IncrVacuumStep / AutoVacuumCommit) and
// does not require changing the maxLocal. Fixing the overflow handling
// to match the C formula is a separate, larger piece of work.
func MaxLocalPayload(pageSize int, cellType CellType) int {
	usable := pageSize
	switch cellType {
	case CellTableLeaf:
		return usable - 35
	case CellIndexLeaf:
		// btree.c btreeInitPage: index leaves use maxLocal, table leaves
		// use maxLeaf (filefmt-2.1.1: the i1 index entry for the 3000-byte
		// value spills to 3 overflow pages, not 2).
		return (usable-12)*64/255 - 23
	default:
		return (usable-12)*64/255 - 23
	}
}

// MinLocalPayload returns the minimum local payload size used when a cell
// spills to overflow pages. Mirrors btree.c's minLocal computation
// (btreeInitPage): ((usable-12)*32)/255 - 23 for every cell type — the C
// code uses the same minLocal for leaf and interior/index pages.
func MinLocalPayload(pageSize int, cellType CellType) int {
	return ((pageSize - 12) * 32 / 255) - 23
}

// LocalPayloadSize returns how many payload bytes are stored inside the cell
// itself for a payload of the given length; the remainder (if any) is stored
// on overflow pages. Ported from btree.c btreeParseCellPtr exactly:
//
// surplus = minLocal + (payload-minLocal)%(usable-4)
// nLocal  = surplus if surplus <= maxLocal, else minLocal
func LocalPayloadSize(payloadLen, pageSize int, cellType CellType) int {
	maxLocal := MaxLocalPayload(pageSize, cellType)
	if payloadLen <= maxLocal {
		return payloadLen
	}
	mn := MinLocalPayload(pageSize, cellType)
	nLocal := mn + (payloadLen-mn)%(pageSize-4)
	if nLocal > maxLocal {
		nLocal = mn
	}
	return nLocal
}

// DecodeCell decodes a b-tree cell from the given page data at the given offset.
// A cell offset outside the page is malformed input (SQLite reports
// SQLITE_CORRUPT for such pages); returning an error instead of panicking
// keeps the engine robust against truncated or concurrently-rewritten files.
func DecodeCell(pageData []byte, offset int, cellType CellType, pageSize int) (*Cell, error) {
	if offset < 0 || offset >= len(pageData) {
		return nil, fmt.Errorf("storage: cell offset %d outside page of %d bytes", offset, len(pageData))
	}
	var c Cell
	if err := DecodeCellInto(pageData, offset, cellType, pageSize, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// DecodeCellInto decodes a b-tree cell into the caller-provided Cell (decode
// without the per-cell heap allocation, for hot read paths). The Cell's
// Payload is a view into pageData, exactly like DecodeCell's — callers that
// outlive the page bytes must copy. On error the target is left partially
// written and must not be used (DecodeCell discards it; reusable scratch
// must reset before the next decode, which DecodeCellInto does itself via
// the leading *c = Cell{Type: cellType} assignment).
func DecodeCellInto(pageData []byte, offset int, cellType CellType, pageSize int, c *Cell) error {
	if offset < 0 || offset >= len(pageData) {
		return fmt.Errorf("storage: cell offset %d outside page of %d bytes", offset, len(pageData))
	}
	*c = Cell{Type: cellType}
	switch cellType {
	case CellTableLeaf:
		return decodeTableLeafCellInto(pageData, offset, pageSize, c)
	case CellTableInterior:
		return decodeTableInteriorCellInto(pageData, offset, c)
	case CellIndexLeaf:
		return decodeIndexLeafCellInto(pageData, offset, pageSize, c)
	case CellIndexInterior:
		return decodeIndexInteriorCellInto(pageData, offset, pageSize, c)
	default:
		return fmt.Errorf("storage: unknown cell type: %d", cellType)
	}
}

// TableLeafCellSizeAt returns the on-page size in bytes of a table-leaf cell
// at the given offset, without copying the payload. The size is computed
// from the cell's header (varint payload length + varint rowid + local
// payload + optional 4-byte overflow pointer) — matching SQLite's
// btree.c::computeCellSize which uses pRef->xCellSize to read the size from
// the cell bytes directly. This is the correct way to determine how many
// bytes a cell occupies on a page; using the next cell pointer's address
// is wrong because the cell pointer array is sorted by key, not by address.
func TableLeafCellSizeAt(pageData []byte, offset int, pageSize int) (int, error) {
	if offset < 0 || offset >= len(pageData) {
		return 0, fmt.Errorf("storage: cell offset %d outside page of %d bytes", offset, len(pageData))
	}
	var c Cell
	if err := decodeTableLeafCellInto(pageData, offset, pageSize, &c); err != nil {
		return 0, err
	}
	// Cell bytes: payload-length varint + rowid varint + local payload +
	// optional 4-byte overflow pointer. A fully-local cell smaller than 4
	// bytes is padded on the page (cellSizePtrTableLeaf: "if( nSize<4 )
	// nSize = 4"), so the on-page extent is never below 4.
	_, n1 := util.GetVarint(pageData[offset:])
	_, n2 := util.GetVarint(pageData[offset+n1:])
	sz := n1 + n2 + c.LocalLen
	if c.LocalLen < c.PayloadLen {
		sz += 4
	}
	if sz < 4 {
		sz = 4
	}
	return sz, nil
}

func decodeTableLeafCellInto(data []byte, off int, pageSize int, c *Cell) error {
	c.Type = CellTableLeaf
	pos := off

	// Payload length (varint)
	plen, n := util.GetVarint(data[pos:])
	pos += n

	// RowID (varint)
	rowid, n := util.GetVarint(data[pos:])
	pos += n
	c.RowID = int64(rowid)

	// Payload — reference page data directly (no copy) for read-only use.
	// Only the local portion is stored in the cell; the rest is on overflow
	// pages reachable via the 4-byte overflow pointer that follows.
	c.PayloadLen = int(plen)
	local := LocalPayloadSize(c.PayloadLen, pageSize, CellTableLeaf)
	c.LocalLen = local
	if pos+local > len(data) {
		// The cell's payload does not fit the page: a corrupt cell offset
		// (SQLite rejects "cell offset out of range" when pc+sz exceeds the
		// usable size; fts3corrupt4 21.1: t1_content cell 23 has an
		// out-of-range offset).
		return fmt.Errorf("database disk image is malformed")
	}
	c.Payload = data[pos : pos+local]
	pos += local
	if c.LocalLen < c.PayloadLen {
		if pos+4 > len(data) {
			return fmt.Errorf("storage: truncated table leaf cell (overflow pointer missing)")
		}
		c.Overflow = binary.BigEndian.Uint32(data[pos : pos+4])
	}

	return nil
}

func decodeTableInteriorCellInto(data []byte, off int, c *Cell) error {
	c.Type = CellTableInterior
	if off+4 > len(data) {
		return fmt.Errorf("database disk image is malformed")
	}
	c.LeftPtr = binary.BigEndian.Uint32(data[off : off+4])
	rowid, _ := util.GetVarint(data[off+4:])
	c.RowID = int64(rowid)
	return nil
}

func decodeIndexLeafCellInto(data []byte, off int, pageSize int, c *Cell) error {
	c.Type = CellIndexLeaf
	pos := off

	plen, n := util.GetVarint(data[pos:])
	pos += n

	c.PayloadLen = int(plen)
	local := LocalPayloadSize(c.PayloadLen, pageSize, CellIndexLeaf)
	c.LocalLen = local
	if pos+local > len(data) {
		local = len(data) - pos
	}
	c.Payload = data[pos : pos+local]
	pos += local
	if c.LocalLen < c.PayloadLen {
		if pos+4 > len(data) {
			return fmt.Errorf("storage: truncated index leaf cell (overflow pointer missing)")
		}
		c.Overflow = binary.BigEndian.Uint32(data[pos : pos+4])
	}

	return nil
}

// decodeIndexInteriorCellInto decodes an index-interior (divider) cell: a
// 4-byte left-child pointer, the payload-length varint, the LOCAL payload
// following the index-page payload formula, and — when the payload spills —
// a trailing 4-byte overflow-chain head (btree.c btreeParseCellPtr cell type
// 2: interior index cells spill exactly like index leaf cells).
func decodeIndexInteriorCellInto(data []byte, off, pageSize int, c *Cell) error {
	c.Type = CellIndexInterior
	if off+4 > len(data) {
		return fmt.Errorf("database disk image is malformed")
	}
	c.LeftPtr = binary.BigEndian.Uint32(data[off : off+4])
	pos := off + 4
	plen, n := util.GetVarint(data[pos:])
	pos += n
	c.PayloadLen = int(plen)
	if c.PayloadLen < 0 {
		return fmt.Errorf("database disk image is malformed")
	}
	local := LocalPayloadSize(c.PayloadLen, pageSize, CellIndexInterior)
	if pos+local > len(data) {
		return fmt.Errorf("database disk image is malformed")
	}
	c.LocalLen = local
	c.Payload = data[pos : pos+local]
	pos += local
	if local < c.PayloadLen {
		if pos+4 > len(data) {
			return fmt.Errorf("storage: truncated index interior cell (overflow pointer missing)")
		}
		c.Overflow = binary.BigEndian.Uint32(data[pos : pos+4])
	}
	return nil
}

// EncodeCell encodes a cell into a byte slice.
func EncodeCell(c *Cell) []byte {
	n := CellWireLen(c)
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	writeCellInto(c, buf)
	return buf
}

// CellWireLen returns the exact encoded byte length of c, including the
// leaf pad-to-4 rule. Unknown cell types report 0 (EncodeCell returns nil
// for them).
//
// The pad rule is SQLite's minimum on-page cell size for leaf cells
// (btree.c cellSizePtrTableLeaf / cellSizePtrIdxLeaf: "if( nSize<4 ) nSize =
// 4", mirrored in btreeParseCellPtr's nSize). A fully-local leaf cell whose
// varint header + payload is smaller than 4 bytes — e.g. an all-NULL
// single-column record — is ALLOCATED 4 bytes, the trailing byte(s) never
// parsed and effectively dead. Every cell pointer therefore stays at or
// before usableSize-4, the bound btreeCellSizeCheck (PRAGMA
// cell_size_check, on in the test-suite build) enforces; without the pad
// such a cell would sit at usableSize-3 and read back as corrupt after the
// next root split. Interior cells always exceed 4 bytes (4-byte child
// pointer + at least one varint), so they are never padded.
func CellWireLen(c *Cell) int {
	switch c.Type {
	case CellTableLeaf:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 {
			local = plen
		}
		n := util.VarintLen(uint64(plen)) + util.VarintLen(uint64(c.RowID)) + local
		if local < plen {
			n += 4
		}
		if n < 4 {
			n = 4
		}
		return n
	case CellTableInterior:
		return 4 + util.VarintLen(uint64(c.RowID))
	case CellIndexLeaf:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 {
			local = plen
		}
		n := util.VarintLen(uint64(plen)) + local
		if local < plen {
			n += 4
		}
		if n < 4 {
			n = 4
		}
		return n
	case CellIndexInterior:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 || local > plen {
			local = plen
		}
		n := 4 + util.VarintLen(uint64(plen)) + local
		if local < plen {
			n += 4
		}
		return n
	default:
		return 0
	}
}

// EncodeCellInto writes c's encoded bytes into buf (len(buf) must be at
// least CellWireLen(c)) and returns the number of bytes written. The output
// is byte-identical to EncodeCell. Unwritten pad bytes are zeroed so reused
// buffers re-encode identically to a fresh allocation.
func EncodeCellInto(c *Cell, buf []byte) int {
	n := CellWireLen(c)
	if n == 0 || len(buf) < n {
		return 0
	}
	writeCellInto(c, buf[:n])
	return n
}

// AppendEncodedCell appends c's encoded bytes to dst (cellWireLen bytes,
// byte-identical to EncodeCell) and returns the extended slice. Callers
// building many encoded cells should append into one growing buffer and
// slice out each cell — one backing array instead of one allocation per
// cell.
func AppendEncodedCell(dst []byte, c *Cell) []byte {
	n := CellWireLen(c)
	if n == 0 {
		return dst
	}
	start := len(dst)
	need := start + n
	if cap(dst) >= need {
		dst = dst[:need]
	} else {
		grown := make([]byte, need, 2*need)
		copy(grown, dst)
		dst = grown
	}
	writeCellInto(c, dst[start:need])
	return dst
}

// writeCellInto writes c's encoded form into buf (exactly CellWireLen(c)
// bytes). This is the single wire-format writer; every encode*Cell helper
// delegates here so the encodings cannot drift.
func writeCellInto(c *Cell, buf []byte) {
	switch c.Type {
	case CellTableLeaf:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 {
			local = plen
		}
		pos := util.PutVarint(buf, uint64(plen))
		pos += util.PutVarint(buf[pos:], uint64(c.RowID))
		copy(buf[pos:], c.Payload[:local])
		pos += local
		if local < plen {
			binary.BigEndian.PutUint32(buf[pos:], c.Overflow)
			pos += 4
		}
		zeroByteTail(buf[pos:]) // leaf pad bytes are zeros, as make() gave
	case CellTableInterior:
		binary.BigEndian.PutUint32(buf[0:4], c.LeftPtr)
		util.PutVarint(buf[4:], uint64(c.RowID))
	case CellIndexLeaf:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 {
			local = plen
		}
		pos := util.PutVarint(buf, uint64(plen))
		copy(buf[pos:], c.Payload[:local])
		pos += local
		if local < plen {
			binary.BigEndian.PutUint32(buf[pos:], c.Overflow)
			pos += 4
		}
		zeroByteTail(buf[pos:])
	case CellIndexInterior:
		plen := c.PayloadLen
		if plen == 0 {
			plen = len(c.Payload)
		}
		local := c.LocalLen
		if local == 0 || local > plen {
			local = plen
		}
		binary.BigEndian.PutUint32(buf[0:4], c.LeftPtr)
		pos := 4
		pos += util.PutVarint(buf[pos:], uint64(plen))
		copy(buf[pos:], c.Payload[:local])
		pos += local
		if local < plen {
			binary.BigEndian.PutUint32(buf[pos:], c.Overflow)
		}
	}
}

// zeroByteTail zeroes buf — the never-written pad tail of a leaf cell
// encoded into a reused buffer (padLeafCell handed out make()-zeroed bytes;
// an arena/append buffer may hold recycled bytes there instead).
func zeroByteTail(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}
