// Package storage implements the SQLite file format primitives:
// database header, page types, cell formats, and record encoding.
package storage

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

// Database header offsets and constants.
const (
	HeaderSize        = 100
	HeaderMagicOffset = 0
	HeaderMagic       = "SQLite format 3\x00"
	PageSizeOffset    = 16 // 2 bytes, big-endian
	PageSizeDefault   = 4096

	// Page type constants
	PageTypeInteriorIndex byte = 0x02
	PageTypeInteriorTable byte = 0x05
	PageTypeLeafIndex     byte = 0x0a
	PageTypeLeafTable     byte = 0x0d

	// Default max embedded payload fraction
	MaxEmbeddedFraction  = 64
	MinEmbeddedFraction  = 32
	LeafEmbeddedFraction = 32

	// Cell pointer array starts at this offset from page header
	CellPointerOffset = 8
)

// DatabaseHeader represents the 100-byte header of an SQLite database file.
type DatabaseHeader struct {
	PageSize         uint32 // actual page size (1 in file means 65536)
	WriteVersion     byte
	ReadVersion      byte
	ReservedSpace    byte
	MaxPayloadFrac   byte
	MinPayloadFrac   byte
	LeafPayloadFrac  byte
	FileChangeCount  uint32
	DatabaseSize     uint32 // in pages
	FirstFreelistTrn uint32
	TotalFreelist    uint32
	SchemaCookie     uint32
	SchemaFormat     uint32
	DefaultCacheSize uint32
	LargestBTreePage uint32
	TextEncoding     uint32
	UserVersion      uint32
	IncrementalVac   uint32
	ApplicationID    uint32
	VersionValidFor  uint32
	SQLiteVersionNum uint32
}

// ParseHeader reads the database header from a 100-byte slice.
func ParseHeader(data []byte) (*DatabaseHeader, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("storage: header too short: %d", len(data))
	}
	magic := string(data[HeaderMagicOffset : HeaderMagicOffset+16])
	if magic != HeaderMagic {
		return nil, fmt.Errorf("storage: invalid magic: %q", magic)
	}
	ps := binary.BigEndian.Uint16(data[16:18])
	var pageSize uint32
	if ps == 1 {
		pageSize = 65536
	} else {
		pageSize = uint32(ps)
	}
	h := &DatabaseHeader{
		PageSize:         pageSize,
		WriteVersion:     data[18],
		ReadVersion:      data[19],
		ReservedSpace:    data[20],
		MaxPayloadFrac:   data[21],
		MinPayloadFrac:   data[22],
		LeafPayloadFrac:  data[23],
		FileChangeCount:  binary.BigEndian.Uint32(data[24:28]),
		DatabaseSize:     binary.BigEndian.Uint32(data[28:32]),
		FirstFreelistTrn: binary.BigEndian.Uint32(data[32:36]),
		TotalFreelist:    binary.BigEndian.Uint32(data[36:40]),
		SchemaCookie:     binary.BigEndian.Uint32(data[40:44]),
		SchemaFormat:     binary.BigEndian.Uint32(data[44:48]),
		DefaultCacheSize: binary.BigEndian.Uint32(data[48:52]),
		LargestBTreePage: binary.BigEndian.Uint32(data[52:56]),
		TextEncoding:     binary.BigEndian.Uint32(data[56:60]),
		UserVersion:      binary.BigEndian.Uint32(data[60:64]),
		IncrementalVac:   binary.BigEndian.Uint32(data[64:68]),
		ApplicationID:    binary.BigEndian.Uint32(data[72:76]),
		VersionValidFor:  binary.BigEndian.Uint32(data[92:96]),
		SQLiteVersionNum: binary.BigEndian.Uint32(data[96:100]),
	}
	if h.MaxPayloadFrac == 0 {
		h.MaxPayloadFrac = MaxEmbeddedFraction
	}
	if h.MinPayloadFrac == 0 {
		h.MinPayloadFrac = MinEmbeddedFraction
	}
	if h.LeafPayloadFrac == 0 {
		h.LeafPayloadFrac = LeafEmbeddedFraction
	}
	return h, nil
}

