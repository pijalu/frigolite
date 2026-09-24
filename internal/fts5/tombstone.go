package fts5

import (
	"encoding/binary"
	"fmt"
)

// Port of fts5_index.c's contentless_delete tombstone hash (lines
// ~7810-8115). Deleted rowids of a segment live in a segmented hash table:
// one or more %_data pages at rowid FTS5_TOMBSTONE_ROWID(segid, ipg), where
// ipg is 0-based. Each page is:
//
//	p[0]       key size in bytes (4, or 8 once a rowid exceeds 32 bits)
//	p[1]       rowid-0 flag (a deleted rowid of 0 cannot hold a slot)
//	p[4..7]    number of occupied slots (big endian u32)
//	p[8..]     nSlot keys of szKey bytes each; 0 means empty
//
// A rowid belongs to page (iRowid mod nPg) and slot ((iRowid / nPg) mod
// nSlot), probing forward on collisions. A page that is at least half full
// refuses new entries (the caller rebuilds the whole table with more pages).

// tombstoneKeySize reads a page's key size (TOMBSTONE_KEYSIZE).
func tombstoneKeySize(pg []byte) int {
	if len(pg) > 0 && pg[0] == 8 {
		return 8
	}
	return 4
}

// tombstoneNSlot returns a page's slot count (TOMBSTONE_NSLOT).
func tombstoneNSlot(pg []byte) int {
	szKey := tombstoneKeySize(pg)
	return (len(pg) - 8) / szKey
}

// newTombstonePage allocates an empty page of nSlot slots.
func newTombstonePage(szKey, nSlot int) []byte {
	pg := make([]byte, 8+nSlot*szKey)
	pg[0] = byte(szKey)
	return pg
}

// tombstonePageAdd adds iRowid to a page (fts5IndexTombstoneAddToPage).
// nPg is the table's total page count (part of the slot hash). Returns 0 on
// success, 1 when the page is over half full (rebuild required), 2 when the
// rowid does not fit the page's key size, and -1 when the table is exhausted.
func tombstonePageAdd(pg []byte, bForce bool, nPg int, rowid int64) int {
	szKey := tombstoneKeySize(pg)
	nSlot := tombstoneNSlot(pg)
	nElem := binary.BigEndian.Uint32(pg[4:8])
	// The occupancy guard precedes the slot computation: a zero-slot page
	// (nElem >= 0 == nSlot/2) always takes the rebuild path, so the modulo
	// below never sees nSlot==0 (C's half-full check at
	// fts5IndexTombstoneAddToPage).
	if szKey == 4 && rowid > 0xFFFFFFFF {
		return 2
	}
	if rowid == 0 {
		pg[1] = 0x01
		return 0
	}
	if !bForce && int(nElem) >= nSlot/2 {
		return 1
	}
	iSlot := int((uint64(rowid) / uint64(nPg)) % uint64(nSlot))
	nCollide := nSlot

	binary.BigEndian.PutUint32(pg[4:8], nElem+1)
	for {
		off := 8 + iSlot*szKey
		if szKey == 4 {
			if binary.BigEndian.Uint32(pg[off:off+4]) == 0 {
				binary.BigEndian.PutUint32(pg[off:off+4], uint32(rowid))
				return 0
			}
		} else {
			if binary.BigEndian.Uint64(pg[off:off+8]) == 0 {
				binary.BigEndian.PutUint64(pg[off:off+8], uint64(rowid))
				return 0
			}
		}
		iSlot = (iSlot + 1) % nSlot
		nCollide--
		if nCollide == 0 {
			return 0 // C's exhausted-collision outcome: a silent no-op
		}
	}
}

// tombstonePageHas reports whether rowid is present in the page.
func tombstonePageHas(pg []byte, rowid int64) bool {
	szKey := tombstoneKeySize(pg)
	nSlot := tombstoneNSlot(pg)
	for i := 0; i < nSlot; i++ {
		off := 8 + i*szKey
		if szKey == 4 {
			if uint64(binary.BigEndian.Uint32(pg[off:off+4])) == uint64(rowid) {
				return true
			}
		} else {
			if binary.BigEndian.Uint64(pg[off:off+8]) == uint64(rowid) {
				return true
			}
		}
	}
	return false
}

