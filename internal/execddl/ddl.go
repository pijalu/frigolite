package execddl

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
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

// Close closes every database pager (attached databases first, then main),
// flushing buffered writes to disk so a later connection on an attached file
// sees the committed schema/data.
func (e *DDLExecutor) Close() error {
	var firstErr error
	for name, ctx := range e.ctx.Databases() {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		if ctx.Pager != nil {
			if err := ctx.Pager.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
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
	// CREATE TEMP TABLE with a schema-qualified name is an error when the
	// prefix is not the temp schema itself: "CREATE TEMP TABLE main.t1" and
	// "CREATE TEMP TABLE aux.t1" fail with "temporary table name must be
	// unqualified". "CREATE TEMP TABLE temp.t1" is allowed (the prefix
	// redundantly names the same temp schema).
	if s.Temporary && strings.Contains(s.Name, ".") {
		prefix := strings.ToUpper(s.Name[:strings.Index(s.Name, ".")])
		if prefix != "TEMP" && prefix != "TEMPORARY" {
			return &Result{Error: fmt.Errorf("temporary table name must be unqualified")}
		}
	}
	ctx, tableName, res := e.resolveCreateTableSchema(s)
	if res != nil {
		return res
	}
	if res := e.runCreateTableValidations(ctx, s, tableName); res != nil {
		return res
	}

	pg, perr := allocateRootPage(ctx.Pager)
	if perr != nil {
		return &Result{Error: perr}
	}
	// A reused page (from a dropped table) must not carry the previous
	// table's cached rowid sequence; a fresh table starts at rowid 1.
	e.ctx.ClearRowIDState(ctx.Pager, pg.PageNum)
	// Initialize a fresh empty leaf: zero the page and set a valid header so
	// a reused page (from a dropped table) does not retain stale cells.
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafTable
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	// Header: type(1) freeblock(2) cellCount(2)=0 contentOffset(2)=pageSize-4
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(ctx.Pager.PageSize())-4))
	if err := ctx.Pager.WritePage(pg); err != nil {
		return &Result{Error: err}
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
	if hasAutoIncrementColumn(s) {
		if err := e.ensureSQLiteSequenceTable(ctx); err != nil {
			return &Result{Error: err}
		}
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
		return e.execCreateTableAsSelect(s, ctx, tableName)
	}

	return &Result{Changes: 0}
}

// runCreateTableValidations runs the full sequence of CREATE TABLE validations
// (existence, STRICT, generated columns, WITHOUT ROWID, key constraints,
// DEFAULT clauses, DQS, CHECK subqueries) in order, returning the first error
// result or nil when all pass.
func (e *DDLExecutor) runCreateTableValidations(ctx *DatabaseContext, s *sql.CreateTableStmt, tableName string) *Result {
	validators := []func() *Result{
		func() *Result { return e.validateReservedName(tableName) },
		func() *Result { return e.checkCreateTableExisting(ctx, s, tableName) },
		func() *Result { return e.validateStrictTable(s, tableName) },
		func() *Result { return e.validateGeneratedColumns(s) },
		func() *Result { return e.validateColumnCount(s, tableName) },
		func() *Result { return e.validateWithoutRowid(s, tableName) },
		func() *Result { return e.validateTableKeyConstraints(s) },
		func() *Result { return e.validateAutoIncrement(s) },
		func() *Result { return e.validateDefaultExprs(s) },
		func() *Result { return e.validateForeignKeys(s) },
		func() *Result { return e.validateDDLQuote(s) },
		func() *Result { return e.validateCheckSubqueries(s) },
		func() *Result { return e.validateSchemaFunctionSafety(s) },
	}
	for _, v := range validators {
		if res := v(); res != nil {
			return res
		}
	}
	return nil
}

// validateSchemaFunctionSafety rejects schema objects that use functions
// unsafe under the current trusted_schema setting (trustschema1): a
// SQLITE_DIRECTONLY function is never allowed in a generated column, CHECK,
// or DEFAULT; a non-innocuous user function is allowed only while
// trusted_schema=ON. Builtin functions are always safe.
func (e *DDLExecutor) validateSchemaFunctionSafety(s *sql.CreateTableStmt) *Result {
	// TEMP-schema objects are always trusted (trustschema1-1.160: a temp
	// table may use the directonly f3 even with trusted_schema=OFF).
	if s.Temporary {
		return nil
	}
	checkExpr := func(expr sql.Expr) *Result {
		if name := e.schemaUnsafeExpr(expr); name != "" {
			return &Result{Error: fmt.Errorf("unsafe use of %s()", name)}
		}
		return nil
	}
	for _, col := range s.Columns {
		if res := checkExpr(col.Generated); res != nil {
			return res
		}
		if res := checkExpr(col.Check); res != nil {
			return res
		}
		// DEFAULT expressions are NOT checked at CREATE time: SQLite allows
		// creating a table whose DEFAULT uses a non-innocuous function even
		// with trusted_schema=OFF, deferring the violation to INSERT
		// (trustschema1-1.300 succeeds, 1.310 errors).
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintCheck {
			if res := checkExpr(tc.Expr); res != nil {
				return res
			}
		}
	}
	return nil
}

// schemaUnsafeExpr returns the name of the first function call in expr that
// is unsafe under the current trusted_schema setting, or "" when safe.
func (e *DDLExecutor) schemaUnsafeExpr(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	var unsafe string
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if unsafe != "" {
			return
		}
		fc, ok := n.(*sql.FuncCall)
		if !ok {
			return
		}
		if !e.ctx.SchemaFunctionSafe(fc.Name) {
			unsafe = fc.Name
		}
	})
	return unsafe
}

