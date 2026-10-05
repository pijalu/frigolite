package frigolite

import (
	"os"
	"strings"
	"testing"
)

// TestMiscregAttachedMalformedTriggerStillDetected pins triggerupfrom-2.4:
// a trigger stored in an ATTACHed database whose body references main-db
// objects is malformed schema — accessing any table after the ATTACH must
// report "trigger tr3 cannot reference objects in database main". The
// PERF.UPDDEL loaded-trigger validation memo (661902335) keyed its verdict
// on MAIN's schema fingerprint alone; the ATTACH moved the walk's input
// (the attached schema now holds triggers) without moving that key, so the
// error silently vanished.
func TestMiscregAttachedMalformedTriggerStillDetected(t *testing.T) {
	dir, _ := os.MkdirTemp("", "miscreg")
	defer os.RemoveAll(dir)
	t.Chdir(dir)

	// Home connection: the trigger is valid here — main.link exists.
	src, err := Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	if r := src.Exec(`
		CREATE TABLE t1(a, b);
		INSERT INTO t1 VALUES(1, 'one');
		CREATE TABLE link(f, t);
		CREATE TRIGGER tr3 BEFORE DELETE ON t1 BEGIN
			UPDATE t1 SET b=coalesce(old.b,old.c) FROM main.link WHERE a=t AND old.a=f;
		END;
	`); r.Error != nil {
		t.Fatal(r.Error)
	}
	src.Close()

	// Fresh connection with an EMPTY main: attaching test.db as yyy loads
	// tr3, whose main.link reference no longer resolves — malformed schema.
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := db.Exec("ATTACH 'test.db' AS yyy; SELECT * FROM t1;")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "trigger tr3 cannot reference objects in database main") {
		t.Fatalf("expected malformed-schema error, got: %v", r.Error)
	}
}

// TestMiscregReattachedSchemaNameRevalidatesTriggers pins the ATTACH/DETACH
// invalidation of the validated-trigger cache: a schema name whose trigger
// validated against one file must re-validate when a different file is
// attached under the same name (the name-keyed marks and the walk memo
// describe the previous schema state).
func TestMiscregReattachedSchemaNameRevalidatesTriggers(t *testing.T) {
	dir, _ := os.MkdirTemp("", "miscreg2")
	defer os.RemoveAll(dir)
	t.Chdir(dir)

	// File A: trigger references main.good — valid while main has good.
	a, err := Open("a.db")
	if err != nil {
		t.Fatal(err)
	}
	if r := a.Exec(`
		CREATE TABLE t1(x);
		CREATE TABLE good(v);
		CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN
			UPDATE t1 SET x=1 FROM main.good;
		END;
	`); r.Error != nil {
		t.Fatal(r.Error)
	}
	a.Close()

	// File B: same trigger name, but main.missing never exists anywhere.
	b, err := Open("b.db")
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Exec(`
		CREATE TABLE t1(x);
		CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN
			UPDATE t1 SET x=0 FROM main.missing;
		END;
	`); r.Error != nil {
		t.Fatal(r.Error)
	}
	b.Close()

	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Attach A, validate tr (runs the memoized statement), detach.
	if r := db.Exec("CREATE TABLE good(v)"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("ATTACH 'a.db' AS yyy"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := db.Exec("INSERT INTO yyy.t1 VALUES(1)"); r.Error != nil {
		t.Fatalf("file A trigger should be valid: %v", r.Error)
	}
	if r := db.Exec("DETACH yyy"); r.Error != nil {
		t.Fatal(r.Error)
	}

	// Re-attach DIFFERENT content under the same schema name: the walk must
	// re-run and report the malformed trigger, not inherit A's verdict.
	if r := db.Exec("ATTACH 'b.db' AS yyy"); r.Error != nil {
		t.Fatal(r.Error)
	}
	r := db.Exec("INSERT INTO yyy.t1 VALUES(2)")
	if r.Error == nil || !strings.Contains(r.Error.Error(), "trigger tr cannot reference objects in database main") {
		t.Fatalf("re-attached schema must re-validate triggers, got: %v", r.Error)
	}
}

// TestMiscregBackupIntoPopulatedAttachedDest pins backup-2.x: a backup into
// an ATTACHed, populated destination file whose page size differs from the
// source must succeed with SQLITE_OK and leave a consistent copy. The
// PERF.UPDDEL allTableIndexes memo (76e92640a) keyed its one slot on MAIN's
// schema fingerprint; the copy's destination DDL moved only the attached
// schema, so INSERT index maintenance wrote into the dropped schema's freed
// root pages ("database disk image is malformed").
func TestMiscregBackupIntoPopulatedAttachedDest(t *testing.T) {
	dir, _ := os.MkdirTemp("", "miscreg3")
	defer os.RemoveAll(dir)
	t.Chdir(dir)

	src, err := Open("src.db")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if r := src.Exec(`
		PRAGMA page_size = 1024;
		BEGIN;
		CREATE TABLE t1(a, b);
		CREATE INDEX i1 ON t1(a, b);
		INSERT INTO t1 VALUES(1, 'aaa');
		INSERT INTO t1 VALUES(2, 'bbb');
		INSERT INTO t1 VALUES(3, 'ccc');
		COMMIT;
	`); r.Error != nil {
		t.Fatal(r.Error)
	}

	dst, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if r := dst.Exec("ATTACH 'test2.db' AS bak"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := dst.Exec("PRAGMA bak.page_size = 4096"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := dst.Exec("BEGIN; CREATE TABLE bak.t1(a, b); CREATE INDEX bak.i1 ON t1(a, b);"); r.Error != nil {
		t.Fatal(r.Error)
	}
	for i := 0; i < 3; i++ {
		if r := dst.Exec("INSERT INTO bak.t1 VALUES(1, 'zzz')"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := dst.Exec("COMMIT"); r.Error != nil {
		t.Fatal(r.Error)
	}

	b, err := src.NewBackup(dst, "bak", "main")
	if err != nil {
		t.Fatal(err)
	}
	for rc := b.Step(200); rc == "SQLITE_OK"; rc = b.Step(200) {
	}
	if rc := b.Finish(); rc != "SQLITE_OK" {
		t.Fatalf("backup finish: %s (%s)", rc, b.ErrMsg())
	}
	if r := dst.Query("PRAGMA bak.integrity_check"); r.Error != nil {
		t.Fatal(r.Error)
	} else if len(r.Rows) != 1 || r.Rows[0][0] != "ok" {
		t.Fatalf("integrity_check: %v", r.Rows)
	}
	if r := dst.Query("SELECT a, b FROM bak.t1 ORDER BY a"); r.Error != nil {
		t.Fatal(r.Error)
	} else if len(r.Rows) != 3 || r.Rows[0][1] != "aaa" || r.Rows[2][1] != "ccc" {
		t.Fatalf("copied rows: %v", r.Rows)
	}
}
