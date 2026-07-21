package exec

import (
	"testing"

	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/sql"
)

func TestExecCreateTable(t *testing.T) {
	pg := pager.OpenInMemory(pager.DefaultPageSize)
	e := NewEngine(pg)
	if err := e.schema.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	parser := sql.NewParser("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)")
	stmts := parser.Parse()
	if len(stmts) == 0 {
		t.Fatal("empty parse result")
	}

	res := e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Exec: %v", res.Error)
	}

	// Insert a row
	parser = sql.NewParser("INSERT INTO t VALUES (1, 'Alice')")
	stmts = parser.Parse()
	res = e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Insert: %v", res.Error)
	}

	// Query
	parser = sql.NewParser("SELECT * FROM t")
	stmts = parser.Parse()
	res = e.Exec(stmts[0])
	if res.Error != nil {
		t.Fatalf("Select: %v", res.Error)
	}
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}
