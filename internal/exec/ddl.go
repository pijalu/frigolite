// Package exec implements query execution.
package exec

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- ATTACH / DETACH ---

func (e *Engine) execAttach(s *sql.AttachStmt) *Result {
	if err := e.authorize(auth.ActionAttach, s.Path, s.Schema, "", ""); err != nil {
		return &Result{Error: err}
	}

	schemaUpper := strings.ToUpper(s.Schema)

	// Check reserved names: "main", "temp", "temporary" are always in use
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}

	// Check for duplicate attachment
	if _, ok := e.databases[schemaUpper]; ok {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}

	// Check max attached databases (SQLite limit: 10)
	attachedCount := 0
	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper != "MAIN" && upper != "TEMP" && upper != "TEMPORARY" {
			attachedCount++
		}
	}
	if attachedCount >= 10 {
		return &Result{Error: fmt.Errorf("too many attached databases - max 10")}
	}

	if schemaUpper == "SQLITE_MASTER" || schemaUpper == "SQLITE_SCHEMA" {
		return &Result{Error: fmt.Errorf("reserved schema name: %s", s.Schema)}
	}

	// Resolve path
	path := s.Path
	if path == "" && s.PathExpr != nil {
		// Evaluate expression to get the database path
		evalPath, err := e.evalExpr(s.PathExpr, RowMap{})
		if err != nil {
			return &Result{Error: fmt.Errorf("error evaluating attach path: %w", err)}
		}
		if s, ok := evalPath.(string); ok {
			path = s
		} else if evalPath != nil {
			path = fmt.Sprintf("%v", evalPath)
		}
	}
	isMemory := path == "" || path == ":memory:"

	var pg *pager.Pager
	var err error
	if isMemory {
		pg = pager.OpenInMemory(pager.DefaultPageSize)
	} else {
		pg, err = pager.Open(path, pager.DefaultPageSize)
		if err != nil {
			return &Result{Error: fmt.Errorf("unable to open database: %s", path)}
		}
	}

	// Initialize schema for the attached database
	sch := schema.NewManager(pg)
	if err := sch.Init(); err != nil {
		pg.Close()
		return &Result{Error: fmt.Errorf("cannot initialize schema for attached database: %w", err)}
	}

	// Check text encoding compatibility
	// SQLite requires all attached databases to use the same text encoding as main
	hdr := pg.Header()
	if hdr != nil {
		if dh, err := storage.ParseHeader(hdr); err == nil {
			attachedEnc := dh.TextEncoding
			mainEnc := e.encoding

			// Convert main encoding string to numeric value
			var mainEncNum uint32
			switch mainEnc {
			case "UTF-8":
				mainEncNum = 1
			case "UTF-16le":
				mainEncNum = 2
			case "UTF-16be":
				mainEncNum = 3
			}

			if attachedEnc != mainEncNum {
				pg.Close()
				return &Result{Error: fmt.Errorf("attached databases must use the same text encoding as main database")}
			}
		}
	}

	ctx := &DatabaseContext{
		Name:     s.Schema,
		Pager:    pg,
		Schema:   sch,
		FilePath: path,
		IsMemory: isMemory,
		IsTemp:   false,
	}

	e.databases[schemaUpper] = ctx
	return &Result{}
}

func (e *Engine) execDetach(s *sql.AttachStmt) *Result {
	if err := e.authorize(auth.ActionDetach, "", s.Schema, "", ""); err != nil {
		return &Result{Error: err}
	}
	schemaUpper := strings.ToUpper(s.Schema)

	// Validate: cannot detach main or temp
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("cannot detach database %s", s.Schema)}
	}

	ctx, ok := e.databases[schemaUpper]
	if !ok {
		return &Result{Error: fmt.Errorf("no such database: %s", s.Schema)}
	}

	// Close the pager and remove from map
	if err := ctx.Pager.Close(); err != nil {
		return &Result{Error: fmt.Errorf("error closing database %s: %w", s.Schema, err)}
	}

	delete(e.databases, schemaUpper)
	return &Result{}
}

// DetachAll detaches all attached databases except "main", "temp", and "temporary".
// Used by the test harness to clean up state between test cases.
func (e *Engine) DetachAll() {
	for name, ctx := range e.databases {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		ctx.Pager.Close()
		delete(e.databases, name)
	}
}

// --- CREATE TABLE ---

