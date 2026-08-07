// Package exec implements query execution.
package exec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Result holds the result of executing a SQL statement.
type Result struct {
	Columns         []string        // column names
	Rows            [][]interface{} // data rows
	Changes         int64           // number of changed rows
	Error           error           // execution error
	LastInsertRowID int64           // rowid of the last inserted row
	Row             []interface{}   // final row written by the statement (used by upsert RETURNING)
	// rowMaps carries the per-row column→value maps for statements that
	// materialize joined results (derived tables). It is internal: the public
	// API ignores it, but execJoins uses it to preserve qualified column keys
	// (t4.a) when a derived table is joined again.
	rowMaps []RowMap
}

// DatabaseContext holds all per-database state for a single database connection.
type DatabaseContext struct {
	Name     string          // schema name ("main", "temp", etc.)
	Pager    *pager.Pager    // file access for this database
	Schema   *schema.Manager // tables, indexes, views for this db
	FilePath string          // path to .db file
	IsMemory bool            // in-memory database
	IsTemp   bool            // temp database
}

// Engine executes SQL statements.
type Engine struct {

	// Authorization
	authorizer auth.Authorizer // authorization callback (nil = allow all)

	// Multi-database support
	databases map[string]*DatabaseContext // schema_name -> context (upper-cased key)
	dbList    []*DatabaseContext          // attached databases in ATTACH order (main first)
	mainDB    *DatabaseContext            // shortcut for "main"

	// Legacy direct fields pointing to mainDB (kept for backward compat with existing code)
	pager             *pager.Pager
	schema            *schema.Manager
	funcs             *function.Registry
	vtabs             *vtab.Registry
	lastRowID         int64
	lastChanges       int64                            // changes made by the last INSERT/UPDATE/DELETE
	colCache          map[string][]sql.ColumnDef       // cached column definitions (tableName -> colDefs)
	tcCache           map[string][]sql.TableConstraint // cached table-level constraints
	stmtCache         map[string][]sql.Stmt            // prepared statement cache (sqlText -> parsed stmts)
	tableRootPages    map[string]uint32                // tracked root pages (updated after splits)
	tableCache        map[string]*cachedTableEntry     // cached table entry lookups
	nextRowIDCache    map[uint32]int64                 // cached next rowid per root page (keyed by rootPage)
	autoIncSeq        map[uint32]int64                 // AUTOINCREMENT sequence: largest rowid ever used per root page
	templateCache     map[string]*sqlTemplateEntry     // normalized SQL → cached AST template
	triggerDepth      int                              // depth of trigger execution
	triggerDepthLimit int                              // SQLITE_LIMIT_TRIGGER_DEPTH (0 = maxTriggerDepth)
	triggerTables     []string                         // chain of tables currently in trigger programs
	triggerNewRow     Row                              // new row values for trigger execution (keyed as "new.colname")
	triggerOldRow     Row                              // old row values for trigger execution (keyed as "old.colname")
	updateSetColumns  []string                         // column names in the current UPDATE's SET clause
	hasTriggersCache  map[string]bool                  // cached trigger existence per table name
	validatedTriggers map[string]bool                  // triggers whose loaded-body schema refs were validated
	uniqueIdxCache    map[string][]uniqueIndexDef      // cached unique-index definitions per table name
	fkCache           map[string][]fkCascadeRef        // cached FK ON DELETE CASCADE refs per parent table
	inTransaction     bool                             // tracks if we're inside a BEGIN/COMMIT block
	ddlBuffer         []func()                         // DDL undo operations for transaction rollback
	txSnapshots       map[string]*pager.PagerState     // pager snapshots per database at BEGIN (for ROLLBACK undo)
	outerRow          Row                              // outer query row for correlated subquery resolution
	outerRowStack     []Row                            // stack of enclosing outer rows for multi-level correlation
	outerRows         []RowMap                         // all outer rows for correlated aggregate evaluation
	aliasStack        []map[string]sql.Expr            // output-column alias maps from enclosing SELECTs (innermost last)
	aliasResolving    map[string]bool                  // alias names currently being resolved (recursion guard)
	cteScopes         [][]sql.CTEDef                   // CTE scopes from enclosing statements (innermost last)
	resolvingCTEs     map[string]bool                  // CTEs currently being resolved (circular reference detection)
	currentScanTable  string                           // table name being scanned (for qualified column resolution)
	currentDMLTable   string                           // table being INSERTed/UPDATEd (for qualified refs in CHECK/defaults)
	currentDMLCtx     *DatabaseContext                 // database context of the table being modified (trigger scoping)
	resolvingViews    map[string]bool                  // tracks views currently being resolved (circular reference detection)
	// schemaPin, when non-nil, restricts unqualified table/view name
	// resolution to a single schema (view-body name resolution, matching
	// SQLite sqlite3FixSrcList semantics).
	schemaPin *DatabaseContext
	// expandingTempView is true while a TEMP-schema view body is being
	// expanded (SQLite reports missing tables in temp views without the
	// "main." prefix).
	expandingTempView bool
	// expandingView is true while any view body is being expanded (used to
	// scope the "main." prefix on missing-table errors to view bodies).
	expandingView bool
	exprDepthLimit int // SQLITE_LIMIT_EXPR_DEPTH: max view/subquery nesting depth (default 1000)
	nestDepth      int // current view/subquery nesting depth
	// Progress handler (db progress N fn): after every progressPeriod
	// operations the callback runs; a true return interrupts the statement
	// with an "interrupted" error (SQLite sqlite3_progress_handler).
	progressPeriod   int
	progressCallback func() bool
	progressCounter  int
	inCompoundMember  bool                             // executing a SELECT member of a compound query
	legacyAlterTable  bool                             // PRAGMA legacy_alter_table setting
	recursiveTriggers bool                             // PRAGMA recursive_triggers setting (allows trigger re-entry)
	foreignKeys       bool                             // PRAGMA foreign_keys setting (enables FK constraint enforcement)
	writableSchema    bool                             // PRAGMA writable_schema setting (permits sqlite_schema edits)
	dqsDDL            bool                             // SQLITE_DBCONFIG_DQS_DDL: allow double-quoted strings in DDL (default true)
	dqsDML            bool                             // SQLITE_DBCONFIG_DQS_DML: allow double-quoted strings in DML (default true)
	recursiveCTELimit int                              // PRAGMA recursive_cte_limit setting (default 100000, matching SQLite test builds)
	reverseUnordered  bool                             // PRAGMA reverse_unordered_selects: reverse the scan order of the top-level SELECT when it has no ORDER BY
	selectDepth       int                              // current SELECT nesting depth (1 = top-level statement)
	countChanges      bool                             // PRAGMA count_changes: DML statements return a row with the changed-row count
	returningStrict   bool                             // RETURNING eval: unknown columns are errors (SQLite semantics)
	returningTable    string                           // table name for RETURNING qualified column resolution
	// aggRowMaps, when non-nil, holds the row set an aggregate query is
	// evaluating over. Nested aggregate functions (e.g. round(avg(x),2))
	// resolve through it instead of evaluating per-row.
	aggRowMaps      []RowMap
	encoding        string                    // database text encoding: "UTF-8", "UTF-16le", "UTF-16be"
	ftsTables       map[string]*fts.FTS3Table // FTS3/4/5 tables (table name -> instance)
	currentFTSMatch string                    // current FTS table for MATCH evaluation context
	usingAutoIndex  bool                      // tracks whether an ephemeral index is being used (for EQP)
	// counterVal is the backing state for the test-only counter() SQL function
	// (SQLite test1.c selectH_counter). It resets at the start of each
	// statement so unused-column pruning tests are unaffected by prior calls.
	counterVal int64
	// nondeterVal is the backing state for the test-only nondeter() SQL
	// function (SQLite having.test): it increments per call and returns
	// counter%2. Resets at statement start (having.test sets ::nondeter_ret 0
	// before each query, and each query is a separate statement).
	nondeterVal int64
}

