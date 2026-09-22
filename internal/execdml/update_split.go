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

// execUpdate wraps the UPDATE pipeline with the echo module's error prefix:
// an UPDATE routed through an echo virtual table reports failures from the
// source write as "echo-vtab-error: %s" (test8.c echoError / xUpdate).
func (e *DMLExecutor) execUpdate(s *sql.UpdateStmt) *Result {
	if _, ok := e.ctx.EchoVTabSource(s.Table); !ok {
		return e.execUpdateInner(s)
	}
	e.echoWriteDepth++
	res := e.execUpdateInner(s)
	if res.Error != nil {
		res.Error = e.wrapEchoWriteError(res.Error)
	}
	e.echoWriteDepth--
	return res
}

// execUpdateInner is execUpdate's statement pipeline (the echo write-through
// wrapper above re-routes its errors). The defers below pin THIS frame: the
// CTE pop, the outer-conflict restore, the DML-context restore, the
// AUTOINCREMENT reset, and the SET-column push must all unwind at statement
// end, so the phases they guard are extracted as validation helpers rather
// than defer-owning steps.
func (e *DMLExecutor) execUpdateInner(s *sql.UpdateStmt) *Result {
	// The UPDATE's WITH clause (CTEs) applies to its FROM tables and SET
	// expressions, including the view/INSTEAD-OF path. Push the CTEs onto
	// the scope stack so UPDATE ... FROM input resolves input as a CTE
	// (upfrom2-3.1) even when the target is a view.
	defer e.pushUpdateCTEs(s)()
	// Echo virtual tables write through to their source table (vtabA-3.1).
	e.redirectEchoVTab(s)
	if res, handled := e.routeUpdateVTab(s); handled {
		return res
	}
	if err := e.ctx.Authorize(auth.ActionUpdate, s.Table, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	tableEntry, dbCtx, res := e.openUpdateTarget(s)
	if res != nil {
		return res
	}

	// Publish the statement's ON CONFLICT policy for trigger-body steps
	// without an explicit OR clause (SQLite trigger.c codeTriggerProgram).
	// Only the outermost DML statement sets it.
	outerPrev := e.ctx.OuterOrConflict()
	if e.ctx.TriggerDepth() == 0 && outerPrev == "" {
		e.ctx.SetOuterOrConflict(s.OnConflict)
		defer e.ctx.SetOuterOrConflict(outerPrev)
	}

	// Track the modified table's database context for trigger scoping.
	prevDMLCtx := e.currentDMLCtx
	e.currentDMLCtx = dbCtx
	if res := e.CheckSameFileWriteConflictRes(dbCtx); res != nil {
		return res
	}
	defer func() { e.currentDMLCtx = prevDMLCtx }()

	cleanup, res := e.guardUpdateTarget(tableEntry)
	if res != nil {
		return res
	}
	defer cleanup()

	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)
	if res := e.validateUpdateExprResolution(s, tableEntry, colDefs); res != nil {
		return res
	}
	if res, handled := e.routeUpdateFTS(tableEntry, colDefs, s); handled {
		return res
	}
	if res := e.validateUpdateIndexCollations(s, tableEntry, colDefs); res != nil {
		return res
	}

	// Record which columns this UPDATE statement's SET clause assigns, so
	// UPDATE OF <cols> triggers fire only when a listed column is in the set.
	// Cleared on return (the engine is single-threaded per connection).
	defer e.pushUpdateSetColumns(s)()

	return e.runUpdatePipeline(s, tableEntry, colDefs)
}

// pushUpdateCTEs pushes the UPDATE's WITH-clause CTEs onto the scope stack
// and returns the restore func (no-op when the statement has no WITH clause).
func (e *DMLExecutor) pushUpdateCTEs(s *sql.UpdateStmt) func() {
	if len(s.CTEs) == 0 {
		return func() {}
	}
	e.ctx.PushCTEScope(s.CTEs)
	return func() { e.ctx.PopCTEScope() }
}

// routeUpdateVTab routes generic updatable virtual tables (sqlite_dbpage
// etc.) before the b-tree paths: their rows come from xFilter, not a root
// page. handled=false keeps the b-tree UPDATE pipeline.
func (e *DMLExecutor) routeUpdateVTab(s *sql.UpdateStmt) (*Result, bool) {
	if res := e.rejectUnsafeVTabUse(s.Table); res != nil {
		return res, true
	}
	return e.execVTabUpdate(s)
}

