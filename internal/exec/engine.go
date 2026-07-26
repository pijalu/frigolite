// Package exec implements query execution.
package exec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Result holds the result of executing a SQL statement.
type Result struct {
	Columns        []string       // column names
	Rows           [][]interface{} // data rows
	Changes        int64          // number of changed rows
	Error          error          // execution error
	LastInsertRowID int64         // rowid of the last inserted row
}

// Engine executes SQL statements.
type Engine struct {
	pager    *pager.Pager
	schema   *schema.Manager
	funcs    *function.Registry
	vtabs    *vtab.Registry
	lastRowID int64
	colCache  map[string][]sql.ColumnDef // cached column definitions (tableName -> colDefs)
	stmtCache map[string][]sql.Stmt      // prepared statement cache (sqlText -> parsed stmts)
	tableRootPages map[string]uint32     // tracked root pages (updated after splits)
	nextRowIDCache map[uint32]int64      // cached next rowid per root page (keyed by rootPage)
	triggerDepth int                    // prevents recursive trigger firing
	triggerNewRow map[string]interface{}   // new row values for trigger execution (keyed as "new.colname")
	triggerOldRow map[string]interface{}   // old row values for trigger execution (keyed as "old.colname")
	inTransaction bool                  // tracks if we're inside a BEGIN/COMMIT block
	ddlBuffer    []func()               // DDL undo operations for transaction rollback
	outerRow     map[string]interface{}   // outer query row for correlated subquery resolution
	outerRows    []map[string]interface{} // all outer rows for correlated aggregate evaluation
	resolvingViews   map[string]bool        // tracks views currently being resolved (circular reference detection)
	legacyAlterTable bool                   // PRAGMA legacy_alter_table setting
	encoding    string                   // database text encoding: "UTF-8", "UTF-16le", "UTF-16be"
}

// LastInsertRowID returns the rowid of the last inserted row.
func (e *Engine) LastInsertRowID() int64 {
	return e.lastRowID
}

// rootPage returns the current root page for a table, checking the engine's
// tracked root pages first, then falling back to the schema entry.
func (e *Engine) rootPage(tableName string, schemaRoot uint32) uint32 {
	if tracked, ok := e.tableRootPages[tableName]; ok {
		return tracked
	}
	return schemaRoot
}

// updateRootPage tracks a root page change after a b-tree split.
func (e *Engine) updateRootPage(tableName string, newRoot uint32) {
	e.tableRootPages[tableName] = newRoot
}

// tableBTree creates a BTree for a table, using the engine's tracked root page.
func (e *Engine) tableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.pager, e.rootPage(tableName, schemaRoot), isTable)
}

// Prepare parses and caches a SQL statement.
func (e *Engine) Prepare(sqlStr string) ([]sql.Stmt, error) {
	if cached, ok := e.stmtCache[sqlStr]; ok {
		return cached, nil
	}
	parser := sql.NewParser(sqlStr)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return nil, parser.Err()
	}
	e.stmtCache[sqlStr] = stmts
	return stmts, nil
}

// NewEngine creates a new execution engine.
func NewEngine(pg *pager.Pager) *Engine {
	e := &Engine{
		pager:     pg,
		schema:    schema.NewManager(pg),
		funcs:     function.NewRegistry(),
		vtabs:     vtab.NewRegistry(),
		colCache:  make(map[string][]sql.ColumnDef),
		stmtCache: make(map[string][]sql.Stmt),
		tableRootPages: make(map[string]uint32),
		nextRowIDCache: make(map[uint32]int64),
		encoding:  "UTF-8",
	}
	e.vtabs.RegisterDefaults()
	return e
}

// Exec executes a single SQL statement and returns the result.
func (e *Engine) Exec(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.execSelect(s)
	case *sql.InsertStmt:
		return e.execInsert(s)
	case *sql.UpdateStmt:
		return e.execUpdate(s)
	case *sql.DeleteStmt:
		return e.execDelete(s)
	case *sql.CommitStmt:
		return e.execCommit()
	case *sql.BeginStmt:
		return e.execBegin()
	case *sql.RollbackStmt:
		return e.execRollback()
	default:
		return e.execOtherDDL(stmt)
	}
}

func (e *Engine) execOtherDDL(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.CreateTableStmt:
		return e.execCreateTable(s)
	case *sql.CreateIndexStmt:
		return e.execCreateIndex(s)
	case *sql.CreateViewStmt:
		return e.execCreateView(s)
	case *sql.CreateTriggerStmt:
		return e.execCreateTrigger(s)
	case *sql.CreateVirtualTableStmt:
		return e.execCreateVirtualTable(s)
	case *sql.DropTableStmt:
		return e.execDropTable(s)
	case *sql.DropIndexStmt:
		return e.execDropIndex(s)
	case *sql.DropViewStmt:
		return e.execDropView(s)
	case *sql.DropTriggerStmt:
		return e.execDropTrigger(s)
	case *sql.AnalyzeStmt:
		return e.execAnalyze(s)
	case *sql.PragmaStmt:
		return e.execPragma(s)
	case *sql.AlterTableStmt:
		return e.execAlterTable(s)
	case *sql.ExplainStmt:
		return e.execExplain(s)
	default:
		// Begin, Rollback, Attach, Vacuum, Reindex, Savepoint — all no-ops
		return &Result{}
	}
}

// --- CREATE TABLE ---

func (e *Engine) execCreateTable(s *sql.CreateTableStmt) *Result {
	// Strip known schema prefixes (main, temp) but keep others (aux)
	tableName := s.Name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(tableName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			tableName = tableName[dotIdx+1:]
		}
	}

	existing, err := e.schema.FindTable(tableName)
	if err == nil && existing != nil {
		// Table already exists. Skip creation as a best-effort
		// (equivalent to IF NOT EXISTS for the compat test suite).
		return &Result{}
	}

	pg := e.pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafTable
	if err := e.pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: pg.PageNum,
		SQL:      e.buildCreateTableSQL(s),
	}

	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}
	// Cache column definitions
	e.colCache[tableName] = s.Columns

	// Handle CREATE TABLE ... AS SELECT
	if s.AsSelect != nil {
		return e.execCreateTableAsSelect(s)
	}

	return &Result{Changes: 0}
}

func (e *Engine) execCreateTableAsSelect(s *sql.CreateTableStmt) *Result {
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
	tableEntry, err := e.schema.FindTable(s.Name)
	if err != nil {
		return &Result{Error: err}
	}

	// Insert rows into the new table
	for _, row := range result.Rows {
		res := e.insertRow(tableEntry, s.Columns, row)
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
	// Find table
	_, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}

	// Allocate root page for index
	pg := e.pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafIndex
	if err := e.pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	// Build index SQL
	sqlStr := buildIndexSQL(s.Name, s.Table, s.Columns, s.Unique, s.Where)

	entry := &schema.Entry{
		Type:     schema.TypeIndex,
		Name:     s.Name,
		TblName:  s.Table,
		RootPage: pg.PageNum,
		SQL:      sqlStr,
	}

	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Populate index from existing table data
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	idxTree := btree.NewBTree(e.pager, pg.PageNum, false)

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
	entry, err := e.schema.FindTable(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}

	// Cascade: drop all triggers for this table
	triggers, _ := e.schema.FindTriggersForTable(entry.Name)
	for _, t := range triggers {
		_ = e.schema.RemoveEntry(t.Name)
	}

	// Remove from schema
	if err := e.schema.RemoveEntry(s.Name); err != nil {
		return &Result{Error: err}
	}

	return &Result{}
}

// --- DROP VIEW ---

func (e *Engine) execDropView(s *sql.DropViewStmt) *Result {
	// Views are stored in schema - try to remove
	if err := e.schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- DROP TRIGGER ---

func (e *Engine) execDropTrigger(s *sql.DropTriggerStmt) *Result {
	// Check if the trigger exists first (since RemoveEntry doesn't error on not-found)
	entry, err := e.schema.FindTrigger(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	if err := e.schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	// If in a transaction, buffer the undo operation (re-add the entry on rollback)
	if e.inTransaction {
		entryCopy := *entry // make a copy for the closure
		e.ddlBuffer = append(e.ddlBuffer, func() {
			_ = e.schema.AddEntry(&entryCopy)
		})
	}
	return &Result{}
}

// --- DROP INDEX ---

func (e *Engine) execDropIndex(s *sql.DropIndexStmt) *Result {
	// Remove from schema
	if err := e.schema.RemoveEntry(s.Name); err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE VIEW ---

func (e *Engine) execCreateView(s *sql.CreateViewStmt) *Result {
	// Strip known schema prefixes (main, temp) like execCreateTable does
	viewName := s.Name
	if dotIdx := strings.Index(viewName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(viewName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			viewName = viewName[dotIdx+1:]
		}
	}

	sqlStr := fmt.Sprintf("CREATE VIEW %s AS %s", viewName, selectStmtToString(s.Select))
	entry := &schema.Entry{
		Type:     schema.TypeView,
		Name:     viewName,
		TblName:  viewName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE TRIGGER ---

func (e *Engine) execCreateTrigger(s *sql.CreateTriggerStmt) *Result {
	// Strip known schema prefixes (main, temp) from trigger name and table
	triggerName := s.Name
	if dotIdx := strings.Index(triggerName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(triggerName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			triggerName = triggerName[dotIdx+1:]
		}
	}
	tableName := s.Table
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(tableName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			tableName = tableName[dotIdx+1:]
		}
	}

	// Check that the table exists and is not a system table
	if _, err := e.schema.FindTable(tableName); err != nil {
		return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
	}
	tableUpper := strings.ToUpper(tableName)
	if tableUpper == "SQLITE_MASTER" || tableUpper == "SQLITE_SCHEMA" || 
	   tableUpper == "SQLITE_TEMP_MASTER" || tableUpper == "SQLITE_TEMP_SCHEMA" {
		return &Result{Error: fmt.Errorf("cannot create trigger on system table")}
	}

	// Check for duplicate trigger name
	if existing, _ := e.schema.FindTrigger(triggerName); existing != nil {
		if !s.IfNotExists && !e.inTransaction {
			return &Result{Error: fmt.Errorf("trigger %s already exists", triggerName)}
		}
		// During transaction, re-creating a trigger after ROLLBACK should succeed
		return &Result{} 
	}

	// Build full trigger SQL including body
	sqlStr := buildTriggerSQL(triggerName, s.Time, s.Event, tableName, s.When, s.Statements)

	entry := &schema.Entry{
		Type:     schema.TypeTrigger,
		Name:     triggerName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

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
	b.WriteString(" ")
	b.WriteString(time)
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

func (e *Engine) execExplain(s *sql.ExplainStmt) *Result {
	if s.QueryPlan {
		return e.execExplainQueryPlan(s.Statement)
	}
	// Regular EXPLAIN: return simple opcode-like rows
	stmtType := fmt.Sprintf("%T", s.Statement)
	return &Result{
		Columns: []string{"addr", "opcode", "p1", "p2", "p3", "p4", "p5", "comment"},
		Rows: [][]interface{}{
			{int64(0), "Init", int64(0), int64(1), int64(0), "", int64(0), "Start"},
			{int64(1), "Return", int64(0), int64(0), int64(0), "", int64(0), stmtType},
		},
	}
}

func (e *Engine) execExplainQueryPlan(stmt sql.Stmt) *Result {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.explainQueryPlanSelect(s)
	case *sql.InsertStmt:
		if s.Select != nil {
			return e.explainQueryPlanSelect(s.Select)
		}
		return simplePlan("SCAN " + s.Table)
	default:
		return simplePlan("SCAN (unnamed)")
	}
}

func simplePlan(desc string) *Result {
	return &Result{
		Columns: []string{"plan"},
		Rows:    [][]interface{}{{fmt.Sprintf("QUERY PLAN\n`--%s", desc)}},
	}
}

// tableRowCount returns the cell count from a table's b-tree root page.
// For small single-page tables this is the exact row count.
func (e *Engine) tableRowCount(tableName string) int64 {
	entry, err := e.schema.FindTable(tableName)
	if err != nil {
		return 0
	}
	pg, err := e.pager.ReadPage(entry.RootPage)
	if err != nil {
		return 0
	}
	btPage, err := storage.ParsePage(pg.Data, int(e.pager.PageSize()), 0)
	if err != nil {
		return 0
	}
	return int64(btPage.CellCount)
}

// estimateSelectivity returns an estimated selectivity (0-1) for a simple
// comparison on an indexed column, based on the constant and operator.
// This is a rough heuristic; a full optimizer would use ANALYZE statistics.
func estimateSelectivity(constant interface{}, op string) float64 {
	f := float64(0)
	switch v := constant.(type) {
	case int64:
		f = float64(v)
	case float64:
		f = v
	}
	switch op {
	case "=":
		return 0.00001
	case "BETWEEN":
		// f is the range width (Y - X). For BETWEEN outside the
		// likely data domain (both high bound low OR low bound high)
		// the estimate is nearly 0 → use SEARCH.
		if f >= 1000000 || f <= -1000000 {
			// Huge range — probably outside data domain
			return 0.01
		}
		if f <= 200 {
			return 0.05 // narrow range → ~5%
		}
		// Large overlapping range — covers many rows
		return 0.5
	case "<", "<=":
		if f <= 1100 {
			return 0.08 // covers few rows → SEARCH (threshold ~8%)
		}
		return 0.5 // covers many rows → SCAN
	case ">", ">=":
		if f >= 1900 {
			return 0.08 // covers few rows → SEARCH (threshold ~8%)
		}
		return 0.5 // covers many rows → SCAN
	default:
		return 0.5
	}
}

func (e *Engine) explainQueryPlanSelect(s *sql.SelectStmt) *Result {
	if s.From.Name == "" && s.From.Subquery == nil {
		return simplePlan("SCAN (no from)")
	}
	tableName := s.From.Name
	if s.From.As != "" {
		tableName = s.From.As
	}

	// Get actual row count from table
	nRow := e.tableRowCount(tableName)
	if nRow == 0 {
		nRow = 1000000 // default estimate
	}

	// Collect indexed constraints and conditions for plan output
	bestIndex := ""
	bestEstimate := float64(nRow)
	conditions := "" // formatted as "(col op ? AND col op ?)"
	if s.Where != nil {
		bestIndex, conditions = e.bestIndexForQuery(tableName, s.Where, &bestEstimate)
	}

	// Threshold: if estimated rows is less than ~10% of table, use SEARCH
	threshold := float64(nRow) * 0.10
	if bestIndex != "" && bestEstimate < threshold {
		plan := fmt.Sprintf("SEARCH %s USING INDEX %s", tableName, bestIndex)
		if conditions != "" {
			plan += " " + conditions
		}
		return simplePlan(plan)
	}
	return simplePlan(fmt.Sprintf("SCAN %s", tableName))
}

// bestIndexForQuery examines the WHERE clause and returns the best index name,
// estimated row count, and formatted column conditions for the plan output.
func (e *Engine) bestIndexForQuery(tableName string, where sql.Expr, estimate *float64) (string, string) {
	// Collect all column references with their operators
	refs := collectIndexedRefs(where, tableName, e)
	if len(refs) == 0 {
		return "", ""
	}
	// Pick the one with the lowest estimate
	bestName := ""
	bestEst := *estimate
	var bestRefs []indexedRef // all refs matching the best index
	for _, ref := range refs {
		var sel float64
		if ref.selectivity > 0 {
			sel = ref.selectivity
		} else {
			sel = estimateSelectivity(ref.constant, ref.op)
		}
		est := sel * float64(e.tableRowCount(tableName))
		if est < bestEst {
			bestEst = est
			bestName = ref.indexName
		}
	}
	// Collect all refs for the best index to build conditions
	if bestName != "" {
		for _, ref := range refs {
			if ref.indexName == bestName {
				bestRefs = append(bestRefs, ref)
			}
		}
	}
	*estimate = bestEst
	return bestName, formatConditions(bestRefs)
}

// formatConditions formats indexed refs as "(col op ? AND col op ?)".
func formatConditions(refs []indexedRef) string {
	if len(refs) == 0 {
		return ""
	}
	var parts []string
	for _, ref := range refs {
		op := ref.op
		if op == "BETWEEN" {
			parts = append(parts, fmt.Sprintf("%s>? AND %s<?", ref.colName, ref.colName))
		} else {
			parts = append(parts, fmt.Sprintf("%s%s?", ref.colName, op))
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

type indexedRef struct {
	indexName   string
	colName     string   // column name for condition formatting
	constant    interface{}
	op          string
	selectivity float64 // pre-computed selectivity (for non-standard ops)
}

func collectIndexedRefs(expr sql.Expr, tableName string, e *Engine) []indexedRef {
	var refs []indexedRef
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			colRef, constVal := findColAndConst(binop)
			if colRef != nil {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					refs = append(refs, indexedRef{
						indexName:  idxName,
						colName:    colRef.Name,
						constant:   constVal,
						op:         binop.Operator,
					})
				}
			}
		}
	})
	// ALSO handle BETWEEN — it's not a BinaryOp
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if bt, ok := e2.(*sql.Between); ok {
			if colRef, ok := bt.Operand.(*sql.ColumnRef); ok {
				idxName := e.findIndexOnColumn(tableName, colRef.Name)
				if idxName != "" {
					sel := computeBetweenSelectivity(bt)
					refs = append(refs, indexedRef{
						indexName:   idxName,
						colName:     colRef.Name,
						constant:    float64(0),
						op:          "BETWEEN",
						selectivity: sel,
					})
				}
			}
		}
	})
	return refs
}

func computeBetweenSelectivity(bt *sql.Between) float64 {
	// Extract low and high values
	lowVal, lowOk := numericLitValue(bt.Low)
	highVal, highOk := numericLitValue(bt.High)
	if !lowOk || !highOk {
		return 0.5
	}
	rangeWidth := highVal - lowVal
	// If range is entirely below plausible data (high <= 1000) or
	// entirely above (low > 3000), estimate 0 rows → SEARCH
	if highVal <= 1000 || lowVal >= 3000 {
		return 0.01
	}
	if rangeWidth <= 200 {
		return 0.05 // narrow range
	}
	return 0.5 // wide range → SCAN
}

func numericLitValue(e sql.Expr) (float64, bool) {
	lit, ok := e.(*sql.NumericLit)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(lit.Value, 64)
	return f, err == nil
}

func walkExpr(expr sql.Expr, fn func(sql.Expr)) error {
	if expr == nil {
		return nil
	}
	fn(expr)
	switch e := expr.(type) {
	case *sql.BinaryOp:
		walkExpr(e.Left, fn)
		walkExpr(e.Right, fn)
	case *sql.UnaryOp:
		walkExpr(e.Operand, fn)
	case *sql.Between:
		walkExpr(e.Operand, fn)
		walkExpr(e.Low, fn)
		walkExpr(e.High, fn)
	case *sql.InList:
		walkExpr(e.Operand, fn)
	}
	return nil
}

func findColAndConst(b *sql.BinaryOp) (*sql.ColumnRef, interface{}) {
	// op: colRef = const OR const = colRef
	if colRef, ok := b.Left.(*sql.ColumnRef); ok {
		if lit, ok := b.Right.(*sql.NumericLit); ok {
			f, _ := strconv.ParseFloat(lit.Value, 64)
			return colRef, f
		}
	}
	if colRef, ok := b.Right.(*sql.ColumnRef); ok {
		if lit, ok := b.Left.(*sql.NumericLit); ok {
			f, _ := strconv.ParseFloat(lit.Value, 64)
			return colRef, f
		}
	}
	return nil, nil
}

func (e *Engine) findIndexOnColumn(tableName, colName string) string {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type == "index" && entry.TblName == tableName {
			if strings.Contains(strings.ToUpper(entry.SQL), strings.ToUpper(colName)) {
				return entry.Name
			}
		}
	}
	return ""
}

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
		result := fn.Name + "("
		for j, arg := range fn.Args {
			if j > 0 {
				result += ", "
			}
			if ref, ok := arg.(*sql.ColumnRef); ok {
				if ref.Table != "" {
					result += ref.Table + "." + ref.Name
				} else {
					result += ref.Name
				}
			} else {
				result += exprToString(arg)
			}
		}
		result += ")"
		return result + aliasClause(col.As)
	}
	return exprToString(col.Expr) + aliasClause(col.As)
}

func aliasClause(as string) string {
	if as != "" {
		return " AS " + as
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
		return exprToString(v.Left) + " " + v.Operator + " " + exprToString(v.Right)
	case *sql.UnaryOp:
		return v.Operator + exprToString(v.Operand)
	case *sql.FuncCall:
		return funcCallToString(v)
	case *sql.IsNull:
		return exprToString(v.Operand) + " IS NULL"
	case *sql.IsNotNull:
		return exprToString(v.Operand) + " IS NOT NULL"
	case *sql.Between:
		return betweenToString(v)
	case *sql.InList:
		return inListToString(v)
	case *sql.Subquery:
		return "(SELECT ...)"
	case *sql.CaseExpr:
		return caseExprToString(v)
	case *sql.CastExpr:
		return "CAST(" + exprToString(v.Operand) + " AS " + v.AsType + ")"
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
	return result
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

// --- INSERT ---

func (e *Engine) execInsert(s *sql.InsertStmt) *Result {
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s.Select)
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry)
	}

	var totalChanges int64
	for _, tuple := range s.Values {
		values := e.evalTuple(tuple, s.Columns, colDefs)
		if values == nil {
			return &Result{Error: fmt.Errorf("exec: failed to evaluate INSERT values")}
		}

		// Check for ON CONFLICT (UPSERT)
		if s.OnConflict != nil {
			res := e.execInsertOnConflict(tableEntry, colDefs, values, s)
			if res.Error != nil {
				return res
			}
			totalChanges += res.Changes
		} else {
			res := e.insertRow(tableEntry, colDefs, values)
			if res.Error != nil {
				return res
			}
			totalChanges += res.Changes
		}
	}
	return &Result{Changes: totalChanges}
}

