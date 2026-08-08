package frigolite_test

import (
	"fmt"
	"testing"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
)

func TestDbgCTEAST(t *testing.T) {
	stmts, err := parse.ParseSQL(`WITH VVV AS (VALUES('a', 'b'), ('c', 'd'), (123, NULL)) SELECT * FROM VVV UNION ALL SELECT * FROM VVV`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, st := range stmts {
		sel, ok := st.(*sql.SelectStmt)
		if !ok {
			t.Fatalf("not select: %T", st)
		}
		fmt.Printf("outer From.Name=%q CTEs=%d Union=%v\n", sel.From.Name, len(sel.CTEs), sel.Union != nil)
		if sel.Union != nil {
			fmt.Printf("  union member From.Name=%q CTEs=%d ValuesChain=%v\n", sel.Union.From.Name, len(sel.Union.CTEs), sel.Union.ValuesChain)
		}
	}
}
