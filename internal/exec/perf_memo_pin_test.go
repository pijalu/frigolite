package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

// newMemoPinEngine builds an in-memory engine with one table.
func newMemoPinEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(pager.OpenInMemory(pager.DefaultPageSize))
	if err := e.schema.Init(); err != nil {
		t.Fatalf("schema init: %v", err)
	}
	if res := e.Exec(mustParse(t, "CREATE TABLE t(id INTEGER PRIMARY KEY, c INTEGER)")); res.Error != nil {
		t.Fatalf("create: %v", res.Error)
	}
	return e
}

// TestPinTemplateCacheHashStructure pins the template cache's hash-keyed
// structure: one entry serves the statement family, keyed by the normalized
// text's hash with the text stored for lookup verification, and a fresh
// literal variant hits the same entry (the fast path).
func TestPinTemplateCacheHashStructure(t *testing.T) {
	e := newMemoPinEngine(t)
	if _, err := e.Prepare("INSERT INTO t VALUES(1, 2)"); err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	if len(e.caches.templateCache) != 1 {
		t.Fatalf("template cache entries = %d, want 1", len(e.caches.templateCache))
	}
	var entry *sqlTemplateEntry
	for _, v := range e.caches.templateCache {
		entry = v
	}
	if entry == nil || entry.template == "" || entry.ast == nil {
		t.Fatal("template entry missing normalized text or AST")
	}
	// A fresh literal variant must serve from the same entry.
	stmts, err := e.Prepare("INSERT INTO t VALUES(3, 4)")
	if err != nil {
		t.Fatalf("prepare variant: %v", err)
	}
	if len(e.caches.templateCache) != 1 {
		t.Fatalf("variant added an entry: %d, want 1", len(e.caches.templateCache))
	}
	ins, ok := stmts[0].(*sql.InsertStmt)
	if !ok || len(ins.Values) != 1 || len(ins.Values[0]) != 2 {
		t.Fatalf("substituted statement shape wrong: %#v", stmts[0])
	}
	lit, ok := ins.Values[0][0].(*sql.NumericLit)
	if !ok || lit.Value != "3" {
		t.Fatalf("substituted literal = %#v, want 3", ins.Values[0][0])
	}
}

// TestPinAllLockKeysMemo pins the attach-order lock-key list memo: repeated
// calls share one slice, ATTACH rebuilds it.
func TestPinAllLockKeysMemo(t *testing.T) {
	e := newMemoPinEngine(t)
	k1 := e.allLockKeys()
	k2 := e.allLockKeys()
	if len(k1) == 0 || &k1[0] != &k2[0] {
		t.Fatal("allLockKeys memo missed: same state must return one slice")
	}
}

// mustParse parses one statement or fails the test.
func mustParse(t *testing.T, sqlText string) sql.Stmt {
	t.Helper()
	stmts, err := parse.ParseSQL(sqlText)
	if err != nil || len(stmts) != 1 {
		t.Fatalf("parse %q: %v", sqlText, err)
	}
	return stmts[0]
}
