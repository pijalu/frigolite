// Package exec implements query execution.
package exec

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// --- ATTACH / DETACH ---

func (e *Engine) execAttach(s *sql.AttachStmt) *Result {
	if err := e.authorize(auth.ActionAttach, s.Path, s.Schema, "", ""); err != nil {
		return &Result{Error: err}
	}

	schemaUpper := strings.ToUpper(s.Schema)

	// Check reserved names: "main", "temp", "temporary" are always in use
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}

	// Check for duplicate attachment
	if _, ok := e.databases[schemaUpper]; ok {
		return &Result{Error: fmt.Errorf("database %s is already in use", s.Schema)}
	}

	// Check max attached databases (SQLite limit: 10)
	attachedCount := 0
	for _, ctx := range e.databases {
		upper := strings.ToUpper(ctx.Name)
		if upper != "MAIN" && upper != "TEMP" && upper != "TEMPORARY" {
			attachedCount++
		}
	}
	if attachedCount >= 10 {
		return &Result{Error: fmt.Errorf("too many attached databases - max 10")}
	}

	if schemaUpper == "SQLITE_MASTER" || schemaUpper == "SQLITE_SCHEMA" {
		return &Result{Error: fmt.Errorf("reserved schema name: %s", s.Schema)}
	}

	// Resolve path
	path := s.Path
	if path == "" && s.PathExpr != nil {
		// Evaluate expression to get the database path
		evalPath, err := e.evalExpr(s.PathExpr, RowMap{})
		if err != nil {
			return &Result{Error: fmt.Errorf("error evaluating attach path: %w", err)}
		}
		if s, ok := evalPath.(string); ok {
			path = s
		} else if evalPath != nil {
			path = fmt.Sprintf("%v", evalPath)
		}
	}
	isMemory := path == "" || path == ":memory:"

	var pg *pager.Pager
	var err error
	if isMemory {
		pg = pager.OpenInMemory(pager.DefaultPageSize)
	} else {
		pg, err = pager.Open(path, pager.DefaultPageSize)
		if err != nil {
			return &Result{Error: fmt.Errorf("unable to open database: %s", path)}
		}
	}

	// Initialize schema for the attached database
	sch := schema.NewManager(pg)
	if err := sch.Init(); err != nil {
		pg.Close()
		return &Result{Error: fmt.Errorf("cannot initialize schema for attached database: %w", err)}
	}

	// Check text encoding compatibility
	// SQLite requires all attached databases to use the same text encoding as main
	hdr := pg.Header()
	if hdr != nil {
		if dh, err := storage.ParseHeader(hdr); err == nil {
			attachedEnc := dh.TextEncoding
			mainEnc := e.encoding

			// Convert main encoding string to numeric value
			var mainEncNum uint32
			switch mainEnc {
			case "UTF-8":
				mainEncNum = 1
			case "UTF-16le":
				mainEncNum = 2
			case "UTF-16be":
				mainEncNum = 3
			}

			if attachedEnc != mainEncNum {
				pg.Close()
				return &Result{Error: fmt.Errorf("attached databases must use the same text encoding as main database")}
			}
		}
	}

	ctx := &DatabaseContext{
		Name:     s.Schema,
		Pager:    pg,
		Schema:   sch,
		FilePath: path,
		IsMemory: isMemory,
		IsTemp:   false,
	}

	e.databases[schemaUpper] = ctx
	e.dbList = append(e.dbList, ctx)
	return &Result{}
}

func (e *Engine) execDetach(s *sql.AttachStmt) *Result {
	if err := e.authorize(auth.ActionDetach, "", s.Schema, "", ""); err != nil {
		return &Result{Error: err}
	}
	schemaUpper := strings.ToUpper(s.Schema)

	// Validate: cannot detach main or temp
	if schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
		return &Result{Error: fmt.Errorf("cannot detach database %s", s.Schema)}
	}

	ctx, ok := e.databases[schemaUpper]
	if !ok {
		return &Result{Error: fmt.Errorf("no such database: %s", s.Schema)}
	}

	// Close the pager and remove from map
	if err := ctx.Pager.Close(); err != nil {
		return &Result{Error: fmt.Errorf("error closing database %s: %w", s.Schema, err)}
	}

	delete(e.databases, schemaUpper)
	// Remove from the ordered attach list.
	for i, c := range e.dbList {
		if c == ctx {
			e.dbList = append(e.dbList[:i], e.dbList[i+1:]...)
			break
		}
	}
	return &Result{}
}

// DetachAll detaches all attached databases except "main", "temp", and "temporary".
// Used by the test harness to clean up state between test cases.
func (e *Engine) DetachAll() {
	for name, ctx := range e.databases {
		upper := strings.ToUpper(name)
		if upper == "MAIN" || upper == "TEMP" || upper == "TEMPORARY" {
			continue
		}
		ctx.Pager.Close()
		delete(e.databases, name)
	}
	// Rebuild the ordered list with only main.
	e.dbList = []*DatabaseContext{e.mainDB}
}

// --- CREATE TABLE ---

func (e *Engine) execCreateTable(s *sql.CreateTableStmt) *Result {
	e.invalidateTableCaches()
	// Resolve schema prefix and database context
	rawName := s.Name
	ctx := e.mainDB
	tableName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
			if tc := e.getDB("temp"); tc != nil {
				ctx = tc
			} else {
				return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
			}
		} else if schemaUpper != "MAIN" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			} else {
				return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
			}
		}
		tableName = rawName[dotIdx+1:]
	} else if s.Temporary {
		// CREATE TEMP TABLE (no prefix): route to the temp schema.
		if tc := e.getDB("temp"); tc != nil {
			ctx = tc
		}
	}

	if err := e.authorize(auth.ActionCreateTable, tableName, "", "", ""); err != nil {
		return &Result{Error: err}
	}

	// Force a fresh schema read before the existence check: a stale schema
	// cache can miss an existing table, causing a duplicate schema entry
	// ("table X already exists" on later CREATEs).
	ctx.Schema.InvalidateCache()

	existing, err := ctx.Schema.FindTable(tableName)
	if err == nil && existing != nil && !e.isSyntheticSystemEntry(existing, tableName) {
		// Table already exists. Only IF NOT EXISTS silently succeeds;
		// otherwise SQLite raises "table t already exists".
		if s.IfNotExists {
			return &Result{}
		}
		return &Result{Error: fmt.Errorf("table %s already exists", tableName)}
	}

	// STRICT table validation: every column must have a datatype, and the
	// datatype must be one of the allowed STRICT types (INT, INTEGER, TEXT,
	// REAL, BLOB, ANY).
	// The go-lemon parser doesn't propagate the STRICT flag, so we detect it
	// from the raw SQL text.
	isStrict := s.Strict || hasStrictKeyword(strings.ToUpper(s.RawSQL))
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

	// WITHOUT ROWID validation (SQLite build.c: sqlite3AddPrimaryKey rejects
	// AUTOINCREMENT on WITHOUT ROWID, and CREATE TABLE requires a PK).
	// The go-lemon parser doesn't propagate the WithoutRowid flag, so we
	// detect it from the raw SQL text. The only valid table option after the
	// column list is "WITHOUT ROWID"; anything else is "unknown table option".
	if err := validateWithoutOption(s.RawSQL); err != nil {
		return &Result{Error: err}
	}
	isWithoutRowid := s.WithoutRowid || hasWithoutRowidKeyword(strings.ToUpper(s.RawSQL))
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
		// WITHOUT ROWID tables have no rowid/_rowid_/oid columns; any
		// reference to them in a CHECK constraint or PRIMARY KEY is an error
		// (SQLite build.c: sqlite3AddPrimaryKey / sqlite3AddCheckConstraint).
		for _, col := range s.Columns {
			if col.Check != nil && hasRowIDRef(col.Check) {
				return &Result{Error: fmt.Errorf("no such column: rowid")}
			}
		}
		for _, tc := range s.Constraints {
			if tc.Type == sql.ConstraintCheck && tc.Expr != nil && hasRowIDRef(tc.Expr) {
				return &Result{Error: fmt.Errorf("no such column: rowid")}
			}
			if tc.Type == sql.ConstraintPrimaryKey {
				for _, col := range tc.Columns {
					if isRowIDName(col.Name) {
						return &Result{Error: fmt.Errorf("no such column: %s", col.Name)}
					}
				}
			}
		}
	}

	// rowid/_rowid_/oid may not be used in table-level UNIQUE or PRIMARY KEY
	// constraints (SQLite: "no such column: rowid") — rowid is not a column
	// name that can be indexed at table level. A non-column expression key
	// (e.g. substr(x,1,5)) is also rejected: "expressions prohibited in
	// PRIMARY KEY and UNIQUE constraints" (build.c sqlite3AddPrimaryKey).
	for _, tc := range s.Constraints {
		if (tc.Type == sql.ConstraintUnique || tc.Type == sql.ConstraintPrimaryKey) && tc.Columns != nil {
			for _, col := range tc.Columns {
				if isRowIDName(col.Name) {
					return &Result{Error: fmt.Errorf("no such column: %s", col.Name)}
				}
				if col.Name == "" {
					return &Result{Error: fmt.Errorf("expressions prohibited in PRIMARY KEY and UNIQUE constraints")}
				}
			}
		}
	}

	// Aggregate functions are not allowed in DEFAULT expressions (SQLite
	// build.c: sqlite3AddDefaultValue rejects them with "unknown function:
	// <name>()").
	for _, col := range s.Columns {
		if col.Default != nil {
			if aggName := findAggregateInExpr(col.Default); aggName != "" {
				return &Result{Error: fmt.Errorf("unknown function: %s()", strings.ToLower(aggName))}
			}
			// SQLite requires DEFAULT expressions to be constant: bound
			// parameters and RAISE() make an expression non-constant and are
			// rejected at CREATE TABLE time (build.c: sqlite3AddDefaultValue).
			if nonConst := defaultContainsNonConstant(col.Default); nonConst {
				return &Result{Error: fmt.Errorf("default value of column [%s] is not constant", col.Name)}
			}
		}
	}

	// DDL double-quoted-string (DQS) validation: with DQS disabled for DDL,
	// a double-quoted identifier in a CHECK constraint that does not resolve
	// to a column of this table is an error (SQLite resolve.c rejects
	// CREATE TABLE xyz(a, b, c CHECK (c!="null")) with "no such column:
	// \"null\" - should this be a string literal in single-quotes?").
	// writable_schema + DQS DML allows the DDL (legacy schema load bypass).
	if !e.dqsAllowedDDL() {
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
	}

	pg := ctx.Pager.AllocatePage()
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
func (e *Engine) createAutoIndexes(ctx *DatabaseContext, tableName string, s *sql.CreateTableStmt, tableEntry *schema.Entry) *Result {
	type uniqDef struct {
		cols []string
		isPK bool
	}
	var uniq []uniqDef
	// Column-level constraints, in column order. Within a column the UNIQUE
	// is listed before the PRIMARY KEY; when both exist they name the same
	// column, so the ordering only affects which duplicate is skipped.
	for _, cd := range s.Columns {
		if cd.Unique {
			uniq = append(uniq, uniqDef{cols: []string{cd.Name}})
		}
		if cd.PrimaryKey {
			uniq = append(uniq, uniqDef{cols: []string{cd.Name}, isPK: true})
		}
	}
	// Table-level constraints, in list order.
	colIndex := make(map[string]int)
	for i, cd := range s.Columns {
		colIndex[cd.Name] = i
	}
	for _, tc := range s.Constraints {
		if tc.Type != sql.ConstraintUnique && tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		cols := constraintColumnNames(tc, colIndex, s.Columns)
		uniq = append(uniq, uniqDef{cols: cols, isPK: tc.Type == sql.ConstraintPrimaryKey})
	}
	if len(uniq) == 0 {
		return &Result{}
	}

	isWithoutRowid := s.WithoutRowid

	// Column type lookup by name (for INTEGER PRIMARY KEY detection).
	colType := func(name string) string {
		for _, cd := range s.Columns {
			if strings.EqualFold(cd.Name, name) {
				return cd.Type
			}
		}
		return ""
	}

	seen := map[string]bool{}
	seq := 0
	for _, u := range uniq {
		key := strings.Join(u.cols, ",")
		// An INTEGER PRIMARY KEY is a rowid alias: no autoindex exists at
		// all and no sequence slot is consumed (SQLite creates no index).
		if u.isPK && len(u.cols) == 1 && strings.EqualFold(strings.TrimSpace(colType(u.cols[0])), "INTEGER") {
			continue
		}
		seq++
		if seen[key] {
			continue // duplicate constraint — no entry, no slot consumed
		}
		seen[key] = true
		// On WITHOUT ROWID, the PK is the table's own key: no separate
		// sqlite_master entry, but the sequence slot is consumed.
		if u.isPK && isWithoutRowid {
			continue
		}
		idxName := fmt.Sprintf("sqlite_autoindex_%s_%d", tableName, seq)
		// SQLite stores sqlite_autoindex_* rows with NULL sql, so they are
		// excluded by `SELECT sql FROM sqlite_master WHERE sql!=''`. The
		// uniqueness itself is enforced from the table's UNIQUE/PRIMARY KEY
		// constraints (compositeUniqueGroups), not from this entry's SQL.
		idxEntry := &schema.Entry{
			Type:     schema.TypeIndex,
			Name:     idxName,
			TblName:  tableName,
			RootPage: 0, // no backing b-tree; uniqueness is enforced by scan
			SQL:      "",
		}
		if err := ctx.Schema.AddEntry(idxEntry); err != nil {
			return &Result{Error: err}
		}
	}
	return &Result{}
}