// Row provides column value lookup for expression evaluation.
// Implementations avoid per-row map allocation by using index-based access.
type Row interface {
	// Get returns the value for a named column and whether it was found.
	Get(name string) (interface{}, bool)
}

// structRow is an index-based Row that stores values in a slice
// with a shared column name→position index, avoiding per-row map allocation.
type structRow struct {
	values []interface{}
	index  map[string]int // shared across rows with same schema
	rowID  int64
}

func (r *structRow) Get(name string) (interface{}, bool) {
	if isRowIDName(name) {
		// rowid/_rowid_/oid are implicit INTEGER-affinity columns. Wrap them so
		// comparisons apply SQLite affinity rules (e.g. rowid <= '0'
		// converts '0' to 0), matching how buildRowMap wraps real columns.
		return &util.ColumnValue{Value: r.rowID, Affinity: 'I'}, true
	}
	if idx, ok := r.index[name]; ok && idx < len(r.values) {
		return r.values[idx], true
	}
	return nil, false
}

// RowMap implements Row for map-backed row stores.
type RowMap map[string]interface{}

// positionalRowKey is a reserved RowMap key holding the row's original
// positional value slice. It lets SELECT * output duplicate-named columns
// (e.g. a view with three columns aliased '') in order, which a name-keyed
// map cannot distinguish. The NUL byte cannot appear in a column name.
const positionalRowKey = "\x00frigolite_positional"

func (m RowMap) Get(name string) (interface{}, bool) {
	v, ok := m[name]
	return v, ok
}

// LastInsertRowID returns the rowid of the last inserted row.
func (e *Engine) LastInsertRowID() int64 {
	return e.lastRowID
}

// SetAuthorizer sets the authorization callback for the engine.
// A nil authorizer allows all operations (default behavior).
func (e *Engine) SetAuthorizer(a auth.Authorizer) {
	e.authorizer = a
}

// SetExprDepthLimit sets the maximum view/subquery nesting depth
// (SQLITE_LIMIT_EXPR_DEPTH). A negative value queries the current limit.
func (e *Engine) SetExprDepthLimit(n int) int {
	if n >= 0 {
		e.exprDepthLimit = n
	}
	return e.exprDepthLimit
}

// SetTriggerDepthLimit sets the maximum trigger nesting depth
// (SQLITE_LIMIT_TRIGGER_DEPTH). A negative value queries the current limit.
// The limit is stored on the engine and used by fireTrigger to abort
// recursive trigger chains.
func (e *Engine) SetTriggerDepthLimit(n int) int {
	if n >= 0 {
		e.triggerDepthLimit = n
	}
	return e.triggerDepthLimit
}

// SetProgressHandler registers a progress callback invoked after every n
// engine operations (n <= 0 disables it). A true return interrupts the
// running statement with an "interrupted" error, matching SQLite's
// sqlite3_progress_handler.
func (e *Engine) SetProgressHandler(n int, fn func() bool) {
	e.progressPeriod = n
	e.progressCallback = fn
	e.progressCounter = 0
}

// checkProgress counts engine operations and, every progressPeriod calls,
// runs the registered callback. Returns a non-nil "interrupted" error when
// the callback requests an abort. A nil callback is a no-op.
func (e *Engine) checkProgress() error {
	if e.progressCallback == nil || e.progressPeriod <= 0 {
		return nil
	}
	e.progressCounter++
	if e.progressCounter >= e.progressPeriod {
		e.progressCounter = 0
		if e.progressCallback() {
			return fmt.Errorf("interrupted")
		}
	}
	return nil
}

// SetDQS configures SQLite's double-quoted-string (DQS) behavior.
// ddl=true allows double-quoted strings in DDL statements (CREATE TABLE
// CHECK/DEFAULT expressions, CREATE INDEX keys); dml=true allows them in DML
// (SELECT/INSERT/UPDATE expressions). Both default to true, matching SQLite.
// When disabled, an unresolved double-quoted identifier is an error
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (e *Engine) SetDQS(ddl, dml bool) {
	e.dqsDDL = ddl
	e.dqsDML = dml
}

