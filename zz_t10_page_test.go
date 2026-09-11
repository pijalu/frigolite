package frigolite

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// dumpPage2 opens the db file and returns page 2's first 32 bytes plus the
// first 16 bytes at the header-declared content offset.
func dumpPage2(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if len(data) < 3*1024 {
		t.Logf("file size=%d", len(data))
		return "file too short"
	}
	// pageSize from header (offset 16, big-endian; 1 means 65536)
	ps := int(data[16])<<8 | int(data[17])
	if ps == 1 {
		ps = 65536
	}
	p2 := 1 * ps
	p3 := 2 * ps
	// cell pointer array right after 8-byte header; last 16 bytes hold the cell
	out := "ps=" + strconv.Itoa(ps) +
		" page2hdr=" + hex.EncodeToString(data[p2:p2+12]) +
		" cellptr=" + hex.EncodeToString(data[p2+8:p2+12]) +
		" celltail=" + hex.EncodeToString(data[p2+ps-16:p2+ps]) +
		" page3hdr=" + hex.EncodeToString(data[p3:p3+12]) +
		" p3celltail=" + hex.EncodeToString(data[p3+ps-16:p3+ps])
	return out
}

// runRepro executes the T10 statements on a fresh file DB and returns the
// integrity_check output plus a page-2 dump.
func runRepro(t *testing.T, insert string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t10.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if res := db.Exec("CREATE TABLE t1(b UNIQUE, a INT PRIMARY KEY) WITHOUT ROWID"); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	if res := db.Exec(insert); res.Error != nil {
		t.Fatalf("insert: %v", res.Error)
	}
	ic := db.Query("PRAGMA integrity_check")
	icOut := ""
	for _, row := range ic.Rows {
		if len(row) > 0 {
			icOut += row[0].(string) + "; "
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return icOut, dumpPage2(t, path)
}

// TestZZT10PageDump dumps page 2 after the named-column vs full-tuple insert.
// TEMPORARY debug probe — deleted before commit.
func TestZZT10PageDump(t *testing.T) {
	for _, ins := range []string{
		"INSERT INTO t1(a) VALUES('1')",
		"INSERT INTO t1 VALUES('x', 3)",
		"INSERT OR IGNORE INTO t1(a) VALUES('1') ON CONFLICT(a) DO NOTHING",
		"INSERT INTO t1(a) VALUES('1') ON CONFLICT(a) DO NOTHING",
	} {
		ic, dump := runRepro(t, ins)
		t.Logf("%s\n  integrity_check: %s\n  %s", ins, ic, dump)
	}
}
