package fts5

import (
	"bytes"
	"encoding/hex"
	"fmt"
)

// The index structure record (%_data id=10) and its per-segment metadata,
// ported from fts5_index.c fts5StructureDecode/fts5StructureWrite. A structure
// record is:
//
//	config-cookie (4 bytes, big endian) [FTS5_STRUCTURE_V2 marker (4 bytes)]
//	  varint nLevel, varint nSegment (total), varint nWriteCounter
//	  per level:  varint nMerge, varint nSeg
//	  per segment: varint segid, pgnoFirst, pgnoLast
//	    [V2 only: varint origin1, origin2, nPgTombstone, nEntryTombstone, nEntry]
//
// The V2 form is used by contentless_delete=1 tables and carries the per-
// segment origin ranges and tombstone counters. nOriginCntr is NOT stored: it
// is recovered as (largest segment origin2)+1 on decode, exactly like C.
//
// The mirror-model segment additionally tracks (in memory only) the rowids of
// the documents flushed into it, so merges and optimize can rebuild a
// segment's persisted blob without a C-format leaf reader.

// structureV2Marker is C's FTS5_STRUCTURE_V2 ("\xFF\x00\x00\x01") prefix that
// distinguishes contentless_delete structure records.
var structureV2Marker = []byte{0xFF, 0x00, 0x00, 0x01}

// Segment is one index segment (Fts5StructureSegment).
type Segment struct {
	Segid           int64
	PgnoFirst       int64
	PgnoLast        int64
	Origin1         uint64 // V2 only
	Origin2         uint64 // V2 only
	NPgTombstone    int64  // V2 only
	NEntryTombstone int64  // V2 only
	NEntry          int64  // V2 only

	// Mirror-model state (not part of C's record): the rowids of the
	// documents persisted in this segment's blob, and their tombstoned
	// subset. Tombstones mirror C's per-segment tombstone hash pages.
	Rowids []int64
	Tombs  map[int64]bool
}

// size returns the segment's leaf-page footprint (fts5SegmentSize).
func (s *Segment) size() int64 {
	if s.PgnoLast < s.PgnoFirst {
		return 1
	}
	return 1 + s.PgnoLast - s.PgnoFirst
}

// StructRec is the decoded index structure (Fts5Structure).
type StructRec struct {
	V2            bool // contentless_delete=1 structure record
	NWriteCounter int64
	NOriginCntr   uint64 // V2 only: origin for the next level-0 segment
	Levels        [][]*Segment
}

// newStructRec builds the initial (empty) structure for a table.
func newStructRec(v2 bool) *StructRec {
	return &StructRec{V2: v2}
}

// totalSegments counts the segments across all levels (pStruct->nSegment).
func (sr *StructRec) totalSegments() int {
	n := 0
	for _, lvl := range sr.Levels {
		n += len(lvl)
	}
	return n
}

// allSegments lists every segment oldest-level-first.
func (sr *StructRec) allSegments() []*Segment {
	var out []*Segment
	for _, lvl := range sr.Levels {
		out = append(out, lvl...)
	}
	return out
}

// encode serializes the record (fts5StructureWrite).
func (sr *StructRec) encode() []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 0}) // config cookie (C: iCookie<0 stores 0)
	if sr.V2 {
		buf.Write(structureV2Marker)
	}
	buf.Write(putVarint(nil, uint64(len(sr.Levels))))
	buf.Write(putVarint(nil, uint64(sr.totalSegments())))
	buf.Write(putVarint(nil, uint64(sr.NWriteCounter)))
	for _, lvl := range sr.Levels {
		nMerge := 0
		buf.Write(putVarint(nil, uint64(nMerge)))
		buf.Write(putVarint(nil, uint64(len(lvl))))
		for _, seg := range lvl {
			buf.Write(putVarint(nil, uint64(seg.Segid)))
			buf.Write(putVarint(nil, uint64(seg.PgnoFirst)))
			buf.Write(putVarint(nil, uint64(seg.PgnoLast)))
			if sr.V2 {
				buf.Write(putVarint(nil, seg.Origin1))
				buf.Write(putVarint(nil, seg.Origin2))
				buf.Write(putVarint(nil, uint64(seg.NPgTombstone)))
				buf.Write(putVarint(nil, uint64(seg.NEntryTombstone)))
				buf.Write(putVarint(nil, uint64(seg.NEntry)))
			}
		}
	}
	return buf.Bytes()
}