// RegisterFunction registers a scalar SQL function for this engine instance.
// It is used by the test harness to reproduce SQLite's TCL-defined functions
// (e.g. `db func f f` where f returns a constant).
func (e *Engine) RegisterFunction(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	e.funcs.Register(name, fn, minArgs, maxArgs)
}

// authorize checks whether an operation is allowed by the authorizer.
// Returns nil if allowed, or an error with "not authorized" if denied.
// ResultIgnore is treated as OK for non-READ operations.
func (e *Engine) authorize(action auth.Action, arg1, arg2, arg3, arg4 string) error {
	a := e.authorizer
	if a == nil {
		return nil
	}
	result := a.Authorize(action, arg1, arg2, arg3, arg4)
	switch result {
	case auth.ResultOK, auth.ResultIgnore:
		return nil
	case auth.ResultDeny:
		return fmt.Errorf("not authorized")
	default:
		return fmt.Errorf("not authorized")
	}
}

// invalidateTableCache clears the table entry cache. Must be called after
// any DDL operation that modifies the schema (CREATE, DROP, ALTER TABLE/INDEX/VIEW/TRIGGER).
func (e *Engine) invalidateTableCache() {
	e.tableCache = make(map[string]*cachedTableEntry)
	// Column definitions are derived from sqlite_schema SQL; any DDL can
	// change them, so drop the parsed-column cache too.
	e.colCache = make(map[string][]sql.ColumnDef)
}

// rootPage returns the current root page for a table, checking the engine's
// tracked root pages first, then falling back to the schema entry.
func (e *Engine) rootPage(tableName string, schemaRoot uint32) uint32 {
	if tracked, ok := e.tableRootPages[tableName]; ok {
		return tracked
	}
	return schemaRoot
}

// updateRootPage tracks a root page change after a b-tree split and persists
// it to sqlite_schema so the correct root survives a reopen (the in-memory
// map alone would be lost, and queries would fall back to the stale schema
// rootpage after the map is cleared).
func (e *Engine) updateRootPage(tableName string, newRoot uint32) {
	e.tableRootPages[tableName] = newRoot
	// Find the table in whichever database context it lives, then persist.
	if entry, ctx, err := e.findTable(tableName); err == nil && entry != nil {
		_ = ctx.Schema.UpdateEntryRoot(entry.Name, newRoot)
	}
}

// tableBTree creates a BTree for a table, using the engine's tracked root page.
// invalidateTableCaches clears per-table caches that depend on the schema
// (column defs, table constraints, unique-index columns). Called after any
// DDL change (CREATE/DROP/ALTER TABLE, INDEX, TRIGGER) so stale entries from
// a previous incarnation of the same table name are not reused.
func (e *Engine) invalidateTableCaches() {
	e.colCache = make(map[string][]sql.ColumnDef)
	e.tcCache = make(map[string][]sql.TableConstraint)
	e.uniqueIdxCache = make(map[string][]uniqueIndexDef)
	e.nextRowIDCache = make(map[uint32]int64)
	e.autoIncSeq = make(map[uint32]int64)
	e.fkCache = make(map[string][]fkCascadeRef)
	e.tableCache = make(map[string]*cachedTableEntry)
	e.tableRootPages = make(map[string]uint32)
}

// restorePager restores a pager snapshot and invalidates all schema caches.
// A pager Restore rolls back page 1 (the schema btree), but the schema
// managers' in-memory caches are NOT automatically invalidated — a stale cache
// can describe a schema that no longer matches the restored btree, causing
// "table X already exists" / missing tables. Call this instead of raw
// Pager.Restore everywhere a statement-level rollback happens.
func (e *Engine) restorePager(pg *pager.Pager, snap *pager.PagerState) {
	if pg == nil || snap == nil {
		return
	}
	pg.Restore(snap)
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

func (e *Engine) tableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
}

// tableBTreeForName resolves the table's owning database context and builds a
// BTree over that context's pager (a table in an ATTACHed database lives on
// the attached pager, not the main pager).
func (e *Engine) tableBTreeForName(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.tablePager(tableName), e.rootPage(tableName, schemaRoot), isTable)
}

// tablePager returns the pager that owns the given table: the attached
// database's pager for tables in ATTACHed databases, else the main pager.
func (e *Engine) tablePager(tableName string) *pager.Pager {
	pg := e.pager
	if _, ctx, err := e.findTable(tableName); err == nil && ctx != nil && ctx.Pager != nil {
		pg = ctx.Pager
	}
	return pg
}

// tableBTreePg creates a BTree for a table using a specific pager.
func (e *Engine) tableBTreePg(pg *pager.Pager, tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(pg, e.rootPage(tableName, schemaRoot), isTable)
}

// maxStmtCacheSize limits the prepared statement cache to avoid unbounded
// memory growth when many unique SQL strings are executed (e.g. INSERT with
// fmt.Sprintf). When the limit is reached, the cache is cleared and rebuilt.
const maxStmtCacheSize = 1000

// cachedTableEntry caches the result of a table lookup to avoid repeated
// schema btree scans. The cache is invalidated on DDL operations.
type cachedTableEntry struct {
	entry *schema.Entry
	ctx   *DatabaseContext
}

// sqlTemplateCache stores parsed statement templates keyed by normalized SQL
// (with literal values replaced by placeholders). When a new SQL string matches
// a cached template, the literal values are substituted into a cloned AST,
// avoiding full re-parsing. This primarily helps INSERT and UPDATE statements
// with varying literal values (e.g. fmt.Sprintf-based benchmarks).
type sqlTemplateEntry struct {
	template string     // normalized SQL with ? for literals
	ast      []sql.Stmt // cached AST (with original values)
}