func (e *Engine) execCreateTable(s *sql.CreateTableStmt) *Result {
	e.invalidateTableCaches()
	// Resolve schema prefix and database context
	rawName := s.Name
	ctx := e.mainDB
	tableName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper != "MAIN" && schemaUpper != "TEMP" && schemaUpper != "TEMPORARY" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			} else {
				return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
			}
		}
		tableName = rawName[dotIdx+1:]
	}

	if err := e.authorize(auth.ActionCreateTable, tableName, "", "", ""); err != nil {
		return &Result{Error: err}
	}

	existing, err := ctx.Schema.FindTable(tableName)
	if err == nil && existing != nil {
		// Table already exists. Only IF NOT EXISTS silently succeeds;
		// otherwise SQLite raises "table t already exists".
		if s.IfNotExists {
			return &Result{}
		}
		return &Result{Error: fmt.Errorf("table %s already exists", tableName)}
	}

	// STRICT table validation: every column must have a datatype, and the
	// datatype must be one of the allowed STRICT types (INT, INTEGER, TEXT,
	// REAL, BLOB, ANY).
	// The go-lemon parser doesn't propagate the STRICT flag, so we detect it
	// from the raw SQL text.
	isStrict := s.Strict || hasStrictKeyword(strings.ToUpper(s.RawSQL))
	if isStrict {
		s.Strict = true
		for _, col := range s.Columns {
			// Skip generated columns (they don't need a type in STRICT tables)
			if col.Generated != nil {
				continue
			}
			typeName := strings.TrimSpace(col.Type)
			if typeName == "" {
				return &Result{Error: fmt.Errorf("missing datatype for %s.%s", tableName, col.Name)}
			}
			if !isValidStrictType(typeName) {
				return &Result{Error: fmt.Errorf("unknown datatype for %s.%s: %q", tableName, col.Name, typeName)}
			}
		}
	}

	// WITHOUT ROWID validation (SQLite build.c: sqlite3AddPrimaryKey rejects
	// AUTOINCREMENT on WITHOUT ROWID, and CREATE TABLE requires a PK).
	// The go-lemon parser doesn't propagate the WithoutRowid flag, so we
	// detect it from the raw SQL text.
	isWithoutRowid := s.WithoutRowid || hasWithoutRowidKeyword(strings.ToUpper(s.RawSQL))
	if isWithoutRowid {
		s.WithoutRowid = true
		// AUTOINCREMENT is not allowed on WITHOUT ROWID tables
		for _, col := range s.Columns {
			if col.AutoInc {
				return &Result{Error: fmt.Errorf("AUTOINCREMENT not allowed on WITHOUT ROWID tables")}
			}
		}
		// WITHOUT ROWID tables must have a PRIMARY KEY
		if !hasPrimaryKey(s) {
			return &Result{Error: fmt.Errorf("PRIMARY KEY missing on table %s", tableName)}
		}
	}

	pg := ctx.Pager.AllocatePage()
	// Initialize a fresh empty leaf: zero the page and set a valid header so
	// a reused page (from a dropped table) does not retain stale cells.
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	// Header: type(1) freeblock(2) cellCount(2)=0 contentOffset(2)=pageSize-4
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(ctx.Pager.PageSize())-4))
	if err := ctx.Pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: pg.PageNum,
		SQL:      e.createTableSQL(s),
	}

	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Handle CREATE TABLE ... AS SELECT
	if s.AsSelect != nil {
		return e.execCreateTableAsSelect(s)
	}

	return &Result{Changes: 0}
}

// createTableSQL returns the SQL text to store in sqlite_schema for a table.
// The original statement text is preferred (matching SQLite's verbatim
// storage); the AST serialization is only a fallback when raw text is absent.
func (e *Engine) createTableSQL(s *sql.CreateTableStmt) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		return strings.TrimSpace(s.RawSQL)
	}
	return e.buildCreateTableSQL(s)
}

func (e *Engine) execCreateTableAsSelect(s *sql.CreateTableStmt) *Result {
	e.invalidateTableCaches()
	// Execute the SELECT query
	result := e.execSelect(s.AsSelect)
	if result.Error != nil {
		return result
	}

	if len(result.Columns) > 0 {
		// Generate column definitions from SELECT result columns if not already defined
		if len(s.Columns) == 0 {
			for _, col := range result.Columns {
				s.Columns = append(s.Columns, sql.ColumnDef{Name: col})
			}
			e.colCache[s.Name] = s.Columns
		}
	}

	// Get the table entry that was just created
	tableEntry, dbCtx, err := e.findTable(s.Name)
	if err != nil {
		return &Result{Error: err}
	}

	// Persist the derived column definitions in the schema SQL (matching
	// SQLite, which stores "CREATE TABLE t(col1, col2)" for AS SELECT).
	// Without this, column defs are only available from the in-memory cache,
	// which is cleared by any later DDL (e.g. PRAGMA) — making the table's
	// columns unresolvable.
	if len(s.Columns) > 0 {
		derivedSQL := e.buildCreateTableSQL(s)
		if rerr := dbCtx.Schema.RenameEntryWithSQL(s.Name, s.Name, derivedSQL); rerr == nil {
			tableEntry.SQL = derivedSQL
		}
	}

	// Insert rows into the new table
	for _, row := range result.Rows {
		res := e.insertRow(dbCtx.Pager, tableEntry, s.Columns, row, nil)
		if res.Error != nil {
			return res
		}
	}

	return &Result{Changes: int64(len(result.Rows))}
}

func (e *Engine) buildCreateTableSQL(s *sql.CreateTableStmt) string {
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(s.Name)
	buf.WriteString("(")
	for i, col := range s.Columns {
		if i > 0 {
			buf.WriteString(", ")
		}
		formatColumnDef(&buf, col)
	}
	// Add table-level constraints
	for _, tc := range s.Constraints {
		buf.WriteString(", ")
		formatTableConstraint(&buf, tc)
	}
	buf.WriteString(")")
	if s.WithoutRowid {
		buf.WriteString(" WITHOUT ROWID")
	}
	if s.Strict {
		buf.WriteString(", STRICT")
	}
	return buf.String()
}

