package storage

// P8.INCRVACUUM phase 2: pointer-map (ptrmap) page read/write.
// Reference: btree.c ptrmapPageno (line ~4130), ptrmapGet (~4275),
// ptrmapPut (~4310).

import (
	"testing"
)

func TestPtrmapPageNo(t *testing.T) {
	// For 1024-byte pages: nPer = 1024/5 + 1 = 205. Each chunk is 205
	// pages (slot 0 = ptrmap page, slots 1..204 = data pages).
	// Pending-byte page for 1024-byte pageSize = 1073741824/1024+1 = 1048577+1 = 1048578.
	//
	// Cases:
	//   pgno=2: chunk=0, posInChunk=0 → ptrmap page for chunk 0 (pgno 2). Yes.
	//   pgno=3: chunk=0, posInChunk=1 → ptrmap page still 2. Yes.
	//   pgno=207: chunk=0, posInChunk=205 → would be the next chunk's ptrmap page
	//     (pgno 207 = 2 + 205). Yes.
	//   pgno=1048578: pending-byte page; PtrmapPageNo should skip it.
	//   pgno=2 with pageSize=4096: nPer=819+1=820. ptrmap page is 2.
	//     3..821 data, 822 is the next chunk's ptrmap page.
	tests := []struct {
		pgno     uint32
		pageSize uint32
		want     uint32
	}{
		{2, 1024, 2},     // first ptrmap page
		{3, 1024, 2},     // data page covered by first ptrmap
		{100, 1024, 2},   // data page covered by first ptrmap
		{206, 1024, 2},   // last data page covered by first ptrmap (slot 204)
		{207, 1024, 207}, // next chunk's ptrmap page
		{208, 1024, 207}, // data page covered by second ptrmap
		{2, 4096, 2},
		{3, 4096, 2},
		{820, 4096, 2},   // data page
		{821, 4096, 2},   // data page (slot 819)
		{822, 4096, 822}, // next ptrmap page
		{823, 4096, 822},
		{0, 1024, 0}, // invalid
		{1, 1024, 0}, // invalid
	}
	for _, tc := range tests {
		got := PtrmapPageNo(tc.pgno, tc.pageSize)
		if got != tc.want {
			t.Errorf("PtrmapPageNo(%d, %d) = %d, want %d", tc.pgno, tc.pageSize, got, tc.want)
		}
	}
}

func TestIsPtrmapPageNo(t *testing.T) {
	tests := []struct {
		pgno     uint32
		pageSize uint32
		want     bool
	}{
		{2, 1024, true},   // first ptrmap page
		{3, 1024, false},  // data page
		{206, 1024, false},
		{207, 1024, true},  // second chunk's ptrmap page
		{412, 1024, true},  // third chunk's ptrmap page
		{0, 1024, false},
		{1, 1024, false},
	}
	for _, tc := range tests {
		got := IsPtrmapPageNo(tc.pgno, tc.pageSize)
		if got != tc.want {
			t.Errorf("IsPtrmapPageNo(%d, %d) = %v, want %v", tc.pgno, tc.pageSize, got, tc.want)
		}
	}
}

