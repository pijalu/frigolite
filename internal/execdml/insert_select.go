package execdml

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"regexp"
	"strings"
)

func (e *DMLExecutor) insertSelectWrittenRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64, s *sql.InsertStmt) (*Result, []interface{}) {
	record, err := storage.EncodeRecord(values)
	if err != nil {
		return &Result{Error: err}, nil
	}
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   rowID,
		Payload: record,
	}
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}, nil
	}
	// Track root page changes (after splits).
	dmlPg := e.dmlPager(tableEntry.Name)
	if tree.RootPage() != e.ctx.RootPagePg(dmlPg, tableEntry.Name, tableEntry.RootPage) {
		e.ctx.UpdateRootPagePg(dmlPg, tableEntry.Name, tree.RootPage())
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage, rowID)

	// Fire the preupdate hook with the new row's values (INSERT ... SELECT).
	puRowID := rowID
	if hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)) {
		puRowID = 0
	}
	if res := e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "INSERT",
		DB:    e.schemaNameForPager(e.dmlPager(tableEntry.Name)),
		Table: tableEntry.Name,
		RowID: puRowID, RowID2: puRowID,
		RowidTable: !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)),
		Old:        nil,
		New:        append([]interface{}(nil), values...),
	}); res != nil {
		return res, nil
	}

	// Maintain indexes (pure context; non-deterministic date functions in
	// index expressions raise SQLite's non-determinism error).
	if err := e.maintainIndexesOnInsert(tableEntry, colDefs, values, rowID); err != nil {
		return &Result{Error: err}, nil
	}

	// Fire AFTER INSERT triggers.
	if e.hasTriggersForTable(tableEntry.Name) {
		if res := e.fireAfterInsertTriggersForRow(tableEntry, colDefs, values, rowID); res != nil {
			return res, nil
		}
	}

	// Handle RETURNING clause — evaluate against the row that was written.
	if s.HasReturning {
		rrow := buildRowMapFromValues(values, colDefs, rowID)
		rv, err := e.evalReturningStrict(s.Returning, rrow, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}, nil
		}
		return nil, rv
	}
	return nil, nil
}

// errRowSkipped signals that an INSERT row was skipped (INSERT OR IGNORE or a
// column-level ON CONFLICT IGNORE) without being an error.

// errRowSkipped signals that an INSERT row was skipped (INSERT OR IGNORE or a
// column-level ON CONFLICT IGNORE) without being an error.
var errRowSkipped = fmt.Errorf("row skipped")

// buildInsertSelectValues maps one SELECT result row into the target table's
// column values, applying the INSERT column list (with defaults) or the
// positional/partial mapping. Returns the values, any explicit _rowid_ value,
// and whether an explicit rowid was supplied.

// assignIPKRowID sets a nil INTEGER PRIMARY KEY column to the assigned
// rowid, returning whether it was nil and its index (a BEFORE INSERT trigger
// sees new.<ipk> as -1).
func assignIPKRowID(colDefs []sql.ColumnDef, values []interface{}, rowID int64) (bool, int) {
	for i, cd := range colDefs {
		if cd.PrimaryKey && i < len(values) && values[i] == nil {
			values[i] = rowID
			return true, i
		}
	}
	return false, -1
}

// resolveInsertRowID determines the row ID for an INSERT ... SELECT row,
// tracking whether the INTEGER PRIMARY KEY column was NULL (auto-assigned).

// resolveInsertRowID determines the row ID for an INSERT ... SELECT row,
// tracking whether the INTEGER PRIMARY KEY column was NULL (auto-assigned).
func (e *DMLExecutor) resolveInsertRowID(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, explicitRowID int64, hasExplicitRowID, isReplace bool, replaceRowID int64) (int64, bool, int) {
	var rowID int64
	ipkWasNil := false
	ipkIndex := -1
	if hasExplicitRowID {
		rowID = explicitRowID
	} else if isReplace {
		rowID = replaceRowID
		ipkWasNil, ipkIndex = assignIPKRowID(colDefs, values, rowID)
	} else {
		var err error
		rowID, err = e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
		if err == nil {
			ipkWasNil, ipkIndex = assignIPKRowID(colDefs, values, rowID)
		}
	}
	return rowID, ipkWasNil, ipkIndex
}

