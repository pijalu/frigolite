package pager

// walview_test.go — UCL conformance tests (portplan/UNIT_CONFORMANCE.md):
// expectations come ONLY from the oracle CLI (/usr/bin/sqlite3) fixtures in
// testdata/walconformance and from src/wal.c / src/pager.c layout rules
// (U1). The -wal/-journal fixtures are oracle-generated mid-session live
// images (tools/orafixture walFiles/journalFiles).

import (
	"os"
	"path/filepath"
	"testing"
)

const walFixturesDir = "../../testdata/walconformance"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(walFixturesDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if len(buf) == 0 {
		t.Fatalf("fixture %s is empty", name)
	}
	return buf
}

// decodeWal is the first-divergence helper (U4): on failure it names the
// exact frame number that breaks the salt/checksum validity chain.
func decodeWal(t *testing.T, name string) (WalHeader, []WalFrame) {
	t.Helper()
	buf := readFixture(t, name)
	h, err := DecodeWalHeader(buf)
	if err != nil {
		t.Fatalf("%s: header: %v", name, err)
	}
	if h.Magic != WalMagic|1 && h.Magic != WalMagic {
		t.Fatalf("%s: magic %#x, want 0x377f0682/3 (wal.c L491)", name, h.Magic)
	}
	if h.Version != WalMaxVersion {
		t.Fatalf("%s: version %d, want %d (wal.c L277)", name, h.Version, WalMaxVersion)
	}
	if !h.HeaderCksumOK {
		t.Fatalf("%s: header checksum mismatch (computed over bytes [0:24], wal.c L949)", name)
	}
	frames, err := DecodeWalFrames(buf, h)
	if err != nil {
		t.Fatalf("%s: frames: %v", name, err)
	}
	return h, frames
}

func TestWALHeaderConformance(t *testing.T) {
	// Page size is set by the scenario pragma; expectations verified here
	// come from the committed scenario JSON (U1: oracle-executed pragmas).
	cases := []struct {
		name     string
		pageSize uint32
	}{
		{"wal-single-commit", 4096},
		{"wal-multi-commit", 4096},
		{"wal-after-checkpoint", 4096},
	}
	for _, c := range cases {
		buf := readFixture(t, c.name+".db-wal")
		h, err := DecodeWalHeader(buf)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// Oracle note: the CLI reports PRAGMA page_size=4096 after the WAL
		// is created (the db existed at default size before the pragma
		// applied); the WAL header carries the effective page size.
		if h.PageSize != c.pageSize {
			t.Errorf("%s: pageSize %d, want %d", c.name, h.PageSize, c.pageSize)
		}
		if !h.HeaderCksumOK {
			t.Errorf("%s: header checksum invalid", c.name)
		}
	}
}

func TestWALFrameChainConformance(t *testing.T) {
	// Frame counts follow deterministically from the scenario SQL: each
	// committed write txn appends frames for the pages it dirties
	// (wal.c walWriteOneFrame/walAppendFrame). The expectations below were
	// confirmed against the oracle-generated fixtures at fixture-commit
	// time and are structural (pgno sequence, commit marks), not byte-level
	// (salts are randomized per wal.c).
	cases := []struct {
		name        string
		nFrames     int   // complete frames in file
		commitMarks []int // 1-based frame numbers carrying CommitDBSize != 0
		allowStale  bool  // trailing pre-checkpoint frames may be invalid
	}{
		// 3 autocommit txns: CREATE TABLE dirties p1,p2; each single-row
		// insert dirties p2 (leaf). Frame size 4096+24=4120, file 16512
		// bytes -> (16512-32)/4120 = 4 frames.
		{"wal-single-commit", 4, []int{2, 3, 4}, false},
		// + BEGIN/COMMIT pair (p2), CREATE t2 + 3000-byte zeroblob
		// (p1,p3 overflow chain). File 28872 -> (28872-32)/4120 = 7 frames.
		{"wal-multi-commit", 7, []int{2, 3, 4, 6, 7}, false},
		// RESTART checkpoint: header CheckpointSeq bumped to 1, salt-1
		// incremented (wal.c L95-97); the 2 post-checkpoint autocommit
		// inserts write 2 valid frames, then a third stale (invalid)
		// trailing frame remains from the pre-checkpoint WAL image.
		{"wal-after-checkpoint", 3, []int{1, 2}, true},
	}
	for _, c := range cases {
		h, frames := decodeWal(t, c.name+".db-wal")
		for _, f := range frames {
			if !f.Valid && !c.allowStale {
				t.Fatalf("%s: frame %d (pgno=%d) fails validity: salts %#x/%#x vs header %#x/%#x (wal.c validity rules (1)/(2))",
					c.name, f.Number, f.PageNumber, f.Salt1, f.Salt2, h.Salt1, h.Salt2)
			}
		}
		valid := 0
		var commits []int
		for _, f := range frames {
			if !f.Valid {
				continue
			}
			valid++
			if f.CommitDBSize != 0 {
				commits = append(commits, f.Number)
			}
		}
		if c.allowStale {
			if len(frames) != c.nFrames {
				t.Errorf("%s: %d total frames, want %d", c.name, len(frames), c.nFrames)
			}
		} else if valid != c.nFrames {
			t.Errorf("%s: %d valid frames, want %d", c.name, valid, c.nFrames)
		}
		if len(commits) != len(c.commitMarks) {
			t.Fatalf("%s: commit frames %v, want %v", c.name, commits, c.commitMarks)
		}
		for i, n := range c.commitMarks {
			if commits[i] != n {
				t.Fatalf("%s: commit frames %v, want %v (first divergence at %d)", c.name, commits, c.commitMarks, i)
			}
		}
		if c.name == "wal-after-checkpoint" {
			if h.CheckpointSeq != 1 {
				t.Errorf("checkpoint seq %d, want 1 after RESTART (wal.c L95-97)", h.CheckpointSeq)
			}
			// The stale pre-checkpoint frame must fail salt validation.
			if frames[len(frames)-1].Valid {
				t.Errorf("trailing stale frame wrongly valid (salt mismatch expected, wal.c rule (1))")
			}
			if LastCommitFrame(frames) != 2 {
				t.Errorf("last commit frame %d, want 2", LastCommitFrame(frames))
			}
		}
	}
}

