package frigolite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// ---------------------------------------------------------------------------
// wal.c:4172-4190 padToSectorBoundary native pin (Fix B, PLAN_S10).
//
// A commit under synchronous=FULL on a device WITHOUT POWERSAFE_OVERWRITE
// repeats its last commit frame until the -wal crosses the next
// SQLITE_DEFAULT_SECTOR_SIZE (4096) boundary; the padding frames join the
// wal-index (mxFrame includes them). With PSO — the unix-VFS default — no
// padding happens. The sync-bits gate keeps NORMAL/OFF commits unpadded.
//
// The harness fixture exercises the same surface through walvfs-5.3 (the
// reader's read mark observes mxFrame=24 = 22 pages + 2 padding frames).
// ---------------------------------------------------------------------------

// walPadFrameCount returns the number of frames the -wal currently holds.
func walPadFrameCount(t *testing.T, path string, pageSize int64) int64 {
	t.Helper()
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	size := fi.Size()
	if size < 32 {
		return 0
	}
	return (size - 32) / (pageSize + 24)
}

// TestWalPadToSectorBoundary pins the padding rules across the PSO and
// synchronous-level gates.
func TestWalPadToSectorBoundary(t *testing.T) {
	// The walvfs-5.0 shape: a 1024-page-size db whose CREATE TABLE commits
	// in rollback mode (journal_mode=wal comes after), then 20 rows of 750
	// bytes — 20 leaf pages + the root update + the page-1 allocation stamp
	// = 22 frames, whose end lands 1488 bytes short of the 6th 4096 sector
	// boundary → 2 padding frames (mxFrame 24, the walvfs-5.3 expectation).
	build := func(t *testing.T, path string) *DB {
		t.Helper()
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		for _, stmt := range []string{
			"PRAGMA page_size = 1024",
			"PRAGMA auto_vacuum = 0",
			"CREATE TABLE t1(x)",
			"PRAGMA journal_mode = WAL",
			"PRAGMA synchronous = FULL",
		} {
			if res := db.Exec(stmt); res.Error != nil {
				t.Fatalf("%s: %v", stmt, res.Error)
			}
		}
		return db
	}
	const pageSize = int64(1024)
	const frameSize = pageSize + 24

	t.Run("pso off pads to sector boundary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		db := build(t, path)
		defer db.Close()
		// testvfs devchar sequential: the whole mask is REPLACED — no PSO.
		walPager(t, db).SetDeviceCharacteristics(0)
		if res := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s LIMIT 20) INSERT INTO t1 SELECT randomblob(750) FROM s"); res.Error != nil {
			t.Fatalf("INSERT: %v", res.Error)
		}
		// 32 + 22*1048 = 23088 → sync point 24576 → two extra frames.
		if n := walPadFrameCount(t, path, pageSize); n != 24 {
			t.Fatalf("frames = %d, want 24 (22 pages + 2 padding)", n)
		}
	})

	t.Run("pso on does not pad", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		db := build(t, path)
		defer db.Close()
		// The unix-VFS default: POWERSAFE_OVERWRITE set — no padding.
		if got := walPager(t, db).DeviceCharacteristics() & pager.IOCAPPowerSafeOverwrite; got == 0 {
			t.Fatalf("default device characteristics lack POWERSAFE_OVERWRITE")
		}
		if res := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s LIMIT 20) INSERT INTO t1 SELECT randomblob(750) FROM s"); res.Error != nil {
			t.Fatalf("INSERT: %v", res.Error)
		}
		if n := walPadFrameCount(t, path, pageSize); n != 22 {
			t.Fatalf("frames = %d, want 22 (no padding)", n)
		}
	})

	t.Run("normal sync does not pad", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		db := build(t, path)
		defer db.Close()
		walPager(t, db).SetDeviceCharacteristics(0)
		if res := db.Exec("PRAGMA synchronous = NORMAL"); res.Error != nil {
			t.Fatalf("synchronous: %v", res.Error)
		}
		if res := db.Exec("WITH s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s LIMIT 20) INSERT INTO t1 SELECT randomblob(750) FROM s"); res.Error != nil {
			t.Fatalf("INSERT: %v", res.Error)
		}
		if n := walPadFrameCount(t, path, pageSize); n != 22 {
			t.Fatalf("frames = %d, want 22 (no padding without commit sync bits)", n)
		}
	})
}