// execInsertSelectConflict validates constraints for an INSERT ... SELECT row
// and handles the ON CONFLICT resolution (IGNORE skips the row via
// errRowSkipped; REPLACE deletes the conflicting row).

// fireBeforeInsertTriggersForRow fires BEFORE INSERT triggers for an inserted
// row. Returns a non-nil Result when a trigger failed, and true when the row
// was skipped (RAISE IGNORE).
func (e *DMLExecutor) fireBeforeInsertTriggersForRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, ipkWasNil bool, ipkIndex int) (*Result, bool) {
	newRow := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			// An auto-assigned INTEGER PRIMARY KEY is not yet known to a
			// BEFORE INSERT trigger: SQLite exposes new.<ipk> as -1.
			if ipkWasNil && i == ipkIndex {
				newRow[colDefs[i].Name] = int64(-1)
			} else {
				newRow[colDefs[i].Name] = v
			}
		}
	}
	// SQLite exposes new.rowid as -1 inside a BEFORE INSERT trigger.
	if !execquery.RowHasRowIDColumn(colDefs) {
		newRow["rowid"] = int64(-1)
		newRow["_rowid_"] = int64(-1)
		newRow["oid"] = int64(-1)
	}
	trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow)
	if trigResult.Error != nil {
		if trigResult.Error == errRaiseIgnore {
			return nil, true
		}
		return trigResult, false
	}
	return nil, false
}

// fireAfterInsertTriggersForRow fires AFTER INSERT triggers for an inserted
// row, returning a non-nil Result when a trigger failed.

// fireAfterInsertTriggersForRow fires AFTER INSERT triggers for an inserted
// row, returning a non-nil Result when a trigger failed.
func (e *DMLExecutor) fireAfterInsertTriggersForRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, rowID int64) *Result {
	newRow := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			newRow[colDefs[i].Name] = v
		}
	}
	if !execquery.RowHasRowIDColumn(colDefs) {
		newRow["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		newRow["_rowid_"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
		newRow["oid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	}
	trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow)
	if trigResult.Error != nil {
		return trigResult
	}
	return nil
}

// computeGeneratedValues fills in values for generated columns (b AS(expr))
// that are still nil, and returns the (possibly extended) values slice.
// Generated expressions may reference other columns of the same row —
// including other generated columns — so evaluation iterates to a fixpoint:
// a VIRTUAL column defined before the STORED column it references (e.g.
// "a INT AS (b*2) VIRTUAL, b INT AS (c*2) STORED") computes on a later pass
// once b is filled. The slice may need to grow when an INSERT...SELECT maps
// fewer columns than the table has (the trailing generated columns are nil).

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
func (e *DMLExecutor) fireAfterInsertTriggers(tableName string, newRow RowMap) *Result {
	return e.fireTriggers(tableName, "INSERT", "AFTER", newRow, nil)
}

// fireBeforeInsertTriggers fires BEFORE INSERT triggers for the given table.

// fireBeforeInsertTriggers fires BEFORE INSERT triggers for the given table.
func (e *DMLExecutor) fireBeforeInsertTriggers(tableName string, newRow RowMap) *Result {
	return e.fireTriggers(tableName, "INSERT", "BEFORE", newRow, nil)
}

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.

// fireAfterUpdateTriggers fires AFTER UPDATE triggers for the given table.
func (e *DMLExecutor) fireAfterUpdateTriggers(tableName string, newRow, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "UPDATE", "AFTER", newRow, oldRow)
}

// fireBeforeUpdateTriggers fires BEFORE UPDATE triggers for the given table.

// fireBeforeUpdateTriggers fires BEFORE UPDATE triggers for the given table.
func (e *DMLExecutor) fireBeforeUpdateTriggers(tableName string, newRow, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "UPDATE", "BEFORE", newRow, oldRow)
}

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.

// fireAfterDeleteTriggers fires AFTER DELETE triggers for the given table.
func (e *DMLExecutor) fireAfterDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "DELETE", "AFTER", nil, oldRow)
}

