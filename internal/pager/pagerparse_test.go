package pager

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

// buildBTreeLeafPage writes a minimal table-leaf b-tree page image into a
// fresh page-sized buffer: page type 0x0d, cellCount cells whose pointer
// array sits right after the 8-byte header and whose content grows down from
// the end. Each cell is a 4-byte table-leaf cell (payload-len varint 0,
// rowid varint, payload "ab") padded to 4 bytes.
func buildBTreeLeafPage(pageSize int, cellCount int, pageType byte) []byte {
	data := make([]byte, pageSize)
	data[0] = pageType
	binary.BigEndian.PutUint16(data[5:7], uint16(pageSize)) // cell content start
	pos := pageSize
	for i := 0; i < cellCount; i++ {
		// 4-byte minimum cell: varint payload-len (0), varint rowid, 2 payload bytes.
		pos -= 4
		data[pos] = 2                 // payload length
		data[pos+1] = byte(i + 1)     // rowid varint
		copy(data[pos+2:pos+4], "ab") // payload
		binary.BigEndian.PutUint16(data[8+i*2:10+i*2], uint16(pos))
	}
	binary.BigEndian.PutUint16(data[3:5], uint16(cellCount))
	binary.BigEndian.PutUint16(data[5:7], uint16(pos))
	return data
}

// TestParsedBTreeMemoHitSameImage verifies the memo serves the identical
// parse for repeated touches of an unchanged page (the point-lookup shape:
// a fresh cursor per statement re-parses the same root/leaf images).
func TestParsedBTreeMemoHitSameImage(t *testing.T) {
	pg := &Page{Data: buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable), PageNum: 7}
	first, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.CellCount != 3 || first.PageType != storage.PageTypeLeafTable {
		t.Fatalf("unexpected first parse: %+v", first)
	}
	second, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if second != first {
		t.Fatalf("memo miss on identical image: %p != %p", second, first)
	}
}

// TestParsedBTreeInvalidatedOnWrite is THE invalidation contract: after the
// page's bytes change (any in-place write — btree mutates Data before
// WritePage), the next parse must reflect the new content. A stale memo here
// would read corrupted cell geometry.
func TestParsedBTreeInvalidatedOnWrite(t *testing.T) {
	pg := &Page{Data: buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable), PageNum: 7}
	before, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}

	// Mutate the header cell count and a cell pointer, exactly like an
	// in-place insert does (writeLeafCell shifts the pointer array and
	// rewrites the header bytes before WritePage).
	binary.BigEndian.PutUint16(pg.Data[3:5], 4) // CellCount 3 -> 4
	binary.BigEndian.PutUint16(pg.Data[8+3*2:10+3*2], 512)

	after, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if after == before {
		t.Fatalf("stale memo returned after page write")
	}
	if after.CellCount != 4 {
		t.Fatalf("stale CellCount %d after write, want 4", after.CellCount)
	}
}

// TestParsedBTreeInvalidatedOnCellPointerRewrite covers the defrag shape:
// cell content moves, pointer array rewritten, cell count unchanged. Only a
// fingerprint covering the POINTER ARRAY (not just the header fields)
// detects this.
func TestParsedBTreeInvalidatedOnCellPointerRewrite(t *testing.T) {
	pg := &Page{Data: buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable), PageNum: 7}
	if _, err := pg.ParsedBTree(1024, 0); err != nil {
		t.Fatalf("first parse: %v", err)
	}
	// Move cell 0 without touching any header field.
	binary.BigEndian.PutUint16(pg.Data[8:10], 1000)
	if _, err := pg.ParsedBTree(1024, 0); err != nil {
		t.Fatalf("second parse: %v", err)
	}
	// The parse result cannot see the move (cell pointers are read per
	// access), but the memo MUST have been refreshed against the new bytes —
	// a memo that ignored pointer-array rewrites would serve a snapshot that
	// no longer matches the page.
	m := pg.parseMemo.Load()
	if m == nil {
		t.Fatalf("no memo stored")
	}
	if !bytes.Equal(m.hdrSnap, pg.Data[m.coff:m.coff+len(m.hdrSnap)]) {
		t.Fatalf("memo snapshot does not match the page after pointer rewrite")
	}
}

