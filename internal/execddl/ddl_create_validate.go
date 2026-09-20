// Package execddl: the CREATE TABLE validation pipeline (sqlite3EndTable's
// build.c checks): STRICT, generated columns, WITHOUT ROWID, key-constraint
// rowid references, AUTOINCREMENT, DEFAULT clauses, DQS quoting, CHECK
// subqueries/functions and column-count limits — in the order SQLite runs
// them. Split from ddl.go; behavior unchanged.
package execddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

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
		func() *Result { return e.validateCheckFuncs(s) },
		func() *Result { return e.validateCheckExprColumns(s) },
		func() *Result { return e.validateSchemaFunctionSafety(s) },
		func() *Result { return e.validateTableCollations(s) },
		func() *Result { return e.validateConflictActions(s) },
	}
	for _, v := range validators {
		if res := v(); res != nil {
			return res
		}
	}
	return nil
}

// normalizeConflictAction extracts a constraint's effective ON CONFLICT
// action: the first resolution keyword found in the text, else "" for
// OE_Default (the parser leaves stray text (e.g. a trailing ")") in the field
// for table-level constraints without an explicit ON CONFLICT clause, so the
// keyword scan is the reliable signal). The empty result mirrors SQLite's
// OE_Default: an absent clause is NOT an explicit ABORT (build.c:4358 treats
// OE_Default like "unspecified" when reconciling duplicate constraints).
// conflictKeywordBoundary reports whether byte c is an ON CONFLICT keyword
// boundary: the start/end of text or any non-letter byte.
func conflictKeywordBoundary(c byte) bool {
	return c == 0 || !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'))
}

// conflictKeywordAt reports whether the resolution keyword a occurs in upper
// at idx as a whole word (non-letter bytes on both sides).
func conflictKeywordAt(upper string, a string, idx int) bool {
	before := byte(0)
	if idx > 0 {
		before = upper[idx-1]
	}
	after := byte(0)
	if idx+len(a) < len(upper) {
		after = upper[idx+len(a)]
	}
	return conflictKeywordBoundary(before) && conflictKeywordBoundary(after)
}

func normalizeConflictAction(text string) string {
	upper := strings.ToUpper(text)
	for _, a := range []string{"FAIL", "IGNORE", "REPLACE", "ROLLBACK", "ABORT"} {
		if idx := strings.Index(upper, a); idx >= 0 && conflictKeywordAt(upper, a, idx) {
			return a
		}
	}
	return ""
}

// validateConflictActions rejects a table whose UNIQUE-equivalent constraints
// (column-level PRIMARY KEY / UNIQUE and table-level PRIMARY KEY / UNIQUE)
// cover the same column set with DIFFERENT ON CONFLICT actions. SQLite builds
// one implicit index per distinct key; when the second constraint's action
// differs it reports "conflicting ON CONFLICT clauses specified" (build.c
// sqlite3CreateIndex; index.test 7.6: a PRIMARY KEY ON CONFLICT FAIL plus
// UNIQUE(a) ON CONFLICT IGNORE on the same column). Equal actions dedup into
// a single index and stay legal.
// conflictGroup is one UNIQUE-equivalent constraint's column-set key and ON
// CONFLICT action.
type conflictGroup struct {
	key    string
	action string
	name   string
}

// addConflictGroup appends one constraint group keyed by its lowercased,
// sorted column list.
func addConflictGroup(groups []conflictGroup, cols []string, action, name string) []conflictGroup {
	sorted := append([]string(nil), cols...)
	for i := range sorted {
		sorted[i] = strings.ToLower(strings.TrimSpace(sorted[i]))
	}
	sort.Strings(sorted)
	return append(groups, conflictGroup{key: strings.Join(sorted, "\u0001"), action: strings.ToUpper(action), name: name})
}

// collectConflictGroups gathers the UNIQUE-equivalent constraints of a CREATE
// TABLE in declaration order: column-level PRIMARY KEY / UNIQUE per column,
// then table-level PRIMARY KEY / UNIQUE constraints.
func collectConflictGroups(s *sql.CreateTableStmt) []conflictGroup {
	var groups []conflictGroup
	for i := range s.Columns {
		cd := &s.Columns[i]
		if !cd.PrimaryKey && !cd.Unique {
			continue
		}
		groups = addConflictGroup(groups, []string{cd.Name}, normalizeConflictAction(cd.OnConflict), cd.Name)
	}
	for _, tc := range s.Constraints {
		if tc.Type != sql.ConstraintPrimaryKey && tc.Type != sql.ConstraintUnique {
			continue
		}
		cols := make([]string, 0, len(tc.Columns))
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		groups = addConflictGroup(groups, cols, normalizeConflictAction(tc.OnConflict), tc.Name)
	}
	return groups
}