func TestPtrmapEntryRoundtrip(t *testing.T) {
	// Build a 1024-byte page (representing the ptrmap page for chunk 0,
	// which covers data pages 3..206). Initialize all entries to 0.
	pageSize := uint32(1024)
	pageData := make([]byte, pageSize)
	// Test writing then reading for several data pages.
	tests := []struct {
		pgno       uint32
		parentType byte
		parentPgno uint32
	}{
		{3, PtrmapBtree, 2},          // page 3 is a btree node, parent page 2
		{10, PtrmapBtree, 2},         // page 10 is an interior table btree node
		{100, PtrmapOverflow1, 50},    // page 100 is an overflow page
		{206, PtrmapFreelist, 0},     // page 206 is on the freelist (no parent)
		{208, PtrmapBtree, 207},      // page 208 is in chunk 2, parent is chunk 2's ptrmap (207)
	}
	for _, tc := range tests {
		// Find the containing ptrmap page.
		ptrmapPg := PtrmapPageNo(tc.pgno, pageSize)
		if ptrmapPg == tc.pgno {
			t.Errorf("pgno %d is a ptrmap page", tc.pgno)
			continue
		}
		// For phase 2, we use the same pageData buffer to represent
		// whichever ptrmap page covers tc.pgno. The offset within
		// that ptrmap page is computed by ptrmapEntryOffset.
		// (For now we don't switch pageData per chunk — the offset
		// formula is local to the chunk, so entries within chunk 0
		// all live in the same buffer.)
		off, err := WritePtrmapEntry(pageData, tc.pgno, pageSize, tc.parentType, tc.parentPgno)
		if err != nil {
			t.Fatalf("WritePtrmapEntry(pgno=%d): %v", tc.pgno, err)
		}
		if off == 0xFFFFFFFF {
			t.Fatalf("WritePtrmapEntry(pgno=%d) returned sentinel offset", tc.pgno)
		}
		gotType, gotPgno, err := PtrmapEntry(pageData, tc.pgno, pageSize)
		if err != nil {
			t.Fatalf("PtrmapEntry(pgno=%d): %v", tc.pgno, err)
		}
		if gotType != tc.parentType {
			t.Errorf("PtrmapEntry(pgno=%d) type = 0x%02x, want 0x%02x", tc.pgno, gotType, tc.parentType)
		}
		if gotPgno != tc.parentPgno {
			t.Errorf("PtrmapEntry(pgno=%d) parentPgno = %d, want %d", tc.pgno, gotPgno, tc.parentPgno)
		}
	}
}

func TestPtrmapEntryZeroIsUninitialized(t *testing.T) {
	// A page with all-zero entries should return (0, 0, nil) for
	// uninitialized entries (SQLite semantics: type 0 = "not yet
	// assigned"; callers handle this).
	pageSize := uint32(1024)
	pageData := make([]byte, pageSize) // all zeros
	gotType, gotPgno, err := PtrmapEntry(pageData, 5, pageSize)
	if err != nil {
		t.Fatalf("PtrmapEntry on zeroed page: %v", err)
	}
	if gotType != 0 || gotPgno != 0 {
		t.Errorf("PtrmapEntry on zeroed page = (%d, %d), want (0, 0)", gotType, gotPgno)
	}
}

func TestPtrmapEntryInvalidInputs(t *testing.T) {
	pageSize := uint32(1024)
	pageData := make([]byte, pageSize)
	// pgno < 2
	if _, _, err := PtrmapEntry(pageData, 0, pageSize); err == nil {
		t.Errorf("PtrmapEntry(pgno=0) should error")
	}
	if _, _, err := PtrmapEntry(pageData, 1, pageSize); err == nil {
		t.Errorf("PtrmapEntry(pgno=1) should error")
	}
	// pgno is a ptrmap page
	if _, _, err := PtrmapEntry(pageData, 2, pageSize); err == nil {
		t.Errorf("PtrmapEntry on ptrmap page should error")
	}
}

func TestPtrmapEntryOffsetOutOfRange(t *testing.T) {
	pageSize := uint32(1024)
	// 5 bytes per entry; for page 100, the entry is at offset ~ (100-3) * 5 = 485.
	// Use a buffer that's too small to hold it.
	pageData := make([]byte, 10)
	if _, _, err := PtrmapEntry(pageData, 100, pageSize); err == nil {
		t.Errorf("PtrmapEntry on too-small page should error")
	}
	// And a buffer that holds page 3's entry but not page 100's.
	pageData2 := make([]byte, 50)
	if _, _, err := PtrmapEntry(pageData2, 3, pageSize); err != nil {
		t.Errorf("PtrmapEntry on page 3 with 50-byte buffer should succeed: %v", err)
	}
	if _, _, err := PtrmapEntry(pageData2, 100, pageSize); err == nil {
		t.Errorf("PtrmapEntry on page 100 with 50-byte buffer should error")
	}
}
