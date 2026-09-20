package execddl

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
)

// MaxAttachedDatabases is the SQLite SQLITE_MAX_ATTACHED default (10): the
// maximum number of attached databases a connection may hold, excluding
// main/temp. The engine enforces this in execAttach and reports it via
// Engine.Limit("SQLITE_LIMIT_ATTACHED").
const MaxAttachedDatabases = 10

// --- ATTACH / DETACH ---

func (e *DDLExecutor) DetachAll() {
	for name, ctx := range e.ctx.Databases() {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		ctx.Pager.Close()
		delete(e.ctx.Databases(), name)
	}
	// Rebuild the ordered list with only main.
	e.ctx.ResetDBList()
}

// closeAttachedPagers closes every attached database's pager (main/temp
// excluded), deduplicating same-file aliases so a shared pager closes exactly
// once, and returns the first close error.
func (e *DDLExecutor) closeAttachedPagers() error {
	var firstErr error
	closed := make(map[*pager.Pager]bool)
	for name, ctx := range e.ctx.Databases() {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		if ctx.Pager != nil {
			// Same-file aliases share one pager: close it exactly once.
			if closed[ctx.Pager] {
				continue
			}
			closed[ctx.Pager] = true
			if err := ctx.Pager.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Close closes every database pager (attached databases first, then main),
// flushing buffered writes to disk so a later connection on an attached file
// sees the committed schema/data.
func (e *DDLExecutor) Close() error {
	firstErr := e.closeAttachedPagers()
	if e.ctx.Pager() != nil {
		if err := e.ctx.Pager().Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// --- CREATE TABLE ---

func (e *DDLExecutor) execCreateTable(s *sql.CreateTableStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if res := tempQualifiedNameError(s); res != nil {
		return res
	}
	ctx, tableName, res := e.resolveCreateTableSchema(s)
	if res != nil {
		return res
	}
	if res := e.tableNameTooBigError(s, tableName); res != nil {
		return res
	}
	if res := e.runCreateTableValidations(ctx, s, tableName); res != nil {
		return res
	}

	// CREATE TABLE ... AS SELECT: SQLite compiles (and name-resolves) the
	// SELECT at PREPARE time, before the CREATE TABLE program runs, so a
	// resolution failure ("no such column: t9.c1", misc1-15.1.x) leaves NO
	// table behind, and a self-referencing select still reports
	// "no such table". Execute the select first; only then allocate/create.
	var ctasResult *Result
	if s.AsSelect != nil {
		ctasResult = e.ctx.ExecSelect(s.AsSelect)
		if ctasResult.Error != nil {
			return ctasResult
		}
	}

	pg, res := e.initTableRootPage(ctx, s)
	if res != nil {
		return res
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: pg.PageNum,
		SQL:      e.createTableSQL(s),
	}

	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// SQLite lazily creates a real sqlite_sequence(name,seq) table when the
	// first AUTOINCREMENT table is created (build.c:2922-2931). The engine
	// mirrors this: a real schema entry lets SELECT/UPDATE/DELETE on
	// sqlite_sequence use the normal table machinery.
	if res := e.ensureAutoIncrementSequence(ctx, s); res != nil {
		return res
	}

	// Create UNIQUE autoindex entries for column-level and table-level
	// UNIQUE constraints (deduplicated; redundant with the PK on WITHOUT
	// ROWID tables are dropped), matching SQLite's sqlite_autoindex_* names.
	if res := e.createAutoIndexes(ctx, tableName, s, entry); res.Error != nil {
		return res
	}

	// Handle CREATE TABLE ... AS SELECT
	if s.AsSelect != nil {
		// The schema prefix is resolved above (ctx/tableName); the AS SELECT
		// path must register the table under the unqualified name in the
		// target schema (SQLite stores "CREATE TABLE t1(...)", never
		// "CREATE TABLE aux.t1(...)").
		return e.execCreateTableAsSelect(s, ctx, tableName, ctasResult)
	}

	return &Result{Changes: 0}
}

// tempQualifiedNameError rejects CREATE TEMP TABLE with a schema-qualified
// name whose prefix is not the temp schema itself: "CREATE TEMP TABLE
// main.t1" and "CREATE TEMP TABLE aux.t1" fail with "temporary table name
// must be unqualified". "CREATE TEMP TABLE temp.t1" is allowed (the prefix
// redundantly names the same temp schema).
func tempQualifiedNameError(s *sql.CreateTableStmt) *Result {
	if !s.Temporary || !strings.Contains(s.Name, ".") {
		return nil
	}
	prefix := strings.ToUpper(s.Name[:strings.Index(s.Name, ".")])
	if prefix != "TEMP" && prefix != "TEMPORARY" {
		return &Result{Error: fmt.Errorf("temporary table name must be unqualified")}
	}
	return nil
}

// tableNameTooBigError applies build.c sqlite3StartTable's
// SQLITE_LIMIT_LENGTH check: a table (or column) name longer than the limit
// fails SQLITE_TOOBIG, "string or blob too big" (sqllimits1-17.x builds a
// >100000-char table name under LENGTH=100000).
func (e *DDLExecutor) tableNameTooBigError(s *sql.CreateTableStmt, tableName string) *Result {
	lim := e.ctx.LengthLimit()
	if lim <= 0 {
		return nil
	}
	if len(tableName) >= lim {
		return &Result{Error: fmt.Errorf("string or blob too big")}
	}
	for _, cd := range s.Columns {
		if len(cd.Name) >= lim {
			return &Result{Error: fmt.Errorf("string or blob too big")}
		}
	}
	return nil
}

// initTableRootPage allocates the new table's root page, clears any previous
// table's cached rowid sequence, initializes a fresh empty leaf, and writes
// the page back. A reused page (from a dropped table) must not carry the
// previous table's stale cells, so the page is zeroed and given a valid
// header. WITHOUT ROWID tables live in an index btree (SQLite build.c: the
// table root is created with BTREE_WRDATA / index-leaf pages).
func (e *DDLExecutor) initTableRootPage(ctx *DatabaseContext, s *sql.CreateTableStmt) (*pager.Page, *Result) {
	pg, perr := allocateRootPage(ctx.Pager)
	if perr != nil {
		return nil, &Result{Error: perr}
	}
	// A reused page (from a dropped table) must not carry the previous
	// table's cached rowid sequence; a fresh table starts at rowid 1.
	e.ctx.ClearRowIDState(ctx.Pager, pg.PageNum)
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	if s.WithoutRowid {
		pg.Data[0] = storage.PageTypeLeafIndex
	}
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	// Header: type(1) freeblock(2) cellCount(2)=0 contentOffset(2)=usableSize
	// zeroPage sets the empty page's cell content area to the USABLE size
	// (put2byte(&data[hdr+5], pBt->usableSize), src/btree.c:2189): usable =
	// pageSize - reserved (header byte 20). A hardcoded pageSize-4 leaves a
	// 4-byte untracked tail that sqlite3 integrity_check reports as
	// "Fragmentation of 4 bytes reported as 0" and every subsequent insert
	// on the page packs from the shrunken end.
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ctx.Pager.UsableSize()))
	if err := ctx.Pager.WritePage(pg); err != nil {
		return nil, &Result{Error: err}
	}
	return pg, nil
}

// ensureAutoIncrementSequence lazily creates the real sqlite_sequence table
// when the CREATE declares an AUTOINCREMENT column (see
// ensureSQLiteSequenceTable).
func (e *DDLExecutor) ensureAutoIncrementSequence(ctx *DatabaseContext, s *sql.CreateTableStmt) *Result {
	if !hasAutoIncrementColumn(s) {
		return nil
	}
	if err := e.ensureSQLiteSequenceTable(ctx); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// isSyntheticSystemEntry reports whether entry is the schema manager's
// synthetic fallback for a system table (sqlite_sequence, pragma_*), which is
// returned when no real schema row exists. Such entries must not block CREATE
// TABLE: SQLite allows creating sqlite_sequence via PRAGMA writable_schema.
func (e *DDLExecutor) isSyntheticSystemEntry(entry *schema.Entry, name string) bool {
	if entry == nil {
		return false
	}
	if entry.RootPage != 1 {
		return false
	}
	upper := strings.ToUpper(name)
	switch upper {
	case "SQLITE_SEQUENCE":
		return strings.Contains(entry.SQL, "seq INTEGER")
	case "SQLITE_SCHEMA", "SQLITE_MASTER", "SQLITE_TEMP_SCHEMA", "SQLITE_TEMP_MASTER":
		return strings.Contains(entry.SQL, "rootpage INTEGER")
	}
	return strings.HasPrefix(upper, "PRAGMA_")
}

// createTableSQL returns the SQL text to store in sqlite_schema for a table.
// The original statement text is preferred (matching SQLite's verbatim
// storage); the AST serialization is only a fallback when raw text is absent.
// SQLite strips the TEMP/TEMPORARY keyword from the stored text (a temp table
// is stored as "CREATE TABLE t(...)" in sqlite_temp_schema).
func (e *DDLExecutor) createTableSQL(s *sql.CreateTableStmt) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		return stripIfNotExists(stripCreateTempKeyword(strings.TrimSpace(s.RawSQL)))
	}
	return e.buildCreateTableSQL(s)
}

// ensureSQLiteSequenceTable creates a real sqlite_sequence(name,seq) table in
// the given database context if none exists. SQLite creates it lazily when the
// first AUTOINCREMENT table is created (build.c:2922-2931). The schema manager
// prefers a real sqlite_sequence entry over its synthetic fallback, so once
// this table exists all sqlite_sequence queries use the normal table
// machinery. A user-created sqlite_sequence (via PRAGMA writable_schema) is
// left untouched.
func (e *DDLExecutor) ensureSQLiteSequenceTable(ctx *DatabaseContext) error {
	entries, err := ctx.Schema.GetEntries(schema.TypeTable)
	if err == nil {
		for _, ent := range entries {
			if strings.EqualFold(ent.Name, "sqlite_sequence") || strings.EqualFold(ent.TblName, "sqlite_sequence") {
				// sqlite_sequence must be an ordinary rowid table: a WITHOUT
				// ROWID impostor (planted via writable_schema, autoinc-12.x)
				// makes the sequence btree unreadable as a table — SQLite
				// reports SQLITE_CORRUPT, "database disk image is malformed",
				// at the next AUTOINCREMENT sequence open.
				if strings.Contains(strings.ToUpper(ent.SQL), "WITHOUT ROWID") {
					return fmt.Errorf("database disk image is malformed")
				}
				return nil // already exists (real or user-created)
			}
		}
	}
	pg, perr := allocateRootPage(ctx.Pager)
	if perr != nil {
		return perr
	}
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	// Empty-leaf header (zeroPage parity): the cell content area is the
	// USABLE size (put2byte(&data[hdr+5], pBt->usableSize),
	// src/btree.c:2189) — the same fix as the CREATE TABLE root above.
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(ctx.Pager.UsableSize()))
	if err := ctx.Pager.WritePage(pg); err != nil {
		return err
	}
	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     "sqlite_sequence",
		TblName:  "sqlite_sequence",
		RootPage: pg.PageNum,
		SQL:      "CREATE TABLE sqlite_sequence(name,seq)",
	}
	return ctx.Schema.AddEntry(entry)
}

