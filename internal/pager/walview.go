package pager

// walview.go — UCL (portplan/UNIT_CONFORMANCE.md §U3) structural decoder for
// the write-ahead log (-wal) byte layout, ported from SQLite src/wal.c.
//
// Layout reference (src/wal.c header comment, L30-96):
//
//	The WAL header is 32 bytes, eight big-endian u32 values:
//	  0: Magic 0x377f0682 (LE checksums) or 0x377f0683 (BE checksums)
//	  4: File format version (3007000)
//	  8: Database page size
//	 12: Checkpoint sequence number
//	 16: Salt-1 (incremented each checkpoint)
//	 20: Salt-2 (randomized each checkpoint)
//	 24: Checksum-1 over first 24 header bytes
//	 28: Checksum-2
//
//	Each frame: 24-byte frame-header then <page-size> bytes of page data.
//	Frame-header, six big-endian u32 values:
//	  0: Page number
//	  4: Commit record: db size in pages after commit; else 0
//	  8: Salt-1 (copy of header)
//	 12: Salt-2 (copy of header)
//	 16: Checksum-1 (cumulative)
//	 20: Checksum-2
//
// A frame is valid iff its salts match the WAL header AND its checksum
// equals the running checksum over (header[0:8-of-frame] + page data) of
// all frames up to and including it (wal.c validity conditions (1)/(2)).

import (
	"encoding/binary"
	"fmt"
)

const (
	// WalHdrSize is WAL_HDRSIZE (wal.c L480): size of the WAL header.
	WalHdrSize = 32
	// WalFrameHdrSize is WAL_FRAME_HDRSIZE (wal.c L477).
	WalFrameHdrSize = 24
	// WalMagic is WAL_MAGIC (wal.c L491); LSB set means big-endian checksums.
	WalMagic = 0x377f0682
	// WalMaxVersion is WAL_MAX_VERSION (wal.c L277).
	WalMaxVersion = 3007000
)

// WalHeader is the decoded 32-byte WAL header (wal.c L34-44).
type WalHeader struct {
	Magic          uint32 // 0x377f0682 or 0x377f0683
	Version        uint32 // 3007000
	PageSize       uint32
	CheckpointSeq  uint32
	Salt1          uint32
	Salt2          uint32
	Checksum1      uint32
	Checksum2      uint32
	BigEndCksum    bool // magic LSB set: checksums computed big-endian
	HeaderCksumOK  bool // header checksum over bytes [0:24] matches
}

// WalFrame is one decoded frame (frame-header + page data reference).
type WalFrame struct {
	Number       int    // 1-based frame index
	PageNumber   uint32
	CommitDBSize uint32 // non-zero marks a commit record
	Salt1        uint32
	Salt2        uint32
	Checksum1    uint32
	Checksum2    uint32
	Valid        bool // salts match header and cumulative checksum matches
	PageData     []byte // page-size slice into the file content
}

// WalChecksumBytes ports walChecksumBytes (wal.c L856): fibonacci-weighted
// checksum over data (a multiple of 8 bytes), chained from (s1, s2).
// bigEnd selects 32-bit big-endian word interpretation (magic 0x377f0683);
// little-endian otherwise (magic 0x377f0682).
func WalChecksumBytes(bigEnd bool, data []byte, s1, s2 uint32) (uint32, uint32) {
	if len(data)%8 != 0 {
		panic("walview: checksum input must be a multiple of 8 bytes")
	}
	for i := 0; i < len(data); i += 8 {
		var x0, x1 uint32
		if bigEnd {
			x0 = binary.BigEndian.Uint32(data[i:])
			x1 = binary.BigEndian.Uint32(data[i+4:])
		} else {
			x0 = binary.LittleEndian.Uint32(data[i:])
			x1 = binary.LittleEndian.Uint32(data[i+4:])
		}
		s1 += x0 + s2
		s2 += x1 + s1
	}
	return s1, s2
}

// walFrameOffset ports walFrameOffset (wal.c L498): byte offset of frame
// iFrame (1-based) given page size szPage.
func walFrameOffset(iFrame int, szPage uint32) int64 {
	return WalHdrSize + int64(iFrame-1)*int64(szPage+WalFrameHdrSize)
}

