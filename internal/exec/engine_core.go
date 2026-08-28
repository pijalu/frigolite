package exec

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// cloneStmtsWithValues clones the cached statement list and substitutes new
// literal values. This avoids re-parsing structurally identical SQL.
// Currently handles InsertStmt values; other types are returned as-is.
func cloneStmtsWithValues(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, error) {
	result := make([]sql.Stmt, len(stmts))
	valIdx := 0
	for i, stmt := range stmts {
		switch s := stmt.(type) {
		case *sql.InsertStmt:
			clone, err := cloneInsertStmt(s, values, &valIdx)
			if err != nil {
				return nil, err
			}
			result[i] = clone
		default:
			// For non-InsertStmt types, return the original (they don't need value substitution)
			result[i] = stmt
		}
	}
	if valIdx != len(values) {
		return nil, fmt.Errorf("template cache: unused values (%d remaining)", len(values)-valIdx)
	}
	return result, nil
}

// cloneInsertStmt clones an InsertStmt, substituting cached literal values into
// its VALUES tuples. valIdx is advanced as values are consumed.
func cloneInsertStmt(s *sql.InsertStmt, values []interface{}, valIdx *int) (*sql.InsertStmt, error) {
	clone := &sql.InsertStmt{
		Table:        s.Table,
		Columns:      s.Columns,
		Values:       make([][]sql.Expr, len(s.Values)),
		OnConflict:   s.OnConflict,
		Returning:    s.Returning,
		HasReturning: s.HasReturning,
		IsReplace:    s.IsReplace,
		OrIgnore:     s.OrIgnore,
		OrConflict:   s.OrConflict,
	}
	// Clone values tuples
	for vi, tuple := range s.Values {
		clone.Values[vi] = make([]sql.Expr, len(tuple))
		for vj, expr := range tuple {
			cloned, err := cloneInsertValue(expr, values, valIdx)
			if err != nil {
				return nil, err
			}
			clone.Values[vi][vj] = cloned
		}
	}
	// Clone Select for INSERT ... SELECT
	if s.Select != nil {
		clone.Select = s.Select
	}
	return clone, nil
}

// cloneInsertValue substitutes a cached literal value for a NumericLit/StringLit
// expression, advancing valIdx. Other expressions are returned unchanged.
func cloneInsertValue(expr sql.Expr, values []interface{}, valIdx *int) (sql.Expr, error) {
	switch expr.(type) {
	case *sql.NumericLit, *sql.StringLit:
	default:
		// Non-value expression — keep original
		return expr, nil
	}
	if *valIdx >= len(values) {
		return nil, fmt.Errorf("template cache: not enough values (need %d, have %d)", len(values), *valIdx+1)
	}
	val := values[*valIdx]
	*valIdx++
	switch v := val.(type) {
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(v, 10)}, nil
	case float64:
		return &sql.NumericLit{Value: strconv.FormatFloat(v, 'g', -1, 64)}, nil
	case string:
		return &sql.StringLit{Value: v}, nil
	}
	return expr, nil // keep original
}

// Prepare parses and caches a SQL statement. Repeated calls with the same SQL
// string return the cached parsed statements without re-parsing.
// Additionally, structurally identical SQL (same after replacing literal values
// with placeholders) uses a template cache to avoid full re-parsing.
func (e *Engine) Prepare(sqlStr string) ([]sql.Stmt, error) {
	// Check exact match cache first (fastest)
	if cached, ok := e.caches.stmtCache[sqlStr]; ok {
		return cached, nil
	}
	if len(e.caches.stmtCache) >= maxStmtCacheSize {
		e.caches.stmtCache = make(map[string][]sql.Stmt)
	}

	// Check template cache — normalize SQL and see if we've seen this structure
	normSQL, values := normalizeSQL(sqlStr)
	if stmts, ok := e.tryTemplateCache(sqlStr, normSQL, values); ok {
		return stmts, nil
	}

	// Full parse using go-lemon generated parser
	stmts, err := parse.ParseSQL(sqlStr)
	if err != nil {
		if len(stmts) > 0 {
			// SQLite executes the parseable prefix before reporting a
			// trailing syntax error. Don't cache partial parses.
			return stmts, err
		}
		return nil, err
	}
	// Prepare-time parameter validation (resolve.c sqlite3ExprAssignVarNumber):
	// ?0 and ?NNN above SQLITE_MAX_VARIABLE_NUMBER fail the prepare itself
	// with "variable number must be between ?1 and ?%d". Cached statements
	// were validated on their first parse, so only fresh parses check here.
	if _, perr := CollectParameterNames(sqlStr); perr != nil {
		return stmts, perr
	}
	e.caches.stmtCache[sqlStr] = stmts
	e.storeTemplateCache(sqlStr, normSQL, values, stmts)
	return stmts, nil
}