// hasAutoIncrementColumn reports whether a CREATE TABLE statement declares at
// least one AUTOINCREMENT column (an INTEGER PRIMARY KEY AUTOINCREMENT).
func hasAutoIncrementColumn(s *sql.CreateTableStmt) bool {
	for _, cd := range s.Columns {
		if cd.AutoInc {
			return true
		}
	}
	return false
}

// stripCreateTempKeyword removes a leading CREATE TEMP [TABLE] / CREATE
// TEMPORARY [TABLE] keyword from stored schema SQL, matching SQLite which
// records temp objects as "CREATE TABLE ..." (without TEMP) in
// sqlite_temp_schema. It also strips a schema prefix from the table name
// ("CREATE TABLE aux.t1(...)" is stored as "CREATE TABLE t1(...)", matching
// SQLite's sqlite_schema storage).
func stripCreateTempKeyword(sqlStr string) string {
	upper := strings.ToUpper(sqlStr)
	tableIdx := -1
	switch {
	case strings.HasPrefix(upper, "CREATE TEMP VIRTUAL TABLE"), strings.HasPrefix(upper, "CREATE TEMPORARY VIRTUAL TABLE"):
		// Find the start of "VIRTUAL TABLE".
		rest := sqlStr[len("CREATE "):]
		idx := strings.Index(strings.ToUpper(rest), "VIRTUAL")
		if idx >= 0 {
			sqlStr = "CREATE " + rest[idx:]
			tableIdx = len("CREATE VIRTUAL TABLE ")
		}
	case strings.HasPrefix(upper, "CREATE VIRTUAL TABLE "):
		tableIdx = len("CREATE VIRTUAL TABLE ")
	case strings.HasPrefix(upper, "CREATE TEMP TABLE"), strings.HasPrefix(upper, "CREATE TEMPORARY TABLE"):
		// Find the start of "TABLE".
		rest := sqlStr[len("CREATE "):]
		idx := strings.Index(strings.ToUpper(rest), "TABLE")
		if idx >= 0 {
			sqlStr = "CREATE " + rest[idx:]
			tableIdx = len("CREATE TABLE ")
		}
	case strings.HasPrefix(upper, "CREATE TABLE "):
		tableIdx = len("CREATE TABLE ")
	}
	if tableIdx >= 0 && tableIdx < len(sqlStr) {
		sqlStr = stripSchemaPrefixFromTableName(sqlStr, tableIdx)
	}
	return sqlStr
}

