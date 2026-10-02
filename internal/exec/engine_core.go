package exec

import (
	"fmt"
	"hash/maphash"
	"strconv"
	"strings"
	"time"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/quota"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"

	"github.com/pijalu/frigolite/internal/util"
)

// cloneStmtsWithValues clones the cached statement list and substitutes new
// literal values, copy-on-write (see template_clone.go). It returns ok=false
// when the template cannot serve the values (unknown statement or expression
// kind, value/count mismatch, non-canonical literal text); the caller then
// falls back to a full parse, which keeps results identical.
func cloneStmtsWithValues(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, bool) {
	return cloneStmtsValues(stmts, values)
}

// insertStmtValues clones an InsertStmt for the shared walker state: template
// mode consumes normalized literal values through c.idx; bind mode substitutes
// sql.ParameterExpr markers (statement literals are kept verbatim). Every
// statement field is carried over so a substituted AST stays identical to a
// fresh parse (the historical clone dropped Alias/CTEs/OrFail).
//
// The clone is built on the walker's depth-indexed scratch when one is
// attached (Engine.cloneStmtsWithValues): every field below is assigned, so a
// recycled tenant is indistinguishable from a fresh struct (clone_scratch.go).
//
// The field walk order MUST match the statement's source order (WITH first,
// then the VALUES tuples or the SELECT body): the normalizer extracts literal
// values left-to-right over the raw text, so the WITH clause — which
// syntactically precedes the INSERT keyword — contributes its literals BEFORE
// the VALUES/SELECT body's. A walk visiting the select list or the tuples
// first would cross-assign the values (the COUNT still lines up, so nothing
// declines): "WITH s(i) AS (SELECT 501 UNION ALL SELECT i+1 FROM s WHERE i <
// 5000) INSERT INTO t SELECT i, 'row' || i FROM s" would put 501 into the
// 'row' literal, leave the anchor at the template's stale value, put 5000
// into the i+1 increment, and put the string "row" into the guard's numeric
// slot — an always-true integer<text comparison that ran a recursive CTE to
// its 1M-row limit and duplicated 1M garbage rows into the table. The same
// contract already governs the SELECT/UPDATE/DELETE walkers (see
// selectStmt's walk-order note in template_clone_stmt.go).
func (c *exprClone) insertStmtValues(s *sql.InsertStmt) (*sql.InsertStmt, error) {
	// Clone WITH-clause bodies first (source order: the WITH clause precedes
	// everything the INSERT clause contributes).
	ctes, cteChanged, ok := c.ctes(s.CTEs)
	if !ok {
		return nil, fmt.Errorf("template clone: WITH clause refused")
	}
	var clone *sql.InsertStmt
	if c.scratch != nil {
		clone = c.scratch.takeInsertStmt()
	}
	if clone == nil {
		clone = new(sql.InsertStmt)
	}
	clone.Table = s.Table
	clone.Alias = s.Alias
	clone.Columns = s.Columns
	if cap(clone.Values) >= len(s.Values) {
		clone.Values = clone.Values[:len(s.Values)]
	} else {
		clone.Values = make([][]sql.Expr, len(s.Values))
	}
	clone.OnConflict = s.OnConflict
	clone.Returning = s.Returning
	clone.HasReturning = s.HasReturning
	clone.IsReplace = s.IsReplace
	clone.OrIgnore = s.OrIgnore
	clone.OrFail = s.OrFail
	clone.OrConflict = s.OrConflict
	clone.RawSQL = s.RawSQL
	if cteChanged {
		clone.CTEs = ctes
	} else {
		clone.CTEs = s.CTEs
	}
	clone.Select = nil
	// Clone values tuples (a recycled tenant's backing arrays are reused in
	// place when the shape fits; every element is rewritten below).
	for vi, tuple := range s.Values {
		tup := clone.Values[vi][:0]
		if cap(tup) < len(tuple) {
			tup = make([]sql.Expr, len(tuple))
		} else {
			tup = tup[:len(tuple)]
		}
		clone.Values[vi] = tup
		for vj, expr := range tuple {
			cloned, err := c.insertValue(expr)
			if err != nil {
				return nil, err
			}
			clone.Values[vi][vj] = cloned
		}
	}
	// Clone Select for INSERT ... SELECT
	if s.Select != nil {
		sel, _, ok := c.selectStmt(s.Select)
		if !ok {
			return nil, fmt.Errorf("template clone: INSERT-SELECT refused")
		}
		clone.Select = sel
	}
	// Clone ON CONFLICT (upsert) expressions
	conflict, conflictChanged, ok := c.conflictClause(s.OnConflict)
	if !ok {
		return nil, fmt.Errorf("template clone: ON CONFLICT refused")
	}
	if conflictChanged {
		clone.OnConflict = conflict
	}
	if c.scratch != nil {
		c.scratch.retireInsert(clone)
	}
	return clone, nil
}

