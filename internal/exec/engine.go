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
	templateCache     map[string]*sqlTemplateEntry     // normalized SQL → cached AST template
	triggerDepth      int                              // prevents recursive trigger firing
	triggerNewRow     Row                              // new row values for trigger execution (keyed as "new.colname")
	triggerOldRow     Row                              // old row values for trigger execution (keyed as "old.colname")
	hasTriggersCache  map[string]bool                  // cached trigger existence per table name
	uniqueIdxCache    map[string][]uniqueIndexDef      // cached unique-index definitions per table name
	fkCache           map[string][]fkCascadeRef        // cached FK ON DELETE CASCADE refs per parent table
	inTransaction     bool                             // tracks if we're inside a BEGIN/COMMIT block
	ddlBuffer         []func()                         // DDL undo operations for transaction rollback
	txSnapshots       map[string]*pager.PagerState     // pager snapshots per database at BEGIN (for ROLLBACK undo)
	outerRow          Row                              // outer query row for correlated subquery resolution
	outerRowStack     []Row                            // stack of enclosing outer rows for multi-level correlation
	outerRows         []RowMap                         // all outer rows for correlated aggregate evaluation
	cteScopes         [][]sql.CTEDef                   // CTE scopes from enclosing statements (innermost last)
	currentScanTable  string                           // table name being scanned (for qualified column resolution)
	resolvingViews    map[string]bool                  // tracks views currently being resolved (circular reference detection)
	legacyAlterTable  bool                             // PRAGMA legacy_alter_table setting
	recursiveTriggers bool                             // PRAGMA recursive_triggers setting (allows trigger re-entry)
	encoding          string                           // database text encoding: "UTF-8", "UTF-16le", "UTF-16be"
	ftsTables         map[string]*fts.FTS3Table        // FTS3/4/5 tables (table name -> instance)
	currentFTSMatch   string                           // current FTS table for MATCH evaluation context
	usingAutoIndex    bool                             // tracks whether an ephemeral index is being used (for EQP)
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
	if name == "rowid" {
		// rowid is an implicit INTEGER-affinity column. Wrap it so
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

// updateRootPage tracks a root page change after a b-tree split.
func (e *Engine) updateRootPage(tableName string, newRoot uint32) {
	e.tableRootPages[tableName] = newRoot
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
	e.fkCache = make(map[string][]fkCascadeRef)
	e.tableCache = make(map[string]*cachedTableEntry)
	e.tableRootPages = make(map[string]uint32)
}

func (e *Engine) tableBTree(tableName string, schemaRoot uint32, isTable bool) *btree.BTree {
	return btree.NewBTree(e.pager, e.rootPage(tableName, schemaRoot), isTable)
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

	e := &Engine{
		databases: map[string]*DatabaseContext{
			"MAIN": mainCtx,
			"TEMP": mainCtx, // TEMP is an alias for main (no true temp db support yet)
		},
		mainDB:           mainCtx,
		pager:            mainCtx.Pager,
		schema:           mainCtx.Schema,
		funcs:            function.NewRegistry(),
		vtabs:            vtab.NewRegistry(),
		colCache:         make(map[string][]sql.ColumnDef),
		stmtCache:        make(map[string][]sql.Stmt),
		tableRootPages:   make(map[string]uint32),
		tableCache:       make(map[string]*cachedTableEntry),
		nextRowIDCache:   make(map[uint32]int64),
		hasTriggersCache: make(map[string]bool),
		encoding:         "UTF-8",
		ftsTables:        make(map[string]*fts.FTS3Table),
	}
	e.vtabs.RegisterDefaults()
	// Register FTS modules (overrides NoopModule defaults)
	ftsMod := fts.NewFTS3Module("fts3")
	e.vtabs.Register("fts3", ftsMod)
	e.vtabs.Register("fts4", fts.NewFTS3Module("fts4"))
	e.vtabs.Register("fts5", fts.NewFTS3Module("fts5"))
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

// findTable searches for a table across all attached databases.
// If the name has a schema prefix (e.g. "aux.t3"), it searches only that database.
// If no schema prefix, it searches main first, then attached databases.
func (e *Engine) findTable(name string) (*schema.Entry, *DatabaseContext, error) {
	// Check table cache first
	if cached, ok := e.tableCache[name]; ok {
		return cached.entry, cached.ctx, nil
	}

	schemaName, objName := parseSchemaName(name)
	if schemaName != "" {
		ctx := e.getDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such table: %s", name)
		}
		entry, err := ctx.Schema.FindTable(objName)
		if err != nil {
			return nil, nil, err
		}
		e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: ctx}
		return entry, ctx, nil
	}

	// No schema prefix: search main first, then attached databases
	entry, err := e.mainDB.Schema.FindTable(name)
	if err == nil {
		e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: e.mainDB}
		return entry, e.mainDB, nil
	}

	// Search attached databases
	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		entry, err := ctx.Schema.FindTable(name)
		if err == nil {
			e.tableCache[name] = &cachedTableEntry{entry: entry, ctx: ctx}
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such table: %s", name)
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

	entry, err := e.mainDB.Schema.FindView(name)
	if err == nil {
		return entry, e.mainDB, nil
	}

	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
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

	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
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

	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		entry, err := ctx.Schema.FindIndex(name)
		if err == nil {
			return entry, ctx, nil
		}
	}

	return nil, nil, fmt.Errorf("no such index: %s", name)
}

// Exec executes a single SQL statement and returns the result.
func (e *Engine) Exec(stmt sql.Stmt) *Result {
	var res *Result
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
	// Track changes and last rowid for CHANGES() / LAST_INSERT_ROWID() functions
	if res != nil {
		e.lastChanges = res.Changes
		if res.LastInsertRowID > 0 {
			e.lastRowID = res.LastInsertRowID
		}
	}
	return res
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