// fireBeforeDeleteTriggers fires BEFORE DELETE triggers for the given table.

// fireBeforeDeleteTriggers fires BEFORE DELETE triggers for the given table.
func (e *DMLExecutor) fireBeforeDeleteTriggers(tableName string, oldRow RowMap) *Result {
	return e.fireTriggers(tableName, "DELETE", "BEFORE", nil, oldRow)
}

// fireTriggers fires triggers matching the given event and timing for the table.

// maxTriggerDepth is SQLite's SQLITE_MAX_TRIGGER_DEPTH default: recursive
// trigger programs abort with "too many levels of trigger recursion" once
// the nesting exceeds this limit.
const maxTriggerDepth = 1000

// validateLoadedTriggers checks every trigger loaded from sqlite_master for
// schema references that no longer resolve. SQLite validates triggers at
// schema load and reports "malformed database schema". Validated triggers
// are cached by name to avoid re-parsing on every statement.

// checkLoadedTableRefCtx verifies a table reference in a loaded trigger
// resolves in the trigger's schema context.
func (e *DMLExecutor) checkLoadedTableRefCtx(table string, t *schema.Entry, trigCtx *DatabaseContext) error {
	schemaName, objName := parseSchemaName(table)
	ctx := trigCtx
	if schemaName != "" {
		upper := strings.ToUpper(schemaName)
		if upper == "TEMP" || upper == "TEMPORARY" {
			schemaName = "temp"
		}
		ctx = e.ctx.GetDB(schemaName)
		if ctx == nil {
			return fmt.Errorf("malformed database schema (%s) - trigger %s cannot reference objects in database %s", t.Name, t.Name, schemaName)
		}
	} else {
		// Unqualified reference: resolve in the trigger's own schema.
		if ctx == nil {
			ctx = e.ctx.MainDB()
		}
		objName = table
	}
	if _, err := ctx.Schema.FindTable(objName); err != nil {
		if _, err2 := ctx.Schema.FindView(objName); err2 != nil {
			// Only a SCHEMA-QUALIFIED reference that no longer resolves is a
			// load-time error (a trigger crossing to a foreign schema after a
			// reopen). An unqualified reference to a dropped same-schema table
			// is left to fail at fire time (SQLite does not re-validate those).
			if schemaName != "" {
				return fmt.Errorf("malformed database schema (%s) - trigger %s cannot reference objects in database %s", t.Name, t.Name, ctx.Name)
			}
		}
	}
	return nil
}

// fireTrigger fires a single trigger matching the given event and timing.
// Returns a Result with an error if execution fails, or nil on success
// (including when the trigger does not match or its WHEN clause is false).

// parseTriggerWhen extracts and parses the WHEN expression of a trigger's
// CREATE TRIGGER SQL text. Returns nil when the trigger has no WHEN clause.
func (e *DMLExecutor) parseTriggerWhen(triggerSQL string) sql.Expr {
	upper := strings.ToUpper(triggerSQL)
	whenIdx := strings.Index(upper, " WHEN ")
	if whenIdx < 0 {
		return nil
	}
	// Find the BEGIN keyword after the WHEN expression, allowing any
	// whitespace (space, newline, tab) before it.
	beginIdx := -1
	for i := whenIdx + len(" WHEN "); i < len(upper); i++ {
		if upper[i] == 'B' && i+5 <= len(upper) && upper[i:i+5] == "BEGIN" {
			beginIdx = i
			break
		}
	}
	if beginIdx < 0 {
		return nil
	}
	exprText := triggerSQL[whenIdx+len(" WHEN ") : beginIdx]
	exprText = strings.TrimSpace(exprText)
	if exprText == "" {
		return nil
	}
	stmts, perr := parse.ParseSQL("SELECT " + exprText)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil
	}
	return sel.Columns[0].Expr
}

// parseTriggerUpdateOf extracts the column list of an "UPDATE OF <cols>"
// trigger declaration. Returns nil when the trigger is not an UPDATE OF form
// (no OF clause). Column names are returned lower-cased (SQLite column
// matching is case-insensitive).

