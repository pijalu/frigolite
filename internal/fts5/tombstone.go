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

	binary.BigEndian.PutUint32(pg[4:8], nElem+1)
	tombstoneSlotInsert(pg, iSlot, rowid)
	return 0
}

// tombstoneSlotInsert probes a page's slots from iSlot for the first free
// slot and stores rowid there. Linear probing wraps at least once; a full
// rotation stops the probe (C's exhausted-collision outcome: a silent
// no-op).
func tombstoneSlotInsert(pg []byte, iSlot int, rowid int64) {
	szKey := tombstoneKeySize(pg)
	nSlot := tombstoneNSlot(pg)
	nCollide := nSlot
	for {
		off := 8 + iSlot*szKey
		if szKey == 4 {
			if binary.BigEndian.Uint32(pg[off:off+4]) == 0 {
				binary.BigEndian.PutUint32(pg[off:off+4], uint32(rowid))
				return
			}
		} else {
			if binary.BigEndian.Uint64(pg[off:off+8]) == 0 {
				binary.BigEndian.PutUint64(pg[off:off+8], uint64(rowid))
				return
			}
		}
		iSlot = (iSlot + 1) % nSlot
		nCollide--
		if nCollide == 0 {
			return
		}
	}
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
	szKey = tombstoneRebuildKeySize(data1, szKey, pendingRowid)
	slotPerPage := tombstoneSlotPerPage(int(t.cfg.Pgsz), szKey)
	nOut, nSlot := tombstoneRebuildLayout(seg, data1, slotPerPage)

	for {
		pages := newTombstonePageSet(szKey, nSlot, nOut)
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
			return t.writeTombstonePageSet(seg, pages)
		}
		nOut, nSlot = nOut*2+1, slotPerPage
	}
}

// tombstoneRebuildKeySize resolves the key width for a rebuild: an existing
// page's width, widened when the pending rowid needs the 8-byte form, 4 as
// the empty-set default.
func tombstoneRebuildKeySize(data1 []byte, szKey int, pendingRowid int64) int {
	if data1 != nil && szKey == 0 {
		szKey = tombstoneKeySize(data1)
	}
	if rowidNeedsWideKey(pendingRowid) {
		szKey = 8
	}
	if szKey == 0 {
		szKey = 4
	}
	return szKey
}

// tombstoneSlotPerPage is one page's slot capacity for a key width, floored
// at C's 32-slot minimum.
func tombstoneSlotPerPage(pgsz, szKey int) int {
	const minSlot = 32
	n := (pgsz - 8) / szKey
	if n < minSlot {
		n = minSlot
	}
	return n
}

// tombstoneRebuildLayout picks the initial page count and slot count for a
// rebuild (fts5IndexTombstoneRebuild's initial sizing).
func tombstoneRebuildLayout(seg *Segment, data1 []byte, slotPerPage int) (nOut, nSlot int) {
	switch {
	case seg.NPgTombstone == 0:
		return 1, 32
	case seg.NPgTombstone == 1:
		nOut, nSlot = 1, 32
		if data1 != nil {
			if nElem := int(binary.BigEndian.Uint32(data1[4:8])); nElem*4 > nSlot {
				nSlot = nElem * 4
			}
		}
		if nSlot > slotPerPage {
			nOut = 0
		}
		if nOut == 0 {
			return int(seg.NPgTombstone)*2 + 1, slotPerPage
		}
		return nOut, nSlot
	default:
		return int(seg.NPgTombstone)*2 + 1, slotPerPage
	}
}

// newTombstonePageSet allocates nOut empty pages of nSlot slots.
func newTombstonePageSet(szKey, nSlot, nOut int) [][]byte {
	pages := make([][]byte, nOut)
	for i := range pages {
		pages[i] = newTombstonePage(szKey, nSlot)
	}
	return pages
}

// writeTombstonePageSet persists a rebuilt page set as the segment's page
// sequence and records the new page count.
func (t *Table) writeTombstonePageSet(seg *Segment, pages [][]byte) error {
	for i, pg := range pages {
		if err := t.writeTombstonePage(seg, int64(i), pg); err != nil {
			return err
		}
	}
	seg.NPgTombstone = int64(len(pages))
	return nil
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
		pg, err := t.rehashSourcePage(seg, data1, iPg1, ii)
		if err != nil {
			return false, err
		}
		if pg == nil {
			continue
		}
		if !tombstoneRehashPage(pg, pages, nOut) {
			return false, nil
		}
		if ii == 0 && iPg1 != 0 {
			pages[0][1] = pg[1]
		}
	}
	return true, nil
}

// rehashSourcePage loads the page to rehash at position ii: the in-hand
// data1 page when ii matches, a fresh read otherwise.
func (t *Table) rehashSourcePage(seg *Segment, data1 []byte, iPg1, ii int64) ([]byte, error) {
	if ii == iPg1 {
		return data1, nil
	}
	return t.readTombstonePage(seg, ii)
}

// tombstoneRehashPage copies every occupied slot of pg into the new page
// set. ok is false when a destination page overflowed.
func tombstoneRehashPage(pg []byte, pages [][]byte, nOut int) bool {
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
			return false
		}
	}
	return true
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

