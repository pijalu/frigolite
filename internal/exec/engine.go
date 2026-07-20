// Package exec implements query execution.
package exec

import (
	"fmt"
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
	Columns []string       // column names
	Rows    [][]interface{} // data rows
	Changes int64          // number of changed rows
	Error   error          // execution error
}

// Engine executes SQL statements.
type Engine struct {
	pager    *pager.Pager
	schema   *schema.Manager
	funcs    *function.Registry
	vtabs    *vtab.Registry
}

// NewEngine creates a new execution engine.
func NewEngine(pg *pager.Pager) *Engine {
	e := &Engine{
		pager:  pg,
		schema: schema.NewManager(pg),
		funcs:  function.NewRegistry(),
		vtabs:  vtab.NewRegistry(),
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
	case *sql.CommitStmt:
		return e.execCommit()
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

	// TODO: populate index from existing table data

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
	colDefs := e.parseColumnDefs(tableEntry.SQL)

	if s.Select != nil {
		return e.execInsertSelect(tableEntry, colDefs, s.Select)
	}
	if len(s.Values) == 0 {
		return e.execInsertDefault(tableEntry)
	}

	values := e.evalInsertValues(s, colDefs)
	record, err := storage.EncodeRecord(values)
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
	// Fire AFTER INSERT triggers
	if trigResult := e.fireAfterInsertTriggers(tableEntry.Name); trigResult.Error != nil {
		return trigResult
	}
	return &Result{Changes: 1}
}

func (e *Engine) execInsertSelect(tableEntry *schema.Entry, colDefs []sql.ColumnDef, selectStmt *sql.SelectStmt) *Result {
	selectResult := e.execSelect(selectStmt)
	if selectResult.Error != nil {
		return selectResult
	}
	var changes int64
	for _, row := range selectResult.Rows {
		record, err := storage.EncodeRecord(row)
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
		changes++
	}
	return &Result{Changes: changes}
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
		triggerSQL := t.SQL
		upper := strings.ToUpper(triggerSQL)
		// Check event matches: "event ON table" pattern
		if !strings.Contains(upper, " "+event+" ") && !strings.Contains(upper, " "+event+" ON") {
			continue
		}
		// Extract statements between BEGIN and END
		beginIdx := strings.Index(upper, "BEGIN")
		if beginIdx < 0 {
			continue
		}
		endIdx := strings.LastIndex(upper, "END")
		if endIdx < 0 {
			continue
		}
		body := triggerSQL[beginIdx+5 : endIdx]
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		parser := sql.NewParser(body)
		stmts := parser.Parse()
		if parser.Err() != nil {
			continue
		}
		for _, stmt := range stmts {
			res := e.Exec(stmt)
			if res.Error != nil {
				return res
			}
		}
	}
	return &Result{}
}