// parseTriggerUpdateOf extracts the column list of an "UPDATE OF <cols>"
// trigger declaration. Returns nil when the trigger is not an UPDATE OF form
// (no OF clause). Column names are returned lower-cased (SQLite column
// matching is case-insensitive).
func parseTriggerUpdateOf(triggerSQL string) []string {
	upper := strings.ToUpper(triggerSQL)
	header := upper
	if beginIdx := strings.Index(upper, "BEGIN"); beginIdx >= 0 {
		header = upper[:beginIdx]
	}
	ofIdx := strings.Index(header, " OF ")
	if ofIdx < 0 {
		return nil
	}
	// The column list runs from after " OF " to the " ON " keyword.
	rest := header[ofIdx+len(" OF "):]
	onIdx := strings.Index(rest, " ON ")
	if onIdx < 0 {
		return nil
	}
	list := rest[:onIdx]
	var cols []string
	for _, part := range strings.Split(list, ",") {
		name := strings.TrimSpace(part)
		name = strings.Trim(name, `"'[]`)
		if name != "" {
			cols = append(cols, strings.ToLower(name))
		}
	}
	return cols
}

// triggerSetsColumn reports whether the current UPDATE statement assigns any
// of the given columns (the UPDATE OF <cols> selectivity check).

// triggerSetsColumn reports whether the current UPDATE statement assigns any
// of the given columns (the UPDATE OF <cols> selectivity check).
func (e *DMLExecutor) triggerSetsColumn(ofCols []string) bool {
	if len(e.updateSetColumns) == 0 {
		// No SET columns recorded (e.g. an UPDATE produced by a view or
		// internal path): fall back to firing (SQLite conservatively fires
		// when the set is unknown).
		return true
	}
	for _, set := range e.updateSetColumns {
		for _, of := range ofCols {
			if strings.EqualFold(set, of) {
				return true
			}
		}
	}
	return false
}

// parseTriggerHeader extracts the declared timing ("BEFORE", "AFTER",
// "INSTEAD OF") and event ("INSERT", "UPDATE", "DELETE") from a trigger's
// CREATE TRIGGER SQL text. It is whitespace-robust: the declaration may have
// any number of spaces/newlines between the timing, event and ON keywords.
// Returns ("", "") when the header cannot be parsed.

// parseTriggerHeader extracts the declared timing ("BEFORE", "AFTER",
// "INSTEAD OF") and event ("INSERT", "UPDATE", "DELETE") from a trigger's
// CREATE TRIGGER SQL text. It is whitespace-robust: the declaration may have
// any number of spaces/newlines between the timing, event and ON keywords.
// Returns ("", "") when the header cannot be parsed.
func parseTriggerHeader(triggerSQL string) (timing, event string) {
	upper := strings.ToUpper(triggerSQL)
	// Only look at the declaration header, before the body's BEGIN keyword.
	header := upper
	if beginIdx := strings.Index(upper, "BEGIN"); beginIdx >= 0 {
		header = upper[:beginIdx]
	}
	if strings.Contains(header, "INSTEAD OF") {
		timing = "INSTEAD"
	} else if strings.Contains(header, "AFTER") {
		timing = "AFTER"
	} else if strings.Contains(header, "BEFORE") {
		timing = "BEFORE"
	}
	// The event is the first standalone INSERT/UPDATE/DELETE word in the
	// header (the table name appears after "ON", so the first event word is
	// always the declared event).
	for _, ev := range []string{"INSERT", "UPDATE", "DELETE"} {
		if regexp.MustCompile(`\b` + ev + `\b`).MatchString(header) {
			event = ev
			break
		}
	}
	return timing, event
}

// checkConstraintText extracts the original CHECK constraint expression text
// from a CREATE TABLE SQL for the given column. Falls back to the re-rendered
// expression when the raw text cannot be located.