// maxTemplateCacheSize limits the template cache entries.
const maxTemplateCacheSize = 100

// normalizeSQL replaces all numeric and string literals in a SQL string with '?'.
// Returns the normalized string and the extracted literal values.
// This is a fast pre-parse scan — it does NOT use the full parser.
// Only handles simple quoted strings and decimal integers/floats.
func normalizeSQL(sql string) (norm string, values []interface{}) {
	var buf strings.Builder
	buf.Grow(len(sql))
	values = make([]interface{}, 0, 16)
	i := 0
	for i < len(sql) {
		ch := sql[i]
		switch {
		case ch == '\'':
			// String literal: '...'
			buf.WriteByte('?')
			start := i + 1
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						// Escaped quote '' — include both, Replaces will handle
						i += 2
						continue
					}
					// End of string
					s := sql[start:i]
					// Only unescape if needed (avoids allocation for common case)
					if containsDoubleQuote(s) {
						s = strings.ReplaceAll(s, "''", "'")
					}
					values = append(values, s)
					i++ // skip closing quote
					break
				}
				i++
			}
		case ch >= '0' && ch <= '9':
			// Numeric literal (decimal integer or float starting with digit)
			buf.WriteByte('?')
			start := i
			i++
			hasDot := false
			for i < len(sql) {
				c := sql[i]
				if c >= '0' && c <= '9' {
					i++
				} else if c == '.' {
					hasDot = true
					i++
				} else if c == 'e' || c == 'E' {
					// Scientific notation
					i++
					if i < len(sql) && (sql[i] == '+' || sql[i] == '-') {
						i++
					}
					for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
						i++
					}
					break
				} else {
					break
				}
			}
			numStr := sql[start:i]
			if hasDot || containsExp(numStr) {
				v, _ := strconv.ParseFloat(numStr, 64)
				values = append(values, v)
			} else {
				v := fastParseInt64(numStr)
				values = append(values, v)
			}
		case ch == '.':
			// Numeric literal starting with dot (e.g., .5)
			buf.WriteByte('?')
			start := i
			i++
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
			}
			v, _ := strconv.ParseFloat(sql[start:i], 64)
			values = append(values, v)
		default:
			buf.WriteByte(ch)
			i++
		}
	}
	norm = buf.String()
	return
}

// fastParseInt64 parses a non-negative decimal integer string without sign.
// Faster than strconv.ParseInt for the common case of simple digits.
func fastParseInt64(s string) int64 {
	n := int64(0)
	for _, c := range []byte(s) {
		n = n*10 + int64(c-'0')
	}
	return n
}

// containsDoubleQuote checks if a string contains SQL escaped quotes (”).
func containsDoubleQuote(s string) bool {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '\'' && s[i+1] == '\'' {
			return true
		}
	}
	return false
}

// containsExp checks if a string contains 'e' or 'E' (scientific notation marker).
func containsExp(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 'e' || s[i] == 'E' {
			return true
		}
	}
	return false
}

