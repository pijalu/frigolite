// Package exec implements query execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE execution ---
// --- UPDATE execution ---

func (e *DMLExecutor) execUpdate(s *sql.UpdateStmt) *Result {
	// The UPDATE's WITH clause (CTEs) applies to its FROM tables and SET
	// expressions, including the view/INSTEAD-OF path. Push the CTEs onto
	// the scope stack so UPDATE ... FROM input resolves input as a CTE
	// (upfrom2-3.1) even when the target is a view.
	if len(s.CTEs) > 0 {
		e.ctx.PushCTEScope(s.CTEs)
		defer e.ctx.PopCTEScope()
	}
	// Echo virtual tables write through to their source table (vtabA-3.1).
	e.redirectEchoVTab(s)
	// Generic updatable virtual tables (sqlite_dbpage etc.) route before the
	// b-tree paths: their rows come from xFilter, not a root page.
	if res := e.rejectUnsafeVTabUse(s.Table); res != nil {
		return res
	}
	if res, handled := e.execVTabUpdate(s); handled {
		return res
	}
	if err := e.ctx.Authorize(auth.ActionUpdate, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		// Not a table — route through INSTEAD OF UPDATE triggers on a view.
		return e.updateOnMissingTable(s, err)
	}

	// Track the modified table's database context for trigger scoping.
	prevDMLCtx := e.currentDMLCtx
	e.currentDMLCtx = dbCtx
	defer func() { e.currentDMLCtx = prevDMLCtx }()

	// Protect system and pragma virtual tables from modification.
	if e.ctx.IsNonModifiableTable(tableEntry) {
		return &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}

	// Direct modification of sqlite_sequence changes AUTOINCREMENT sequences;
	// clear the in-memory cache so the next INSERT reads the real table fresh.
	if isSQLiteSequenceName(tableEntry.Name) {
		defer e.ctx.ResetAutoIncSeq()
	}

	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// Route FTS virtual table updates directly to the FTS table (SQLite's
	// fts3UpdateMethod handles docid and content column updates).
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.ctx.ExecFTSUpdate(tableEntry.Name, ftsTable, colDefs, s)
	}

	// Record which columns this UPDATE statement's SET clause assigns, so
	// UPDATE OF <cols> triggers fire only when a listed column is in the set.
	// Cleared on return (the engine is single-threaded per connection).
	defer e.pushUpdateSetColumns(s)()

	if res := e.prepareUpdate(s, tableEntry, colDefs); res != nil {
		return res
	}

	colIndex := buildColumnIndex(colDefs)

	// When the table has triggers, defer SET evaluation to the apply loop so
	// the changes() counter and user functions observe SQLite's row-by-row
	// interleaving (e_changes 5.1.2): row N's AFTER trigger runs before row
	// N+1's SET expressions are evaluated (update.c:1117-1120). Non-trigger
	// paths keep the bulk pre-computed values.
	deferSetEval := e.hasTriggersForTable(tableEntry.Name) &&
		!strings.EqualFold(s.OnConflict, "REPLACE") &&
		!strings.EqualFold(s.OnConflict, "IGNORE")
	changes, err := e.collectUpdateChanges(s.Table, tableEntry.RootPage, colIndex, colDefs, s, deferSetEval)
	if err != nil {
		return &Result{Error: err}
	}

	// Enforce NOT NULL and CHECK constraints on the new values (SQLite checks
	// these per-row during UPDATE; a violation aborts the whole statement).
	// UPDATE OR IGNORE skips violating rows instead of aborting, so the
	// per-row check happens inside applyUpdateIgnore (below).
	if res := e.preCheckUpdate(s, tableEntry, colDefs, changes); res.Error != nil {
		return res
	}

	// Handle RETURNING clause — evaluate against updated rows before applying
	var returningRows [][]interface{}
	if s.HasReturning {
		returningRows, err = e.evalUpdateReturning(s, changes, colDefs, tableEntry.Name)
		if err != nil {
			return &Result{Error: err}
		}
	}

	// Enforce FOREIGN KEY constraints on the new values (PRAGMA foreign_keys).
	if res := e.checkUpdateForeignKeys(s, tableEntry, colDefs, changes); res.Error != nil {
		return res
	}

	result := e.dispatchUpdate(s, tableEntry, colDefs, changes)
	if result.Error != nil {
		return result
	}

	// Fire AFTER UPDATE triggers with the new and old row values. The
	// applyUpdateWithTriggers and applyUpdateIgnore paths fire AFTER triggers
	// themselves; only the REPLACE path reaches this block.
	if res := e.fireUpdateAfterTriggers(s, tableEntry, colDefs, changes); res.Error != nil {
		return res
	}

	// Direct edits to sqlite_schema (PRAGMA writable_schema=ON) are schema
	// changes: re-read the schema btree on the next table lookup.
	e.invalidateSchemaIfNeeded(tableEntry.Name)

	// If RETURNING clause was present, return result rows instead of change count
	return e.finishUpdate(s, colDefs, returningRows, result)
}

