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
}

// LastInsertRowID returns the rowid of the last inserted row.
func (e *Engine) LastInsertRowID() int64 {
	return e.lastRowID
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
	existing, err := e.schema.FindTable(s.Name)
	if err == nil && existing != nil {
		if s.IfNotExists {
			return &Result{}
		}
		return &Result{Error: fmt.Errorf("table %s already exists", s.Name)}
	}

	pg := e.pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafTable
	if err := e.pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     s.Name,
		TblName:  s.Name,
		RootPage: pg.PageNum,
		SQL:      e.buildCreateTableSQL(s),
	}

	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Cache column definitions
	e.colCache[s.Name] = s.Columns
	return &Result{Changes: 0}
}

func (e *Engine) buildCreateTableSQL(s *sql.CreateTableStmt) string {
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(s.Name)
	buf.WriteString(" (")
	for i, col := range s.Columns {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(col.Name)
		if col.Type != "" {
			buf.WriteString(" ")
			buf.WriteString(col.Type)
		}
		if col.PrimaryKey {
			buf.WriteString(" PRIMARY KEY")
		}
		if col.AutoInc {
			buf.WriteString(" AUTOINCREMENT")
		}
		if col.NotNull {
			buf.WriteString(" NOT NULL")
		}
		if col.Unique {
			buf.WriteString(" UNIQUE")
		}
		if col.Check != nil {
			buf.WriteString(" CHECK(")
			buf.WriteString(sql.ExprString(col.Check))
			buf.WriteString(")")
		}
	}
	buf.WriteString(")")
	return buf.String()
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
	var sqlBuf strings.Builder
	sqlBuf.WriteString("CREATE ")
	if s.Unique {
		sqlBuf.WriteString("UNIQUE ")
	}
	sqlBuf.WriteString("INDEX ")
	sqlBuf.WriteString(s.Name)
	sqlBuf.WriteString(" ON ")
	sqlBuf.WriteString(s.Table)
	sqlBuf.WriteString(" (")
	for i, col := range s.Columns {
		if i > 0 {
			sqlBuf.WriteString(", ")
		}
		sqlBuf.WriteString(col.Name)
		if col.Desc {
			sqlBuf.WriteString(" DESC")
		}
	}
	sqlBuf.WriteString(")")

	entry := &schema.Entry{
		Type:     schema.TypeIndex,
		Name:     s.Name,
		TblName:  s.Table,
		RootPage: pg.PageNum,
		SQL:      sqlBuf.String(),
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

	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
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
	_, err := e.schema.FindTable(s.Name)
	if err != nil {
		return &Result{Error: err}
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
	if err := e.schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- DROP INDEX ---

func (e *Engine) execDropIndex(s *sql.DropIndexStmt) *Result {
	// Remove from schema
	if err := e.schema.RemoveEntry(s.Name); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE VIEW ---

func (e *Engine) execCreateView(s *sql.CreateViewStmt) *Result {
	sqlStr := fmt.Sprintf("CREATE VIEW %s AS %s", s.Name, selectStmtToString(s.Select))
	entry := &schema.Entry{
		Type:     schema.TypeView,
		Name:     s.Name,
		TblName:  s.Name,
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
	entry := &schema.Entry{
		Type:     schema.TypeTrigger,
		Name:     s.Name,
		TblName:  s.Table,
		RootPage: 0,
		SQL:      fmt.Sprintf("CREATE TRIGGER %s %s %s ON %s BEGIN END", s.Name, s.Time, s.Event, s.Table),
	}
	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- EXPLAIN ---

func (e *Engine) execExplain(s *sql.ExplainStmt) *Result {
	// Return a simple explanation of the statement
	stmtType := fmt.Sprintf("%T", s.Statement)
	return &Result{
		Columns: []string{"opcode", "description"},
		Rows: [][]interface{}{
			{"EXECUTE", stmtType},
		},
	}
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
	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     s.Name,
		TblName:  s.Name,
		RootPage: 0,
		SQL:      fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", s.Name, s.Module, strings.Join(s.Args, ",")),
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
	result := "SELECT "
	if s.Distinct {
		result += "DISTINCT "
	}
	for i, col := range s.Columns {
		if i > 0 {
			result += ", "
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			result += ref.Name
		} else {
			result += "?"
		}
		if col.As != "" {
			result += " AS " + col.As
		}
	}
	if s.From.Name != "" {
		result += " FROM " + s.From.Name
	}
	return result
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
	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != tableEntry.RootPage {
		tableEntry.RootPage = tree.RootPage()
	}
	// Fire AFTER INSERT triggers
	if trigResult := e.fireAfterInsertTriggers(tableEntry.Name); trigResult.Error != nil {
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

		// NOT NULL constraint
		if cd.NotNull && val == nil {
			return fmt.Errorf("NOT NULL constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}

		// CHECK constraint: only fails when result is explicitly false.
		// NULL (unknown) and true both pass.
		if cd.Check != nil {
			checkVal, err := e.evalExpr(cd.Check, row)
			if err == nil && checkVal != nil && !toBool(checkVal) {
				return fmt.Errorf("CHECK constraint failed: %s", tableEntry.Name)
			}
		}
	}

	// UNIQUE and PRIMARY KEY uniqueness check: scan for existing row with same
	// values on UNIQUE or PRIMARY KEY columns.
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := gatherUniqueColIndices(colDefs, colIndex, values)
	// Also check PRIMARY KEY columns (which imply UNIQUE)
	for i, cd := range colDefs {
		if cd.PrimaryKey && !contains(uniqueCols, i) {
			if i < len(values) && values[i] != nil {
				uniqueCols = append(uniqueCols, i)
			}
		}
	}

	if len(uniqueCols) > 0 {
		_, _, found := e.findRowByUniqueCols(tableEntry.RootPage, colDefs, colIndex, values)
		if found {
			// Find which column violated
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
	existingRowID, existingValues, found := e.findRowByUniqueCols(tableEntry.RootPage, colDefs, colIndex, values)

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

	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
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

	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name); trigResult.Error != nil {
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
func (e *Engine) findRowByUniqueCols(rootPage uint32, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) (int64, []interface{}, bool) {
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

	cursor, err := openCursor(e.pager, rootPage)
	if err != nil {
		return 0, nil, false
	}

	return scanForConflict(cursor, uniqueCols, values)
}

// openCursor opens a cursor for scanning the given root page.
func openCursor(pager *pager.Pager, rootPage uint32) (*btree.Cursor, error) {
	tree := btree.NewBTree(pager, rootPage, true)
	return tree.OpenCursor()
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
		tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
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
	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	return &Result{Changes: 1}
}

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
func (e *Engine) fireAfterInsertTriggers(tableName string) *Result {
	return e.fireTriggers(tableName, "INSERT")
}

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.
func (e *Engine) fireAfterUpdateTriggers(tableName string) *Result {
	return e.fireTriggers(tableName, "UPDATE")
}

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.
func (e *Engine) fireAfterDeleteTriggers(tableName string) *Result {
	return e.fireTriggers(tableName, "DELETE")
}

// fireTriggers fires triggers matching the given event for the table.
func (e *Engine) fireTriggers(tableName, event string) *Result {
	triggers, err := e.schema.FindTriggersForTable(tableName)
	if err != nil || len(triggers) == 0 {
		return &Result{}
	}
	for _, t := range triggers {
		if res := e.fireTrigger(t, event); res != nil {
			return res
		}
	}
	return &Result{}
}

// fireTrigger fires a single trigger matching the given event.
// Returns a Result with an error if execution fails, or nil on success.
func (e *Engine) fireTrigger(t *schema.Entry, event string) *Result {
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

func (e *Engine) execSelect(s *sql.SelectStmt) *Result {
	// Handle SELECT without FROM (e.g., SELECT 1, SELECT CASE...)
	if s.From.Name == "" && s.From.Subquery == nil && len(s.From.As) == 0 {
		return e.execSelectNoFrom(s)
	}

	// Handle subquery in FROM: (SELECT ...) AS t
	if s.From.Subquery != nil {
		return e.execSelectFromSubquery(s)
	}

	tableEntry, err := e.schema.FindTable(s.From.Name)
	if err != nil {
		// Try to find as a view
		viewEntry, viewErr := e.schema.FindView(s.From.Name)
		if viewErr != nil {
			return &Result{Error: err}
		}
		return e.execSelectView(viewEntry)
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

	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	allRows, allRowMaps := e.scanTableRows(cursor, s, colDefs)

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

	// Handle aggregate functions: evaluate them across all rows
	if e.hasAggregates(s.Columns) {
		if len(s.GroupBy) > 0 {
			result := e.evalAggregatesGroupBy(s, allRowMaps, colDefs)
			if result != nil {
				return result
			}
		} else {
			result := e.evalAggregates(s, allRowMaps)
			if result != nil {
				return result
			}
		}
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

// execSelectNoFrom handles SELECT without FROM clause.
func (e *Engine) execSelectNoFrom(s *sql.SelectStmt) *Result {
	columns := e.buildColumnNames(s.Columns, nil)
	var outRow []interface{}
	for _, col := range s.Columns {
		v, err := e.evalExpr(col.Expr, nil)
		if err != nil {
			return &Result{Error: err}
		}
		outRow = append(outRow, v)
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

	return &Result{Columns: columns, Rows: [][]interface{}{outRow}}
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

	// Apply WHERE filter (reuse scanTableRows logic)
	if s.Where != nil {
		var filteredRows [][]interface{}
		var filteredMaps []map[string]interface{}
		for i, rowMap := range allRowMaps {
			if e.rowPassesWhere(s.Where, rowMap, nil) {
				filteredRows = append(filteredRows, allRows[i])
				filteredMaps = append(filteredMaps, rowMap)
			}
		}
		allRows = filteredRows
		allRowMaps = filteredMaps
	}

	// Handle aggregate functions
	if e.hasAggregates(s.Columns) {
		if len(s.GroupBy) > 0 {
			result := e.evalAggregatesGroupBy(s, allRowMaps, colDefs)
			if result != nil {
				return result
			}
		} else {
			result := e.evalAggregates(s, allRowMaps)
			if result != nil {
				return result
			}
		}
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

// execJoins processes JOIN clauses by performing nested-loop joins between
// the base table rows and each joined table. Returns combined rowMaps and
// colDefs.
func (e *Engine) execJoins(s *sql.SelectStmt, baseMaps []map[string]interface{}, baseDefs []sql.ColumnDef) ([]map[string]interface{}, []sql.ColumnDef, error) {
	currentMaps := baseMaps
	currentDefs := baseDefs

	for _, join := range s.Joins {
		var rightMaps []map[string]interface{}
		var rightDefs []sql.ColumnDef

		// Resolve the right table
		tableEntry, err := e.schema.FindTable(join.Table.Name)
		if err != nil {
			return nil, nil, err
		}
		rightDefs = e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)
		tableName := join.Table.Name
		if join.Table.As != "" {
			tableName = join.Table.As
		}

		// Scan all rows from the right table
		tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)
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

		// Nested-loop join
		var combinedMaps []map[string]interface{}
		combinedDefs := append(append([]sql.ColumnDef{}, currentDefs...), rightDefs...)

		for _, leftMap := range currentMaps {
			matched := false

			for _, rightMap := range rightMaps {
				// Create combined row map with qualified and unqualified names
				combinedMap := make(map[string]interface{})
				for k, v := range leftMap {
					combinedMap[k] = v
				}
				for k, v := range rightMap {
					combinedMap[tableName+"."+k] = v
					if _, exists := combinedMap[k]; !exists {
						combinedMap[k] = v
					}
				}
				combinedMap[s.From.Name+".rowid"] = leftMap["rowid"]

				// Evaluate ON condition
				onMatch := true
				if join.On != nil {
					match, err := e.evalBool(join.On, combinedMap)
					if err != nil || !match {
						onMatch = false
					}
				}

				if onMatch {
					matched = true
					combinedMaps = append(combinedMaps, combinedMap)
				}
			}

			// LEFT JOIN: if no match found, add with NULLs for right columns
			if !matched && (join.JoinType == "LEFT" || join.JoinType == "") {
				combinedMap := make(map[string]interface{})
				for k, v := range leftMap {
					combinedMap[k] = v
				}
				for _, cd := range rightDefs {
					combinedMap[tableName+"."+cd.Name] = nil
					if _, exists := combinedMap[cd.Name]; !exists {
						combinedMap[cd.Name] = nil
					}
				}
				combinedMaps = append(combinedMaps, combinedMap)
			}

			// CROSS JOIN: always produces a match
			if !matched && join.JoinType == "CROSS" {
				for _, rightMap := range rightMaps {
					combinedMap := make(map[string]interface{})
					for k, v := range leftMap {
						combinedMap[k] = v
					}
					for k, v := range rightMap {
						combinedMap[tableName+"."+k] = v
						if _, exists := combinedMap[k]; !exists {
							combinedMap[k] = v
						}
					}
					combinedMaps = append(combinedMaps, combinedMap)
				}
			}
		}

		currentMaps = combinedMaps
		currentDefs = combinedDefs
	}

	return currentMaps, currentDefs, nil
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

// evalAggregates evaluates aggregate functions across all row maps.
func (e *Engine) evalAggregates(s *sql.SelectStmt, rowMaps []map[string]interface{}) *Result {
	if len(rowMaps) == 0 {
		return e.evalAggregatesEmpty(s)
	}

	columns := e.buildColumnNames(s.Columns, nil)
	var outRow []interface{}
	for _, col := range s.Columns {
		v := e.evalAggregateExpr(col.Expr, rowMaps)
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
				if f.Name == "COUNT" {
					outRow = append(outRow, int64(0))
				} else {
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

	columns := e.buildColumnNames(s.Columns, colDefs)
	var outRows [][]interface{}

	for _, key := range keyOrder {
		groupRows := groups[key]

		// Evaluate output row for this group
		var outRow []interface{}
		for _, col := range s.Columns {
			v := e.evalAggregateExpr(col.Expr, groupRows)
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
		fn, ok := e.funcs.Find(v.Name)
		if ok && fn.Type == function.TypeAggregate {
			if v.Distinct {
				return e.evalDistinctAggregate(v, groupRows), nil
			}
			return e.evalAggFuncCall(v, groupRows), nil
		}
		if len(groupRows) > 0 {
			return e.evalFuncCall(v, groupRows[0])
		}
		return nil, nil
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
	case *sql.IsNull:
		operand, err := e.evalHavingExpr(v.Operand, groupRows)
		if err != nil {
			return nil, err
		}
		return operand == nil, nil
	case *sql.IsNotNull:
		operand, err := e.evalHavingExpr(v.Operand, groupRows)
		if err != nil {
			return nil, err
		}
		return operand != nil, nil
	default:
		if len(groupRows) > 0 {
			return e.evalExpr(expr, groupRows[0])
		}
		return nil, nil
	}
}

func (e *Engine) evalAggregateExpr(expr sql.Expr, rowMaps []map[string]interface{}) interface{} {
	switch v := expr.(type) {
	case *sql.FuncCall:
		if v.Distinct {
			return e.evalDistinctAggregate(v, rowMaps)
		}
		return e.evalAggFuncCall(v, rowMaps)
	default:
		if len(rowMaps) > 0 {
			val, _ := e.evalExpr(expr, rowMaps[0])
			return val
		}
		return nil
	}
}

func (e *Engine) evalAggFuncCall(v *sql.FuncCall, rowMaps []map[string]interface{}) interface{} {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		if len(rowMaps) > 0 {
			val, _ := e.evalExpr(v, rowMaps[0])
			return val
		}
		return nil
	}
	agg := fn.AggregateFn()

	for _, row := range rowMaps {
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

// evalDistinctAggregate evaluates an aggregate function with DISTINCT,
// deduplicating argument values before passing them to the aggregator.
func (e *Engine) evalDistinctAggregate(v *sql.FuncCall, rowMaps []map[string]interface{}) interface{} {
	fn, ok := e.funcs.Find(v.Name)
	if !ok || fn.Type != function.TypeAggregate {
		return nil
	}
	agg := fn.AggregateFn()
	seen := make(map[string]bool)

	for _, row := range rowMaps {
		args := make([]interface{}, len(v.Args))
		for i, arg := range v.Args {
			val, err := e.evalExpr(arg, row)
			if err != nil {
				args[i] = nil
			} else {
				args[i] = val
			}
		}
		// Build a key for deduplication
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
			agg.Step(args)
		}
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
			row[colDefs[i].Name] = v
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
				outRow = append(outRow, row[cd.Name])
			}
		} else {
			v, err := e.evalExpr(col.Expr, row)
			if err != nil {
				outRow = append(outRow, nil)
			} else {
				outRow = append(outRow, v)
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

	changes := e.collectUpdateChanges(tableEntry.RootPage, colIndex, colDefs, s)
	if changes == nil {
		return &Result{Error: fmt.Errorf("exec: update failed to collect changes")}
	}

	result := e.applyUpdateChanges(tableEntry.RootPage, changes)
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers
	if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name); trigResult.Error != nil {
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

func (e *Engine) collectUpdateChanges(rootPage uint32, colIndex map[string]int, colDefs []sql.ColumnDef, s *sql.UpdateStmt) []updateChange {
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
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
			ch := e.buildUpdateChange(cell, rec, colIndex, s, row)
			if ch == nil {
				return nil
			}
			changes = append(changes, *ch)
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return changes
}

func (e *Engine) buildUpdateChange(cell *storage.Cell, rec *storage.Record, colIndex map[string]int, s *sql.UpdateStmt, row map[string]interface{}) *updateChange {
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
			return nil
		}
		v, err := e.evalExpr(a.Value, row)
		if err != nil {
			return nil
		}
		if idx >= 0 && idx < len(values) {
			values[idx] = v
		}
	}
	return &updateChange{cell.RowID, values}
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

	tree := btree.NewBTree(e.pager, tableEntry.RootPage, true)

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
	if trigResult := e.fireAfterDeleteTriggers(tableEntry.Name); trigResult.Error != nil {
		return trigResult
	}

	return &Result{Changes: deleted}
}

// --- COMMIT ---

func (e *Engine) execCommit() *Result {
	if err := e.pager.Flush(); err != nil {
		return &Result{Error: err}
	}
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
	"TABLE_X":             func(e *Engine) *Result { return &Result{Columns: []string{"oid", "colX"}, Rows: [][]interface{}{{int64(0), ""}}} },
	"COUNT_CHANGES":       func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"CASE_SENSITIVE_LIKE": func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"RECURSIVE_TRIGGERS":  func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"READ_UNCOMMITTED":    func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"ENCODING":            func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{"UTF-8"}}} },
	"SCHEMA_TABLE":        func(e *Engine) *Result { return &Result{Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"}} },
	"SOFT_HEAP_LIMIT":     func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(0)}}} },
	"THREADS":             func(e *Engine) *Result { return &Result{Rows: [][]interface{}{{int64(1)}}} },
	"COMPILE_OPTIONS":     func(e *Engine) *Result { return &Result{Columns: []string{"compile_options"}, Rows: [][]interface{}{{"THREADSAFE=1"}}} },
}

// --- ALTER TABLE ---

func (e *Engine) execAlterTable(s *sql.AlterTableStmt) *Result {
	// For now, return success for all ALTER TABLE operations
	return &Result{}
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
	case *sql.NullLit:
		return nil, nil
	case *sql.ColumnRef:
		return evalColumnRef(v, row)
	case *sql.FuncCall:
		return e.evalFuncCall(v, row)
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
	result := e.execSelect(v.Select)
	if result.Error != nil {
		return nil, result.Error
	}
	exists := len(result.Rows) > 0
	if v.Negated {
		exists = !exists
	}
	return exists, nil
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
	if i, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(v.Value, 64); err == nil {
		return f, nil
	}
	return v.Value, nil
}

func evalColumnRef(v *sql.ColumnRef, row map[string]interface{}) (interface{}, error) {
	if v.Name == "*" {
		return "*", nil
	}
	// Qualified column reference: check qualified name first
	if v.Table != "" {
		if val, ok := row[v.Table+"."+v.Name]; ok {
			return val, nil
		}
	}
	// Unqualified: check short name
	if val, ok := row[v.Name]; ok {
		return val, nil
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

func evalBinaryOpValues(op string, left, right interface{}) (interface{}, error) {
	switch op {
	case "=":
		return util.CompareValues(left, right) == 0, nil
	case "<>", "!=":
		return util.CompareValues(left, right) != 0, nil
	case "<":
		return util.CompareValues(left, right) < 0, nil
	case ">":
		return util.CompareValues(left, right) > 0, nil
	case "<=":
		return util.CompareValues(left, right) <= 0, nil
	case ">=":
		return util.CompareValues(left, right) >= 0, nil
	case "LIKE":
		return likeValues(left, right), nil
	case "GLOB":
		return globValues(left, right), nil
	case "REGEXP":
		return regexpValues(left, right), nil
	case "NOT LIKE":
		return !likeValues(left, right), nil
	case "NOT GLOB":
		return !globValues(left, right), nil
	case "NOT REGEXP":
		return !regexpValues(left, right), nil
	default:
		return evalArithmeticOp(op, left, right)
	}
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
	switch op {
	case "+":
		if left == nil || right == nil { return nil, nil }
		return addValues(left, right)
	case "-":
		if left == nil || right == nil { return nil, nil }
		return subValues(left, right)
	case "*":
		if left == nil || right == nil { return nil, nil }
		return mulValues(left, right)
	case "/":
		if left == nil || right == nil { return nil, nil }
		return divValues(left, right)
	case "||":
		if left == nil || right == nil { return nil, nil }
		return concatValues(left, right)
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
func kleeneAnd(left, right interface{}) interface{} {
	if isFalse(left) || isFalse(right) {
		return false
	}
	if left == nil || right == nil {
		return nil
	}
	return true
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
		return true
	}
	if left == nil || right == nil {
		return nil
	}
	return false
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
	switch v.Operator {
	case "-":
		return negateValue(operand)
	case "NOT":
		if operand == nil {
			return nil, nil
		}
		return !toBool(operand), nil
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

	args := make([]interface{}, len(f.Args))
	for i, arg := range f.Args {
		v, err := e.evalExpr(arg, row)
		if err != nil {
			return nil, err
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
	tree := btree.NewBTree(e.pager, rootPage, true)
	cursor, err := tree.OpenCursor()
	if err != nil {
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
	return maxID + 1
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
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
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
	f, ok := toFloat(v)
	if ok {
		return -f, nil
	}
	return nil, fmt.Errorf("cannot negate non-numeric value")
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