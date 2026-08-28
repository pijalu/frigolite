package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
)

func TestExecCreateTable(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	stmts, err := parse.ParseSQL("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	if err != nil || len(stmts) == 0 {
		t.Fatalf("empty parse result: %v", err)
	}

	res := e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Exec: %v", res.Error)
	}

	// Insert a row
	stmts, err = parse.ParseSQL("INSERT INTO t VALUES (1, 'Alice')")
	if err != nil || len(stmts) == 0 {
		t.Fatalf("empty parse result: %v", err)
	}
	res = e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Insert: %v", res.Error)
	}

	// Query
	stmts, err = parse.ParseSQL("SELECT * FROM t")
	if err != nil || len(stmts) == 0 {
		t.Fatalf("empty parse result: %v", err)
	}
	res = e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Select: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}
