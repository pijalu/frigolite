package exec

import (
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
)

// This file forwards SELECT-family helpers that moved to internal/execquery.
// The execution engine keeps thin same-named wrappers so existing call sites
// (DDL validation, FK checks, trigger firing, RETURNING, PRAGMA) stay
// unchanged while the implementation lives in the query sub-package.

func isSQLiteSequence(name string) bool {
	return execquery.IsSQLiteSequence(name)
}

func isHiddenColumnDef(cd sql.ColumnDef) bool {
	return execquery.IsHiddenColumnDef(cd)
}

func isSchemaTable(name string) bool {
	return execquery.IsSchemaTable(name)
}

func pkColumnNames(createSQL string, colDefs []sql.ColumnDef) []string {
	return execquery.PKColumnNames(createSQL, colDefs)
}

// selectNeedsRowMaps adapts the Engine to the SelectEngine the query
// sub-package expects.
func selectNeedsRowMaps(e *Engine, s *sql.SelectStmt, tableName string) bool {
	return execquery.SelectNeedsRowMaps(e.selectEngine, s, tableName)
}

func viewDeclaredColumns(viewSQL string) []string {
	return execquery.ViewDeclaredColumns(viewSQL)
}