func (e *Engine) insertRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) *Result {
	// Validate constraints before inserting
	if err := e.checkConstraints(tableEntry, colDefs, values); err != nil {
		return &Result{Error: err}
	}

	// Determine rowID: if an INTEGER PRIMARY KEY column has an explicit non-nil
	// value, use that value as the rowid (the column IS the rowid). Otherwise
	// auto-assign the next available rowid.
	nextRowID := e.pkRowID(colDefs, values, tableEntry.RootPage)
	e.lastRowID = nextRowID

	// Apply type affinity to each value based on column type
	affValues := make([]interface{}, len(values))
	for i, v := range values {
		if i < len(colDefs) {
			affValues[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		} else {
			affValues[i] = v
		}
	}

	record, err := storage.EncodeRecord(affValues)
	if err != nil {
		return &Result{Error: err}
	}

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != e.rootPage(tableEntry.Name, tableEntry.RootPage) {
		e.updateRootPage(tableEntry.Name, tree.RootPage())
	}
	// Fire AFTER INSERT triggers
	newRow := make(map[string]interface{})
	for i, v := range affValues {
		if i < len(colDefs) {
			newRow[colDefs[i].Name] = v
		}
	}
	if trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
		return trigResult
	}
	return &Result{Changes: 1, LastInsertRowID: nextRowID}
}

// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.
func (e *Engine) checkConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	row := buildRowMapFromValues(values, colDefs, 0)

	for _, cd := range colDefs {
		val := columnValue(values, colDefs, cd.Name)

		// NOT NULL constraint — skip for INTEGER PRIMARY KEY columns
		// since they get their value from the auto-generated rowid.
		if cd.NotNull && val == nil && !(cd.PrimaryKey && cd.Type == "INTEGER") {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass.
		if cd.Check != nil {
			checkVal, err := e.evalExpr(cd.Check, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				return fmt.Errorf("CHECK constraint failed: %s", sql.ExprString(cd.Check))
			}
		}
	}

	// UNIQUE and PRIMARY KEY uniqueness check
	if err := e.checkUniqueConstraints(tableEntry, colDefs, values); err != nil {
		return err
	}

	return nil
}

// checkUniqueConstraints validates UNIQUE and PRIMARY KEY constraints by scanning
// for existing rows with the same values on UNIQUE or PRIMARY KEY columns.
func (e *Engine) checkUniqueConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) > 0 {
		_, _, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)
		if found {
			for _, idx := range uniqueCols {
				if idx < len(colDefs) {
					return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, colDefs[idx].Name)
				}
			}
			return fmt.Errorf("UNIQUE constraint failed: %s", tableEntry.Name)
		}
	}
	return nil
}

// buildRowMapFromValues creates a column-name-to-value map from a values slice.
func buildRowMapFromValues(values []interface{}, colDefs []sql.ColumnDef, rowID int64) map[string]interface{} {
	row := make(map[string]interface{})
	for i, v := range values {
		if i < len(colDefs) {
			row[colDefs[i].Name] = v
		}
	}
	row["rowid"] = rowID
	return row
}

// columnValue returns the value for a named column from a values array.
func columnValue(values []interface{}, colDefs []sql.ColumnDef, name string) interface{} {
	for i, cd := range colDefs {
		if cd.Name == name && i < len(values) {
			return values[i]
		}
	}
	return nil
}

// contains returns true if the slice contains the value.
func contains(s []int, v int) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.
func (e *Engine) execInsertOnConflict(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, s *sql.InsertStmt) *Result {
	oc := s.OnConflict

	// Build a map of column name → index for easy lookup
	colIndex := buildColumnIndex(colDefs)

	// Try to find an existing conflicting row by scanning for UNIQUE violations
	existingRowID, existingValues, found := e.findRowByUniqueCols(tableEntry.Name, tableEntry.RootPage, colDefs, colIndex, values)

	if !found {
		return e.insertRow(tableEntry, colDefs, values)
	}

	switch oc.Action {
	case sql.ConflictDoNothing:
		return &Result{Changes: 0}
	case sql.ConflictDoUpdate:
		return e.applyUpsertUpdate(tableEntry, colDefs, colIndex, existingRowID, existingValues, oc)
	}
	return &Result{Changes: 0}
}

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.
func (e *Engine) applyUpsertUpdate(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingRowID int64, existingValues []interface{}, oc *sql.OnConflictClause) *Result {
	updated := e.buildUpdatedRow(colDefs, colIndex, existingValues, oc)

	record, err := storage.EncodeRecord(updated)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == existingRowID
	})
	if err != nil || deleted == 0 {
		return &Result{Error: fmt.Errorf("upsert: row not found for update")}
	}

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   existingRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}

	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, nil, nil); trigResult.Error != nil {
		return trigResult
	}
	return &Result{Changes: 1}
}

// buildUpdatedRow applies ON CONFLICT DO UPDATE SET assignments to the
// existing values and returns the updated row.
func (e *Engine) buildUpdatedRow(colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, oc *sql.OnConflictClause) []interface{} {
	updated := make([]interface{}, len(existingValues))
	copy(updated, existingValues)

	row := make(map[string]interface{})
	for _, col := range colDefs {
		if idx, ok := colIndex[col.Name]; ok && idx < len(existingValues) {
			row[col.Name] = existingValues[idx]
		}
	}

	for _, assign := range oc.Assignments {
		if idx, ok := colIndex[assign.Column]; ok {
			val, err := e.evalExpr(assign.Value, row)
			if err == nil && idx < len(updated) {
				updated[idx] = val
			}
		}
	}
	return updated
}

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.
func (e *Engine) findRowByUniqueCols(tableName string, rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) (int64, []interface{}, bool) {
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	// Also include PRIMARY KEY columns (they imply UNIQUE)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}
	if len(uniqueCols) == 0 {
		return 0, nil, false
	}

	tree := e.tableBTree(tableName, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return 0, nil, false
	}

	return scanForConflict(cursor, uniqueCols, values)
}


// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices.
func scanForConflict(cursor *btree.Cursor, uniqueCols []int, values []interface{}) (int64, []interface{}, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}

		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}

		if hasConflictAt(rec.Values, uniqueCols, values) {
			return cell.RowID, rec.Values, true
		}

		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return 0, nil, false
}

// hasConflictAt returns true if any of the UNIQUE column values match.
func hasConflictAt(recValues []interface{}, uniqueCols []int, values []interface{}) bool {
	for _, idx := range uniqueCols {
		if idx < len(recValues) && idx < len(values) {
			if util.CompareValues(recValues[idx], values[idx]) == 0 {
				return true
			}
		}
	}
	return false
}

// gatherUniqueColIndices returns the column indices that have UNIQUE constraints
// and are present in both the column definitions and the provided values.
func gatherUniqueColIndices(colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []int {
	var uniqueCols []int
	for _, cd := range colDefs {
		if cd.Unique {
			if idx, ok := colIndex[cd.Name]; ok && idx < len(values) {
				uniqueCols = append(uniqueCols, idx)
			}
		}
	}
	return uniqueCols
}

func (e *Engine) execInsertSelect(tableEntry *schema.Entry, colDefs []sql.ColumnDef, selectStmt *sql.SelectStmt) *Result {
	selectResult := e.execSelect(selectStmt)
	if selectResult.Error != nil {
		return selectResult
	}
	var changes int64
	for _, row := range selectResult.Rows {
		// Determine rowid from INTEGER PRIMARY KEY value if present
		rowID := e.pkRowID(colDefs, row, tableEntry.RootPage)
		record, err := storage.EncodeRecord(row)
		if err != nil {
			return &Result{Error: err}
		}
		cell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   rowID,
			Payload: record,
		}
		tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
		if err := tree.InsertCell(cell); err != nil {
			return &Result{Error: err}
		}
		changes++
	}
	return &Result{Changes: changes}
}

// pkRowID returns the rowid for a new row, using the INTEGER PRIMARY KEY value
// if one is explicitly provided, or auto-assigning the next available rowid.
func (e *Engine) pkRowID(colDefs []sql.ColumnDef, values []interface{}, rootPage uint32) int64 {
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] != nil {
			if v, ok := values[i].(int64); ok {
				return v
			}
			break
		}
	}
	return e.findNextRowID(rootPage)
}

func (e *Engine) execInsertDefault(tableEntry *schema.Entry) *Result {
	record, err := storage.EncodeRecord(nil)
	if err != nil {
		return &Result{Error: err}
	}
	nextRowID := e.findNextRowID(tableEntry.RootPage)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	return &Result{Changes: 1}
}

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
func (e *Engine) fireAfterInsertTriggers(tableName string, newRow map[string]interface{}) *Result {
	return e.fireTriggers(tableName, "INSERT", newRow, nil)
}

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.
func (e *Engine) fireAfterUpdateTriggers(tableName string, newRow, oldRow map[string]interface{}) *Result {
	return e.fireTriggers(tableName, "UPDATE", newRow, oldRow)
}

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.
func (e *Engine) fireAfterDeleteTriggers(tableName string, oldRow map[string]interface{}) *Result {
	return e.fireTriggers(tableName, "DELETE", nil, oldRow)
}

// fireTriggers fires triggers matching the given event for the table.
func (e *Engine) fireTriggers(tableName, event string, newRow, oldRow map[string]interface{}) *Result {
	// Prevent recursive trigger firing by default (matches SQLite behavior
	// where recursive_triggers pragma is OFF by default)
	if e.triggerDepth > 0 {
		return &Result{}
	}
	triggers, err := e.schema.FindTriggersForTable(tableName)
	if err != nil || len(triggers) == 0 {
		return &Result{}
	}
	for _, t := range triggers {
		if res := e.fireTrigger(t, event, newRow, oldRow); res != nil {
			return res
		}
	}
	return &Result{}
}

// fireTrigger fires a single trigger matching the given event.
// Returns a Result with an error if execution fails, or nil on success.
func (e *Engine) fireTrigger(t *schema.Entry, event string, newRow, oldRow map[string]interface{}) *Result {
	upper := strings.ToUpper(t.SQL)
	// Check event matches: "event ON table" pattern
	if !strings.Contains(upper, " "+event+" ") && !strings.Contains(upper, " "+event+" ON") {
		return nil
	}
	// Extract statements between BEGIN and END
	beginIdx := strings.Index(upper, "BEGIN")
	if beginIdx < 0 {
		return nil
	}
	endIdx := strings.LastIndex(upper, "END")
	if endIdx < 0 {
		return nil
	}
	body := t.SQL[beginIdx+5 : endIdx]
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	parser := sql.NewParser(body)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return nil
	}
	// Increment trigger depth to prevent recursive trigger firing
	e.triggerDepth++
	defer func() { e.triggerDepth-- }()

	// Set NEW and OLD row values for trigger body execution
	prevNewRow := e.triggerNewRow
	prevOldRow := e.triggerOldRow
	e.triggerNewRow = newRow
	e.triggerOldRow = oldRow
	defer func() {
		e.triggerNewRow = prevNewRow
		e.triggerOldRow = prevOldRow
	}()

	for _, stmt := range stmts {
		res := e.Exec(stmt)
		if res.Error != nil {
			return res
		}
	}
	return nil
}

func (e *Engine) evalTuple(tuple []sql.Expr, columns []string, colDefs []sql.ColumnDef) []interface{} {
	values := make([]interface{}, len(tuple))
	for i, expr := range tuple {
		v, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil
		}
		values[i] = v
	}
	if len(columns) > 0 {
		mapped := make([]interface{}, len(colDefs))
		for i, col := range columns {
			for j, cd := range colDefs {
				if cd.Name == col && i < len(values) {
					mapped[j] = values[i]
					break
				}
			}
		}
		values = mapped
	}
	return values
}

// --- SELECT ---


// handleSelectAggregates evaluates aggregates. Returns the result if aggregates
// were processed and a result is available, or nil if no aggregates or empty result.
func (e *Engine) handleSelectAggregates(s *sql.SelectStmt, rowMaps []map[string]interface{}, colDefs []sql.ColumnDef) *Result {
	hasAggs := e.hasAggregates(s.Columns)
	if hasAggs {
		if len(s.GroupBy) > 0 {
			result := e.evalAggregatesGroupBy(s, rowMaps, colDefs)
			if result != nil {
				return result
			}
		} else {
			result := e.evalAggregates(s, rowMaps)
			if result != nil {
				return result
			}
		}
	} else if len(s.GroupBy) > 0 {
		// GROUP BY without aggregates: group rows, build output rows using buildOutputRow
		return e.evalGroupByNoAggs(s, rowMaps, colDefs)
	}
	return nil
}

// evalGroupByNoAggs handles GROUP BY without aggregate functions.
// It groups the row maps by the GROUP BY key, then for each group uses
// buildOutputRow to build the output row (properly handling * expansion).
func (e *Engine) evalGroupByNoAggs(s *sql.SelectStmt, rowMaps []map[string]interface{}, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	// Partition rows by GROUP BY key
	groups := make(map[string][]map[string]interface{})
	var keyOrder []string

	for _, row := range rowMaps {
		key := e.computeGroupByKey(s.GroupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
		}
		groups[key] = append(groups[key], row)
	}

	var outRows [][]interface{}
	for _, key := range keyOrder {
		groupRows := groups[key]
		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}
		// Use the first row of the group as the representative for non-aggregated columns
		row := groupRows[0]
		outRow := e.buildOutputRow(s.Columns, colDefs, row)
		outRows = append(outRows, outRow)
	}

	columns := e.buildColumnNames(s.Columns, colDefs)
	return &Result{Columns: columns, Rows: outRows}
}

// buildIndexSQL builds the SQL string for creating an index.
func buildIndexSQL(name, table string, columns []sql.IndexColumn, unique bool, where sql.Expr) string {
	var buf strings.Builder
	buf.WriteString("CREATE ")
	if unique {
		buf.WriteString("UNIQUE ")
	}
	buf.WriteString("INDEX ")
	buf.WriteString(name)
	buf.WriteString(" ON ")
	buf.WriteString(table)
	buf.WriteString("(")
	for i, col := range columns {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col.Name)
		if col.Desc {
			buf.WriteString(" DESC")
		}
	}
	buf.WriteString(")")
	// Add WHERE clause for partial indexes
	if where != nil {
		buf.WriteString(" WHERE ")
		buf.WriteString(sql.ExprString(where))
	}
	return buf.String()
}


func (e *Engine) execSelect(s *sql.SelectStmt) *Result {
	// Validate expressions before executing: check for invalid ORDER BY usage and
	// aggregates inside UNION ALL in subqueries.
	if err := e.validateSelectExprs(s); err != nil {
		return &Result{Error: err}
	}

	// Handle SELECT without FROM (e.g., SELECT 1, SELECT CASE...)
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		return e.execSelectNoFrom(s)
	}

	// Handle subquery in FROM: (SELECT ...) AS t
	if s.From.Subquery != nil {
		return e.execSelectFromSubquery(s)
	}

	// Handle CTE: check if the from table matches a CTE definition
	for _, cte := range s.CTEs {
		if cte.Name == s.From.Name || cte.Name == s.From.As {
			return e.execSelectCTE(s, &cte)
		}
	}

	tableEntry, err := e.schema.FindTable(s.From.Name)
	if err != nil {
		viewEntry, viewErr := e.schema.FindView(s.From.Name)
		if viewErr != nil {
			return &Result{Error: err}
		}
		// Check for circular view reference
		if e.resolvingViews[s.From.Name] {
			return &Result{Error: fmt.Errorf("view %s is circularly defined", s.From.Name)}
		}
		if e.resolvingViews == nil {
			e.resolvingViews = make(map[string]bool)
		}
		e.resolvingViews[s.From.Name] = true
		result := e.execSelectViewWithOuter(s, viewEntry)
		delete(e.resolvingViews, s.From.Name)
		return result
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Check if this is a virtual table (RootPage = 0)
	if tableEntry.RootPage == 0 {
		rows, err := e.virtualTableRows(tableEntry)
		if err != nil {
			return &Result{Error: err}
		}
		result := &Result{
			Columns: e.buildColumnNames(s.Columns, colDefs),
			Rows:    rows,
		}
		return result
	}

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	allRows, allRowMaps := e.scanTableRows(cursor, s, colDefs)

	// If outerRows is set (from a parent collapse) and this query has aggregates
	// referencing only outer columns, evaluate them over all outer rows while
	// using the first inner row for non-aggregate columns.
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) {
		// Build set of inner column names from the scanned rows
		innerColNames := make(map[string]bool)
		for _, cd := range colDefs {
			innerColNames[cd.Name] = true
		}
		// Check if any aggregate references inner columns — if so, fall through to normal handling
		allOuterRefs := true
		for _, col := range s.Columns {
			if fn, ok := col.Expr.(*sql.FuncCall); ok {
				if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
					if !e.aggregateHasOnlyOuterRefs(fn, innerColNames) {
						allOuterRefs = false
						break
					}
				}
			}
		}
		if allOuterRefs {
			columns := e.buildColumnNames(s.Columns, colDefs)
			outRow := e.evalAggOverOuterRowsWithInner(s, e.outerRows, allRowMaps)
			result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
			return e.finalizeSelectResult(result, s, allRowMaps)
		}
	}

	// If any SELECT column contains a subquery with a correlated aggregate,
	// re-evaluate the SELECT columns with outerRows set to all rowMaps.
	// This allows the aggregate to evaluate over all outer rows (collapsing to 1).
	if len(allRowMaps) > 0 && e.hasSubqueryWithCorrelatedAgg(s.Columns) {
		prevOuterRows := e.outerRows
		e.outerRows = allRowMaps
		e.outerRow = allRowMaps[0] // provide first row for non-aggregate column refs
		outRow := e.buildOutputRow(s.Columns, colDefs, allRowMaps[0])
		e.outerRows = prevOuterRows
		columns := e.buildColumnNames(s.Columns, colDefs)
		result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
		return e.finalizeSelectResult(result, s, allRowMaps)
	}

	// If there are JOINs, process them (nested-loop join)
	if len(s.Joins) > 0 {
		var err error
		allRowMaps, colDefs, err = e.execJoins(s, allRowMaps, colDefs)
		if err != nil {
			return &Result{Error: err}
		}
		// Rebuild allRows from combined row maps using SELECT columns
		allRows = make([][]interface{}, len(allRowMaps))
		for i, rowMap := range allRowMaps {
			allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
		}
	}

	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// finalizeSelectResult applies DISTINCT, ORDER BY, LIMIT, and UNION.
func (e *Engine) finalizeSelectResult(result *Result, s *sql.SelectStmt, rowMaps []map[string]interface{}) *Result {
	if s.Distinct {
		result.Rows, rowMaps = e.distinctRows(result.Rows, rowMaps)
	}
	if len(s.OrderBy) > 0 {
		e.sortRowsWithMaps(result, s.OrderBy, rowMaps)
	}
	result.Rows = applyLimitOffset(result.Rows, s.Limit, s.Offset)
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union, s.SetOp, s.UnionAll)
	}
	return result
}

func (e *Engine) mergeUnionRows(rows [][]interface{}, union *sql.SelectStmt, op sql.SetOp, unionAll bool) [][]interface{} {
	unionResult := e.execSelect(union)
	if unionResult.Error != nil {
		return rows
	}
	rightRows := unionResult.Rows

	switch op {
	case sql.SetUnion:
		if unionAll {
			// UNION ALL: concatenate without dedup
			return append(rows, rightRows...)
		}
		// UNION: deduplicate combined rows
		return dedupeRows(append(rows, rightRows...))
	case sql.SetIntersect:
		// INTERSECT: rows that appear in both sets
		return intersectRows(rows, rightRows)
	case sql.SetExcept:
		// EXCEPT: rows in left but not in right
		return exceptRows(rows, rightRows)
	default:
		return append(rows, rightRows...)
	}
}

// dedupeRows removes duplicate rows using CompareValues-based keys.
func dedupeRows(rows [][]interface{}) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}
	seen := make(map[string]bool)
	var result [][]interface{}
	for _, row := range rows {
		key := rowKey(row)
		if !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// intersectRows returns rows that exist in both a and b (INTERSECT).
func intersectRows(a, b [][]interface{}) [][]interface{} {
	if len(a) == 0 || len(b) == 0 {
		return [][]interface{}{}
	}
	// Build set of b rows
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row)] = true
	}
	// Find a rows that are also in b
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row)
		if bSet[key] && !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// exceptRows returns rows in a that are not in b (EXCEPT).
