package frigolite

import (
	"fmt"
	"os"
	"testing"
)

// TestBackupBasicBackup verifies a basic file-to-file backup copies schema
// and data (backup.test backup-1.4).
func TestBackupEmptySourceRewritesDestAsOnePage(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	// Destination with content at the default page size (3 pages on disk).
	dst, err := Open("dest.db")
	if err != nil {
		t.Fatal(err)
	}
	if r := dst.Exec("CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 'x'); INSERT INTO t1 VALUES(2, 'y');"); r.Error != nil {
		t.Fatal(r.Error)
	}
	defer dst.Close()
	fi, err := os.Stat("dest.db")
	if err != nil {
		t.Fatal(err)
	}
	destSizeBefore := fi.Size()
	if destSizeBefore < 2048 {
		t.Fatalf("pre: dest file expected >= 2 pages, got %d bytes", destSizeBefore)
	}

	// Empty source: opened but never written (0-byte file, pager.c lazy
	// creation).
	src, err := Open("src.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if rc := b.Step(-1); rc != "SQLITE_DONE" {
		t.Fatalf("step: %s", rc)
	}
	if rc := b.Finish(); rc != "SQLITE_OK" {
		t.Fatalf("finish: %s", rc)
	}

	// backup.c:417-424 (nSrcPage==0 → sqlite3BtreeNewDb + truncate to
	// nDestTruncate=1): the destination is a fresh 1-page database at the
	// SOURCE page size — exactly pageSize bytes on disk.
	fi, err = os.Stat("dest.db")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 1024 {
		t.Errorf("dest file after empty-source backup = %d bytes, want 1024", fi.Size())
	}
	dst2, err := Open("dest.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst2.Close()
	res := dst2.Query("SELECT count(*) FROM sqlite_master")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) == 1 {
		if n, ok := res.Rows[0][0].(int64); !ok || n != 0 {
			t.Errorf("sqlite_master count = %v, want 0", res.Rows[0][0])
		}
	} else {
		t.Errorf("unexpected sqlite_master result: %v", res.Rows)
	}
	// The rewritten image must be structurally valid.
	res = dst2.Query("PRAGMA integrity_check")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.Rows) > 0 && res.Rows[0][0] != "ok" {
		t.Errorf("integrity_check = %v, want ok", res.Rows[0][0])
	}
}

func TestBackupBasicBackup(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	r := src.Exec("CREATE TABLE t1(a, b); CREATE INDEX i1 ON t1(a, b); INSERT INTO t1 VALUES(1, 'x'); INSERT INTO t1 VALUES(2, 'y');")
	if r.Error != nil {
		t.Fatal(r.Error)
	}

	dst, err := Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	rc := b.Step(200)
	if rc != "SQLITE_DONE" {
		t.Fatalf("step 200: got %s want SQLITE_DONE", rc)
	}
	if rc := b.Finish(); rc != "SQLITE_OK" {
		t.Fatalf("finish: got %s want SQLITE_OK", rc)
	}

	// Destination must match source content.
	got := dst.Query("SELECT * FROM t1 ORDER BY a")
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("dst rows: got %d want 2", len(got.Rows))
	}
	// Integrity check.
	ic := dst.Query("PRAGMA integrity_check")
	if ic.Error != nil {
		t.Fatal(ic.Error)
	}
	if fmt.Sprint(ic.Rows) != "[[ok]]" {
		t.Fatalf("integrity: got %v", ic.Rows)
	}
	// Index must exist.
	idx := dst.Query("SELECT name FROM sqlite_master WHERE type='index' ORDER BY name")
	if fmt.Sprint(idx.Rows) != "[[i1]]" {
		t.Fatalf("indexes: got %v", idx.Rows)
	}
}