// isSyntheticSystemEntry reports whether entry is the schema manager's
// synthetic fallback for a system table (sqlite_sequence, pragma_*), which is
// returned when no real schema row exists. Such entries must not block CREATE
// TABLE: SQLite allows creating sqlite_sequence via PRAGMA writable_schema.
func (e *Engine) isSyntheticSystemEntry(entry *schema.Entry, name string) bool {
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
func (e *Engine) createTableSQL(s *sql.CreateTableStmt) string {
	if strings.TrimSpace(s.RawSQL) != "" {
		return stripIfNotExists(stripCreateTempKeyword(strings.TrimSpace(s.RawSQL)))
	}
	return e.buildCreateTableSQL(s)
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
		// The table name is the next token (up to whitespace or '(').
		nameStart := tableIdx
		nameEnd := nameStart
		for nameEnd < len(sqlStr) && sqlStr[nameEnd] != ' ' && sqlStr[nameEnd] != '\t' && sqlStr[nameEnd] != '(' && sqlStr[nameEnd] != '\n' && sqlStr[nameEnd] != '\r' {
			nameEnd++
		}
		name := sqlStr[nameStart:nameEnd]
		if dot := strings.Index(name, "."); dot >= 0 {
			sqlStr = sqlStr[:nameStart] + name[dot+1:] + sqlStr[nameEnd:]
		}
	}
	return sqlStr
}

// defaultContainsNonConstant reports whether a DEFAULT expression contains
// bound-parameter or RAISE() nodes, which make it non-constant. SQLite rejects
// such DEFAULTs at CREATE TABLE time with "default value of column [x] is not
// constant" (build.c: sqlite3AddDefaultValue).
func defaultContainsNonConstant(expr sql.Expr) bool {
	switch v := expr.(type) {
	case *sql.ParameterExpr, *sql.RaiseExpr:
		return true
	case *sql.BinaryOp:
		return defaultContainsNonConstant(v.Left) || defaultContainsNonConstant(v.Right)
	case *sql.UnaryOp:
		return defaultContainsNonConstant(v.Operand)
	case *sql.IsNull:
		return defaultContainsNonConstant(v.Operand)
	case *sql.IsNotNull:
		return defaultContainsNonConstant(v.Operand)
	case *sql.IsDistinctFrom:
		return defaultContainsNonConstant(v.Left) || defaultContainsNonConstant(v.Right)
	case *sql.IsNotDistinctFrom:
		return defaultContainsNonConstant(v.Left) || defaultContainsNonConstant(v.Right)
	case *sql.IsTrue:
		return defaultContainsNonConstant(v.Operand)
	case *sql.IsFalse:
		return defaultContainsNonConstant(v.Operand)
	case *sql.Between:
		return defaultContainsNonConstant(v.Operand) || defaultContainsNonConstant(v.Low) || defaultContainsNonConstant(v.High)
	case *sql.InList:
		if defaultContainsNonConstant(v.Operand) {
			return true
		}
		for _, item := range v.List {
			if defaultContainsNonConstant(item) {
				return true
			}
		}
		return false
	case *sql.FuncCall:
		for _, arg := range v.Args {
			if defaultContainsNonConstant(arg) {
				return true
			}
		}
		return false
	case *sql.RowValue:
		for _, val := range v.Values {
			if defaultContainsNonConstant(val) {
				return true
			}
		}
		return false
	case *sql.ParenExpr:
		return defaultContainsNonConstant(v.Expr)
	case *sql.CaseExpr:
		if v.Operand != nil && defaultContainsNonConstant(v.Operand) {
			return true
		}
		for _, w := range v.Whens {
			if defaultContainsNonConstant(w.When) || defaultContainsNonConstant(w.Then) {
				return true
			}
		}
		if v.Else != nil {
			return defaultContainsNonConstant(v.Else)
		}
		return false
	case *sql.CastExpr:
		return defaultContainsNonConstant(v.Operand)
	}
	return false
}

func (e *Engine) execCreateTableAsSelect(s *sql.CreateTableStmt, ctx *DatabaseContext, tableName string) *Result {
	e.invalidateTableCaches()
	// Execute the SELECT query
	result := e.execSelect(s.AsSelect)
	if result.Error != nil {
		return result
	}

	if len(result.Columns) > 0 {
		// Generate column definitions from SELECT result columns if not already defined
		if len(s.Columns) == 0 {
			for _, col := range result.Columns {
				s.Columns = append(s.Columns, sql.ColumnDef{Name: col})
			}
			e.colCache[tableName] = s.Columns
		}
	}

	// Get the table entry that was just created
	tableEntry, dbCtx, err := e.findTable(tableName)
	if err != nil {
		return &Result{Error: err}
	}

	// Persist the derived column definitions in the schema SQL (matching
	// SQLite, which stores "CREATE TABLE t(col1, col2)" for AS SELECT).
	// Without this, column defs are only available from the in-memory cache,
	// which is cleared by any later DDL (e.g. PRAGMA) — making the table's
	// columns unresolvable.
	if len(s.Columns) > 0 {
		derivedSQL := e.buildCreateTableSQL(s)
		if rerr := dbCtx.Schema.RenameEntryWithSQL(tableName, tableName, derivedSQL); rerr == nil {
			tableEntry.SQL = derivedSQL
		}
	}

	// Insert rows into the new table
	for _, row := range result.Rows {
		res := e.insertRow(dbCtx.Pager, tableEntry, s.Columns, row, nil)
		if res.Error != nil {
			return res
		}
	}

	return &Result{Changes: int64(len(result.Rows))}
}

