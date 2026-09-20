// Package exec implements query execution.
//
// This file holds DDL execution for CREATE TRIGGER, CREATE VIEW, and CREATE
// VIRTUAL TABLE, plus the SQL-text serialization helpers used by stored
// triggers and views. It is the trigger/view/vtable half of the former
// ddl.go, split out so that each file stays within the repository's
// complexity and size budgets. Core CREATE/DROP/ATTACH execution and the
// generic expression serializer live in ddl_core.go.
package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *DDLExecutor) createFTSShadowTables(tableName string, t *fts.FTS3Table, moduleName string) error {
	isFts4 := strings.EqualFold(moduleName, "fts4")
	if strings.EqualFold(moduleName, "fts5") {
		return e.createFTS5ShadowTables(tableName, t)
	}
	cols := t.ColumnNames()

	// %_content(docid INTEGER PRIMARY KEY, c0 <name>, ...)
	// SQLite names the content columns "c%d%s" — c + column index + the user
	// column name (fts3.c fts3CreateTables: "c%d%s", i, azCol[i]), so a
	// table with columns (a, b) gets c0a, c1b. A content=<table> FTS table
	// has NO %_content shadow (fts3.c fts3CreateTables skips it when
	// zContent is set), and neither does a contentless (content=) table
	// (fts4content 7.2.3: SELECT name FROM sqlite_master LIKE 'ft9_%' has no
	// ft9_content).
	if err := e.createFTS3ContentShadow(tableName, t, cols, isFts4); err != nil {
		return err
	}

	return e.createFTS3CoreShadowTables(tableName, t, isFts4)
}