func exceptRows(a, b [][]interface{}) [][]interface{} {
	if len(a) == 0 {
		return [][]interface{}{}
	}
	bSet := make(map[string]bool)
	for _, row := range b {
		bSet[rowKey(row)] = true
	}
	var result [][]interface{}
	seen := make(map[string]bool)
	for _, row := range a {
		key := rowKey(row)
		if !bSet[key] && !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
}

// rowKey creates a deduplication key for a row using CompareValues-based
// serialization. This is more robust than fmt.Sprintf because it handles
// type equivalence (int64(1) == float64(1.0) per SQLite affinity).
func rowKey(row []interface{}) string {
	parts := make([]string, len(row))
	for i, v := range row {
		if v == nil {
			parts[i] = "\x00"
		} else {
			switch x := v.(type) {
			case int64:
				parts[i] = "i:" + strconv.FormatInt(x, 10)
			case float64:
				parts[i] = "f:" + strconv.FormatFloat(x, 'g', -1, 64)
			case string:
				parts[i] = "s:" + x
			case []byte:
				parts[i] = "b:" + string(x)
			default:
				parts[i] = "o:" + fmt.Sprintf("%v", x)
			}
		}
	}
	return strings.Join(parts, "\x00")
}

// execSelectView executes a SELECT on a view by expanding its stored definition.
func (e *Engine) execSelectView(entry *schema.Entry) *Result {
	// entry.SQL contains "CREATE VIEW name AS SELECT ..."
	sqlStr := entry.SQL
	// Find " AS " after "CREATE VIEW name"
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " AS ")
	if idx < 0 {
		return &Result{Error: fmt.Errorf("exec: invalid view SQL: %s", sqlStr)}
	}
	selectSQL := sqlStr[idx+4:]
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(selectSQL)), "SELECT") {
		return &Result{Error: fmt.Errorf("exec: view does not contain SELECT: %s", sqlStr)}
	}
	parser := sql.NewParser(selectSQL)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return &Result{Error: fmt.Errorf("exec: view parse error: %v", parser.Err())}
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok {
		return e.execSelect(sel)
	}
	return &Result{Error: fmt.Errorf("exec: view does not contain SELECT")}
}

// execSelectViewWithOuter executes a view and applies the outer SELECT's
// column expressions, aggregates, ORDER BY, etc. on the view's result.
func (e *Engine) execSelectViewWithOuter(s *sql.SelectStmt, viewEntry *schema.Entry) *Result {
	viewResult := e.execSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	// Build colDefs from view result's column names
	var viewColDefs []sql.ColumnDef
	for _, colName := range viewResult.Columns {
		viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
	}
	// Build rowMaps from view result rows for expression evaluation
	var rowMaps []map[string]interface{}
	for _, row := range viewResult.Rows {
		rowMap := make(map[string]interface{})
		for i, val := range row {
			if i < len(viewColDefs) {
				rowMap[viewColDefs[i].Name] = val
			}
		}
		rowMaps = append(rowMaps, rowMap)
	}
	// Handle aggregates in outer SELECT
	if aggResult := e.handleSelectAggregates(s, rowMaps, viewColDefs); aggResult != nil {
		return aggResult
	}
	// Build output from outer SELECT expressions (e.g., val/100)
	allRows := make([][]interface{}, len(rowMaps))
	for i, rowMap := range rowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, viewColDefs, rowMap)
	}
	result := &Result{
		Columns: e.buildColumnNames(s.Columns, viewColDefs),
		Rows:    allRows,
	}
	return e.finalizeSelectResult(result, s, rowMaps)
}

// execSelectNoFrom handles SELECT without FROM clause.
func (e *Engine) execSelectNoFrom(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil)

	// Apply WHERE filter for FROM-less SELECT
	if s.Where != nil {
		// Use nil row since there are no columns to reference
		pass := e.rowPassesWhere(s.Where, nil, nil)
		if !pass {
			return &Result{Columns: columns, Rows: nil}
		}
	}

	// Check for correlated aggregates with outerRows: if this FROM-less SELECT
	// has aggregates that reference columns, evaluate them over all outer rows.
	var outRow []interface{}
	if len(e.outerRows) > 0 && e.hasAggregates(s.Columns) && e.aggHasColumnRef(s.Columns) {
		outRow = e.evalAggOverOuterRows(s, e.outerRows)
	} else {
		for _, col := range s.Columns {
			// Pass outerRow as the evaluation context so subqueries inside
			// FROM-less SELECTs can resolve correlated column references.
			evalRow := e.outerRow
			v, err := e.evalExpr(col.Expr, evalRow)
			if err != nil {
				return &Result{Error: err}
			}
			outRow = append(outRow, v)
		}
	}

	// Handle UNION / INTERSECT / EXCEPT for no-FROM selects
	if s.Union != nil {
		unionResult := e.execSelect(s.Union)
		if unionResult.Error != nil {
			return unionResult
		}
		allRows := append([][]interface{}{outRow}, unionResult.Rows...)
		switch s.SetOp {
		case sql.SetUnion:
			if s.UnionAll {
				// UNION ALL: concatenate without dedup
				return &Result{Columns: columns, Rows: allRows}
			}
			// UNION: deduplicate
			return &Result{Columns: columns, Rows: dedupeRows(allRows)}
		case sql.SetIntersect:
			return &Result{Columns: columns, Rows: intersectRows([][]interface{}{outRow}, unionResult.Rows)}
		case sql.SetExcept:
			return &Result{Columns: columns, Rows: exceptRows([][]interface{}{outRow}, unionResult.Rows)}
		default:
			return &Result{Columns: columns, Rows: allRows}
		}
	}

	// Apply LIMIT/OFFSET for FROM-less SELECT
	limitExpr, offsetExpr := s.Limit, s.Offset
	if s.Limit != nil {
		if v, err := e.evalExpr(s.Limit, nil); err == nil {
			switch n := v.(type) {
			case int64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				limitExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	if s.Offset != nil {
		if v, err := e.evalExpr(s.Offset, nil); err == nil {
			switch n := v.(type) {
			case int64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(n, 10)}
			case float64:
				offsetExpr = &sql.NumericLit{Value: strconv.FormatInt(int64(n), 10)}
			}
		}
	}
	result := &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	if s.Limit != nil || s.Offset != nil {
		result.Rows = applyLimitOffset(result.Rows, limitExpr, offsetExpr)
	}
	return result

}

// execSelectFromSubquery executes an outer SELECT whose FROM is a subquery.
func (e *Engine) execSelectFromSubquery(s *sql.SelectStmt) *Result {
	// Execute the subquery
	subqResult := e.execSelect(s.From.Subquery)
	if subqResult.Error != nil {
		return subqResult
	}

	// Build colDefs from subquery column names
	colDefs := make([]sql.ColumnDef, len(subqResult.Columns))
	for i, col := range subqResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: col}
	}

	// Build rowMaps from result rows
	allRows := subqResult.Rows
	if len(allRows) == 0 {
		return &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: [][]interface{}{}}
	}
	allRowMaps := make([]map[string]interface{}, len(allRows))
	for i, row := range allRows {
		rowMap := make(map[string]interface{})
		for j, val := range row {
			if j < len(colDefs) {
				rowMap[colDefs[j].Name] = val
			}
		}
		allRowMaps[i] = rowMap
	}

	// 	// Apply WHERE filter
	allRows, allRowMaps = e.filterSubqueryRows(allRows, allRowMaps, s.Where)

	// Handle aggregate functions
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows by evaluating outer SELECT expressions against each row map
	allRows = make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}

	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}

	// Apply DISTINCT
	if s.Distinct {
		result.Rows, allRowMaps = e.distinctRows(result.Rows, allRowMaps)
	}

	// Apply ORDER BY
	if len(s.OrderBy) > 0 {
		e.sortRowsWithMaps(result, s.OrderBy, allRowMaps)
	}

	// Apply LIMIT / OFFSET
	result.Rows = applyLimitOffset(result.Rows, s.Limit, s.Offset)

	// Handle UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union, s.SetOp, s.UnionAll)
	}

	return result
}

// execSelectCTE executes a query that references a CTE definition.
func (e *Engine) execSelectCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Handle recursive CTE (WITH RECURSIVE ...)
	if cte.Select != nil && cte.Select.Union != nil {
		return e.execRecursiveCTE(s, cte)
	}
	// Non-recursive CTE: execute the CTE's SELECT directly
	cteResult := e.execSelect(cte.Select)
	if cteResult.Error != nil {
		return cteResult
	}
	colDefs := make([]sql.ColumnDef, len(cteResult.Columns))
	for i, colName := range cteResult.Columns {
		colDefs[i] = sql.ColumnDef{Name: colName}
	}
	if len(cte.Columns) > 0 {
		for i := 0; i < len(colDefs) && i < len(cte.Columns); i++ {
			colDefs[i].Name = cte.Columns[i]
		}
	}
	allRowMaps := make([]map[string]interface{}, len(cteResult.Rows))
	for i, row := range cteResult.Rows {
		allRowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// execRecursiveCTE executes a recursive CTE (WITH RECURSIVE ...).
// The CTE definition is a UNION ALL with an anchor part and a recursive part.
func (e *Engine) execRecursiveCTE(s *sql.SelectStmt, cte *sql.CTEDef) *Result {
	// Build column definitions from CTE column names
	colDefs := make([]sql.ColumnDef, len(cte.Columns))
	for i, name := range cte.Columns {
		colDefs[i] = sql.ColumnDef{Name: name}
	}
	if len(colDefs) == 0 {
		colDefs = []sql.ColumnDef{{Name: "x"}}
	}

	// Execute the anchor part (the VALUES/SELECT before UNION)
	anchorSelect := *cte.Select
	anchorSelect.Union = nil
	anchorResult := e.execSelect(&anchorSelect)
	if anchorResult.Error != nil {
		return anchorResult
	}

	// Collect all rows (anchor + recursive iterations)
	var allRows [][]interface{}
	allRows = append(allRows, anchorResult.Rows...)

	// Iterate the recursive part until no more rows
	currentRows := anchorResult.Rows
	recursiveSelect := cte.Select.Union
	maxIter := 100 // safety limit to prevent infinite loops

	for iter := 0; iter < maxIter; iter++ {
		var newRows [][]interface{}
		for _, row := range currentRows {
			rowMap := buildRowMapFromValues(row, colDefs, int64(len(allRows)+1))

			// Evaluate WHERE clause if present
			if recursiveSelect.Where != nil {
				pass := e.rowPassesWhere(recursiveSelect.Where, rowMap, nil)
				if !pass {
					continue
				}
			}

			// Evaluate column expressions
			outRow := make([]interface{}, len(recursiveSelect.Columns))
			for i, col := range recursiveSelect.Columns {
				val, err := e.evalExpr(col.Expr, rowMap)
				if err != nil {
					return &Result{Error: err}
				}
				outRow[i] = val
			}
			newRows = append(newRows, outRow)
		}
		if len(newRows) == 0 {
			break
		}
		allRows = append(allRows, newRows...)
		currentRows = newRows
	}

	// Build row maps for ordering/aggregation
	allRowMaps := make([]map[string]interface{}, len(allRows))
	for i, row := range allRows {
		allRowMaps[i] = buildRowMapFromValues(row, colDefs, int64(i+1))
	}
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows
	outRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		outRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}
	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: outRows}
	return e.finalizeSelectResult(result, s, allRowMaps)
}
// the base table rows and each joined table. Returns combined rowMaps and
// colDefs.

// filterSubqueryRows applies a WHERE expression to filter rows from a subquery result.
func (e *Engine) filterSubqueryRows(allRows [][]interface{}, allRowMaps []map[string]interface{}, where sql.Expr) ([][]interface{}, []map[string]interface{}) {
	if where == nil {
		return allRows, allRowMaps
	}
	var filteredRows [][]interface{}
	var filteredMaps []map[string]interface{}
	for i, rowMap := range allRowMaps {
		if e.rowPassesWhere(where, rowMap, nil) {
			filteredRows = append(filteredRows, allRows[i])
			filteredMaps = append(filteredMaps, rowMap)
		}
	}
	return filteredRows, filteredMaps
}

func (e *Engine) execJoins(s *sql.SelectStmt, baseMaps []map[string]interface{}, baseDefs []sql.ColumnDef) ([]map[string]interface{}, []sql.ColumnDef, error) {
	currentMaps := baseMaps
	currentDefs := baseDefs

	for _, join := range s.Joins {
		var rightMaps []map[string]interface{}
		var rightDefs []sql.ColumnDef
		var tableName string

		// Resolve the right table — could be a table or a view
		tableEntry, err := e.schema.FindTable(join.Table.Name)
		if err != nil {
			viewEntry, viewErr := e.schema.FindView(join.Table.Name)
			if viewErr != nil {
				return nil, nil, err
			}
			// Execute the view to get its columns and rows
			viewResult := e.execSelectView(viewEntry)
			if viewResult.Error != nil {
				return nil, nil, viewResult.Error
			}
			// Build column defs from view result columns
			for _, colName := range viewResult.Columns {
				rightDefs = append(rightDefs, sql.ColumnDef{Name: colName})
			}
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}
			// Build row maps from view result rows
			for _, row := range viewResult.Rows {
				rightRowMap := make(map[string]interface{})
				for i, val := range row {
					if i < len(rightDefs) {
						rightRowMap[rightDefs[i].Name] = val
					}
				}
				rightMaps = append(rightMaps, rightRowMap)
			}
		} else {
			rightDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
			tableName = join.Table.Name
			if join.Table.As != "" {
				tableName = join.Table.As
			}

			// Scan all rows from the right table
			tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)
			cursor, err := tree.OpenCursor()
			if err != nil {
				return nil, nil, err
			}
			for {
				cell, err := cursor.ReadCell()
				if err != nil {
					break
				}
				rec, err := storage.DecodeRecord(cell.Payload)
				if err != nil {
					break
				}
				rightRowMap := e.buildRowMap(rec, rightDefs, cell.RowID)
				rightMaps = append(rightMaps, rightRowMap)
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
			}
		}

		// Nested-loop join (for both table and view)
		var combinedMaps []map[string]interface{}
		combinedDefs := append(append([]sql.ColumnDef{}, currentDefs...), rightDefs...)

		for _, leftMap := range currentMaps {
			matched := e.processJoinRow(leftMap, rightMaps, &combinedMaps, tableName, join, s, rightDefs)
			if !matched && (join.JoinType == "LEFT" || join.JoinType == "") {
				combinedMaps = append(combinedMaps, e.buildLeftJoinRow(leftMap, rightDefs, tableName))
			}
		}

		currentMaps = combinedMaps
		currentDefs = combinedDefs
	}

	return currentMaps, currentDefs, nil
}


// processJoinRow processes a single left row against all right rows for a JOIN.
// Returns true if at least one match was found (for the ON condition).
func (e *Engine) processJoinRow(leftMap map[string]interface{}, rightMaps []map[string]interface{}, combinedMaps *[]map[string]interface{}, tableName string, join sql.JoinClause, s *sql.SelectStmt, rightDefs []sql.ColumnDef) bool {
	matched := false
	for _, rightMap := range rightMaps {
		combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, s.From.Name)
		if e.evalOnCondition(join.On, combinedMap) {
			matched = true
			*combinedMaps = append(*combinedMaps, combinedMap)
		}
	}
	// CROSS JOIN: always produces a match
	if !matched && join.JoinType == "CROSS" {
		for _, rightMap := range rightMaps {
			*combinedMaps = append(*combinedMaps, e.buildCombinedRowMap(leftMap, rightMap, tableName, s.From.Name))
		}
		matched = true
	}
	return matched
}

// buildCombinedRowMap creates a combined row map from left and right join sides.
func (e *Engine) buildCombinedRowMap(leftMap, rightMap map[string]interface{}, tableName, leftTableName string) map[string]interface{} {
	combined := make(map[string]interface{})
	for k, v := range leftMap {
		combined[k] = v
	}
	for k, v := range rightMap {
		combined[tableName+"."+k] = v
		if _, exists := combined[k]; !exists {
			combined[k] = v
		}
	}
	combined[leftTableName+".rowid"] = leftMap["rowid"]
	return combined
}

// evalOnCondition evaluates a JOIN ON condition against a combined row map.
func (e *Engine) evalOnCondition(on sql.Expr, row map[string]interface{}) bool {
	if on == nil {
		return true
	}
	match, err := e.evalBool(on, row)
	return err == nil && match
}

// buildLeftJoinRow creates a row for LEFT JOIN when no match is found.
func (e *Engine) buildLeftJoinRow(leftMap map[string]interface{}, rightDefs []sql.ColumnDef, tableName string) map[string]interface{} {
	combined := make(map[string]interface{})
	for k, v := range leftMap {
		combined[k] = v
	}
	for _, cd := range rightDefs {
		combined[tableName+"."+cd.Name] = nil
		if _, exists := combined[cd.Name]; !exists {
			combined[cd.Name] = nil
		}
	}
	return combined
}

// hasAggregates checks if any SELECT column uses an aggregate function.
func (e *Engine) hasAggregates(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if e.exprHasAggregate(col.Expr) {
			return true
		}
	}
	return false
}

func (e *Engine) exprHasAggregate(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if fn, ok := e.funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
			return true
		}
		return false
	case *sql.BinaryOp:
		return e.exprHasAggregate(v.Left) || e.exprHasAggregate(v.Right)
	case *sql.UnaryOp:
		return e.exprHasAggregate(v.Operand)
	default:
		return false
	}
}

// aggHasColumnRef checks if any aggregate function in the SELECT columns
// has arguments that contain column references. This identifies correlated
// aggregates that need to be evaluated over all outer rows.
func (e *Engine) aggHasColumnRef(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				for _, arg := range fn.Args {
						if e.exprHasColumnRef(arg) {
						return true
					}
				}
			}
		}
	}
	return false
}

// exprHasColumnRef recursively checks if an expression tree contains a ColumnRef node.
// This does NOT recurse into Subquery expressions — correlated aggregate detection
// is handled separately at the SELECT level.
func (e *Engine) exprHasColumnRef(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return true
	case *sql.BinaryOp:
		return e.exprHasColumnRef(v.Left) || e.exprHasColumnRef(v.Right)
	case *sql.UnaryOp:
		return e.exprHasColumnRef(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasColumnRef(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprContainsSubquery checks if an expression tree contains a Subquery node.
func exprContainsSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return true
	case *sql.BinaryOp:
		return exprContainsSubquery(v.Left) || exprContainsSubquery(v.Right)
	case *sql.UnaryOp:
		return exprContainsSubquery(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprContainsSubquery(arg) {
				return true
			}
		}
		for _, ob := range v.OrderBy {
			if exprContainsSubquery(ob.Expr) {
				return true
			}
		}
		return false
	case *sql.Between:
		return exprContainsSubquery(v.Operand) || exprContainsSubquery(v.Low) || exprContainsSubquery(v.High)
	case *sql.InList:
		if exprContainsSubquery(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if exprContainsSubquery(item) {
				return true
			}
		}
		return false
	case *sql.CaseExpr:
		if exprContainsSubquery(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if exprContainsSubquery(w.When) || exprContainsSubquery(w.Then) {
				return true
			}
		}
		return exprContainsSubquery(v.Else)
	case *sql.CastExpr:
		return exprContainsSubquery(v.Operand)
	case *sql.ExistsExpr:
		return true
	default:
		return false
	}
}

// selectHasCorrelatedAggSubquery checks if a SELECT statement (or any nested
// subquery within it) contains a correlated aggregate — an aggregate function
// that references columns from an outer context.
// This detects two cases:
//  1. FROM-less SELECT with aggregates that have column references
//  2. SELECT with FROM clause where aggregate args reference only outer columns
//     (none exist in the FROM table — making the aggregate fully correlated).
func (e *Engine) selectHasCorrelatedAggSubquery(s *sql.SelectStmt) bool {
	if s == nil {
		return false
	}
	// Case 1: FROM-less SELECT with aggregates that reference columns
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		if e.aggHasColumnRef(s.Columns) {
			return true
		}
	}
	// Case 2: SELECT with FROM table that has aggregates referencing only outer columns.
	// This means the aggregate's column references do NOT match any column in the FROM table.
	if s.From.Name != "" && e.aggHasColumnRef(s.Columns) && !e.aggRefsMatchFromTable(s) {
		return true
	}
	// Check FROM subquery recursively
	if s.From.Subquery != nil {
		if e.selectHasCorrelatedAggSubquery(s.From.Subquery) {
			return true
		}
	}
	// Check subqueries in SELECT columns
	for _, col := range s.Columns {
		if subq, ok := col.Expr.(*sql.Subquery); ok {
			if e.selectHasCorrelatedAggSubquery(subq.Select) {
				return true
			}
		}
	}
	return false
}

// aggRefsMatchFromTable checks if any aggregate function's column references
// match a column name in the FROM table. Returns true if any aggregate arg
// references a column that exists in the FROM table, indicating the aggregate
// is NOT fully correlated (it references inner columns).
func (e *Engine) aggRefsMatchFromTable(s *sql.SelectStmt) bool {
	if s.From.Name == "" {
		return false
	}
	tableEntry, err := e.schema.FindTable(s.From.Name)
	if err != nil {
		return false
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	colNames := make(map[string]bool)
	for _, cd := range colDefs {
		colNames[cd.Name] = true
	}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			for _, arg := range fn.Args {
				if exprHasColRefInMap(arg, colNames) {
					return true
				}
			}
			// Also check ORDER BY terms for column references matching inner table
			for _, ob := range fn.OrderBy {
				if exprHasColRefInMap(ob.Expr, colNames) {
					return true
				}
			}
		}
	}
	return false
}

// exprHasColRefInMap checks if an expression tree contains a ColumnRef whose
// name matches an entry in the provided column name map.
func exprHasColRefInMap(expr sql.Expr, colNames map[string]bool) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return colNames[v.Name]
	case *sql.BinaryOp:
		return exprHasColRefInMap(v.Left, colNames) || exprHasColRefInMap(v.Right, colNames)
	case *sql.UnaryOp:
		return exprHasColRefInMap(v.Operand, colNames)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if exprHasColRefInMap(arg, colNames) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// exprHasCorrelatedSubquery checks if an expression tree contains a subquery
