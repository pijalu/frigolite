package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execddl"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// tableConstraints parses and caches the table-level constraints of a CREATE
// TABLE statement. This is a schema-parsing helper (not expression
// evaluation): the parse cache lives with the engine's schema state. The cache
// is keyed by table name + SQL (like parseColumnDefs) so a DDL rewrite that
// changes the SQL (e.g. ALTER TABLE RENAME COLUMN rewriting a FOREIGN KEY
// clause) does not serve stale constraints.
func (e *Engine) tableConstraints(tableName, createSQL string) []sql.TableConstraint {
	if e.caches.tcCache == nil {
		e.caches.tcCache = make(map[string][]sql.TableConstraint)
	}
	cacheKey := tableName + "\x00" + createSQL
	if cached, ok := e.caches.tcCache[cacheKey]; ok {
		return cached
	}
	stmts, perr := parse.ParseSQLSchema(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	e.caches.tcCache[cacheKey] = ct.Constraints
	return ct.Constraints
}

func (e *Engine) parseColumnDefs(tableName, createSQL string) []sql.ColumnDef {
	// Check cache first. Keyed by table name + SQL so tables with the same
	// short name in different schemas (main.t1 vs aux.t1) do not collide.
	cacheKey := tableName + "\x00" + createSQL
	if cached, ok := e.caches.colCache[cacheKey]; ok {
		return cached
	}
	// Fall back to re-parsing (schema-reload mode: accepts COLLATE/ASC-DESC
	// on FK column lists that writable_schema may have stored).
	stmts, perr := parse.ParseSQLSchema(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if ok && ct != nil && len(ct.Columns) > 0 {
		// Trim generation keywords the go-lemon grammar accumulated into a
		// generated column's type name (e.g. "int generated always" → "int").
		for i := range ct.Columns {
			cd := &ct.Columns[i]
			if cd.Generated != nil {
				cd.Type = execddl.TrimGenerationType(cd.Type)
			}
		}
		// Cache for future use
		e.caches.colCache[cacheKey] = ct.Columns
		return ct.Columns
	}
	// CREATE VIRTUAL TABLE t1 USING module(a, b, c): the module arguments are
	// the virtual table's column names. FTS tables report their real column
	// names through the FTS table instance; other virtual tables use the raw
	// argument list (see vtabColumnDefs).
	// For virtual tables, check if we have an FTS table registered first: an
	// FTS table's real column names win over the raw module argument list.
	if ftsTable, ok := e.ftsTables[tableName]; ok {
		// A content=<table> table whose content table was missing at
		// connection time has no derived columns yet; re-derive them now
		// that the content table exists (fts4content 6.2.5: CREATE TABLE
		// t7(x, y) after a reopen with t7 dropped, then SELECT * FROM ft7
		// returns the x/y values). Guard against a content table that is the
		// FTS table itself (CREATE VIRTUAL TABLE t1 USING fts4(content=t1))
		// or a chain that cycles: only re-derive when the FTS table has no
		// columns, the content table is a different table, and the content
		// table is not itself an FTS table being resolved.
		if len(ftsTable.ColumnNames()) == 0 && ftsTable.ContentTable() != "" && !ftsTable.Contentless() &&
			!strings.EqualFold(ftsTable.ContentTable(), tableName) {
			if ctEntry, _, cerr := e.FindTable(ftsTable.ContentTable()); cerr == nil && ctEntry != nil {
				ctDefs := e.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
				var names []string
				for _, cd := range ctDefs {
					if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
						continue
					}
					names = append(names, cd.Name)
				}
				ftsTable.SetColumnNames(names)
			}
		}
		colDefs := make([]sql.ColumnDef, len(ftsTable.ColumnNames()))
		for i, name := range ftsTable.ColumnNames() {
			colDefs[i] = sql.ColumnDef{Name: name, Type: ""}
		}
		// SQLite's FTS3/4/5 modules expose extra hidden columns after the
		// user columns (xColumnCount = nColumn+1 in the C API, plus docid):
		// a column named after the table itself (used for "t4 MATCH 'b'"
		// full-table match expressions and special per-table functions such
		// as INSERT INTO t4(t4) VALUES(...)) and "docid" (an alias for
		// rowid). Hidden columns are excluded from * expansion and PRAGMA
		// table_info but readable by explicit references.
		colDefs = append(colDefs,
			sql.ColumnDef{Name: tableName, Type: "", Hidden: true},
			sql.ColumnDef{Name: "docid", Type: "", Hidden: true})
		// The languageid=<col> option adds another hidden column named by
		// the option's value (fts3.c fts3DeclareVtab appends
		// ", %Q HIDDEN" for the languageid column). Its value is the
		// document's stored language id (fts4langid 1.4: SELECT lang_id).
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			colDefs = append(colDefs, sql.ColumnDef{Name: langCol, Type: "", Hidden: true})
		}
		e.caches.colCache[tableName] = colDefs
		return colDefs
	}
	if colDefs := e.vtabColumnDefs(tableName, stmts[0]); colDefs != nil {
		return colDefs
	}
	return nil
}