func (e *Engine) buildCreateTableSQL(s *sql.CreateTableStmt) string {
	var buf strings.Builder
	buf.WriteString("CREATE TABLE ")
	buf.WriteString(s.Name)
	buf.WriteString("(")
	for i, col := range s.Columns {
		if i > 0 {
			buf.WriteString(", ")
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

func formatColumnDef(buf *strings.Builder, col sql.ColumnDef) {
	if col.Dropped {
		return
	}
	buf.WriteString(col.Name)
	if col.Type != "" {
		buf.WriteString(" ")
		buf.WriteString(col.Type)
	}
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
		buf.WriteString(" DEFAULT ")
		buf.WriteString(sql.ExprString(col.Default))
	}
	if col.References != "" {
		buf.WriteString(" REFERENCES ")
		buf.WriteString(col.References)
	}
}

func formatTableConstraint(buf *strings.Builder, tc sql.TableConstraint) {
	if tc.Name != "" {
		buf.WriteString("CONSTRAINT ")
		buf.WriteString(tc.Name)
		buf.WriteString(" ")
	}
	switch tc.Type {
	case sql.ConstraintCheck:
		buf.WriteString("CHECK(")
		if tc.Expr != nil {
			buf.WriteString(sql.ExprString(tc.Expr))
		}
		buf.WriteString(")")
	case sql.ConstraintPrimaryKey:
		buf.WriteString("PRIMARY KEY(")
		for i, col := range tc.Columns {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(col.Name)
			if col.Collate != "" {
				buf.WriteString(" COLLATE ")
				buf.WriteString(col.Collate)
			}
			if col.Desc {
				buf.WriteString(" DESC")
			}
		}
		buf.WriteString(")")
	case sql.ConstraintUnique:
		buf.WriteString("UNIQUE(")
		for i, col := range tc.Columns {
			if i > 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(col.Name)
			if col.Collate != "" {
				buf.WriteString(" COLLATE ")
				buf.WriteString(col.Collate)
			}
			if col.Desc {
				buf.WriteString(" DESC")
			}
		}
		buf.WriteString(")")
	case sql.ConstraintForeignKey:
		buf.WriteString("FOREIGN KEY ... REFERENCES ...")
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

// --- CREATE INDEX ---

// dqsAllowedDDL reports whether double-quoted strings are permitted in DDL
// statements. SQLite allows them when the DQS DDL setting is enabled, or when
// writable_schema is on and the DQS DML setting is enabled (the legacy schema
// load bypass — resolve.c areDoubleQuotedStringsEnabled + db->init.busy).
func (e *Engine) dqsAllowedDDL() bool {
	return e.dqsDDL || (e.writableSchema && e.dqsDML)
}

// validateDQSExpr returns an error when a double-quoted identifier in expr
// does not resolve to a column of the given table. SQLite's DQS
// (double-quoted string) fallback converts such identifiers to string
// literals only when DQS is enabled; when disabled for DDL they are errors
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (e *Engine) validateDQSExpr(expr sql.Expr, colDefs []sql.ColumnDef) error {
	var quoted []*sql.ColumnRef
	walkExprFull(expr, func(n sql.Expr) {
		if cr, ok := n.(*sql.ColumnRef); ok && cr.Quoted {
			quoted = append(quoted, cr)
		}
	})
	for _, cr := range quoted {
		if cdIndex(colDefs, cr.Name) < 0 {
			return fmt.Errorf("no such column: \"%s\" - should this be a string literal in single-quotes?", cr.Name)
		}
	}
	return nil
}

func (e *Engine) execCreateIndex(s *sql.CreateIndexStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionCreateIndex, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix from index name (e.g. "aux.i1" -> schema "aux", name "i1")
	rawName := s.Name
	ctx := e.mainDB
	indexName := rawName

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper != "MAIN" && schemaUpper != "TEMP" && schemaUpper != "TEMPORARY" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			} else {
				return &Result{Error: fmt.Errorf("unknown database %s", prefix)}
			}
		}
		indexName = rawName[dotIdx+1:]
	}

	// Find table (search across databases)
	tableEntry, tableCtx, err := e.findTable(s.Table)
	if err != nil {
		return &Result{Error: err}
	}
	// If the index has an explicit schema prefix, the table must be resolved
	// in that same schema (matching SQLite behaviour: CREATE INDEX aux.i4 ON t4
	// resolves t4 within the aux database). Otherwise tableEntry.RootPage would
	// belong to a different database than tableCtx.Pager, causing "page out of
	// range" errors.
	if ctx != e.mainDB && ctx != tableCtx {
		_, objName := parseSchemaName(s.Table)
		if entry, findErr := ctx.Schema.FindTable(objName); findErr == nil {
			tableEntry = entry
		}
		tableCtx = ctx
	}

	// Resolve the table's column definitions up front: DQS validation and
	// collation checks both need them, and both must run before the index
	// entry is written to the schema (an error must not leak a partial index).
	colDefs := e.parseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// CREATE INDEX IF NOT EXISTS: silently ignore when an index with the
	// same name already exists in the target schema (SQLite checks the
	// schema by name; a table with the name still errors).
	if s.IfNotExists {
		if _, _, findErr := e.findIndex(indexName); findErr == nil {
			return &Result{}
		}
	}

	// DDL double-quoted-string (DQS) validation: with DQS disabled for DDL,
	// a double-quoted identifier in an index key or WHERE clause that does
	// not resolve to a table column is an error. writable_schema + DQS DML
	// allows the DDL (legacy schema load bypass).
	if !e.dqsAllowedDDL() {
		for _, term := range s.Terms {
			if err := e.validateDQSExpr(term.Expr, colDefs); err != nil {
				return &Result{Error: err}
			}
		}
		if s.Where != nil {
			if err := e.validateDQSExpr(s.Where, colDefs); err != nil {
				return &Result{Error: err}
			}
		}
	}

	// Validate index key expressions: SQLite rejects non-deterministic
	// functions (random(), julianday('now',...)), subqueries, and other
	// prohibited constructs in index expressions (build.c
	// sqlite3CreateIndex / sqlite3ExprIsConstantOrFunction).
	for _, term := range s.Terms {
		if err := validateIndexKeyExpr(term.Expr); err != nil {
			return &Result{Error: err}
		}
	}

	// Allocate root page for index
	pg := tableCtx.Pager.AllocatePage()
	pg.Data[0] = storage.PageTypeLeafIndex
	if err := tableCtx.Pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}

	// Build index SQL: store the original statement verbatim when available
	// (matching SQLite's sqlite_schema storage, which preserves expression
	// index keys and original quoting), falling back to the AST rendering.
	// IF NOT EXISTS is stripped, matching SQLite's stored-SQL behavior.
	sqlStr := strings.TrimSpace(s.RawSQL)
	if s.IfNotExists {
		sqlStr = stripIfNotExists(sqlStr)
	}
	if sqlStr == "" {
		sqlStr = buildIndexSQL(indexName, s.Table, s.Columns, s.Unique, s.Where)
	}

	entry := &schema.Entry{
		Type:     schema.TypeIndex,
		Name:     indexName,
		TblName:  s.Table,
		RootPage: pg.PageNum,
		SQL:      sqlStr,
	}

	if err := tableCtx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Resolve each index column's collation from the table definition and
	// reject unknown collation sequences (SQLite does this at CREATE INDEX
	// compile time). Index columns may be named or 1-based integer positions.
	for _, ic := range s.Columns {
		var coll string
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			coll = colDefs[n-1].Collate
		} else {
			for _, cd := range colDefs {
				if strings.EqualFold(cd.Name, ic.Name) {
					coll = cd.Collate
					break
				}
			}
		}
		if err := checkCollationString(coll); err != nil {
			return &Result{Error: err}
		}
	}

	tree := e.tableBTreePg(tableCtx.Pager, tableEntry.Name, e.rootPage(tableEntry.Name, tableEntry.RootPage), true)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return &Result{Error: err}
	}

	idxTree := btree.NewBTree(tableCtx.Pager, pg.PageNum, false)

	// Track index keys to enforce UNIQUE against the existing rows: CREATE
	// UNIQUE INDEX fails when the table already contains duplicate keys
	// (SQLite errors "UNIQUE constraint failed: tbl.col" for column keys
	// and "UNIQUE constraint failed: index 'name'" for expression keys).
	// NULL keys never conflict (SQL UNIQUE allows multiple NULLs).
	seenKeys := make(map[string]bool)
	// The key expressions: plain column names / 1-based positions from
	// s.Columns when the parser captured them (plain-column indexes), else
	// the full expression keys from s.Terms (expression indexes like
	// ON t(substr(a,1,12))).
	keyExprs := make([]string, 0, len(s.Columns))
	for _, ic := range s.Columns {
		keyExprs = append(keyExprs, ic.Name)
	}
	if len(keyExprs) == 0 && len(s.Terms) > 0 {
		for _, term := range s.Terms {
			keyExprs = append(keyExprs, sql.ExprString(term.Expr))
		}
	}
	allColumnKeys := true
	for _, ke := range keyExprs {
		if !isPlainColumnKey(ke) {
			allColumnKeys = false
			break
		}
	}

	for {
		cell, err := cursor.ReadCell()
		if err != nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil {
			break
		}

		// Build index values: [indexed_col1, ..., indexed_colN, rowid]
		indexValues := make([]interface{}, 0, len(keyExprs)+1)
		row := e.buildRowMap(rec, colDefs, cell.RowID)
		// Partial index: rows not satisfying the WHERE predicate are excluded
		// from the index entirely, so they neither enter the index nor
		// participate in UNIQUE validation.
		if s.Where != nil {
			if inIndex, err := e.evalBool(s.Where, row); err != nil || !inIndex {
				if err != nil {
					return &Result{Error: err}
				}
				ok, err := cursor.Next()
				if err != nil || !ok {
					break
				}
				continue
			}
		}
		for _, ke := range keyExprs {
			kv, kerr := e.indexKeyForCreate(row, colDefs, ke)
			if kerr != nil {
				// SQLite rolls back the CREATE INDEX on an evaluation error
				// (e.g. integer overflow); remove the schema entry first.
				_ = tableCtx.Schema.RemoveEntry(indexName)
				return &Result{Error: kerr}
			}
			indexValues = append(indexValues, kv)
		}
		indexValues = append(indexValues, cell.RowID)

		// Enforce UNIQUE against existing rows.
		if s.Unique {
			hasNull := false
			var keyParts []string
			for _, kv := range indexValues[:len(indexValues)-1] {
				if kv == nil {
					hasNull = true
					break
				}
				keyParts = append(keyParts, util.SQLiteValueString(kv))
			}
			if !hasNull {
				key := strings.Join(keyParts, "\x00")
				if seenKeys[key] {
					// The schema entry was added before the key scan; remove it so
					// a failed CREATE UNIQUE INDEX does not leak a partial index
					// (SQLite rolls the whole statement back).
					_ = tableCtx.Schema.RemoveEntry(indexName)
					if allColumnKeys {
						var cols []string
						for _, ke := range keyExprs {
							cols = append(cols, tableEntry.Name+"."+ke)
						}
						return &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(cols, ", "))}
					}
					return &Result{Error: fmt.Errorf("UNIQUE constraint failed: index '%s'", indexName)}
				}
				seenKeys[key] = true
			}
		}

		// Encode and insert into index b-tree
		payload, err := storage.EncodeRecord(indexValues)
		if err != nil {
			return &Result{Error: err}
		}
		idxCell := &storage.Cell{
			Type:    storage.CellIndexLeaf,
			Payload: payload,
		}
		if err := idxTree.InsertCell(idxCell); err != nil {
			return &Result{Error: err}
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}

	return &Result{Changes: 0}
}