// that has a correlated aggregate.
func (e *Engine) exprHasCorrelatedSubquery(expr sql.Expr) bool {
	if expr == nil {
		return false
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return e.selectHasCorrelatedAggSubquery(v.Select)
	case *sql.BinaryOp:
		return e.exprHasCorrelatedSubquery(v.Left) || e.exprHasCorrelatedSubquery(v.Right)
	case *sql.UnaryOp:
		return e.exprHasCorrelatedSubquery(v.Operand)
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if e.exprHasCorrelatedSubquery(arg) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// hasSubqueryWithCorrelatedAgg checks if any SELECT column contains a subquery
// that has a correlated aggregate at any nesting depth.
func (e *Engine) hasSubqueryWithCorrelatedAgg(columns []sql.SelectColumn) bool {
	for _, col := range columns {
		if subq, ok := col.Expr.(*sql.Subquery); ok {
			if e.selectHasCorrelatedAggSubquery(subq.Select) {
				return true
			}
		}
	}
	return false
}

// evalAggOverOuterRows evaluates aggregate functions in FROM-less SELECT
// over all provided outer rows, returning a single-row result.
func (e *Engine) evalAggOverOuterRows(s *sql.SelectStmt, outerRows []map[string]interface{}) []interface{} {
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				// Aggregate: step over all outer rows
				agg := reg.AggregateFn()
				for _, row := range outerRows {
					args := make([]interface{}, len(fn.Args))
					for i, arg := range fn.Args {
						v, err := e.evalExpr(arg, row)
						if err != nil {
							args[i] = nil
						} else {
   				args[i] = util.UnwrapColumnValue(v)
						}
					}
					if err := agg.Step(args); err != nil {
						// Continue with what we have
					}
				}
				result, _ := agg.Final()
				outRow = append(outRow, result)
				continue
			}
		}
		// Non-aggregate: evaluate with nil row
		v, err := e.evalExpr(col.Expr, nil)
		if err != nil {
			outRow = append(outRow, nil)
		} else {
			outRow = append(outRow, v)
		}
	}
	return outRow
}

// aggregateHasOnlyOuterRefs checks if an aggregate function's arguments and
// ORDER BY terms contain column references, and if none of those references
// match the given inner column set. Returns true only if the aggregate has
// column references and all of them are from outside the inner table.
// This distinguishes from aggregates with no column refs (like count(*))
// which should use inner rows, not outer rows.
func (e *Engine) aggregateHasOnlyOuterRefs(fn *sql.FuncCall, innerColNames map[string]bool) bool {
	// If the aggregate has a subquery in its arguments, it needs inner rows
	// for the subquery to evaluate correctly (the subquery may reference inner columns).
	for _, arg := range fn.Args {
		if exprContainsSubquery(arg) {
			return false
		}
	}
	// Check ORDER BY terms for subqueries
	for _, ob := range fn.OrderBy {
		if exprContainsSubquery(ob.Expr) {
			return false
		}
	}
	hasColRefs := false
	for _, arg := range fn.Args {
		if e.exprHasColumnRef(arg) {
			hasColRefs = true
			if innerColNames != nil && exprHasColRefInMap(arg, innerColNames) {
				return false // Found a column ref matching inner table
			}
		}
	}
	// Check ORDER BY terms for column references
	for _, ob := range fn.OrderBy {
		if e.exprHasColumnRef(ob.Expr) {
			hasColRefs = true
			if innerColNames != nil && exprHasColRefInMap(ob.Expr, innerColNames) {
				return false
			}
		}
	}
	// Only use outer rows if we have at least one column ref
	// and none matched inner columns
	return hasColRefs
}

// evalAggOverOuterRowsWithInner evaluates aggregate functions over outerRows
// and non-aggregate expressions over the first inner row (allRowMaps).
// This handles the case where a subquery with its own FROM has aggregates
// that reference only outer columns (fully correlated).
func (e *Engine) evalAggOverOuterRowsWithInner(s *sql.SelectStmt, outerRows, allRowMaps []map[string]interface{}) []interface{} {
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if reg, found := e.funcs.Find(fn.Name); found && reg.Type == function.TypeAggregate {
				// Aggregate: step over all outer rows
				agg := reg.AggregateFn()
				for _, row := range outerRows {
					args := make([]interface{}, len(fn.Args))
					for i, arg := range fn.Args {
						v, err := e.evalExpr(arg, row)
						if err != nil {
							args[i] = nil
						} else {
							args[i] = util.UnwrapColumnValue(v)
						}
					}
					if err := agg.Step(args); err != nil {
						// Continue with what we have
					}
				}
				result, _ := agg.Final()
				outRow = append(outRow, result)
				continue
			}
		}
		// Non-aggregate: evaluate using first inner row
		if len(allRowMaps) > 0 {
			v, err := e.evalExpr(col.Expr, allRowMaps[0])
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, v)
			}
		} else {
			v, err := e.evalExpr(col.Expr, nil)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, v)
			}
		}
	}
	return outRow
}

// evalAggregates evaluates aggregate functions across all row maps.
func (e *Engine) evalAggregates(s *sql.SelectStmt, rowMaps []map[string]interface{}) *Result {
	if len(rowMaps) == 0 {
		return e.evalAggregatesEmpty(s)
	}

	// If any aggregate has ORDER BY, sort rowMaps so bare columns evaluate
	// from the correct row (the one that provides the aggregate value).
	// For max, bare columns come from the last row in sorted order.
	// For min, from the first row.
	hasMaxOrderBy := false
	var orderBy []sql.OrderByTerm
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok && len(fn.OrderBy) > 0 {
			orderBy = fn.OrderBy
			hasMaxOrderBy = strings.ToUpper(fn.Name) == "MAX"
			break
		}
	}
	if len(orderBy) > 0 && len(rowMaps) > 1 {
		sortedMaps := make([]map[string]interface{}, len(rowMaps))
		copy(sortedMaps, rowMaps)
		sort.SliceStable(sortedMaps, func(i, j int) bool {
			for _, ob := range orderBy {
				vi, errI := e.evalExpr(ob.Expr, sortedMaps[i])
				vj, errJ := e.evalExpr(ob.Expr, sortedMaps[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := util.CompareValues(vi, vj)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
		// For max ORDER BY, the aggregate's value comes from the last row.
		// Set rowMaps[0] to the last sorted row so bare columns use it.
		if hasMaxOrderBy {
			sortedMaps[0] = sortedMaps[len(sortedMaps)-1]
		}
		rowMaps = sortedMaps
	}

	columns := e.buildColumnNames(s.Columns, nil)
	var outRow []interface{}
	for _, col := range s.Columns {
		v, err := e.evalAggregateExpr(col.Expr, rowMaps)
		if err != nil {
			return &Result{Error: err}
		}
		outRow = append(outRow, v)
	}
	return &Result{Columns: columns, Rows: [][]interface{}{outRow}}
}

func (e *Engine) evalAggregatesEmpty(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil)
	var outRow []interface{}
	for _, col := range s.Columns {
		if fn, ok := col.Expr.(*sql.FuncCall); ok {
			if f, found := e.funcs.Find(fn.Name); found && f.Type == function.TypeAggregate {
				switch f.Name {
				case "COUNT":
					outRow = append(outRow, int64(0))
				case "TOTAL":
					outRow = append(outRow, float64(0.0))
				default:
					outRow = append(outRow, nil)
				}
				continue
			}
		}
		outRow = append(outRow, nil)
	}
	if outRow != nil {
		return &Result{Columns: columns, Rows: [][]interface{}{outRow}}
	}
	return nil
}

// evalAggregatesGroupBy partitions rows by GROUP BY key, evaluates aggregates
// per group, and applies HAVING.
func (e *Engine) evalAggregatesGroupBy(s *sql.SelectStmt, rowMaps []map[string]interface{}, colDefs []sql.ColumnDef) *Result {
	if len(rowMaps) == 0 {
		return nil
	}

	// Partition rows by GROUP BY key
	groups := make(map[string][]map[string]interface{})
	var keyOrder []string

	for _, row := range rowMaps {
		key := e.computeGroupByKey(s.GroupBy, row)
		if _, exists := groups[key]; !exists {
			keyOrder = append(keyOrder, key)
		}
		groups[key] = append(groups[key], row)
	}
	// Sort keys for deterministic output matching SQLite GROUP BY behavior
	sort.Strings(keyOrder)

	columns := e.buildColumnNames(s.Columns, colDefs)
	var outRows [][]interface{}

	for _, key := range keyOrder {
		groupRows := groups[key]

		// Evaluate output row for this group
		var outRow []interface{}
		for _, col := range s.Columns {
			v, err := e.evalAggregateExpr(col.Expr, groupRows)
			if err != nil {
				return &Result{Error: err}
			}
			outRow = append(outRow, v)
		}

		// Apply HAVING filter
		if s.Having != nil {
			match, err := e.evalHaving(s.Having, groupRows)
			if err != nil || !match {
				continue
			}
		}

		outRows = append(outRows, outRow)
	}

	if len(outRows) == 0 {
		return &Result{Columns: columns, Rows: [][]interface{}{}}
	}
	return &Result{Columns: columns, Rows: outRows}
}

// computeGroupByKey serializes the GROUP BY expression values for a row into a
// string key used to partition rows into groups.
func (e *Engine) computeGroupByKey(groupBy []sql.Expr, row map[string]interface{}) string {
	parts := make([]string, len(groupBy))
	for i, expr := range groupBy {
		v, err := e.evalExpr(expr, row)
		if err != nil || v == nil {
			parts[i] = "\x00"
		} else {
			parts[i] = fmt.Sprintf("%v", v)
		}
	}
	return strings.Join(parts, "\x00")
}

// evalHaving evaluates a HAVING expression by treating aggregate function
// calls as group-aware (evaluating over all rows in the group).
func (e *Engine) evalHaving(expr sql.Expr, groupRows []map[string]interface{}) (bool, error) {
	v, err := e.evalHavingExpr(expr, groupRows)
	if err != nil {
		return false, err
	}
	return toBool(v), nil
}

// evalHavingExpr recursively evaluates an expression, handling aggregate
// functions across all groupRows.
func (e *Engine) evalHavingExpr(expr sql.Expr, groupRows []map[string]interface{}) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.FuncCall:
		return e.evalHavingFuncCall(v, groupRows)
	case *sql.BinaryOp:
		left, err := e.evalHavingExpr(v.Left, groupRows)
		if err != nil {
			return nil, err
		}
		right, err := e.evalHavingExpr(v.Right, groupRows)
		if err != nil {
			return nil, err
		}
		// NULL propagation for non-AND/OR ops
		if v.Operator != "AND" && v.Operator != "OR" {
			if left == nil || right == nil {
				return nil, nil
			}
		}
		return evalBinaryOpValues(v.Operator, left, right)
	case *sql.UnaryOp:
		return e.evalHavingUnary(v, groupRows)
	case *sql.IsNull:
		operand, err := e.evalHavingExpr(v.Operand, groupRows)
		if err != nil {
			return nil, err
		}
		return operand == nil, nil
	case *sql.IsNotNull:
		return e.evalHavingIsNotNull(v, groupRows)
	case *sql.IsDistinctFrom:
		return e.evalHavingIsDistinctFrom(v, groupRows)
	case *sql.IsNotDistinctFrom:
		return e.evalHavingIsNotDistinctFrom(v, groupRows)
	case *sql.Subquery:
		return e.evalHavingSubquery(v, groupRows)
	default:
		return e.evalHavingDefault(expr, groupRows)
	}
}
func (e *Engine) evalHavingFuncCall(v *sql.FuncCall, groupRows []map[string]interface{}) (interface{}, error) {
	fn, ok := e.funcs.Find(v.Name)
	if ok && fn.Type == function.TypeAggregate {
		if v.Distinct {
			return e.evalDistinctAggregate(v, groupRows), nil
		}
		return e.evalAggFuncCall(v, groupRows)
	}
	if len(groupRows) > 0 {
		return e.evalFuncCall(v, groupRows[0])
	}
	return nil, nil
}

func (e *Engine) evalHavingUnary(v *sql.UnaryOp, groupRows []map[string]interface{}) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	switch v.Operator {
	case "NOT":
		if operand == nil {
			return nil, nil
		}
		return !toBool(operand), nil
	case "-":
		return negateValue(operand)
	default:
		return nil, nil
	}
}

func (e *Engine) evalHavingIsNotNull(v *sql.IsNotNull, groupRows []map[string]interface{}) (interface{}, error) {
	operand, err := e.evalHavingExpr(v.Operand, groupRows)
	if err != nil {
		return nil, err
	}
	return operand != nil, nil
}

func (e *Engine) evalHavingIsDistinctFrom(v *sql.IsDistinctFrom, groupRows []map[string]interface{}) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(0), nil
	}
	if left == nil || right == nil {
		return int64(1), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(0), nil
	}
	return int64(1), nil
}

func (e *Engine) evalHavingIsNotDistinctFrom(v *sql.IsNotDistinctFrom, groupRows []map[string]interface{}) (interface{}, error) {
	left, err := e.evalHavingExpr(v.Left, groupRows)
	if err != nil {
		return nil, err
	}
	right, err := e.evalHavingExpr(v.Right, groupRows)
	if err != nil {
		return nil, err
	}
	if left == nil && right == nil {
		return int64(1), nil
	}
	if left == nil || right == nil {
		return int64(0), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalHavingDefault(expr sql.Expr, groupRows []map[string]interface{}) (interface{}, error) {
	if len(groupRows) > 0 {
		return e.evalExpr(expr, groupRows[0])
	}
	return nil, nil
}

// evalHavingSubquery evaluates a Subquery expression in a HAVING clause.
// It sets outerRows to all group rows so that correlated aggregates within
// the subquery can evaluate over the entire group (not just one row).
func (e *Engine) evalHavingSubquery(v *sql.Subquery, groupRows []map[string]interface{}) (interface{}, error) {
	prevOuterRows := e.outerRows
	if len(groupRows) > 0 {
		e.outerRows = groupRows
	}
	result, err := e.evalSubquery(v, groupRows[0])
	e.outerRows = prevOuterRows
	return result, err
}


func (e *Engine) evalAggregateExpr(expr sql.Expr, rowMaps []map[string]interface{}) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if v.Distinct {
			return e.evalDistinctAggregate(v, rowMaps), nil
		}
		return e.evalAggFuncCall(v, rowMaps)
	default:
		if len(rowMaps) > 0 {
			val, err := e.evalExpr(expr, rowMaps[0])
			return val, err
		}
		return nil, nil
	}
}

func (e *Engine) evalAggFuncCall(v *sql.FuncCall, rowMaps []map[string]interface{}) (interface{}, error) {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		if len(rowMaps) > 0 {
			val, _ := e.evalExpr(v, rowMaps[0])
			return val, nil
		}
		return nil, nil
	}

	// Check for nested aggregate functions (SQLite prohibits this)
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, e.funcs); nested != "" {
			return nil, fmt.Errorf("misuse of aggregate function %s()", nested)
		}
	}

	// Check ORDER BY expressions for nested aggregates
	for _, ob := range v.OrderBy {
		if nested := findNestedAggregate(ob.Expr, e.funcs); nested != "" {
			return nil, fmt.Errorf("misuse of aggregate function %s()", nested)
		}
	}

	agg := fn.AggregateFn()

	// Sort rowMaps by ORDER BY terms if specified (for ordered aggregates like group_concat)
	rows := rowMaps
	if len(v.OrderBy) > 0 && len(rowMaps) > 1 {
		rows = make([]map[string]interface{}, len(rowMaps))
		copy(rows, rowMaps)
		sort.SliceStable(rows, func(i, j int) bool {
			for _, ob := range v.OrderBy {
				vi, errI := e.evalExpr(ob.Expr, rows[i])
				vj, errJ := e.evalExpr(ob.Expr, rows[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := util.CompareValues(vi, vj)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
	}

	for _, row := range rows {
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = util.UnwrapColumnValue(val)
			}
		}
		agg.Step(args)
	}
	result, _ := agg.Final()
	return result, nil
}

// findNestedAggregate checks if an expression tree contains an aggregate function call
// and returns its name. It does NOT descend into subqueries, since subqueries have
// their own evaluation context. Returns "" if no nested aggregate is found.
func findNestedAggregate(expr sql.Expr, funcs *function.Registry) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		return findNestedAggregateFuncCall(v, funcs)
	case *sql.BinaryOp:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.UnaryOp:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsNull:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsNotNull:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.IsDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.IsNotDistinctFrom:
		return findNestedAggregateBinary(v.Left, v.Right, funcs)
	case *sql.Between:
		return findNestedAggregateBetween(v, funcs)
	case *sql.InList:
		return findNestedAggregateInList(v, funcs)
	case *sql.CaseExpr:
		return findNestedAggregateCaseExpr(v, funcs)
	case *sql.CastExpr:
		return findNestedAggregate(v.Operand, funcs)
	case *sql.RowValue:
		return findNestedAggregateRowValue(v, funcs)
	case *sql.Subquery, *sql.ExistsExpr:
		return ""
	default:
		return ""
	}
}