// Encode encodes the header into a 100-byte slice.
func (h *DatabaseHeader) Encode() []byte {
	buf := make([]byte, HeaderSize)
	copy(buf[HeaderMagicOffset:], HeaderMagic)
	var ps uint16
	if h.PageSize >= 65536 {
		ps = 1
	} else {
		ps = uint16(h.PageSize)
	}
	binary.BigEndian.PutUint16(buf[16:18], ps)
	buf[18] = h.WriteVersion
	buf[19] = h.ReadVersion
	buf[20] = h.ReservedSpace
	buf[21] = h.MaxPayloadFrac
	buf[22] = h.MinPayloadFrac
	buf[23] = h.LeafPayloadFrac
	binary.BigEndian.PutUint32(buf[24:28], h.FileChangeCount)
	binary.BigEndian.PutUint32(buf[28:32], h.DatabaseSize)
	binary.BigEndian.PutUint32(buf[32:36], h.FirstFreelistTrn)
	binary.BigEndian.PutUint32(buf[36:40], h.TotalFreelist)
	binary.BigEndian.PutUint32(buf[40:44], h.SchemaCookie)
	binary.BigEndian.PutUint32(buf[44:48], h.SchemaFormat)
	binary.BigEndian.PutUint32(buf[48:52], h.DefaultCacheSize)
	binary.BigEndian.PutUint32(buf[52:56], h.LargestBTreePage)
	binary.BigEndian.PutUint32(buf[56:60], h.TextEncoding)
	binary.BigEndian.PutUint32(buf[60:64], h.UserVersion)
	binary.BigEndian.PutUint32(buf[64:68], h.IncrementalVac)
	binary.BigEndian.PutUint32(buf[72:76], h.ApplicationID)
	binary.BigEndian.PutUint32(buf[92:96], h.VersionValidFor)
	binary.BigEndian.PutUint32(buf[96:100], h.SQLiteVersionNum)
	return buf
}

// DefaultHeader returns a header with sensible defaults for a new database.
func DefaultHeader(pageSize uint32) *DatabaseHeader {
	if pageSize == 0 {
		pageSize = PageSizeDefault
	}
	return &DatabaseHeader{
		PageSize:         pageSize,
		WriteVersion:     1, // legacy (1=journal, 2=WAL)
		ReadVersion:      1,
		ReservedSpace:    0,
		MaxPayloadFrac:   MaxEmbeddedFraction,
		MinPayloadFrac:   MinEmbeddedFraction,
		LeafPayloadFrac:  LeafEmbeddedFraction,
		TextEncoding:     1, // UTF-8
		SchemaFormat:     4, // 4 = format 4 (current)
		SQLiteVersionNum: 3045000,
	}
}

// BTreePage is a parsed b-tree page header.
type BTreePage struct {
	PageType     byte
	FirstFree    uint16
	CellCount    uint16
	CellContent  uint16 // offset where cell content starts
	FragFree     byte
	RightmostPtr uint32 // for interior pages
}

// ParsePage parses a b-tree page header from page data at the given content offset.
// pageData must be at least 12 bytes from the start of the content.
// The contentOffset is 100 for page 1 (after database header), 0 for other pages.
func ParsePage(pageData []byte, pageSize int, contentOffset int) (*BTreePage, error) {
	header := pageData[contentOffset:]
	if len(header) < 8 {
		return nil, fmt.Errorf("storage: page data too short: %d", len(pageData))
	}
	p := &BTreePage{
		PageType:    header[0],
		FirstFree:   binary.BigEndian.Uint16(header[1:3]),
		CellCount:   binary.BigEndian.Uint16(header[3:5]),
		CellContent: binary.BigEndian.Uint16(header[5:7]),
		FragFree:    header[7],
	}
	switch p.PageType {
	case PageTypeInteriorIndex, PageTypeInteriorTable:
		p.RightmostPtr = binary.BigEndian.Uint32(header[8:12])
	default:
		// Leaf pages don't have rightmost pointer
	}
	if err := validatePageHeader(p, pageData, pageSize, contentOffset); err != nil {
		return nil, err
	}
	return p, nil
}