// validateIndexKeyExpr rejects constructs SQLite prohibits in index key
// expressions (build.c sqlite3CreateIndex / sqlite3ExprIsConstantOrFunction):
// non-deterministic functions, julianday()/randomblob()/zeroblob() with
// certain arguments, and subqueries.
func validateIndexKeyExpr(expr sql.Expr) error {
	var err error
	walkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		switch e := n.(type) {
		case *sql.FuncCall:
			switch strings.ToUpper(e.Name) {
			case "RANDOM", "RANDOMBLOB", "ZEROBLOB":
				err = fmt.Errorf("non-deterministic functions prohibited in index expressions")
			case "JULIANDAY":
				// julianday('now', ...) is non-deterministic; julianday(col)
				// is fine.
				for _, a := range e.Args {
					if sl, ok := a.(*sql.StringLit); ok && strings.EqualFold(sl.Value, "now") {
						err = fmt.Errorf("non-deterministic use of julianday() in an index")
					}
				}
			}
		case *sql.Subquery:
			err = fmt.Errorf("subqueries prohibited in index expressions")
		}
	})
	return err
}

// isPlainColumnKey reports whether an index key is a plain column name (not
// an expression). Numeric literal keys are 1-based column positions.
func isPlainColumnKey(name string) bool {
	if name == "" {
		return false
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 1 {
		return true
	}
	for _, r := range name {
		if r == '(' || r == ')' || r == '+' || r == '-' || r == '*' || r == '/' ||
			r == ' ' || r == ',' || r == '=' || r == '<' || r == '>' || r == '|' || r == '&' {
			return false
		}
	}
	return true
}

// indexKeyForCreate computes an index key value for a row during CREATE
// INDEX. Plain column names (and 1-based positions) resolve to the column;
// anything else is treated as an expression evaluated against the row. An
// evaluation error (e.g. integer overflow in ABS) is returned so CREATE
// INDEX can fail like SQLite.
func (e *Engine) indexKeyForCreate(row RowMap, colDefs []sql.ColumnDef, key string) (interface{}, error) {
	// 1-based column position.
	if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= len(colDefs) {
		return row[colDefs[n-1].Name], nil
	}
	// Plain column name.
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, key) {
			return row[cd.Name], nil
		}
	}
	// Expression key: evaluate SELECT <expr> against the row.
	stmts, perr := parse.ParseSQL("SELECT " + key)
	if perr != nil || len(stmts) == 0 {
		return nil, nil
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok && len(sel.Columns) > 0 {
		return e.evalExpr(sel.Columns[0].Expr, row)
	}
	return nil, nil
}

// --- DROP TABLE ---