// conflictClause substitutes an ON CONFLICT (upsert) clause chain, returning
// the (possibly shared) clause and whether anything changed under it.
func (c *exprClone) conflictClause(oc *sql.OnConflictClause) (*sql.OnConflictClause, bool, bool) {
	if oc == nil {
		return nil, false, true
	}
	targetWhere, twChanged, ok := c.exprField(oc.TargetWhere)
	if !ok {
		return nil, false, false
	}
	assignments, aChanged, ok := c.assignments(oc.Assignments)
	if !ok {
		return nil, false, false
	}
	where, wChanged, ok := c.exprField(oc.Where)
	if !ok {
		return nil, false, false
	}
	next, nextChanged, ok := c.conflictClause(oc.Next)
	if !ok {
		return nil, false, false
	}
	if !twChanged && !aChanged && !wChanged && !nextChanged {
		return oc, false, true
	}
	return &sql.OnConflictClause{
		ConflictColumn: oc.ConflictColumn,
		TargetExpr:     oc.TargetExpr,
		TargetWhere:    targetWhere,
		Action:         oc.Action,
		Assignments:    assignments,
		Where:          where,
		Next:           next,
	}, true, true
}

// insertValue substitutes a cached literal value for a NumericLit/StringLit
// expression (template mode), or a bound value for a sql.ParameterExpr marker
// (bind mode), advancing walker state as values are consumed. Other
// expressions are returned unchanged. The substitute literal is rebuilt FROM
// THE VALUE'S KIND so the statement's stored type is the one the user wrote:
// the historical FormatFloat-only rendering coerced an integral REAL literal
// to INTEGER through the template cache (INSERT ... VALUES(8.0) repeated
// persisted typeof=integer; the first, uncached execution stored real —
// oracle: real|8.0).
func (c *exprClone) insertValue(expr sql.Expr) (sql.Expr, error) {
	if c.bind != nil {
		switch e := expr.(type) {
		case *sql.ParameterExpr:
			cloned, ok := c.bindParam(e)
			if !ok {
				return nil, fmt.Errorf("bind: parameter substitution refused")
			}
			return cloned, nil
		default:
			// Statement literal or expression — immutable text, keep original
			return expr, nil
		}
	}
	switch expr.(type) {
	case *sql.NumericLit, *sql.StringLit:
	default:
		// Non-value expression — keep original
		return expr, nil
	}
	if c.idx >= len(c.values) {
		return nil, fmt.Errorf("template cache: not enough values (need %d, have %d)", len(c.values), c.idx+1)
	}
	val := c.values[c.idx]
	c.idx++
	switch v := val.(type) {
	case int64:
		return &sql.NumericLit{Value: strconv.FormatInt(v, 10)}, nil
	case float64:
		s := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0" // 'g' drops the decimal point: keep the REAL kind
		}
		return &sql.NumericLit{Value: s}, nil
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
	// Tokenize-time SQL length limit (tokenize.c sqlite3RunParser: mxSqlLen
	// counts the SQL text against db->aLimit[SQLITE_LIMIT_SQL_LENGTH];
	// exhaustion sets pParse->rc = SQLITE_TOOBIG with the default message,
	// before any statement runs — sqllimits1-6.1).
	if e.settings.sqlLengthLimit != 0 && len(sqlStr) > e.settings.sqlLengthLimit {
		return nil, fmt.Errorf("string or blob too big")
	}
	// Check exact match cache first (fastest)
	if cached, ok := e.caches.stmtCache[sqlStr]; ok {
		return cached, nil
	}
	if len(e.caches.stmtCache) >= maxStmtCacheSize {
		e.caches.stmtCache = make(map[string][]sql.Stmt)
	}

	// Check template cache — normalize SQL and see if we've seen this structure.
	// The substitution buffer and value slice are the engine's per-statement
	// scratch (recycled across statements; neither outlives the Prepare call).
	normSQL, values, normBuf := normalizeSQLScratch(sqlStr, e.normBuf, e.normValues)
	e.normBuf, e.normValues = normBuf, values
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
	e.storeTemplateCache(normSQL, values, stmts)
	return stmts, nil
}