// openUpdateTarget resolves the UPDATE's target table and enforces alias
// masking: with "UPDATE t1 AS a", the original table name is not a valid
// qualifier in WHERE/SET (wherelimit-0.5.2). A missing table routes through
// INSTEAD OF UPDATE triggers on a view.
func (e *DMLExecutor) openUpdateTarget(s *sql.UpdateStmt) (*schema.Entry, *execquery.DatabaseContext, *Result) {
	tableEntry, dbCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		return nil, nil, e.updateOnMissingTable(s, err)
	}
	if res := e.validateDMLAliasQualifier(s.Table, s.Alias, updateTargetExprs(s)); res != nil {
		return nil, nil, res
	}
	return tableEntry, dbCtx, nil
}

// updateTargetExprs collects the UPDATE's WHERE and SET value expressions
// (the expressions qualified-name validation walks).
func updateTargetExprs(s *sql.UpdateStmt) []sql.Expr {
	exprs := make([]sql.Expr, 0, len(s.Assignments)+1)
	for _, a := range s.Assignments {
		exprs = append(exprs, a.Value)
	}
	return append(exprs, s.Where)
}

// guardUpdateTarget protects system and pragma virtual tables from
// modification, and schedules the AUTOINCREMENT cache reset for direct
// sqlite_sequence edits (the next INSERT must read the real table fresh).
// It returns the cleanup func to defer (a no-op in the common case).
func (e *DMLExecutor) guardUpdateTarget(tableEntry *schema.Entry) (func(), *Result) {
	if e.ctx.IsNonModifiableTable(tableEntry) {
		return func() {}, &Result{Error: fmt.Errorf("table %s may not be modified", tableEntry.Name)}
	}
	if isSQLiteSequenceName(tableEntry.Name) {
		return e.ctx.ResetAutoIncSeq, nil
	}
	return func() {}, nil
}

// validateUpdateExprResolution performs prepare-time name resolution
// (resolve.c parity): WHERE and SET value expressions must resolve every
// column and function. UPDATE...FROM is skipped — its WHERE references the
// joined tables' columns.
func (e *DMLExecutor) validateUpdateExprResolution(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if s.From.Name != "" {
		return nil
	}
	qualifiers := []string{s.Table}
	if s.Alias != "" {
		qualifiers = append(qualifiers, s.Alias)
	}
	return e.validateDMLExprs(qualifiers, colDefs, !hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)), updateTargetExprs(s))
}

// routeUpdateFTS sends virtual-table updates to the FTS engines: fts5
// updates route through the fts5 engine; FTS3/4 updates go directly to the
// FTS table (SQLite's fts3UpdateMethod handles docid and content column
// updates). handled=false keeps the b-tree UPDATE pipeline.
func (e *DMLExecutor) routeUpdateFTS(tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.UpdateStmt) (*Result, bool) {
	// Route fts5 virtual table updates through the fts5 engine.
	if t5, ok := e.ctx.FTS5Tables()[tableEntry.Name]; ok {
		return e.execFTS5Update(t5, colDefs, s), true
	}
	// Route FTS virtual table updates directly to the FTS table (SQLite's
	// fts3UpdateMethod handles docid and content column updates).
	if ftsTable, ok := e.ctx.FTSTables()[tableEntry.Name]; ok {
		return e.ctx.ExecFTSUpdate(tableEntry.Name, ftsTable, colDefs, s), true
	}
	return nil, false
}

// validateUpdateIndexCollations enforces that index maintenance collations
// resolve at prepare time (build.c sqlite3LocateCollSeq). SQLite maintains
// an index only when the statement assigns one of its key columns (or a
// column its expression keys / partial predicate reference), so "SET c1 =
// ..." fails while "SET c2 = ..." succeeds after a reopen without the
// collation (collate3-3.2/3.3). Assigning the rowid rebuilds every index.
func (e *DMLExecutor) validateUpdateIndexCollations(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	changed := make(map[string]bool, len(s.Assignments)+len(s.SetParenColumns))
	rowidAssigned := false
	for _, a := range s.Assignments {
		lower := strings.ToLower(a.Column)
		changed[lower] = true
		if lower == "rowid" || lower == "_rowid_" || lower == "oid" {
			rowidAssigned = true
		}
	}
	for _, col := range s.SetParenColumns {
		changed[strings.ToLower(col)] = true
	}
	if len(changed) == 0 {
		return nil
	}
	var maintained map[string]bool
	if !rowidAssigned {
		maintained = changed
	}
	return e.validateIndexCollations(tableEntry, colDefs, maintained)
}