// validatePageHeader enforces the page-type and free-space consistency
// checks behind SQLite's "free space corruption" (reported as "database
// disk image is malformed"): the cell content area must start after the cell
// pointer array and before the end of the page, and the first free-block
// pointer must lie inside the page. Crash-written pages carry inconsistent
// offsets (fts3corrupt4 21.1/24.1: Tree 4/7 free space corruption; a cell
// pointer beyond the page). The engine now writes cellcontent=pageSize on
// empty pages (matching SQLite), so a page with cellcontent=0 or an
// out-of-range value is corrupt. The stored offset is a 16-bit field; for a
// 65536-byte page the page size wraps to 0 on disk (SQLite writes
// (u16)cellOffset, and a full 65536 offset becomes 0), so a CellContent of 0
// is valid only when pageSize is exactly 65536. The checks are skipped for
// partial/synthetic page buffers smaller than a real page (unit tests build
// 12-byte headers).
func validatePageHeader(p *BTreePage, pageData []byte, pageSize int, contentOffset int) error {
	switch p.PageType {
	case PageTypeInteriorIndex, PageTypeInteriorTable, PageTypeLeafIndex, PageTypeLeafTable:
	default:
		return fmt.Errorf("storage: unknown page type: 0x%02x", p.PageType)
	}
	cellPtrEnd := uint16(contentOffset + 8 + 2*int(p.CellCount))
	cellContent := int(p.CellContent)
	if cellContent == 0 && pageSize == 65536 {
		cellContent = 65536
	}
	if len(pageData) >= pageSize && (cellContent < int(cellPtrEnd) || cellContent > pageSize) {
		return fmt.Errorf("database disk image is malformed")
	}
	if p.FirstFree > uint16(pageSize) {
		return fmt.Errorf("database disk image is malformed")
	}
	return nil
}

// CellPointer reads a cell pointer at index i from the cell pointer array.
// The cell pointer array starts at (contentOffset + 8) in pageData. The
// pointer is masked with (pageSize-1), matching SQLite's findCell maskPage
// behavior: an out-of-range pointer (crash-corrupted page) wraps into the
// page buffer instead of erroring (fts3corrupt4 25.1: t2 page 7 has a cell
// pointer 4310 on a 4096-page; the oracle reads it fine).
func CellPointer(pageData []byte, contentOffset int, i int, pageSize int) uint16 {
	offset := contentOffset + 8 + i*2
	return binary.BigEndian.Uint16(pageData[offset:offset+2]) & uint16(pageSize-1)
}

// CellType indicates the kind of b-tree cell.
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
func MaxLocalPayload(pageSize int, cellType CellType) int {
	usable := pageSize
	switch cellType {
	case CellTableLeaf, CellIndexLeaf:
		return usable - 35
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
	switch cellType {
	case CellTableLeaf:
		return decodeTableLeafCell(pageData, offset, pageSize)
	case CellTableInterior:
		return decodeTableInteriorCell(pageData, offset)
	case CellIndexLeaf:
		return decodeIndexLeafCell(pageData, offset, pageSize)
	case CellIndexInterior:
		return decodeIndexInteriorCell(pageData, offset)
	default:
		return nil, fmt.Errorf("storage: unknown cell type: %d", cellType)
	}
}

func decodeTableLeafCell(data []byte, off int, pageSize int) (*Cell, error) {
	c := &Cell{Type: CellTableLeaf}
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
		return nil, fmt.Errorf("database disk image is malformed")
	}
	c.Payload = data[pos : pos+local]
	pos += local
	if c.LocalLen < c.PayloadLen {
		if pos+4 > len(data) {
			return nil, fmt.Errorf("storage: truncated table leaf cell (overflow pointer missing)")
		}
		c.Overflow = binary.BigEndian.Uint32(data[pos : pos+4])
	}

	return c, nil
}

func decodeTableInteriorCell(data []byte, off int) (*Cell, error) {
	c := &Cell{Type: CellTableInterior}
	c.LeftPtr = binary.BigEndian.Uint32(data[off : off+4])
	rowid, _ := util.GetVarint(data[off+4:])
	c.RowID = int64(rowid)
	return c, nil
}

func decodeIndexLeafCell(data []byte, off int, pageSize int) (*Cell, error) {
	c := &Cell{Type: CellIndexLeaf}
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
			return nil, fmt.Errorf("storage: truncated index leaf cell (overflow pointer missing)")
		}
		c.Overflow = binary.BigEndian.Uint32(data[pos : pos+4])
	}

	return c, nil
}