// tryTemplateCache attempts to reuse a cached AST template for structurally
// identical SQL (same after replacing literal values). It returns (nil, false)
// when there is no usable template, falling through to a full parse.
func (e *Engine) tryTemplateCache(sqlStr, normSQL string, values []interface{}) ([]sql.Stmt, bool) {
	if normSQL == sqlStr || len(values) == 0 {
		return nil, false
	}
	cached, ok := e.caches.templateCache[normSQL]
	if !ok {
		return nil, false
	}
	// Template cache hit — clone AST with new values. If the clone fails
	// (e.g. wrong value count), fall through to re-parse.
	cloned, err := cloneStmtsWithValues(cached.ast, values)
	if err != nil {
		return nil, false
	}
	// Also cache by exact SQL for future exact matches
	e.caches.stmtCache[sqlStr] = cloned
	return cloned, true
}

// storeTemplateCache records a parsed statement list as a template for
// structurally identical SQL, bounded by maxTemplateCacheSize.
func (e *Engine) storeTemplateCache(sqlStr, normSQL string, values []interface{}, stmts []sql.Stmt) {
	if normSQL == sqlStr || len(values) == 0 || len(e.caches.templateCache) >= maxTemplateCacheSize {
		return
	}
	if e.caches.templateCache == nil {
		e.caches.templateCache = make(map[string]*sqlTemplateEntry)
	}
	e.caches.templateCache[normSQL] = &sqlTemplateEntry{
		template: normSQL,
		ast:      stmts,
	}
}

// detectExternalSchemaChanges checks every attached database's schema manager
// for external file modification (an attached file written by another
// connection). When a change is detected the pager cache, tableCache, and
// rowid/sequence caches are invalidated so the next lookup re-reads the file.
func (e *Engine) detectExternalSchemaChanges() {
	// Only the outermost statement checks for external file modification.
	// Nested Exec calls (trigger bodies, the FTS segment flush's internal
	// shadow-table writes) run mid-statement where no other connection can
	// commit; checking there would issue a file read (FileChangeCounter
	// pread) for every internal write — the dominant cost of per-row FTS
	// builds (fts3_build_db_2 20000: 5 preads per flush over 20k flushes).
	// The FTS segment flush itself (which runs at depth 1 inside
	// execFlushAutocommit) must also skip the check: its internal segdir
	// reads would compare the file counter against an in-flight dirty state
	// and could drop the pager cache (unflushed merge writes) — the cause of
	// fts4merge 5.x losing the L1/L2 segments after a merge sequence.
	if e.tx.execDepth > 1 || e.tx.inFTSFlush {
		return
	}
	changed := false
	for _, ctx := range e.databases {
		if e.externalSchemaChanged(ctx) {
			changed = true
		}
	}
	if changed {
		e.caches.tableCache = make(map[string]*cachedTableEntry)
		e.caches.nextRowIDCache = make(map[rowidCacheKey]int64)

		e.caches.autoIncSeq = make(map[rowidCacheKey]int64)
	}
}

// externalSchemaChanged checks one database's schema manager for an external
// file modification and reports whether it was invalidated. The MAIN database
// is included: a second connection to the same file may have committed DDL
// (e.g. ALTER TABLE RENAME COLUMN) that invalidates cached table entries
// (altercol-2.3). TEMP is in-memory and never tracked.
func (e *Engine) externalSchemaChanged(ctx *DatabaseContext) bool {
	if ctx == nil || ctx.Schema == nil || ctx.Pager == nil {
		return false
	}
	upper := strings.ToUpper(ctx.Name)
	if upper == "TEMP" || upper == "TEMPORARY" {
		return false
	}
	ctx.Schema.CheckExternalMod()
	if !ctx.Schema.ConsumeExternalInvalidation() {
		return false
	}
	// Another connection committed to this database; refresh the
	// per-connection data_version so PRAGMA data_version observes it
	// (own commits do not change data_version).
	if hdr := ctx.Pager.Header(); hdr != nil {
		if dh, err := storage.ParseHeader(hdr); err == nil {
			e.settings.dataVersion = int64(dh.FileChangeCount) + 1
		}
	}
	return true
}