// redirectEchoVTab rewrites an UPDATE on an echo virtual table to its source
// table (vtabA-3.1).
func (e *DMLExecutor) redirectEchoVTab(s *sql.UpdateStmt) {
	if srcName, ok := e.ctx.EchoVTabSource(s.Table); ok {
		s.Table = srcName
	}
}

// updateOnMissingTable routes an UPDATE on a name that is not a table through
// INSTEAD OF UPDATE triggers on a view, or returns the original lookup error.
func (e *DMLExecutor) updateOnMissingTable(s *sql.UpdateStmt, err error) *Result {
	viewEntry, _, viewErr := e.ctx.FindView(s.Table)
	if viewErr == nil {
		return e.execUpdateView(s, viewEntry)
	}
	return &Result{Error: err}
}

// pushUpdateSetColumns records the SET-clause column names on the engine so
// UPDATE OF <cols> triggers fire only when a listed column is in the set. It
// returns a closure that restores the previous value on return.
func (e *DMLExecutor) pushUpdateSetColumns(s *sql.UpdateStmt) func() {
	prev := e.updateSetColumns
	e.updateSetColumns = nil
	for _, a := range s.Assignments {
		e.updateSetColumns = append(e.updateSetColumns, a.Column)
	}
	e.updateSetColumns = append(e.updateSetColumns, s.SetParenColumns...)
	return func() { e.updateSetColumns = prev }
}

// prepareUpdate validates an UPDATE's RETURNING clause.
func (e *DMLExecutor) prepareUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if s.HasReturning {
		if err := e.validateReturning(s.Returning, colDefs, tableEntry.Name); err != nil {
			return &Result{Error: err}
		}
	}
	// SQLite rejects UPDATE ... FROM where the target object/alias reappears
	// in the FROM clause: "target object/alias may not appear in FROM
	// clause: X" (upfrom2-5.x).
	if err := e.validateUpdateFromTarget(s, tableEntry.Name); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// validateUpdateFromTarget rejects an UPDATE ... FROM whose FROM clause