func decodeIndexInteriorCell(data []byte, off int) (*Cell, error) {
	c := &Cell{Type: CellIndexInterior}
	c.LeftPtr = binary.BigEndian.Uint32(data[off : off+4])
	pos := off + 4
	plen, n := util.GetVarint(data[pos:])
	pos += n
	payloadLen := int(plen)
	if pos+payloadLen > len(data) {
		payloadLen = len(data) - pos
	}
	c.Payload = data[pos : pos+payloadLen]
	return c, nil
}

// EncodeCell encodes a cell into a byte slice.
func EncodeCell(c *Cell) []byte {
	switch c.Type {
	case CellTableLeaf:
		return encodeTableLeafCell(c)
	case CellTableInterior:
		return encodeTableInteriorCell(c)
	case CellIndexLeaf:
		return encodeIndexLeafCell(c)
	case CellIndexInterior:
		return encodeIndexInteriorCell(c)
	default:
		return nil
	}
}

func encodeTableLeafCell(c *Cell) []byte {
	plen := c.PayloadLen
	if plen == 0 {
		plen = len(c.Payload)
	}
	local := c.LocalLen
	if local == 0 {
		local = plen
	}
	plenLen := util.VarintLen(uint64(plen))
	rowidLen := util.VarintLen(uint64(c.RowID))
	// The cell stores the full payload length in the header, the local
	// payload portion, then a 4-byte overflow pointer when the payload
	// spills to overflow pages.
	totalLen := plenLen + rowidLen + local
	if local < plen {
		totalLen += 4
	}
	buf := make([]byte, totalLen)
	pos := 0
	pos += util.PutVarint(buf[pos:], uint64(plen))
	pos += util.PutVarint(buf[pos:], uint64(c.RowID))
	copy(buf[pos:], c.Payload[:local])
	pos += local
	if local < plen {
		binary.BigEndian.PutUint32(buf[pos:], c.Overflow)
	}
	return buf
}

func encodeTableInteriorCell(c *Cell) []byte {
	rowidLen := util.VarintLen(uint64(c.RowID))
	buf := make([]byte, 4+rowidLen)
	binary.BigEndian.PutUint32(buf[0:4], c.LeftPtr)
	util.PutVarint(buf[4:], uint64(c.RowID))
	return buf
}

func encodeIndexLeafCell(c *Cell) []byte {
	plen := c.PayloadLen
	if plen == 0 {
		plen = len(c.Payload)
	}
	local := c.LocalLen
	if local == 0 {
		local = plen
	}
	plenLen := util.VarintLen(uint64(plen))
	totalLen := plenLen + local
	if local < plen {
		totalLen += 4
	}
	buf := make([]byte, totalLen)
	pos := 0
	pos += util.PutVarint(buf[pos:], uint64(plen))
	copy(buf[pos:], c.Payload[:local])
	pos += local
	if local < plen {
		binary.BigEndian.PutUint32(buf[pos:], c.Overflow)
	}
	return buf
}

func encodeIndexInteriorCell(c *Cell) []byte {
	plen := len(c.Payload)
	plenLen := util.VarintLen(uint64(plen))
	buf := make([]byte, 4+plenLen+plen)
	binary.BigEndian.PutUint32(buf[0:4], c.LeftPtr)
	pos := 4
	pos += util.PutVarint(buf[pos:], uint64(plen))
	copy(buf[pos:], c.Payload)
	return buf
}

// SerialType constants for record encoding.
const (
	SerialNull  = 0
	SerialInt8  = 1
	SerialInt16 = 2
	SerialInt24 = 3
	SerialInt32 = 4
	SerialInt48 = 5
	SerialInt64 = 6
	SerialFloat = 7
	SerialZero  = 8
	SerialOne   = 9
	SerialMin   = 12 // first usable string/blob serial type
)