// findTable searches for a table across all attached databases.
// If the name has a schema prefix (e.g. "aux.t3"), it searches only that database.
// If no schema prefix, it searches main first, then attached databases.
func (e *Engine) findTable(name string) (*schema.Entry, *DatabaseContext, error) {

	// An attached database's file may have been modified by an external
	// connection; the schema manager's checkExternalMod drops the pager cache
	// and any tableCache entries become stale. Detect the change up front so
	// the cache check below does not return a stale entry.
	e.detectExternalSchemaChanges()

	// During trigger-body DML the current DML context scopes unqualified
	// names to the trigger's own schema: a DELETE FROM t9 inside a main
	// trigger must resolve t9 in main only (SQLite fixes trigger bodies to
	// their schema at CREATE time), so a same-named table in an attached
	// database does NOT satisfy it. TEMP triggers are exempt: their bodies
	// may reference tables in any database (altercol-18.0: a TEMP trigger
	// body INSERT INTO log resolves aux.log). This must run BEFORE the table
	// cache (a cached aux.t9 must not satisfy a main-scoped lookup) and
	// before any temp/main/attached fallback.
	if entry, ctx, handled := e.findTableTriggerScoped(name); handled {
		if entry == nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
		return entry, ctx, nil
	}

	// Check table cache first
	if cached, ctx, ok := e.findTableCached(name); ok {
		return cached, ctx, nil
	}

	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		return e.findTableQualified(name, schemaName, objName)
	}

	// A schema pin (view being expanded in its own schema) restricts
	// unqualified name resolution to that schema, matching SQLite's
	// sqlite3FixSrcList: the body of a non-temp view cannot see temp/other
	// schema objects of the same name.
	if e.selectEngine.SchemaPin() != nil {
		entry, err := e.selectEngine.SchemaPin().Schema.FindTable(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
		e.cacheTableEntry(name, entry, e.selectEngine.SchemaPin())
		return entry, e.selectEngine.SchemaPin(), nil
	}

	// No schema prefix: search temp first (temp shadows main), then main,
	// then attached databases. A temp VIEW with this name shadows a main
	// TABLE: return an error so the caller falls through to view resolution
	// (SQLite resolves the temp view first and reports circularity when the
	// view's body re-enters its own name). Schema tables (sqlite_master/etc)
	// and sqlite_sequence always resolve to their native (main) schema,
	// never to the temp schema's synthetic fallback.
	if entry, ctx, found, err := e.findTableTemp(name); found || err != nil {
		return entry, ctx, err
	}

	// No schema prefix: search main first, then attached databases
	entry, err := e.mainDB.Schema.FindTable(name)
	if err == nil {
		e.cacheTableEntry(name, entry, e.mainDB)
		return entry, e.mainDB, nil
	}
	// A corrupt database (freelist/root page beyond the file) must report
	// "database disk image is malformed", not "no such table" (altercorrupt
	// loads images whose header is broken; the ALTER TABLE must fail with the
	// corruption error, matching SQLite).
	if isCorruptErr(err) {
		return nil, nil, err
	}
	if entry, ctx, ok := e.findTableInList(name); ok {
		return entry, ctx, nil
	}
	return nil, nil, fmt.Errorf("no such table: %s", name)
}

// findTableTriggerScoped resolves an unqualified table name from inside a
// trigger body. Non-TEMP triggers fix their bodies to their own schema, so
// the name resolves there exclusively; handled is true when a trigger
// context decided the lookup (found or not-found error carried in entry/ctx).
// Callers must check handled before the table cache.
func (e *Engine) findTableTriggerScoped(name string) (entry *schema.Entry, ctx *DatabaseContext, handled bool) {
	trigCtx := e.dml.CurrentTriggerCtx()
	if trigCtx == nil {
		return nil, nil, false
	}
	if trigCtx == e.getDB("temp") || trigCtx == e.getDB("TEMPORARY") {
		return nil, nil, false
	}
	if found, err := trigCtx.Schema.FindTable(name); err == nil {
		e.cacheTableEntry(name, found, trigCtx)
		return found, trigCtx, true
	}
	return nil, nil, true
}

// findTableCached returns a cached table entry when one exists for this
// name and is valid under the current schema pin (a view expansion must not
// reuse another schema's cached entry: attach-4.13).
func (e *Engine) findTableCached(name string) (*schema.Entry, *DatabaseContext, bool) {
	cached, ok := e.caches.tableCache[name]
	if !ok {
		return nil, nil, false
	}
	if e.selectEngine.SchemaPin() != nil && cached.ctx != e.selectEngine.SchemaPin() {
		return nil, nil, false
	}
	// Re-hydrate FTS state for cached entries too: a fresh engine has an
	// empty ftsTables map until the first lookup, and tableCache may be
	// consulted before ensureFTSForTable has run (e.g. after a schema
	// invalidation that cleared only the cache used by UPDATE).
	e.ensureFTSForTable(cached.entry)
	return cached.entry, cached.ctx, true
}