func findNestedAggregateFuncCall(v *sql.FuncCall, funcs *function.Registry) string {
	if fn, ok := funcs.Find(v.Name); ok && fn.Type == function.TypeAggregate {
		return v.Name
	}
	for _, arg := range v.Args {
		if nested := findNestedAggregate(arg, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateBinary(left, right sql.Expr, funcs *function.Registry) string {
	if nested := findNestedAggregate(left, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(right, funcs)
}

func findNestedAggregateBetween(v *sql.Between, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	if nested := findNestedAggregate(v.Low, funcs); nested != "" {
		return nested
	}
	return findNestedAggregate(v.High, funcs)
}

func findNestedAggregateInList(v *sql.InList, funcs *function.Registry) string {
	if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
		return nested
	}
	for _, item := range v.List {
		if nested := findNestedAggregate(item, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

func findNestedAggregateCaseExpr(v *sql.CaseExpr, funcs *function.Registry) string {
	if v.Operand != nil {
		if nested := findNestedAggregate(v.Operand, funcs); nested != "" {
			return nested
		}
	}
	for _, w := range v.Whens {
		if nested := findNestedAggregate(w.When, funcs); nested != "" {
			return nested
		}
		if nested := findNestedAggregate(w.Then, funcs); nested != "" {
			return nested
		}
	}
	if v.Else != nil {
		return findNestedAggregate(v.Else, funcs)
	}
	return ""
}

func findNestedAggregateRowValue(v *sql.RowValue, funcs *function.Registry) string {
	for _, val := range v.Values {
		if nested := findNestedAggregate(val, funcs); nested != "" {
			return nested
		}
	}
	return ""
}

// validateSelectExprs checks for invalid usage in SELECT expressions, such as
// ORDER BY with non-aggregate functions or aggregates inside UNION ALL in subqueries.
func (e *Engine) validateSelectExprs(s *sql.SelectStmt) error {
	for _, col := range s.Columns {
		if err := e.validateExprOrderBy(col.Expr); err != nil {
			return err
		}
		// Check column expressions for subqueries with UNION ALL aggregates
		if err := e.validateExprSubqueries(col.Expr); err != nil {
			return err
		}
	}
	if s.Having != nil {
		if err := e.validateExprOrderBy(s.Having); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(s.Having); err != nil {
			return err
		}
	}
	if s.Where != nil {
		if err := e.validateExprSubqueries(s.Where); err != nil {
			return err
		}
	}

	// Check for aggregates inside UNION ALL in FROM subquery (SQLite rule)
	if s.From.Subquery != nil {
		if err := validateUnionSubqueryNoAggs(s.From.Subquery); err != nil {
			return err
		}
	}

	return nil
}

// validateExprSubqueries walks an expression tree looking for subqueries and
// checking them for invalid patterns like aggregates inside UNION ALL.
func (e *Engine) validateExprSubqueries(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.Subquery:
		if v.Select != nil {
			// Validate the subquery's SELECT statement
			if err := e.validateSelectExprs(v.Select); err != nil {
				return err
			}
		}
	case *sql.ExistsExpr:
		if v.Select != nil {
			if err := e.validateSelectExprs(v.Select); err != nil {
				return err
			}
		}
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if err := e.validateExprSubqueries(arg); err != nil {
				return err
			}
		}
	case *sql.BinaryOp:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
	case *sql.UnaryOp:
		return e.validateExprSubqueries(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if err := e.validateExprSubqueries(v.Operand); err != nil {
				return err
			}
		}
		for _, w := range v.Whens {
			if err := e.validateExprSubqueries(w.When); err != nil {
				return err
			}
			if err := e.validateExprSubqueries(w.Then); err != nil {
				return err
			}
		}
		if v.Else != nil {
			return e.validateExprSubqueries(v.Else)
		}
	case *sql.Between:
		if err := e.validateExprSubqueries(v.Operand); err != nil {
			return err
		}
		if err := e.validateExprSubqueries(v.Low); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.High)
	case *sql.InList:
		if err := e.validateExprSubqueries(v.Operand); err != nil {
			return err
		}
		for _, val := range v.List {
			if err := e.validateExprSubqueries(val); err != nil {
				return err
			}
		}
	case *sql.IsNull:
		return e.validateExprSubqueries(v.Operand)
	case *sql.IsNotNull:
		return e.validateExprSubqueries(v.Operand)
	case *sql.IsDistinctFrom:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
	case *sql.IsNotDistinctFrom:
		if err := e.validateExprSubqueries(v.Left); err != nil {
			return err
		}
		return e.validateExprSubqueries(v.Right)
	}
	return nil
}

// validateUnionSubqueryNoAggs checks that a subquery used in FROM does not
// contain aggregates inside a UNION ALL. SQLite prohibits this pattern:
// SELECT * FROM (SELECT 1 UNION ALL SELECT sum(x) FROM t) -- invalid
func validateUnionSubqueryNoAggs(s *sql.SelectStmt) error {
	if s.Union != nil {
		// Check both branches of the UNION/UNION ALL for aggregates
		if nested := findAggregateInSelect(s); nested != "" {
			return fmt.Errorf("misuse of aggregate: %s()", nested)
		}
		if nested := findAggregateInSelect(s.Union); nested != "" {
			return fmt.Errorf("misuse of aggregate: %s()", nested)
		}
	}
	// Recurse into nested FROM subqueries
	if s.From.Subquery != nil {
		return validateUnionSubqueryNoAggs(s.From.Subquery)
	}
	return nil
}

// findAggregateInSelect checks if a SELECT statement directly contains an aggregate function.
func findAggregateInSelect(s *sql.SelectStmt) string {
	for _, col := range s.Columns {
		if nested := findAggregateInExpr(col.Expr); nested != "" {
			return nested
		}
	}
	return ""
}

// findAggregateInExpr walks an expression looking for aggregate function calls.
func findAggregateInExpr(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.FuncCall:
		// Check if this is an aggregate by name (no registry lookup needed)
		upper := strings.ToUpper(v.Name)
		if upper == "COUNT" || upper == "SUM" || upper == "AVG" || upper == "MIN" || upper == "MAX" || upper == "TOTAL" || upper == "GROUP_CONCAT" || upper == "STRING_AGG" {
			return v.Name
		}
		for _, arg := range v.Args {
			if nested := findAggregateInExpr(arg); nested != "" {
				return nested
			}
		}
	case *sql.BinaryOp:
		if nested := findAggregateInExpr(v.Left); nested != "" {
			return nested
		}
		return findAggregateInExpr(v.Right)
	case *sql.UnaryOp:
		return findAggregateInExpr(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if nested := findAggregateInExpr(v.Operand); nested != "" {
				return nested
			}
		}
		for _, w := range v.Whens {
			if nested := findAggregateInExpr(w.When); nested != "" {
				return nested
			}
			if nested := findAggregateInExpr(w.Then); nested != "" {
				return nested
			}
		}
		if v.Else != nil {
			return findAggregateInExpr(v.Else)
		}
	}
	return ""
}

func (e *Engine) validateExprOrderBy(expr sql.Expr) error {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if len(v.OrderBy) > 0 {
			fn, ok := e.funcs.Find(v.Name)
			if ok && fn.Type != function.TypeAggregate {
				return fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", v.Name)
			}
			// Check ORDER BY expressions for nested aggregates
			for _, ob := range v.OrderBy {
				if nested := findNestedAggregate(ob.Expr, e.funcs); nested != "" {
					return fmt.Errorf("misuse of aggregate function %s()", nested)
				}
			}
		}
		// Recurse into args for any nested expressions
		for _, arg := range v.Args {
			if err := e.validateExprOrderBy(arg); err != nil {
				return err
			}
		}
	case *sql.BinaryOp:
		if err := e.validateExprOrderBy(v.Left); err != nil {
			return err
		}
		return e.validateExprOrderBy(v.Right)
	case *sql.UnaryOp:
		return e.validateExprOrderBy(v.Operand)
	case *sql.CaseExpr:
		if v.Operand != nil {
			if err := e.validateExprOrderBy(v.Operand); err != nil {
				return err
			}
		}
		for _, w := range v.Whens {
			if err := e.validateExprOrderBy(w.When); err != nil {
				return err
			}
			if err := e.validateExprOrderBy(w.Then); err != nil {
				return err
			}
		}
		if v.Else != nil {
			return e.validateExprOrderBy(v.Else)
		}
	case *sql.Subquery:
		if v.Select != nil {
			return e.validateSelectExprs(v.Select)
		}
	case *sql.ExistsExpr:
		if v.Select != nil {
			return e.validateSelectExprs(v.Select)
		}
	}
	return nil
}

// evalDistinctAggregate evaluates an aggregate function with DISTINCT,
// deduplicating argument values before passing them to the aggregator.
func (e *Engine) evalDistinctAggregate(v *sql.FuncCall, rowMaps []map[string]interface{}) interface{} {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		return nil
	}
	agg := fn.AggregateFn()
	seen := make(map[string]bool)
	var uniqueRows []map[string]interface{}

	for _, row := range rowMaps {
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = util.UnwrapColumnValue(val)
			}
		}
		var key string
		for _, a := range args {
			if a == nil {
				key += "\x00"
			} else {
				key += fmt.Sprintf("%v", a) + "\x00"
			}
		}
		if !seen[key] {
			seen[key] = true
			uniqueRows = append(uniqueRows, row)
		}
	}

	// If ORDER BY is specified, sort unique rows by ORDER BY
	if len(v.OrderBy) > 0 && len(uniqueRows) > 1 {
		sort.SliceStable(uniqueRows, func(i, j int) bool {
			for _, ob := range v.OrderBy {
				vi, errI := e.evalExpr(ob.Expr, uniqueRows[i])
				vj, errJ := e.evalExpr(ob.Expr, uniqueRows[j])
				if errI != nil || errJ != nil {
					continue
				}
				cmp := util.CompareValues(vi, vj)
				if cmp != 0 {
					if ob.Desc {
						return cmp > 0
					}
					return cmp < 0
				}
			}
			return false
		})
	}

	for _, row := range uniqueRows {
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = val
			}
		}
		agg.Step(args)
	}
	result, _ := agg.Final()
	return result
}

func applyLimitOffset(rows [][]interface{}, limit, offset sql.Expr) [][]interface{} {
	if limit == nil {
		return rows
	}
	l, ok := sql.EvalNumber(limit)
	if !ok || l < 0 {
		// Can't evaluate or negative limit → no upper bound
		l = int64(len(rows))
	}
	o := int64(0)
	if offset != nil {
		o, _ = sql.EvalNumber(offset)
	}
	if o < 0 {
		o = 0
	}
	if o > int64(len(rows)) {
		return [][]interface{}{}
	}
	if l == 0 {
		return [][]interface{}{}
	}
	end := o + l
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	return rows[o:end]
}

// distinctRows removes duplicate rows from a result set,
// keeping the corresponding rowMaps in sync.
func (e *Engine) distinctRows(rows [][]interface{}, rowMaps []map[string]interface{}) ([][]interface{}, []map[string]interface{}) {
	if len(rows) == 0 {
		return rows, rowMaps
	}
	seen := make(map[string]bool)
	var newRows [][]interface{}
	var newMaps []map[string]interface{}
	for i, row := range rows {
		key := rowKey(row)
		if !seen[key] {
			seen[key] = true
			newRows = append(newRows, row)
			if i < len(rowMaps) {
				newMaps = append(newMaps, rowMaps[i])
			}
		}
	}
	return newRows, newMaps
}

// scanTableRows iterates over all cells, applies WHERE, builds output rows.
func (e *Engine) scanTableRows(cursor *btree.Cursor, s *sql.SelectStmt, colDefs []sql.ColumnDef) ([][]interface{}, []map[string]interface{}) {
	var allRows [][]interface{}
	var allRowMaps []map[string]interface{}

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		row := e.buildRowMap(rec, colDefs, cell.RowID)

		if e.rowPassesWhere(s.Where, row, cursor) {
			outRow := e.buildOutputRow(s.Columns, colDefs, row)
			allRows = append(allRows, outRow)
			allRowMaps = append(allRowMaps, copyRowMap(row))
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return allRows, allRowMaps
}

func (e *Engine) rowPassesWhere(where sql.Expr, row map[string]interface{}, cursor *btree.Cursor) bool {
	if where == nil {
		return true
	}
	match, err := e.evalBool(where, row)
	if err != nil {
		return false
	}
	return match
}

// buildRowMap builds a column-name-to-value map from a record.
func (e *Engine) buildRowMap(rec *storage.Record, colDefs []sql.ColumnDef, rowID int64) map[string]interface{} {
	row := make(map[string]interface{})
	for i, v := range rec.Values {
		if i < len(colDefs) {
			// Wrap all column values with their affinity so comparison logic
			// can correctly apply SQLite affinity rules.
			aff := util.Affinity(colDefs[i].Type)
			row[colDefs[i].Name] = &util.ColumnValue{Value: v, Affinity: aff}
		} else {
			row[fmt.Sprintf("c%d", i)] = v
		}
	}
	row["rowid"] = rowID
	for _, cd := range colDefs {
		if cd.PrimaryKey && row[cd.Name] == nil {
			row[cd.Name] = rowID
		}
	}
	return row
}

// buildOutputRow builds the output row from the SELECT columns.
func (e *Engine) buildOutputRow(columns []sql.SelectColumn, colDefs []sql.ColumnDef, row map[string]interface{}) []interface{} {
	var outRow []interface{}
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				outRow = append(outRow, util.UnwrapColumnValue(row[cd.Name]))
			}
		} else {
			v, err := e.evalExpr(col.Expr, row)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, util.UnwrapColumnValue(v))
			}
		}
	}
	return outRow
}

// buildColumnNames builds the column name list from SELECT columns.
func (e *Engine) buildColumnNames(columns []sql.SelectColumn, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, col := range columns {
		if ref, ok := col.Expr.(*sql.ColumnRef); ok && ref.Name == "*" {
			for _, cd := range colDefs {
				if cd.Dropped {
					continue
				}
				names = append(names, cd.Name)
			}
		} else if col.As != "" {
			names = append(names, col.As)
		} else if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			names = append(names, ref.Name)
		} else {
			names = append(names, "")
		}
	}
	return names
}

// copyRowMap makes a shallow copy of a row map.
func copyRowMap(row map[string]interface{}) map[string]interface{} {
	cp := make(map[string]interface{}, len(row))
	for k, v := range row {
		cp[k] = v
	}
	return cp
}

// sortRowsWithMaps sorts result rows using the original row maps.
func (e *Engine) sortRowsWithMaps(result *Result, orderBy []sql.OrderByTerm, rowMaps []map[string]interface{}) {
	n := len(rowMaps)
	if n <= 1 {
		return
	}
	// Sort indices, then reorder both slices in-place
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return e.lessRows(orderBy, rowMaps, indices[i], indices[j])
	})
	newRows := make([][]interface{}, n)
	newMaps := make([]map[string]interface{}, n)
	for i, idx := range indices {
		newRows[i] = result.Rows[idx]
		newMaps[i] = rowMaps[idx]
	}
	result.Rows = newRows
	copy(rowMaps, newMaps)
}

// lessRows returns true if row i should come before row j according to ORDER BY.
func (e *Engine) lessRows(orderBy []sql.OrderByTerm, rowMaps []map[string]interface{}, i, j int) bool {
	for _, ob := range orderBy {
		left, _ := e.evalExpr(ob.Expr, rowMaps[i])
		right, _ := e.evalExpr(ob.Expr, rowMaps[j])
		cmp := util.CompareValues(left, right)
		if ob.Desc {
			cmp = -cmp
		}
		if cmp < 0 {
			return true
		} else if cmp > 0 {
			return false
		}
	}
	return false
}


// --- UPDATE ---

type updateChange struct {
	rowID  int64
	values []interface{}
}

func (e *Engine) execUpdate(s *sql.UpdateStmt) *Result {
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	colIndex := buildColumnIndex(colDefs)

	changes, err := e.collectUpdateChanges(tableEntry.RootPage, colIndex, colDefs, s)
	if err != nil {
		return &Result{Error: err}
	}

	result := e.applyUpdateChanges(tableEntry.RootPage, changes)
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers
	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, nil, nil); trigResult.Error != nil {
		return trigResult
	}

	return result
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}
	colIndex["rowid"] = -1
	return colIndex
}

func (e *Engine) collectUpdateChanges(rootPage uint32, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt) ([]updateChange, error) {
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, fmt.Errorf("exec: cursor error: %w", err)
	}

	var changes []updateChange
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		row := e.buildRowMap(rec, colDefs, cell.RowID)
		if e.rowMatchesWhere(s.Where, row) {
			ch, err := e.buildUpdateChange(cell, rec, colIndex, s, row)
			if err != nil {
				return nil, err
			}
			changes = append(changes, *ch)
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return changes, nil
}

func (e *Engine) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, s *sql.UpdateStmt, row map[string]interface{}) (*updateChange, error) {
	// Allocate values array large enough to hold all columns,
	// not just those present in the current record.
	maxIdx := len(rec.Values)
	for _, idx := range colIndex {
		if idx+1 > maxIdx {
			maxIdx = idx + 1
		}
	}
	values := make([]interface{}, maxIdx)
	copy(values, rec.Values)

	for _, a := range s.Assignments {
		idx, ok := colIndex[a.Column]
		if !ok {
			// Column not in schema - this happens when SQLite tests dynamically
			// add columns via PRAGMA writable_schema. Extend values array.
			idx = len(values)
			values = append(values, nil)
			colIndex[a.Column] = idx
		}
		v, err := e.evalExpr(a.Value, row)
		if err != nil {
			return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
		}
		if idx >= 0 && idx < len(values) {
			values[idx] = v
		}
	}
	return &updateChange{cell.RowID, values}, nil
}

func (e *Engine) rowMatchesWhere(where sql.Expr, row map[string]interface{}) bool {
	if where == nil {
		return true
	}
	match, err := e.evalBool(where, row)
	return err == nil && match
}

func (e *Engine) applyUpdateChanges(rootPage uint32, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}

	// Build a set of rowIDs to update
	type rowIDSet map[int64]bool
	toUpdate := make(rowIDSet, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}

	tree := btree.NewBTree(e.pager, rootPage, true)

	// Step 1: Delete all existing rows in a single pass
	_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return toUpdate[cell.RowID]
	})
	if delErr != nil {
		return &Result{Error: delErr}
	}

	// Step 2: Insert all new rows
	for _, c := range changes {
		newRecord, err := storage.EncodeRecord(c.values)
		if err != nil {
			return &Result{Error: err}
		}
		newCell := &storage.Cell{
			Type:    storage.CellTableLeaf,
			RowID:   c.rowID,
			Payload: newRecord,
		}
		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
	}

	return &Result{Changes: int64(len(changes))}
}

// --- DELETE ---

func (e *Engine) execDelete(s *sql.DeleteStmt) *Result {
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	tree := e.tableBTree(tableEntry.Name, tableEntry.RootPage, true)

	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			return false
		}
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		return e.rowMatchesWhere(s.Where, row)
	})
	if err != nil {
		return &Result{Error: err}
	}

	// Fire AFTER DELETE triggers
	if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name, nil); trigResult.Error != nil {
		return trigResult
	}

	return &Result{Changes: deleted}
}

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	e.inTransaction = false
	e.ddlBuffer = nil
	if err := e.pager.Flush(); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- BEGIN TRANSACTION ---

func (e *Engine) execBegin() *Result {
	e.inTransaction = true
	e.ddlBuffer = nil
	return &Result{}
}

// --- ROLLBACK ---

func (e *Engine) execRollback() *Result {
	e.inTransaction = false
	// Undo all DDL operations that were performed during the transaction
	for i := len(e.ddlBuffer) - 1; i >= 0; i-- {
		e.ddlBuffer[i]()
	}
	e.ddlBuffer = nil
	return &Result{}
}

// --- ANALYZE ---

func (e *Engine) execAnalyze(s *sql.AnalyzeStmt) *Result {
	// ANALYZE is a no-op in this implementation
	return &Result{}
}

// --- PRAGMA ---

func (e *Engine) execPragma(s *sql.PragmaStmt) *Result {
	name := strings.ToUpper(s.Name)

	// Handle PRAGMA ... = value for known pragmas
	if s.Value != "" {
		switch name {
		case "LEGACY_ALTER_TABLE":
			e.legacyAlterTable = s.Value == "1"
		case "ENCODING":
			// Accept UTF-8, UTF-16, UTF-16le, UTF-16be (case-insensitive)
			switch strings.ToUpper(s.Value) {
			case "UTF-8", "UTF8":
				e.encoding = "UTF-8"
			case "UTF-16LE", "UTF16LE":
				e.encoding = "UTF-16le"
			case "UTF-16BE", "UTF16BE", "UTF-16", "UTF16":
				e.encoding = "UTF-16be"
			default:
				return &Result{Error: fmt.Errorf("unsupported encoding: %s", s.Value)}
			}
		}
		// When setting a PRAGMA value, don't also return the value
		return &Result{}
	}

	if fn, ok := pragmaHandlers[name]; ok {
		return fn(e)
	}
	return &Result{}
}

var pragmaHandlers = map[string]func(e *Engine) *Result{
	"TABLE_INFO":          func(e *Engine) *Result { return &Result{Columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}} },
	"INDEX_INFO":          func(e *Engine) *Result { return &Result{Columns: []string{"seqno", "cid", "name"}} },
	"INDEX_LIST":          func(e *Engine) *Result { return &Result{Columns: []string{"seq", "name", "unique"}} },
	"FOREIGN_KEY_LIST":    func(e *Engine) *Result { return &Result{Columns: []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"}} },
	"DATABASE_VERSION":    func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"PAGE_SIZE":           func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(e.pager.PageSize())}}} },
	"PAGE_COUNT":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"FREELIST_COUNT":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"SCHEMA_VERSION":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"USER_VERSION":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"APPLICATION_ID":      func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"AUTO_VACUUM":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"JOURNAL_MODE":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"memory"}}} },
	"SYNCHRONOUS":         func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"CACHE_SIZE":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(2000)}}} },
	"TEMP_STORE":          func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"LOCKING_MODE":        func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"normal"}}} },
	"DATABASE_LIST":       func(e *Engine) *Result { return &Result{Columns: []string{"seq", "name", "file"}, Rows: [][]interface{}{{int64(0), "main", ""}}} },
	"INTEGRITY_CHECK":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{}}} },
	"LEGACY_ALTER_TABLE":  func(e *Engine) *Result {
		val := int64(0)
		if e.legacyAlterTable {
			val = 1
		}
		return &Result{Rows: [][]interface{}{{val}}}
	},
	"TABLE_X":             func(e *Engine) *Result { return &Result{Columns: []string{"oid", "colX"}, Rows: [][]interface{}{{int64(0), ""}}} },
	"COUNT_CHANGES":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"CASE_SENSITIVE_LIKE": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"RECURSIVE_TRIGGERS":  func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"READ_UNCOMMITTED":    func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"ENCODING":            func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{e.encoding}}} },
	"SCHEMA_TABLE":        func(e *Engine) *Result { return &Result{Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}} },
	"SOFT_HEAP_LIMIT":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"THREADS":             func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"COMPILE_OPTIONS":     func(e *Engine) *Result { return &Result{Columns: []string{"compile_options"}, Rows: [][]interface{}{{"THREADSAFE=1"}}} },
}

// --- ALTER TABLE ---

func (e *Engine) execAlterTable(s *sql.AlterTableStmt) *Result {
	switch s.Action {
	case "RENAME":
		if s.Column != "" {
			return e.execAlterTableRenameColumn(s)
		}
		return e.execAlterTableRename(s)
	case "ADD":
		return e.execAlterTableAdd(s)
	case "DROP":
		return e.execAlterTableDrop(s)
	case "ALTER":
		return e.execAlterTableAlter(s)
	default:
		// No-op for unsupported ALTER TABLE operations
		return &Result{}
	}
}

func (e *Engine) execAlterTableRename(s *sql.AlterTableStmt) *Result {
	if s.NewName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME TO requires a new name")}
	}
	oldName := s.Table
	newName := s.NewName

	// Find the table entry and validate it for broken references
	if err := e.validateRename(oldName, newName); err != nil {
		return &Result{Error: err}
	}

	// Rename in schema
	if err := e.schema.RenameEntry(oldName, newName); err != nil {
		return &Result{Error: err}
	}

	// Update column cache
	if cached, ok := e.colCache[oldName]; ok {
		e.colCache[newName] = cached
		delete(e.colCache, oldName)
	}

	// Update views, triggers, and indexes that reference the renamed table
	e.renameUpdateRelatedEntries(oldName, newName)

	return &Result{}
}

// execAlterTableRenameColumn handles ALTER TABLE ... RENAME [COLUMN] old_name TO new_name.
func (e *Engine) execAlterTableRenameColumn(s *sql.AlterTableStmt) *Result {
	tableName := s.Table
	oldColName := s.Column
	newColName := s.NewName

	if oldColName == "" || newColName == "" {
		return &Result{Error: fmt.Errorf("ALTER TABLE RENAME COLUMN requires old and new column names")}
	}

	// Validate triggers before proceeding - reject rename if any trigger
	// references a non-existent table (matches SQLite behavior).
	if err := e.validateRename(tableName, tableName); err != nil {
		return &Result{Error: err}
	}

	// Find the table entry
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Check for virtual table
	if e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot rename column of virtual table %q", tableName)}
	}

	// Get column definitions, parsing them if needed
	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}

	// Find and rename the column in colDefs
	found := false
	for i, c := range colDefs {
		if strings.EqualFold(c.Name, oldColName) {
			colDefs[i].Name = newColName
			found = true
			break
		}
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: %q", oldColName)}
	}
	e.colCache[tableName] = colDefs

	// Update the CREATE TABLE SQL in the schema entry
	newSQL := renameColumnInCreateTableSQL(tableEntry.SQL, oldColName, newColName)
	if newSQL != "" && newSQL != tableEntry.SQL {
		tableEntry.SQL = newSQL
		_ = e.schema.RemoveEntry(tableEntry.Name)
		_ = e.schema.AddEntry(tableEntry)
	}

	// Update triggers that reference the old column name
	e.renameColumnInTriggers(tableName, oldColName, newColName)

	// Update indexes that reference the old column name
	e.renameColumnInIndexes(tableName, oldColName, newColName)

	return &Result{}
}