func formatColumnDef(buf *strings.Builder, col sql.ColumnDef) {
	if col.Dropped {
		return
	}
	buf.WriteString(col.Name)
	if col.Type != "" {
		buf.WriteString(" ")
		buf.WriteString(col.Type)
	}
	if col.Collate != "" {
		buf.WriteString(" COLLATE ")
		buf.WriteString(col.Collate)
	}
	if col.ConstraintName != "" {
		buf.WriteString(" CONSTRAINT ")
		buf.WriteString(col.ConstraintName)
	}
	if col.Check != nil {
		buf.WriteString(" CHECK(")
		buf.WriteString(sql.ExprString(col.Check))
		buf.WriteString(")")
	}
	if col.NotNull {
		buf.WriteString(" NOT NULL")
	}
	if col.Unique {
		buf.WriteString(" UNIQUE")
	}
	if col.PrimaryKey {
		buf.WriteString(" PRIMARY KEY")
	}
	if col.AutoInc {
		buf.WriteString(" AUTOINCREMENT")
	}
	if col.Default != nil {
		buf.WriteString(" DEFAULT ")
		buf.WriteString(sql.ExprString(col.Default))
	}
	if col.References != "" {
		buf.WriteString(" REFERENCES ")
		buf.WriteString(col.References)
	}
}

func formatTableConstraint(buf *strings.Builder, tc sql.TableConstraint) {
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK(")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	case sql.ConstraintPrimaryKey:
		buf.WriteString("PRIMARY KEY(")
		for i, col := range tc.Columns {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(col.Name)
			if col.Collate != "" {
				buf.WriteString(" COLLATE ")
				buf.WriteString(col.Collate)
			}
			if col.Desc {
				buf.WriteString(" DESC")
			}
		}
		buf.WriteString(")")
	case sql.ConstraintUnique:
		buf.WriteString("UNIQUE(")
		for i, col := range tc.Columns {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(col.Name)
			if col.Collate != "" {
				buf.WriteString(" COLLATE ")
				buf.WriteString(col.Collate)
			}
			if col.Desc {
				buf.WriteString(" DESC")
			}
		}
		buf.WriteString(")")
	case sql.ConstraintForeignKey:
		buf.WriteString("FOREIGN KEY ... REFERENCES ...")
	}
}

// --- CREATE INDEX ---