// vtabColumnDefs resolves column definitions for a CREATE VIRTUAL TABLE
// statement: the echo module mirrors its underlying table, modules that
// declare column names provide real definitions, and other modules use the
// raw argument list. Returns nil when the statement is not a virtual table
// or no columns could be determined.
func (e *Engine) vtabColumnDefs(tableName string, stmt sql.Stmt) []sql.ColumnDef {
	vt, ok := stmt.(*sql.CreateVirtualTableStmt)
	if !ok || vt == nil {
		return nil
	}
	// The echo module mirrors its underlying table: the vtab's columns
	// are the source table's columns (with HIDDEN columns flagged).
	if strings.EqualFold(vt.Module, "echo") && len(vt.Args) > 0 {
		if colDefs := e.echoColumnDefs(tableName, vt.Args[0]); colDefs != nil {
			return colDefs
		}
	}
	// A module that declares column names (e.g. generate_series' "value"
	// column) provides the real column definitions; the CREATE VIRTUAL
	// TABLE arguments are module parameters, not column names.
	if colDefs := e.moduleColumnDefs(tableName, vt.Module, vt.Args); colDefs != nil {
		return colDefs
	}
	var colDefs []sql.ColumnDef
	for _, arg := range vt.Args {
		if arg == "" {
			continue
		}
		colDefs = append(colDefs, sql.ColumnDef{Name: arg, Type: ""})
	}
	if len(colDefs) > 0 {
		e.caches.colCache[tableName] = colDefs
		return colDefs
	}
	return nil
}

// echoColumnDefs resolves column definitions for an echo virtual table by
// mirroring its underlying source table (with HIDDEN columns flagged).
func (e *Engine) echoColumnDefs(tableName, srcArg string) []sql.ColumnDef {
	srcName := strings.Trim(srcArg, "'\"")
	srcEntry, _, ferr := e.findTable(srcName)
	if ferr != nil {
		return nil
	}
	srcDefs := e.parseColumnDefs(srcEntry.Name, srcEntry.SQL)
	// Deep-copy so mutating the Hidden flag does not corrupt the source
	// table's cached column definitions.
	colDefs := make([]sql.ColumnDef, len(srcDefs))
	copy(colDefs, srcDefs)
	for i := range colDefs {
		// Apply the virtual-table HIDDEN rule: a standalone "hidden" word
		// in the column type flags the column as hidden and is stripped
		// from the declared type.
		if typ, hidden := stripHiddenToken(colDefs[i].Type); hidden {
			colDefs[i].Type = typ
			colDefs[i].Hidden = true
		}
	}
	e.caches.colCache[tableName] = colDefs
	return colDefs
}

// moduleColumnDefs resolves column definitions from a virtual-table module
// that declares column names via the ColumnInfo interface.
func (e *Engine) moduleColumnDefs(tableName, moduleName string, args []string) []sql.ColumnDef {
	module, found := e.vtabs.Find(moduleName)
	if !found {
		return nil
	}
	inst, cerr := module.Connect(args)
	if cerr != nil {
		return nil
	}
	ci, ok := inst.(vtab.ColumnInfo)
	if !ok {
		return nil
	}
	// A module that also declares column types (ColumnTypeInfo) provides the
	// declared types so the column affinities drive comparisons (e.g.
	// fts3tokenize's `input = 123` converts the text to numeric).
	var types []string
	if cti, ok := inst.(vtab.ColumnTypeInfo); ok {
		types = cti.ColumnTypes()
	}
	var colDefs []sql.ColumnDef
	var hidden map[int]bool
	if hc, ok := inst.(vtab.HiddenColumnInfo); ok {
		hidden = hc.HiddenColumns()
	}
	var pk map[int]bool
	if pki, ok := inst.(vtab.PrimaryKeyInfo); ok {
		pk = pki.PrimaryKeyColumns()
	}
	// WITHOUT ROWID vtabs report their PRIMARY KEY columns as NOT NULL
	// (sqlite3 table_info semantics for non-IPK primary keys).
	var withoutRowid bool
	if wr, ok := inst.(interface{ WithoutRowid() bool }); ok {
		withoutRowid = wr.WithoutRowid()
	}
	for i, name := range ci.Columns() {
		if hidden[i] {
			continue
		}
		typ := ""
		if i < len(types) {
			typ = types[i]
		}
		cd := sql.ColumnDef{Name: name, Type: typ, PrimaryKey: pk[i]}
		if withoutRowid && pk[i] {
			cd.NotNull = true
		}
		colDefs = append(colDefs, cd)
	}
	e.caches.colCache[tableName] = colDefs
	return colDefs
}