// templateCacheHash keys the template cache: a seeded AES hash of the
// normalized text. The seed is per-process; lookups verify the candidate
// entry's stored text against the normalized bytes before use, so a (never
// observed) collision degrades to a full parse, never to a wrong template.
var templateCacheHash = maphash.MakeSeed()

// tryTemplateCache attempts to reuse a cached AST template for structurally
// identical SQL (same after replacing literal values). normSQL is the
// recycled normalization scratch (nil when the statement held no literals).
// It returns (nil, false) when there is no usable template, falling through
// to a full parse.
func (e *Engine) tryTemplateCache(sqlStr string, normSQL []byte, values []interface{}) ([]sql.Stmt, bool) {
	if len(normSQL) == 0 || len(values) == 0 {
		return nil, false
	}
	cached, ok := e.caches.templateCache[maphash.Bytes(templateCacheHash, normSQL)]
	if !ok || cached.template != string(normSQL) {
		return nil, false
	}
	// Template cache hit — clone AST with new values. If the clone refuses
	// (unknown shape or value mismatch), fall through to re-parse.
	// The clone is NOT stored in the exact-text stmtCache: a structurally
	// identical statement with different literals has a different exact text,
	// so the store only paid a map insert + entry churn per statement (the
	// cache filled to its cap and was wholesale-dropped under unique-text
	// streams) while every exact-text repeat still re-clones from the same
	// template below. Results are identical either way: a substituted AST is
	// byte-for-byte what a fresh parse of the statement text produces, and
	// statement execution treats AST nodes as immutable.
	cloned, ok := e.cloneStmtsValuesScratch(cached.ast, values)
	if !ok {
		return nil, false
	}
	return cloned, true
}