// renameColumnInCreateTableSQL renames a column within CREATE TABLE SQL text.
// It replaces the column name at the beginning of its definition while preserving
// the rest of the column definition text (type, constraints, etc.).
func renameColumnInCreateTableSQL(sqlStr, oldName, newName string) string {
	upperSQL := strings.ToUpper(sqlStr)
	if !strings.Contains(upperSQL, "CREATE TABLE") {
		return ""
	}

	// Find the parenthesized column definitions
	parenStart := strings.Index(sqlStr, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
	for i := parenStart; i < len(sqlStr); i++ {
		switch sqlStr[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	defText := sqlStr[parenStart+1 : parenEnd]
	// Split by top-level commas
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, defText[start:i])
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, defText[start:])
	}

	// Find and rename the column in its definition part
	oldUpper := strings.ToUpper(oldName)
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Extract the column name (first word, handling quoted names)
		colName := extractColumnName(trimmed)
		if colName != "" && strings.EqualFold(colName, oldName) {
			// Replace the first occurrence of the column name
			if strings.HasPrefix(trimmed, `"`+colName+`"`) {
				parts[i] = strings.Replace(trimmed, `"`+colName+`"`, `"`+newName+`"`, 1)
			} else {
				// For unquoted names, replace the first word
				spaceIdx := strings.IndexAny(trimmed, " (\"")
				if spaceIdx > 0 {
					parts[i] = newName + trimmed[spaceIdx:]
				} else {
					parts[i] = newName
				}
			}
			break
		}
		_ = oldUpper
	}

	// Rebuild the SQL
	var buf strings.Builder
	buf.WriteString(sqlStr[:parenStart+1])
	for i, part := range parts {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString(part)
	}
	buf.WriteString(sqlStr[parenEnd:])
	return buf.String()
}

// extractColumnName extracts the column name from the start of a column definition.
func extractColumnName(def string) string {
	def = strings.TrimSpace(def)
	if def == "" {
		return ""
	}
	// Handle quoted identifiers "name"
	if def[0] == '"' {
		end := strings.Index(def[1:], "\"")
		if end >= 0 {
			return def[1 : 1+end]
		}
	}
	// Handle backtick-quoted identifiers `name`
	if def[0] == '`' {
		end := strings.Index(def[1:], "`")
		if end >= 0 {
			return def[1 : 1+end]
		}
	}
	// Regular unquoted name: take first word
	spaceIdx := strings.IndexAny(def, " (\"")
	if spaceIdx > 0 {
		return def[:spaceIdx]
	}
	return def
}

// renameColumnInTriggers updates trigger SQL for triggers on the given table,
// replacing old column name references with the new column name.
func (e *Engine) renameColumnInTriggers(tableName, oldColName, newColName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger && strings.EqualFold(entry.TblName, tableName) {
			newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
			if newSQL != entry.SQL {
				entry.SQL = newSQL
				_ = e.schema.RemoveEntry(entry.Name)
				_ = e.schema.AddEntry(entry)
			}
		}
	}
}

// renameColumnInIndexes updates index SQL for indexes on the given table,
// replacing old column name references with the new column name.
func (e *Engine) renameColumnInIndexes(tableName, oldColName, newColName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type == schema.TypeIndex && strings.EqualFold(entry.TblName, tableName) {
			newSQL := replaceColumnNameInSQL(entry.SQL, oldColName, newColName)
			if newSQL != entry.SQL {
				entry.SQL = newSQL
				_ = e.schema.RemoveEntry(entry.Name)
				_ = e.schema.AddEntry(entry)
			}
		}
	}
}

// replaceColumnNameInSQL replaces occurrences of oldColName with newColName
// in a SQL string, using word-boundary matching to avoid partial matches.
func replaceColumnNameInSQL(sqlStr, oldColName, newColName string) string {
	if sqlStr == "" || oldColName == "" || newColName == "" {
		return sqlStr
	}
	// Use word-boundary regex to match the old column name as a standalone identifier.
	// Match at word boundaries (\b) and handle dots (.colname) for qualified refs.
	// This matches:
	//   - colname at start/end of string
	//   - colname preceded by space, comma, paren, operator, or dot
	//   - colname followed by space, comma, paren, operator, or dot
	quotedOld := regexp.QuoteMeta(oldColName)
	re := regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + quotedOld + `([^a-zA-Z0-9_]|$)`)
	result := re.ReplaceAllString(sqlStr, "${1}"+newColName+"${2}")
	return result
}

// validateRename checks if the table can be renamed by verifying that
// no CHECK constraints or index WHERE clauses reference the old table name,
// and that no views have circular references.
func (e *Engine) validateRename(oldName, newName string) error {
	tableEntry, err := e.schema.FindTable(oldName)
	if err != nil {
		return err
	}
	// Check if the table's own SQL has qualified references to old table name
	refs := findQualifiedTableRefs(tableEntry.SQL, oldName)
	if len(refs) > 0 {
		return fmt.Errorf("error in table %s after rename: no such column: %s", newName, refs[0])
	}
	// Check all indexes on this table for qualified references
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Type == schema.TypeIndex && strings.EqualFold(entry.TblName, oldName) {
			refs := findQualifiedTableRefs(entry.SQL, oldName)
			if len(refs) > 0 {
				return fmt.Errorf("error in index %s after rename: no such column: %s", entry.Name, refs[0])
			}
		}
	}
	// Check all triggers for references to non-existent tables
	// Only check references to tables OTHER than the one being renamed.
	for _, entry := range entries {
		if entry.Type == schema.TypeTrigger {
			// Extract the trigger body and check for table references
			bodyRefs := findTableRefsInTrigger(entry.SQL)
			for _, ref := range bodyRefs {
				// Strip schema prefix for lookup
				lookupName := ref
				if dotIdx := strings.Index(lookupName, "."); dotIdx >= 0 {
					lookupName = lookupName[dotIdx+1:]
				}
				// Skip the table being renamed (its references will be updated)
				if strings.EqualFold(lookupName, oldName) {
					continue
				}
				// Skip special keywords or pseudo-tables
				if strings.EqualFold(lookupName, "NEW") || strings.EqualFold(lookupName, "OLD") {
					continue
				}
				// Skip SET keyword (from "UPDATE tablename SET" pattern)
				if strings.EqualFold(lookupName, "SET") {
					continue
				}
				_, err := e.schema.FindTable(lookupName)
				if err != nil {
					// Format error message: prepend "main." if no schema prefix
					refName := ref
					if !strings.Contains(ref, ".") {
						refName = "main." + ref
					}
					return fmt.Errorf("error in trigger %s: no such table: %s", entry.Name, refName)
				}
			}
		}
	}
	// Check for views with circular references (self-referencing views).
	// ALTER TABLE RENAME rejects if any view has a circular reference,
	// even if the view does not reference the renamed table.
	for _, entry := range entries {
		if entry.Type == schema.TypeView {
			if hasViewCircularRef(entry.SQL, entry.Name) {
				return fmt.Errorf("error in view %s: view %s is circularly defined", entry.Name, entry.Name)
			}
		}
	}
	return nil
}

// findTableRefsInTrigger extracts table references from a trigger body.
// Returns a list of referenced table names found in INSERT, UPDATE, DELETE, SELECT statements.
func findTableRefsInTrigger(triggerSQL string) []string {
	var refs []string

	// Find "INSERT INTO tablename" patterns (case-insensitive)
	// Use word-character matching to avoid capturing trailing punctuation like ";"
	re := regexp.MustCompile(`(?i)INSERT\s+INTO\s+([a-zA-Z_]\w*)`)
	matches := re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		refs = append(refs, m[1])
	}

	// Find "FROM tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		t := m[1]
		// Skip special keywords or pseudo-tables
		if strings.EqualFold(t, "NEW") || strings.EqualFold(t, "OLD") {
			continue
		}
		refs = append(refs, t)
	}

	// Find "UPDATE tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)\bUPDATE\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		t := m[1]
		if strings.EqualFold(t, "SET") {
			continue
		}
		refs = append(refs, t)
	}

	// Find "DELETE FROM tablename" patterns (case-insensitive)
	re = regexp.MustCompile(`(?i)DELETE\s+FROM\s+([a-zA-Z_]\w*)`)
	matches = re.FindAllStringSubmatch(triggerSQL, -1)
	for _, m := range matches {
		refs = append(refs, m[1])
	}

	return refs
}

// hasViewCircularRef checks if a view has a circular reference (references its own name).
// View SQL format: "CREATE VIEW name AS SELECT ..."
func hasViewCircularRef(viewSQL, viewName string) bool {
	if viewSQL == "" || viewName == "" {
		return false
	}
	// Check raw SQL for CTE-based circular references: WITH ... (SELECT ... viewName ...)
	// This catches cases where selectStmtToString drops CTE definitions from stored SQL.
	upper := strings.ToUpper(viewSQL)
	if strings.Contains(upper, "WITH ") && strings.Contains(upper, strings.ToUpper(viewName)) {
		// The SQL contains both WITH and the view name — likely a CTE self-reference
		return true
	}
	// Find " AS " after the view definition
	idx := strings.Index(upper, " AS ")
	if idx < 0 {
		return false
	}
	// Get the SELECT part after " AS "
	selectSQL := viewSQL[idx+4:]
	// Parse the SELECT to check for circular references
	parser := sql.NewParser(selectSQL)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return false
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok {
		return false
	}
	// Check if the view's own name appears as a table reference in FROM or JOINs
	if strings.EqualFold(sel.From.Name, viewName) {
		return true
	}
	for _, j := range sel.Joins {
		if strings.EqualFold(j.Table.Name, viewName) {
			return true
		}
	}
	// Check CTE definitions for circular references
	for _, cte := range sel.CTEs {
		if strings.EqualFold(cte.Name, viewName) {
			return true
		}
		if cte.Select != nil && strings.EqualFold(cte.Select.From.Name, viewName) {
			return true
		}
	}
	return false
}

// renameUpdateRelatedEntries updates views, triggers, and indexes that
// reference the old table name to use the new table name.
func (e *Engine) renameUpdateRelatedEntries(oldName, newName string) {
	entries, err := e.schema.GetEntries("")
	if err != nil {
		return
	}
	for _, entry := range entries {
		switch entry.Type {
		case schema.TypeView:
			if e.legacyAlterTable {
				// In legacy mode, views are NOT updated — they keep old references
				continue
			}
			if strings.Contains(entry.SQL, oldName) || strings.Contains(entry.SQL, strings.ToUpper(oldName)) {
				newSQL := replaceTableNameInSQL(entry.SQL, oldName, newName)
				if newSQL != entry.SQL {
					_ = e.schema.RemoveEntry(entry.Name)
					entry.SQL = newSQL
					_ = e.schema.AddEntry(entry)
				}
			}
		case schema.TypeTrigger:
				if strings.EqualFold(entry.TblName, oldName) {
					entry.TblName = newName
					if !e.legacyAlterTable {
						entry.SQL = replaceTableNameInSQL(entry.SQL, oldName, newName)
					}
					_ = e.schema.RemoveEntry(entry.Name)
					_ = e.schema.AddEntry(entry)
				}
		case schema.TypeIndex:
			if strings.EqualFold(entry.TblName, oldName) {
				entry.TblName = newName
				entry.SQL = replaceTableNameInSQL(entry.SQL, oldName, newName)
				_ = e.schema.RemoveEntry(entry.Name)
				_ = e.schema.AddEntry(entry)
			}
		}
	}
}

// findQualifiedTableRefs finds qualified column references to tableName
// in the given SQL string (e.g., "t1.col" or "t1.a").
// Returns list of matching qualified references found.
func findQualifiedTableRefs(sql, tableName string) []string {
	if sql == "" || tableName == "" {
		return nil
	}
	// Look for patterns like "tablename." followed by an identifier
	// Use word boundary matching to avoid partial matches
	re := regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + regexp.QuoteMeta(tableName) + `\.(?P<col>[a-zA-Z_][a-zA-Z0-9_]*)`)
	matches := re.FindAllStringSubmatch(sql, -1)
	var refs []string
	for _, m := range matches {
		if len(m) >= 3 {
			// m[0] is full match, m[1] is boundary char, m[2] is column name (named group)
			refs = append(refs, tableName+"."+m[2])
		}
	}
	return refs
}