// isCorruptErr reports whether err is a database-corruption error that must
// be surfaced as-is rather than mapped to a table-not-found error.
func isCorruptErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database disk image is malformed")
}

// cacheTableEntry records a found table in the table cache and re-hydrates its
// FTS state.
func (e *Engine) cacheTableEntry(name string, entry *schema.Entry, ctx *DatabaseContext) {
	e.ensureFTSForTable(entry)
	e.caches.tableCache[name] = &cachedTableEntry{entry: entry, ctx: ctx}
}

// findTableQualified resolves a schema-qualified table name ("schema.table"),
// retrying once after an external schema invalidation for attached databases.
func (e *Engine) findTableQualified(name, schemaName, objName string) (*schema.Entry, *DatabaseContext, error) {
	ctx := e.getDB(schemaName)
	if ctx == nil {
		return nil, nil, fmt.Errorf("no such table: %s", name)
	}
	entry, err := ctx.Schema.FindTable(objName)
	if err != nil && !strings.EqualFold(schemaName, "main") && !strings.EqualFold(schemaName, "temp") && !strings.EqualFold(schemaName, "temporary") {
		// The attached database's file may have been modified by an
		// external connection since we attached (schema reload test):
		// drop the pager cache and schema and retry once. In-memory
		// pagers have no file to re-read, so invalidating their cache
		// would lose every page (including the schema root).
		if ctx.Pager != nil && !ctx.IsMemory {
			ctx.Pager.InvalidateCache()
		}
		ctx.Schema.InvalidateCache()
		entry, err = ctx.Schema.FindTable(objName)
	}
	if err != nil {
		// SQLite reports the schema-qualified name when a qualified
		// reference fails ("no such table: main.txx"), not just the
		// bare object name.
		return nil, nil, fmt.Errorf("no such table: %s.%s", schemaName, objName)
	}
	e.cacheTableEntry(name, entry, ctx)
	return entry, ctx, nil
}

// findTableTemp resolves an unqualified table name in the temp schema (temp
// shadows main). When a same-named temp VIEW exists it returns an error so the
// caller falls through to view resolution.
func (e *Engine) findTableTemp(name string) (*schema.Entry, *DatabaseContext, bool, error) {
	tc := e.getDB("temp")
	if tc == nil || tc == e.mainDB || isSchemaTable(name) || isSQLiteSequence(name) {
		return nil, nil, false, nil
	}
	if entry, err := tc.Schema.FindTable(name); err == nil {
		e.cacheTableEntry(name, entry, tc)
		return entry, tc, true, nil
	}
	if _, vErr := tc.Schema.FindView(name); vErr == nil {
		return nil, nil, false, fmt.Errorf("no such table: %s", name)
	}
	return nil, nil, false, nil
}

// findTableInList searches attached databases (excluding main) for a table in
// ATTACH order (deterministic; SQLite resolves unqualified names to the first
// database that has the table).
func (e *Engine) findTableInList(name string) (*schema.Entry, *DatabaseContext, bool) {
	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindTable(name)
		if err == nil {
			e.cacheTableEntry(name, entry, ctx)
			return entry, ctx, true
		}
	}
	return nil, nil, false
}

// findView searches for a view across all attached databases.
func (e *Engine) findView(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		return e.findViewQualified(name, schemaName, objName)
	}

	// A schema pin (view being expanded in its own schema) restricts
	// unqualified view resolution to that schema (SQLite sqlite3FixSrcList).
	if e.selectEngine.SchemaPin() != nil {
		entry, err := e.selectEngine.SchemaPin().Schema.FindView(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such view: %s", name)
		}
		return entry, e.selectEngine.SchemaPin(), nil
	}

	// Search the temp schema first (temp shadows main for unqualified names).
	if tc := e.getDB("temp"); tc != nil && tc != e.mainDB {
		if entry, err := tc.Schema.FindView(name); err == nil {
			return entry, tc, nil
		}
	}

	entry, err := e.mainDB.Schema.FindView(name)
	if err == nil {
		return entry, e.mainDB, nil
	}
	if entry, ctx, ok := e.findViewInList(name); ok {
		return entry, ctx, nil
	}
	return nil, nil, fmt.Errorf("no such view: %s", name)
}