// SerialTypeLength returns the data length for a serial type code.
// Returns the byte length of the value.
func SerialTypeLength(serialType uint64) (int64, error) {
	switch {
	case serialType == SerialNull:
		return 0, nil
	case serialType >= SerialInt8 && serialType <= SerialInt64:
		// Serial type -> byte length: 1→1, 2→2, 3→3, 4→4, 5→6, 6→8
		switch serialType {
		case 5:
			return 6, nil
		case 6:
			return 8, nil
		default:
			return int64(serialType), nil
		}
	case serialType == SerialFloat:
		return 8, nil
	case serialType == SerialZero || serialType == SerialOne:
		return 0, nil
	case serialType >= SerialMin:
		if serialType%2 == 0 {
			return int64((serialType - 12) / 2), nil
		}
		return int64((serialType - 13) / 2), nil
	default:
		return 0, fmt.Errorf("storage: unknown serial type: %d", serialType)
	}
}

// Record represents a decoded SQLite record (row).
type Record struct {
	Values []interface{}
}

// DecodeRecord decodes a record from a byte slice.
func DecodeRecord(data []byte) (*Record, error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	if n == 0 {
		return nil, fmt.Errorf("storage: corrupt record header size")
	}
	pos += n
	hdrEnd := int(hdrSize)

	// Decode serial type codes
	var serialTypes []uint64
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		if n == 0 {
			return nil, fmt.Errorf("storage: corrupt record header at offset %d", pos)
		}
		pos += n
		serialTypes = append(serialTypes, st)
	}

	// Decode values
	r := &Record{Values: make([]interface{}, len(serialTypes))}
	for i, st := range serialTypes {
		valLen, err := SerialTypeLength(st)
		if err != nil {
			return nil, err
		}
		if pos+int(valLen) > len(data) {
			return nil, fmt.Errorf("storage: record data too short at value %d: need %d bytes at offset %d, have %d", i, valLen, pos, len(data))
		}
		v := decodeValue(st, data[pos:pos+int(valLen)])
		r.Values[i] = v
		pos += int(valLen)
	}

	return r, nil
}

// ParseRecordHeader parses a SQLite record header and returns the serial type
// codes for each column and the byte offset where the value data begins.
// The value data starts at the returned dataStart offset within the data slice.
// Serial types are allocated on a stack buffer when there are ≤16 columns.
func ParseRecordHeader(data []byte) (serialTypes []uint64, dataStart int, err error) {
	pos := 0

	// Header size (varint)
	hdrSize, n := util.GetVarint(data[pos:])
	pos += n
	hdrEnd := int(hdrSize)

	// Decode serial type codes. Use a stack-allocated array for common
	// column counts (≤16) to avoid heap allocation per row.
	var stackSerialTypes [16]uint64
	if hdrEnd-pos <= len(stackSerialTypes)*9 { // rough upper bound: each varint ≤ 9 bytes
		serialTypes = stackSerialTypes[:0]
	}
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
		pos += n
		serialTypes = append(serialTypes, st)
	}

	return serialTypes, pos, nil
}

// DecodeRecordValuesFromTypes decodes record values into target using pre-parsed
// serial types and data offset. Only columns in colIndices are decoded (nil = all).
// This avoids re-parsing the record header when performing multi-phase decode.
func DecodeRecordValuesFromTypes(data []byte, dataStart int, target []interface{}, serialTypes []uint64, colIndices map[int]bool) int {
	return decodeRecordValuesFromTypes(data, dataStart, target, serialTypes, colIndices)
}

// decodeRecordValuesFromTypes decodes record values into target using pre-parsed
func decodeRecordValuesFromTypes(data []byte, dataStart int, target []interface{}, serialTypes []uint64, colIndices map[int]bool) int {
	pos := dataStart
	count := len(serialTypes)
	if count > len(target) {
		count = len(target)
	}
	decodeAll := colIndices == nil
	for i := 0; i < count; i++ {
		valLen, err := SerialTypeLength(serialTypes[i])
		if err != nil {
			return i
		}
		// bounds check (safety: skip instead of panic for corrupted data)
		if pos+int(valLen) > len(data) {
			// truncated record — stop decoding
			return i
		}
		if decodeAll || colIndices[i] {
			target[i] = decodeValue(serialTypes[i], data[pos:pos+int(valLen)])
		}
		// For skipped columns (not in colIndices), advance past the data but
		// leave target[i] as its zero value (nil).
		pos += int(valLen)
	}
	return count
}