// storeTemplateCache records a parsed statement list as a template for
// structurally identical SQL, bounded by maxTemplateCacheSize. normSQL is
// the normalization scratch; a fresh template materializes its normalized
// text once (the per-statement string the lookup path never pays).
func (e *Engine) storeTemplateCache(normSQL []byte, values []interface{}, stmts []sql.Stmt) {
	if len(normSQL) == 0 || len(values) == 0 || len(e.caches.templateCache) >= maxTemplateCacheSize {
		return
	}
	if e.caches.templateCache == nil {
		e.caches.templateCache = make(map[uint64]*sqlTemplateEntry)
	}
	key := maphash.Bytes(templateCacheHash, normSQL)
	if existing, ok := e.caches.templateCache[key]; ok && existing.template == string(normSQL) {
		return
	}
	e.caches.templateCache[key] = &sqlTemplateEntry{
		template: string(normSQL),
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
	if strings.EqualFold(ctx.Name, "TEMP") || strings.EqualFold(ctx.Name, "TEMPORARY") {
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

// findTable searches for a table across all attached databases, enforcing
// the SQLITE_PREPARE_NO_VTAB mode (see noVtabNoSuchTable).
// If the name has a schema prefix (e.g. "aux.t3"), it searches only that database.
// If no schema prefix, it searches main first, then attached databases.
func (e *Engine) findTable(name string) (*schema.Entry, *DatabaseContext, error) {
	entry, ctx, err := e.findTableUncached(name)
	if err == nil && entry != nil && e.noVtabDepth > 0 && isVtabSchemaEntry(entry) {
		return nil, nil, e.noVtabNoSuchTable(name)
	}
	return entry, ctx, err
}

// isVtabSchemaEntry reports whether a schema entry creates a virtual table
// (build.c IsVirtual).
func isVtabSchemaEntry(entry *schema.Entry) bool {
	return entry != nil && util.HasPrefixFoldASCII(entry.SQL, "CREATE VIRTUAL TABLE")
}

// noVtabNoSuchTable renders the resolution failure for a virtual table hidden
// by the NO_VTAB statement mode (build.c:454): trigger bodies are schema-
// fixed (sqlite3FixSrcList), so an unqualified reference from inside a
// trigger body reports the trigger's database ("no such table: main.rt").
func (e *Engine) noVtabNoSuchTable(name string) error {
	if schemaName, objName := parseSchemaName(name); schemaName != "" {
		return fmt.Errorf("no such table: %s.%s", schemaName, objName)
	}
	if tc := e.dml.CurrentTriggerCtx(); tc != nil && tc != e.getDB("temp") && tc != e.getDB("TEMPORARY") {
		return fmt.Errorf("no such table: %s.%s", strings.ToLower(tc.Name), name)
	}
	return fmt.Errorf("no such table: %s", name)
}

// findTableUncached is findTable without the NO_VTAB gate.
func (e *Engine) findTableUncached(name string) (*schema.Entry, *DatabaseContext, error) {
	// An attached database's file may have been modified by an external
	// connection; the schema manager's checkExternalMod drops the pager cache
	// and any tableCache entries become stale. Detect the change up front so
	// the cache check below does not return a stale entry.
	e.detectExternalSchemaChanges()

	// Addressing the temp schema's system tables opens the lazily-created
	// temp btree (pragma.c PragTyp_DATABASE_LIST's aDb[i].pBt stays NULL
	// until something addresses temp): "SELECT * FROM sqlite_temp_master"
	// then makes PRAGMA database_list report the temp row (pragma-6.1).
	// The name check is allocation-free (it runs for every findTable, so
	// the ToUpper here was a per-statement heap hit on the point-op floor);
	// EqualFold over the exact lengths matches the old ToUpper comparison
	// for every ASCII spelling.
	if sch, obj := parseSchemaName(name); sch == "" || strings.EqualFold(sch, "temp") || strings.EqualFold(sch, "temporary") {
		if (len(obj) == 18 && strings.EqualFold(obj, "sqlite_temp_master")) ||
			(len(obj) == 17 && strings.EqualFold(obj, "sqlite_temp_schema")) {
			e.openTempBtree()
		}
	}

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
			return nil, nil, triggerScopedNoSuchTable(name, ctx)
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
	return e.findTableUnqualified(name)
}

// findTableUnqualified resolves an unqualified table name after the
// trigger-scoped and cached lookups missed: schema-pin restricted resolution,
// temp-first shadowing, then main and the attached databases.
func (e *Engine) findTableUnqualified(name string) (*schema.Entry, *DatabaseContext, error) {
	// A schema pin (view being expanded in its own schema) restricts
	// unqualified name resolution to that schema, matching SQLite's
	// sqlite3FixSrcList: the body of a non-temp view cannot see temp/other
	// schema objects of the same name. The fixer stores the owning schema on
	// the source item, so sqlite3LocateTable receives it as the database and
	// the not-found error carries the qualifier ("no such table: main.t9",
	// trigger4-3.3 — a view scan whose body table was dropped).
	if pin := e.selectEngine.SchemaPin(); pin != nil {
		entry, err := pin.Schema.FindTable(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such table: %s.%s", strings.ToLower(pin.Name), name)
		}
		e.cacheTableEntry(name, entry, pin)
		return entry, pin, nil
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
	// Return the trigger context so the caller can qualify the not-found
	// error with the trigger's own schema: trigger bodies are schema-fixed at
	// CREATE time (sqlite3FixSrcList), so the error reports that schema, not
	// the firing statement's database.
	return nil, trigCtx, true
}

// triggerScopedNoSuchTable renders the not-found error for a table lookup
// scoped to a trigger's own schema. build.c sqlite3LocateTable: the
// schema-fixed source item carries its database, so the error is qualified
// ("no such table: main.t9"); a qualified name reports the prefix as written.
func triggerScopedNoSuchTable(name string, trigCtx *DatabaseContext) error {
	if schemaName, objName := parseSchemaName(name); schemaName != "" {
		return fmt.Errorf("no such table: %s.%s", schemaName, objName)
	}
	return fmt.Errorf("no such table: %s.%s", strings.ToLower(trigCtx.Name), name)
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
	// The not-found error is qualified with the pinned schema, matching
	// sqlite3LocateTable's LOCATE_VIEW message.
	if pin := e.selectEngine.SchemaPin(); pin != nil {
		entry, err := pin.Schema.FindView(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such view: %s.%s", strings.ToLower(pin.Name), name)
		}
		return entry, pin, nil
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
// execDepthLeave unwinds one Exec nesting level. When the outermost
// statement finishes it clears snapActive, ends the statement's WAL read
// snapshot when no explicit transaction keeps it open (autocommit parity:
// the next statement begins a fresh read transaction and observes other
// connections' commits — P7.WAL-G7 slice 3), and — mirroring
// vdbeapi.c:779-782 — clears the interrupt flag: sqlite3 clears
// u1.isInterrupted when the last active statement returns, so an
// interrupted statement does not poison the following one
// (interrupt-2.5.3/2.7 observe 0 right after). An interrupt raised while no
// statement is active (between statements) survives until the next Exec
// consumes it at entry.
func (e *Engine) execDepthLeave() {
	e.tx.execDepth--
	if e.tx.execDepth == 0 {
		e.tx.snapActive = false
		e.interrupted = false
		e.walEndStmtRead()
	}
}

// failInterruptedStmt returns the SQLITE_INTERRUPT statement result, forcing
// a full transaction rollback when a non-read-only statement was interrupted
// inside an explicit transaction (src/vdbeaux.c:3358-3383: SQLITE_INTERRUPT
// is a "special" error → sqlite3RollbackAll). interrupt-3.x: the following
// bare ROLLBACK must fail with "cannot rollback - no transaction is active".
func (e *Engine) failInterruptedStmt(stmt sql.Stmt) *Result {
	// COMMIT/ROLLBACK statements are not read-only either (sqlite3VdbeReadOnly
	// scans the program for write opcodes), so an interrupt flagged at their
	// entry forces the same whole-transaction rollback: an interrupted COMMIT
	// never commits (vdbeaux.c:3358-3383 special errors → sqlite3RollbackAll).
	_, isCommit := stmt.(*sql.CommitStmt)
	_, isRollback := stmt.(*sql.RollbackStmt)
	if (e.isDMLStmt(stmt) || isCommit || isRollback) && e.tx.inTransaction {
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
	// Statement-scoped b-tree cursor lifecycle (btree.c closes a statement's
	// cursors when its program halts): track the wrappers created during this
	// statement and release them — unregistering their cursors from the
	// cross-statement invalidation registry — when it returns. Nested Exec
	// frames (trigger bodies, eval()) mark their own segment, so an inner
	// statement never releases the enclosing statement's positioned cursors
	// (misc8-1.6 contract).
	treeMark := len(e.stmtBtrees)
	e.stmtBtreeDepth++
	defer func() {
		e.stmtBtreeDepth--
		e.releaseStatementTrees(treeMark)
	}()

	// Reset the test-only counter() function state at the start of each
	// statement. SQLite's column-pruning optimization skips evaluating
	// counter() in unused columns; since our engine lacks that optimization,
	// resetting per-statement keeps the results consistent (counter() values
	// within a single statement start from 1).
	e.testState.counterVal = 0
	e.testState.nondeterVal = 0
	// Statement-scoped auxdata (stmtrand() sequence state) dies when the next
	// outermost statement starts, mirroring SQLite freeing auxdata at
	// sqlite3_reset. Nested statements (triggers) share the outer aux.
	e.resetOuterStatementScopes()
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
	// rolled back. We emulate SQLite's statement journal (pager.c
	// sub-journal): the statement's scope captures the before-image of each
	// page at its first modification, and a failure replays exactly those
	// images — a statement that modifies nothing rolls back nothing. Nested
	// Exec calls (trigger bodies) open scopes of their own, so a failure
	// inside a trigger rolls back the inner statement and then propagates to
	// the outer statement's restore.
	isDML := e.isDMLStmt(stmt)
	snaps := e.execSnapshotDML(stmt, isDML)
	defer e.endStatementScopes(snaps)
	// A nested-rollback flag from a PREVIOUS statement must not suppress this
	// statement's own failure-path restore.
	e.tx.nestedRollback = false

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
	if e.tx.inTransaction {
		// vdbe.c OP_Transaction: a statement inside an open transaction
		// holds the WRITER lock on every database it wrote for the LIFE of
		// the transaction — pager locks survive a savepoint ROLLBACK TO
		// (only COMMIT / full ROLLBACK releases them). Remember the dbs so
		// PRAGMA lock_status keeps reporting "reserved".
		e.noteReservedDbs()
	}
	if res := e.execFlushAutocommit(stmt, res, isDML); res != nil {
		// A commit-hook abort (sqlite3_commit_hook returning nonzero) fails
		// the statement and rolls back its changes (SQLite rolls the implicit
		// transaction back instead of committing it). The quota layer makes
		// any writing statement's flush fallible (SQLITE_FULL), including
		// DDL — restore whenever a snapshot exists.
		e.restoreStatementSnapsIfAny(snaps)
		return res
	}
	return e.execAfterWrite(stmt, res, isDML)
}

// resetOuterStatementScopes resets the outermost statement's scopes: the
// correlated-subquery row scope and the statement-scoped auxdata. Statement-
// boundary reset of the correlated-subquery row scope (see
// SelectEngine.ResetStatementCorrelatedScope): a stale outerRow from a prior
// statement must not reach this one's FROM-less / join-ON validation
// (insert2-4.1) — but the reset happens BEFORE any DML outer-row scope for
// THIS statement is installed (with1-4.3).
func (e *Engine) resetOuterStatementScopes() {
	if e.tx.execDepth != 1 {
		return
	}
	e.selectEngine.ResetStatementCorrelatedScope()
	e.expr.ResetStatementAux()
}

// restoreStatementSnapsIfAny restores the failed statement's pager and FTS
// snapshots when any exist.
func (e *Engine) restoreStatementSnapsIfAny(snaps []pagerSnap) {
	if len(snaps) > 0 {
		e.restoreAllPagers(snaps)
		e.restoreAllFTS()
	}
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
	// Read-only connection gate (SQLITE_OPEN_READONLY, pager.c
	// sqlite3PagerWrite → SQLITE_READONLY): every statement that writes —
	// DML, DDL, VACUUM/ANALYZE — fails with "attempt to write a readonly
	// database" on a connection opened read-only; reads are unaffected
	// (openv2-1.4/2.2, rdonly).
	if e.pager != nil && e.pager.ReadOnly() && e.stmtWritesDatabase(stmt) {
		return &Result{Error: fmt.Errorf("attempt to write a readonly database")}
	}
	// Cross-connection pager lock matrix (src/pager.c + os_unix.c): another
	// connection's RESERVED lock blocks our writes; its EXCLUSIVE lock blocks
	// our reads and writes (lock3-3.2/4.1/4.2).
	if err := e.CrossConnLockError(stmt); err != nil {
		return &Result{Error: err}
	}
	e.noteStmtReadLock(stmt)
	// WAL write gate (P7.WAL-G7 slice 2, sqlite3WalBeginWriteTransaction
	// parity): writing statements open the WAL write transaction BEFORE the
	// btree phase reads pages, so the WRITER shm lock freezes the snapshot
	// the statement's page images build on. A stale snapshot fails here with
	// "database is locked" (SQLITE_BUSY_SNAPSHOT, walprotocol2-2.2/2.3);
	// with a busy timeout the statement retries from a fresh read snapshot
	// (2.4/2.5) — the pager drops its page cache, which is safe exactly
	// because no btree cursor has opened yet.
	if err := e.walBeginStmtWrite(stmt); err != nil {
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

// execSnapshotDML opens the statement-atomicity journal for a DML statement
// (nil scopes when none is needed): every DML statement gets a pager.c-style
// statement journal (before-images captured lazily at first page write — a
// statement that modifies nothing rolls back nothing), and nested writes
// (trigger bodies, the FTS flush's shadow writes) are covered by the
// outermost statement's scope, so both skip opening their own. The
// single-row VALUES INSERT keeps its historical skip entirely (see
// dmlCanSkipSnapshot): a failed/interrupted row of that shape leaves its
// writes visible — the seam frigolite_fts5interrupt_test.go pins. The CTE
// scope push stays in Exec (its defer must outlive dispatch).
func (e *Engine) execSnapshotDML(stmt sql.Stmt, isDML bool) []pagerSnap {
	e.ftsSnapshots = nil
	// With the quota layer active, any writing statement's COMMIT can fail
	// with SQLITE_FULL (the pager's file-growth check), and SQLite rolls a
	// failed statement back (vdbeaux.c:3358-3383 treats SQLITE_FULL as a
	// transaction-abort error) — so DDL statements need the journal too.
	if !isDML && !quota.Active() {
		return nil
	}
	if isDML && e.dmlCanSkipSnapshot(stmt) {
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
	// in every flush snapshots the whole index). The pager journal still
	// covers the shadow btree writes.
	if !e.stmtTargetsFTSContent(stmt) {
		e.ftsSnapshots = nil
	}
	if e.tx.execDepth == 1 {
		e.tx.snapActive = true
	}
	return snaps
}