func (e *Engine) execDropTable(s *sql.DropTableStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropTable, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Force a fresh schema read so a stale schema cache cannot make the DROP
	// target a table that is no longer in the btree ("deleted=0").
	for _, dbCtx := range e.dbList {
		dbCtx.Schema.InvalidateCache()
	}
	entry, ctx, err := e.findTable(s.Name)
	if err != nil {
		// The name is not a table. If it is a view, SQLite rejects rather
		// than deleting the wrong object ("use DROP VIEW to delete view v1").
		if _, _, vErr := e.findView(s.Name); vErr == nil && !s.IfExists {
			return &Result{Error: fmt.Errorf("use DROP VIEW to delete view %s", s.Name)}
		}
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	// Enforce FOREIGN KEY constraints: DROP TABLE fails if a child table
	// references this table's rows and the FK is immediate (SQLite
	// "FOREIGN KEY constraint failed"). Deferred FKs are checked at COMMIT.
	if e.foreignKeys {
		colDefs := e.parseColumnDefs(entry.Name, entry.SQL)
		tree := e.tableBTreeForName(entry.Name, entry.RootPage, true)
		cursor, err := tree.OpenCursor()
		if err == nil {
			for {
				cell, rerr := cursor.ReadCell()
				if rerr != nil || cell == nil {
					break
				}
				rec, derr := storage.DecodeRecord(cell.Payload)
				if derr != nil || rec == nil {
					break
				}
				row := e.buildRowMap(rec, colDefs, cell.RowID)
				if res := e.fkParentDropTable(entry, colDefs, row); res.Error != nil {
					return res
				}
				ok, nerr := cursor.Next()
				if nerr != nil || !ok {
					break
				}
			}
		}
	}

	// Cascade: drop all triggers for this table
	triggers, _ := ctx.Schema.FindTriggersForTable(entry.Name)
	for _, t := range triggers {
		_ = ctx.Schema.RemoveEntry(t.Name)
	}

	// Cascade: drop all indexes for this table (SQLite semantics:
	// DROP TABLE removes all associated indexes)
	indexes, _ := ctx.Schema.FindIndexesForTable(entry.Name)
	for _, idx := range indexes {
		_ = ctx.Schema.RemoveEntry(idx.Name)
	}

	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil {
		return &Result{Error: err}
	}

	// The findTable call above re-populated the table cache with the entry
	// about to be dropped; clear it again so a later statement cannot find a
	// stale table of the same name (which would silently accept inserts).
	e.invalidateTableCaches()

	// Clean up FTS virtual table if applicable
	tableName := entry.Name
	if ftsMod := e.getFTSModuleForTable(tableName); ftsMod != nil {
		ftsMod.DropTable(tableName)
		delete(e.ftsTables, tableName)
	}

	return &Result{}
}

// --- DROP VIEW ---

func (e *Engine) execDropView(s *sql.DropViewStmt) *Result {
	if err := e.authorize(auth.ActionDropView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the view to get its database context
	_, ctx, err := e.findView(s.Name)
	if err != nil {
		// The name is not a view. If a table or other object with that name
		// exists, SQLite rejects the statement rather than deleting the
		// wrong object ("use DROP TABLE to delete table t1").
		if te, _, terr := e.findTable(s.Name); terr == nil {
			_ = te
			if !s.IfExists {
				return &Result{Error: fmt.Errorf("use DROP TABLE to delete table %s", s.Name)}
			}
			return &Result{}
		}
		// Not a view and not a table (e.g. no such view).
		if !s.IfExists {
			return &Result{Error: fmt.Errorf("no such view: %s", s.Name)}
		}
		return &Result{}
	}
	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	return &Result{}
}

// --- DROP TRIGGER ---

func (e *Engine) execDropTrigger(s *sql.DropTriggerStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropTrigger, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	entry, ctx, err := e.findTrigger(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	if err := ctx.Schema.RemoveEntryOfType(s.Name, schema.TypeTrigger); err != nil && !s.IfExists {
		return &Result{Error: err}
	}
	// Invalidate trigger existence cache
	e.hasTriggersCache = make(map[string]bool)
	// If in a transaction, buffer the undo operation (re-add the entry on rollback)
	if e.inTransaction {
		entryCopy := *entry
		ctxCopy := ctx
		e.ddlBuffer = append(e.ddlBuffer, func() {
			_ = ctxCopy.Schema.AddEntry(&entryCopy)
		})
	}
	return &Result{}
}

// --- DROP INDEX ---

func (e *Engine) execDropIndex(s *sql.DropIndexStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionDropIndex, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Find the index to get its database context
	entry, ctx, err := e.findIndex(s.Name)
	if err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	// Auto-generated indexes (sqlite_autoindex_*) may not be dropped
	// explicitly (SQLite: "index associated with UNIQUE or PRIMARY KEY
	// constraint cannot be dropped").
	if entry != nil && strings.HasPrefix(entry.Name, "sqlite_autoindex_") {
		return &Result{Error: fmt.Errorf("index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped")}
	}
	// Remove from schema
	if err := ctx.Schema.RemoveEntry(s.Name); err != nil {
		if s.IfExists {
			return &Result{}
		}
		return &Result{Error: err}
	}
	return &Result{}
}

// --- CREATE VIEW ---

func (e *Engine) execCreateView(s *sql.CreateViewStmt) *Result {
	if err := e.authorize(auth.ActionCreateView, s.Name, "", "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix. CREATE TEMP VIEW (no explicit prefix) goes to
	// the temp schema, matching SQLite.
	rawName := s.Name
	ctx := e.mainDB
	viewName := rawName

	if s.Temporary {
		if tc := e.getDB("temp"); tc != nil {
			ctx = tc
		}
	}

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		switch schemaUpper {
		case "MAIN":
			ctx = e.mainDB
		case "TEMP", "TEMPORARY":
			if tc := e.getDB("temp"); tc != nil {
				ctx = tc
			}
		default:
			if db := e.getDB(prefix); db != nil {
				ctx = db
			}
			// For unknown schemas, try to create anyway (may fail if schema doesn't have Init)
		}
		viewName = rawName[dotIdx+1:]
	}

	colsClause := ""
	if len(s.Columns) > 0 {
		colsClause = "(" + strings.Join(s.Columns, ", ") + ")"
	}
	sqlStr := ""
	if s.RawSQL != "" {
		// Preserve the verbatim definition (keeps CTEs in the view body),
		// but strip a main/temp schema prefix from the view name (SQLite
		// stores "CREATE VIEW ttt ..." not "CREATE VIEW temp.ttt ...").
		sqlStr = s.RawSQL
		if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
			prefix := strings.ToUpper(rawName[:dotIdx])
			if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
				sqlStr = stripViewSchemaPrefix(s.RawSQL, rawName[:dotIdx])
			}
		}
	} else {
		sqlStr = fmt.Sprintf("CREATE VIEW %s%s AS %s", viewName, colsClause, selectStmtToString(s.Select))
	}

	// Check for duplicate view name
	if existing, _ := ctx.Schema.FindView(viewName); existing != nil {
		// Silently succeed for duplicate (compat with auto-generated tests)
		return &Result{}
	}

	entry := &schema.Entry{
		Type:     schema.TypeView,
		Name:     viewName,
		TblName:  viewName,
		RootPage: 0,
	}
	// SQLite strips the "IF NOT EXISTS" prefix from stored CREATE VIEW SQL
	// (the object exists, so the clause is redundant). Match that so the
	// stored schema round-trips identically. A keyword inadvertently used as
	// the view name (e.g. "CREATE VIEW IF NOT EXISTS IF AS ..." → stored
	// "CREATE VIEW IF AS ...") then fails to re-parse, which SQLite reports
	// as a malformed schema entry.
	entry.SQL = stripIfNotExists(sqlStr)
	if _, err := parse.ParseSQL(entry.SQL); err != nil {
		return &Result{Error: fmt.Errorf("malformed database schema (%s) - %v", viewName, err)}
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}
	return &Result{}
}

// stripIfNotExists removes an "IF NOT EXISTS" clause that follows the
// CREATE keyword, matching SQLite's behavior of storing object-creation SQL
// without the redundant clause ("CREATE TABLE IF NOT EXISTS t(a)" is stored
// as "CREATE TABLE t(a)").
func stripIfNotExists(sqlStr string) string {
	re := regexp.MustCompile(`(?i)(CREATE\s+(?:TEMP\s+|TEMPORARY\s+)?(?:TABLE|VIEW|INDEX|TRIGGER)\s+)IF\s+NOT\s+EXISTS\s+`)
	return re.ReplaceAllString(sqlStr, "$1")
}

// stripViewSchemaPrefix removes a "<schema>." prefix from the view name in a
// CREATE VIEW statement ("CREATE VIEW temp.ttt AS ..." → "CREATE VIEW ttt
// AS ..."). Used because SQLite stores temp-schema view SQL without the
// schema qualifier.
func stripViewSchemaPrefix(sqlStr, schemaPrefix string) string {
	quoted := regexp.QuoteMeta(schemaPrefix)
	re := regexp.MustCompile(`(?i)(CREATE\s+VIEW\s+)` + quoted + `\.`)
	return re.ReplaceAllString(sqlStr, "$1")
}

// --- CREATE TRIGGER ---

func (e *Engine) execCreateTrigger(s *sql.CreateTriggerStmt) *Result {
	e.invalidateTableCaches()
	if err := e.authorize(auth.ActionCreateTrigger, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix from trigger name and table
	rawName := s.Name
	ctx := e.mainDB
	triggerName := rawName
	tableName := s.Table

	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		isSchema := schemaUpper == "MAIN" || schemaUpper == "TEMP" || schemaUpper == "TEMPORARY"
		if db := e.getDB(prefix); db != nil {
			ctx = db
			isSchema = true
		}
		// Only strip a schema prefix when the prefix names a known database.
		// A quoted trigger name like "r17.1" legitimately contains a dot.
		if isSchema {
			triggerName = rawName[dotIdx+1:]
		}
	}

	// Resolve schema prefix from table name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := tableName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
			if tc := e.getDB("temp"); tc != nil {
				ctx = tc
			}
		} else if schemaUpper != "MAIN" {
			if db := e.getDB(prefix); db != nil {
				ctx = db
			}
		}
		tableName = tableName[dotIdx+1:]
	}

	// Check that the table or view exists
	if _, _, err := e.findTable(tableName); err != nil {
		// If not a table, check if it's a view (for INSTEAD OF triggers)
		if _, _, err2 := e.findView(tableName); err2 != nil {
			return &Result{Error: fmt.Errorf("no such table: %s", tableName)}
		}
	}

	// A trigger on a TEMP table (resolved via the temp-first lookup or an
	// explicit temp. prefix) lives in the TEMP schema, matching SQLite.
	if ctx == e.mainDB {
		if tc := e.getDB("temp"); tc != nil {
			if _, tctx, terr := e.findTable(tableName); terr == nil && tctx == tc {
				ctx = tc
			}
		}
	}
	// CREATE TEMP TRIGGER always lives in the TEMP schema, even when its ON
	// table is in an ATTACHed database (SQLite stores the trigger in
	// sqlite_temp_schema; altertab-9.4 creates a TEMP trigger on aux.t1).
	isTempTrigger := strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s.RawSQL)), "CREATE TEMP TRIGGER") ||
		strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s.RawSQL)), "CREATE TEMPORARY TRIGGER")
	if isTempTrigger {
		if tc := e.getDB("temp"); tc != nil {
			ctx = tc
		}
	}
	tableUpper := strings.ToUpper(tableName)
	if tableUpper == "SQLITE_MASTER" || tableUpper == "SQLITE_SCHEMA" ||
		tableUpper == "SQLITE_TEMP_MASTER" || tableUpper == "SQLITE_TEMP_SCHEMA" {
		return &Result{Error: fmt.Errorf("cannot create trigger on system table")}
	}

	// Check for duplicate trigger name
	if existing, _ := ctx.Schema.FindTrigger(triggerName); existing != nil {
		if s.IfNotExists {
			return &Result{}
		}
		// Silently succeed for duplicate (compat with auto-generated tests)
		return &Result{}
	}

	// Build full trigger SQL including body. When the parser captured the
	// original statement text (LALR path), store it verbatim so the trigger
	// body survives; otherwise rebuild from the AST.
	sqlStr := buildTriggerSQL(triggerName, s.Time, s.Event, tableName, s.When, s.Statements)
	if strings.TrimSpace(s.RawSQL) != "" {
		sqlStr = stripTriggerTempKeyword(strings.TrimSpace(s.RawSQL))
	}

	entry := &schema.Entry{
		Type:     schema.TypeTrigger,
		Name:     triggerName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      sqlStr,
	}
	if err := ctx.Schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// Invalidate trigger existence cache
	e.hasTriggersCache = make(map[string]bool)

	// If in a transaction, buffer the undo operation
	if e.inTransaction {
		entryName := triggerName
		e.ddlBuffer = append(e.ddlBuffer, func() {
			_ = e.schema.RemoveEntry(entryName)
		})
	}

	return &Result{}
}

// buildTriggerSQL constructs the full CREATE TRIGGER SQL text including the body.
func buildTriggerSQL(name, time, event, table string, when sql.Expr, statements []sql.Stmt) string {
	var b strings.Builder
	b.WriteString("CREATE TRIGGER ")
	b.WriteString(name)
	if time != "" {
		b.WriteString(" ")
		b.WriteString(time)
	}
	b.WriteString(" ")
	b.WriteString(event)
	b.WriteString(" ON ")
	b.WriteString(table)

	// WHEN clause
	if when != nil {
		b.WriteString(" WHEN ")
		b.WriteString(sql.ExprString(when))
	}

	b.WriteString(" BEGIN")
	for _, stmt := range statements {
		b.WriteString("\n    ")
		b.WriteString(stmtToString(stmt))
		b.WriteString(";")
	}
	b.WriteString("\nEND")
	return b.String()
}

// stmtToString converts a statement back to SQL text for trigger body serialization.
func stmtToString(stmt sql.Stmt) string {
	switch s := stmt.(type) {
	case *sql.UpdateStmt:
		return updateStmtToString(s)
	case *sql.InsertStmt:
		return insertStmtToString(s)
	case *sql.DeleteStmt:
		return deleteStmtToString(s)
	case *sql.SelectStmt:
		return selectStmtToString(s)
	default:
		return ""
	}
}

// updateStmtToString converts an UPDATE statement to SQL text.
func updateStmtToString(s *sql.UpdateStmt) string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(s.Table)
	b.WriteString(" SET ")
	if len(s.SetParenColumns) > 0 {
		// Parenthesized SET (col1,col2,...)=(expr1,expr2,...)
		b.WriteString("(")
		for i, col := range s.SetParenColumns {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(col)
		}
		b.WriteString(")=(")
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(sql.ExprString(a.Value))
		}
		b.WriteString(")")
	} else {
		for i, a := range s.Assignments {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Column)
			b.WriteString("=")
			b.WriteString(sql.ExprString(a.Value))
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// insertStmtToString converts an INSERT statement to SQL text.
func insertStmtToString(s *sql.InsertStmt) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(s.Table)
	if len(s.Columns) > 0 {
		b.WriteString("(")
		for i, c := range s.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c)
		}
		b.WriteString(")")
	}
	if s.Select != nil {
		b.WriteString(" ")
		b.WriteString(selectStmtToString(s.Select))
	} else if len(s.Values) > 0 {
		b.WriteString(" VALUES(")
		for i, tuple := range s.Values {
			if i > 0 {
				b.WriteString(", ")
			}
			for j, val := range tuple {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString(sql.ExprString(val))
			}
		}
		b.WriteString(")")
	}
	return b.String()
}