// TestBackupPageCounting verifies the remaining/pagecount accounting
// (backup.test backup-6).
func TestBackupPageCounting(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	r := src.Exec("PRAGMA page_size = 1024; CREATE TABLE t1(a, b); CREATE INDEX i1 ON t1(a, b); INSERT INTO t1 VALUES(1, randomblob(1000)); INSERT INTO t1 VALUES(2, randomblob(1000)); INSERT INTO t1 VALUES(3, randomblob(1000)); INSERT INTO t1 VALUES(4, randomblob(1000)); INSERT INTO t1 VALUES(5, randomblob(1000));")
	if r.Error != nil {
		t.Fatal(r.Error)
	}

	dst, err := Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	// nTotal must be captured from the backup's reported pagecount (which
	// includes SQLite-visible auto-index pages), not the raw pager count.
	nTotal := b.Pagecount()
	if nTotal < 3 {
		t.Fatalf("expected at least 3 pages, got %d", nTotal)
	}
	if rc := b.Step(1); rc != "SQLITE_OK" {
		t.Fatalf("step 1: got %s want SQLITE_OK", rc)
	}
	if got, want := b.Pagecount(), nTotal; got != want {
		t.Fatalf("pagecount: got %d want %d", got, want)
	}
	if got, want := b.Remaining(), nTotal-1; got != want {
		t.Fatalf("remaining: got %d want %d", got, want)
	}
	if rc := b.Step(5); rc != "SQLITE_OK" {
		t.Fatalf("step 5: got %s want SQLITE_OK", rc)
	}
	if got, want := b.Remaining(), nTotal-6; got != want {
		t.Fatalf("remaining after 6: got %d want %d", got, want)
	}
	// Source grows mid-backup.
	if r := src.Exec("CREATE TABLE t2(a PRIMARY KEY, b)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if rc := b.Step(1); rc != "SQLITE_OK" {
		t.Fatalf("step after grow: got %s want SQLITE_OK", rc)
	}
	if got, want := b.Remaining(), nTotal+2-7; got != want {
		t.Fatalf("remaining after grow: got %d want %d", got, want)
	}
	if got, want := b.Pagecount(), nTotal+2; got != want {
		t.Fatalf("pagecount after grow: got %d want %d", got, want)
	}
	if rc := b.Finish(); rc != "SQLITE_OK" {
		t.Fatalf("finish: got %s want SQLITE_OK", rc)
	}
}

// TestBackupErrors verifies the sqlite3_backup_init error cases (backup.test
// backup-4.1/4.4 and backup2's db backup/restore messages).
func TestBackupErrors(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_ = src.Exec("CREATE TABLE t1(a)")
	dst, err := Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	// Unknown source schema.
	_, err = src.NewBackup(dst, "main", "aux")
	if err == nil {
		t.Fatal("expected error for unknown source schema")
	}
	if got, want := err.Error(), "unknown database aux"; got != want {
		t.Fatalf("unknown src: got %q want %q", got, want)
	}
	if got, want := dst.LastErr(), "unknown database aux"; got != want {
		t.Fatalf("dst errmsg: got %q want %q", got, want)
	}
	// Unknown dest schema.
	_, err = src.NewBackup(dst, "aux", "main")
	if err == nil {
		t.Fatal("expected error for unknown dest schema")
	}
	// Same handle.
	_, err = src.NewBackup(src, "main", "main")
	if err == nil {
		t.Fatal("expected error for same handle")
	}
	if got, want := err.Error(), "source and destination must be distinct"; got != want {
		t.Fatalf("same handle: got %q want %q", got, want)
	}
	if got, want := src.LastErrCode(), "SQLITE_ERROR"; got != want {
		t.Fatalf("errcode: got %q want %q", got, want)
	}
}

// TestBackupMemoryDestEmptyPageSizeAdoptsSource verifies SQLite backup.c
// setDestPgsz behavior for an empty in-memory destination.
func TestBackupMemoryDestEmptyPageSizeAdoptsSource(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if r := src.Exec("PRAGMA page_size = 1024; CREATE TABLE t1(a); INSERT INTO t1 VALUES(1)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	dst, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if r := dst.Exec("PRAGMA page_size = 4096"); r.Error != nil {
		t.Fatal(r.Error)
	}
	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if rc := b.Step(-1); rc != "SQLITE_DONE" {
		t.Fatalf("step: got %s want SQLITE_DONE", rc)
	}
	if got := dst.Query("PRAGMA page_size"); got.Error != nil || len(got.Rows) != 1 || got.Rows[0][0] != int64(1024) {
		t.Fatalf("destination page size: %#v", got)
	}
}