// DecodeWalHeader parses and validates the 32-byte WAL header. The header
// checksum is computed over the first 24 bytes with zero seed using the
// byte order selected by the magic LSB (wal.c L949: walChecksumBytes over
// the first WAL_HDRSIZE-8 bytes).
func DecodeWalHeader(buf []byte) (WalHeader, error) {
	var h WalHeader
	if len(buf) < WalHdrSize {
		return h, fmt.Errorf("walview: short WAL header: %d bytes", len(buf))
	}
	h.Magic = binary.BigEndian.Uint32(buf[0:])
	h.Version = binary.BigEndian.Uint32(buf[4:])
	h.PageSize = binary.BigEndian.Uint32(buf[8:])
	h.CheckpointSeq = binary.BigEndian.Uint32(buf[12:])
	h.Salt1 = binary.BigEndian.Uint32(buf[16:])
	h.Salt2 = binary.BigEndian.Uint32(buf[20:])
	h.Checksum1 = binary.BigEndian.Uint32(buf[24:])
	h.Checksum2 = binary.BigEndian.Uint32(buf[28:])
	if h.Magic != WalMagic && h.Magic != WalMagic|1 {
		return h, fmt.Errorf("walview: bad magic %#x", h.Magic)
	}
	h.BigEndCksum = h.Magic&1 != 0
	s1, s2 := WalChecksumBytes(h.BigEndCksum, buf[:WalFrameHdrSize], 0, 0)
	h.HeaderCksumOK = s1 == h.Checksum1 && s2 == h.Checksum2
	return h, nil
}

// DecodeWalFrames decodes every complete frame after the header, validating
// salts and the cumulative checksum chain (wal.c validity rules (1)/(2)).
// Decoding stops at the first incomplete trailing frame (torn write).
func DecodeWalFrames(buf []byte, h WalHeader) ([]WalFrame, error) {
	if h.PageSize == 0 {
		return nil, fmt.Errorf("walview: zero page size")
	}
	frameSize := int64(h.PageSize) + WalFrameHdrSize
	nFrames := int((int64(len(buf)) - WalHdrSize) / frameSize)
	if nFrames < 0 {
		return nil, fmt.Errorf("walview: file shorter than WAL header")
	}
	frames := make([]WalFrame, 0, nFrames)
	// Cumulative checksum seeded with the header checksum (wal.c L985-987:
	// aCksum initialized from the WAL header checksum, then extended with
	// aFrame[0:8] and the page data of each frame).
	s1, s2 := h.Checksum1, h.Checksum2
	for i := 1; i <= nFrames; i++ {
		off := walFrameOffset(i, h.PageSize)
		fh := buf[off : off+WalFrameHdrSize]
		data := buf[off+WalFrameHdrSize : off+frameSize]
		f := WalFrame{
			Number:       i,
			PageNumber:   binary.BigEndian.Uint32(fh[0:]),
			CommitDBSize: binary.BigEndian.Uint32(fh[4:]),
			Salt1:        binary.BigEndian.Uint32(fh[8:]),
			Salt2:        binary.BigEndian.Uint32(fh[12:]),
			Checksum1:    binary.BigEndian.Uint32(fh[16:]),
			Checksum2:    binary.BigEndian.Uint32(fh[20:]),
			PageData:     data,
		}
		s1, s2 = WalChecksumBytes(h.BigEndCksum, fh[:8], s1, s2)
		s1, s2 = WalChecksumBytes(h.BigEndCksum, data, s1, s2)
		f.Valid = f.Salt1 == h.Salt1 && f.Salt2 == h.Salt2 &&
			s1 == f.Checksum1 && s2 == f.Checksum2
		frames = append(frames, f)
	}
	return frames, nil
}

// LastCommitFrame returns the index of the last frame marked as a commit
// record (CommitDBSize != 0), or 0 when no commit frame exists — mirroring
// wal.c mxFrame recovery semantics (frames past the last commit are ignored).
func LastCommitFrame(frames []WalFrame) int {
	last := 0
	for _, f := range frames {
		if f.Valid && f.CommitDBSize != 0 {
			last = f.Number
		}
	}
	return last
}