func decodeValue(serialType uint64, data []byte) interface{} {
	switch {
	case serialType == SerialNull:
		return nil
	case serialType == SerialZero, serialType == SerialOne:
		return smallIntValue(serialType)
	case serialType == SerialInt8, serialType == SerialInt16, serialType == SerialInt32, serialType == SerialInt64:
		return decodeBigEndianInt(serialType, data)
	case serialType == SerialInt24:
		v := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
		if v&0x800000 != 0 {
			v |= 0xFF000000 // sign extend
		}
		return int64(int32(v))
	case serialType == SerialInt48:
		v := uint64(data[0])<<40 | uint64(data[1])<<32 | uint64(data[2])<<24 |
			uint64(data[3])<<16 | uint64(data[4])<<8 | uint64(data[5])
		if v&0x800000000000 != 0 {
			v |= 0xFFFF000000000000
		}
		return int64(v)
	case serialType == SerialFloat:
		return float64(math.Float64frombits(binary.BigEndian.Uint64(data)))
	default:
		if serialType%2 == 0 {
			// Blob
			b := make([]byte, len(data))
			copy(b, data)
			return b
		}
		// Text
		return string(data)
	}
}

// smallIntValue returns the int64 value of the SerialZero/SerialOne cases.
func smallIntValue(serialType uint64) interface{} {
	if serialType == SerialOne {
		return int64(1)
	}
	return int64(0)
}

// decodeBigEndianInt decodes a big-endian signed integer of the size implied
// by the given integer serial type.
func decodeBigEndianInt(serialType uint64, data []byte) interface{} {
	switch serialType {
	case SerialInt8:
		return int64(int8(data[0]))
	case SerialInt16:
		return int64(int16(binary.BigEndian.Uint16(data)))
	case SerialInt32:
		return int64(int32(binary.BigEndian.Uint32(data)))
	case SerialInt64:
		return int64(binary.BigEndian.Uint64(data))
	}
	return nil
}

// EncodeRecord encodes a record from a slice of Go values.
func EncodeRecord(values []interface{}) ([]byte, error) {
	// Optimized: avoid per-value byte slice allocations by computing sizes
	// first, then writing directly into a single output buffer.

	// First pass: compute serial types and data sizes
	// Use stack arrays for common column counts
	var stackSerialTypes [16]uint64
	var stackDataLens [16]int
	serialTypes := stackSerialTypes[:0]
	dataLens := stackDataLens[:0]

	for _, v := range values {
		st, dl := encodeValueSize(v)
		serialTypes = append(serialTypes, st)
		dataLens = append(dataLens, dl)
	}

	// Compute serial type varint total length
	var serialTypesLen int
	for _, st := range serialTypes {
		serialTypesLen += util.VarintLen(st)
	}

	// Header size = size of header-size varint + sum of serial-type varints
	hdrSize := serialTypesLen + 1
	for {
		hdrSizeLen := util.VarintLen(uint64(hdrSize))
		newHdrSize := serialTypesLen + hdrSizeLen
		if newHdrSize == hdrSize {
			break
		}
		hdrSize = newHdrSize
	}

	// Total data length
	var totalDataLen int
	for _, dl := range dataLens {
		totalDataLen += dl
	}

	// SQLite test instrumentation (UPDATE_MAX_BLOBSIZE on OP_MakeRecord,
	// src/vdbe.c): the record Mem is nHdr+nData bytes; zeroblobs met while
	// scanning backwards before any real data (i.e. followed only by NULLs
	// and other zeroblobs) stay unexpanded as an nZero tail and are NOT
	// counted. NULLs carry zero data bytes so they pass through.
	instrumented := hdrSize + totalDataLen
	for i := len(values) - 1; i >= 0; i-- {
		switch v := values[i].(type) {
		case value.ZeroBlob:
			instrumented -= v.N
		case nil:
			// zero data bytes; keep scanning
		default:
			goto blobsizeDone
		}
	}
blobsizeDone:
	updateMaxBlobsize(instrumented)

	// Build the record

	buf := make([]byte, hdrSize+totalDataLen)
	pos := 0

	// Header size varint
	pos += util.PutVarint(buf[pos:], uint64(hdrSize))

	// Serial types
	for _, st := range serialTypes {
		pos += util.PutVarint(buf[pos:], st)
	}

	// Values — write directly into the buffer
	for i, v := range values {
		encodeValueInto(v, buf[pos:pos+dataLens[i]])
		pos += dataLens[i]
	}

	return buf, nil
}