// findViewQualified resolves a schema-qualified view name ("schema.view").
func (e *Engine) findViewQualified(name, schemaName, objName string) (*schema.Entry, *DatabaseContext, error) {
	ctx := e.getDB(schemaName)
	if ctx == nil {
		return nil, nil, fmt.Errorf("no such view: %s", name)
	}
	entry, err := ctx.Schema.FindView(objName)
	if err != nil {
		return nil, nil, err
	}
	return entry, ctx, nil
}

// findViewInList searches attached databases (excluding main) for a view.
func (e *Engine) findViewInList(name string) (*schema.Entry, *DatabaseContext, bool) {
	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindView(name)
		if err == nil {
			return entry, ctx, true
		}
	}
	return nil, nil, false
}

// validateNoRaiseOutsideTrigger walks a statement's expression trees and
// rejects RAISE() expressions when not inside a trigger program (SQLite's
// "RAISE() may only be used within a trigger-program"). This is a compile-
// time check: the runtime evaluation in evalRaiseExpr would miss RAISE()
// inside expressions that never execute (e.g. GROUP BY/HAVING over an empty
// table).
// errRaiseOutsideTrigger is reported when RAISE() appears outside a trigger
// program (SQLite: "RAISE() may only be used within a trigger-program").
var errRaiseOutsideTrigger = fmt.Errorf("RAISE() may only be used within a trigger-program")

// isRaiseExpr reports whether expr is a RAISE() expression, whether parsed as
// a RaiseExpr node or as a raise() function call.
func isRaiseExpr(expr sql.Expr) bool {
	if _, ok := expr.(*sql.RaiseExpr); ok {
		return true
	}
	if f, ok := expr.(*sql.FuncCall); ok && strings.EqualFold(f.Name, "raise") {
		return true
	}
	return false
}

// checkRaiseInExpr walks an expression tree and rejects RAISE() expressions.
// Subquery/EXISTS selects are delegated to checkSelect so the walk covers
// GROUP BY/HAVING of nested selects.
func checkRaiseInExpr(expr sql.Expr, checkSelect func(*sql.SelectStmt) error) error {
	if expr == nil {
		return nil
	}
	if isRaiseExpr(expr) {
		return errRaiseOutsideTrigger
	}
	switch v := expr.(type) {
	case *sql.Subquery:
		return checkSelect(v.Select)
	case *sql.ExistsExpr:
		return checkSelect(v.Select)
	}
	for _, kid := range raiseChildExprs(expr) {
		if err := checkRaiseInExpr(kid, checkSelect); err != nil {
			return err
		}
	}
	return nil
}

// raiseChildExprs returns the immediate child expressions of an expression
// node (empty for leaves), mirroring the expression kinds whose subtrees can
// contain a RAISE() outside a trigger.
func raiseChildExprs(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return []sql.Expr{v.Expr}
	case *sql.BinaryOp:
		return []sql.Expr{v.Left, v.Right}
	case *sql.UnaryOp:
		return []sql.Expr{v.Operand}
	case *sql.FuncCall:
		return v.Args
	case *sql.CastExpr:
		return []sql.Expr{v.Operand}
	case *sql.CaseExpr:
		return caseChildExprs(v)
	case *sql.Between:
		return []sql.Expr{v.Operand, v.Low, v.High}
	case *sql.InList:
		kids := []sql.Expr{v.Operand}
		kids = append(kids, v.List...)
		return kids
	case *sql.RowValue:
		return v.Values
	case *sql.IsNull:
		return []sql.Expr{v.Operand}
	case *sql.IsNotNull:
		return []sql.Expr{v.Operand}
	}
	return nil
}

// caseChildExprs returns the operand, WHEN/THEN pairs, and ELSE of a CASE
// expression in traversal order.
func caseChildExprs(v *sql.CaseExpr) []sql.Expr {
	kids := []sql.Expr{v.Operand, v.Else}
	for _, w := range v.Whens {
		kids = append(kids, w.When, w.Then)
	}
	return kids
}