// stripSchemaPrefixFromTableName removes a schema prefix (main.t) from the
// table name at the given offset in a CREATE TABLE statement.
func stripSchemaPrefixFromTableName(sqlStr string, tableIdx int) string {
	// The table name is the next token (up to whitespace or '(').
	nameStart := tableIdx
	nameEnd := nameStart
	for nameEnd < len(sqlStr) && sqlStr[nameEnd] != ' ' && sqlStr[nameEnd] != '\t' && sqlStr[nameEnd] != '(' && sqlStr[nameEnd] != '\n' && sqlStr[nameEnd] != '\r' {
		nameEnd++
	}
	name := sqlStr[nameStart:nameEnd]
	if dot := strings.Index(name, "."); dot >= 0 {
		return sqlStr[:nameStart] + name[dot+1:] + sqlStr[nameEnd:]
	}
	return sqlStr
}

// defaultContainsNonConstant reports whether a DEFAULT expression contains
// bound-parameter or RAISE() nodes, which make it non-constant. SQLite rejects
// such DEFAULTs at CREATE TABLE time with "default value of column [x] is not
// constant" (build.c: sqlite3AddDefaultValue).
func (e *DDLExecutor) buildCreateTableSQL(s *sql.CreateTableStmt) string {
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	// The table name must re-parse: a non-plain identifier (e.g. the quoted
	// '%ss%' of CREATE TEMP TABLE '%ss%' AS SELECT) is stored quoted, the way
	// sqlite3EndTable renders it (e_select2-2.x: an unquoted %ss% in the
	// stored schema text fails the schema re-parse with 'near "%": syntax
	// error'). A schema prefix is kept and its tail quoted when needed.
	buf.WriteString(quotedStoredTableName(s.Name))
	buf.WriteString("(")
	for i, col := range s.Columns {
		if i > 0 {
			// SQLite stores derived (CREATE TABLE ... AS SELECT) column lists
			// without a space after the comma: CREATE TABLE x1(m,n).
			buf.WriteString(",")
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
	if s.Strict {
		buf.WriteString(", STRICT")
	}
	return buf.String()
}

// quotedStoredTableName renders a table name for stored schema SQL: a plain
// identifier is kept as-is; anything else (spaces, %, embedded quotes, a
// leading digit) is double-quoted with embedded quotes doubled so the stored
// CREATE text re-parses. A "schema.table" prefix keeps its qualifier and
// quotes the tail.
func quotedStoredTableName(name string) string {
	if dot := strings.Index(name, "."); dot >= 0 {
		prefix, tail := name[:dot+1], name[dot+1:]
		return prefix + quoteIdentIfKeyword(tail)
	}
	return quoteIdentIfKeyword(name)
}

// quoteIdentIfKeyword double-quotes an identifier when it is a SQL keyword
// that the parser would otherwise reject in column position (e.g. a column
// named "notnull" — the derived CREATE TABLE ... AS SELECT of
// pragma_table_info's notnull column). Plain identifiers are returned as-is.
func quoteIdentIfKeyword(name string) string {
	// Any name that is not a plain SQL identifier (starts with a letter or
	// underscore and contains only letters/digits/underscores) must be
	// quoted — e.g. expression-derived column names like "a+b" from
	// CREATE TABLE ... AS SELECT a+b. An unquoted "a + b" would re-parse
	// as three tokens and corrupt the derived schema.
	if !isPlainIdentifier(name) {
		return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
	}
	switch strings.ToUpper(name) {
	case "NOTNULL", "NULL", "PRIMARY", "UNIQUE", "CHECK", "DEFAULT", "REFERENCES",
		"COLLATE", "CONSTRAINT", "GENERATED", "AUTOINCREMENT", "ON", "KEY",
		"ORDER", "GROUP", "BY", "INDEX", "TABLE", "SELECT", "INSERT", "UPDATE",
		"DELETE", "CREATE", "DROP", "FROM", "WHERE", "JOIN", "LEFT", "RIGHT",
		"INNER", "OUTER", "FULL", "CROSS", "NATURAL", "AS", "AND", "OR", "NOT",
		"LIKE", "GLOB", "IS", "IN", "BETWEEN", "CASE", "WHEN", "THEN", "ELSE",
		"END", "CAST", "VALUES", "SET", "TO", "WITH", "UNION", "ALL", "EXCEPT",
		"INTERSECT", "DISTINCT", "LIMIT", "OFFSET", "HAVING", "ASC", "DESC",
		"IF", "EXISTS", "TEMP", "TEMPORARY", "VIEW", "TRIGGER", "BEFORE", "AFTER",
		"INSTEAD", "OF", "EACH", "ROW", "BEGIN", "COMMIT", "ROLLBACK", "TRANSACTION",
		"INTEGER", "REAL", "TEXT", "BLOB", "ANY", "INT", "VARCHAR", "FOREIGN", "RECURSIVE":
		return "\"" + name + "\""
	}
	return name
}

func formatColumnDef(buf *strings.Builder, col sql.ColumnDef) {
	if col.Dropped {
		return
	}
	buf.WriteString(quoteIdentIfKeyword(col.Name))
	if col.Type != "" {
		buf.WriteString(" ")
		buf.WriteString(col.Type)
	}
	writeColumnGenerated(buf, col)
	writeColumnConstraints(buf, col)
}

// writeColumnGenerated appends the GENERATED ALWAYS AS (...) clause.
func writeColumnGenerated(buf *strings.Builder, col sql.ColumnDef) {
	if col.Generated != nil {
		buf.WriteString(" GENERATED ALWAYS AS (")
		buf.WriteString(sql.ExprString(col.Generated))
		buf.WriteString(")")
	}
}

// writeColumnConstraints appends the COLLATE / CONSTRAINT / CHECK / NOT NULL /
// UNIQUE / PRIMARY KEY / AUTOINCREMENT / DEFAULT / REFERENCES clauses.
func writeColumnConstraints(buf *strings.Builder, col sql.ColumnDef) {
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
		buf.WriteString(" DEFAULT (")
		buf.WriteString(sql.ExprString(col.Default))
		buf.WriteString(")")
	}
	if col.References != "" {
		buf.WriteString(" REFERENCES ")
		buf.WriteString(col.References)
	}
}

// stripTriggerTempKeyword removes a leading CREATE TEMP [TRIGGER] / CREATE
// TEMPORARY [TRIGGER] keyword from stored trigger SQL, matching SQLite which
// records temp triggers as "CREATE TRIGGER ..." (without TEMP) in
// sqlite_temp_schema.
func stripTriggerTempKeyword(sqlStr string) string {
	upper := strings.ToUpper(sqlStr)
	if strings.HasPrefix(upper, "CREATE TEMP TRIGGER") || strings.HasPrefix(upper, "CREATE TEMPORARY TRIGGER") {
		// Find the start of "TRIGGER".
		rest := sqlStr[len("CREATE "):]
		idx := strings.Index(strings.ToUpper(rest), "TRIGGER")
		if idx >= 0 {
			return "CREATE " + rest[idx:]
		}
	}
	return sqlStr
}

// validateCheckExprColumns resolves the column references of every CHECK
// constraint (column-level and table-level) against the new table's columns
// and rejects bound parameters (build.c sqlite3AddCheckConstraint /
// sqlite3ExprCodeTarget on the check expression): check-3.3's CHECK(q<x)
// reports "no such column: q"; check-5.1/5.2 report "parameters prohibited
// in CHECK constraints".
func (e *DDLExecutor) validateCheckExprColumns(s *sql.CreateTableStmt) *Result {
	for _, expr := range createTableChecks(s) {
		if res := e.validateCheckExprNodes(s, expr); res != nil {
			return res
		}
	}
	return nil
}

// createTableChecks collects the CHECK expressions of a CREATE TABLE
// statement: column-level checks in column order, then table-level
// ConstraintCheck entries in list order.
func createTableChecks(s *sql.CreateTableStmt) []sql.Expr {
	checks := []sql.Expr{}
	for i := range s.Columns {
		if s.Columns[i].Check != nil {
			checks = append(checks, s.Columns[i].Check)
		}
	}
	for i := range s.Constraints {
		if s.Constraints[i].Type == sql.ConstraintCheck && s.Constraints[i].Expr != nil {
			checks = append(checks, s.Constraints[i].Expr)
		}
	}
	return checks
}

// validateCheckExprNodes walks one CHECK expression and reports the first
// prohibited construct: bound parameters ("parameters prohibited in CHECK
// constraints") and unresolvable bare column references ("no such column").
func (e *DDLExecutor) validateCheckExprNodes(s *sql.CreateTableStmt, expr sql.Expr) *Result {
	var res *Result
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if res != nil {
			return
		}
		switch v := n.(type) {
		case *sql.ParameterExpr:
			res = &Result{Error: fmt.Errorf("parameters prohibited in CHECK constraints")}
		case *sql.ColumnRef:
			// Double-quoted tokens keep the DQS string fallback inside
			// CHECK expressions ("integer" in check-2.1), so only bare
			// identifiers must resolve (check-3.3's bare q). A
			// foreign-qualified reference (t2.x inside t3's CHECK)
			// reports the qualified spelling (check-3.5).
			if !v.Quoted && !createTableRefResolves(s, tableNameOf(s), v) {
				res = &Result{Error: fmt.Errorf("no such column: %s", checkRefText(v))}
			}
		}
	})
	return res
}