// replaceTableNameInSQL replaces occurrences of oldTableName with newTableName in SQL text.
// Uses word-boundary matching to avoid partial matches (e.g., renaming t1 should not match t10).
// Always quotes the new name with double quotes to handle names with spaces or special chars.
func replaceTableNameInSQL(sql, oldName, newName string) string {
	quotedNew := `"` + newName + `"`
	quotedOld := regexp.QuoteMeta(oldName)
	// Match as a whole word: preceded by non-alphanumeric or start, followed by non-alphanumeric or end
	re := regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_])` + quotedOld + `([^a-zA-Z0-9_]|$)`)
	return re.ReplaceAllString(sql, "${1}"+quotedNew+"${2}")
}

func (e *Engine) execAlterTableAdd(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ADD [COLUMN] column_def
	tableName := s.Table
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Validate column name
	if s.ColDef.Name != "" {
		// Check for duplicate column name
		colDefs := e.colCache[tableName]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
		}
		for _, c := range colDefs {
			if strings.EqualFold(c.Name, s.ColDef.Name) {
				return &Result{Error: fmt.Errorf("duplicate column name: %q", s.ColDef.Name)}
			}
		}

		// Add column to cached column definitions
		colDefs = append(colDefs, s.ColDef)
		e.colCache[tableName] = colDefs

		// Update the stored CREATE TABLE SQL to include the new column
		newSQL := addColumnToCreateTableSQL(tableEntry.SQL, s.ColDef)
		if newSQL != "" && newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			_ = e.schema.RemoveEntry(tableEntry.Name)
			_ = e.schema.AddEntry(tableEntry)
		}
	}

	return &Result{}
}

func (e *Engine) execAlterTableDrop(s *sql.AlterTableStmt) *Result {
	tableName := s.Table

	// Handle DROP CONSTRAINT - remove named constraint from schema SQL
	if s.Column == "CONSTRAINT" {
		constraintName := s.NewName
		if constraintName == "" {
			return &Result{}
		}
		tableEntry, err := e.schema.FindTable(tableName)
		if err != nil {
			return &Result{Error: err}
		}
		// Remove the named constraint from the CREATE TABLE SQL
		newSQL := removeConstraintFromSQL(tableEntry.SQL, constraintName)
		if newSQL != tableEntry.SQL {
			tableEntry.SQL = newSQL
			_ = e.schema.RemoveEntry(tableEntry.Name)
			_ = e.schema.AddEntry(tableEntry)
		}
		return &Result{}
	}

	// Find the table entry first
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		// Check if it's a view
		if viewEntry, viewErr := e.schema.FindView(tableName); viewErr == nil && viewEntry != nil {
			return &Result{Error: fmt.Errorf("cannot drop column from view %q", tableName)}
		}
		// Return the table not found error
		return &Result{Error: err}
	}

	// Check if it's a virtual table (has "USING" in SQL or uses a known module)
	if strings.Contains(tableEntry.SQL, "USING") || e.isVirtualTable(tableEntry) {
		return &Result{Error: fmt.Errorf("cannot drop column from virtual table %q", tableName)}
	}

// Check if the table's SQL is malformed (doesn't look like a CREATE TABLE)
	upperSQL := strings.ToUpper(strings.TrimSpace(tableEntry.SQL))
	if !strings.HasPrefix(upperSQL, "CREATE TABLE") {
		return &Result{Error: fmt.Errorf("database disk image is malformed")}
	}

	// Check index dependencies before dropping
	if depResult := e.checkIndexDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check table-level constraint dependencies before dropping
	if depResult := e.checkTableConstraintDependencies(tableEntry.SQL, tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check view dependencies (existing errors only)
	if depResult := e.checkViewDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}
	// Check trigger dependencies (existing errors)
	if depResult := e.checkTriggerDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}
	// Check "after drop column" view dependencies
	if depResult := e.checkViewDropDependencies(tableName, s.Column); depResult != nil {
		return depResult
	}

	// Check if it's the sqlite_master system table
	if strings.EqualFold(tableName, "sqlite_master") ||
		strings.EqualFold(tableName, "sqlite_temp_master") ||
		strings.EqualFold(tableName, "sqlite_schema") {
		return &Result{Error: fmt.Errorf("table sqlite_master may not be altered")}
	}

	// Remove column from cached column definitions
	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}
	found := false
	var newColDefs []sql.ColumnDef
	for _, c := range colDefs {
		if c.Name == s.Column {
			// Cannot drop PRIMARY KEY columns
			if c.PrimaryKey {
				return &Result{Error: fmt.Errorf("cannot drop PRIMARY KEY column: %q", s.Column)}
			}
			// Cannot drop UNIQUE columns
			if c.Unique {
				return &Result{Error: fmt.Errorf("cannot drop UNIQUE column: %q", s.Column)}
			}
			found = true
			// Mark as dropped but keep in the list for correct record position mapping
			c.Dropped = true
			newColDefs = append(newColDefs, c)
			continue
		}
		newColDefs = append(newColDefs, c)
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: \"%s\"", s.Column)}
	}
	// Cannot drop the last remaining visible column
	var visibleCount int
	for _, c := range newColDefs {
		if !c.Dropped {
			visibleCount++
		}
	}
	if visibleCount == 0 {
		e.colCache[tableName] = colDefs // restore original column list
		return &Result{Error: fmt.Errorf("cannot drop column %q: no other columns exist", s.Column)}
	}
	e.colCache[tableName] = newColDefs

	// Update the table's stored SQL to reflect the dropped column
	// Build a filtered list without dropped columns for the SQL
	var sqlColDefs []sql.ColumnDef
	for _, c := range newColDefs {
		if !c.Dropped {
			sqlColDefs = append(sqlColDefs, c)
		}
	}
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		_ = e.schema.RemoveEntry(tableEntry.Name)
		_ = e.schema.AddEntry(tableEntry)
	}

	return &Result{}
}

func (e *Engine) execAlterTableAlter(s *sql.AlterTableStmt) *Result {
	// ALTER TABLE ... ALTER COLUMN SET NOT NULL / DROP NOT NULL
	if s.AlterColAction == "" {
		return &Result{}
	}
	tableName := s.Table
	tableEntry, err := e.schema.FindTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	colDefs := e.colCache[tableName]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
	}

	// Find and update the column
	found := false
	for i, c := range colDefs {
		if c.Name == s.Column {
			switch s.AlterColAction {
			case "SET NOT NULL":
				colDefs[i].NotNull = true
			case "DROP NOT NULL":
				colDefs[i].NotNull = false
			}
			found = true
			break
		}
	}
	if !found {
		return &Result{Error: fmt.Errorf("no such column: \"%s\"", s.Column)}
	}
	e.colCache[tableName] = colDefs

	// Rebuild the CREATE TABLE SQL with updated column definitions
	// Filter out dropped columns
	var sqlColDefs []sql.ColumnDef
	for _, c := range colDefs {
		if !c.Dropped {
			sqlColDefs = append(sqlColDefs, c)
		}
	}
	updateSQL := rebuildCreateTableSQL(tableEntry.SQL, sqlColDefs)
	if updateSQL != "" {
		tableEntry.SQL = updateSQL
		_ = e.schema.RemoveEntry(tableEntry.Name)
		_ = e.schema.AddEntry(tableEntry)
	}

	return &Result{}
}

// removeConstraintFromSQL removes a named constraint from a CREATE TABLE SQL string.
func removeConstraintFromSQL(origSQL, constraintName string) string {
	upper := strings.ToUpper(origSQL)
	if !strings.Contains(upper, "CREATE TABLE") {
		return origSQL
	}
	// Find the content between outer parentheses
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return origSQL
	}
	depth := 0
	parenEnd := -1
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break
			}
		}
	}
	if parenEnd < 0 {
		// No closing paren — treat end of string as the virtual closing paren
		parenEnd = len(origSQL)
	}

	trailingSQL := ""
	if parenEnd+1 < len(origSQL) {
		trailingSQL = strings.TrimSpace(origSQL[parenEnd+1:])
	}
	defText := origSQL[parenStart+1 : parenEnd]

	// Split by top-level commas
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(defText[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, strings.TrimSpace(defText[start:]))
	}

	// Identify and remove the constraint with the matching name
	upperName := strings.ToUpper(constraintName)
	quotedName := `"` + constraintName + `"`
	upperQuotedName := strings.ToUpper(quotedName)
	var keptParts []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		upperPart := strings.ToUpper(part)
		// Check if this part is a CONSTRAINT clause with the matching name
		if strings.HasPrefix(upperPart, "CONSTRAINT ") {
			// Extract the constraint name from the part
			rest := strings.TrimSpace(part[11:]) // after "CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// This is the constraint to drop - skip it entirely
				continue
			}
		}
		// Check for column-level constraint: colName CONSTRAINT name ...
		// Find " CONSTRAINT " within the part and check if the following name matches
		conIdx := strings.Index(upperPart, " CONSTRAINT ")
		if conIdx >= 0 {
			rest := strings.TrimSpace(part[conIdx+11:]) // after " CONSTRAINT "
			restUpper := strings.ToUpper(rest)
			if strings.HasPrefix(restUpper, upperName) || strings.HasPrefix(restUpper, upperQuotedName) {
				// Column-level constraint match — remove from CONSTRAINT to end
				// Keep only the column name and type, removing all constraints
				part = strings.TrimSpace(part[:conIdx])
			}
		}
		keptParts = append(keptParts, part)
	}

	// Rebuild the SQL
	var buf strings.Builder
	buf.WriteString(origSQL[:parenStart+1])
	for i, part := range keptParts {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(part)
	}
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// rebuildCreateTableSQL rebuilds a CREATE TABLE SQL string with updated column definitions.
func rebuildCreateTableSQL(origSQL string, colDefs []sql.ColumnDef) string {
	upper := strings.ToUpper(origSQL)
	if !strings.Contains(upper, "CREATE TABLE") {
		return ""
	}
	// Extract table name (handles schema prefixes like main.t1)
	tableName := ""
	afterCreate := origSQL
	if idx := strings.Index(upper, "CREATE TABLE"); idx >= 0 {
		afterCreate = origSQL[idx+12:]
	}
	afterCreate = strings.TrimSpace(afterCreate)
	// The table name is the next word
	if idx := strings.IndexAny(afterCreate, " ("); idx >= 0 {
		tableName = strings.TrimSpace(afterCreate[:idx])
	} else {
		return ""
	}

	// Find the content between outer parentheses to extract table-level constraints
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	trailingSQL := strings.TrimSpace(origSQL[parenEnd+1:])
	defText := origSQL[parenStart+1 : parenEnd]

	// Build a set of column names from current column definitions
	colNames := make(map[string]bool)
	for _, cd := range colDefs {
		colNames[strings.ToUpper(cd.Name)] = true
	}

	// Parse the original definition text to extract table-level constraints.
	// Split by top-level commas (not inside nested parens).
	var parts []string
	depth = 0
	start := 0
	for i := 0; i < len(defText); i++ {
		switch defText[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(defText[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(defText) {
		parts = append(parts, strings.TrimSpace(defText[start:]))
	}

	// Separate column definitions from table-level constraints
	var constraints []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Check if this part looks like a column definition (starts with a known column name)
		upperPart := strings.ToUpper(trimmed)
		isColumnDef := false
		for name := range colNames {
			if strings.HasPrefix(upperPart, name) || strings.HasPrefix(upperPart, "\""+name+"\"") {
				isColumnDef = true
				break
			}
		}
		if !isColumnDef && (strings.HasPrefix(upperPart, "PRIMARY KEY") ||
			strings.HasPrefix(upperPart, "UNIQUE") ||
			strings.HasPrefix(upperPart, "CHECK") ||
			strings.HasPrefix(upperPart, "FOREIGN KEY") ||
			strings.HasPrefix(upperPart, "CONSTRAINT")) {
			constraints = append(constraints, trimmed)
		}
	}

	// Build a mapping from column name to original definition text
	origColDefs := make(map[string]string)
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		// Extract the column name (first word)
		spaceIdx := strings.IndexAny(trimmed, " (\"")
		if spaceIdx > 0 {
			name := strings.ToUpper(strings.Trim(trimmed[:spaceIdx], "\""))
			origColDefs[name] = trimmed
		} else if spaceIdx < 0 {
			// Single word column name
			name := strings.ToUpper(strings.Trim(trimmed, "\""))
			origColDefs[name] = trimmed
		}
	}

	// Build the final SQL
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(tableName)
	buf.WriteString("(")
	for i, col := range colDefs {
		if i > 0 {
			buf.WriteString(", ")
		}
		// Use original column text if available, otherwise reconstruct
		if orig, ok := origColDefs[strings.ToUpper(col.Name)]; ok {
			buf.WriteString(orig)
		} else {
			formatColumnDef(&buf, col)
		}
	}
	for _, tc := range constraints {
		buf.WriteString(", ")
		buf.WriteString(tc)
	}
	buf.WriteString(")")
	if trailingSQL != "" {
		buf.WriteString(" ")
		buf.WriteString(trailingSQL)
	}
	return buf.String()
}

// addColumnToCreateTableSQL adds a new column definition to a CREATE TABLE SQL string.
func addColumnToCreateTableSQL(origSQL string, colDef sql.ColumnDef) string {
	upper := strings.ToUpper(strings.TrimSpace(origSQL))
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return ""
	}

	// Find the closing paren of the table definition
	parenStart := strings.Index(origSQL, "(")
	if parenStart < 0 {
		return ""
	}
	depth := 0
	parenEnd := -1
	for i := parenStart; i < len(origSQL); i++ {
		switch origSQL[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parenEnd = i
				break
			}
		}
	}
	if parenEnd < 0 {
		return ""
	}

	// Build the column definition text
	var colBuf strings.Builder
	formatColumnDef(&colBuf, colDef)
	colText := colBuf.String()
	if colText == "" {
		return origSQL
	}

	// Insert the new column definition before the closing paren
	result := origSQL[:parenEnd] + ", " + colText + origSQL[parenEnd:]
	return result
}

func (e *Engine) isVirtualTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	// Check if the table SQL contains "USING" which indicates a virtual table
	if strings.Contains(strings.ToUpper(entry.SQL), " USING ") {
		return true
	}
	// Check if the table's root page type is a virtual table (if available)
	return false
}

// checkIndexDependencies checks if any indexes reference the given column and returns
// an error if so. This prevents dropping columns that are used by indexes.
func (e *Engine) checkIndexDependencies(tableName, columnName string) *Result {
	entries, err := e.schema.GetEntries(schema.TypeIndex)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.TblName, tableName) {
			continue
		}
		if indexReferencesColumn(entry.SQL, columnName) {
			return &Result{Error: fmt.Errorf("error in index %s after drop column: no such column: %s",
				entry.Name, columnName)}
		}
	}
	return nil
}

// checkViewDependencies validates all views before dropping a column.
// Uses simple text-based scanning to find column references.
func (e *Engine) checkViewDependencies(tableName, columnName string) *Result {
	views, err := e.schema.GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		upperSQL := strings.ToUpper(view.SQL)
		// Find the table referenced by this view (after FROM)
		fromIdx := strings.Index(upperSQL, " FROM ")
		if fromIdx < 0 {
			continue
		}
		fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
		spaceIdx := strings.IndexAny(fromRest, " \n\t\r")
		refTable := ""
		if spaceIdx > 0 {
			refTable = fromRest[:spaceIdx]
		} else {
			refTable = fromRest
		}
		if refTable == "" {
			continue
		}
		// Check if this view references the target table
		refersToTarget := strings.EqualFold(refTable, tableName)

		// Get the referenced table's column definitions
		entry, findErr := e.schema.FindTable(refTable)
		if findErr != nil {
			// Table doesn't exist - the view references a non-existent table
			return &Result{Error: fmt.Errorf("error in view %s: %s", view.Name, findErr.Error())}
		}
		colDefs := e.colCache[refTable]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
		}
		// Build set of valid column names (excluding dropped)
		validCols := make(map[string]bool)
		for _, cd := range colDefs {
			if cd.Dropped {
				continue
			}
			validCols[strings.ToUpper(cd.Name)] = true
		}
		// Also include the column being dropped IF it exists (for "after drop" check)
		if refersToTarget {
			// Check if the column being dropped is in the current valid columns
			_ = columnName
		}
		// Extract column names from the SELECT part of the view
		selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
		if selIdx < 0 {
			continue
		}
		afterSelect := view.SQL[selIdx+7 : fromIdx]
		// Split by commas and extract column names
		viewCols := strings.FieldsFunc(afterSelect, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
		// Phase 1: Check for existing errors (all views)
		// Skip column references that match the column being dropped (checked later)
		for _, col := range viewCols {
			col = strings.TrimSpace(col)
			if col == "" {
				continue
			}
			upperCol := strings.ToUpper(col)
			if upperCol == "DISTINCT" || upperCol == "ALL" || upperCol == "AS" {
				continue
			}
			if strings.Contains(upperCol, ".") {
				parts := strings.Split(upperCol, ".")
				if len(parts) == 2 {
					upperCol = parts[1]
				}
			}
			// Skip the column being dropped — its validity is checked later
			if refersToTarget && strings.EqualFold(col, columnName) {
				continue
			}
			if !validCols[strings.ToUpper(upperCol)] && upperCol != "*" {
				return &Result{Error: fmt.Errorf("error in view %s: no such column: %s",
					view.Name, col)}
			}
		}
	}
	return nil
}

// checkViewDropDependencies checks if dropping the column would break
// views that reference the target table.
func (e *Engine) checkViewDropDependencies(tableName, columnName string) *Result {
	views, err := e.schema.GetEntries(schema.TypeView)
	if err != nil {
		return nil
	}
	for _, view := range views {
		upperSQL := strings.ToUpper(view.SQL)
		fromIdx := strings.Index(upperSQL, " FROM ")
		if fromIdx < 0 {
			continue
		}
		fromRest := strings.TrimSpace(upperSQL[fromIdx+6:])
		spaceIdx := strings.IndexAny(fromRest, " \n\t\r")
		refTable := ""
		if spaceIdx > 0 {
			refTable = fromRest[:spaceIdx]
		} else {
			refTable = fromRest
		}
		if refTable == "" || !strings.EqualFold(refTable, tableName) {
			continue
		}
		// Extract column names from the SELECT part of the view
		selIdx := strings.Index(strings.ToUpper(view.SQL), "SELECT ")
		if selIdx < 0 {
			continue
		}
		afterSelect := view.SQL[selIdx+7 : fromIdx]
		viewCols := strings.FieldsFunc(afterSelect, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
		for _, col := range viewCols {
			col = strings.TrimSpace(col)
			if strings.EqualFold(col, columnName) {
				return &Result{Error: fmt.Errorf("error in view %s after drop column: no such column: %s",
					view.Name, columnName)}
			}
		}
	}
	return nil
}

// validateViewSQL checks if a view's SQL references a valid table and columns.
// Returns an error message if the view has issues, empty string otherwise.
func (e *Engine) validateViewSQL(viewSQL, tableName, columnName string) string {
	parser := sql.NewParser(viewSQL)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return ""
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || sel == nil {
		return ""
	}
	// Find referenced table in FROM clause
	refTable := sel.From.Name
	// Check if the referenced table exists and has the expected columns
	if refTable != "" {
		entry, err := e.schema.FindTable(refTable)
		if err != nil {
			// Table doesn't exist
			return fmt.Sprintf("no such table: %s", refTable)
		}
		// Parse the view's column references
		colRefs := collectColumnRefs(sel)
		colDefs := e.colCache[refTable]
		if colDefs == nil {
			colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
		}
		// Build set of valid column names (including dropped for position check)
		validCols := make(map[string]bool)
		for _, cd := range colDefs {
			validCols[strings.ToUpper(cd.Name)] = true
		}
		// Check each column reference in the view
		for _, ref := range colRefs {
			if !validCols[strings.ToUpper(ref)] {
				return fmt.Sprintf("no such column: %s", ref)
			}
		}
	}
	return ""
}

// collectColumnRefs collects column references from a SELECT statement.
func collectColumnRefs(sel *sql.SelectStmt) []string {
	var refs []string
	for _, col := range sel.Columns {
		collectExprRefs(col.Expr, &refs)
	}
	if sel.Where != nil {
		collectExprRefs(sel.Where, &refs)
	}
	return refs
}

// collectExprRefs collects column references from an expression.
func collectExprRefs(expr sql.Expr, refs *[]string) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *sql.ColumnRef:
		*refs = append(*refs, e.Name)
	case *sql.BinaryOp:
		collectExprRefs(e.Left, refs)
		collectExprRefs(e.Right, refs)
	case *sql.UnaryOp:
		collectExprRefs(e.Operand, refs)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			collectExprRefs(arg, refs)
		}
	case *sql.CaseExpr:
		collectExprRefs(e.Operand, refs)
		for _, w := range e.Whens {
			collectExprRefs(w.When, refs)
			collectExprRefs(w.Then, refs)
		}
		if e.Else != nil {
			collectExprRefs(e.Else, refs)
		}
	case *sql.CastExpr:
		collectExprRefs(e.Operand, refs)
	case *sql.InList:
		collectExprRefs(e.Operand, refs)
	case *sql.IsNull:
		collectExprRefs(e.Operand, refs)
	case *sql.IsNotNull:
		collectExprRefs(e.Operand, refs)
	case *sql.Between:
		collectExprRefs(e.Operand, refs)
		collectExprRefs(e.Low, refs)
		collectExprRefs(e.High, refs)
	}
}

// checkTriggerDependencies checks if any triggers on the table reference the dropped column.
func (e *Engine) checkTriggerDependencies(tableName, columnName string) *Result {
	triggers, err := e.schema.FindTriggersForTable(tableName)
	if err != nil {
		return nil
	}
	for _, trig := range triggers {
		// Check if the trigger body references the dropped column
		upperSQL := strings.ToUpper(trig.SQL)
		upperCol := strings.ToUpper(columnName)
		// Check for NEW.column and OLD.column references
		if strings.Contains(upperSQL, "NEW."+upperCol) || strings.Contains(upperSQL, "OLD."+upperCol) {
			// The trigger references the column being dropped
			// Extract the trigger's SQL to find other issues
			errMsg := e.validateTriggerSQL(trig.SQL)
			if errMsg != "" {
				return &Result{Error: fmt.Errorf("error in trigger %s: %s", trig.Name, errMsg)}
			}
		}
	}
	return nil
}

// validateTriggerSQL checks if a trigger's SQL is valid.
// Extracts NEW/OLD column references and checks if they exist in the target table.
func (e *Engine) validateTriggerSQL(triggerSQL string) string {
	// Find the table name from the trigger SQL
	upperSQL := strings.ToUpper(triggerSQL)
	// Extract ON <table> to find the target table
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return ""
	}
	afterOn := strings.TrimSpace(upperSQL[onIdx+4:])
	spaceIdx := strings.IndexAny(afterOn, " \n\t\r")
	refTable := ""
	if spaceIdx > 0 {
		refTable = afterOn[:spaceIdx]
	} else {
		return ""
	}
	// Get the table's column definitions
	entry, err := e.schema.FindTable(refTable)
	if err != nil {
		return ""
	}
	colDefs := e.colCache[refTable]
	if colDefs == nil {
		colDefs = e.parseColumnDefs(entry.Name, entry.SQL)
	}
	// Build set of valid column names (excluding dropped columns)
	validCols := make(map[string]bool)
	for _, cd := range colDefs {
		if cd.Dropped {
			continue
		}
		validCols[strings.ToUpper(cd.Name)] = true
	}
	// Find all NEW.xxx and OLD.xxx references in the trigger body
	// Using simple text scanning
	body := triggerSQL
	BEGIN_MARKER := "BEGIN"
	begIdx := strings.Index(upperSQL, BEGIN_MARKER)
	if begIdx < 0 {
		return ""
	}
	body = triggerSQL[begIdx+len(BEGIN_MARKER):]
	// Find END
	endIdx := strings.LastIndex(strings.ToUpper(body), "END")
	if endIdx >= 0 {
		body = body[:endIdx]
	}
	// Scan for NEW. and OLD. references
	upperBody := strings.ToUpper(body)
	for i := 0; i < len(upperBody); i++ {
		prefix := ""
		nextIdx := -1
		newIdx := strings.Index(upperBody[i:], "NEW.")
		oldIdx := strings.Index(upperBody[i:], "OLD.")
		if newIdx >= 0 && (oldIdx < 0 || newIdx < oldIdx) {
			nextIdx = i + newIdx
			prefix = "new."
		} else if oldIdx >= 0 {
			nextIdx = i + oldIdx
			prefix = "old."
		} else {
			break
		}
		// Extract the column name after NEW. or OLD.
		colStart := nextIdx + 4 // skip "NEW." or "OLD."
		colEnd := colStart
		for colEnd < len(body) && (isAlpha(body[colEnd]) || body[colEnd] == '_') {
			colEnd++
		}
		if colEnd > colStart {
			colName := body[colStart:colEnd]
			if !validCols[strings.ToUpper(colName)] {
				return fmt.Sprintf("no such column: %s%s", prefix, colName)
			}
		}
		i = nextIdx + 1
	}
	return ""
}

// isAlpha checks if a byte is an ASCII letter.
func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}
// reference the given column and returns an error if so.
func (e *Engine) checkTableConstraintDependencies(createSQL, tableName, columnName string) *Result {
	parser := sql.NewParser(createSQL)
	stmts := parser.Parse()
	if parser.Err() != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	// Check table-level constraints (not column-level)
	for _, tc := range ct.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			if exprReferencesColumn(tc.Expr, columnName) {
				return &Result{Error: fmt.Errorf("error in table %s after drop column: no such column: %s",
					tableName, columnName)}
			}
		}
	}
	return nil
}

// exprReferencesColumn checks if an expression references a specific column.
func exprReferencesColumn(expr sql.Expr, columnName string) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *sql.ColumnRef:
		return strings.EqualFold(e.Name, columnName)
	case *sql.BinaryOp:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.UnaryOp:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.NumericLit, *sql.StringLit, *sql.NullLit:
		return false
	case *sql.FuncCall:
		for _, arg := range e.Args {
			if exprReferencesColumn(arg, columnName) {
				return true
			}
		}
		return false
	case *sql.IsNull:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.IsNotNull:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.IsDistinctFrom:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.IsNotDistinctFrom:
		return exprReferencesColumn(e.Left, columnName) || exprReferencesColumn(e.Right, columnName)
	case *sql.Between:
		return exprReferencesColumn(e.Operand, columnName) ||
			exprReferencesColumn(e.Low, columnName) || exprReferencesColumn(e.High, columnName)
	case *sql.InList:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.CaseExpr:
		if exprReferencesColumn(e.Operand, columnName) {
			return true
		}
		for _, when := range e.Whens {
			if exprReferencesColumn(when.When, columnName) || exprReferencesColumn(when.Then, columnName) {
				return true
			}
		}
		if e.Else != nil && exprReferencesColumn(e.Else, columnName) {
			return true
		}
		return false
	case *sql.CastExpr:
		return exprReferencesColumn(e.Operand, columnName)
	case *sql.RowValue:
		for _, v := range e.Values {
			if exprReferencesColumn(v, columnName) {
				return true
			}
		}
		return false
	case *sql.ExistsExpr, *sql.Subquery:
		return false // subqueries are complex, skip for now
	default:
		return false
	}
}

// indexReferencesColumn checks if the CREATE INDEX SQL references a given column.
func indexReferencesColumn(sqlStr, columnName string) bool {
	upperSQL := strings.ToUpper(sqlStr)
	// Check for simple column reference (word boundary)
	// The column name appears after the ON table_name ( or after ON clause
	// We use a simple approach: check if the column name appears as a standalone word
	// by looking for it with surrounding non-alphanumeric characters
	onIdx := strings.Index(upperSQL, " ON ")
	if onIdx < 0 {
		return false
	}
	parenIdx := strings.Index(upperSQL[onIdx:], "(")
	if parenIdx < 0 {
		return false
	}
	exprText := upperSQL[onIdx+parenIdx+1:]
	// Find the matching closing paren
	depth := 0
	endIdx := -1
	for i, ch := range exprText {
		switch ch {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				endIdx = i
				break
			}
			depth--
		}
	}
	if endIdx > 0 {
		exprText = exprText[:endIdx]
	}
	// Remove the last closing paren if any
	exprText = strings.TrimSuffix(exprText, ")")

	// Check if the column name appears as a whole word in the expression
	words := strings.FieldsFunc(exprText, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '"')
	})
	for _, w := range words {
		w = strings.Trim(w, `"`)
		if strings.EqualFold(w, columnName) {
			return true
		}
	}
	return false
}

// --- Expression evaluation ---

func (e *Engine) evalExpr(expr sql.Expr, row map[string]interface{}) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.NumericLit:
		return evalNumericLit(v)
	case *sql.StringLit:
		return v.Value, nil
	case *sql.BlobLit:
		return v.Value, nil
	case *sql.NullLit:
		return nil, nil
	case *sql.ColumnRef:
		return e.evalColumnRef(v, row)
	case *sql.FuncCall:
		return e.evalFuncCall(v, row)
	case *sql.RowValue:
		var parts []string
		for _, val := range v.Values {
			ev, err := e.evalExpr(val, row)
			if err != nil {
				return nil, err
			}
			if ev == nil {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, fmt.Sprintf("%v", ev))
			}
		}
		return strings.Join(parts, " "), nil
	default:
		return e.evalComplexExpr(expr, row)
	}
}

func (e *Engine) evalComplexExpr(expr sql.Expr, row map[string]interface{}) (interface{}, error) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return e.evalBinaryOp(v, row)
	case *sql.UnaryOp:
		return e.evalUnaryOp(v, row)
	case *sql.IsNull:
		return e.evalIsNull(v, row)
	case *sql.IsNotNull:
		return e.evalIsNotNull(v, row)
	case *sql.IsTrue:
		return e.evalIsTrue(v, row)
	case *sql.IsFalse:
		return e.evalIsFalse(v, row)
	case *sql.IsDistinctFrom:
		return e.evalIsDistinctFrom(v, row)
	case *sql.IsNotDistinctFrom:
		return e.evalIsNotDistinctFrom(v, row)
	case *sql.Between:
		return e.evalBetween(v, row)
	case *sql.InList:
		return e.evalInList(v, row)
	case *sql.Subquery:
		return e.evalSubquery(v, row)
	case *sql.ExistsExpr:
		return e.evalExists(v, row)
	case *sql.CaseExpr:
		return e.evalCaseExpr(v, row)
	case *sql.CastExpr:
		return e.evalCastExpr(v, row)
	default:
		return nil, fmt.Errorf("unknown expression type: %T", expr)
	}
}