// validateNoRaiseOutsideTrigger walks a statement's expression trees and
// rejects RAISE() expressions when not inside a trigger program (SQLite's
// "RAISE() may only be used within a trigger-program"). This is a compile-
// time check: the runtime evaluation in evalRaiseExpr would miss RAISE()
// inside expressions that never execute (e.g. GROUP BY/HAVING over an empty
// table).
// countStatementFromTerms counts the total number of FROM-clause terms (base
// FROM plus JOIN operands) across a statement's SELECT trees, including nested
// subqueries, CTE bodies, and DML select sources — matching SQLite's SrcList
// growth (SQLITE_MAX_SRCLIST = 200, with1 22.1).
func (e *Engine) validateNoRaiseOutsideTrigger(stmt sql.Stmt) error {
	checkSelect := func(s *sql.SelectStmt) error { return e.checkSelectRaise(s) }
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.checkSelectRaise(s)
	case *sql.InsertStmt:
		for _, tuple := range s.Values {
			if err := checkRaiseInExprs(tuple, checkSelect); err != nil {
				return err
			}
		}
		if s.Select != nil {
			return e.checkSelectRaise(s.Select)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			if err := checkRaiseInExpr(a.Value, checkSelect); err != nil {
				return err
			}
		}
		return checkRaiseInExpr(s.Where, checkSelect)
	case *sql.DeleteStmt:
		return checkRaiseInExpr(s.Where, checkSelect)
	}
	return nil
}

// checkRaiseInExprs walks a list of expressions, rejecting RAISE() anywhere.
func checkRaiseInExprs(exprs []sql.Expr, checkSelect func(*sql.SelectStmt) error) error {
	for _, expr := range exprs {
		if err := checkRaiseInExpr(expr, checkSelect); err != nil {
			return err
		}
	}
	return nil
}

// checkSelectRaise walks a SELECT's GROUP BY, HAVING, and compound tails for
// RAISE() expressions. Only GROUP BY and HAVING need the early check: SQLite
// validates ORDER BY column matching first ("1st ORDER BY term does not match
// any column", triggerC-16.1), and SELECT-column/WHERE RAISE() is caught by
// runtime evaluation when rows exist. GROUP BY/HAVING over an empty table
// never evaluates, hiding the error (triggerC-16.2).
func (e *Engine) checkSelectRaise(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	checkSelect := func(sel *sql.SelectStmt) error { return e.checkSelectRaise(sel) }
	for _, g := range s.GroupBy {
		if err := checkRaiseInExpr(g, checkSelect); err != nil {
			return err
		}
	}
	if err := checkRaiseInExpr(s.Having, checkSelect); err != nil {
		return err
	}
	if s.Union != nil {
		return e.checkSelectRaise(s.Union)
	}
	return nil
}

// execDepthLeave unwinds one Exec nesting level. When the outermost
// statement finishes it clears snapActive and — mirroring vdbeapi.c:779-782
// — clears the interrupt flag: sqlite3 clears u1.isInterrupted when the
// last active statement returns, so an interrupted statement does not
// poison the following one (interrupt-2.5.3/2.7 observe 0 right after).
// An interrupt raised while no statement is active (between statements)
// survives until the next Exec consumes it at entry.
func (e *Engine) execDepthLeave() {
	e.tx.execDepth--
	if e.tx.execDepth == 0 {
		e.tx.snapActive = false
		e.interrupted = false
	}
}

// failInterruptedStmt returns the SQLITE_INTERRUPT statement result, forcing
// a full transaction rollback when a non-read-only statement was interrupted
// inside an explicit transaction (src/vdbeaux.c:3358-3383: SQLITE_INTERRUPT
// is a "special" error → sqlite3RollbackAll). interrupt-3.x: the following
// bare ROLLBACK must fail with "cannot rollback - no transaction is active".
func (e *Engine) failInterruptedStmt(stmt sql.Stmt) *Result {
	if e.isDMLStmt(stmt) && e.tx.inTransaction {
		e.execRollback()
	}
	return &Result{Error: fmt.Errorf("interrupted")}
}

