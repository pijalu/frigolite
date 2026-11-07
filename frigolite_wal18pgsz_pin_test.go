package frigolite

import (
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
)

// ---------------------------------------------------------------------------
// wal-18.2 native engine-contract pin.
//
// The harness fixture drops the crafting do_test body (wal.test:1172-1208):
// the converter cannot express `binary format`/`binary scan` (the logcksum
// proc) or the `open/puts/close` file write, so the crafted -wal never
// exists and the 24 valid-page-size assertions run against nothing. This
// test pins the SAME engine-visible contract natively (see
// portplan/PLAN_S9_wal18pgsz.md):
//
//   - A WAL whose header page-size is a power of 2 in [512, 65536] is
//     recovered: its single commit frame (garbage content) is applied and
//     the database becomes corrupt (integrity_check errors).
//   - A WAL whose header page-size is invalid (128, 256, 131072 > MAX, or
//     1016 non-power-of-2) is ignored: the log counts as empty and
//     integrity_check reports "ok".
//
// C reference: wal.c walIndexRecover's header gate (L1440-1449) and the
// min(nOut, sz) frame read (L3643-3650).
// ---------------------------------------------------------------------------

// wal18CraftLog writes the hand-crafted WAL of wal-18.2: a 32-byte header
// (magic 0x377F0682 little-endian-checksum, version 3007000, page size
// pgsz, checkpoint seq 1234, salts 22/23) plus one commit frame for page
// pgno (nTruncate 5) whose body is pgsz pseudo-random bytes, with the
// checksum chain the TCL logcksum proc computes.
func wal18CraftLog(t *testing.T, path string, pgsz int, pgno uint32) {
	t.Helper()
	be := binary.BigEndian
	hdr := make([]byte, 24)
	be.PutUint32(hdr[0:], 931071618) // WAL magic, little-endian checksums
	be.PutUint32(hdr[4:], 3007000)
	be.PutUint32(hdr[8:], uint32(pgsz))
	be.PutUint32(hdr[12:], 1234)
	be.PutUint32(hdr[16:], 22)
	be.PutUint32(hdr[20:], 23)
	c1, c2 := pager.WalChecksumBytes(false, hdr, 0, 0)
	hdr = append(hdr, make([]byte, 8)...)
	be.PutUint32(hdr[24:], c1)
	be.PutUint32(hdr[28:], c2)

	body := make([]byte, pgsz)
	rng := rand.New(rand.NewSource(int64(pgsz)*1000 + int64(pgno))) //nolint:gosec
	for i := 0; i < pgsz; i += 8 {
		be.PutUint64(body[i:], rng.Uint64())
	}

	frame := make([]byte, 24)
	be.PutUint32(frame[0:], pgno)
	be.PutUint32(frame[4:], 5) // commit record: nTruncate=5 pages
	be.PutUint32(frame[8:], 22)
	be.PutUint32(frame[12:], 23)
	c1, c2 = pager.WalChecksumBytes(false, frame[:8], c1, c2)
	c1, c2 = pager.WalChecksumBytes(false, body, c1, c2)
	be.PutUint32(frame[16:], c1)
	be.PutUint32(frame[20:], c2)

	content := append(hdr, frame...)
	content = append(content, body...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write crafted wal: %v", err)
	}
}

// wal18CopyFile copies src to dst byte-for-byte.
func wal18CopyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// TestWalLogPageSizeValidation pins wal-18.2's recovery contract across the
// twelve header page sizes and the three frame page numbers.
func TestWalLogPageSizeValidation(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "test.db")
	keep := filepath.Join(dir, "testX.db")

	// wal-18.0's fixture database: 3 pages, WAL mode, one checkpointed row
	// plus three rows left standing in the log.
	db, err := Open(base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, stmt := range []string{
		"PRAGMA page_size = 1024",
		"PRAGMA auto_vacuum = 0",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = OFF",
		"CREATE TABLE t1(a, b, UNIQUE(a, b))",
		"INSERT INTO t1 VALUES(0, 0)",
		"PRAGMA wal_checkpoint",
		"INSERT INTO t1 VALUES(1, 2)",
		"INSERT INTO t1 VALUES(3, 4)",
		"INSERT INTO t1 VALUES(5, 6)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	if err := wal18CopyFile(keep, base); err != nil {
		t.Fatalf("copy aside: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, tc := range []struct {
		pgsz  int
		valid bool
	}{
		{128, false}, {256, false},
		{512, true}, {1024, true}, {2048, true}, {4096, true},
		{8192, true}, {16384, true}, {32768, true}, {65536, true},
		{131072, false}, {1016, false},
	} {
		for pg := uint32(1); pg <= 3; pg++ {
			name := "pgsz" + strconv.Itoa(tc.pgsz) + "/pg" + strconv.Itoa(int(pg))
			t.Run(name, func(t *testing.T) {
				if err := wal18CopyFile(base, keep); err != nil {
					t.Fatalf("restore: %v", err)
				}
				walPath := base + "-wal"
				if err := os.RemoveAll(walPath); err != nil {
					t.Fatalf("remove wal: %v", err)
				}
				wal18CraftLog(t, walPath, tc.pgsz, pg)
				fi, err := os.Stat(walPath)
				if err != nil || fi.Size() != int64(56+tc.pgsz) {
					t.Fatalf("crafted wal size = %v, want %d", fi, 56+tc.pgsz)
				}

				w, err := Open(base)
				if err != nil {
					// The TCL catches only the integrity_check body; an open
					// failure still counts as corruption (valid sizes) but
					// is unexpected for the ignored-log cases.
					if tc.valid {
						return
					}
					t.Fatalf("Open with ignored log failed: %v", err)
				}
				defer w.Close()
				res := w.Query("PRAGMA integrity_check")
				if tc.valid {
					// The garbage frame must surface as corruption: either
					// the query errors or the report is not "ok".
					if res.Error == nil && len(res.Rows) > 0 && res.Rows[0][0] == "ok" {
						t.Fatalf("pgsz %d: integrity_check ok, want corruption", tc.pgsz)
					}
				} else {
					// The log must be ignored entirely.
					if res.Error != nil {
						t.Fatalf("pgsz %d: integrity_check error %v, want ok", tc.pgsz, res.Error)
					}
					if len(res.Rows) == 0 || res.Rows[0][0] != "ok" {
						t.Fatalf("pgsz %d: integrity_check = %v, want ok", tc.pgsz, res.Rows)
					}
				}
			})
		}
	}
}