// hasTableLevelCheck reports whether a column-definition fragment is a
// table-level (not column-level) CHECK: the fragment's first keyword is
// CHECK or CONSTRAINT (a column-level CHECK is preceded by the column name
// and type).
func hasTableLevelCheck(part string) bool {
	trimmed := strings.TrimSpace(part)
	pUpper := strings.ToUpper(trimmed)
	if strings.HasPrefix(pUpper, "CHECK") {
		return true
	}
	if strings.HasPrefix(pUpper, "CONSTRAINT") {
		return true
	}
	return false
}

// checkExprText extracts the text inside the first CHECK(...) of a column
// definition fragment, handling nested parentheses.

// checkExprText extracts the text inside the first CHECK(...) of a column
// definition fragment, handling nested parentheses.
func checkExprText(part string) string {
	pUpper := strings.ToUpper(part)
	ci := strings.Index(pUpper, "CHECK")
	if ci < 0 {
		return ""
	}
	lp := strings.Index(part[ci:], "(")
	if lp < 0 {
		return ""
	}
	lp += ci
	depth := 0
	for i := lp; i < len(part); i++ {
		switch part[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(part[lp+1 : i])
			}
		}
	}
	return ""
}

// evalReturningStrict evaluates RETURNING expressions with strict column
// resolution: unknown columns and invalid qualifiers produce "no such column"
// errors (SQLite semantics), and table-qualified wildcards are rejected.
func (e *DMLExecutor) evalReturningStrict(ret sql.SelectColumn, row Row, colDefs []sql.ColumnDef, tableName string) ([]interface{}, error) {
	prevStrict, prevTable := e.ctx.ReturningStrict(), e.ctx.ReturningTable()
	e.ctx.SetReturningStrict(true)
	e.ctx.SetReturningTable(tableName)
	defer func() { e.ctx.SetReturningStrict(prevStrict); e.ctx.SetReturningTable(prevTable) }()
	return e.evalReturningExprs(ret, row, colDefs)
}

// evalReturningExprs evaluates RETURNING expressions against a row and
// returns a flat list of values. It handles three cases:
//   - RETURNING * : expands to all column values
//   - RETURNING expr (single) : returns the single expression value
//   - RETURNING expr, ..., * , ... : multi-expression with * expanded inline

// viewDeclaredColumns returns the explicit column list from a CREATE VIEW
// declaration (CREATE VIEW v(a,b) AS ...). Returns nil when the view has no
// declared column list.
func (e *DMLExecutor) viewDeclaredColumns(viewEntry *schema.Entry) []string {
	stmts, perr := parse.ParseSQL(viewEntry.SQL)
	if perr != nil {
		return nil
	}
	for _, st := range stmts {
		if c, ok := st.(*sql.CreateViewStmt); ok {
			return c.Columns
		}
	}
	return nil
}

// execInsertView handles INSERT statements whose target is a view. SQLite
// routes such statements through INSTEAD OF triggers; resolving the view's
// columns (which validates collations in its SELECT) happens first.

// viewColumnNames returns the output column names of a view's SELECT: the
// explicit alias when present, otherwise the column reference name or the
// expression text. A bare "*" is expanded through the FROM source (SQLite
// resolves view output columns the same way as the result columns of a plain
// SELECT). For a compound SELECT the head member determines the output names.
func (e *DMLExecutor) viewColumnNames(sel *sql.SelectStmt) []string {
	if sel == nil {
		return nil
	}
	var names []string
	for _, col := range sel.Columns {
		if col.As != "" {
			names = append(names, col.As)
			continue
		}
		if ref, ok := col.Expr.(*sql.ColumnRef); ok {
			if ref.Name == "*" && ref.Table == "" {
				// Expand through the FROM source (single table or joined
				// result). When expansion fails, fall through to the
				// unexpanded name so the caller can still report the view's
				// column list.
				if cols, err := e.ctx.TableColumnNames(sel.From.Name); err == nil {
					names = append(names, cols...)
					continue
				}
			}
			names = append(names, ref.Name)
			continue
		}
		names = append(names, e.exprName(col.Expr))
	}
	return names
}

// exprName returns a human-readable name for an expression (fallback for view
// columns without an alias).