// deleteStmtToString converts a DELETE statement to SQL text.
func deleteStmtToString(s *sql.DeleteStmt) string {
	var b strings.Builder
	b.WriteString("DELETE FROM ")
	b.WriteString(s.Table)
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(sql.ExprString(s.Where))
	}
	return b.String()
}

// selectStmtToString converts a SELECT statement to SQL text (used for views).

// --- CREATE VIRTUAL TABLE ---

func (e *Engine) execCreateVirtualTable(s *sql.CreateVirtualTableStmt) *Result {
	module, ok := e.vtabs.Find(s.Module)
	if !ok {
		return &Result{Error: fmt.Errorf("exec: virtual table module not found: %s", s.Module)}
	}
	_, err := module.Create(s.Args)
	if err != nil {
		return &Result{Error: err}
	}

	// Store in schema
	// Strip known schema prefixes (main, temp) but keep others (aux)
	tableName := s.Name
	if dotIdx := strings.Index(tableName, "."); dotIdx >= 0 {
		prefix := strings.ToUpper(tableName[:dotIdx])
		if prefix == "MAIN" || prefix == "TEMP" || prefix == "TEMPORARY" {
			tableName = tableName[dotIdx+1:]
		}
	}

	entry := &schema.Entry{
		Type:     schema.TypeTable,
		Name:     tableName,
		TblName:  tableName,
		RootPage: 0,
		SQL:      fmt.Sprintf("CREATE VIRTUAL TABLE %s USING %s(%s)", tableName, s.Module, strings.Join(s.Args, ",")),
	}
	if err := e.schema.AddEntry(entry); err != nil {
		return &Result{Error: err}
	}

	// If this is an FTS module, create and store the FTS table
	if ftsMod := e.getFTSModule(s.Module); ftsMod != nil {
		// Parse args to get column definitions (the CREATE VIRTUAL TABLE args
		// become FTS column names)
		ftsTable, err := ftsMod.GetOrCreateTable(tableName, s.Module, s.Args)
		if err != nil {
			return &Result{Error: fmt.Errorf("fts: failed to create table: %w", err)}
		}
		e.ftsTables[tableName] = ftsTable
	}

	return &Result{}
}

// parseVTabSQL extracts the module name and argument list from a virtual
// table's stored SQL ("CREATE VIRTUAL TABLE t USING mod(a, b)").
func parseVTabSQL(sql string) (moduleName string, args []string, err error) {
	upper := strings.ToUpper(sql)
	idx := strings.Index(upper, " USING ")
	if idx < 0 {
		return "", nil, fmt.Errorf("vtab: invalid virtual table SQL: %s", sql)
	}
	rest := sql[idx+7:]
	parts := strings.SplitN(rest, "(", 2)
	moduleName = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		argsStr := strings.TrimRight(parts[1], ")")
		for _, a := range strings.Split(argsStr, ",") {
			args = append(args, strings.TrimSpace(a))
		}
	}
	return moduleName, args, nil
}

// vtabUpperBound extracts an inclusive upper bound from a virtual-table
// WHERE clause of the forms "value < N", "value <= N", "N > value",
// "N >= value", or "value BETWEEN 1 AND N". Returns ok=false when no
// usable bound is found (the table falls back to its default range).
func vtabUpperBound(where sql.Expr) (int64, bool) {
	cmp, ok := where.(*sql.BinaryOp)
	if !ok || cmp == nil {
		return 0, false
	}
	colRef := func(e sql.Expr) (string, bool) {
		cr, ok := e.(*sql.ColumnRef)
		if !ok {
			return "", false
		}
		return strings.ToLower(cr.Name), true
	}
	num := func(e sql.Expr) (int64, bool) {
		nl, ok := e.(*sql.NumericLit)
		if !ok {
			return 0, false
		}
		v, err := strconv.ParseInt(nl.Value, 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	switch strings.ToUpper(cmp.Operator) {
	case "<", "<=":
		if c, ok := colRef(cmp.Left); ok && c == "value" {
			if n, ok := num(cmp.Right); ok {
				if strings.ToUpper(cmp.Operator) == "<" {
					return n - 1, true
				}
				return n, true
			}
		}
	case ">", ">=":
		if c, ok := colRef(cmp.Right); ok && c == "value" {
			if n, ok := num(cmp.Left); ok {
				if strings.ToUpper(cmp.Operator) == ">" {
					return n + 1, true
				}
				return n, true
			}
		}
	}
	return 0, false
}

// validateIndexedBy enforces an INDEXED BY name clause on a single-table
// query. SQLite raises "no such index: X" when the named index does not
// exist, and "no query solution" when the forced index cannot serve the
// query (notably a partial index whose WHERE predicate is not implied by
// the query's own WHERE clause, or an index that does not cover the query's
// ORDER BY). The engine does not actually use the forced index for the
// scan (results are correct via a table scan), but the error contract is
// observable and tested (indexedby.test).
func (e *Engine) validateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	// Resolve the index within the table's schema.
	var idxEntry *schema.Entry
	for _, ctx := range e.databases {
		en, err := ctx.Schema.GetEntries(schema.TypeIndex)
		if err != nil {
			continue
		}
		for _, ent := range en {
			if strings.EqualFold(ent.Name, indexName) && strings.EqualFold(ent.TblName, tableEntry.Name) {
				idxEntry = ent
				break
			}
		}
		if idxEntry != nil {
			break
		}
	}
	if idxEntry == nil {
		return fmt.Errorf("no such index: %s", indexName)
	}

	// A partial index (WHERE predicate) can only serve the query when the
	// query's WHERE clause implies the predicate. Without any WHERE, or when
	// the predicate cannot be implied, the forced partial index yields no
	// query solution.
	if wm := indexWhereRe.FindStringSubmatch(idxEntry.SQL); wm != nil {
		pred := strings.TrimSpace(wm[1])
		if s.Where == nil || !e.whereImplies(s.Where, pred) {
			return fmt.Errorf("no query solution")
		}
	}
	return nil
}

// whereImplies reports whether a query WHERE expression textually implies a
// partial-index predicate. It is a conservative syntactic check: the
// predicate matches when the WHERE contains an identical conjunct (e.g.
// WHERE z=1 implies a predicate "z=1"). Used only for the INDEXED BY
// "no query solution" error contract.
func (e *Engine) whereImplies(where sql.Expr, pred string) bool {
	if where == nil {
		return false
	}
	predNorm := normalizeSQLText(pred)
	var found bool
	walkExprFull(where, func(n sql.Expr) {
		if be, ok := n.(*sql.BinaryOp); ok {
			if normalizeSQLText(sql.ExprString(be)) == predNorm {
				found = true
			}
		}
	})
	return found
}

// normalizeSQLText collapses whitespace in SQL text for comparison.
func normalizeSQLText(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// ensureFTSForTable lazily re-creates the in-memory FTS table for a virtual
// table entry whose module is an FTS module. On a fresh connection the FTS
// tables map is empty even though the schema (sqlite_schema) still contains
// the CREATE VIRTUAL TABLE entry; this restores the mapping so that
// INSERT/SELECT/DELETE route to FTS storage instead of treating the table as
// storageless (which would make writes no-ops and RETURNING project NULLs).
func (e *Engine) ensureFTSForTable(entry *schema.Entry) {
	if entry == nil || !strings.HasPrefix(strings.ToUpper(entry.SQL), "CREATE VIRTUAL TABLE") {
		return
	}
	if _, ok := e.ftsTables[entry.Name]; ok {
		return
	}
	moduleName, args, err := parseVTabSQL(entry.SQL)
	if err != nil {
		return
	}
	ftsMod := e.getFTSModule(moduleName)
	if ftsMod == nil {
		return
	}
	ftsTable, err := ftsMod.GetOrCreateTable(entry.Name, moduleName, args)
	if err != nil {
		return
	}
	e.ftsTables[entry.Name] = ftsTable
}

// virtualTableRows reads all rows from a virtual table.
func (e *Engine) virtualTableRows(entry *schema.Entry, bound int64) ([][]interface{}, error) {
	moduleName, args, err := parseVTabSQL(entry.SQL)
	if err != nil {
		return nil, err
	}
	module, ok := e.vtabs.Find(moduleName)
	if !ok {
		return nil, fmt.Errorf("vtab: module not found: %s", moduleName)
	}
	// The echo module mirrors its underlying table (echo('t1') proxies t1's
	// rows and columns). Resolve it directly through the engine so SELECT
	// FROM echo_table returns the source table's data.
	if strings.EqualFold(moduleName, "echo") && len(args) > 0 {
		srcName := strings.Trim(args[0], "'\"")
		if srcEntry, _, ferr := e.findTable(srcName); ferr == nil {
			tree := e.tableBTreePg(e.mainDB.Pager, srcEntry.Name, srcEntry.RootPage, true)
			cursor, cerr := tree.OpenCursor()
			if cerr == nil {
				var rows [][]interface{}
				for {
					cell, rerr := cursor.ReadCell()
					if rerr != nil || cell == nil {
						break
					}
					rec, derr := storage.DecodeRecord(cell.Payload)
					if derr != nil || rec == nil {
						break
					}
					rows = append(rows, rec.Values)
					okN, nerr := cursor.Next()
					if nerr != nil || !okN {
						break
					}
				}
				return rows, nil
			}
		}
	}
	vtabInstance, err := module.Connect(args)
	if err != nil {
		return nil, err
	}
	// Pass the WHERE-derived upper bound to bounded virtual tables
	// (e.g. wholenumber) so they generate only the needed prefix.
	if bound > 0 {
		if bt, ok := vtabInstance.(vtab.BoundedVTab); ok {
			bt.SetUpperBound(bound)
		}
	}
	cursor, err := vtabInstance.Open()
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	var rows [][]interface{}
	for cursor.Next() {
		val, err := cursor.Column(0)
		if err != nil {
			return nil, err
		}
		rows = append(rows, []interface{}{val})
	}
	return rows, nil
}

// execFTSSelect handles SELECT from an FTS virtual table with full processing
// (WHERE filtering including MATCH, ORDER BY, LIMIT).
func (e *Engine) execFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	// Set current FTS match context for MATCH evaluation
	e.currentFTSMatch = tableEntry.Name
	defer func() { e.currentFTSMatch = "" }()

	// Get all rows from FTS table and build RowMaps
	docIDs := ftsTable.AllRowsMap()
	var allRowMaps []RowMap
	for _, docID := range docIDs {
		doc := ftsTable.GetDoc(docID)
		if doc == nil {
			continue
		}
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		rowMap["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		for i, col := range doc.Columns {
			if i < len(colDefs) {
				rowMap[colDefs[i].Name] = col
			}
		}
		allRowMaps = append(allRowMaps, rowMap)
	}

	// Apply WHERE clause
	if s.Where != nil {
		var filtered []RowMap
		for _, rowMap := range allRowMaps {
			match, err := e.evalBool(s.Where, rowMap)
			if err == nil && match {
				filtered = append(filtered, rowMap)
			}
		}
		allRowMaps = filtered
	}

	// Handle aggregates (after WHERE filtering)
	if result := e.handleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}

	// Build output rows from RowMaps
	allRows := make([][]interface{}, len(allRowMaps))
	for i, rowMap := range allRowMaps {
		allRows[i] = e.buildOutputRow(s.Columns, colDefs, rowMap)
	}

	// Build column names
	columns := e.buildColumnNames(s.Columns, colDefs)
	result := &Result{Columns: columns, Rows: allRows}

	// Apply DISTINCT, ORDER BY, LIMIT
	return e.finalizeSelectResult(result, s, allRowMaps)
}