// cloneStmtsWithValues clones the cached statement list and substitutes new
// literal values. This avoids re-parsing structurally identical SQL.
// Currently handles InsertStmt values; other types are returned as-is.
func cloneStmtsWithValues(stmts []sql.Stmt, values []interface{}) ([]sql.Stmt, error) {
	result := make([]sql.Stmt, len(stmts))
	valIdx := 0
	for i, stmt := range stmts {
		switch s := stmt.(type) {
		case *sql.InsertStmt:
			clone := &sql.InsertStmt{
				Table:        s.Table,
				Columns:      s.Columns,
				Values:       make([][]sql.Expr, len(s.Values)),
				OnConflict:   s.OnConflict,
				Returning:    s.Returning,
				HasReturning: s.HasReturning,
				IsReplace:    s.IsReplace,
				OrIgnore:     s.OrIgnore,
			}
			// Clone values tuples
			for vi, tuple := range s.Values {
				clone.Values[vi] = make([]sql.Expr, len(tuple))
				for vj, expr := range tuple {
					switch expr.(type) {
					case *sql.NumericLit, *sql.StringLit:
						if valIdx >= len(values) {
							return nil, fmt.Errorf("template cache: not enough values (need %d, have %d)", len(values), valIdx+1)
						}
						val := values[valIdx]
						valIdx++
						switch v := val.(type) {
						case int64:
							clone.Values[vi][vj] = &sql.NumericLit{Value: strconv.FormatInt(v, 10)}
						case float64:
							clone.Values[vi][vj] = &sql.NumericLit{Value: strconv.FormatFloat(v, 'g', -1, 64)}
						case string:
							clone.Values[vi][vj] = &sql.StringLit{Value: v}
						default:
							clone.Values[vi][vj] = expr // keep original
						}
					default:
						// Non-value expression — keep original
						clone.Values[vi][vj] = expr
					}
				}
			}
			// Clone Select for INSERT ... SELECT
			if s.Select != nil {
				clone.Select = s.Select
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

// Prepare parses and caches a SQL statement. Repeated calls with the same SQL
// string return the cached parsed statements without re-parsing.
// Additionally, structurally identical SQL (same after replacing literal values
// with placeholders) uses a template cache to avoid full re-parsing.
func (e *Engine) Prepare(sqlStr string) ([]sql.Stmt, error) {
	// Check exact match cache first (fastest)
	if cached, ok := e.stmtCache[sqlStr]; ok {
		return cached, nil
	}
	if len(e.stmtCache) >= maxStmtCacheSize {
		e.stmtCache = make(map[string][]sql.Stmt)
	}

	// Check template cache — normalize SQL and see if we've seen this structure
	normSQL, values := normalizeSQL(sqlStr)
	if normSQL != sqlStr && len(values) > 0 {
		if cached, ok := e.templateCache[normSQL]; ok {
			// Template cache hit — clone AST with new values
			cloned, err := cloneStmtsWithValues(cached.ast, values)
			if err == nil {
				// Also cache by exact SQL for future exact matches
				e.stmtCache[sqlStr] = cloned
				return cloned, nil
			}
			// If clone fails (e.g. wrong value count), fall through to re-parse
		}
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
	e.stmtCache[sqlStr] = stmts

	// Store template in template cache
	if normSQL != sqlStr && len(values) > 0 && len(e.templateCache) < maxTemplateCacheSize {
		if e.templateCache == nil {
			e.templateCache = make(map[string]*sqlTemplateEntry)
		}
		e.templateCache[normSQL] = &sqlTemplateEntry{
			template: normSQL,
			ast:      stmts,
		}
	}

	return stmts, nil
}

// NewEngine creates a new execution engine.
func NewEngine(pg *pager.Pager) *Engine {
	mainCtx := &DatabaseContext{
		Name:     "main",
		Pager:    pg,
		Schema:   schema.NewManager(pg),
		FilePath: "",
		IsMemory: false,
		IsTemp:   false,
	}

	tempCtx := (*DatabaseContext)(nil)

	// Create the real temp database: an in-memory schema separate from main.
	// SQLite's temp database shadows main for unqualified name resolution
	// (temp-first), so a temp view/table with the same name as a main object
	// takes precedence for unqualified references.
	if tempPg := pager.OpenInMemory(pager.DefaultPageSize); tempPg != nil {
		tc := &DatabaseContext{
			Name:     "temp",
			Pager:    tempPg,
			Schema:   schema.NewManager(tempPg),
			FilePath: "",
			IsMemory: true,
			IsTemp:   true,
		}
		if err := tc.Schema.Init(); err == nil {
			tempCtx = tc
		}
	}

	e := &Engine{
		databases: map[string]*DatabaseContext{
			"MAIN": mainCtx,
		},
		dbList:            []*DatabaseContext{mainCtx},
		mainDB:            mainCtx,
		pager:             mainCtx.Pager,
		schema:            mainCtx.Schema,
		funcs:             function.NewRegistry(),
		vtabs:             vtab.NewRegistry(),
		colCache:          make(map[string][]sql.ColumnDef),
		stmtCache:         make(map[string][]sql.Stmt),
		tableRootPages:    make(map[string]uint32),
		tableCache:        make(map[string]*cachedTableEntry),
		nextRowIDCache:    make(map[uint32]int64),
		autoIncSeq:        make(map[uint32]int64),
		hasTriggersCache:  make(map[string]bool),
		encoding:          "UTF-8",
		recursiveCTELimit: 100000,
		exprDepthLimit:    1000, // SQLite default SQLITE_LIMIT_EXPR_DEPTH
		dqsDDL:            true, // SQLite default: double-quoted strings allowed in DDL
		dqsDML:            true, // SQLite default: double-quoted strings allowed in DML
		ftsTables:         make(map[string]*fts.FTS3Table),
	}
	if tempCtx != nil {
		e.databases["TEMP"] = tempCtx
		e.databases["TEMPORARY"] = tempCtx
		e.dbList = append(e.dbList, tempCtx)
	}
	e.vtabs.RegisterDefaults()
	// Register FTS modules (overrides NoopModule defaults)
	ftsMod := fts.NewFTS3Module("fts3")
	e.vtabs.Register("fts3", ftsMod)
	e.vtabs.Register("fts4", fts.NewFTS3Module("fts4"))
	e.vtabs.Register("fts5", fts.NewFTS3Module("fts5"))
	// SQLite-internal functions used by ALTER TABLE machinery.
	e.funcs.Register("sqlite_rename_quotefix", fnSQLiteRenameQuoteFix, 2, 2)
	return e
}

// getDB returns the database context for a given schema name.
// Returns nil if the schema is not found.
func (e *Engine) getDB(name string) *DatabaseContext {
	upper := strings.ToUpper(name)
	if db, ok := e.databases[upper]; ok {
		return db
	}
	return nil
}

// parseSchemaName splits a qualified name like "aux.t3" into schema name ("aux") and object name ("t3").
// If no schema prefix is present, returns ("", name).
func parseSchemaName(name string) (schema string, object string) {
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		return name[:dotIdx], name[dotIdx+1:]
	}
	return "", name
}

// resolveDB resolves a potentially schema-qualified name to a database context and the unqualified name.
// If no schema prefix is present, returns nil for ctx (caller should use mainDB).
//
//lint:ignore U1000 Planned for P3 ATTACH
func (e *Engine) resolveDB(name string) (ctx *DatabaseContext, object string) {
	schemaName, object := parseSchemaName(name)
	if schemaName == "" {
		return nil, object
	}
	ctx = e.getDB(schemaName)
	return ctx, object
}

// detectExternalSchemaChanges checks every attached database's schema manager
// for external file modification (an attached file written by another
// connection). When a change is detected the pager cache and tableCache are
// invalidated so the next lookup re-reads the file.
func (e *Engine) detectExternalSchemaChanges() {
	for _, ctx := range e.databases {
		if ctx == nil || ctx.Schema == nil || ctx.Pager == nil {
			continue
		}
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		before := ctx.Schema.FileStamp()
		ctx.Schema.CheckExternalMod()
		if ctx.Schema.FileStamp() != before {
			e.tableCache = make(map[string]*cachedTableEntry)
		}
	}
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
	// Check table cache first
	if cached, ok := e.tableCache[name]; ok {
		// Re-hydrate FTS state for cached entries too: a fresh engine has an
		// empty ftsTables map until the first lookup, and tableCache may be
		// consulted before ensureFTSForTable has run (e.g. after a schema
		// invalidation that cleared only the cache used by UPDATE).
		e.ensureFTSForTable(cached.entry)
		return cached.entry, cached.ctx, nil
	}

	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
		entry, err := ctx.Schema.FindTable(objName)
		if err != nil && schemaName != "main" && schemaName != "temp" {
			// The attached database's file may have been modified by an
			// external connection since we attached (schema reload test):
			// drop the pager cache and schema and retry once.
			if ctx.Pager != nil {
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
		e.ensureFTSForTable(entry)
		e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: ctx}
		return entry, ctx, nil
	}

	// A schema pin (view being expanded in its own schema) restricts
	// unqualified name resolution to that schema, matching SQLite's
	// sqlite3FixSrcList: the body of a non-temp view cannot see temp/other
	// schema objects of the same name.
	if e.schemaPin != nil {
		entry, err := e.schemaPin.Schema.FindTable(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
		e.ensureFTSForTable(entry)
		e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: e.schemaPin}
		return entry, e.schemaPin, nil
	}

	// No schema prefix: search temp first (temp shadows main), then main,
	// then attached databases. A temp VIEW with this name shadows a main
	// TABLE: return an error so the caller falls through to view resolution
	// (SQLite resolves the temp view first and reports circularity when the
	// view's body re-enters its own name). Schema tables (sqlite_master/etc)
	// and sqlite_sequence always resolve to their native (main) schema,
	// never to the temp schema's synthetic fallback.
	if tc := e.getDB("temp"); tc != nil && tc != e.mainDB && !isSchemaTable(name) && !isSQLiteSequence(name) {
		if entry, err := tc.Schema.FindTable(name); err == nil {
			e.ensureFTSForTable(entry)
			e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: tc}
			return entry, tc, nil
		}
		if _, vErr := tc.Schema.FindView(name); vErr == nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
	}

	// No schema prefix: search main first, then attached databases
	entry, err := e.mainDB.Schema.FindTable(name)
	if err == nil {
		e.ensureFTSForTable(entry)
		e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: e.mainDB}
		return entry, e.mainDB, nil
	}

	// Search attached databases in ATTACH order (deterministic; SQLite resolves
	// unqualified names to the first database that has the table).
	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindTable(name)
		if err == nil {
			e.ensureFTSForTable(entry)
			e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: ctx}
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such table: %s", name)
}

// isNonModifiableTable reports whether a table entry cannot be modified by
// INSERT/UPDATE/DELETE: the sqlite_schema system tables and pragma virtual
// tables (PRAGMA_ prefixed) are read-only.
func (e *Engine) isNonModifiableTable(entry *schema.Entry) bool {
	if entry == nil {
		return false
	}
	switch {
	case strings.EqualFold(entry.Name, "sqlite_master"),
		strings.EqualFold(entry.Name, "sqlite_schema"),
		strings.EqualFold(entry.Name, "sqlite_temp_master"),
		strings.EqualFold(entry.Name, "sqlite_temp_schema"):
		// PRAGMA writable_schema=ON permits direct edits to sqlite_schema.
		return !e.writableSchema
	}
	return strings.HasPrefix(strings.ToUpper(entry.Name), "PRAGMA_")
}

// isStoragelessVirtualTable reports whether a table entry is a virtual table
// without module-backed row storage (rtree, echo, dbstat, ...). Such tables
// accept writes as no-ops; FTS tables have real storage and are excluded.
func (e *Engine) isStoragelessVirtualTable(entry *schema.Entry) bool {
	if entry == nil || !strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
		return false
	}
	_, isFTS := e.ftsTables[entry.Name]
	return !isFTS
}

// findView searches for a view across all attached databases.
func (e *Engine) findView(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
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

	// A schema pin (view being expanded in its own schema) restricts
	// unqualified view resolution to that schema (SQLite sqlite3FixSrcList).
	if e.schemaPin != nil {
		entry, err := e.schemaPin.Schema.FindView(name)
		if err != nil {
			return nil, nil, fmt.Errorf("no such view: %s", name)
		}
		return entry, e.schemaPin, nil
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

	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindView(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such view: %s", name)
}

// findTrigger searches for a trigger across all attached databases.
func (e *Engine) findTrigger(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such trigger: %s", name)
		}
		entry, err := ctx.Schema.FindTrigger(objName)
		if err != nil {
			return nil, nil, err
		}
		return entry, ctx, nil
	}

	entry, err := e.mainDB.Schema.FindTrigger(name)
	if err == nil {
		return entry, e.mainDB, nil
	}

	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindTrigger(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such trigger: %s", name)
}

// findIndex searches for an index across all attached databases.
func (e *Engine) findIndex(name string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such index: %s", name)
		}
		entry, err := ctx.Schema.FindIndex(objName)
		if err != nil {
			return nil, nil, err
		}
		return entry, ctx, nil
	}

	entry, err := e.mainDB.Schema.FindIndex(name)
	if err == nil {
		return entry, e.mainDB, nil
	}

	for _, ctx := range e.dbList {
		if ctx == e.mainDB {
			continue
		}
		entry, err := ctx.Schema.FindIndex(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such index: %s", name)
}

// validateNoRaiseOutsideTrigger walks a statement's expression trees and
// rejects RAISE() expressions when not inside a trigger program (SQLite's
// "RAISE() may only be used within a trigger-program"). This is a compile-
// time check: the runtime evaluation in evalRaiseExpr would miss RAISE()
// inside expressions that never execute (e.g. GROUP BY/HAVING over an empty
// table).
func (e *Engine) validateNoRaiseOutsideTrigger(stmt sql.Stmt) error {
	var check func(expr sql.Expr) error
	check = func(expr sql.Expr) error {
		if expr == nil {
			return nil
		}
		if _, ok := expr.(*sql.RaiseExpr); ok {
			return fmt.Errorf("RAISE() may only be used within a trigger-program")
		}
		if f, ok := expr.(*sql.FuncCall); ok && strings.EqualFold(f.Name, "raise") {
			return fmt.Errorf("RAISE() may only be used within a trigger-program")
		}
		// Recursively visit children.
		switch v := expr.(type) {
		case *sql.ParenExpr:
			return check(v.Expr)
		case *sql.BinaryOp:
			if err := check(v.Left); err != nil {
				return err
			}
			return check(v.Right)
		case *sql.UnaryOp:
			return check(v.Operand)
		case *sql.FuncCall:
			for _, a := range v.Args {
				if err := check(a); err != nil {
					return err
				}
			}
		case *sql.CastExpr:
			return check(v.Operand)
		case *sql.CaseExpr:
			if err := check(v.Operand); err != nil {
				return err
			}
			for _, w := range v.Whens {
				if err := check(w.When); err != nil {
					return err
				}
				if err := check(w.Then); err != nil {
					return err
				}
			}
			return check(v.Else)
		case *sql.Between:
			if err := check(v.Operand); err != nil {
				return err
			}
			if err := check(v.Low); err != nil {
				return err
			}
			return check(v.High)
		case *sql.InList:
			if err := check(v.Operand); err != nil {
				return err
			}
			for _, item := range v.List {
				if err := check(item); err != nil {
					return err
				}
			}
		case *sql.RowValue:
			for _, sub := range v.Values {
				if err := check(sub); err != nil {
					return err
				}
			}
		case *sql.IsNull:
			return check(v.Operand)
		case *sql.IsNotNull:
			return check(v.Operand)
		case *sql.Subquery:
			return e.checkSelectRaise(v.Select)
		case *sql.ExistsExpr:
			return e.checkSelectRaise(v.Select)
		}
		return nil
	}

	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return e.checkSelectRaise(s)
	case *sql.InsertStmt:
		for _, tuple := range s.Values {
			for _, expr := range tuple {
				if err := check(expr); err != nil {
					return err
				}
			}
		}
		if s.Select != nil {
			return e.checkSelectRaise(s.Select)
		}
	case *sql.UpdateStmt:
		for _, a := range s.Assignments {
			if err := check(a.Value); err != nil {
				return err
			}
		}
		return check(s.Where)
	case *sql.DeleteStmt:
		return check(s.Where)
	}
	return nil
}

// checkSelectRaise walks a SELECT's columns, WHERE, GROUP BY, HAVING, ORDER
// BY, and compound tails for RAISE() expressions.
func (e *Engine) checkSelectRaise(s *sql.SelectStmt) error {
	if s == nil {
		return nil
	}
	var check func(expr sql.Expr) error
	check = func(expr sql.Expr) error {
		if expr == nil {
			return nil
		}
		if _, ok := expr.(*sql.RaiseExpr); ok {
			return fmt.Errorf("RAISE() may only be used within a trigger-program")
		}
		if f, ok := expr.(*sql.FuncCall); ok && strings.EqualFold(f.Name, "raise") {
			return fmt.Errorf("RAISE() may only be used within a trigger-program")
		}
		switch v := expr.(type) {
		case *sql.ParenExpr:
			return check(v.Expr)
		case *sql.BinaryOp:
			if err := check(v.Left); err != nil {
				return err
			}
			return check(v.Right)
		case *sql.UnaryOp:
			return check(v.Operand)
		case *sql.FuncCall:
			for _, a := range v.Args {
				if err := check(a); err != nil {
					return err
				}
			}
		case *sql.CastExpr:
			return check(v.Operand)
		case *sql.CaseExpr:
			if err := check(v.Operand); err != nil {
				return err
			}
			for _, w := range v.Whens {
				if err := check(w.When); err != nil {
					return err
				}
				if err := check(w.Then); err != nil {
					return err
				}
			}
			return check(v.Else)
		case *sql.Between:
			if err := check(v.Operand); err != nil {
				return err
			}
			if err := check(v.Low); err != nil {
				return err
			}
			return check(v.High)
		case *sql.InList:
			if err := check(v.Operand); err != nil {
				return err
			}
			for _, item := range v.List {
				if err := check(item); err != nil {
					return err
				}
			}
		case *sql.RowValue:
			for _, sub := range v.Values {
				if err := check(sub); err != nil {
					return err
				}
			}
		case *sql.IsNull:
			return check(v.Operand)
		case *sql.IsNotNull:
			return check(v.Operand)
		case *sql.Subquery:
			return e.checkSelectRaise(v.Select)
		case *sql.ExistsExpr:
			return e.checkSelectRaise(v.Select)
		}
		return nil
	}
	// Only GROUP BY and HAVING need the early check: SQLite validates ORDER
	// BY column matching first ("1st ORDER BY term does not match any
	// column", triggerC-16.1), and SELECT-column/WHERE RAISE() is caught by
	// runtime evaluation when rows exist. GROUP BY/HAVING over an empty table
	// never evaluates, hiding the error (triggerC-16.2).
	for _, g := range s.GroupBy {
		if err := check(g); err != nil {
			return err
		}
	}
	if err := check(s.Having); err != nil {
		return err
	}
	if s.Union != nil {
		return e.checkSelectRaise(s.Union)
	}
	return nil
}

// Exec executes a single SQL statement and returns the result.
func (e *Engine) Exec(stmt sql.Stmt) *Result {
	// Reset the test-only counter() function state at the start of each
	// statement. SQLite's column-pruning optimization skips evaluating
	// counter() in unused columns; since our engine lacks that optimization,
	// resetting per-statement keeps the results consistent (counter() values
	// within a single statement start from 1).
	e.counterVal = 0
	e.nondeterVal = 0

	// SQLite guarantees statement atomicity: when a statement fails (a
	// constraint violation, a trigger error, etc.) every change it made is
	// rolled back. We emulate that by snapshotting all pagers before DML and
	// restoring them on error. Nested Exec calls (trigger bodies) snapshot
	// again, so a failure inside a trigger rolls back the inner statement and
	// then propagates to the outer statement's restore.
	var snaps []pagerSnap
	isDML := false
	switch stmt.(type) {
	case *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt:
		isDML = true
		// SQLite guarantees statement atomicity: when a statement fails (a
		// constraint violation, a trigger error, etc.) every change it made
		// is rolled back. We emulate that by snapshotting all pagers before
		// DML and restoring them on error. A single-row VALUES INSERT cannot
		// fail after writing (constraints are checked before the write), so
		// it needs no snapshot — this avoids an O(pages) copy per insert in
		// bulk-load loops (100k-row inserts are ~100x faster).
		if !e.dmlCanSkipSnapshot(stmt) {
			snaps = e.snapshotAllPagers()
		}
	}

	var res *Result
	// RAISE() is only valid inside a trigger program. SQLite rejects it at
	// prepare time; the engine's runtime evaluation would miss it when the
	// containing expression never executes (e.g. GROUP BY/HAVING over an
	// empty table), so validate the whole statement here.
	if e.triggerDepth == 0 {
		if err := e.validateNoRaiseOutsideTrigger(stmt); err != nil {
			return &Result{Error: err}
		}
		// Triggers loaded from sqlite_master may reference objects that no
		// longer resolve (reopen with different attachments); SQLite reports
		// "malformed database schema" at schema load. Validate once per
		// statement.
		if err := e.validateLoadedTriggers(); err != nil {
			return &Result{Error: err}
		}
	}
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		res = e.execSelect(s)
	case *sql.InsertStmt:
		res = e.execInsert(s)
	case *sql.UpdateStmt:
		res = e.execUpdate(s)
	case *sql.DeleteStmt:
		res = e.execDelete(s)
	case *sql.CommitStmt:
		res = e.execCommit()
	case *sql.BeginStmt:
		res = e.execBegin()
	case *sql.RollbackStmt:
		res = e.execRollback()
	default:
		res = e.execOtherDDL(stmt)
	}

	// Roll back the whole statement on error, restoring all pagers and
	// dropping row-id caches that may reference restored pages.
	if isDML && res != nil && res.Error != nil {
		e.restoreAllPagers(snaps)
		e.nextRowIDCache = make(map[uint32]int64)
		e.autoIncSeq = make(map[uint32]int64)
		res.Changes = 0
		res.LastInsertRowID = 0
	}

	// Track changes and last rowid for CHANGES() / LAST_INSERT_ROWID() functions.
	// SQLite: sqlite3_changes() reflects the last INSERT/UPDATE/DELETE only;
	// SELECT/DDL statements do not reset the counter.
	if res != nil {
		if isDML {
			e.lastChanges = res.Changes
		}
		if res.LastInsertRowID > 0 {
			e.lastRowID = res.LastInsertRowID
		}
	}

	// PRAGMA count_changes: a DML statement returns a single row with the
	// changed-row count (SQLite's legacy behavior when the pragma is on).
	if isDML && res != nil && res.Error == nil && e.countChanges && len(res.Rows) == 0 {
		res.Rows = [][]interface{}{{res.Changes}}
	}
	// Flush attached database pagers after a successful DML/DDL so a later
	// connection on the attached file sees the writes immediately (SQLite
	// commits each statement). The main pager is flushed on Close.
	if res != nil && res.Error == nil {
		for name, ctx := range e.databases {
			upper := strings.ToUpper(name)
			if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
				continue
			}
			if ctx.Pager != nil {
				_ = ctx.Pager.Flush()
			}
		}
	}
	return res
}