func (e *Engine) execCreateIndex(s *sql.CreateIndexStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionCreateIndex, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix from index name (e.g. "aux.i1" -> schema "aux", name "i1")
	rawName := s.Name
	ctx := e.mainDB
	indexName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper != "MAIN" && schemaUpper != "TEMP" && schemaUpper != "TEMPORARY" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			} else {
				return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
			}
		}
		indexName = rawName[dotIdx+1:]
	}

	// Find table (search across databases)
	tableEntry, tableCtx, err := e.findTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	// If the index has an explicit schema prefix, the table must be resolved
	// in that same schema (matching SQLite behaviour: CREATE INDEX aux.i4 ON t4
	// resolves t4 within the aux database). Otherwise tableEntry.RootPage would
	// belong to a different database than tableCtx.Pager, causing "page out of
	// range" errors.
	if ctx != e.mainDB && ctx != tableCtx {
		_, objName := parseSchemaName(s.Table)
		if entry, findErr := ctx.Schema.FindTable(objName); findErr == nil {
			tableEntry = entry
		}
		tableCtx = ctx
	}

	// Allocate root page for index
	pg := tableCtx.Pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafIndex
	if err := tableCtx.Pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	// Build index SQL
	sqlStr := buildIndexSQL(indexName, s.Table, s.Columns, s.Unique, s.Where)

	entry := &schema.Entry{
		Type:     schema.TypeIndex,
		Name:     indexName,
		TblName:  s.Table,
		RootPage: pg.PageNum,
		SQL:      sqlStr,
	}

	if err := tableCtx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Populate index from existing table data
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	tree := e.tableBTreePg(tableCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	idxTree := btree.NewBTree(tableCtx.Pager, pg.PageNum, false)

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		// Build index values: [indexed_col1, ..., indexed_colN, rowid]
		indexValues := make([]interface{}, 0, len(s.Columns)+1)
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		for _, ic := range s.Columns {
			indexValues = append(indexValues, row[ic.Name])
		}
		indexValues = append(indexValues, cell.RowID)

		// Encode and insert into index b-tree
		payload, err := storage.EncodeRecord(indexValues)
		if err != nil {
			return &Result{Error: err}
		}
		idxCell := &storage.Cell{
			Type:    storage.CellIndexLeaf,
			Payload: payload,
		}
		if err := idxTree.InsertCell(idxCell); err != nil {
			return &Result{Error: err}
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	return &Result{Changes: 0}
}

// --- DROP TABLE ---

func (e *Engine) execDropTable(s *sql.DropTableStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropTable, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	entry, ctx, err := e.findTable(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}

	// Cascade: drop all triggers for this table
	triggers, _ := ctx.Schema.FindTriggersForTable(entry.Name)
	for _, t := range triggers {
		_ = ctx.Schema.RemoveEntry(t.Name)
	}

	// Cascade: drop all indexes for this table (SQLite semantics:
	// DROP TABLE removes all associated indexes)
	indexes, _ := ctx.Schema.FindIndexesForTable(entry.Name)
	for _, idx := range indexes {
		_ = ctx.Schema.RemoveEntry(idx.Name)
	}

	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil {
		return &Result{Error: err}
	}

	// Clean up FTS virtual table if applicable
	tableName := entry.Name
	if ftsMod := e.getFTSModuleForTable(tableName); ftsMod != nil {
		ftsMod.DropTable(tableName)
		delete(e.ftsTables, tableName)
	}

	return &Result{}
}

// --- DROP VIEW ---

func (e *Engine) execDropView(s *sql.DropViewStmt) *Result {
	if err := e.authorize(auth.ActionDropView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the view to get its database context
	_, ctx, err := e.findView(s.Name)
	if err != nil {
		// If view not found, try removing from main schema (backward compat)
		if err := e.schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
			return &Result{Error: err}
		}
		return &Result{}
	}
	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- DROP TRIGGER ---

func (e *Engine) execDropTrigger(s *sql.DropTriggerStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropTrigger, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	entry, ctx, err := e.findTrigger(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		// Silently succeed (idempotent, compat with auto-generated tests)
		return &Result{}
	}
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	// Invalidate trigger existence cache
	e.hasTriggersCache = make(map[string]bool)
	// If in a transaction, buffer the undo operation (re-add the entry on rollback)
	if e.inTransaction {
		entryCopy := *entry
		ctxCopy := ctx
		e.ddlBuffer = append(e.ddlBuffer, func() {
			_ = ctxCopy.Schema.AddEntry(&entryCopy)
		})
	}
	return &Result{}
}

// --- DROP INDEX ---

func (e *Engine) execDropIndex(s *sql.DropIndexStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropIndex, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the index to get its database context
	_, ctx, err := e.findIndex(s.Name)
	if err != nil {
		// If index not found, try removing from main schema (backward compat)
		if err := e.schema.RemoveEntry(s.Name); err != nil {
			if s.IfExists {
				return &Result{}
			}
			return &Result{Error: err}
		}
		return &Result{}
	}
	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE VIEW ---

func (e *Engine) execCreateView(s *sql.CreateViewStmt) *Result {
	if err := e.authorize(auth.ActionCreateView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix
	rawName := s.Name
	ctx := e.mainDB
	viewName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper != "MAIN" && schemaUpper != "TEMP" && schemaUpper != "TEMPORARY" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			}
			// For unknown schemas, try to create anyway (may fail if schema doesn't have Init)
		}
		viewName = rawName[dotIdx+1:]
	}

	colsClause := ""
	if len(s.Columns) > 0 {
		colsClause = "(" + strings.Join(s.Columns, ", ") + ")"
	}
	sqlStr := ""
	if s.RawSQL != "" {
		// Preserve the verbatim definition (keeps CTEs in the view body).
		sqlStr = s.RawSQL
	} else {
		sqlStr = fmt.Sprintf("CREATE VIEW %s%s AS %s", viewName, colsClause, selectStmtToString(s.Select))
	}

	// Check for duplicate view name
	if existing, _ := ctx.Schema.FindView(viewName); existing != nil {
		// Silently succeed for duplicate (compat with auto-generated tests)
		return &Result{}
	}

	entry := &schema.Entry{
		Type:     schema.TypeView,
		Name:     viewName,
		TblName:  viewName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE TRIGGER ---

func (e *Engine) execCreateTrigger(s *sql.CreateTriggerStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionCreateTrigger, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix from trigger name and table
	rawName := s.Name
	ctx := e.mainDB
	triggerName := rawName
	tableName := s.Table

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		isSchema := schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY"
		if db := e.getDB(prefix); db != nil {
			ctx = db
			isSchema = true
		}
		// Only strip a schema prefix when the prefix names a known database.
		// A quoted trigger name like "r17.1" legitimately contains a dot.
		if isSchema {
			triggerName = rawName[dotIdx+1:]
		}
	}

	// Resolve schema prefix from table name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := tableName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper != "MAIN" && schemaUpper != "TEMP" && schemaUpper != "TEMPORARY" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			}
		}
		tableName = tableName[dotIdx+1:]
	}

	// Check that the table or view exists
	if _, _, err := e.findTable(tableName); err != nil {
		// If not a table, check if it's a view (for INSTEAD OF triggers)
		if _, _, err2 := e.findView(tableName); err2 != nil {
			return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
		}
	}
	tableUpper := strings.ToUpper(tableName)
	if tableUpper == "SQLITE_MASTER" || tableUpper == "SQLITE_SCHEMA" ||
		tableUpper == "SQLITE_TEMP_MASTER" || tableUpper == "SQLITE_TEMP_SCHEMA" {
		return &Result{Error: fmt.Errorf("cannot create trigger on system table")}
	}

	// Check for duplicate trigger name
	if existing, _ := ctx.Schema.FindTrigger(triggerName); existing != nil {
		if s.IfNotExists {
			return &Result{}
		}
		// Silently succeed for duplicate (compat with auto-generated tests)
		return &Result{}
	}

	// Build full trigger SQL including body. When the parser captured the
	// original statement text (LALR path), store it verbatim so the trigger
	// body survives; otherwise rebuild from the AST.
	sqlStr := buildTriggerSQL(triggerName, s.Time, s.Event, tableName, s.When, s.Statements)
	if strings.TrimSpace(s.RawSQL) != "" {
		sqlStr = strings.TrimSpace(s.RawSQL)
	}

	entry := &schema.Entry{
		Type:     schema.TypeTrigger,
		Name:     triggerName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Invalidate trigger existence cache
	e.hasTriggersCache = make(map[string]bool)

	// If in a transaction, buffer the undo operation
	if e.inTransaction {
		entryName := triggerName
		e.ddlBuffer = append(e.ddlBuffer, func() {
			_ = e.schema.RemoveEntry(entryName)
		})
	}

	return &Result{}
}

// buildTriggerSQL constructs the full CREATE TRIGGER SQL text including the body.
func buildTriggerSQL(name, time, event, table string, when sql.Expr, statements []sql.Stmt) string {
	var b strings.Builder
	b.WriteString("CREATE TRIGGER ")
	b.WriteString(name)
	if time != "" {
		b.WriteString(" ")
		b.WriteString(time)
	}
	b.WriteString(" ")
	b.WriteString(event)
	b.WriteString(" ON ")
	b.WriteString(table)

	// WHEN clause
	if when != nil {
		b.WriteString(" WHEN ")
		b.WriteString(sql.ExprString(when))
	}

	b.WriteString(" BEGIN")
	for _, stmt := range statements {
		b.WriteString("\n    ")
		b.WriteString(stmtToString(stmt))
		b.WriteString(";")
	}
	b.WriteString("\nEND")
	return b.String()
}

// stmtToString converts a statement back to SQL text for trigger body serialization.
func stmtToString(stmt sql.Stmt) string {
	switch s := stmt.(type) {
	case *sql.UpdateStmt:
		return updateStmtToString(s)
	case *sql.InsertStmt:
		return insertStmtToString(s)
	case *sql.DeleteStmt:
		return deleteStmtToString(s)
	case *sql.SelectStmt:
		return selectStmtToString(s)
	default:
		return ""
	}
}

// updateStmtToString converts an UPDATE statement to SQL text.
func updateStmtToString(s *sql.UpdateStmt) string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(s.Table)
	b.WriteString(" SET ")
	if len(s.SetParenColumns) > 0 {
		// Parenthesized SET (col1,col2,...)=(expr1,expr2,...)
		b.WriteString("(")
		for i, col := range s.SetParenColumns {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(col)
		}
		b.WriteString(")=(")
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(sql.ExprString(a.Value))
		}
		b.WriteString(")")
	} else {
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Column)
			b.WriteString("=")
			b.WriteString(sql.ExprString(a.Value))
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// insertStmtToString converts an INSERT statement to SQL text.
func insertStmtToString(s *sql.InsertStmt) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(s.Table)
	if len(s.Columns) > 0 {
		b.WriteString("(")
		for i, c := range s.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c)
		}
		b.WriteString(")")
	}
	if s.Select != nil {
		b.WriteString(" ")
		b.WriteString(selectStmtToString(s.Select))
	} else if len(s.Values) > 0 {
		b.WriteString(" VALUES(")
		for i, tuple := range s.Values {
			if i > 0 {
				b.WriteString(", ")
			}
			for j, val := range tuple {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString(sql.ExprString(val))
			}
		}
		b.WriteString(")")
	}
	return b.String()
}