// runUpdatePipeline drives a resolved b-tree UPDATE from prepare through
// collect, constraint pre-checks, apply (dispatch), AFTER triggers, and the
// RETURNING/schema-cookie epilogue.
func (e *DMLExecutor) runUpdatePipeline(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	if res := e.prepareUpdate(s, tableEntry, colDefs); res != nil {
		return res
	}
	if res := e.prepareUpdateTriggers(tableEntry); res != nil {
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
	// per-row check happens inside applyUpdateIgnore (below). Per-constraint
	// ON CONFLICT clauses (statement OR-clause > column clause) may resolve a
	// NOT NULL violation by substituting the column DEFAULT (REPLACE) or
	// skipping the row (IGNORE) — notnull-2.6..2.9.
	changes, pres := e.preCheckUpdate(s, tableEntry, colDefs, changes)
	if pres.Error != nil {
		// ON CONFLICT FAIL keeps the rows updated before the violation
		// (SQLite's per-row loop writes incrementally — check-6.5/6.6
		// "UPDATE OR FAIL t1 SET x=7-x" keeps the first row's change).
		if pres.KeepPriorRowsOnError() && len(changes) > 0 {
			if ares := e.dispatchUpdate(s, tableEntry, colDefs, changes); ares.Error != nil {
				return ares
			}
		}
		return pres
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

// prepareUpdate validates an UPDATE's RETURNING clause and resolves the SET
// target columns. SQLite resolves assignment targets at prepare time
// (update.c sqlite3Update's column lookup), so an unknown column raises
// "no such column: X" even when no row matches the WHERE clause
// (update.test 9.1).
func (e *DMLExecutor) prepareUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) *Result {
	colIndex := buildColumnIndex(colDefs)
	hasRowidColumn := execquery.RowHasRowIDColumn(colDefs)
	for _, a := range s.Assignments {
		if _, ok := colIndex[strings.ToLower(a.Column)]; ok {
			continue
		}
		// SET rowid/_rowid_/oid moves the cell's rowid (only when the table
		// does not declare a column shadowing that name).
		if execquery.IsRowIDName(a.Column) && !hasRowidColumn {
			continue
		}
		return &Result{Error: fmt.Errorf("no such column: %s", a.Column)}
	}
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
// applyUpdateIgnore. A NOT NULL violation is resolved by the effective
// conflict action — statement OR-clause first, then the column's own
// ON CONFLICT clause: REPLACE substitutes the column DEFAULT (no DEFAULT →
// ABORT semantics, SQLite ON CONFLICT docs), IGNORE drops the row's change.
// The (possibly filtered) change list is returned so skipped rows are not
// applied or returned via RETURNING.
func (e *DMLExecutor) preCheckUpdate(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) ([]updateChange, *Result) {
	if strings.EqualFold(s.OnConflict, "IGNORE") {
		return changes, &Result{}
	}
	// Only materialize deferred SET values when there are constraints to
	// check: the check needs the new values, but materializing here would
	// fire the changes() user-function ordering before the apply loop (the
	// per-row interleaving only matters for the trigger path, which defers).
	if e.updateHasConstraints(colDefs) {
		colIndex := buildColumnIndex(colDefs)
		for i := range changes {
			if err := e.materializeChangeValues(&changes[i], s, colIndex, colDefs); err != nil {
				return nil, &Result{Error: err}
			}
		}
	}
	return e.resolveUpdateNotNullConflicts(s, tableEntry, colDefs, changes)
}

// resolveUpdateNotNullConflicts validates NOT NULL and CHECK constraints for
// each change, resolving NOT NULL violations per the effective conflict
// action (statement OR-clause overrides the column clause; the column's
// ON CONFLICT clause applies otherwise; plain violations error). REPLACE
// substitutes the column DEFAULT (looping for multiple NOT NULL columns);
// IGNORE drops the change so the row is left untouched (notnull-2.9 keeps
// {1 2 3 4 5} — the whole update of that row is skipped).
func (e *DMLExecutor) resolveUpdateNotNullConflicts(s *sql.UpdateStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) ([]updateChange, *Result) {
	if !hasNotNullOrCheckConstraint(colDefs) && len(e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL)) == 0 {
		return changes, &Result{}
	}
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	var pkCols map[int]bool
	if withoutRowid {
		pkCols = e.primaryKeyColIndices(tableEntry.Name, tableEntry.SQL, colDefs)
	}
	prevDML := e.currentDMLTable
	e.currentDMLTable = tableEntry.Name
	defer func() { e.currentDMLTable = prevDML }()

	stmtClause := strings.ToUpper(s.OnConflict)
	kept := make([]updateChange, 0, len(changes))
	for _, ch := range changes {
		r := e.resolveChangeNotNullConflicts(ch, tableEntry, colDefs, withoutRowid, pkCols, stmtClause)
		if r.res != nil {
			// ON CONFLICT FAIL: the changes validated before the violation
			// were already written in SQLite's per-row loop and survive the
			// failed statement (check-6.5/6.6).
			if stmtClause == "FAIL" && len(kept) > 0 {
				r.res.SetKeepPriorRowsOnError()
				return kept, r.res
			}
			return changes, r.res
		}
		if r.drop {
			// Skip the whole row: the change is dropped.
			continue
		}
		kept = append(kept, *r.keep)
	}
	return kept, &Result{}
}

// notNullResolution is one change's NOT NULL/CHECK constraint outcome: keep
// the (possibly DEFAULT-substituted) change, drop it, or abort the statement.
type notNullResolution struct {
	keep *updateChange
	drop bool
	res  *Result
}

// resolveChangeNotNullConflicts validates one change, re-validating after
// every DEFAULT substitution until the row passes or its outcome is decided.
func (e *DMLExecutor) resolveChangeNotNullConflicts(ch updateChange, tableEntry *schema.Entry, colDefs []sql.ColumnDef, withoutRowid bool, pkCols map[int]bool, stmtClause string) notNullResolution {
	for {
		row := buildRowMapFromValues(ch.values, colDefs, ch.rowID)
		res := e.checkRowUpdateConstraints(ch.values, row, tableEntry, colDefs, withoutRowid, pkCols)
		if res.Error == nil {
			return notNullResolution{keep: &ch}
		}
		errStr := res.Error.Error()
		if !strings.Contains(errStr, "NOT NULL constraint failed") {
			return notNullResolution{res: res} // CHECK (and other) violations stand
		}
		cd := violatedNotNullColumn(errStr, colDefs)
		if cd == nil {
			return notNullResolution{res: res}
		}
		action := stmtClause
		if action == "" {
			action = cd.OnConflict
		}
		switch action {
		case "IGNORE":
			return notNullResolution{drop: true}
		case "REPLACE":
			values, sub := e.substituteNotNullDefault(ch, cd, colDefs, res)
			if sub != nil {
				return notNullResolution{res: sub}
			}
			ch.values = values
			continue // re-validate the substituted row
		default:
			return notNullResolution{res: res}
		}
	}
}

// substituteNotNullDefault substitutes a NOT NULL-violating column's DEFAULT
// under the REPLACE action and recomputes generated columns. A column without
// a DEFAULT yields ABORT semantics (SQLite ON CONFLICT clause documentation):
// the original violation Result is returned and values is nil.
func (e *DMLExecutor) substituteNotNullDefault(ch updateChange, cd *sql.ColumnDef, colDefs []sql.ColumnDef, res *Result) ([]interface{}, *Result) {
	if cd.Default == nil {
		// REPLACE without a DEFAULT uses ABORT semantics
		// (SQLite ON CONFLICT clause documentation).
		return nil, res
	}
	dv, derr := e.ctx.EvalExpr(cd.Default, nil)
	if derr != nil {
		return nil, &Result{Error: derr}
	}
	values := append([]interface{}(nil), ch.values...)
	values[cdIndex(colDefs, cd.Name)] = dv
	if gerr := e.computeGeneratedValues(colDefs, values); gerr != nil {
		return nil, &Result{Error: gerr}
	}
	return values, nil
}

// violatedNotNullColumn extracts the column definition named by a
// "NOT NULL constraint failed: <table>.<column>" error.
func violatedNotNullColumn(errStr string, colDefs []sql.ColumnDef) *sql.ColumnDef {
	dot := strings.LastIndex(errStr, ".")
	if dot < 0 {
		return nil
	}
	name := errStr[dot+1:]
	for i := range colDefs {
		if strings.EqualFold(colDefs[i].Name, name) {
			return &colDefs[i]
		}
	}
	return nil
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
	// A table whose columns (or table-level constraints) carry their own ON
	// CONFLICT clauses resolves each row's conflict under the VIOLATED
	// constraint's clause — process row-by-row (conflict-9.3..9.25: a
	// single UPDATE mixes IGNORE/FAIL/REPLACE/ABORT/ROLLBACK columns).
	if hasColumnConflictClauses(colDefs, tableEntry, e) {
		return e.runPlainUpdatePerRow(s, tableEntry, colDefs, changes)
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
	wrOrder := e.ctx.WRStorageOrder(tableEntry.SQL, colDefs)
	tree := e.dmlTableBTree(tableName, tableEntry.RootPage)
	for i := range changes {
		c := changes[i]
		if res := e.checkEarlierChanges(changes, i, c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry.Name); res.Error != nil {
			return res
		}
		if res := e.checkLiveTableConflictsWR(tree, changes[:i], c, colDefs, colIndex, uniqueCols, idxColsList, tableEntry, wrOrder); res.Error != nil {
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
		if res := e.writeUpdateCell(tree, tableName, tableEntry.RootPage, c, updateWriteRowID(c), c.values, tableEntry, colDefs); res.Error != nil {
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
	colDefs := make([]sql.ColumnDef, len(viewCols))
	for i, c := range viewCols {
		colDefs[i] = sql.ColumnDef{Name: c}
	}
	// Convert each view row into a RowMap keyed by the view's column names.
	// Collect matched (old,new) pairs first so UPDATE ... ORDER BY ... LIMIT
	// applies to the trigger rows (SQLite processes only the LIMIT window).
	pairs, err := e.collectViewUpdatePairs(s, viewResult.Rows, viewCols)
	if err != nil {
		return &Result{Error: err}
	}
	pairs = orderUpdateViewPairs(e, s, pairs)
	pairs, err = limitUpdateViewPairs(e, s, pairs)
	if err != nil {
		return &Result{Error: err}
	}
	if res := e.fireInsteadOfUpdateTriggers(viewEntry.Name, pairs); res != nil {
		return res
	}
	// RETURNING on a view UPDATE projects the matched (old,new) pairs with
	// the NEW values (SQLite: the RETURNING row uses the SET-clause values;
	// window1 73.2 "UPDATE t2 SET c=99 WHERE b=4 RETURNING *" → 4 99).
	if s.HasReturning {
		return e.viewUpdateReturning(s, viewEntry, pairs, colDefs)
	}
	// The view update itself counts 0 changes (SQLite: INSTEAD OF trigger
	// interception is not counted); the trigger body's DML counts via its
	// own Exec.
	return &Result{}
}

// fireInsteadOfUpdateTriggers fires the INSTEAD OF UPDATE triggers for each
// matched view row pair.
func (e *DMLExecutor) fireInsteadOfUpdateTriggers(viewName string, pairs []viewUpdatePair) *Result {
	for _, p := range pairs {
		// A view is modified exclusively through INSTEAD OF triggers; the
		// declared timing is INSTEAD (parseTriggerHeader), so fire with that
		// timing — "BEFORE" would skip the trigger and silently drop the
		// update (fts4upfrom 1.3: UPDATE on a view with INSTEAD OF UPDATE
		// triggers writes through to the underlying table).
		if res := e.fireTriggers(viewName, "UPDATE", "INSTEAD", p.newRow, p.oldRow); res != nil && res.Error != nil {
			return res
		}
	}
	return nil
}

// viewUpdateReturning evaluates an UPDATE-on-view RETURNING clause against
// the matched (old,new) pairs' NEW rows.
func (e *DMLExecutor) viewUpdateReturning(s *sql.UpdateStmt, viewEntry *schema.Entry, pairs []viewUpdatePair, colDefs []sql.ColumnDef) *Result {
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