// encodeValueSize returns the serial type and data length for a value
// without allocating any byte slices.
func encodeValueSize(v interface{}) (uint64, int) {
	switch val := v.(type) {
	case nil:
		return SerialNull, 0
	case int64:
		return encodeInt64Size(val)
	case float64:
		return SerialFloat, 8
	case string:
		return uint64(13 + len(val)*2), len(val)
	case []byte:
		return uint64(12 + len(val)*2), len(val)
	case value.ZeroBlob:
		// zeroblob(N): blob serial type covers N bytes; the zeros are
		// written into the record payload without materializing a buffer
		// (SQLite MEM_Zero, expanded on demand).
		return uint64(12 + val.N*2), val.N
	default:
		s := fmt.Sprintf("%v", v)
		return uint64(13 + len(s)*2), len(s)
	}
}

// EncodeValueSize returns the serial type and data length for a value
// without allocating any byte slices (exported for size checks outside the
// storage package).
func EncodeValueSize(v interface{}) (uint64, int) {
	return encodeValueSize(v)
}

// encodeInt64Size returns the serial type and data length for an int64
// without allocating.
func encodeInt64Size(val int64) (uint64, int) {
	if val == 0 || val == 1 {
		return smallIntSerial(val)
	}
	return intSizeSerial(val)
}

// smallIntSerial returns the serial type for the int64 values 0 and 1.
func smallIntSerial(val int64) (uint64, int) {
	if val == 1 {
		return SerialOne, 0
	}
	return SerialZero, 0
}

// intSizeSerial returns the serial type for a non-0/1 int64 based on its
// magnitude. Negative magnitudes use ^val (bitwise NOT = -(val+1)) so the
// range boundaries match the signed bounds exactly and MinInt64 falls through
// to the 8-byte SerialInt64.
func intSizeSerial(val int64) (uint64, int) {
	mag := val
	if val < 0 {
		mag = ^val
	}
	switch {
	case mag <= 127:
		return SerialInt8, 1
	case mag <= 32767:
		return SerialInt16, 2
	case mag <= 8388607:
		return SerialInt24, 3
	case mag <= 2147483647:
		return SerialInt32, 4
	case mag <= 140737488355327:
		return SerialInt48, 6
	default:
		return SerialInt64, 8
	}
}

// encodeValueInto writes a value's data directly into the given buffer.
// The buffer must be pre-sized correctly (use encodeValueSize to compute).
func encodeValueInto(v interface{}, buf []byte) {
	switch val := v.(type) {
	case nil:
		// no data
	case int64:
		encodeInt64Into(val, buf)
	case float64:
		binary.BigEndian.PutUint64(buf, math.Float64bits(val))
	case string:
		copy(buf, val)
	case []byte:
		copy(buf, val)
	case value.ZeroBlob:
		// buf is already zero-filled (allocated with make([]byte, n));
		// zeroblob content needs no copy
	default:
		s := fmt.Sprintf("%v", v)
		copy(buf, s)
	}
}

func encodeInt64Into(val int64, buf []byte) {
	switch {
	case val == 0, val == 1:
		// no data
	case val >= -128 && val <= 127:
		buf[0] = byte(int8(val))
	case val >= -32768 && val <= 32767:
		binary.BigEndian.PutUint16(buf, uint16(int16(val)))
	case val >= -8388608 && val <= 8388607:
		v := uint32(int32(val))
		buf[0] = byte(v >> 16)
		buf[1] = byte(v >> 8)
		buf[2] = byte(v)
	case val >= -2147483648 && val <= 2147483647:
		binary.BigEndian.PutUint32(buf, uint32(int32(val)))
	case val >= -140737488355328 && val <= 140737488355327:
		v := uint64(val)
		buf[0] = byte(v >> 40)
		buf[1] = byte(v >> 32)
		buf[2] = byte(v >> 24)
		buf[3] = byte(v >> 16)
		buf[4] = byte(v >> 8)
		buf[5] = byte(v)
	default:
		binary.BigEndian.PutUint64(buf, uint64(val))
	}
}
