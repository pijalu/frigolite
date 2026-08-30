package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/pager"
)

// TestAutoVacuumCommitCallbackFires verifies the sqlite3_autovacuum_pages
// callback (P8.INCRVACUUM phase 4) fires with the expected args at COMMIT
// time when the database is in FULL autovacuum mode. The callback must
// receive the file size in pages, the free count, and the page size.
//
// Reference: btree.c autoVacuumCommit (~line 4174) and
// sqlite3_autovacuum_pages (main.c ~line 2430).
func TestAutoVacuumCommitCallbackFires(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Set FULL autovacuum mode while the DB is still empty (so the pager
	// activates it).
	mustExec(t, e, "PRAGMA auto_vacuum=FULL")

	// Create a table.
	mustExec(t, e, "CREATE TABLE t1(x)")

	// Insert a 10000-byte payload so t1's btree has 1 root page + ~9
	// overflow pages. The cell local-payload is bounded by the page size,
	// so a 10000-byte zeroblob spills into ~9 overflow pages (1024 byte
	// pages, ~960 bytes local payload per cell).
	mustExec(t, e, "INSERT INTO t1 VALUES(zeroblob(10000))")

	// Sanity: ~12 pages in the file.
	if got := pg.NumPages(); got < 10 || got > 14 {
		t.Logf("warning: NumPages()=%d, expected ~12", got)
	}

	// Register the callback.
	var (
		called       int
		gotSchema    string
		gotFilesize  uint32
		gotFreesize  uint32
		gotPagesize  uint32
	)
	e.SetAutovacuumPagesCallback(func(schema string, fileSize, nFree, pageSize uint32) uint32 {
		called++
		gotSchema = schema
		gotFilesize = fileSize
		gotFreesize = nFree
		gotPagesize = pageSize
		// Drain all available pages (full vacuum this batch).
		return nFree
	})

	// BEGIN; DELETE; COMMIT (mirrors autovacuum2-1.1/1.2).
	mustExec(t, e, "BEGIN")
	mustExec(t, e, "DELETE FROM t1")
	mustExec(t, e, "COMMIT")

	if called != 1 {
		t.Fatalf("expected callback to fire once at COMMIT, got %d", called)
	}
	if gotFreesize != 9 {
		t.Errorf("nFree = %d, want 9 (overflow pages of the 10000-byte payload)", gotFreesize)
	}
	if gotPagesize != 1024 {
		t.Errorf("pageSize = %d, want 1024", gotPagesize)
	}
	if gotFilesize < 10 || gotFilesize > 14 {
		t.Errorf("fileSize = %d pages, expected ~12", gotFilesize)
	}
	if gotSchema != "main" {
		t.Errorf("schema = %q, want %q", gotSchema, "main")
	}
}

// TestRegisterCallback ensures SetAutovacuumPagesCallback stores the
// callback on the engine field and a subsequent getter returns it.
func TestRegisterCallback(t *testing.T) {
	pg := pager.OpenInMemory(1024)
	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	called := false
	cb := func(schema string, fileSize, nFree, pageSize uint32) uint32 {
		called = true
		return 0
	}
	e.SetAutovacuumPagesCallback(cb)
	got := e.getAutovacPagesCallback()
	if got == nil {
		t.Fatal("getter returned nil after set")
	}
	// Invoke the getter to confirm it's the same callback.
	got("main", 12, 9, 1024)
	if !called {
		t.Error("callback registered but not invoked through getter")
	}
	// Clear with nil.
	e.SetAutovacuumPagesCallback(nil)
	if e.getAutovacPagesCallback() != nil {
		t.Error("SetAutovacuumPagesCallback(nil) should clear")
	}
}

// mustExec is a tiny helper to run a single SQL statement in a test.
func mustExec(t *testing.T, e *Engine, sql string) *Result {
	t.Helper()
	stmts, err := parse.ParseSQL(sql)
	if err != nil || len(stmts) == 0 {
		t.Fatalf("parse %q: %v", sql, err)
	}
	res := e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("exec %q: %v", sql, res.Error)
	}
	return res
}