// repeats the target table or its alias (upfrom2-5.x). The restriction is on
// the effective NAME: SQLite rejects the target's own name or alias
// reappearing in FROM, but ALLOWS a different alias of the same underlying
// table (fts4upfrom 1.x: UPDATE ft SET b=o.c FROM ft AS o is valid — the
// FROM alias o is distinct, so the self-join is a legitimate correlated
// update). When the matching FROM table aliases the target, the error names
// the alias (UPDATE x1 AS grapes ... FROM x1 AS grapes → "...clause:
// grapes"); otherwise it names the table.
func (e *DMLExecutor) validateUpdateFromTarget(s *sql.UpdateStmt, targetTable string) error {
	if s.From.Name == "" {
		return nil
	}
	// The target's effective name is its alias when present, else the table.
	effective := strings.ToLower(targetTable)
	if strings.TrimSpace(s.Alias) != "" {
		effective = strings.ToLower(strings.TrimSpace(s.Alias))
	}
	match := func(ref sql.TableRef) string {
		refName := strings.ToLower(ref.Name)
		if ref.As == "" && refName == effective {
			return ref.Name
		}
		if ref.As != "" && strings.ToLower(ref.As) == effective {
			return ref.As
		}
		return ""
	}
	if nm := match(s.From); nm != "" {
		return fmt.Errorf("target object/alias may not appear in FROM clause: %s", nm)
	}
	for _, jc := range s.FromJoins {
		if nm := match(jc.Table); nm != "" {
			return fmt.Errorf("target object/alias may not appear in FROM clause: %s", nm)
		}
	}
	return nil
}

// preCheckUpdate enforces NOT NULL and CHECK constraints before an UPDATE is
// applied, except under UPDATE OR IGNORE where per-row checks happen inside
// applyUpdateIgnore.
func (e *DMLExecutor) preCheckUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if strings.EqualFold(s.OnConflict, "IGNORE") {
		return &Result{}
	}
	// Only materialize deferred SET values when there are constraints to
	// check: the check needs the new values, but materializing here would
	// fire the changes() user-function ordering before the apply loop (the
	// per-row interleaving only matters for the trigger path, which defers).
	if e.updateHasConstraints(colDefs) {
		colIndex := buildColumnIndex(colDefs)
		for i := range changes {
			if err := e.materializeChangeValues(&changes[i], s, colIndex, colDefs); err != nil {
				return &Result{Error: err}
			}
		}
	}
	return e.checkUpdateConstraints(tableEntry, colDefs, changes)
}

// updateHasConstraints reports whether an UPDATE target's columns carry
// NOT NULL or CHECK constraints that preCheckUpdate must enforce.
func (e *DMLExecutor) updateHasConstraints(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		if cd.NotNull || cd.Check != nil {
			return true
		}
	}
	return false
}

// evalUpdateReturning evaluates the RETURNING clause against the updated rows
// (before the rows are written).
func (e *DMLExecutor) evalUpdateReturning(s *sql.UpdateStmt, changes []updateChange, colDefs []sql.ColumnDef, tableName string) ([][]interface{}, error) {
	var returningRows [][]interface{}
	colIndex := buildColumnIndex(colDefs)
	for i := range changes {
		if err := e.materializeChangeValues(&changes[i], s, colIndex, colDefs); err != nil {
			return nil, err
		}
		ch := &changes[i]
		row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		values, err := e.evalReturningStrict(s.Returning, row, colDefs, tableName)
		if err != nil {
			return nil, err
		}
		returningRows = append(returningRows, values)
	}
	return returningRows, nil
}

// checkUpdateForeignKeys enforces child-direction FOREIGN KEY constraints on
// an UPDATE's new values (PRAGMA foreign_keys).
func (e *DMLExecutor) checkUpdateForeignKeys(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if !e.ctx.ForeignKeys() {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	for i := range changes {
		ch := &changes[i]
		if err := e.materializeChangeValues(ch, s, colIndex, colDefs); err != nil {
			return &Result{Error: err}
		}
		// Pass ch.rowID so a self-referential FK does not count the row's
		// own OLD key value as a valid parent for the NEW child value.
		if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, ch.values, ch.rowID); res.Error != nil {
			return res
		}
	}
	return &Result{}
}

// dispatchUpdate applies the UPDATE using the conflict-resolution mode and
// trigger state (REPLACE, IGNORE, trigger-per-row, or the plain path).
func (e *DMLExecutor) dispatchUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if strings.EqualFold(s.OnConflict, "REPLACE") {
		return e.applyUpdateReplace(tableEntry, colDefs, changes)
	}
	if strings.EqualFold(s.OnConflict, "IGNORE") {
		return e.applyUpdateIgnore(tableEntry, colDefs, changes)
	}
	if e.hasTriggersForTable(tableEntry.Name) {
		return e.applyUpdateWithTriggers(tableEntry, colDefs, changes, s)
	}
	return e.runPlainUpdate(s, tableEntry, colDefs, changes)
}

