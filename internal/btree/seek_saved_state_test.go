package btree

// Review probe (fleet/review-perf-audit): the Seek* entry points
// (SeekToRowID / SeekToKey / SeekIndexKey) do not reset a cursor's
// cursorRequireSeek state or its savedKey, so a cursor whose position was
// saved by a cross-statement write (saveAllCursors → saveCursorPosition) and
// is THEN re-seeked ends up holding a stale saved key. The next
// ReadCellData/Next runs restoreIfNeeded and jumps back to the OLD key.
//
// Engine-level SQL probes (probe_review_cursor_test.go) could not observe
// this: the exec layers open fresh cursors per evaluation. This probe pins
// the btree-level contract directly so the gap is either confirmed latent
// (report P3) or fixed by a state reset.

import (
	"bytes"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/storage"
)

func probeBtree(t *testing.T) *BTree {
	t.Helper()
	pg := pager.OpenInMemory(1024)
	pg.AllocatePage()
	rootPg, _ := pg.ReadPage(1)
	rootPg.Data[pager.HeaderSize] = storage.PageTypeLeafTable
	setEmptyLeafContent(rootPg)
	pg.WritePage(rootPg)
	return NewBTree(pg, 1, true)
}

func probeInsert(t *testing.T, tr *BTree, id int64, payload byte) {
	t.Helper()
	cell := &storage.Cell{Type: storage.CellTableLeaf, RowID: id, Payload: bytes.Repeat([]byte{payload}, 8)}
	if err := tr.InsertCell(cell); err != nil {
		t.Fatalf("insert %d: %v", id, err)
	}
}

// Re-seek after save: writer saves the cursor (cross-wrapper misc8 save),
// then the OWNER seeks to a NEW key and reads — the read must observe the
// NEW key's row, not the saved one.
func TestProbeSeekResetsSavedState(t *testing.T) {
	tr := probeBtree(t)
	probeInsert(t, tr, 1, 0x01)
	probeInsert(t, tr, 3, 0x03)
	probeInsert(t, tr, 5, 0x05)

	owner, err := tr.OpenCursor()
	if err != nil {
		t.Fatal(err)
	}
	if found, err := owner.SeekToRowID(1); err != nil || !found {
		t.Fatalf("seek 1: found=%v err=%v", found, err)
	}

	// A nested write through a SECOND wrapper on the same tree: saveAllCursors
	// must save the owner cursor's position (state → cursorRequireSeek).
	tr2 := NewBTree(tr.pager, 1, true)
	probeInsert(t, tr2, 7, 0x07)

	if owner.state != cursorRequireSeek {
		t.Fatalf("expected cursorRequireSeek after cross-wrapper write, got %v", owner.state)
	}

	// Owner re-seeks to a NEW key (the nested-loop pattern), then reads.
	if found, err := owner.SeekToRowID(3); err != nil || !found {
		t.Fatalf("re-seek 3: found=%v err=%v", found, err)
	}
	data, rowID, err := owner.ReadCellData()
	if err != nil {
		t.Fatal(err)
	}
	if rowID != 3 {
		t.Fatalf("ReadCellData after re-seek: rowID=%d, want 3 (stale saved-key restore)", rowID)
	}
	if data[0] != 0x03 {
		t.Fatalf("ReadCellData after re-seek: payload %#x, want 0x03", data[0])
	}
}