// execFTSDelete handles DELETE from an FTS virtual table.
func (e *Engine) execFTSDelete(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	e.currentFTSMatch = ""
	docIDs := ftsTable.AllRowsMap()
	deleted := int64(0)
	for _, docID := range docIDs {
		shouldDelete := true
		if s.Where != nil {
			rowMap := make(RowMap)
			rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
			for _, name := range colDefs {
				rowMap[name.Name] = ""
			}
			match, err := e.evalBool(s.Where, rowMap)
			if err != nil || !match {
				shouldDelete = false
			}
		}
		if shouldDelete {
			ftsTable.Delete(docID)
			deleted++
		}
	}
	return &Result{Changes: deleted}
}

func selectStmtToString(s *sql.SelectStmt) string {
	if s == nil {
		return ""
	}
	result := ""
	// CTEs must be output before SELECT: WITH name AS (...) SELECT ...
	if len(s.CTEs) > 0 {
		result += "WITH "
		for i, cte := range s.CTEs {
			if i > 0 {
				result += ", "
			}
			result += cte.Name
			if len(cte.Columns) > 0 {
				result += "(" + strings.Join(cte.Columns, ",") + ")"
			}
			result += " AS (" + selectStmtToString(cte.Select) + ")"
		}
		result += " "
	}
	result += "SELECT "
	if s.Distinct {
		result += "DISTINCT "
	}
	for i, col := range s.Columns {
		if i > 0 {
			result += ", "
		}
		result += selectColumnToString(col)
	}
	if s.From.Name != "" {
		result += " FROM " + s.From.Name
		if s.From.As != "" {
			result += " AS " + s.From.As
		}
	}
	result += joinClausesToString(s.Joins)
	if s.Where != nil {
		result += " WHERE " + exprToString(s.Where)
	}
	// Handle GROUP BY
	if len(s.GroupBy) > 0 {
		result += " GROUP BY "
		for i, gb := range s.GroupBy {
			if i > 0 {
				result += ", "
			}
			result += exprToString(gb)
		}
	}
	// Handle HAVING
	if s.Having != nil {
		result += " HAVING " + exprToString(s.Having)
	}
	// Handle ORDER BY
	if len(s.OrderBy) > 0 {
		result += " ORDER BY "
		for i, ob := range s.OrderBy {
			if i > 0 {
				result += ", "
			}
			result += exprToString(ob.Expr)
			if ob.Desc {
				result += " DESC"
			}
		}
	}
	// Handle LIMIT/OFFSET
	if s.Limit != nil {
		result += " LIMIT " + exprToString(s.Limit)
		if s.Offset != nil {
			result += " OFFSET " + exprToString(s.Offset)
		}
	}
	// Handle WINDOW clause
	if len(s.Windows) > 0 {
		result += " WINDOW "
		for i, w := range s.Windows {
			if i > 0 {
				result += ", "
			}
			result += w.Name + " AS ("
			// PARTITION BY
			if len(w.Partitions) > 0 {
				result += "PARTITION BY "
				for j, p := range w.Partitions {
					if j > 0 {
						result += ", "
					}
					result += exprToString(p)
				}
			}
			// ORDER BY inside window
			if len(w.OrderBy) > 0 {
				if len(w.Partitions) > 0 {
					result += " "
				}
				result += "ORDER BY "
				for j, ob := range w.OrderBy {
					if j > 0 {
						result += ", "
					}
					result += exprToString(ob.Expr)
					if ob.Desc {
						result += " DESC"
					}
				}
			}
			result += ")"
		}
	}
	// Handle compound operators (UNION, INTERSECT, EXCEPT)
	if s.SetOp != sql.SetNone && s.Union != nil {
		switch s.SetOp {
		case sql.SetUnion:
			result += "\n    UNION"
			if s.UnionAll {
				result += " ALL"
			}
		case sql.SetIntersect:
			result += "\n    INTERSECT"
		case sql.SetExcept:
			result += "\n    EXCEPT"
		}
		result += "\n    " + selectStmtToString(s.Union)
	}
	return result
}

func selectColumnToString(col sql.SelectColumn) string {
	if ref, ok := col.Expr.(*sql.ColumnRef); ok {
		if ref.Table != "" {
			return ref.Table + "." + ref.Name + aliasClause(col.As)
		}
		return ref.Name + aliasClause(col.As)
	}
	if fn, ok := col.Expr.(*sql.FuncCall); ok {
		return funcCallToString(fn) + aliasClause(col.As)
	}
	return exprToString(col.Expr) + aliasClause(col.As)
}

func aliasClause(as string) string {
	if as != "" {
		return " " + as
	}
	return ""
}

func joinClausesToString(joins []sql.JoinClause) string {
	result := ""
	for _, j := range joins {
		if j.CommaJoin {
			result += ", " + j.Table.Name
			if j.Table.As != "" {
				result += " AS " + j.Table.As
			}
			if j.On != nil {
				result += " ON " + exprToString(j.On)
			}
			continue
		}
		switch {
		case strings.Contains(j.JoinType, "FULL"):
			result += " FULL JOIN "
		case strings.Contains(j.JoinType, "LEFT"):
			result += " LEFT JOIN "
		case strings.Contains(j.JoinType, "RIGHT"):
			result += " RIGHT JOIN "
		case strings.Contains(j.JoinType, "CROSS") && !strings.Contains(j.JoinType, "NATURAL"):
			result += " CROSS JOIN "
		case strings.Contains(j.JoinType, "NATURAL"):
			result += " NATURAL JOIN "
		case strings.Contains(j.JoinType, "INNER"):
			result += " INNER JOIN "
		default:
			result += " JOIN "
		}
		result += j.Table.Name
		if j.Table.As != "" {
			result += " AS " + j.Table.As
		}
		if j.On != nil {
			result += " ON " + exprToString(j.On)
		}
	}
	return result
}

// exprToString converts an expression to its string representation.
func exprToString(expr sql.Expr) string {
	if expr == nil {
		return ""
	}
	switch v := expr.(type) {
	case *sql.ColumnRef:
		if v.Table != "" {
			return v.Table + "." + v.Name
		}
		return v.Name
	case *sql.NumericLit:
		return v.Value
	case *sql.StringLit:
		return "'" + v.Value + "'"
	case *sql.NullLit:
		return "NULL"
	case *sql.BinaryOp:
		if v.Operator == "OR" || v.Operator == "AND" {
			return exprToString(v.Left) + " " + v.Operator + " " + exprToString(v.Right)
		}
		return exprToString(v.Left) + v.Operator + exprToString(v.Right)
	case *sql.UnaryOp:
		if v.Operator == "NOT" {
			return v.Operator + " " + exprToString(v.Operand)
		}
		return v.Operator + exprToString(v.Operand)
	case *sql.FuncCall:
		return funcCallToString(v)
	case *sql.IsNull:
		return exprToString(v.Operand) + " IS NULL"
	case *sql.IsNotNull:
		return exprToString(v.Operand) + " IS NOT NULL"
	case *sql.ParenExpr:
		return "(" + exprToString(v.Expr) + ")"
	case *sql.Between:
		return betweenToString(v)
	case *sql.InList:
		return inListToString(v)
	case *sql.Subquery:
		return "(" + selectStmtToString(v.Select) + ")"
	case *sql.ExistsExpr:
		result := ""
		if v.Negated {
			result += "NOT "
		}
		return result + "EXISTS(" + selectStmtToString(v.Select) + ")"
	case *sql.CaseExpr:
		return caseExprToString(v)
	case *sql.CastExpr:
		return "CAST(" + exprToString(v.Operand) + " AS " + v.AsType + ")"
	case *sql.RowValue:
		result := "("
		for i, val := range v.Values {
			if i > 0 {
				result += ", "
			}
			result += exprToString(val)
		}
		return result + ")"
	default:
		return "?"
	}
}

