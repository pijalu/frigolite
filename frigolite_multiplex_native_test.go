// SPDX-License-Identifier: GPL-3.0-or-later
package frigolite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNativeMultiplex pins the multiplexor VFS shim (src/test_multiplex.c
// port) through the public API: a file database is always attached to a
// multiplexor group (enabled by default, chunk size 2147418112 — a
// pass-through); multiplex_control(2, N) sets the chunk size (rounded up to
// a 64 KiB multiple); rows spanning chunk files survive a close/reopen with
// the chunk size rederived from chunk 0's on-disk size; the multiplex_*
// pragmas report the group state.
func TestNativeMultiplex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	must := func(sql string) {
		t.Helper()
		if res := db.Exec(sql); res.Error != nil {
			t.Fatalf("%s: %v", sql, res.Error)
		}
	}
	one := func(sql string) string {
		t.Helper()
		res := db.Query(sql)
		if res.Error != nil || len(res.Rows) != 1 {
			t.Fatalf("%s: %v %v", sql, res.Error, res.Rows)
		}
		return formatHarnessValue("", res.Rows[0][0])
	}
	if got := one(`PRAGMA multiplex_enabled`); got != "1" {
		t.Fatalf("multiplex_enabled = %s, want 1", got)
	}
	if got := one(`PRAGMA multiplex_chunksize`); got != "2147418112" {
		t.Fatalf("default chunksize = %s, want 2147418112", got)
	}
	if got := one(`PRAGMA multiplex_filecount`); got != "1" {
		t.Fatalf("filecount = %s, want 1", got)
	}
	if res := db.Exec(`SELECT multiplex_control(-1, 0)`); res.Error == nil {
		t.Fatal("multiplex_control(-1) must error")
	}
	if res := db.Exec(`SELECT multiplex_control(4, 1)`); res.Error == nil {
		t.Fatal("multiplex_control(4) must error")
	}
	must(`SELECT multiplex_control(2, 4096)`) // rounds up to 65536
	if got := one(`PRAGMA multiplex_chunksize`); got != "65536" {
		t.Fatalf("chunksize = %s, want 65536", got)
	}
	must(`CREATE TABLE t1(a PRIMARY KEY, b)`)
	must(`INSERT INTO t1 VALUES(1, 'one'), (2, randomblob(65536)), (3, randomblob(65536))`)
	if got := one(`PRAGMA multiplex_filecount`); got != "3" {
		t.Fatalf("filecount after chunk-spanning inserts = %s, want 3", got)
	}
	if got := one(`SELECT length(b) FROM t1 WHERE a=2`); got != "65536" {
		t.Fatalf("length(b) across chunk boundary = %s, want 65536", got)
	}
	db.Close()

	// Reopen: the chunk size is rederived from chunk 0's on-disk size (a
	// 64 KiB multiple), and the data spanning chunks is readable.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	one2 := func(sql string) string {
		t.Helper()
		res := db2.Query(sql)
		if res.Error != nil || len(res.Rows) != 1 {
			t.Fatalf("reopen %s: %v %v", sql, res.Error, res.Rows)
		}
		return formatHarnessValue("", res.Rows[0][0])
	}
	if got := one2(`SELECT count(*) FROM t1`); got != "3" {
		t.Fatalf("reopen count = %s, want 3", got)
	}
	if got := one2(`SELECT length(b) FROM t1 WHERE a=3`); got != "65536" {
		t.Fatalf("reopen length(b) = %s, want 65536", got)
	}
	if got := one2(`PRAGMA multiplex_chunksize`); got != "65536" {
		t.Fatalf("reopen chunksize = %s, want 65536 (rederived)", got)
	}
	if got := one2(`PRAGMA multiplex_filecount`); got != "3" {
		t.Fatalf("reopen filecount = %s, want 3", got)
	}
	// The chunk files carry the three-digit suffix.
	if _, err := os.Stat(path + "001"); err != nil {
		t.Fatalf("chunk 1 file: %v", err)
	}
	if _, err := os.Stat(path + "002"); err != nil {
		t.Fatalf("chunk 2 file: %v", err)
	}
}