// tombstoneRowid renders a tombstone page's %_data rowid
// (FTS5_TOMBSTONE_ROWID(segid, ipg): the segid is offset by 2^16 so
// tombstone and leaf rowid spaces never collide).
func tombstoneRowid(segid, ipg int64) int64 {
	return driRowid(segid+1<<16, 0, 0, ipg)
}

// segmentRowid renders a segment blob page's %_data rowid
// (FTS5_SEGMENT_ROWID(segid, pgno)).
func segmentRowid(segid, pgno int64) int64 {
	return driRowid(segid, 0, 0, pgno)
}

// driRowid packs (segid, dlidx, height, pgno) into a %_data rowid
// (fts5_dri: segid << 37 | dlidx << 36 | height << 31 | pgno).
func driRowid(segid int64, dlidx, height int, pgno int64) int64 {
	return segid<<37 | int64(dlidx)<<36 | int64(height)<<31 | pgno
}

// tombstoneAdd records one deleted rowid in segment seg's tombstone hash,
// rebuilding/growing the page set exactly as C does
// (fts5IndexTombstoneAdd + fts5IndexTombstoneRebuild). Existing pages are
// rewritten; new pages are written; the segment's nPgTombstone is updated and
// the structure record re-persisted by the caller.
func (t *Table) tombstoneAdd(seg *Segment, rowid int64) error {
	t.nContentlessDelete++
	if seg.NPgTombstone > 0 {
		iPg := rowid % seg.NPgTombstone
		pg, err := t.readTombstonePage(seg, iPg)
		if err != nil {
			return err
		}
		switch tombstonePageAdd(pg, false, int(seg.NPgTombstone), rowid) {
		case 0:
			return t.writeTombstonePage(seg, iPg, pg)
		case 2:
			// key-size growth falls through to the rebuild with szKey=8
		default:
			return t.rebuildTombstones(seg, pg, iPg, 0, rowid)
		}
		return t.rebuildTombstones(seg, pg, iPg, 0, rowid)
	}
	return t.rebuildTombstones(seg, nil, -1, 4, rowid)
}

// rebuildTombstones rebuilds a segment's tombstone hash around one pending
// add (fts5IndexTombstoneRebuild). pendingKey/pendingSz describe the rowid
// that triggered the rebuild (already-read page may be nil).
func (t *Table) rebuildTombstones(seg *Segment, data1 []byte, iPg1 int64, szKey int, pendingRowid int64) error {
	if data1 != nil && szKey == 0 {
		szKey = tombstoneKeySize(data1)
	}
	if rowidNeedsWideKey(pendingRowid) {
		szKey = 8
	}
	if szKey == 0 {
		szKey = 4
	}
	const minSlot = 32
	slotPerPage := (int(t.cfg.Pgsz) - 8) / szKey
	if slotPerPage < minSlot {
		slotPerPage = minSlot
	}
	var nOut, nSlot int
	switch {
	case seg.NPgTombstone == 0:
		nOut, nSlot = 1, minSlot
	case seg.NPgTombstone == 1:
		var nElem uint32
		if data1 != nil {
			nElem = binary.BigEndian.Uint32(data1[4:8])
		}
		nOut, nSlot = 1, minSlot
		if int(nElem)*4 > nSlot {
			nSlot = int(nElem) * 4
		}
		if nSlot > slotPerPage {
			nOut = 0
		}
	default:
		nOut, nSlot = int(seg.NPgTombstone)*2+1, slotPerPage
	}
	if nOut == 0 {
		nOut, nSlot = int(seg.NPgTombstone)*2+1, slotPerPage
	}

	for {
		pages := make([][]byte, nOut)
		for i := range pages {
			pages[i] = newTombstonePage(szKey, nSlot)
		}
		ok, err := t.rehashTombstones(seg, data1, iPg1, pages, nOut)
		if err != nil {
			return err
		}
		if ok {
			if res := tombstonePageAdd(pages[pendingRowid%int64(nOut)], true, nOut, pendingRowid); res != 0 {
				// The forced add can still exhaust a page: retry larger.
				nOut, nSlot = nOut*2+1, slotPerPage
				continue
			}
			for i, pg := range pages {
				if err := t.writeTombstonePage(seg, int64(i), pg); err != nil {
					return err
				}
			}
			seg.NPgTombstone = int64(nOut)
			return nil
		}
		nOut, nSlot = nOut*2+1, slotPerPage
	}
}

