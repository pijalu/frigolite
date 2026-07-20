// Package storage implements the SQLite file format primitives:
// database header, page types, cell formats, and record encoding.
package storage

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/pijalu/frigolite/internal/util"
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
	MaxEmbeddedFraction = 64
	MinEmbeddedFraction = 32
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
		VersionValidFor:  binary.BigEndian.Uint32(data[76:80]),
		SQLiteVersionNum: binary.BigEndian.Uint32(data[80:84]),
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
	binary.BigEndian.PutUint32(buf[76:80], h.VersionValidFor)
	binary.BigEndian.PutUint32(buf[80:84], h.SQLiteVersionNum)
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

	// Validate
	if p.PageType != PageTypeInteriorIndex &&
		p.PageType != PageTypeInteriorTable &&
		p.PageType != PageTypeLeafIndex &&
		p.PageType != PageTypeLeafTable {
		return nil, fmt.Errorf("storage: unknown page type: 0x%02x", p.PageType)
	}

	return p, nil
}

// CellPointer reads a cell pointer at index i from the cell pointer array.
// The cell pointer array starts at (contentOffset + 8) in pageData.
func CellPointer(pageData []byte, contentOffset int, i int) uint16 {
	offset := contentOffset + 8 + i*2
	return binary.BigEndian.Uint16(pageData[offset : offset+2])
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
	LeftPtr uint32           // for interior cells
	RowID   int64            // for table cells
	Payload []byte           // raw payload
}

