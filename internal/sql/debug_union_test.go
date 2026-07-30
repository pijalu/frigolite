package sql

import (
	"fmt"
	"testing"
)

func TestDebugUnionOrderBy(t *testing.T) {
	query := "SELECT 456 UNION ALL SELECT 123 ORDER BY 1;"
	p := NewParser(query)
	stmts := p.Parse()
	if p.Err() != nil {
		t.Fatalf("parse error: %v", p.Err())
	}
	for _, stmt := range stmts {
		if s, ok := stmt.(*SelectStmt); ok {
			fmt.Printf("Top-level SelectStmt:\n")
			fmt.Printf("  Columns: %d\n", len(s.Columns))
			fmt.Printf("  OrderBy: %v (len=%d)\n", s.OrderBy, len(s.OrderBy))
			fmt.Printf("  SetOp: %v\n", s.SetOp)
			fmt.Printf("  UnionAll: %v\n", s.UnionAll)
			for i, ob := range s.OrderBy {
				fmt.Printf("  OrderBy[%d]: %T %+v\n", i, ob.Expr, ob.Expr)
			}
			if s.Union != nil {
				fmt.Printf("  Union Columns: %d\n", len(s.Union.Columns))
				fmt.Printf("  Union OrderBy: %v (len=%d)\n", s.Union.OrderBy, len(s.Union.OrderBy))
				fmt.Printf("  Union SetOp: %v\n", s.Union.SetOp)
				for i, ob := range s.Union.OrderBy {
					fmt.Printf("  Union OrderBy[%d]: %T %+v\n", i, ob.Expr, ob.Expr)
				}
			}
		}
	}
}