// runPlainUpdate applies a plain UPDATE: check UNIQUE/PK conflicts, enforce
// FOREIGN KEY parent actions, then write the new rows. tableName is the name
// as written in the statement (may be schema-qualified, e.g. "aux.p1").
// UPDATE OR FAIL processes rows incrementally (SQLite's ON CONFLICT FAIL
// semantics: a conflict aborts the statement but rows already written before
// the conflict survive). Other modes (default/ABORT/ROLLBACK) are statement-
// atomic: all UNIQUE/PK conflicts are checked up-front and nothing is written
// before the first conflict (OR ROLLBACK additionally rolls back the whole
// transaction via execRollbackOnError).
func (e *DMLExecutor) runPlainUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if strings.EqualFold(s.OnConflict, "FAIL") {
		return e.runUpdateFail(s.Table, tableEntry, colDefs, changes)
	}
	// Plain UPDATE (default/ABORT/ROLLBACK): check UNIQUE/PK constraints on
	// the new values (SQLite errors on conflicts; there is no REPLACE
	// resolution). Nothing is written until every row passes, so a conflict
	// leaves the statement with no partial writes.
	if res := e.checkUpdateConflicts(tableEntry, colDefs, changes); res.Error != nil {
		return res
	}
	// Enforce FOREIGN KEY parent actions: children referencing the old
	// key values are restricted (error) or cascaded/updated.
	if e.ctx.ForeignKeys() {
		for _, ch := range changes {
			oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
			newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
			if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, ch.rowID); res.Error != nil {
				return res
			}
		}
	}
	return e.applyUpdateChanges(s.Table, tableEntry.RootPage, changes)
}

// runUpdateFail applies UPDATE OR FAIL row-by-row: each change is checked for
// UNIQUE/PK conflicts against the rows written so far and the live table
// (excluding the rows already written), then written immediately. On the first
// conflict the statement aborts, but the rows written before it survive — the
// engine's statement-level rollback skips OR FAIL statements (execRollbackOnError).
func (e *DMLExecutor) runUpdateFail(tableName string, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	colIndex := buildColumnIndex(colDefs)
	uniqueCols := uniqueColsForTable(colDefs)
	idxColsList := e.uniqueIndexColumns(tableEntry.Name)
	tree := e.dmlTableBTree(tableName, tableEntry.RootPage)
	for i := range changes {
		c := changes[i]
		if res := e.checkEarlierChanges(changes, i, c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
			return res
		}
		if res := e.checkLiveTableConflicts(tree, changes[:i], c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
			return res
		}
		// FOREIGN KEY parent action for this row, before the write (a
		// mid-statement FK error with OR FAIL keeps the rows written so far).
		if e.ctx.ForeignKeys() {
			oldRow := buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
			newRow := buildRowMapFromValues(c.values, colDefs, c.rowID)
			if res := e.ctx.FkParentUpdate(tableEntry, colDefs, oldRow, newRow, c.rowID); res.Error != nil {
				return res
			}
		}
		if res := e.writeUpdateCell(tree, tableName, tableEntry.RootPage, c, updateWriteRowID(c), c.values); res.Error != nil {
			return res
		}
	}
	return &Result{Changes: int64(len(changes))}
}