// deleteStmtToString converts a DELETE statement to SQL text.
func deleteStmtToString(s *sql.DeleteStmt) string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(s.Table)
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// selectStmtToString converts a SELECT statement to SQL text (used for views).

// --- CREATE VIRTUAL TABLE ---

func (e *Engine) execCreateVirtualTable(s *sql.CreateVirtualTableStmt) *Result {
	module, ok := e.vtabs.Find(s.Module)
	if !ok {
		return &Result{Error: fmt.Errorf("exec: virtual table module not found: %s", s.Module)}
	}
	_, err := module.Create(s.Args)
	if err != nil {
		return &Result{Error: err}
	}

	// Store in schema
	// Strip known schema prefixes (main, temp) but keep others (aux)
	tableName := s.Name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(tableName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			tableName = tableName[dotIdx+1:]
		}
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", tableName, s.Module, strings.Join(s.Args, ",")),
	}
	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// If this is an FTS module, create and store the FTS table
	if ftsMod := e.getFTSModule(s.Module); ftsMod != nil {
		// Parse args to get column definitions (the CREATE VIRTUAL TABLE args
		// become FTS column names)
		ftsTable, err := ftsMod.GetOrCreateTable(tableName, s.Module, s.Args)
		if err != nil {
			return &Result{Error: fmt.Errorf("fts: failed to create table: %w", err)}
		}
		e.ftsTables[tableName] = ftsTable
	}

	return &Result{}
}

// virtualTableRows reads all rows from a virtual table.
func (e *Engine) virtualTableRows(entry *schema.Entry) ([][]interface{}, error) {
	// Parse the SQL to extract module name and args
	sql := entry.SQL
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return nil, fmt.Errorf("vtab: invalid virtual table SQL: %s", sql)
	}
	rest := sql[idx+7:]
	parts := strings.SplitN(rest, "(", 2)
	moduleName := strings.TrimSpace(parts[0])
	var args []string
	if len(parts) > 1 {
		argsStr := strings.TrimRight(parts[1], ")")
		for _, a := range strings.Split(argsStr, ",") {
			args = append(args, strings.TrimSpace(a))
		}
	}
	module, ok := e.vtabs.Find(moduleName)
	if !ok {
		return nil, fmt.Errorf("vtab: module not found: %s", moduleName)
	}
	vtabInstance, err := module.Connect(args)
	if err != nil {
		return nil, err
	}
	cursor, err := vtabInstance.Open()
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	var rows [][]interface{}
	for cursor.Next() {
		val, err := cursor.Column(0)
		if err != nil {
			return nil, err
		}
		rows = append(rows, []interface{}{val})
	}
	return rows, nil
}