// DecodeCell decodes a b-tree cell from the given page data at the given offset.
func DecodeCell(pageData []byte, offset int, cellType CellType, pageSize int) (*Cell, error) {
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

	// Payload
	payloadLen := int(plen)
	if pos+payloadLen > len(data) {
		payloadLen = len(data) - pos
	}
	c.Payload = make([]byte, payloadLen)
	copy(c.Payload, data[pos:pos+payloadLen])

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

	payloadLen := int(plen)
	if pos+payloadLen > len(data) {
		payloadLen = len(data) - pos
	}
	c.Payload = make([]byte, payloadLen)
	copy(c.Payload, data[pos:pos+payloadLen])

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
	c.Payload = make([]byte, payloadLen)
	copy(c.Payload, data[pos:pos+payloadLen])
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
	plen := len(c.Payload)
	plenLen := util.VarintLen(uint64(plen))
	rowidLen := util.VarintLen(uint64(c.RowID))
	totalLen := plenLen + rowidLen + plen
	buf := make([]byte, totalLen)
	pos := 0
	pos += util.PutVarint(buf[pos:], uint64(plen))
	pos += util.PutVarint(buf[pos:], uint64(c.RowID))
	copy(buf[pos:], c.Payload)
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
	plen := len(c.Payload)
	plenLen := util.VarintLen(uint64(plen))
	buf := make([]byte, plenLen+plen)
	pos := 0
	pos += util.PutVarint(buf[pos:], uint64(plen))
	copy(buf[pos:], c.Payload)
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
	SerialNull   = 0
	SerialInt8   = 1
	SerialInt16  = 2
	SerialInt24  = 3
	SerialInt32  = 4
	SerialInt48  = 5
	SerialInt64  = 6
	SerialFloat  = 7
	SerialZero   = 8
	SerialOne    = 9
	SerialMin    = 12 // first usable string/blob serial type
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
	pos += n
	hdrEnd := int(hdrSize)

	// Decode serial type codes
	var serialTypes []uint64
	for pos < hdrEnd {
		st, n := util.GetVarint(data[pos:])
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

func decodeValue(serialType uint64, data []byte) interface{} {
	switch {
	case serialType == SerialNull:
		return nil
	case serialType == SerialZero:
		return int64(0)
	case serialType == SerialOne:
		return int64(1)
	case serialType == SerialInt8:
		return int64(int8(data[0]))
	case serialType == SerialInt16:
		return int64(int16(binary.BigEndian.Uint16(data)))
	case serialType == SerialInt24:
		v := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
		if v&0x800000 != 0 {
			v |= 0xFF000000 // sign extend
		}
		return int64(int32(v))
	case serialType == SerialInt32:
		return int64(int32(binary.BigEndian.Uint32(data)))
	case serialType == SerialInt48:
		v := uint64(data[0])<<40 | uint64(data[1])<<32 | uint64(data[2])<<24 |
			uint64(data[3])<<16 | uint64(data[4])<<8 | uint64(data[5])
		if v&0x800000000000 != 0 {
			v |= 0xFFFF000000000000
		}
		return int64(v)
	case serialType == SerialInt64:
		return int64(binary.BigEndian.Uint64(data))
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

// EncodeRecord encodes a record from a slice of Go values.
func EncodeRecord(values []interface{}) ([]byte, error) {
	// First pass: compute serial types and sizes
	type entry struct {
		serialType uint64
		data       []byte
	}
	entries := make([]entry, len(values))

	for i, v := range values {
		e := &entries[i]
		e.serialType, e.data = encodeValue(v)
	}

	// Compute serial type varint total length
	var serialTypesLen int
	for _, e := range entries {
		serialTypesLen += util.VarintLen(e.serialType)
	}

	// Header size = size of header-size varint + sum of serial-type varints
	// We need to compute this iteratively since header-size includes itself
	hdrSize := serialTypesLen + 1 // initial estimate (1 byte for header-size varint)
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
	for _, e := range entries {
		totalDataLen += len(e.data)
	}

	// Build the record
	buf := make([]byte, hdrSize+totalDataLen)
	pos := 0

	// Header size varint
	pos += util.PutVarint(buf[pos:], uint64(hdrSize))

	// Serial types
	for _, e := range entries {
		pos += util.PutVarint(buf[pos:], e.serialType)
	}

	// Values
	for _, e := range entries {
		copy(buf[pos:], e.data)
		pos += len(e.data)
	}

	return buf, nil
}

func encodeValue(v interface{}) (uint64, []byte) {
	switch val := v.(type) {
	case nil:
		return SerialNull, nil
	case int64:
		return encodeInt64(val)
	case float64:
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, math.Float64bits(val))
		return SerialFloat, buf
	case string:
		st := uint64(13 + len(val)*2)
		return st, []byte(val)
	case []byte:
		st := uint64(12 + len(val)*2)
		b := make([]byte, len(val))
		copy(b, val)
		return st, b
	default:
		s := fmt.Sprintf("%v", v)
		st := uint64(13 + len(s)*2)
		return st, []byte(s)
	}
}

func encodeInt64(val int64) (uint64, []byte) {
	switch {
	case val == 0:
		return SerialZero, nil
	case val == 1:
		return SerialOne, nil
	case val >= -128 && val <= 127:
		return SerialInt8, []byte{byte(int8(val))}
	case val >= -32768 && val <= 32767:
		buf := make([]byte, 2)
		binary.BigEndian.PutUint16(buf, uint16(int16(val)))
		return SerialInt16, buf
	case val >= -8388608 && val <= 8388607:
		buf := make([]byte, 3)
		v := uint32(int32(val))
		buf[0] = byte(v >> 16)
		buf[1] = byte(v >> 8)
		buf[2] = byte(v)
		return SerialInt24, buf
	case val >= -2147483648 && val <= 2147483647:
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(int32(val)))
		return SerialInt32, buf
	case val >= -140737488355328 && val <= 140737488355327:
		buf := make([]byte, 6)
		v := uint64(val)
		buf[0] = byte(v >> 40)
		buf[1] = byte(v >> 32)
		buf[2] = byte(v >> 24)
		buf[3] = byte(v >> 16)
		buf[4] = byte(v >> 8)
		buf[5] = byte(v)
		return SerialInt48, buf
	default:
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(val))
		return SerialInt64, buf
	}
}