// decodeStructRec parses a structure record (fts5StructureDecode). It fails
// with an error on the malformed shapes C rejects (bad varints, level/segment
// count mismatch, pgnoLast<pgnoFirst).
func decodeStructRec(data []byte) (*StructRec, error) {
	// Minimum record: legacy 4-byte cookie + three 1-byte varints.
	if len(data) < 7 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	sr := &StructRec{}
	pos := 4 // cookie
	if len(data) >= 8 && bytes.Equal(data[pos:pos+4], structureV2Marker) {
		sr.V2 = true
		pos += 4
	}
	nLevel, n := getVarint(data[pos:])
	if n <= 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	pos += n
	nSegment, n := getVarint(data[pos:])
	if n <= 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	pos += n
	wc, n := getVarint(data[pos:])
	if n <= 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	sr.NWriteCounter = int64(wc)
	pos += n

	if nLevel > 64 || nSegment > 65535 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	remaining := nSegment
	maxOrigin := uint64(0)
	for lvl := 0; lvl < int(nLevel); lvl++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		nMerge, n := getVarint(data[pos:])
		if n <= 0 {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		nSeg, n := getVarint(data[pos:])
		if n <= 0 {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		pos += n
		if nSeg > remaining {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		remaining -= nSeg
		segs := make([]*Segment, 0, nSeg)
		for i := 0; i < int(nSeg); i++ {
			seg := &Segment{Tombs: map[int64]bool{}}
			var v uint64
			if v, n = getVarint(data[pos:]); n <= 0 {
				return nil, fmt.Errorf("database disk image is malformed")
			}
			seg.Segid = int64(v)
			pos += n
			if v, n = getVarint(data[pos:]); n <= 0 {
				return nil, fmt.Errorf("database disk image is malformed")
			}
			seg.PgnoFirst = int64(v)
			pos += n
			if v, n = getVarint(data[pos:]); n <= 0 {
				return nil, fmt.Errorf("database disk image is malformed")
			}
			seg.PgnoLast = int64(v)
			pos += n
			if sr.V2 {
				if seg.Origin1, n = getVarint(data[pos:]); n <= 0 {
					return nil, fmt.Errorf("database disk image is malformed")
				}
				pos += n
				if seg.Origin2, n = getVarint(data[pos:]); n <= 0 {
					return nil, fmt.Errorf("database disk image is malformed")
				}
				pos += n
				if v, n = getVarint(data[pos:]); n <= 0 {
					return nil, fmt.Errorf("database disk image is malformed")
				}
				seg.NPgTombstone = int64(v)
				pos += n
				if v, n = getVarint(data[pos:]); n <= 0 {
					return nil, fmt.Errorf("database disk image is malformed")
				}
				seg.NEntryTombstone = int64(v)
				pos += n
				if v, n = getVarint(data[pos:]); n <= 0 {
					return nil, fmt.Errorf("database disk image is malformed")
				}
				seg.NEntry = int64(v)
				pos += n
				if seg.Origin2 > maxOrigin {
					maxOrigin = seg.Origin2
				}
			}
			if seg.PgnoLast < seg.PgnoFirst {
				return nil, fmt.Errorf("database disk image is malformed")
			}
			segs = append(segs, seg)
		}
		if lvl > 0 && nMerge > 0 && nSeg == 0 {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		if lvl == int(nLevel)-1 && nMerge > 0 {
			return nil, fmt.Errorf("database disk image is malformed")
		}
		sr.Levels = append(sr.Levels, segs)
	}
	if remaining != 0 {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	if sr.V2 {
		sr.NOriginCntr = maxOrigin + 1
	}
	return sr, nil
}

// structureWrite persists the structure record (fts5StructureWrite's
// fts5DataWrite of %_data id=10).
func (t *Table) structureWrite() error {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	hexed := hexEncode(t.structRec.encode())
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id=10; INSERT INTO %s(id, block) VALUES(10, X'%s');",
		qData, qData, hexed))
	return err
}

// structureRead loads the structure record from %_data (fts5StructureRead).
// A missing or unreadable record yields the initial empty structure — the
// caller decides whether that is an error for its context.
func (t *Table) structureRead() (*StructRec, error) {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT block FROM %s WHERE id=10", qData))
	if err != nil || len(rows) == 0 || rows[0][0] == nil {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	raw, ok := toBytes(rows[0][0])
	if !ok {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	return decodeStructRec(raw)
}

// allocateSegid takes the next free segment id (fts5AllocateSegid: ids come
// from a bitmap; the mirror model uses a monotonic counter seeded past every
// existing id, C's fts5MaxSegment fallback when the bitmap is exhausted).
func (t *Table) allocateSegid() int64 {
	id := t.nextSegid
	if id < 1 {
		id = 1
	}
	for {
		used := false
		for _, seg := range t.structRec.allSegments() {
			if seg.Segid == id {
				used = true
				break
			}
		}
		if !used {
			break
		}
		id++
	}
	t.nextSegid = id + 1
	return id
}

// getVarint decodes one SQLite-format varint (fts5GetVarint64: 7-bit groups,
// most significant first, high bit of each byte but the last is the
// continuation flag; the 9th byte contributes a full 8 bits). It returns the
// value and the number of bytes consumed (n<=0 on truncation).
func getVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, -1
		}
		c := b[i]
		v = v<<7 | uint64(c&0x7F)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return 0, -1
	}
	return v<<8 | uint64(b[8]), 9
}

// hexEncode renders bytes as a SQL blob hex literal body.
func hexEncode(b []byte) string { return hex.EncodeToString(b) }