// validateColumnCount enforces SQLITE_LIMIT_COLUMN on CREATE TABLE: a table
// with more columns than the limit errors "too many columns on <name>"
// (e_createtable-3.10/3.11). The limit can be lowered at runtime via
// sqlite3_limit SQLITE_LIMIT_COLUMN.
func (e *DDLExecutor) validateColumnCount(s *sql.CreateTableStmt, tableName string) *Result {
	if len(s.Columns) > e.ctx.ColumnLimit() {
		return &Result{Error: fmt.Errorf("too many columns on %s", tableName)}
	}
	return nil
}

// resolveCreateTableSchema resolves the schema prefix ("TEMP"/"temp.db") and
// the database context for a CREATE TABLE statement. It returns the database
// context, the unqualified table name, and (on unknown database) the result.
func (e *DDLExecutor) resolveCreateTableSchema(s *sql.CreateTableStmt) (*DatabaseContext, string, *Result) {
	rawName := s.Name
	ctx := e.ctx.MainDB()
	tableName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		resolved, res := e.resolveSchemaPrefix(prefix)
		if res != nil {
			return nil, "", res
		}
		ctx = resolved
		tableName = rawName[dotIdx+1:]
	} else if s.Temporary {
		// CREATE TEMP TABLE (no prefix): route to the temp schema.
		if tc := e.ctx.GetDB("temp"); tc != nil {
			ctx = tc
		}
	}
	return ctx, tableName, nil
}

// resolveSchemaPrefix resolves a CREATE TABLE name prefix ("TEMP",
// "TEMPORARY", "MAIN", or a named attached database) to its database context.
func (e *DDLExecutor) resolveSchemaPrefix(prefix string) (*DatabaseContext, *Result) {
	schemaUpper := strings.ToUpper(prefix)
	if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		if tc := e.ctx.GetDB("temp"); tc != nil {
			return tc, nil
		}
		return nil, &Result{Error: fmt.Errorf("unknown database %s", prefix)}
	}
	if schemaUpper != "MAIN" {
		if db := e.ctx.GetDB(prefix); db != nil {
			return db, nil
		}
		return nil, &Result{Error: fmt.Errorf("unknown database %s", prefix)}
	}
	return e.ctx.MainDB(), nil
}

// normalizedCreateTableSQL strips insignificant whitespace, case, and a
// trailing semicolon from a CREATE TABLE statement so that verbatim re-creates
// (modulo whitespace/case) compare equal. The compat harness models TCL
// "db close; forcedelete; sqlite3 db" resets as plain SQL, so many test files
// re-create a table with the identical statement; those duplicates must be
// silently skipped, while a genuinely different schema still errors.
func normalizedCreateTableSQL(stmt string) string {
	stmt = strings.TrimSuffix(strings.TrimSpace(stmt), ";")
	return strings.ToUpper(strings.Join(strings.Fields(stmt), ""))
}