func (e *Engine) evalInsertValues(s *sql.InsertStmt, colDefs []sql.ColumnDef) []interface{} {
	values := make([]interface{}, len(s.Values))
	for i, expr := range s.Values {
		v, err := e.evalExpr(expr, nil)
		if err != nil {
			return nil
		}
		values[i] = v
	}
	if len(s.Columns) > 0 {
		mapped := make([]interface{}, len(colDefs))
		for i, col := range s.Columns {
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
	if s.From.Name == "" && len(s.From.As) == 0 {
		return e.execSelectNoFrom(s)
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
	colDefs := e.parseColumnDefs(tableEntry.SQL)

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

	// Handle aggregate functions: evaluate them across all rows
	if e.hasAggregates(s.Columns) {
		result := e.evalAggregates(s, allRowMaps)
		if result != nil {
			return result
		}
	}

	result := &Result{Columns: e.buildColumnNames(s.Columns, colDefs), Rows: allRows}

	// Apply DISTINCT
	if s.Distinct {
		result.Rows = e.distinctRows(result.Rows)
	}

	// Apply ORDER BY
	if len(s.OrderBy) > 0 {
		e.sortRowsWithMaps(result, s.OrderBy, allRowMaps)
	}

	// Apply LIMIT / OFFSET
	result.Rows = applyLimitOffset(result.Rows, s.Limit, s.Offset)

	// Handle UNION / INTERSECT / EXCEPT
	if s.Union != nil {
		result.Rows = e.mergeUnionRows(result.Rows, s.Union)
	}

	return result
}

func (e *Engine) mergeUnionRows(rows [][]interface{}, union *sql.SelectStmt) [][]interface{} {
	unionResult := e.execSelect(union)
	if unionResult.Error != nil {
		return rows
	}
	allRows := append(rows, unionResult.Rows...)
	seen := make(map[string]bool)
	var merged [][]interface{}
	for _, row := range allRows {
		key := fmt.Sprintf("%v", row)
		if !seen[key] {
			seen[key] = true
			merged = append(merged, row)
		}
	}
	return merged
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

	// Handle UNION for no-FROM selects
	if s.Union != nil {
		unionResult := e.execSelect(s.Union)
		if unionResult.Error != nil {
			return unionResult
		}
		allRows := append([][]interface{}{outRow}, unionResult.Rows...)
		seen := make(map[string]bool)
		var unionRows [][]interface{}
		for _, row := range allRows {
			key := fmt.Sprintf("%v", row)
			if !seen[key] {
				seen[key] = true
				unionRows = append(unionRows, row)
			}
		}
		return &Result{Columns: columns, Rows: unionRows}
	}

	return &Result{Columns: columns, Rows: [][]interface{}{outRow}}
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

func (e *Engine) evalAggregateExpr(expr sql.Expr, rowMaps []map[string]interface{}) interface{} {
	switch v := expr.(type) {
	case *sql.FuncCall:
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

func applyLimitOffset(rows [][]interface{}, limit, offset sql.Expr) [][]interface{} {
	if limit == nil {
		return rows
	}
	l, _ := sql.EvalNumber(limit)
	o := int64(0)
	if offset != nil {
		o, _ = sql.EvalNumber(offset)
	}
	if o > int64(len(rows)) {
		return nil
	}
	end := o + l
	if end > int64(len(rows)) {
		end = int64(len(rows))
	}
	if o < 0 {
		o = 0
	}
	return rows[o:end]
}

// distinctRows removes duplicate rows from a result set.
func (e *Engine) distinctRows(rows [][]interface{}) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}
	seen := make(map[string]bool)
	var result [][]interface{}
	for _, row := range rows {
		key := fmt.Sprintf("%v", row)
		if !seen[key] {
			seen[key] = true
			result = append(result, row)
		}
	}
	return result
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
	n := len(result.Rows)
	if n <= 1 {
		return
	}
	for i := 0; i < n-1; i++ {
		for j := 0; j < n-i-1; j++ {
			if e.compareRows(orderBy, rowMaps, j, j+1) {
				result.Rows[j], result.Rows[j+1] = result.Rows[j+1], result.Rows[j]
				rowMaps[j], rowMaps[j+1] = rowMaps[j+1], rowMaps[j]
			}
		}
	}
}

func (e *Engine) compareRows(orderBy []sql.OrderByTerm, rowMaps []map[string]interface{}, a, b int) bool {
	for _, ob := range orderBy {
		left, _ := e.evalExpr(ob.Expr, rowMaps[a])
		right, _ := e.evalExpr(ob.Expr, rowMaps[b])
		cmp := util.CompareValues(left, right)
		if ob.Desc {
			cmp = -cmp
		}
		if cmp > 0 {
			return true // swap needed
		} else if cmp < 0 {
			return false // no swap
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
	colDefs := e.parseColumnDefs(tableEntry.SQL)

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
	values := make([]interface{}, len(rec.Values))
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
		if idx >= 0 {
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
	var changeCount int64
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
		tree := btree.NewBTree(e.pager, rootPage, true)

		// Delete existing cell with same RowID to avoid duplicates
		_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			return cell.RowID == c.rowID
		})
		if delErr != nil {
			return &Result{Error: delErr}
		}

		if err := tree.InsertCell(newCell); err != nil {
			return &Result{Error: err}
		}
		changeCount++
	}
	return &Result{Changes: changeCount}
}

// --- DELETE ---

func (e *Engine) execDelete(s *sql.DeleteStmt) *Result {
	tableEntry, err := e.schema.FindTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	colDefs := e.parseColumnDefs(tableEntry.SQL)

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
	case *sql.FuncCall:
		return e.evalFuncCall(v, row)
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
	if val, ok := row[v.Name]; ok {
		return val, nil
	}
	if v.Table != "" {
		if val, ok := row[v.Table+"."+v.Name]; ok {
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
	default:
		return evalArithmeticOp(op, left, right)
	}
}

func evalArithmeticOp(op string, left, right interface{}) (interface{}, error) {
	switch op {
	case "+":
		return addValues(left, right)
	case "-":
		return subValues(left, right)
	case "*":
		return mulValues(left, right)
	case "/":
		return divValues(left, right)
	case "||":
		return concatValues(left, right)
	case "AND":
		return toBool(left) && toBool(right), nil
	case "OR":
		return toBool(left) || toBool(right), nil
	default:
		return nil, fmt.Errorf("unknown operator: %s", op)
	}
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

func (e *Engine) parseColumnDefs(createSQL string) []sql.ColumnDef {
	// Parse column definitions from CREATE TABLE SQL
	parser := sql.NewParser(createSQL)
	stmts := parser.Parse()
	if len(stmts) > 0 {
		if ct, ok := stmts[0].(*sql.CreateTableStmt); ok {
			return ct.Columns
		}
	}
	return nil
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
		return af - bf, nil
	}
	return nil, fmt.Errorf("cannot subtract non-numeric values")
}

func mulValues(a, b interface{}) (interface{}, error) {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
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
		return af / bf, nil
	}
	return nil, fmt.Errorf("cannot divide non-numeric values")
}

func concatValues(a, b interface{}) (interface{}, error) {
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

func likeMatch(s, pattern string) bool {
	return likeMatchRecursive(s, pattern, 0, 0)
}

func likeMatchRecursive(s, pattern string, idx, patIdx int) bool {
	for patIdx < len(pattern) {
		c := pattern[patIdx]
		switch c {
		case '%':
			return likeMatchPercent(s, pattern, idx, patIdx)
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

func likeMatchPercent(s, pattern string, idx, patIdx int) bool {
	patIdx++
	if patIdx >= len(pattern) {
		return true
	}
	for idx < len(s) {
		if likeMatchRecursive(s, pattern, idx, patIdx) {
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