// TestParsedBTreePage1ContentOffset keeps page 1's 100-byte content offset
// distinct in the memo key: the same Page parsed at two offsets must not
// cross-serve.
func TestParsedBTreePage1ContentOffset(t *testing.T) {
	raw := make([]byte, 1024)
	copy(raw[HeaderSize:], buildBTreeLeafPage(924, 2, storage.PageTypeLeafTable))
	pg := &Page{Data: raw, PageNum: 1}
	at100, err := pg.ParsedBTree(1024, HeaderSize)
	if err != nil {
		t.Fatalf("parse at 100: %v", err)
	}
	if at100.CellCount != 2 {
		t.Fatalf("CellCount %d, want 2", at100.CellCount)
	}
	// Corrupt the b-tree header region (behind the DB header) — the memo
	// keyed at offset 100 must notice.
	pg.Data[HeaderSize] = 0x77
	if _, err := pg.ParsedBTree(1024, HeaderSize); err == nil {
		t.Fatalf("corrupt page type accepted after memo fill")
	}
}

// TestParsedBTreeCorruptPageNeverMemoized: an unparsable page errors on
// every access (corruption detection must not be masked by a cached error OR
// a cached parse), and once the image is repaired the parse succeeds — the
// same page object can go from failing to parsing without an explicit
// invalidation.
func TestParsedBTreeCorruptPageNeverMemoized(t *testing.T) {
	pg := &Page{Data: buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable), PageNum: 7}
	pg.Data[0] = 0x99 // unknown page type
	if _, err := pg.ParsedBTree(1024, 0); err == nil {
		t.Fatalf("corrupt page type accepted")
	}
	if pg.parseMemo.Load() != nil {
		t.Fatalf("failed parse was memoized")
	}
	if _, err := pg.ParsedBTree(1024, 0); err == nil {
		t.Fatalf("corrupt page type accepted on second access")
	}
	copy(pg.Data, buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable))
	got, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("repaired page still failing: %v", err)
	}
	if got.CellCount != 3 {
		t.Fatalf("repaired parse CellCount %d, want 3", got.CellCount)
	}
}

// TestParsedBTreeInteriorKeepsRightmostPtr pins the fingerprint span: an
// interior page's right-most pointer is part of the parse result, so a write
// that changes ONLY that pointer must invalidate the memo.
func TestParsedBTreeInteriorKeepsRightmostPtr(t *testing.T) {
	raw := buildBTreeLeafPage(1024, 2, storage.PageTypeInteriorTable)
	binary.BigEndian.PutUint32(raw[8:12], 50)
	pg := &Page{Data: raw, PageNum: 4}
	first, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.RightmostPtr != 50 {
		t.Fatalf("RightmostPtr %d, want 50", first.RightmostPtr)
	}
	binary.BigEndian.PutUint32(pg.Data[8:12], 60)
	second, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if second == first || second.RightmostPtr != 60 {
		t.Fatalf("stale right-most pointer after write: %d", second.RightmostPtr)
	}
}