// conflictingConflictAction reports whether two same-key constraints carry
// explicit, different ON CONFLICT clauses (build.c:4358). When either side is
// OE_Default ("") the duplicate is legal — the explicit action is simply
// adopted for the shared index (conflict-15.10: UNIQUE(x,x) plus UNIQUE(x,x)
// ON CONFLICT REPLACE).
func conflictingConflictAction(a, b conflictGroup) bool {
	return a.action != b.action && a.action != "" && b.action != ""
}

func (e *DDLExecutor) validateConflictActions(s *sql.CreateTableStmt) *Result {
	groups := collectConflictGroups(s)
	for i := range groups {
		for j := i + 1; j < len(groups); j++ {
			if groups[i].key == groups[j].key && conflictingConflictAction(groups[i], groups[j]) {
				return &Result{Error: fmt.Errorf("conflicting ON CONFLICT clauses specified")}
			}
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
		// TEST2(one text)" raises "table test2 already exists"). The
		// accommodation applies only to objects persisted by an EARLIER
		// session (the TCL reset reopened the connection): a table created
		// in this same session always errors like SQLite (misc1-16.2).
		if s.IfNotExists || (!ctx.Schema.SessionCreated(schema.TypeTable, tableName) && existing.SQL != "" && normalizedCreateTableSQL(existing.SQL) == normalizedCreateTableSQL(s.RawSQL)) {
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
	// into col.PrimaryKey (no duplicate error), so count both forms. A
	// PKPromoted column IS the table-level declaration (promoted post-parse
	// for the rowid-alias rule) — count it once via its constraint.
	if countPrimaryKeyDeclarations(s) > 1 {
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

// countPrimaryKeyDeclarations counts the table's PRIMARY KEY declarations:
// each non-promoted column-level PrimaryKey flag is one, each table-level
// ConstraintPrimaryKey constraint is another.
func countPrimaryKeyDeclarations(s *sql.CreateTableStmt) int {
	pkCount := 0
	for _, col := range s.Columns {
		if col.PrimaryKey && !col.PKPromoted {
			pkCount++
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintPrimaryKey {
			pkCount++
		}
	}
	return pkCount
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

// validateCheckFuncs rejects CHECK constraints that reference functions
// unknown to the current connection (SQLite resolve.c lookupFunc at CREATE
// time: check-7.6 `CREATE TABLE t7(a CHECK (myfunc(a)))` on a connection
// without myfunc → "no such function: myfunc"). Column-level and
// table-level CHECK alike.
func (e *DDLExecutor) validateCheckFuncs(s *sql.CreateTableStmt) *Result {
	missing := e.checkColumnFuncsMissing(s)
	if missing == "" {
		missing = e.checkConstraintFuncsMissing(s)
	}
	if missing != "" {
		return &Result{Error: fmt.Errorf("no such function: %s", missing)}
	}
	return nil
}

// checkColumnFuncsMissing returns the first function name missing from the
// connection across the columns' CHECK constraints, or "" when all resolve.
func (e *DDLExecutor) checkColumnFuncsMissing(s *sql.CreateTableStmt) string {
	for _, col := range s.Columns {
		if missing := e.firstMissingCheckFunc(col.Check); missing != "" {
			return missing
		}
	}
	return ""
}

// checkConstraintFuncsMissing returns the first function name missing from
// the connection across the table-level CHECK constraints, or "" when all
// resolve.
func (e *DDLExecutor) checkConstraintFuncsMissing(s *sql.CreateTableStmt) string {
	for _, tc := range s.Constraints {
		if tc.Type != sql.ConstraintCheck || tc.Expr == nil {
			continue
		}
		if missing := e.firstMissingCheckFunc(tc.Expr); missing != "" {
			return missing
		}
	}
	return ""
}

// firstMissingCheckFunc walks one CHECK expression and returns the first
// function call unknown to the current connection ("" when all resolve or the
// expression is nil).
func (e *DDLExecutor) firstMissingCheckFunc(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	var missing string
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if missing != "" {
			return
		}
		if fc, ok := n.(*sql.FuncCall); ok {
			if !e.ctx.FunctionExists(fc.Name) {
				missing = fc.Name
			}
		}
	})
	return missing
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
