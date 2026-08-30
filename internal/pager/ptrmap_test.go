package pager

// P8.INCRVACUUM phase 2: pager-level ReadPtrmap/WritePtrmap roundtrip
// through the cache + on-disk file. Uses OpenInMemory for isolation.

import (
	"testing"

	"github.com/pijalu/frigolite/internal/storage"
)

func TestPtrmapReadWrite(t *testing.T) {
	pageSize := uint32(1024)
	pg := OpenInMemory(pageSize)
	// Allocate enough pages to span multiple ptrmap chunks.
	// nPer=205; allocating pages 2..30 covers one ptrmap page (pgno=2).
	_ = pg.AllocatePage() // pgno 1
	for i := uint32(0); i < 30; i++ {
		_ = pg.AllocatePage() // pgnos 2..31
	}
	// Write entries for several pages and read them back.
	tests := []struct {
		pgno       uint32
		parentType byte
		parentPgno uint32
	}{
		{3, storage.PtrmapBtreeNode, 2},
		{5, storage.PtrmapHasRowid, 2},
		{20, storage.PtrmapOverflow, 7},
		{50, storage.PtrmapBtreeNode, 2}, // also in first chunk
	}
	for _, tc := range tests {
		if err := pg.WritePtrmap(tc.pgno, tc.parentType, tc.parentPgno); err != nil {
			t.Fatalf("WritePtrmap(%d): %v", tc.pgno, err)
		}
	}
	// Read them back through the in-memory cache.
	for _, tc := range tests {
		gotType, gotPgno, err := pg.ReadPtrmap(tc.pgno)
		if err != nil {
			t.Fatalf("ReadPtrmap(%d): %v", tc.pgno, err)
		}
		if gotType != tc.parentType {
			t.Errorf("ReadPtrmap(%d) type = 0x%02x, want 0x%02x", tc.pgno, gotType, tc.parentType)
		}
		if gotPgno != tc.parentPgno {
			t.Errorf("ReadPtrmap(%d) parentPgno = %d, want %d", tc.pgno, gotPgno, tc.parentPgno)
		}
	}
	// Read after a flush: persist and re-open.
	if err := pg.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// We can't easily re-open in-memory, so just check the cache
	// didn't lose data.
	for _, tc := range tests {
		gotType, _, err := pg.ReadPtrmap(tc.pgno)
		if err != nil {
			t.Fatalf("ReadPtrmap(%d) post-flush: %v", tc.pgno, err)
		}
		if gotType != tc.parentType {
			t.Errorf("ReadPtrmap(%d) post-flush type = 0x%02x, want 0x%02x", tc.pgno, gotType, tc.parentType)
		}
	}
}

func TestPtrmapInvalidPgno(t *testing.T) {
	pg := OpenInMemory(1024)
	_ = pg.AllocatePage()
	if _, _, err := pg.ReadPtrmap(0); err == nil {
		t.Errorf("ReadPtrmap(0) should error")
	}
	if _, _, err := pg.ReadPtrmap(1); err == nil {
		t.Errorf("ReadPtrmap(1) should error")
	}
	if err := pg.WritePtrmap(0, 0, 0); err == nil {
		t.Errorf("WritePtrmap(0) should error")
	}
	// pgno=2 is a ptrmap page (pageSize=1024 has chunk size 205, so
	// first ptrmap page is 2). ReadPtrmap(2) should error.
	if _, _, err := pg.ReadPtrmap(2); err == nil {
		t.Errorf("ReadPtrmap(2) on a ptrmap page should error")
	}
}