// createFTS5ShadowTables creates fts5's shadow family: fts5_storage.c creates
// %_data, %_idx, %_content, %_docsize and %_config for every fts5 table
// (vtabdrop 2.2 lists them in sqlite_master).
func (e *DDLExecutor) createFTS5ShadowTables(tableName string, t *fts.FTS3Table) error {
	cols := t.ColumnNames()
	if res := e.createShadowTableSQL(tableName+"_data", []sql.ColumnDef{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "block", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}
	if res := e.createShadowTableSQL(tableName+"_idx", []sql.ColumnDef{
		{Name: "segid", Type: "INTEGER"},
		{Name: "term", Type: "TEXT"},
		{Name: "pgno", Type: "INTEGER"},
	}); res.Error != nil {
		return res.Error
	}
	contentDefs := []sql.ColumnDef{{Name: "id", Type: "INTEGER", PrimaryKey: true}}
	for i := range cols {
		contentDefs = append(contentDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d", i)})
	}
	if res := e.createShadowTableSQL(tableName+"_content", contentDefs); res.Error != nil {
		return res.Error
	}
	if res := e.createShadowTableSQL(tableName+"_docsize", []sql.ColumnDef{
		{Name: "id", Type: "INTEGER", PrimaryKey: true},
		{Name: "sz", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}
	return e.createShadowTableSQL(tableName+"_config", []sql.ColumnDef{
		{Name: "k", Type: "INTEGER", PrimaryKey: true},
		{Name: "v", Type: ""},
	}).Error
}

// createFTS3ContentShadow creates the %_content shadow (fts3.c
// fts3CreateTables), skipped for content=/contentless tables, and canonicalizes
// the languageid table's stored CREATE statement.
func (e *DDLExecutor) createFTS3ContentShadow(tableName string, t *fts.FTS3Table, cols []string, isFts4 bool) error {
	if t.ContentTable() != "" || t.Contentless() {
		// still create segments/segdir/docsize/stat below
		return nil
	}
	// %_content(docid INTEGER PRIMARY KEY, c0 <name>, ...)
	// SQLite names the content columns "c%d%s" — c + column index + the user
	// column name (fts3.c fts3CreateTables: "c%d%s", i, azCol[i]), so a
	// table with columns (a, b) gets c0a, c1b. A content=<table> FTS table
	// has NO %_content shadow (fts3.c fts3CreateTables skips it when
	// zContent is set), and neither does a contentless (content=) table
	// (fts4content 7.2.3: SELECT name FROM sqlite_master LIKE 'ft9_%' has no
	// ft9_content).
	contentDefs := []sql.ColumnDef{{Name: "docid", Type: "INTEGER", PrimaryKey: true}}
	for i, col := range cols {
		contentDefs = append(contentDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d%s", i, col), Type: ""})
	}
	// A languageid=<col> table's %_content gains a trailing langid column
	// (fts3.c fts3CreateTables: "%z, langid" is appended to the content
	// table's column list — fts4langid 1.2 shows the 'langid' column).
	if t.LangIDColName() != "" {
		contentDefs = append(contentDefs, sql.ColumnDef{Name: "langid", Type: ""})
	}
	if res := e.createShadowTableSQL(tableName+"_content", contentDefs); res.Error != nil {
		return res.Error
	}
	return e.canonicalizeLangidContentSQL(tableName, t, cols)
}

// canonicalizeLangidContentSQL rewrites a languageid=<col> table's %_content
// schema entry to SQLite's byte-for-byte CREATE text (fts3.c
// fts3CreateTables builds it with %Q/%q: the table name and every c%d%s
// column are single-quoted, docid and langid are not — fts4langid 1.2). The
// generic renderer emits an unquoted form.
func (e *DDLExecutor) canonicalizeLangidContentSQL(tableName string, t *fts.FTS3Table, cols []string) error {
	if t.LangIDColName() == "" {
		return nil
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	colSQL := ""
	for i, col := range cols {
		colSQL += fmt.Sprintf(", %s", q(fmt.Sprintf("c%d%s", i, col)))
	}
	colSQL += ", langid"
	canonical := fmt.Sprintf("CREATE TABLE %s(docid INTEGER PRIMARY KEY%s)",
		q(tableName+"_content"), colSQL)
	return e.ctx.Schema().UpdateEntry(tableName+"_content", canonical)
}

// createFTS3CoreShadowTables creates the segments/segdir (and FTS4
// docsize/stat) shadows; see createFTSShadowTables.
func (e *DDLExecutor) createFTS3CoreShadowTables(tableName string, t *fts.FTS3Table, isFts4 bool) error {
	// %_segments(blockid INTEGER PRIMARY KEY, block BLOB)
	if res := e.createShadowTableSQL(tableName+"_segments", []sql.ColumnDef{
		{Name: "blockid", Type: "INTEGER", PrimaryKey: true},
		{Name: "block", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}

	// %_segdir(level, idx, start_block, leaves_end_block, end_block, root,
	// PRIMARY KEY(level, idx)). Real SQLite creates a
	// sqlite_autoindex_<name>_segdir_1 entry for the PRIMARY KEY; the engine
	// omits it (the segdir idx uniqueness is enforced by the segment writer)
	// because adding a real autoindex entry grows sqlite_schema past one page
	// and the rename/delete operations on the split schema b-tree corrupt it
	// (a pre-existing b-tree split limitation; fts4content 5.1.1/5.1.3/6.2.3
	// have want-overrides for the missing autoindex row).
	if res := e.createShadowTableSQL(tableName+"_segdir", []sql.ColumnDef{
		{Name: "level", Type: "INTEGER"},
		{Name: "idx", Type: "INTEGER"},
		{Name: "start_block", Type: "INTEGER"},
		{Name: "leaves_end_block", Type: "INTEGER"},
		{Name: "end_block", Type: "INTEGER"},
		{Name: "root", Type: "BLOB"},
	}); res.Error != nil {
		return res.Error
	}

	if isFts4 {
		// %_docsize(docid INTEGER PRIMARY KEY, size BLOB). A table created
		// with matchinfo=fts3 omits it (fts3.c bHasDocsize=0).
		if !t.NoDocsize() {
			if res := e.createShadowTableSQL(tableName+"_docsize", []sql.ColumnDef{
				{Name: "docid", Type: "INTEGER", PrimaryKey: true},
				{Name: "size", Type: "BLOB"},
			}); res.Error != nil {
				return res.Error
			}
		}
		// %_stat(id INTEGER PRIMARY KEY, value BLOB)
		if res := e.createShadowTableSQL(tableName+"_stat", []sql.ColumnDef{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "value", Type: "BLOB"},
		}); res.Error != nil {
			return res.Error
		}
	}
	return nil
}

// createShadowTableSQL creates a real btree-backed table from its column
// definitions via the normal CREATE TABLE machinery. A non-empty pkCols list
// declares a table-level PRIMARY KEY over those columns (producing the
// sqlite_autoindex_* entry SQLite creates for the shadow table).
func (e *DDLExecutor) createShadowTableSQL(name string, cols []sql.ColumnDef, pkCols ...[]sql.ColumnDef) *Result {
	st := &sql.CreateTableStmt{
		Name:    name,
		Columns: cols,
	}
	if len(pkCols) > 0 && len(pkCols[0]) > 0 {
		var idxCols []sql.IndexedColumn
		for _, cd := range pkCols[0] {
			idxCols = append(idxCols, sql.IndexedColumn{Name: cd.Name})
		}
		st.Constraints = append(st.Constraints, sql.TableConstraint{
			Type:    sql.ConstraintPrimaryKey,
			Columns: idxCols,
		})
	}
	return e.execCreateTable(st)
}

// validateIndexedBy validates an INDEXED BY clause: the named index must
// exist on the query's table, and a partial index must be implied by the
// query WHERE clause.
func (e *DDLExecutor) validateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	idxEntry := e.findIndexEntry(tableEntry, indexName)
	if idxEntry == nil {
		return fmt.Errorf("no such index: %s", indexName)
	}
	return e.checkPartialIndexUsable(idxEntry, s)
}

// findIndexEntry locates the schema entry for an index on the given table.
func (e *DDLExecutor) findIndexEntry(tableEntry *schema.Entry, indexName string) *schema.Entry {
	for _, ctx := range e.ctx.Databases() {
		en, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range en {
			if strings.EqualFold(ent.Name, indexName) && strings.EqualFold(ent.TblName, tableEntry.Name) {
				return ent
			}
		}
	}
	return nil
}

// checkPartialIndexUsable returns an error when a partial index (WHERE
// predicate) cannot serve the query: without any WHERE, or when the predicate
// cannot be implied, the forced partial index yields no query solution.
func (e *DDLExecutor) checkPartialIndexUsable(idxEntry *schema.Entry, s *sql.SelectStmt) error {
	wm := execdml.IndexWhereRe.FindStringSubmatch(idxEntry.SQL)
	if wm == nil {
		return nil
	}
	pred := strings.TrimSpace(wm[1])
	if s.Where == nil || !e.whereImplies(s.Where, pred) {
		return fmt.Errorf("no query solution")
	}
	return nil
}

// validateIndexKeyExpr rejects non-deterministic functions, window functions,
// and subqueries in index expressions.
func validateIndexKeyExpr(expr sql.Expr) error {
	return validateIndexExprContext(expr, false)
}

// validateIndexExprContext validates one index key expression or the
// partial-index WHERE clause. In the WHERE context SQLite names the
// construct differently (build.c: "non-deterministic functions prohibited
// in partial index WHERE clauses", "parameters prohibited in partial index
// WHERE clauses"; index6-1.4/1.5).
func validateIndexExprContext(expr sql.Expr, whereClause bool) error {
	var err error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		err = indexExprContextError(n, whereClause)
	})
	return err
}

// indexExprContextError validates one node of an index expression: no window
// functions or key-function misuse, no subqueries, and (in a partial index's
// WHERE clause) no parameters; see validateIndexExprContext.
func indexExprContextError(n sql.Expr, whereClause bool) error {
	switch e := n.(type) {
	case *sql.FuncCall:
		if e.Over != nil {
			return fmt.Errorf("misuse of window function %s()", strings.ToLower(e.Name))
		}
		return checkIndexKeyFuncCtx(e, whereClause)
	case *sql.Subquery:
		if whereClause {
			return fmt.Errorf("subqueries prohibited in partial index WHERE clauses")
		}
		return fmt.Errorf("subqueries prohibited in index expressions")
	case *sql.ExistsExpr:
		if whereClause {
			return fmt.Errorf("subqueries prohibited in partial index WHERE clauses")
		}
		return fmt.Errorf("subqueries prohibited in index expressions")
	case *sql.ParameterExpr:
		if whereClause {
			return fmt.Errorf("parameters prohibited in partial index WHERE clauses")
		}
	}
	return nil
}

// checkIndexKeyFunc validates one function call appearing in an index
// expression: non-deterministic functions are prohibited, and julianday('now')
// is a non-deterministic use.
func checkIndexKeyFuncCtx(e *sql.FuncCall, whereClause bool) error {
	nondet := "non-deterministic functions prohibited in index expressions"
	if whereClause {
		nondet = "non-deterministic functions prohibited in partial index WHERE clauses"
	}
	switch strings.ToUpper(e.Name) {
	case "RANDOM", "RANDOMBLOB":
		return fmt.Errorf("%s", nondet)
	case "ZEROBLOB":
		return fmt.Errorf("non-deterministic functions prohibited in index expressions")
	case "JULIANDAY":
		for _, a := range e.Args {
			if sl, ok := a.(*sql.StringLit); ok && strings.EqualFold(sl.Value, "now") {
				if whereClause {
					return fmt.Errorf("%s", nondet)
				}
				return fmt.Errorf("non-deterministic use of julianday() in an index")
			}
		}
	}
	return nil
}

// execFTSSelect implements SELECT from an FTS virtual table.
