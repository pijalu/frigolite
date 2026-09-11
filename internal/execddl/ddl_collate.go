package execddl

import (
	sql "github.com/pijalu/frigolite/internal/sql"
)

// validateTableCollations rejects CREATE TABLE whose column-level COLLATE
// names (or table-level PRIMARY KEY/UNIQUE column collations) are not
// registered on this connection — build.c sqlite3AddCollateType resolves the
// collation at CREATE time, so an unknown name aborts the statement with
// "no such collation sequence: NAME" before any schema entry is written
// (collate3-1.2, collate7).
func (e *DDLExecutor) validateTableCollations(s *sql.CreateTableStmt) *Result {
	check := func(name string) *Result {
		if name == "" {
			return nil
		}
		if err := e.ctx.CheckCollationString(name); err != nil {
			return &Result{Error: err}
		}
		return nil
	}
	for i := range s.Columns {
		if res := check(s.Columns[i].Collate); res != nil {
			return res
		}
	}
	for _, tc := range s.Constraints {
		for _, ic := range tc.Columns {
			if res := check(ic.Collate); res != nil {
				return res
			}
		}
	}
	return nil
}