// Exec executes a single SQL statement and returns the result.
func (e *Engine) Exec(stmt sql.Stmt) *Result {
	if res := e.execEntry(stmt); res != nil {
		return res
	}
	defer e.execDepthLeave()

	// Reset the test-only counter() function state at the start of each
	// statement. SQLite's column-pruning optimization skips evaluating
	// counter() in unused columns; since our engine lacks that optimization,
	// resetting per-statement keeps the results consistent (counter() values
	// within a single statement start from 1).
	e.testState.counterVal = 0
	e.testState.nondeterVal = 0
	// Operator-overload probing is statement-scoped: materialization of a
	// opted-in vtab during THIS statement re-arms it.
	e.overloadProbe = false

	// Pin 'now' for the whole statement (SQLite sqlite3StmtCurrentTime): all
	// date/time functions using 'now' within this statement return the same
	// instant, even when a user function sleeps in between. Use the
	// hookable clock so the test harness's sqlite_current_time override
	// (function.SetNowFunc) takes effect.
	function.SetStmtTime(function.Now())
	defer function.SetStmtTime(time.Time{})

	// SQLite guarantees statement atomicity: when a statement fails (a
	// constraint violation, a trigger error, etc.) every change it made is
	// rolled back. We emulate that by snapshotting all pagers before DML and
	// restoring them on error. Nested Exec calls (trigger bodies) snapshot
	// again, so a failure inside a trigger rolls back the inner statement and
	// then propagates to the outer statement's restore.
	isDML := e.isDMLStmt(stmt)
	snaps := e.execSnapshotDML(stmt, isDML)

	// Push DML WITH (CTE) definitions before preflight validation so
	// subqueries inside SET/WHERE can resolve the CTE by name. The CTE
	// scope covers the whole single statement including its preflight
	// checks (validateUpdateSubqueries checks subquery FROM tables via
	// findCTE). The push must precede execPreflight; it is popped after
	// execDispatch so the dispatch-time withDMLCTEs does not double-push.
	dmlCTEs := dmlCTEsForPreflight(stmt)
	if len(dmlCTEs) > 0 {
		if dup := duplicateCTENameExec(dmlCTEs); dup != "" {
			return &Result{Error: fmt.Errorf("duplicate WITH table name: %s", dup)}
		}
		e.selectEngine.PushCTEScope(dmlCTEs)
		defer e.selectEngine.PopCTEScope()
	}

	if res := e.execPreflight(stmt); res != nil {
		return res
	}
	res := e.execDispatch(stmt)
	// A nested eval()/trigger ran ROLLBACK mid-statement: the enclosing
	// statement fails with "abort due to ROLLBACK" (SQLite SQLITE_ABORT_ROLLBACK).
	// This only applies at the outermost Exec level; nested Exec calls inside
	// the statement must not consume the flag. When the enclosing statement
	// already failed with its own error (e.g. an OR ROLLBACK constraint
	// violation whose shadow-table write propagated the rollback), SQLite
	// reports the original error, not the abort (spellfix.test 7.4.2/7.5.2:
	// "constraint failed" with autocommit restored).
	if e.tx.execDepth == 1 && e.tx.rollbackAborted && !isRollbackStmt(stmt) {
		e.tx.rollbackAborted = false
		if res.Error == nil {
			res = &Result{Error: fmt.Errorf("abort due to ROLLBACK")}
		}
	}
	res = e.execPostFK(stmt, res, isDML)
	res = e.execRollbackOnError(stmt, res, snaps, isDML)
	e.execTrackChanges(res, isDML)
	if res := e.execFlushAutocommit(stmt, res, isDML); res != nil {
		// A commit-hook abort (sqlite3_commit_hook returning nonzero) fails
		// the statement and rolls back its changes (SQLite rolls the implicit
		// transaction back instead of committing it).
		if isDML && len(snaps) > 0 {
			e.restoreAllPagers(snaps)
			e.restoreAllFTS()
		}
		return res
	}
	return e.execAfterWrite(stmt, res, isDML)
}