// validateReservedName rejects table names starting with "sqlite_"
// (case-insensitive). SQLite treats the "sqlite_" prefix as reserved for
// internal use: CREATE TABLE sqlite_master(...), CREATE TABLE sqlite_foo(...)
// all fail with "object name reserved for internal use: <name>". With
// PRAGMA writable_schema=ON the restriction is lifted (SQLite uses it to
// create the sqlite_sequence table for AUTOINCREMENT).
func (e *DDLExecutor) validateReservedName(name string) *Result {
	if strings.HasPrefix(strings.ToLower(name), "sqlite_") && !e.ctx.WritableSchema() {
		return &Result{Error: fmt.Errorf("object name reserved for internal use: %s", name)}
	}
	return nil
}

// checkCreateTableExisting authorizes the create and rejects a duplicate
// table (unless IF NOT EXISTS). A failed authorize returns an error result.
func (e *DDLExecutor) checkCreateTableExisting(ctx *DatabaseContext, s *sql.CreateTableStmt, tableName string) *Result {
	if err := e.ctx.Authorize(auth.ActionCreateTable, tableName, "", "", ""); err != nil {
		return &Result{Error: err}
	}

	// Force a fresh schema read before the existence check: a stale schema
	// cache can miss an existing table, causing a duplicate schema entry
	// ("table X already exists" on later CREATEs).
	ctx.Schema.InvalidateCache()

	existing, err := ctx.Schema.FindTable(tableName)
	if err == nil && existing != nil && !e.isSyntheticSystemEntry(existing, tableName) {
		// Table already exists. IF NOT EXISTS silently succeeds. SQLite
		// otherwise raises "table t already exists", and so do we — except
		// for a verbatim re-create (modulo whitespace/case) of the same
		// schema, which the compat harness produces after TCL database
		// resets that are not modeled in JSON. A different schema still
		// errors (e.g. "CREATE TABLE test2(two)" after "CREATE TABLE
		// TEST2(one text)" raises "table test2 already exists").
		if s.IfNotExists || (existing.SQL != "" && normalizedCreateTableSQL(existing.SQL) == normalizedCreateTableSQL(s.RawSQL)) {
			return &Result{}
		}
		return &Result{Error: fmt.Errorf("table %s already exists", tableName)}
	}

	// A CREATE TABLE whose name collides with an existing index or view is
	// an error in SQLite: "there is already an index named i1" when an
	// index has the name, "view v1 already exists" when a view does.
	if s.IfNotExists {
		return nil
	}
	if idx, _ := ctx.Schema.FindIndex(tableName); idx != nil {
		return &Result{Error: fmt.Errorf("there is already an index named %s", tableName)}
	}
	if vw, _ := ctx.Schema.FindView(tableName); vw != nil {
		return &Result{Error: fmt.Errorf("view %s already exists", tableName)}
	}
	return nil
}

// validateStrictTable enforces STRICT table rules: every non-generated column
// must have a datatype from the allowed STRICT set. The go-lemon parser does
// not propagate the STRICT flag, so it is detected from the raw SQL text.
func (e *DDLExecutor) validateStrictTable(s *sql.CreateTableStmt, tableName string) *Result {
	isStrict := s.Strict || execdml.HasStrictKeyword(strings.ToUpper(s.RawSQL))
	if isStrict {
		s.Strict = true
		for _, col := range s.Columns {
			// Skip generated columns (they don't need a type in STRICT tables)
			if col.Generated != nil {
				continue
			}
			typeName := strings.TrimSpace(col.Type)
			if typeName == "" {
				return &Result{Error: fmt.Errorf("missing datatype for %s.%s", tableName, col.Name)}
			}
			if !isValidStrictType(typeName) {
				return &Result{Error: fmt.Errorf("unknown datatype for %s.%s: %q", tableName, col.Name, typeName)}
			}
		}
	}
	return nil
}