// fireUpdateAfterTriggers fires AFTER UPDATE triggers for UPDATE OR REPLACE,
// the only conflict mode whose AFTER triggers are not fired by the apply
// function itself (applyUpdateReplace fires DELETE triggers, not UPDATE).
func (e *DMLExecutor) fireUpdateAfterTriggers(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) *Result {
	if !strings.EqualFold(s.OnConflict, "REPLACE") || !e.hasTriggersForTable(tableEntry.Name) {
		return &Result{}
	}
	for _, ch := range changes {
		newRow := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		oldRow := buildRowMapFromValues(ch.oldValues, colDefs, ch.rowID)
		if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	return &Result{}
}

// invalidateSchemaIfNeeded re-reads the schema btree after direct edits to
// sqlite_schema (PRAGMA writable_schema=ON).
func (e *DMLExecutor) invalidateSchemaIfNeeded(tableName string) {
	if execquery.IsSchemaTable(tableName) {
		e.ctx.Schema().InvalidateCache()
		e.ctx.InvalidateTableCaches()
	}
}

// finishUpdate returns the RETURNING result rows when the UPDATE has a
// RETURNING clause, or the regular change-count result.
func (e *DMLExecutor) finishUpdate(s *sql.UpdateStmt, colDefs []sql.ColumnDef, returningRows [][]interface{}, result *Result) *Result {
	if s.HasReturning {
		columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
		return &Result{Columns: columns, Rows: returningRows}
	}
	return result
}

// execUpdateView routes UPDATE on a view through INSTEAD OF UPDATE triggers.
// The view's SELECT is executed (with the UPDATE's WHERE applied) to find
// matching rows; for each, the trigger fires with OLD.* and NEW.* values
// where NEW reflects the SET clause applied to the view's output columns.
func (e *DMLExecutor) execUpdateView(s *sql.UpdateStmt, viewEntry *schema.Entry) *Result {
	if !e.hasTriggersForTable(viewEntry.Name) {
		return &Result{Error: fmt.Errorf("cannot modify %s because it is a view", viewEntry.Name)}
	}
	// Qualified view column references (main.v5.b, v5.x) must resolve against
	// the view row during WHERE/SET evaluation.
	prevDML := e.currentDMLTable
	e.currentDMLTable = viewEntry.Name
	defer func() { e.currentDMLTable = prevDML }()
	viewResult := e.ctx.ExecSelectView(viewEntry)
	if viewResult.Error != nil {
		return viewResult
	}
	viewCols := viewResult.Columns
	// Apply the view's declared column list (CREATE VIEW v(a,b) AS ...) so
	// INSTEAD OF trigger OLD/NEW rows are keyed by the declared names even
	// when the SELECT produces expression columns without names.
	if decl := e.viewDeclaredColumns(viewEntry); len(decl) > 0 {
		viewCols = decl
	}
	if len(viewCols) == 0 {
		return &Result{}
	}
	// Convert each view row into a RowMap keyed by the view's column names.
	// Collect matched (old,new) pairs first so UPDATE ... ORDER BY ... LIMIT
	// applies to the trigger rows (SQLite processes only the LIMIT window).
	colDefs := make([]sql.ColumnDef, len(viewCols))
	for i, c := range viewCols {
		colDefs[i] = sql.ColumnDef{Name: c}
	}
	pairs, err := e.collectViewUpdatePairs(s, viewResult.Rows, viewCols)
	if err != nil {
		return &Result{Error: err}
	}
	pairs = orderUpdateViewPairs(e, s, pairs)
	pairs, err = limitUpdateViewPairs(e, s, pairs)
	if err != nil {
		return &Result{Error: err}
	}
	for _, p := range pairs {
		// A view is modified exclusively through INSTEAD OF triggers; the
		// declared timing is INSTEAD (parseTriggerHeader), so fire with that
		// timing — "BEFORE" would skip the trigger and silently drop the
		// update (fts4upfrom 1.3: UPDATE on a view with INSTEAD OF UPDATE
		// triggers writes through to the underlying table).
		if res := e.fireTriggers(viewEntry.Name, "UPDATE", "INSTEAD", p.newRow, p.oldRow); res != nil && res.Error != nil {
			return res
		}
	}
	// RETURNING on a view UPDATE projects the matched (old,new) pairs with
	// the NEW values (SQLite: the RETURNING row uses the SET-clause values;
	// window1 73.2 "UPDATE t2 SET c=99 WHERE b=4 RETURNING *" → 4 99).
	if s.HasReturning {
		var returningRows [][]interface{}
		for _, p := range pairs {
			values, err := e.evalReturningStrict(s.Returning, p.newRow, colDefs, viewEntry.Name)
			if err != nil {
				return &Result{Error: err}
			}
			returningRows = append(returningRows, values)
		}
		columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
		return &Result{Columns: columns, Rows: returningRows}
	}
	// The view update itself counts 0 changes (SQLite: INSTEAD OF trigger
	// interception is not counted); the trigger body's DML counts via its
	// own Exec.
	return &Result{}
}

// viewUpdatePair records a matched (old,new) row pair for an UPDATE on a
// view, before ORDER BY / LIMIT are applied to the trigger rows.
type viewUpdatePair struct {
	oldRow RowMap
	newRow RowMap
}

// buildViewOldRow converts one view result row into a RowMap keyed by the
// view's column names (with a nil rowid).
func buildViewOldRow(rowVals []interface{}, viewCols []string) RowMap {
	oldRow := make(RowMap)
	hasRowID := false
	for i, v := range rowVals {
		if i < len(viewCols) {
			oldRow[viewCols[i]] = v
			if strings.EqualFold(viewCols[i], "rowid") {
				oldRow["rowid"] = v
				hasRowID = true
			}
		}
	}
	if !hasRowID {
		oldRow["rowid"] = nil
	}
	return oldRow
}

// applyViewWhere evaluates the UPDATE's WHERE clause against a view row
// (joined with the UPDATE FROM tables when present), returning all matched row
// maps to evaluate SET expressions against. For UPDATE ... FROM, SQLite fires
// the INSTEAD OF trigger once per JOIN COMBINATION (window1 73.4: 3 view rows
// × 3 FROM rows = 9 trigger firings), so every matching joined row is returned.
func (e *DMLExecutor) applyViewWhere(s *sql.UpdateStmt, oldRow RowMap) ([]RowMap, bool, error) {
	if s.Where == nil {
		return []RowMap{oldRow}, true, nil
	}
	if s.From.Name != "" || s.From.Subquery != nil {
		joined, jerr := e.joinUpdateFromRows(s, oldRow)
		if jerr != nil {
			return nil, false, jerr
		}
		var matched []RowMap
		for _, jrow := range joined {
			pass, err := e.ctx.EvalBool(s.Where, jrow)
			if err == nil && pass {
				matched = append(matched, jrow)
			}
		}
		return matched, len(matched) > 0, nil
	}
	pass, err := e.ctx.EvalBool(s.Where, oldRow)
	if err != nil || !pass {
		return nil, false, nil
	}
	return []RowMap{oldRow}, true, nil
}

// applyViewSetAssignments builds the NEW view row by applying the SET
// assignments to the old values (evaluated against the matched eval row).
// winVals carries precomputed window-function values per SET node (nil when
// the SET clause has no window functions); rowIdx is the eval-row index used
// to pick the per-row window value.
func (e *DMLExecutor) applyViewSetAssignments(s *sql.UpdateStmt, oldRow, evalRow RowMap, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (RowMap, error) {
	newRow := make(RowMap, len(oldRow))
	for k, v := range oldRow {
		newRow[k] = v
	}
	for _, a := range s.Assignments {
		v, err := e.ctx.EvalExpr(a.Value, evalRow)
		if err != nil && len(winVals) > 0 {
			// A window function in SET is not a plain scalar function; when
			// the assignment's expression contains a window function, rebuild
			// the value with the precomputed window results substituted.
			if sub, suberr := e.substituteWindowSetValue(a.Value, evalRow, winVals, rowIdx); suberr == nil {
				v = sub
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("exec: failed to evaluate SET expression for %s: %w", a.Column, err)
		}
		newRow[a.Column] = util.UnwrapColumnValue(v)
	}
	return newRow, nil
}

// substituteWindowSetValue re-evaluates a SET expression with window functions
// replaced by their precomputed values. Returns the evaluated value, or an
// error when the expression cannot be evaluated with substitutions.
func (e *DMLExecutor) substituteWindowSetValue(expr sql.Expr, row RowMap, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (interface{}, error) {
	replaced, err := replaceWindowFuncs(expr, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	return e.ctx.EvalExpr(replaced, row)
}

// replaceWindowFuncs deep-copies an expression tree, replacing each window
// FuncCall node with a literal holding its precomputed value for rowIdx.
func replaceWindowFuncs(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	// Clone the expression, substituting window FuncCalls with literals.
	clone, err := cloneExprWithWindowSubst(expr, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	return clone, nil
}

// cloneExprWithWindowSubst deep-copies an expression, replacing each window
// FuncCall with a NumericLit/StringLit/NullLit carrying its precomputed value.
func cloneExprWithWindowSubst(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	if expr == nil {
		return nil, nil
	}
	switch v := expr.(type) {
	case *sql.FuncCall:
		return cloneWindowSubstFuncCall(v, winVals, rowIdx)
	case *sql.BinaryOp, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		return cloneWindowSubstPair(expr, winVals, rowIdx)
	case *sql.UnaryOp, *sql.ParenExpr, *sql.CastExpr, *sql.IsNull,
		*sql.IsNotNull, *sql.IsTrue, *sql.IsFalse:
		return cloneWindowSubstSingle(expr, winVals, rowIdx)
	case *sql.Between:
		return cloneWindowSubstBetween(v, winVals, rowIdx)
	case *sql.InList:
		return cloneWindowSubstInList(v, winVals, rowIdx)
	case *sql.CaseExpr:
		return cloneWindowSubstCase(v, winVals, rowIdx)
	default:
		// Leaf nodes (ColumnRef, literals, Subquery, etc.) are returned as-is.
		return expr, nil
	}
}

// cloneWindowSubstFuncCall clones a FuncCall, substituting a window function
// with its precomputed value and recursing into a plain function's arguments.
func cloneWindowSubstFuncCall(v *sql.FuncCall, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	if v.Over != nil {
		vals, ok := winVals[v]
		if !ok {
			return nil, fmt.Errorf("window function %s has no precomputed value", v.Name)
		}
		if rowIdx >= len(vals) {
			return nil, fmt.Errorf("window function %s row index out of range", v.Name)
		}
		return literalForValue(vals[rowIdx]), nil
	}
	args := make([]sql.Expr, len(v.Args))
	for i, a := range v.Args {
		c, err := cloneExprWithWindowSubst(a, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		args[i] = c
	}
	cp := *v
	cp.Args = args
	return &cp, nil
}

// cloneWindowSubstPair clones a two-operand expression node (BinaryOp /
// IsDistinctFrom / IsNotDistinctFrom), recursing into both operands.
func cloneWindowSubstPair(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	l, r, err := cloneWindowSubstOperands(expr, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return &sql.BinaryOp{Operator: v.Operator, Left: l, Right: r}, nil
	case *sql.IsDistinctFrom:
		return &sql.IsDistinctFrom{Left: l, Right: r}, nil
	case *sql.IsNotDistinctFrom:
		return &sql.IsNotDistinctFrom{Left: l, Right: r}, nil
	}
	return expr, nil
}

// cloneWindowSubstOperands clones the two operands of a pair expression.
func cloneWindowSubstOperands(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, sql.Expr, error) {
	switch v := expr.(type) {
	case *sql.BinaryOp:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	case *sql.IsDistinctFrom:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	case *sql.IsNotDistinctFrom:
		return cloneWindowSubstPairChildren(v.Left, v.Right, winVals, rowIdx)
	}
	return nil, nil, nil
}

// cloneWindowSubstPairChildren clones a left/right child pair.
func cloneWindowSubstPairChildren(left, right sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, sql.Expr, error) {
	l, err := cloneExprWithWindowSubst(left, winVals, rowIdx)
	if err != nil {
		return nil, nil, err
	}
	r, err := cloneExprWithWindowSubst(right, winVals, rowIdx)
	if err != nil {
		return nil, nil, err
	}
	return l, r, nil
}

// cloneWindowSubstSingle clones a single-operand expression node (UnaryOp,
// ParenExpr, CastExpr, IsNull, IsNotNull, IsTrue, IsFalse).
func cloneWindowSubstSingle(expr sql.Expr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(winSingleOperand(expr), winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return &sql.UnaryOp{Operator: v.Operator, Operand: o}, nil
	case *sql.ParenExpr:
		return &sql.ParenExpr{Expr: o}, nil
	case *sql.CastExpr:
		return &sql.CastExpr{Operand: o, AsType: v.AsType}, nil
	case *sql.IsNull:
		return &sql.IsNull{Operand: o}, nil
	case *sql.IsNotNull:
		return &sql.IsNotNull{Operand: o}, nil
	case *sql.IsTrue:
		return &sql.IsTrue{Operand: o}, nil
	case *sql.IsFalse:
		return &sql.IsFalse{Operand: o}, nil
	}
	return expr, nil
}

// winSingleOperand returns the single operand of a unary-like expression node.
func winSingleOperand(expr sql.Expr) sql.Expr {
	switch v := expr.(type) {
	case *sql.UnaryOp:
		return v.Operand
	case *sql.ParenExpr:
		return v.Expr
	case *sql.CastExpr:
		return v.Operand
	case *sql.IsNull:
		return v.Operand
	case *sql.IsNotNull:
		return v.Operand
	case *sql.IsTrue:
		return v.Operand
	case *sql.IsFalse:
		return v.Operand
	}
	return nil
}

// cloneWindowSubstBetween clones a BETWEEN expression, recursing into the
// operand, low, and high bounds.
func cloneWindowSubstBetween(v *sql.Between, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	lo, err := cloneExprWithWindowSubst(v.Low, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	hi, err := cloneExprWithWindowSubst(v.High, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	return &sql.Between{Negated: v.Negated, Operand: o, Low: lo, High: hi}, nil
}

// cloneWindowSubstInList clones an IN-list expression, recursing into the
// operand and every list item.
func cloneWindowSubstInList(v *sql.InList, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	o, err := cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
	if err != nil {
		return nil, err
	}
	list := make([]sql.Expr, len(v.List))
	for i, item := range v.List {
		c, err := cloneExprWithWindowSubst(item, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		list[i] = c
	}
	return &sql.InList{Negated: v.Negated, Operand: o, List: list}, nil
}

// cloneWindowSubstCase clones a CASE expression, recursing into the operand,
// WHEN/THEN pairs, and ELSE.
func cloneWindowSubstCase(v *sql.CaseExpr, winVals map[*sql.FuncCall][]interface{}, rowIdx int) (sql.Expr, error) {
	var operand sql.Expr
	var err error
	if v.Operand != nil {
		operand, err = cloneExprWithWindowSubst(v.Operand, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
	}
	whens := make([]sql.WhenClause, len(v.Whens))
	for i, w := range v.Whens {
		wc, err := cloneExprWithWindowSubst(w.When, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		tc, err := cloneExprWithWindowSubst(w.Then, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
		whens[i] = sql.WhenClause{When: wc, Then: tc}
	}
	var els sql.Expr
	if v.Else != nil {
		els, err = cloneExprWithWindowSubst(v.Else, winVals, rowIdx)
		if err != nil {
			return nil, err
		}
	}
	return &sql.CaseExpr{Operand: operand, Whens: whens, Else: els}, nil
}

// literalForValue wraps a window value as a literal expression node.