// exprName returns a human-readable name for an expression (fallback for view
// columns without an alias).
func (e *DMLExecutor) exprName(expr sql.Expr) string {
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return v.Name
	case *sql.BinaryOp:
		return e.exprName(v.Left) + v.Operator + e.exprName(v.Right)
	case *sql.NumericLit:
		return v.Value
	case *sql.StringLit:
		return v.Value
	case *sql.FuncCall:
		return v.Name
	}
	return "col"
}

// validateCollationsInSelect walks every expression in a SELECT statement and
// verifies that each COLLATE operator names a known collation sequence.

// validateCollationsInSelect walks every expression in a SELECT statement and
// verifies that each COLLATE operator names a known collation sequence.
func (e *DMLExecutor) validateCollationsInSelect(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	for _, col := range s.Columns {
		if err := e.validateCollationsInExpr(col.Expr); err != nil {
			return err
		}
	}
	if err := e.validateCollationsInExpr(s.Where); err != nil {
		return err
	}
	if err := e.validateCollationsInExpr(s.Having); err != nil {
		return err
	}
	for _, g := range s.GroupBy {
		if err := e.validateCollationsInExpr(g); err != nil {
			return err
		}
	}
	for _, o := range s.OrderBy {
		if err := e.validateCollationsInExpr(o.Expr); err != nil {
			return err
		}
	}
	if s.Union != nil {
		return e.validateCollationsInSelect(s.Union)
	}
	return nil
}

// validateCollationsInExpr verifies COLLATE operators in an expression tree.

// checkCollationName verifies that a COLLATE operand names a known collation.
func (e *DMLExecutor) checkCollationName(expr sql.Expr) error {
	var name string
	switch v := expr.(type) {
	case *sql.StringLit:
		name = v.Value
	case *sql.ColumnRef:
		name = v.Name
	default:
		return nil
	}
	return e.checkCollationString(name)
}

// checkCollationString verifies that a collation name is a known sequence
// (a built-in or a registered custom collation).

// checkCollationString verifies that a collation name is a known sequence
// (a built-in or a registered custom collation).
func (e *DMLExecutor) checkCollationString(name string) error {
	switch strings.ToUpper(name) {
	case "", "BINARY", "NOCASE", "RTRIM":
		return nil
	default:
		if e.ctx.LookupCollation(name) != nil {
			return nil
		}
		return fmt.Errorf("no such collation sequence: %s", name)
	}
}

// colDefAt returns the column definition with the given name (case-insensitive),
// or nil.
func colDefAt(colDefs []sql.ColumnDef, name string) *sql.ColumnDef {
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, name) {
			return &colDefs[i]
		}
	}
	return nil
}

// scanTableForMatch iterates a table's records, invoking match for each;
// returns the first matching cell and record, or nil when none matched.
func (e *DMLExecutor) scanTableForMatch(tableEntry *schema.Entry, match func(rec *storage.Record, cell *storage.Cell) bool) (*storage.Cell, *storage.Record, error) {
	tree := e.uniqueScanTree(tableEntry.Name, tableEntry.RootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil, err
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		if match(rec, cell) {
			return cell, rec, nil
		}
		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return nil, nil, nil
}

// wrapValueForRowMap wraps a column value with its declared affinity and
// collation so comparisons and affinity-reporting functions see them, matching
// how scanned table rows are wrapped.
func wrapValueForRowMap(v interface{}, cd sql.ColumnDef) interface{} {
	// Wrap the value with its column's affinity so comparisons and
	// affinity-reporting functions (affinity()) see the declared affinity,
	// matching how scanned table rows are wrapped. Only wrap when the column
	// declares an explicit type (an empty type is the generic no-affinity
	// case, where the raw value is used).
	if cd.Type != "" {
		cv := &util.ColumnValue{Value: v, Affinity: util.Affinity(cd.Type)}
		if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
			return &collatedValue{Value: cv, Collation: strings.ToUpper(coll)}
		}
		return cv
	}
	// Wrap values whose column declares a collation (e.g. NOCASE) so
	// comparisons against them use that collation (SQLite column
	// collation rules). Only non-BINARY collations are wrapped.
	if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		return &collatedValue{Value: v, Collation: strings.ToUpper(coll)}
	}
	return v
}

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