// TestParsedBTreeCanaryRejectsMutatedParse is the memofix regression: a
// consumer that MUTATES the shared *BTreePage (the memo tranche's forbidden
// but hard-to-enforce contract — the index-decode slice-bounds shape) must
// not poison the memo. The byte fingerprint cannot see Go-side struct writes,
// so the canary re-derives the fields from the snapshot on every access: the
// first access after the mutation must self-heal by re-parsing from the
// (unchanged) page bytes instead of serving the mutant's stale CellCount to
// the next cell-pointer walk.
func TestParsedBTreeCanaryRejectsMutatedParse(t *testing.T) {
	pg := &Page{Data: buildBTreeLeafPage(1024, 3, storage.PageTypeLeafTable), PageNum: 7}
	first, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.CellCount != 3 {
		t.Fatalf("CellCount %d, want 3", first.CellCount)
	}

	// Simulate the latent mutator: a consumer writing through the shared
	// parse — inflating CellCount the way a stale decrement/adjustment
	// would (an inflated count walks the cell pointer array past its end).
	first.CellCount = 5000
	first.PageType = storage.PageTypeInteriorIndex

	second, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if second == first {
		t.Fatalf("canary served the mutated memo generation")
	}
	if second.CellCount != 3 || second.PageType != storage.PageTypeLeafTable {
		t.Fatalf("next parse sees mutant state: CellCount %d type 0x%02x, want 3/leaf",
			second.CellCount, second.PageType)
	}

	// The self-heal is durable: the repaired generation serves again.
	third, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("third parse: %v", err)
	}
	if third != second || third.CellCount != 3 {
		t.Fatalf("repaired memo not stable: %+v", third)
	}
}

// TestParsedBTreeInteriorCanaryCoversRightmostPtr pins the canary on the
// interior-page fields too: a mutated RightmostPtr must be rejected the same
// way (a stale pointer sends the descent to a foreign page).
func TestParsedBTreeInteriorCanaryCoversRightmostPtr(t *testing.T) {
	raw := buildBTreeLeafPage(1024, 2, storage.PageTypeInteriorTable)
	binary.BigEndian.PutUint32(raw[8:12], 50)
	pg := &Page{Data: raw, PageNum: 4}
	first, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.RightmostPtr != 50 {
		t.Fatalf("RightmostPtr %d, want 50", first.RightmostPtr)
	}
	first.RightmostPtr = 999 // simulated consumer mutation
	second, err := pg.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if second == first || second.RightmostPtr != 50 {
		t.Fatalf("canary served mutated RightmostPtr: %d", second.RightmostPtr)
	}
}

// TestParsedBTreeInvalidatedThroughPager walks the real pager path: fill the
// memo via a read, write the page through WritePage (in-place byte mutation,
// then journal/dirty bookkeeping), and assert the next read parses the fresh
// image — the engine's "SELECT sees the INSERT" contract at pager level.
func TestParsedBTreeInvalidatedThroughPager(t *testing.T) {
	p := OpenInMemory(1024)
	defer p.Close()
	p.AllocatePage() // page 1: keeps the database header
	pg := p.AllocatePage()
	copy(pg.Data, buildBTreeLeafPage(1024, 1, storage.PageTypeLeafTable))
	if err := p.WritePage(pg); err != nil {
		t.Fatalf("WritePage: %v", err)
	}
	back, err := p.ReadPage(pg.PageNum)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	first, err := back.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if first.CellCount != 1 {
		t.Fatalf("CellCount %d, want 1", first.CellCount)
	}
	// Second insert mutates the page bytes in place, then WritePage.
	binary.BigEndian.PutUint16(back.Data[3:5], 2)
	pos := 1024 - 4 - 4
	back.Data[pos] = 2
	back.Data[pos+1] = 2
	copy(back.Data[pos+2:pos+4], "cd")
	binary.BigEndian.PutUint16(back.Data[8+1*2:10+1*2], uint16(pos))
	if err := p.WritePage(back); err != nil {
		t.Fatalf("WritePage 2: %v", err)
	}
	again, err := p.ReadPage(pg.PageNum)
	if err != nil {
		t.Fatalf("ReadPage 2: %v", err)
	}
	if again != back {
		t.Fatalf("pager handed back a different handle for a cached page")
	}
	second, err := again.ParsedBTree(1024, 0)
	if err != nil {
		t.Fatalf("parse 2: %v", err)
	}
	if second.CellCount != 2 {
		t.Fatalf("STALE parse after WritePage: CellCount %d, want 2", second.CellCount)
	}
}