// pagerSnap pairs a pager with the snapshot taken from it, so a restore can
// match each pager to its own snapshot regardless of map iteration order.
type pagerSnap struct {
	pg    *pager.Pager
	state *pager.PagerState
}

// dmlCanSkipSnapshot reports whether a DML statement can skip the pre-rollback
// pager snapshot because it cannot fail after partially writing. A single-row
// VALUES INSERT (no SELECT, no RETURNING, not REPLACE/upsert, no triggers, no
// FK enforcement) either writes its one row or fails before writing — there is
// no partial state to restore.
func (e *Engine) dmlCanSkipSnapshot(stmt sql.Stmt) bool {
	ins, ok := stmt.(*sql.InsertStmt)
	if !ok {
		return false // UPDATE/DELETE can fail mid-scan after earlier writes
	}
	if ins.Select != nil || ins.HasReturning || ins.IsReplace || len(ins.Values) != 1 {
		return false
	}
	if ins.OnConflict != nil {
		return false // DO NOTHING / DO UPDATE upsert paths may skip or modify rows
	}
	if e.foreignKeys {
		return false // FK enforcement could reject after other writes
	}
	if e.hasTriggersForTable(ins.Table) {
		return false // a trigger could fail after the insert
	}
	return true
}

// snapshotAllPagers captures the in-memory state of every database pager,
// pairing each snapshot with the pager it came from.
func (e *Engine) snapshotAllPagers() []pagerSnap {
	var snaps []pagerSnap
	seen := make(map[*pager.Pager]bool)
	for _, ctx := range e.databases {
		if ctx == nil || ctx.Pager == nil || seen[ctx.Pager] {
			continue
		}
		seen[ctx.Pager] = true
		snaps = append(snaps, pagerSnap{pg: ctx.Pager, state: ctx.Pager.Snapshot()})
	}
	return snaps
}

// restoreAllPagers restores each pager to the snapshot captured from it by
// snapshotAllPagers. Pairing by pager identity (rather than positional index)
// keeps snapshots matched even though e.databases is a map with random
// iteration order.
func (e *Engine) restoreAllPagers(snaps []pagerSnap) {
	if len(snaps) == 0 {
		return
	}
	for _, snap := range snaps {
		if snap.pg != nil && snap.state != nil {
			snap.pg.Restore(snap.state)
		}
	}
	e.invalidateTableCaches()
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
}

func (e *Engine) execOtherDDL(stmt sql.Stmt) *Result {
	// Invalidate table cache on any DDL operation to ensure consistency
	e.invalidateTableCache()

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
	case *sql.AttachStmt:
		if s.IsDetach {
			return e.execDetach(s)
		}
		return e.execAttach(s)
	default:
		// Begin, Rollback, Vacuum, Reindex, Savepoint — all no-ops
		return &Result{}
	}
}
