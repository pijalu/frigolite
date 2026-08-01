package main

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/sql"
)

func main() {
	p := sql.NewParser("CREATE TABLE c1(c, d REFERENCES p1(b) ON DELETE CASCADE)")
	stmts := p.Parse()
	ct := stmts[0].(*sql.CreateTableStmt)
	for _, cd := range ct.Columns {
		fmt.Printf("col %s ref=%q\n", cd.Name, cd.References)
	}
}