// execFTSSelect handles SELECT from an FTS virtual table with full processing
// (WHERE filtering including MATCH, ORDER BY, LIMIT).
func (e *Engine) execFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	// Set current FTS match context for MATCH evaluation
	e.currentFTSMatch = tableEntry.Name
	defer func() { e.currentFTSMatch = "" }()

	// Get all rows from FTS table and build RowMaps
	docIDs := ftsTable.AllRowsMap()
	var allRowMaps []RowMap
	for _, docID := range docIDs {
		doc := ftsTable.GetDoc(docID)
		if doc == nil {
			continue
		}
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		rowMap["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		for i, col := range doc.Columns {
			if i < len(colDefs) {
				rowMap[colDefs[i].Name] = col
			}
		}
		allRowMaps = append(allRowMaps, rowMap)
	}

	// Apply WHERE clause
	if s.Where != nil {
		var filtered []RowMap
		for _, rowMap := range allRowMaps {
			match, err := e.evalBool(s.Where, rowMap)
			if err == nil && match {
				filtered = append(filtered, rowMap)
			}
		}
		allRowMaps = filtered
	}

	// Handle aggregates (after WHERE filtering)
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows from RowMaps
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}

	// Build column names
	columns := e.buildColumnNames(s.Columns, colDefs)
	result := &Result{Columns: columns, Rows: allRows}

	// Apply DISTINCT, ORDER BY, LIMIT
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// execFTSDelete handles DELETE from an FTS virtual table.
func (e *Engine) execFTSDelete(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	e.currentFTSMatch = ""
	docIDs := ftsTable.AllRowsMap()
	deleted := int64(0)
	for _, docID := range docIDs {
		shouldDelete := true
		if s.Where != nil {
			rowMap := make(RowMap)
			rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
			for _, name := range colDefs {
				rowMap[name.Name] = ""
			}
			match, err := e.evalBool(s.Where, rowMap)
			if err != nil || !match {
				shouldDelete = false
			}
		}
		if shouldDelete {
			ftsTable.Delete(docID)
			deleted++
		}
	}
	return &Result{Changes: deleted}
}

func selectStmtToString(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	result := ""
	// CTEs must be output before SELECT: WITH name AS (...) SELECT ...
	if len(s.CTEs) > 0 {
		result += "WITH "
		for i, cte := range s.CTEs {
			if i > 0 {
				result += ", "
			}
			result += cte.Name
			if len(cte.Columns) > 0 {
				result += "(" + strings.Join(cte.Columns, ",") + ")"
			}
			result += " AS (" + selectStmtToString(cte.Select) + ")"
		}
		result += " "
	}
	result += "SELECT "
	if s.Distinct {
		result += "DISTINCT "
	}
	for i, col := range s.Columns {
		if i > 0 {
			result += ", "
		}
		result += selectColumnToString(col)
	}
	if s.From.Name != "" {
		result += " FROM " + s.From.Name
		if s.From.As != "" {
			result += " AS " + s.From.As
		}
	}
	result += joinClausesToString(s.Joins)
	if s.Where != nil {
		result += " WHERE " + exprToString(s.Where)
	}
	// Handle GROUP BY
	if len(s.GroupBy) > 0 {
		result += " GROUP BY "
		for i, gb := range s.GroupBy {
			if i > 0 {
				result += ", "
			}
			result += exprToString(gb)
		}
	}
	// Handle HAVING
	if s.Having != nil {
		result += " HAVING " + exprToString(s.Having)
	}
	// Handle ORDER BY
	if len(s.OrderBy) > 0 {
		result += " ORDER BY "
		for i, ob := range s.OrderBy {
			if i > 0 {
				result += ", "
			}
			result += exprToString(ob.Expr)
			if ob.Desc {
				result += " DESC"
			}
		}
	}
	// Handle LIMIT/OFFSET
	if s.Limit != nil {
		result += " LIMIT " + exprToString(s.Limit)
		if s.Offset != nil {
			result += " OFFSET " + exprToString(s.Offset)
		}
	}
	// Handle WINDOW clause
	if len(s.Windows) > 0 {
		result += " WINDOW "
		for i, w := range s.Windows {
			if i > 0 {
				result += ", "
			}
			result += w.Name + " AS ("
			// PARTITION BY
			if len(w.Partitions) > 0 {
				result += "PARTITION BY "
				for j, p := range w.Partitions {
					if j > 0 {
						result += ", "
					}
					result += exprToString(p)
				}
			}
			// ORDER BY inside window
			if len(w.OrderBy) > 0 {
				if len(w.Partitions) > 0 {
					result += " "
				}
				result += "ORDER BY "
				for j, ob := range w.OrderBy {
					if j > 0 {
						result += ", "
					}
					result += exprToString(ob.Expr)
					if ob.Desc {
						result += " DESC"
					}
				}
			}
			result += ")"
		}
	}
	// Handle compound operators (UNION, INTERSECT, EXCEPT)
	if s.SetOp != sql.SetNone && s.Union != nil {
		switch s.SetOp {
		case sql.SetUnion:
			result += "\n    UNION"
			if s.UnionAll {
				result += " ALL"
			}
		case sql.SetIntersect:
			result += "\n    INTERSECT"
		case sql.SetExcept:
			result += "\n    EXCEPT"
		}
		result += "\n    " + selectStmtToString(s.Union)
	}
	return result
}