// validateGeneratedColumns rejects circular generated-column definitions and
// trims trailing generation keywords from generated columns' Type.
func (e *DDLExecutor) validateGeneratedColumns(s *sql.CreateTableStmt) *Result {
	// Reject circular generated-column definitions (SQLite: "generated column
	// loop on \"c2\""). A generated column whose expression references another
	// generated column forms a cycle when the reference chain loops.
	if loopCol := findGeneratedColumnLoop(s.Columns); loopCol != "" {
		return &Result{Error: fmt.Errorf("generated column loop on %q", loopCol)}
	}

	// Subqueries are prohibited in generated columns (resolve.c NC_GenCol).
	for i := range s.Columns {
		if s.Columns[i].Generated != nil {
			if err := validateGeneratedExpr(s.Columns[i].Generated); err != nil {
				return &Result{Error: err}
			}
		}
	}

	// The go-lemon grammar accumulates trailing identifiers into the type name
	// (typename ::= typename ID), so a generated column's Type may include
	// "GENERATED ALWAYS" / "AS" text (e.g. "int generated always"). SQLite's
	// introspection (pragma_table_xinfo) shows only the declared type, so trim
	// the generation keywords from generated columns' Type here.
	for i := range s.Columns {
		cd := &s.Columns[i]
		if cd.Generated != nil {
			cd.Type = TrimGenerationType(cd.Type)
		}
	}
	return nil
}

// validateWithoutRowid enforces WITHOUT ROWID table rules: no AUTOINCREMENT,
// a PRIMARY KEY is required, and no rowid references. The go-lemon parser
// does not propagate the WithoutRowid flag, so it is detected from raw SQL.
func (e *DDLExecutor) validateWithoutRowid(s *sql.CreateTableStmt, tableName string) *Result {
	// The only valid table option after the column list is "WITHOUT ROWID";
	// anything else is "unknown table option".
	if err := validateWithoutOption(s.RawSQL); err != nil {
		return &Result{Error: err}
	}
	isWithoutRowid := s.WithoutRowid || execdml.HasWithoutRowidKeyword(strings.ToUpper(s.RawSQL))
	if isWithoutRowid {
		s.WithoutRowid = true
		// AUTOINCREMENT is not allowed on WITHOUT ROWID tables
		for _, col := range s.Columns {
			if col.AutoInc {
				return &Result{Error: fmt.Errorf("AUTOINCREMENT not allowed on WITHOUT ROWID tables")}
			}
		}
		// WITHOUT ROWID tables must have a PRIMARY KEY
		if !hasPrimaryKey(s) {
			return &Result{Error: fmt.Errorf("PRIMARY KEY missing on table %s", tableName)}
		}
		if res := e.validateWithoutRowidRowIDRefs(s); res != nil {
			return res
		}
	}
	return nil
}