// TestBackupMemoryDestPageSizeMismatch verifies the in-memory destination
// page-size mismatch returns SQLITE_READONLY (backup.test backup-4.5).
func TestBackupMemoryDestPageSizeMismatch(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_ = src.Exec("CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, 2)")
	dst, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	_ = dst.Exec("PRAGMA page_size = 4096; CREATE TABLE t2(a, b); INSERT INTO t2 VALUES(3, 4)")

	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatalf("init should succeed: %v", err)
	}
	if rc := b.Step(5000); rc != "SQLITE_READONLY" {
		t.Fatalf("step: got %s want SQLITE_READONLY", rc)
	}
	if rc := b.Finish(); rc != "SQLITE_READONLY" {
		t.Fatalf("finish: got %s want SQLITE_READONLY", rc)
	}
}

// TestBackupCloseBusy verifies that closing a connection with an active
// backup fails with SQLITE_BUSY (backup.test backup-4.3).
func TestBackupCloseBusy(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	os.Chdir(dir)

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_ = src.Exec("CREATE TABLE t1(a)")
	_ = src.Exec("ATTACH 'test3.db' AS aux1; CREATE TABLE aux1.t1(a, b)")
	dst, err := Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	_ = dst.Exec("ATTACH 'test4.db' AS aux2; CREATE TABLE aux2.t2(a, b)")

	b, err := src.NewBackup(dst, "aux2", "aux1")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	// Closing the destination connection while the backup is active must fail.
	if err := dst.Close(); err == nil {
		t.Fatal("expected close error with active backup")
	} else if got, want := err.Error(), "unable to close due to unfinalized statements or unfinished backups"; got != want {
		t.Fatalf("close errmsg: got %q want %q", got, want)
	}
	if got, want := dst.LastErr(), "unable to close due to unfinalized statements or unfinished backups"; got != want {
		t.Fatalf("dst errmsg: got %q want %q", got, want)
	}
	// DETACH of the backup SOURCE must fail (the backup holds a read lock on
	// it, SQLite: "database aux1 is locked"). The destination is not locked.
	dr := src.Exec("DETACH aux1")
	if dr.Error == nil {
		t.Fatal("expected DETACH error with active backup")
	} else if got, want := dr.Error.Error(), "database aux1 is locked"; got != want {
		t.Fatalf("detach errmsg: got %q want %q", got, want)
	}
	// The destination schema can be detached (backup5-3.2 detaches it
	// mid-backup); the backup then errors on step.
	dr2 := dst.Exec("DETACH aux2")
	if dr2.Error != nil {
		t.Fatalf("dest detach should succeed: %v", dr2.Error)
	}
	dst.Exec("ATTACH 'test4.db' AS aux2") // re-attach so the backup's Step works
	// Completing the backup releases the locks.
	if rc := b.Step(50); rc != "SQLITE_DONE" {
		t.Fatalf("step: got %s", rc)
	}
	if rc := b.Finish(); rc != "SQLITE_OK" {
		t.Fatalf("finish: got %s", rc)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close after finish: %v", err)
	}
}

// TestBackupBusyLocking verifies SQLITE_BUSY scenarios (backup.test backup-7).
func TestBackupBusyLocking(t *testing.T) {
	dir, _ := os.MkdirTemp("", "backup")
	defer os.RemoveAll(dir)
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_ = src.Exec("CREATE TABLE t1(a, b); INSERT INTO t1 VALUES(1, randomblob(1000)); INSERT INTO t1 SELECT 2, randomblob(1000) FROM t1; INSERT INTO t1 SELECT 3, randomblob(1000) FROM t1;")
	dst, err := Open("test2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	b, err := src.NewBackup(dst, "main", "main")
	if err != nil {
		t.Fatal(err)
	}
	if rc := b.Step(5); rc != "SQLITE_OK" {
		t.Fatalf("step 5: got %s", rc)
	}
	// Another connection takes BEGIN EXCLUSIVE on the source file.
	locker, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	locker.BeginExclusive()
	if rc := b.Step(5); rc != "SQLITE_BUSY" {
		t.Fatalf("step with exclusive: got %s want SQLITE_BUSY", rc)
	}
	locker.Exec("ROLLBACK") // clears exclusive via execRollback? need BeginExclusive cleared
	if rc := b.Step(5000); rc != "SQLITE_DONE" {
		t.Fatalf("step after release: got %s want SQLITE_DONE", rc)
	}
	locker.Close()
}