// execEntry performs Exec's statement-entry gates: the prepared-read write
// block, sqlite3_interrupt() consumption, the execDepth push with the
// outermost-statement external-file validation, and the SQLITE_TEST
// interrupt-countdown check. A non-nil result means the statement failed
// before execution (execDepth is balanced; callers must NOT execDepthLeave).
func (e *Engine) execEntry(stmt sql.Stmt) *Result {
	if e.WriteBlockedByPreparedRead(stmt) {
		return &Result{Error: fmt.Errorf("database is locked")}
	}
	// Cross-connection pager lock matrix (src/pager.c + os_unix.c): another
	// connection's RESERVED lock blocks our writes; its EXCLUSIVE lock blocks
	// our reads and writes (lock3-3.2/4.1/4.2).
	if err := e.CrossConnLockError(stmt); err != nil {
		return &Result{Error: err}
	}
	// sqlite3_interrupt(): when the interrupt flag is set, the next statement
	// on this connection fails with "interrupted" and the flag is consumed
	// (SQLite clears it when the interrupted step returns).
	if e.interrupted {
		e.interrupted = false
		// SQLITE_INTERRUPT on a non-read-only statement forces a full
		// transaction rollback (src/vdbeaux.c:3358-3383): an interrupted write
		// inside an explicit transaction rolls the whole transaction back, so
		// a later bare ROLLBACK fails with "cannot rollback - no transaction is
		// active" (interrupt-3.x). Done before execDepth++ so nested statements
		// (trigger bodies) never consume an outer flag.
		return e.failInterruptedStmt(stmt)
	}

	e.tx.execDepth++

	// Start of an outermost statement: allow the schema managers' external-mod
	// check to re-read the file change counter once (a connection must observe
	// commits made by other connections between statements). Nested Exec calls
	// (triggers, the FTS flush's shadow-table writes) leave the flag set so
	// their repeated FindTable/GetEntries calls skip the FileChangeCounter
	// Pread — the dominant cost of per-row FTS builds.
	if e.tx.execDepth == 1 {
		e.execResetExternalChecks()
		// Every outermost statement re-validates the database files the way
		// sqlite3PagerSharedLock + lockBtree do at each transaction start: an
		// externally patched header or a truncated file is corruption
		// (incrcorrupt 1.x/2.x hexio_write and chan truncate under an open
		// connection).
		if res := e.execDBFileChecks(stmt); res != nil {
			e.execDepthLeave()
			return res
		}
	}

	// SQLITE_TEST interrupt countdown / progress callback: sqlite3VdbeExec
	// decrements sqlite3_interrupt_count before every opcode (src/vdbe.c loop
	// head), so every statement — even trivial DDL like DROP TABLE or VACUUM —
	// consumes at least one op; interrupt-1.2's loop relies on each attempt
	// failing with SQLITE_INTERRUPT until the countdown outlasts the work.
	if err := e.checkProgress(); err != nil {
		res := e.failInterruptedStmt(stmt)
		e.execDepthLeave()
		return res
	}
	return nil
}

// execSnapshotDML decides the statement-atomicity snapshot for a DML
// statement (nil when none is needed): a single-row VALUES INSERT cannot
// fail after writing, and nested writes (trigger bodies, the FTS flush's
// shadow writes) are covered by the outermost statement's snapshot, so both
// skip the O(pages) copy. The CTE scope push stays in Exec (its defer must
// outlive dispatch).
func (e *Engine) execSnapshotDML(stmt sql.Stmt, isDML bool) []pagerSnap {
	e.ftsSnapshots = nil
	if !isDML || e.dmlCanSkipSnapshot(stmt) {
		return nil
	}
	if e.tx.execDepth > 1 && (e.tx.snapActive || e.tx.inFTSFlush) {
		return nil
	}
	snaps := e.snapshotAllPagers()
	// An inner Exec that writes only FTS SHADOW tables (%_segdir, %_segments,
	// %_stat) does not modify the in-memory FTS index, so the O(index)
	// InvertedIndex snapshot is unnecessary — skipping it removes the O(n^2)
	// term from per-row FTS builds (fts3_build_db_2 30040: the %_stat REPLACE
	// in every flush snapshots the whole index). The pager snapshot still
	// covers the shadow btree writes.
	if !e.stmtTargetsFTSContent(stmt) {
		e.ftsSnapshots = nil
	}
	if e.tx.execDepth == 1 {
		e.tx.snapActive = true
	}
	return snaps
}

// execAfterWrite performs Exec's post-commit work for a successful DML
// statement: reload the in-memory FTS index when the statement wrote an FTS
// table's SHADOW tables directly (outside the FTS flush) — SQLite always
// reads the index from the segments, so a hand-edited segment root must be
// reflected on the next MATCH/SELECT (fts4record 1.x). Corruption errors are
// normalized on the way out.
func (e *Engine) execAfterWrite(stmt sql.Stmt, res *Result, isDML bool) *Result {
	// execDepth is still 1 here for an outermost statement (the deferred
	// execDepthLeave runs after Exec returns) — equivalent to the original
	// post-leave depth==0 check.
	if isDML && res != nil && res.Error == nil && !e.tx.inFTSFlush && e.tx.execDepth == 1 {
		if owner := e.stmtFTSShadowOwner(stmt); owner != "" {
			e.ReloadFTSIndex(owner)
		}
	}
	return e.normalizeCorruptionError(res)
}

// normalizeCorruptionError maps low-level btree/storage corruption errors to
// SQLite's "database disk image is malformed" message. The btree surfaces
// structural damage (an unknown page type, an out-of-range cell offset) as
// internal errors; SQLite reports all of them as SQLITE_CORRUPT (fts3corrupt4
// 27.2: a crash-written page with type 0x00 during a huge recursive INSERT).