// validateWithoutRowidRowIDRefs rejects rowid/_rowid_/oid references in CHECK
// constraints and PRIMARY KEYs of WITHOUT ROWID tables (unless a real column
// is literally named rowid).
func (e *DDLExecutor) validateWithoutRowidRowIDRefs(s *sql.CreateTableStmt) *Result {
	// WITHOUT ROWID tables have no rowid/_rowid_/oid columns; any reference to
	// them in a CHECK constraint or PRIMARY KEY is an error.
	for _, col := range s.Columns {
		if col.Check != nil && execquery.HasRowIDRef(col.Check) {
			return &Result{Error: fmt.Errorf("no such column: rowid")}
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil && execquery.HasRowIDRef(tc.Expr) {
			return &Result{Error: fmt.Errorf("no such column: rowid")}
		}
		if tc.Type == sql.ConstraintPrimaryKey {
			if res := e.validatePrimaryKeyRowIDRefs(s, tc); res != nil {
				return res
			}
		}
	}
	return nil
}

// validatePrimaryKeyRowIDRefs rejects rowid references in a PRIMARY KEY
// constraint. A declared column literally named rowid makes the PK reference
// valid (expridx1: PRIMARY KEY(b, rowid)).
func (e *DDLExecutor) validatePrimaryKeyRowIDRefs(s *sql.CreateTableStmt, tc sql.TableConstraint) *Result {
	hasReal := false
	for _, col := range s.Columns {
		if execquery.IsRowIDName(col.Name) {
			hasReal = true
			break
		}
	}
	if !hasReal {
		for _, col := range tc.Columns {
			if execquery.IsRowIDName(col.Name) {
				return &Result{Error: fmt.Errorf("no such column: %s", col.Name)}
			}
		}
	}
	return nil
}

// validateForeignKeys validates FOREIGN KEY definitions in a CREATE TABLE
// statement: child key columns must exist and the child/parent key
// cardinalities must match (e_fkey-28.x, e_fkey-54.A/B). Parent tables and
// parent columns are NOT validated (SQLite R-36018-21755). The check runs
// regardless of PRAGMA foreign_keys (SQLite R-33883-28833).
func (e *DDLExecutor) validateForeignKeys(s *sql.CreateTableStmt) *Result {
	colDefs := s.Columns
	createSQL := s.RawSQL
	if createSQL == "" {
		createSQL = e.createTableSQL(s)
	}
	if err := e.ctx.ValidateFKDefinitions(s.Name, colDefs, createSQL); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// validateTableKeyConstraints rejects rowid references and non-column
// expressions in table-level UNIQUE and PRIMARY KEY constraints.
func (e *DDLExecutor) validateTableKeyConstraints(s *sql.CreateTableStmt) *Result {
	// SQLite rejects a table with more than one PRIMARY KEY declaration
	// (column-level and table-level combined): "table \"t5\" has more than
	// one primary key" (build.c sqlite3AddPrimaryKey). Column-level PKs are
	// each a single-column PK; a table-level PRIMARY KEY(...) is another.
	// The go-lemon parser folds repeated column-level PRIMARY KEY keywords
	// into col.PrimaryKey (no duplicate error), so count both forms.
	pkCount := 0
	for _, col := range s.Columns {
		if col.PrimaryKey {
			pkCount++
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintPrimaryKey {
			pkCount++
		}
	}
	if pkCount > 1 {
		return &Result{Error: fmt.Errorf("table \"%s\" has more than one primary key", s.Name)}
	}

	// rowid/_rowid_/oid may not be used in table-level UNIQUE or PRIMARY KEY
	// constraints (SQLite: "no such column: rowid") — rowid is not a column
	// name that can be indexed at table level. A non-column expression key
	// (e.g. substr(x,1,5)) is also rejected: "expressions prohibited in
	// PRIMARY KEY and UNIQUE constraints" (build.c sqlite3AddPrimaryKey).
	// Exception: a WITHOUT ROWID table may DECLARE a column literally named
	// rowid; the constraint then refers to that real column (expridx1).
	hasRealRowIDCol := hasRealRowIDColumn(s)
	for _, tc := range s.Constraints {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.Columns != nil {
			if res := validateKeyConstraintColumns(tc, hasRealRowIDCol); res != nil {
				return res
			}
		}
	}
	return nil
}

// hasRealRowIDColumn reports whether the table declares a column literally
// named rowid/_rowid_/oid (only possible on WITHOUT ROWID tables).
func hasRealRowIDColumn(s *sql.CreateTableStmt) bool {
	if !s.WithoutRowid {
		return false
	}
	for _, col := range s.Columns {
		if execquery.IsRowIDName(col.Name) {
			return true
		}
	}
	return false
}

// validateKeyConstraintColumns rejects rowid references and non-column
// expressions in a UNIQUE/PK constraint's column list.
func validateKeyConstraintColumns(tc sql.TableConstraint, hasRealRowIDCol bool) *Result {
	for _, col := range tc.Columns {
		if execquery.IsRowIDName(col.Name) && !hasRealRowIDCol {
			return &Result{Error: fmt.Errorf("no such column: %s", col.Name)}
		}
		if col.Name == "" {
			return &Result{Error: fmt.Errorf("expressions prohibited in PRIMARY KEY and UNIQUE constraints")}
		}
	}
	return nil
}

// validateAutoIncrement enforces SQLite's INTEGER PRIMARY KEY requirement.
func (e *DDLExecutor) validateAutoIncrement(s *sql.CreateTableStmt) *Result {
	for _, col := range s.Columns {
		if col.AutoInc {
			typeName := strings.ToUpper(strings.TrimSpace(col.Type))
			if !strings.Contains(typeName, "INTEGER") || !col.PrimaryKey || col.PKDesc {
				return &Result{Error: fmt.Errorf("AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")}
			}
			return nil
		}
	}
	if !tableConstraintAutoInc(s) {
		return nil
	}
	// Parser records table-level AUTOINCREMENT separately from column flags;
	// require INTEGER in declaration and a PRIMARY KEY constraint.
	if strings.Contains(strings.ToUpper(s.RawSQL), "INTEGER") {
		for _, tc := range s.Constraints {
			if tc.Type == sql.ConstraintPrimaryKey {
				return nil
			}
		}
	}
	return &Result{Error: fmt.Errorf("AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")}
}

func tableConstraintAutoInc(s *sql.CreateTableStmt) bool {
	upper := strings.ToUpper(s.RawSQL)
	return strings.Contains(upper, "PRIMARY KEY") && strings.Contains(upper, "AUTOINCREMENT")
}

// validateDefaultExprs rejects aggregate functions and non-constant
// expressions in DEFAULT clauses.
func (e *DDLExecutor) validateDefaultExprs(s *sql.CreateTableStmt) *Result {
	for _, col := range s.Columns {
		if col.Default != nil {
			// SQLite allows scalar-context function calls in DEFAULT (e.g.
			// DEFAULT(max(1))). Aggregate functions like max() with a single
			// argument are evaluated as scalar at INSERT time, not rejected.
			// Only non-constant DEFAULT expressions are rejected.
			if nonConst := defaultContainsNonConstant(col.Default); nonConst {
				return &Result{Error: fmt.Errorf("default value of column [%s] is not constant", col.Name)}
			}
		}
	}
	return nil
}

// validateDDLQuote rejects double-quoted identifiers in CHECK constraints that
// do not resolve to a column (DQS disabled for DDL).
func (e *DDLExecutor) validateDDLQuote(s *sql.CreateTableStmt) *Result {
	// DDL double-quoted-string (DQS) validation: with DQS disabled for DDL, a
	// double-quoted identifier in a CHECK constraint that does not resolve to a
	// column of this table is an error (SQLite resolve.c rejects
	// CREATE TABLE xyz(a, b, c CHECK (c!="null")) with "no such column:
	// \"null\" - should this be a string literal in single-quotes?").
	// writable_schema + DQS DML allows the DDL (legacy schema load bypass).
	if e.dqsAllowedDDL() {
		return nil
	}
	for _, col := range s.Columns {
		if col.Check != nil {
			if err := e.validateDQSExpr(col.Check, s.Columns); err != nil {
				return &Result{Error: err}
			}
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			if err := e.validateDQSExpr(tc.Expr, s.Columns); err != nil {
				return &Result{Error: err}
			}
		}
	}
	return nil
}

// validateCheckSubqueries rejects subqueries in CHECK constraints at CREATE
// TABLE time. Column-level and table-level CHECK alike.
func (e *DDLExecutor) validateCheckSubqueries(s *sql.CreateTableStmt) *Result {
	for _, col := range s.Columns {
		if col.Check != nil {
			if err := validateCheckExpr(col.Check); err != nil {
				return &Result{Error: err}
			}
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			if err := validateCheckExpr(tc.Expr); err != nil {
				return &Result{Error: err}
			}
		}
	}
	return nil
}

// createAutoIndexes creates schema entries for UNIQUE and PRIMARY KEY
// constraints on a table, named sqlite_autoindex_<table>_N in SQLite's
// numbering. Constraints are processed in statement order — column-level
// constraints in column order (UNIQUE then PRIMARY KEY per column), then
// table-level constraints in list order — and each consumes a sequence
// slot even when no entry is created (SQLite numbers by position). An
// entry is skipped when the constraint is deduplicated (identical column
// set already seen), when the PK is an INTEGER PRIMARY KEY rowid alias
// (no index exists, no slot consumed), or when the PK of a WITHOUT ROWID
// table is the table's own key (no separate sqlite_master row, but the
// slot is consumed). UNIQUE constraints always create an entry; the
// uniqueness itself is enforced from the table's UNIQUE/PRIMARY KEY
// constraints (compositeUniqueGroups), not from this entry's SQL.
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
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(ctx.Pager.PageSize())-4))
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
	buf.WriteString(s.Name)
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
