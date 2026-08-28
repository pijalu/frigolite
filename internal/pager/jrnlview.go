package pager

// jrnlview.go — UCL (portplan/UNIT_CONFORMANCE.md §U3) structural decoder
// for the rollback journal (-journal) byte layout, ported from SQLite
// src/pager.c (writeJournalHdr, L1423+, and the aJournalMagic definition
// at L757).
//
// Journal header format (pager.c comment at writeJournalHdr):
//
//	- 8 bytes:  magic aJournalMagic (d9 d5 05 f9 20 a1 63 d7)
//	- 4 bytes:  number of records in journal, or -1 (0xffffffff)
//	- 4 bytes:  random number used for page checksum init (cksumInit)
//	- 4 bytes:  initial database page count
//	- 4 bytes:  sector size
//	- 4 bytes:  database page size
//
// followed by (JOURNAL_HDR_SZ - 28) bytes of unused space, where
// JOURNAL_HDR_SZ(pPager) == pPager->sectorSize (pager.c L771).
//
// Each page record is JOURNAL_PG_SZ == pageSize + 8 bytes (pager.c L764):
//
//	- 4 bytes: page number
//	- pageSize bytes: page content
//	- 4 bytes: checksum (journalChecksum, pager.c: sum of u32 words
//	  interpreted per SQLITE_BIGENDIAN plus cksumInit, page[0:pageSize-200])

import (
	"encoding/binary"
	"fmt"
)

// journalMagic is aJournalMagic (pager.c L757-759).
var journalMagic = []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// JournalHeader is the decoded rollback-journal header.
type JournalHeader struct {
	RecordCount int64  // -1 means 0xffffffff (no-sync growth marker)
	CksumInit   uint32 // random per-journal value added to page checksums
	DBPageCount uint32 // initial database size in pages
	SectorSize  uint32
	PageSize    uint32
	// MagicValid reports whether the 8-byte magic matches aJournalMagic.
	// writeJournalHdr (pager.c L1484-1490) zeroes magic+record-count until
	// the header is synced, so a live mid-transaction journal legitimately
	// shows zeroed magic with the remaining header fields populated.
	MagicValid bool
}

// JournalPage is one decoded page record.
type JournalPage struct {
	Number     int    // 1-based record index
	PageNumber uint32
	Checksum   uint32 // stored checksum field
	Data       []byte // page-size slice into the file content
}

// DecodeJournalHeader parses the rollback journal header (pager.c
// writeJournalHdr layout). buf must contain at least the first 28 bytes.
func DecodeJournalHeader(buf []byte) (JournalHeader, error) {
	var h JournalHeader
	if len(buf) < 28 {
		return h, fmt.Errorf("jrnlview: short journal header: %d bytes", len(buf))
	}
	h.MagicValid = true
	for i, b := range journalMagic {
		if buf[i] != b {
			h.MagicValid = false
			break
		}
	}
	if !h.MagicValid {
		// Only an all-zero magic+nRec pair is acceptable (pre-sync header,
		// pager.c L1488 memset branch).
		for i := 0; i < 12; i++ {
			if buf[i] != 0 {
				return h, fmt.Errorf("jrnlview: bad magic at byte %d: %#x", i, buf[i])
			}
		}
	}
	nRec := binary.BigEndian.Uint32(buf[8:])
	if nRec == 0xffffffff {
		h.RecordCount = -1
	} else {
		h.RecordCount = int64(nRec)
	}
	h.CksumInit = binary.BigEndian.Uint32(buf[12:])
	h.DBPageCount = binary.BigEndian.Uint32(buf[16:])
	h.SectorSize = binary.BigEndian.Uint32(buf[20:])
	h.PageSize = binary.BigEndian.Uint32(buf[24:])
	return h, nil
}

// DecodeJournalPages decodes every complete page record of a journal whose
// first header sits at offset 0. Records start at the first
// JOURNAL_HDR_SZ-aligned offset (journalHdrOffset, pager.c L1353) and each
// spans JOURNAL_PG_SZ bytes. Decoding stops at the first incomplete record
// or at a subsequent journal header (multi-header journals are flagged by
// the caller via len(pages) vs header.RecordCount).
func DecodeJournalPages(buf []byte, h JournalHeader) ([]JournalPage, error) {
	if h.PageSize == 0 || h.SectorSize == 0 {
		return nil, fmt.Errorf("jrnlview: zero page/sector size")
	}
	hdr := int64(h.SectorSize)
	recSize := int64(h.PageSize) + 8
	nRec := int((int64(len(buf)) - hdr) / recSize)
	if nRec < 0 {
		return nil, fmt.Errorf("jrnlview: file shorter than journal header")
	}
	pages := make([]JournalPage, 0, nRec)
	for i := 1; i <= nRec; i++ {
		off := hdr + int64(i-1)*recSize
		rec := buf[off : off+recSize]
		pages = append(pages, JournalPage{
			Number:     i,
			PageNumber: binary.BigEndian.Uint32(rec[0:]),
			Data:       rec[4 : 4+int64(h.PageSize)],
			Checksum:   binary.BigEndian.Uint32(rec[4+int64(h.PageSize):]),
		})
	}
	return pages, nil
}

// JournalChecksum ports pager_cksum (pager.c L2238-2246): the page-record
// checksum is cksumInit plus the bytes aData[pageSize-200],
// aData[pageSize-400], ... sampled every 200 bytes downward. The checksum
// covers (sparsely) both ends of the record to catch torn garbage after a
// power failure (pager.c L747-755 comment).
func JournalChecksum(cksumInit uint32, page []byte) uint32 {
	cksum := cksumInit
	for i := len(page) - 200; i > 0; i -= 200 {
		cksum += uint32(page[i])
	}
	return cksum
}