// createTableRefResolves reports whether a column reference inside a CHECK
// constraint resolves against the table being created. The qualifier chain
// (db.table or schema.table, e.g. "main.t810.a", "xyzzy.t811.b") resolves
// when its LAST segment matches the table's own name; "rowid"/"oid"/
// "_rowid_" always resolve (check-8.1, check-9.1).
func createTableRefResolves(s *sql.CreateTableStmt, tableName string, ref *sql.ColumnRef) bool {
	switch strings.ToLower(ref.Name) {
	case "rowid", "oid", "_rowid_":
		return true
	}
	if ref.Table != "" {
		qual := ref.Table
		if i := strings.LastIndex(qual, "."); i >= 0 {
			qual = qual[i+1:]
		}
		if !strings.EqualFold(qual, tableName) {
			return false
		}
	}
	for i := range s.Columns {
		if strings.EqualFold(s.Columns[i].Name, ref.Name) {
			return true
		}
	}
	return false
}

func tableNameOf(s *sql.CreateTableStmt) string { return s.Name }

// checkRefText renders a column reference as written (qualifier chain
// included) for "no such column" messages inside CHECK constraints.
func checkRefText(ref *sql.ColumnRef) string {
	if ref.Table != "" {
		return ref.Table + "." + ref.Name
	}
	return ref.Name
}