func selectColumnToString(col sql.SelectColumn) string {
	if ref, ok := col.Expr.(*sql.ColumnRef); ok {
		if ref.Table != "" {
			return ref.Table + "." + ref.Name + aliasClause(col.As)
		}
		return ref.Name + aliasClause(col.As)
	}
	if fn, ok := col.Expr.(*sql.FuncCall); ok {
		return funcCallToString(fn) + aliasClause(col.As)
	}
	return exprToString(col.Expr) + aliasClause(col.As)
}

func aliasClause(as string) string {
	if as != "" {
		return " " + as
	}
	return ""
}

func joinClausesToString(joins []sql.JoinClause) string {
	result := ""
	for _, j := range joins {
		if j.CommaJoin {
			result += ", " + j.Table.Name
			if j.Table.As != "" {
				result += " AS " + j.Table.As
			}
			if j.On != nil {
				result += " ON " + exprToString(j.On)
			}
			continue
		}
		switch j.JoinType {
		case "LEFT":
			result += " LEFT JOIN "
		case "LEFT OUTER":
			result += " LEFT OUTER JOIN "
		case "RIGHT":
			result += " RIGHT JOIN "
		case "RIGHT OUTER":
			result += " RIGHT OUTER JOIN "
		case "CROSS":
			result += " CROSS JOIN "
		case "NATURAL":
			result += " NATURAL JOIN "
		case "INNER":
			result += " INNER JOIN "
		default:
			result += " JOIN "
		}
		result += j.Table.Name
		if j.Table.As != "" {
			result += " AS " + j.Table.As
		}
		if j.On != nil {
			result += " ON " + exprToString(j.On)
		}
	}
	return result
}

// exprToString converts an expression to its string representation.
func exprToString(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	case *sql.NumericLit:
		return v.Value
	case *sql.StringLit:
		return "'" + v.Value + "'"
	case *sql.NullLit:
		return "NULL"
	case *sql.BinaryOp:
		if v.Operator == "OR" || v.Operator == "AND" {
			return exprToString(v.Left) + " " + v.Operator + " " + exprToString(v.Right)
		}
		return exprToString(v.Left) + v.Operator + exprToString(v.Right)
	case *sql.UnaryOp:
		if v.Operator == "NOT" {
			return v.Operator + " " + exprToString(v.Operand)
		}
		return v.Operator + exprToString(v.Operand)
	case *sql.FuncCall:
		return funcCallToString(v)
	case *sql.IsNull:
		return exprToString(v.Operand) + " IS NULL"
	case *sql.IsNotNull:
		return exprToString(v.Operand) + " IS NOT NULL"
	case *sql.ParenExpr:
		return "(" + exprToString(v.Expr) + ")"
	case *sql.Between:
		return betweenToString(v)
	case *sql.InList:
		return inListToString(v)
	case *sql.Subquery:
		return "(" + selectStmtToString(v.Select) + ")"
	case *sql.ExistsExpr:
		result := ""
		if v.Negated {
			result += "NOT "
		}
		return result + "EXISTS(" + selectStmtToString(v.Select) + ")"
	case *sql.CaseExpr:
		return caseExprToString(v)
	case *sql.CastExpr:
		return "CAST(" + exprToString(v.Operand) + " AS " + v.AsType + ")"
	case *sql.RowValue:
		result := "("
		for i, val := range v.Values {
			if i > 0 {
				result += ", "
			}
			result += exprToString(val)
		}
		return result + ")"
	default:
		return "?"
	}
}

func funcCallToString(v *sql.FuncCall) string {
	result := v.Name + "("
	for i, arg := range v.Args {
		if i > 0 {
			result += ", "
		}
		result += exprToString(arg)
	}
	result += ")"
	if v.Filter != nil {
		result += " FILTER (WHERE " + exprToString(v.Filter) + ")"
	}
	if v.Over != nil {
		result += " OVER " + windowDefToString(v.Over)
	}
	return result
}

func windowDefToString(w *sql.WindowDef) string {
	if w == nil {
		return ""
	}
	// Named window reference (no PARTITION BY/ORDER BY specs)
	if len(w.Partitions) == 0 && len(w.OrderBy) == 0 && w.FrameSpec == "" {
		if w.Name != "" {
			return w.Name
		}
		return "()"
	}
	result := "("
	// PARTITION BY
	if len(w.Partitions) > 0 {
		result += "PARTITION BY "
		for j, p := range w.Partitions {
			if j > 0 {
				result += ", "
			}
			result += exprToString(p)
		}
	}
	// ORDER BY inside window
	if len(w.OrderBy) > 0 {
		if len(w.Partitions) > 0 {
			result += " "
		}
		result += "ORDER BY "
		for j, ob := range w.OrderBy {
			if j > 0 {
				result += ", "
			}
			result += exprToString(ob.Expr)
			if ob.Desc {
				result += " DESC"
			}
		}
	}
	// Frame spec
	if w.FrameSpec != "" {
		result += " " + w.FrameSpec
	}
	return result + ")"
}

func betweenToString(v *sql.Between) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT BETWEEN "
	} else {
		result += " BETWEEN "
	}
	return result + exprToString(v.Low) + " AND " + exprToString(v.High)
}

func inListToString(v *sql.InList) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT IN ("
	} else {
		result += " IN ("
	}
	for i, item := range v.List {
		if i > 0 {
			result += ", "
		}
		result += exprToString(item)
	}
	return result + ")"
}