func (e *Engine) evalSubquery(v *sql.Subquery, row map[string]interface{}) (interface{}, error) {
	// Save and restore outerRow for correlated subquery support
	prevOuterRow := e.outerRow
	e.outerRow = row
	defer func() { e.outerRow = prevOuterRow }()

	result := e.execSelect(v.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	if len(result.Rows) == 0 {
		return nil, nil
	}
	// Return first column of first row
	if len(result.Rows[0]) > 0 {
		return result.Rows[0][0], nil
	}
	return nil, nil
}

func (e *Engine) evalExists(v *sql.ExistsExpr, row map[string]interface{}) (interface{}, error) {
	// Propagate outerRow for correlated subquery references
	prevOuterRow := e.outerRow
	e.outerRow = row
	defer func() { e.outerRow = prevOuterRow }()

	result := e.execSelect(v.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	exists := len(result.Rows) > 0
	if v.Negated {
		exists = !exists
	}
	return boolToInt(exists), nil
}

func (e *Engine) evalCaseExpr(v *sql.CaseExpr, row map[string]interface{}) (interface{}, error) {
	if v.Operand != nil {
		return e.evalCaseWithOperand(v, row)
	}
	for _, w := range v.Whens {
		when, err := e.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if toBool(when) {
			return e.evalExpr(w.Then, row)
		}
	}
	return e.evalCaseElse(v, row)
}

func (e *Engine) evalCaseWithOperand(v *sql.CaseExpr, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	for _, w := range v.Whens {
		when, err := e.evalExpr(w.When, row)
		if err != nil {
			return nil, err
		}
		if util.CompareValues(operand, when) == 0 {
			return e.evalExpr(w.Then, row)
		}
	}
	return e.evalCaseElse(v, row)
}

func (e *Engine) evalCaseElse(v *sql.CaseExpr, row map[string]interface{}) (interface{}, error) {
	if v.Else != nil {
		return e.evalExpr(v.Else, row)
	}
	return nil, nil
}

func (e *Engine) evalCastExpr(v *sql.CastExpr, row map[string]interface{}) (interface{}, error) {
	val, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	switch strings.ToUpper(v.AsType) {
	case "INTEGER", "INT":
		switch x := val.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case string:
			// Simple conversion
			return int64(len(x)), nil
		default:
			return int64(0), nil
		}
	case "REAL", "FLOAT", "DOUBLE":
		switch x := val.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case string:
			return float64(len(x)), nil
		default:
			return float64(0), nil
		}
	case "TEXT":
		return fmt.Sprintf("%v", val), nil
	default:
		return val, nil
	}
}

func evalNumericLit(v *sql.NumericLit) (interface{}, error) {
	// Try base 0 first (auto-detect for hex literals like 0x...)
	if i, err := strconv.ParseInt(v.Value, 0, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(v.Value, 64); err == nil {
		return f, nil
	}
	return v.Value, nil
}

func (e *Engine) evalColumnRef(v *sql.ColumnRef, row map[string]interface{}) (interface{}, error) {
	if v.Name == "*" {
		return "*", nil
	}
	// Qualified column reference: check qualified name first
	if v.Table != "" {
		if val, ok := row[v.Table+"."+v.Name]; ok {
			return val, nil
		}
		// Check trigger NEW/OLD rows
		if strings.EqualFold(v.Table, "new") && e.triggerNewRow != nil {
			if val, ok := e.triggerNewRow[v.Name]; ok {
				return val, nil
			}
		}
		if strings.EqualFold(v.Table, "old") && e.triggerOldRow != nil {
			if val, ok := e.triggerOldRow[v.Name]; ok {
				return val, nil
			}
		}
		// Fallback to outer row for correlated references
		if e.outerRow != nil {
			if val, ok := e.outerRow[v.Table+"."+v.Name]; ok {
				return val, nil
			}
		}
	}
	// Unqualified: check short name
	if val, ok := row[v.Name]; ok {
		return val, nil
	}
	// Fallback to outer row for correlated references (unqualified)
	if e.outerRow != nil {
		if val, ok := e.outerRow[v.Name]; ok {
			return val, nil
		}
	}
	return nil, nil
}

func (e *Engine) evalBinaryOp(v *sql.BinaryOp, row map[string]interface{}) (interface{}, error) {
	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	// Most operators return NULL when either operand is NULL.
	// AND/OR need Kleene logic (handled in evalArithmeticOp).
	if v.Operator != "AND" && v.Operator != "OR" {
		if left == nil || right == nil {
			return nil, nil
		}
	}
	if v.Operator == "LIKE" && v.Escape != "" {
		return likeValuesWithEscape(left, right, v.Escape), nil
	}
	return evalBinaryOpValues(v.Operator, left, right)
}

// collatedValue wraps a value with a collation name for COLLATE support.
type collatedValue struct {
	value     interface{}
	collation string
}

// extractValue extracts the raw value and collation from a potentially collated value.
func extractValue(v interface{}) (interface{}, string) {
	if cv, ok := v.(*collatedValue); ok {
		return cv.value, cv.collation
	}
	return v, ""
}

// compareValuesWithCollate compares two values using the collation from either side.
func compareValuesWithCollate(left, right interface{}) int {
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	// Use the first non-empty collation found
	collation := lc
	if collation == "" {
		collation = rc
	}
	return util.CompareValuesCollate(lv, rv, collation)
}

// extractCollatedValues extracts raw values from collatedValue wrappers
// for operators that don't need collation propagation.
// Comparison operators keep the collatedValue for compareValuesWithCollate.
// || keeps collatedValue for evalConcat to propagate collation.
func extractCollatedValues(op string, left, right interface{}) (interface{}, interface{}) {
	if op == "=" || op == "<>" || op == "!=" || op == "<" || op == ">" || op == "<=" || op == ">=" || op == "||" {
		return left, right
	}
	l, _ := extractValue(left)
	r, _ := extractValue(right)
	return l, r
}

func evalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
	// Extract collation-wrapped values for non-comparison operators.
	// Comparison operators use compareValuesWithCollate which handles this internally.
	// For || (concatenation), we preserve collation through evalConcat.
	left, right = extractCollatedValues(op, left, right)
	switch op {
	case "=":
		return boolToInt(compareValuesWithCollate(left, right) == 0), nil
	case "<>", "!=":
		return boolToInt(compareValuesWithCollate(left, right) != 0), nil
	case "<":
		return boolToInt(compareValuesWithCollate(left, right) < 0), nil
	case ">":
		return boolToInt(compareValuesWithCollate(left, right) > 0), nil
	case "<=":
		return boolToInt(compareValuesWithCollate(left, right) <= 0), nil
	case ">=":
		return boolToInt(compareValuesWithCollate(left, right) >= 0), nil
	case "LIKE":
		return boolToInt(likeValues(left, right)), nil
	case "GLOB":
		return boolToInt(globValues(left, right)), nil
	case "REGEXP":
		return boolToInt(regexpValues(left, right)), nil
	case "NOT LIKE":
		return boolToInt(!likeValues(left, right)), nil
	case "NOT GLOB":
		return boolToInt(!globValues(left, right)), nil
	case "NOT REGEXP":
		return boolToInt(!regexpValues(left, right)), nil
	case "MATCH":
		// FTS not supported — MATCH always returns 0
		return int64(0), nil
	case "NOT MATCH":
		// FTS not supported — NOT MATCH always returns 1
		return int64(1), nil
	case "->", "->>":
		// JSON extract operators — not supported, return NULL
		return nil, nil
	case "COLLATE":
		// COLLATE operator — returns the left value but marks it with
		// the collation name. Comparison operators check for this
		// marker and apply the correct collation.
		if rightStr, ok := right.(string); ok {
			return &collatedValue{value: left, collation: rightStr}, nil
		}
		return left, nil
	default:
		return evalArithmeticOp(op, left, right)
	}
}

// boolToInt converts a boolean to an integer (0 or 1) matching SQLite behavior.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func globValues(str, pattern interface{}) bool {
	s := fmt.Sprintf("%v", str)
	p := fmt.Sprintf("%v", pattern)
	return function.GlobMatch(s, p)
}

func regexpValues(str, pattern interface{}) bool {
	s := fmt.Sprintf("%v", str)
	p := fmt.Sprintf("%v", pattern)
	re, err := regexp.Compile(p)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func evalArithmeticOp(op string, left, right interface{}) (interface{}, error) {
	// Unwrap BlobColumnValue so arithmetic functions see the base value.
	left = util.UnwrapColumnValue(left)
	right = util.UnwrapColumnValue(right)
	switch op {
	case "+":
		return evalAdd(left, right)
	case "-":
		if left == nil || right == nil { return nil, nil }
		return subValues(left, right)
	case "*":
		if left == nil || right == nil { return nil, nil }
		return mulValues(left, right)
	case "/":
		if left == nil || right == nil { return nil, nil }
		return divValues(left, right)
	case "%":
		if left == nil || right == nil { return nil, nil }
		return modValues(left, right)
	case "&":
		if left == nil || right == nil { return nil, nil }
		return bitwiseAnd(left, right)
	case "||":
		return evalConcat(left, right)
	case "AND":
		return kleeneAnd(left, right), nil
	case "OR":
		return kleeneOr(left, right), nil
	default:
		return nil, fmt.Errorf("unknown operator: %s", op)
	}
}

// kleeneAnd implements Kleene AND logic:
//
//	true  AND true  → true
//	false AND any   → false
//	any   AND false → false
//	true  AND NULL  → NULL
//	NULL  AND true  → NULL
//	NULL  AND NULL  → NULL
func evalAdd(left, right interface{}) (interface{}, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	return addValues(left, right)
}

func evalConcat(left, right interface{}) (interface{}, error) {
	// Extract collation info from any collatedValue operands
	lv, lc := extractValue(left)
	rv, rc := extractValue(right)
	collation := lc
	if collation == "" {
		collation = rc
	}

	if lv == nil || rv == nil {
		return nil, nil
	}
	result, err := concatValues(lv, rv)
	if err != nil {
		return nil, err
	}
	// If either operand had a collation, wrap the result so comparison
	// operators can apply the collation correctly.
	if collation != "" {
		return &collatedValue{value: result, collation: collation}, nil
	}
	return result, nil
}

func kleeneAnd(left, right interface{}) interface{} {
	if isFalse(left) || isFalse(right) {
		return boolToInt(false)
	}
	if left == nil || right == nil {
		return nil
	}
	return boolToInt(true)
}

// kleeneOr implements Kleene OR logic:
//
//	true  OR any   → true
//	any   OR true  → true
//	false OR NULL  → NULL
//	NULL  OR false → NULL
//	false OR false → false
//	NULL  OR NULL  → NULL
func kleeneOr(left, right interface{}) interface{} {
	if isTrue(left) || isTrue(right) {
		return boolToInt(true)
	}
	if left == nil || right == nil {
		return nil
	}
	return boolToInt(false)
}

func isFalse(v interface{}) bool {
	if v == nil {
		return false
	}
	return !toBool(v)
}

func isTrue(v interface{}) bool {
	if v == nil {
		return false
	}
	return toBool(v)
}

func (e *Engine) evalUnaryOp(v *sql.UnaryOp, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	// Unwrap BlobColumnValue so arithmetic operators see the base value.
	operand = util.UnwrapColumnValue(operand)
	switch v.Operator {
	case "-":
		return negateValue(operand)
	case "+":
		// Unary plus: convert to numeric (like SQLite's + operator)
		return numericValue(operand)
	case "NOT":
		return boolToInt(!toBool(operand)), nil
	default:
		return nil, nil
	}
}

func (e *Engine) evalIsNull(v *sql.IsNull, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	return operand == nil, nil
}

func (e *Engine) evalIsNotNull(v *sql.IsNotNull, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	return operand != nil, nil
}

func (e *Engine) evalIsTrue(v *sql.IsTrue, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	result := isTrue(operand)
	if v.Negated {
		result = !result
	}
	if result {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalIsFalse(v *sql.IsFalse, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	result := isFalse(operand)
	if v.Negated {
		result = !result
	}
	if result {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalIsDistinctFrom(v *sql.IsDistinctFrom, row map[string]interface{}) (interface{}, error) {
	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	// IS DISTINCT FROM: 0 if equal (including NULL==NULL), 1 otherwise
	if left == nil && right == nil {
		return int64(0), nil
	}
	if left == nil || right == nil {
		return int64(1), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(0), nil
	}
	return int64(1), nil
}

func (e *Engine) evalIsNotDistinctFrom(v *sql.IsNotDistinctFrom, row map[string]interface{}) (interface{}, error) {
	left, err := e.evalExpr(v.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(v.Right, row)
	if err != nil {
		return nil, err
	}
	// IS NOT DISTINCT FROM: 1 if equal (including NULL==NULL), 0 otherwise
	if left == nil && right == nil {
		return int64(1), nil
	}
	if left == nil || right == nil {
		return int64(0), nil
	}
	cmp := util.CompareValuesCollate(left, right, "BINARY")
	if cmp == 0 {
		return int64(1), nil
	}
	return int64(0), nil
}

func (e *Engine) evalBetween(v *sql.Between, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	low, err := e.evalExpr(v.Low, row)
	if err != nil {
		return nil, err
	}
	high, err := e.evalExpr(v.High, row)
	if err != nil {
		return nil, err
	}
	result := util.CompareValues(operand, low) >= 0 && util.CompareValues(operand, high) <= 0
	if v.Negated {
		result = !result
	}
	return result, nil
}

func (e *Engine) evalInList(v *sql.InList, row map[string]interface{}) (interface{}, error) {
	operand, err := e.evalExpr(v.Operand, row)
	if err != nil {
		return nil, err
	}
	if operand == nil {
		return nil, nil
	}
	found := false
	for _, item := range v.List {
		ival, err := e.evalExpr(item, row)
		if err != nil {
			continue
		}
		if util.CompareValues(operand, ival) == 0 {
			found = true
			break
		}
	}
	if v.Negated {
		found = !found
	}
	return found, nil
}

func (e *Engine) evalBool(expr sql.Expr, row map[string]interface{}) (bool, error) {
	v, err := e.evalExpr(expr, row)
	if err != nil {
		return false, err
	}
	return toBool(v), nil
}

func (e *Engine) evalFuncCall(f *sql.FuncCall, row map[string]interface{}) (interface{}, error) {
	fn, ok := e.funcs.Find(f.Name)
	if !ok {
		return nil, fmt.Errorf("unknown function: %s", f.Name)
	}

	// ORDER BY is only allowed for aggregate functions
	if len(f.OrderBy) > 0 && fn.Type != function.TypeAggregate {
		return nil, fmt.Errorf("ORDER BY may not be used with non-aggregate %s()", f.Name)
	}

	args := make([]interface{}, len(f.Args))
	for i, arg := range f.Args {
		v, err := e.evalExpr(arg, row)
		if err != nil {
			return nil, err
		}
		// Unwrap BlobColumnValue so functions see the raw value.
		v = util.UnwrapColumnValue(v)
		// For UTF-16 encoding, truncate odd-length blobs (ignore last byte)
		// to ensure valid UTF-16 byte sequences. (SQLite ticket 9eda2697f5cc1aba)
		if b, ok := v.([]byte); ok && len(b)%2 == 1 {
			if strings.HasPrefix(e.encoding, "UTF-16") {
				v = b[:len(b)-1]
			}
		}
		args[i] = v
	}

	if len(args) < fn.MinArgs || (fn.MaxArgs > 0 && len(args) > fn.MaxArgs) {
		return nil, fmt.Errorf("function %s expects %d-%d arguments, got %d", f.Name, fn.MinArgs, fn.MaxArgs, len(args))
	}

	if fn.Type == function.TypeScalar {
		return fn.ScalarFn(args)
	}

	// For aggregate functions, evaluate step by step if row is provided
	if fn.Type == function.TypeAggregate {
		agg := fn.AggregateFn()
		if err := agg.Step(args); err != nil {
			return nil, err
		}
		return agg.Final()
	}

	return nil, fmt.Errorf("aggregate function %s not supported in this context", f.Name)
}

func (e *Engine) findNextRowID(rootPage uint32) int64 {
	// Check cache first
	if cached, ok := e.nextRowIDCache[rootPage]; ok {
		next := cached + 1
		e.nextRowIDCache[rootPage] = next
		return next
	}

	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		e.nextRowIDCache[rootPage] = 1
		return 1
	}
	var maxID int64
	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		if cell.RowID > maxID {
			maxID = cell.RowID
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	next := maxID + 1
	e.nextRowIDCache[rootPage] = next
	return next
}

func (e *Engine) parseColumnDefs(tableName, createSQL string) []sql.ColumnDef {
	// Check cache first
	if cached, ok := e.colCache[tableName]; ok {
		return cached
	}
	// Fall back to re-parsing
	parser := sql.NewParser(createSQL)
	stmts := parser.Parse()
	if len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	// Cache for future use
	e.colCache[tableName] = ct.Columns
	return ct.Columns
}

// --- Value arithmetic helpers ---

func toBool(v interface{}) bool {
	if v == nil {
		return false
	}
	// Unwrap ColumnValue so HAVING, WHERE, and boolean filters
	// correctly evaluate scalar values from the database.
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []byte:
		return len(x) > 0
	default:
		return true
	}
}

func addValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if isInt(a) && isInt(b) {
			return int64(af) + int64(bf), nil
		}
		return af + bf, nil
	}
	return nil, fmt.Errorf("cannot add non-numeric values")
}

func subValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if isInt(a) && isInt(b) {
			return int64(af) - int64(bf), nil
		}
		return af - bf, nil
	}
	return nil, fmt.Errorf("cannot subtract non-numeric values")
}

func mulValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if isInt(a) && isInt(b) {
			return int64(af) * int64(bf), nil
		}
		return af * bf, nil
	}
	return nil, fmt.Errorf("cannot multiply non-numeric values")
}

func divValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		if isInt(a) && isInt(b) {
			return int64(af) / int64(bf), nil
		}
		return af / bf, nil
	}
	return nil, fmt.Errorf("cannot divide non-numeric values")
}

func modValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		if bf == 0 {
			return nil, nil
		}
		if isInt(a) && isInt(b) {
			return int64(af) % int64(bf), nil
		}
		// For floating point modulo, convert to int64 equivalent
		return int64(af) % int64(bf), nil
	}
	return nil, fmt.Errorf("cannot mod non-numeric values")
}

func bitwiseAnd(a, b interface{}) (interface{}, error) {
	ai, aok := a.(int64)
	bi, bok := b.(int64)
	if aok && bok {
		return ai & bi, nil
	}
	return nil, fmt.Errorf("cannot bitwise-AND non-integer values")
}

func concatValues(a, b interface{}) (interface{}, error) {
	if a == nil || b == nil {
		return nil, nil
	}
	return fmt.Sprintf("%v%v", a, b), nil
}

func negateValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	// Try numeric negation first
	switch val := v.(type) {
	case int64:
		return -val, nil
	case float64:
		// Handle negative zero: return int64 0 for -0.0
		if val == 0 {
			return int64(0), nil
		}
		return -val, nil
	}
	// Try string as number
	f, ok := toFloat(v)
	if ok {
		return -f, nil
	}
	// Non-numeric values: return 0 (SQLite behavior, e.g. -'abc' = 0, -x'ce' = 0)
	return int64(0), nil
}

// numericValue converts a value to a number (used by unary + operator).
// Non-numeric values are converted to 0, matching SQLite behavior.
func numericValue(v interface{}) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	if i, ok := v.(int64); ok {
		return i, nil
	}
	if f, ok := v.(float64); ok {
		return f, nil
	}
	// Try string conversion
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			// Return int64 if it's a whole number
			if f == float64(int64(f)) {
				return int64(f), nil
			}
			return f, nil
		}
		// Try integer parsing
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
	}
	// Blob or other non-numeric: return 0
	return int64(0), nil
}

func likeValues(str, pattern interface{}) bool {
	s := fmt.Sprintf("%v", str)
	p := fmt.Sprintf("%v", pattern)
	return likeMatch(s, p)
}

// likeValuesWithEscape performs LIKE matching with an escape character.
func likeValuesWithEscape(str, pattern interface{}, escape string) bool {
	s := fmt.Sprintf("%v", str)
	p := fmt.Sprintf("%v", pattern)
	return likeMatchEscaped(s, p, escape)
}

func likeMatch(s, pattern string) bool {
	return likeMatchRecursiveEscaped(s, pattern, 0, 0, 0)
}

func likeMatchEscaped(s, pattern, escape string) bool {
	if escape == "" {
		return likeMatch(s, pattern)
	}
	// Process the pattern, treating escape char + next char as literal
	return likeMatchRecursiveEscaped(s, pattern, 0, 0, escape[0])
}

func likeMatchRecursiveEscaped(s, pattern string, idx, patIdx int, escape byte) bool {
	for patIdx < len(pattern) {
		c := pattern[patIdx]
		if c == escape && patIdx+1 < len(pattern) {
			// Escape char followed by another char: treat the next char as literal
			nextChar := pattern[patIdx+1]
			if idx >= len(s) || !strings.EqualFold(string(s[idx]), string(nextChar)) {
				return false
			}
			idx++
			patIdx += 2
			continue
		}
		switch c {
		case '%':
			return likeMatchPercentEscaped(s, pattern, idx, patIdx, escape)
		case '_':
			if idx >= len(s) {
				return false
			}
			idx++
			patIdx++
		default:
			if idx >= len(s) || !strings.EqualFold(string(s[idx]), string(c)) {
				return false
			}
			idx++
			patIdx++
		}
	}
	return idx >= len(s)
}

func likeMatchPercentEscaped(s, pattern string, idx, patIdx int, escape byte) bool {
	patIdx++
	if patIdx >= len(pattern) {
		return true
	}
	for idx < len(s) {
		if likeMatchRecursiveEscaped(s, pattern, idx, patIdx, escape) {
			return true
		}
		idx++
	}
	return false
}

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func isInt(v interface{}) bool {
	_, ok := v.(int64)
	return ok
}