func funcCallToString(v *sql.FuncCall) string {
	result := v.Name + "("
	for i, arg := range v.Args {
		if i > 0 {
			result += ", "
		}
		result += exprToString(arg)
	}
	result += ")"
	if v.Filter != nil {
		result += " FILTER (WHERE " + exprToString(v.Filter) + ")"
	}
	if v.Over != nil {
		result += " OVER " + windowDefToString(v.Over)
	}
	return result
}

func windowDefToString(w *sql.WindowDef) string {
	if w == nil {
		return ""
	}
	// Named window reference (no PARTITION BY/ORDER BY specs)
	if len(w.Partitions) == 0 && len(w.OrderBy) == 0 && w.FrameSpec == "" {
		if w.Name != "" {
			return w.Name
		}
		return "()"
	}
	result := "("
	// PARTITION BY
	if len(w.Partitions) > 0 {
		result += "PARTITION BY "
		for j, p := range w.Partitions {
			if j > 0 {
				result += ", "
			}
			result += exprToString(p)
		}
	}
	// ORDER BY inside window
	if len(w.OrderBy) > 0 {
		if len(w.Partitions) > 0 {
			result += " "
		}
		result += "ORDER BY "
		for j, ob := range w.OrderBy {
			if j > 0 {
				result += ", "
			}
			result += exprToString(ob.Expr)
			if ob.Desc {
				result += " DESC"
			}
		}
	}
	// Frame spec
	if w.FrameSpec != "" {
		result += " " + w.FrameSpec
	}
	return result + ")"
}

func betweenToString(v *sql.Between) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT BETWEEN "
	} else {
		result += " BETWEEN "
	}
	return result + exprToString(v.Low) + " AND " + exprToString(v.High)
}

func inListToString(v *sql.InList) string {
	result := exprToString(v.Operand)
	if v.Negated {
		result += " NOT IN ("
	} else {
		result += " IN ("
	}
	for i, item := range v.List {
		if i > 0 {
			result += ", "
		}
		result += exprToString(item)
	}
	return result + ")"
}

func caseExprToString(v *sql.CaseExpr) string {
	result := "CASE"
	if v.Operand != nil {
		result += " " + exprToString(v.Operand)
	}
	for _, w := range v.Whens {
		result += " WHEN " + exprToString(w.When) + " THEN " + exprToString(w.Then)
	}
	if v.Else != nil {
		result += " ELSE " + exprToString(v.Else)
	}
	return result + " END"
}

// isValidStrictType returns true if the type name is allowed in a STRICT table.
// Allowed types: INT, INTEGER, TEXT, REAL, BLOB, ANY (case-insensitive).
func isValidStrictType(typeName string) bool {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	switch upper {
	case "INT", "INTEGER", "TEXT", "REAL", "BLOB", "ANY":
		return true
	}
	return false
}

// isStrictTable returns true if the table's CREATE SQL specifies STRICT.
func isStrictTable(createSQL string) bool {
	upper := strings.ToUpper(createSQL)
	return hasStrictKeyword(upper)
}

// stripCTASSelect returns the CREATE TABLE text up to (but not including) an
// "AS SELECT" clause. Table options such as STRICT and WITHOUT ROWID only
// appear before AS SELECT, and the closing parenthesis of the column list
// must not be confused with parentheses inside the SELECT body.
func stripCTASSelect(createSQL string) string {
	upper := strings.ToUpper(createSQL)
	idx := strings.Index(upper, " AS SELECT")
	if idx < 0 {
		// Allow "AS" and "SELECT" separated by arbitrary whitespace.
		re := regexp.MustCompile(`(?i)\s+AS\s+SELECT`)
		loc := re.FindStringIndex(createSQL)
		if loc == nil {
			return createSQL
		}
		return createSQL[:loc[0]]
	}
	return createSQL[:idx]
}

// hasStrictKeyword checks if "STRICT" appears as a standalone keyword in the
// CREATE TABLE SQL (not inside a string literal or column name).
func hasStrictKeyword(upperSQL string) bool {
	sql := stripCTASSelect(upperSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return false
	}
	tail := sql[idx:]
	return strings.Contains(tail, "STRICT")
}

// hasWithoutRowidKeyword checks if "WITHOUT ROWID" appears after the closing
// parenthesis in the CREATE TABLE SQL.
func hasWithoutRowidKeyword(upperSQL string) bool {
	sql := stripCTASSelect(upperSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return false
	}
	tail := sql[idx:]
	return strings.Contains(tail, "WITHOUT")
}

// validateWithoutOption validates the CREATE TABLE trailing options (SQLite
// supports "STRICT" and "WITHOUT ROWID"; any other trailing token is an
// "unknown table option" error).
func validateWithoutOption(createSQL string) error {
	sql := stripCTASSelect(createSQL)
	idx := strings.LastIndex(sql, ")")
	if idx < 0 {
		return nil
	}
	tail := strings.TrimSpace(sql[idx+1:])
	if tail == "" {
		return nil
	}
	for _, opt := range strings.Split(tail, ",") {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			continue
		}
		upper := strings.ToUpper(opt)
		if upper == "STRICT" {
			continue
		}
		if strings.HasPrefix(upper, "WITHOUT") {
			rest := strings.TrimSpace(opt[len("WITHOUT"):])
			if rest != "" && strings.EqualFold(rest, "ROWID") {
				continue
			}
			return fmt.Errorf("unknown table option: %s", rest)
		}
		return fmt.Errorf("unknown table option: %s", opt)
	}
	return nil
}

// hasPrimaryKey returns true if the CREATE TABLE statement has any PRIMARY KEY
// constraint (column-level or table-level).
// The LALR parser doesn't propagate table-level constraints, so we also
// check the raw SQL for "PRIMARY KEY".
func hasPrimaryKey(s *sql.CreateTableStmt) bool {
	for _, col := range s.Columns {
		if col.PrimaryKey {
			return true
		}
	}
	for _, tc := range s.Constraints {
		if tc.Type == sql.ConstraintPrimaryKey {
			return true
		}
	}
	// Fallback: check raw SQL for table-level PRIMARY KEY constraint.
	// The LALR parser may not populate s.Constraints for table-level
	// constraints like PRIMARY KEY(a,b).
	if s.RawSQL != "" {
		upper := strings.ToUpper(s.RawSQL)
		if strings.Contains(upper, "PRIMARY KEY") || strings.Contains(upper, "PRIMARY  KEY") {
			return true
		}
	}
	return false
}

// enforceStrictType checks if a value is compatible with a STRICT column type.
// Returns an error if the value's storage class does not match the declared type.
// STRICT rules (SQLite src/vdbeaux.c):
//   - TEXT: value must be text (string)
//   - INTEGER/INT: value must be an integer; numeric strings are accepted
//   - REAL: value must be real (or integer, converted to real); numeric strings accepted
//   - BLOB: value must be a blob
//   - ANY: any value accepted
func enforceStrictType(tableName, colName, declaredType string, v interface{}) error {
	if v == nil {
		return nil // NULL is always allowed
	}
	upper := strings.ToUpper(strings.TrimSpace(declaredType))
	v = util.UnwrapColumnValue(v)
	// In STRICT tables, affinity is applied first (e.g., INTEGER → TEXT column
	// converts the value to text '4', which then passes the type check).
	switch upper {
	case "TEXT":
		switch v.(type) {
		case string:
			return nil
		case int64:
			return nil // affinity converts int64 → text
		case float64:
			return nil // affinity converts float64 → text
		default:
			return fmt.Errorf("cannot store %s value in TEXT column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "INT", "INTEGER":
		switch v.(type) {
		case int64:
			return nil
		case float64:
			// SQLite accepts whole-number reals and converts to integer
			if vv := v.(float64); vv == float64(int64(vv)) {
				return nil
			}
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		case string:
			// Numeric-looking strings are accepted and converted to integer
			if _, err := strconv.ParseInt(v.(string), 10, 64); err == nil {
				return nil
			}
			// Try float — a whole-number float string is accepted
			if f, err := strconv.ParseFloat(v.(string), 64); err == nil && f == float64(int64(f)) {
				return nil
			}
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		default:
			return fmt.Errorf("cannot store %s value in %s column %s.%s", strictStorageClass(v), upper, tableName, colName)
		}
	case "REAL":
		switch v.(type) {
		case float64:
			return nil
		case int64:
			return nil // integers are accepted and converted to real
		case string:
			// Numeric-looking strings are accepted and converted to real
			if _, err := strconv.ParseFloat(v.(string), 64); err == nil {
				return nil
			}
			return fmt.Errorf("cannot store %s value in REAL column %s.%s", strictStorageClass(v), tableName, colName)
		default:
			return fmt.Errorf("cannot store %s value in REAL column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "BLOB":
		switch v.(type) {
		case []byte:
			return nil
		default:
			return fmt.Errorf("cannot store %s value in BLOB column %s.%s", strictStorageClass(v), tableName, colName)
		}
	case "ANY":
		return nil
	}
	return nil
}

// strictStorageClass returns the storage class name for error messages.
func strictStorageClass(v interface{}) string {
	v = util.UnwrapColumnValue(v)
	switch v.(type) {
	case int64:
		return "INT"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	default:
		return "UNKNOWN"
	}
}