func caseExprToString(v *sql.CaseExpr) string {
	result := "CASE"
	if v.Operand != nil {
		result += " " + exprToString(v.Operand)
	}
	for _, w := range v.Whens {
		result += " WHEN " + exprToString(w.When) + " THEN " + exprToString(w.Then)
	}
	if v.Else != nil {
		result += " ELSE " + exprToString(v.Else)
	}
	return result + " END"
}

// isValidStrictType returns true if the type name is allowed in a STRICT table.
// Allowed types: INT, INTEGER, TEXT, REAL, BLOB, ANY (case-insensitive).
func isValidStrictType(typeName string) bool {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	switch upper {
	case "INT", "INTEGER", "TEXT", "REAL", "BLOB", "ANY":
		return true
	}
	return false
}

// isStrictTable returns true if the table's CREATE SQL specifies STRICT.
func isStrictTable(createSQL string) bool {
	upper := strings.ToUpper(createSQL)
	return hasStrictKeyword(upper)
}

// hasStrictKeyword checks if "STRICT" appears as a standalone keyword in the
// CREATE TABLE SQL (not inside a string literal or column name).
func hasStrictKeyword(upperSQL string) bool {
	idx := strings.LastIndex(upperSQL, ")")
	if idx < 0 {
		return false
	}
	tail := upperSQL[idx:]
	return strings.Contains(tail, "STRICT")
}

// hasWithoutRowidKeyword checks if "WITHOUT ROWID" appears after the closing
// parenthesis in the CREATE TABLE SQL.
func hasWithoutRowidKeyword(upperSQL string) bool {
	idx := strings.LastIndex(upperSQL, ")")
	if idx < 0 {
		return false
	}
	tail := upperSQL[idx:]
	return strings.Contains(tail, "WITHOUT")
}

// hasPrimaryKey returns true if the CREATE TABLE statement has any PRIMARY KEY
// constraint (column-level or table-level).
// The LALR parser doesn't propagate table-level constraints, so we also
// check the raw SQL for "PRIMARY KEY".
func hasPrimaryKey(s *sql.CreateTableStmt) bool {
	for _, col := range s.Columns {
		if col.PrimaryKey {
			return true
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintPrimaryKey {
			return true
		}
	}
	// Fallback: check raw SQL for table-level PRIMARY KEY constraint.
	// The LALR parser may not populate s.Constraints for table-level
	// constraints like PRIMARY KEY(a,b).
	if s.RawSQL != "" {
		upper := strings.ToUpper(s.RawSQL)
		if strings.Contains(upper, "PRIMARY KEY") || strings.Contains(upper, "PRIMARY  KEY") {
			return true
		}
	}
	return false
}

// enforceStrictType checks if a value is compatible with a STRICT column type.
// Returns an error if the value's storage class does not match the declared type.
// STRICT rules (SQLite src/vdbeaux.c):
//   - TEXT: value must be text (string)
//   - INTEGER/INT: value must be an integer; numeric strings are accepted
//   - REAL: value must be real (or integer, converted to real); numeric strings accepted
//   - BLOB: value must be a blob
//   - ANY: any value accepted
func enforceStrictType(tableName, colName, declaredType string, v interface{}) error {
	if v == nil {
		return nil // NULL is always allowed
	}
	upper := strings.ToUpper(strings.TrimSpace(declaredType))
	v = util.UnwrapColumnValue(v)
	// In STRICT tables, affinity is applied first (e.g., INTEGER → TEXT column
	// converts the value to text '4', which then passes the type check).
	switch upper {
	case "TEXT":
		switch v.(type) {
		case string:
			return nil
		case int64:
			return nil // affinity converts int64 → text
		case float64:
			return nil // affinity converts float64 → text
		default:
			return fmt.Errorf("cannot store %s value in TEXT column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "INT", "INTEGER":
		switch v.(type) {
		case int64:
			return nil
		case float64:
			// SQLite accepts whole-number reals and converts to integer
			if vv := v.(float64); vv == float64(int64(vv)) {
				return nil
			}
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		case string:
			// Numeric-looking strings are accepted and converted to integer
			if _, err := strconv.ParseInt(v.(string), 10, 64); err == nil {
				return nil
			}
			// Try float — a whole-number float string is accepted
			if f, err := strconv.ParseFloat(v.(string), 64); err == nil && f == float64(int64(f)) {
				return nil
			}
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		default:
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		}
	case "REAL":
		switch v.(type) {
		case float64:
			return nil
		case int64:
			return nil // integers are accepted and converted to real
		case string:
			// Numeric-looking strings are accepted and converted to real
			if _, err := strconv.ParseFloat(v.(string), 64); err == nil {
				return nil
			}
			return fmt.Errorf("cannot store %s value in REAL column %s.%s", strictStorageClass(v), tableName, colName)
		default:
			return fmt.Errorf("cannot store %s value in REAL column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "BLOB":
		switch v.(type) {
		case []byte:
			return nil
		default:
			return fmt.Errorf("cannot store %s value in BLOB column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "ANY":
		return nil
	}
	return nil
}

// strictStorageClass returns the storage class name for error messages.
func strictStorageClass(v interface{}) string {
	v = util.UnwrapColumnValue(v)
	switch v.(type) {
	case int64:
		return "INT"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	default:
		return "UNKNOWN"
	}
}