func TestWALChecksumVector(t *testing.T) {
	// Golden vector: the wal-single-commit fixture header checksum must
	// recompute exactly (header bytes are oracle-written; recomputation is
	// the wal.c L949 walChecksumBytes call with zero seed).
	buf := readFixture(t, "wal-single-commit.db-wal")
	h, err := DecodeWalHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	s1, s2 := WalChecksumBytes(h.BigEndCksum, buf[:WalFrameHdrSize], 0, 0)
	if s1 != h.Checksum1 || s2 != h.Checksum2 {
		t.Errorf("header checksum recompute: got %08x/%08x, stored %08x/%08x",
			s1, s2, h.Checksum1, h.Checksum2)
	}
}

func TestJournalConformance(t *testing.T) {
	// Live mid-transaction PERSIST journals: the header was written by
	// writeJournalHdr with the pre-sync zeroed magic+nRec (pager.c L1488)
	// and populated cksumInit/dbOrigSize/sectorSize/pageSize.
	cases := []struct {
		name     string
		pageSize uint32
		sector   uint32
		minRecs  int // complete page records present
	}{
		{"jrnl-persist-basic", 1024, 512, 2},
		{"jrnl-persist-multi", 1024, 512, 2},
	}
	for _, c := range cases {
		buf := readFixture(t, c.name+".db-journal")
		h, err := DecodeJournalHeader(buf)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if h.MagicValid {
			t.Errorf("%s: live pre-sync journal unexpectedly shows valid magic (pager.c L1484-1490)", c.name)
		}
		if h.PageSize != c.pageSize {
			t.Errorf("%s: pageSize %d, want %d", c.name, h.PageSize, c.pageSize)
		}
		if h.SectorSize != c.sector {
			t.Errorf("%s: sectorSize %d, want %d", c.name, h.SectorSize, c.sector)
		}
		pages, err := DecodeJournalPages(buf, h)
		if err != nil {
			t.Fatalf("%s: pages: %v", c.name, err)
		}
		if len(pages) < c.minRecs {
			t.Fatalf("%s: %d page records, want >= %d (first divergence: record count)", c.name, len(pages), c.minRecs)
		}
		// Playback validity (pager.c L2343-2347): a record whose stored
		// checksum differs from pager_cksum(cksumInit, data) is stale
		// (leftover from an older journal generation, which PERSIST mode
		// may leave in place) and terminates the current generation
		// (SQLITE_DONE). First record must be valid; everything past the
		// first invalid record is ignored.
		valid := 0
		for _, p := range pages {
			if JournalChecksum(h.CksumInit, p.Data) != p.Checksum {
				break
			}
			valid++
		}
		if valid < 1 {
			t.Errorf("%s: record 1 (pgno=%d) fails pager_cksum validity: stored %08x, computed %08x",
				c.name, pages[0].PageNumber, pages[0].Checksum, JournalChecksum(h.CksumInit, pages[0].Data))
		}
	}
}
