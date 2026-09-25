// Package storage implements the SQLite file format primitives:
// database header, page types, cell formats, and record encoding.
// Cell encoding lives in cell.go; record encoding in record.go.
package storage

import (
	"encoding/binary"
	"fmt"
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
		ApplicationID:    binary.BigEndian.Uint32(data[68:72]),
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
	binary.BigEndian.PutUint32(buf[68:72], h.ApplicationID)
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
	if p.PageType == 0 {
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
