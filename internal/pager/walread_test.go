package pager

// walread_test.go — unit tests for the slice-3 read-snapshot primitives
// (plan/goals/P7.WAL-G7.md): walFindFrame's minFrame rule on the shared
// hash tables and the read-mark selection over aReadMark[] (wal.c
// walTryBeginRead's selection loop).

import (
	"path/filepath"
	"testing"
)

// TestWalIndexFindMinFrameRule pins walIndexFind's minFrame rule on real
// hash-table entries: frames at or below minFrame are excluded (their page
// versions are checkpointed; a stale hash entry must never be served from
// the WAL), the newest surviving frame wins, and mxFrame caps the search.
func TestWalIndexFindMinFrameRule(t *testing.T) {
	wi, err := acquireWALIndex(filepath.Join(t.TempDir(), "minframe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer wi.release()
	// Page 7 written at frames 2 and 5; page 9 at frame 3.
	err = wi.WriterSection(func() error {
		for _, e := range []struct{ f, pg uint32 }{{2, 7}, {3, 9}, {5, 7}} {
			if aerr := wi.appendLocked(e.f, e.pg, 0); aerr != nil {
				return aerr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pg, mx, min, want uint32
		name              string
	}{
		{7, 2, 0, 2, "mxFrame caps at 2"},
		{7, 5, 0, 5, "newest frame wins without cutoff"},
		{7, 5, 3, 5, "stale frame below minFrame skipped"},
		{7, 5, 4, 5, "newest frame above cutoff"},
		{7, 5, 5, 5, "cutoff at the frame itself"},
		{7, 5, 6, 0, "cutoff above every entry"},
		{9, 5, 0, 3, "page 9 at frame 3"},
		{9, 5, 3, 3, "page 9 at its own frame"},
		{9, 5, 4, 0, "page 9's only entry is checkpointed"},
		{9, 2, 0, 0, "page 9 above mxFrame cap"},
	} {
		if got := wi.FindFrame(tc.pg, tc.mx, tc.min); got != tc.want {
			t.Errorf("%s: FindFrame(%d, mx=%d, min=%d) = %d, want %d",
				tc.name, tc.pg, tc.mx, tc.min, got, tc.want)
		}
	}
}

// TestWalSelectReadMarkRule pins walTryBeginRead's aReadMark[] selection:
// the largest mark not exceeding mxFrame wins, READMARK_NOT_USED entries
// never qualify, and a full field of over-mxFrame marks selects nothing.
func TestWalSelectReadMarkRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marks   [5]uint32
		mxFrame uint32
		wantI   int
		wantM   uint32
	}{
		{
			name:    "empty marks select nothing",
			marks:   [5]uint32{0, ReadmarkNotUsed, ReadmarkNotUsed, ReadmarkNotUsed, ReadmarkNotUsed},
			mxFrame: 9,
			wantI:   0, wantM: 0,
		},
		{
			name:    "largest mark at mxFrame wins",
			marks:   [5]uint32{0, ReadmarkNotUsed, 4, 7, ReadmarkNotUsed},
			mxFrame: 7,
			wantI:   3, wantM: 7,
		},
		{
			name:    "largest below mxFrame still wins",
			marks:   [5]uint32{0, 3, ReadmarkNotUsed, 5, 2},
			mxFrame: 9,
			wantI:   3, wantM: 5,
		},
		{
			name:    "marks above mxFrame never qualify",
			marks:   [5]uint32{0, 3, 5, 8, 9},
			mxFrame: 7,
			wantI:   2, wantM: 5,
		},
		{
			name:    "reset mark zero qualifies at mxFrame zero",
			marks:   [5]uint32{0, 0, ReadmarkNotUsed, ReadmarkNotUsed, ReadmarkNotUsed},
			mxFrame: 0,
			wantI:   1, wantM: 0,
		},
	} {
		info := WalCkptInfo{AReadMark: tc.marks}
		gotI, gotM := walSelectReadMarkHeld(&info, tc.mxFrame)
		if gotI != tc.wantI || gotM != tc.wantM {
			t.Errorf("%s: = (%d,%d), want (%d,%d)", tc.name, gotI, gotM, tc.wantI, tc.wantM)
		}
	}
}