// rowidNeedsWideKey reports whether a rowid needs the 8-byte key form.
func rowidNeedsWideKey(rowid int64) bool { return rowid > 0xFFFFFFFF }

// rehashTombstones copies the existing tombstone keys into the new page set
// (fts5IndexTombstoneRehash). ok is false when a page overflowed.
func (t *Table) rehashTombstones(seg *Segment, data1 []byte, iPg1 int64, pages [][]byte, nOut int) (bool, error) {
	for i := range pages {
		pages[i][0] = byte(tombstoneKeySize(pages[i]))
		binary.BigEndian.PutUint32(pages[i][4:8], 0)
	}
	for ii := int64(0); ii < seg.NPgTombstone; ii++ {
		var pg []byte
		if ii == iPg1 {
			pg = data1
		} else {
			var err error
			pg, err = t.readTombstonePage(seg, ii)
			if err != nil {
				return false, err
			}
		}
		if pg == nil {
			continue
		}
		nSlotIn := tombstoneNSlot(pg)
		for i := 0; i < nSlotIn; i++ {
			off := 8 + i*tombstoneKeySize(pg)
			var val uint64
			if tombstoneKeySize(pg) == 4 {
				val = uint64(binary.BigEndian.Uint32(pg[off : off+4]))
			} else {
				val = binary.BigEndian.Uint64(pg[off : off+8])
			}
			if val == 0 {
				continue
			}
			dst := pages[val%uint64(nOut)]
			if res := tombstonePageAdd(dst, false, nOut, int64(val)); res != 0 {
				return false, nil
			}
		}
		if ii == 0 && iPg1 != 0 {
			pages[0][1] = pg[1]
		}
	}
	return true, nil
}

// readTombstonePage loads one tombstone page of a segment from %_data.
func (t *Table) readTombstonePage(seg *Segment, ipg int64) ([]byte, error) {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	rows, err := t.db.ExecSQL(fmt.Sprintf("SELECT block FROM %s WHERE id=%d", qData, tombstoneRowid(seg.Segid, ipg)))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0][0] == nil {
		return nil, nil
	}
	raw, _ := toBytes(rows[0][0])
	return raw, nil
}

// writeTombstonePage persists one tombstone page of a segment.
func (t *Table) writeTombstonePage(seg *Segment, ipg int64, pg []byte) error {
	qData := qual(t.dbName, t.cfg.Name+"_data")
	_, err := t.db.ExecSQL(fmt.Sprintf("INSERT OR REPLACE INTO %s(id, block) VALUES(%d, X'%s');",
		qData, tombstoneRowid(seg.Segid, ipg), hexEncode(pg)))
	return err
}

// removeTombstoneRows deletes every tombstone page row of a segment from
// %_data (fts5DataRemoveSegment's tombstone half).
func (t *Table) removeTombstoneRows(seg *Segment) error {
	if seg.NPgTombstone == 0 {
		return nil
	}
	qData := qual(t.dbName, t.cfg.Name+"_data")
	lo := tombstoneRowid(seg.Segid, 0)
	hi := tombstoneRowid(seg.Segid, seg.NPgTombstone-1)
	_, err := t.db.ExecSQL(fmt.Sprintf("DELETE FROM %s WHERE id>=%d AND id<=%d", qData, lo, hi))
	return err
}

// tombstoneContains reports whether rowid is tombstoned in seg, reading the
// page-set membership the way C's segment iterators do
// (fts5IndexTombstoneHas: page = rowid % nPgTombstone, then probe).
func (t *Table) tombstoneContains(seg *Segment, rowid int64) bool {
	if seg.Tombs != nil {
		return seg.Tombs[rowid]
	}
	if seg.NPgTombstone == 0 {
		return false
	}
	pg, err := t.readTombstonePage(seg, rowid%seg.NPgTombstone)
	if err != nil || pg == nil {
		return false
	}
	return tombstonePageHas(pg, rowid)
}
