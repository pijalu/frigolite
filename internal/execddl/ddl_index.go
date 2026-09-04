package execddl

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- CREATE INDEX ---

// dqsAllowedDDL reports whether double-quoted strings are permitted in DDL
// statements. SQLite allows them when the DQS DDL setting is enabled, or when
// writable_schema is on and the DQS DML setting is enabled (the legacy schema
// load bypass — resolve.c areDoubleQuotedStringsEnabled + db->init.busy).
func (e *DDLExecutor) dqsAllowedDDL() bool {
	return e.ctx.DQSAllowDDL() || (e.ctx.WritableSchema() && e.ctx.DQSAllowDML())
}

// validateDQSExpr returns an error when a double-quoted identifier in expr
// does not resolve to a column of the given table. SQLite's DQS
// (double-quoted string) fallback converts such identifiers to string
// literals only when DQS is enabled; when disabled for DDL they are errors
// ("no such column: \"X\" - should this be a string literal in single-quotes?").
func (e *DDLExecutor) validateDQSExpr(expr sql.Expr, colDefs []sql.ColumnDef) error {
	var quoted []*sql.ColumnRef
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if cr, ok := n.(*sql.ColumnRef); ok && cr.Quoted {
			quoted = append(quoted, cr)
		}
	})
	for _, cr := range quoted {
		if execdml.CdIndex(colDefs, cr.Name) < 0 {
			return fmt.Errorf("no such column: \"%s\" - should this be a string literal in single-quotes?", cr.Name)
		}
	}
	return nil
}

func (e *DDLExecutor) execCreateIndex(s *sql.CreateIndexStmt) *Result {
	e.ctx.InvalidateTableCaches()
	if err := e.ctx.Authorize(auth.ActionCreateIndex, s.Name, s.Table, "", ""); err != nil {
		return &Result{Error: err}
	}
	// Resolve schema prefix from index name (e.g. "aux.i1" -> schema "aux", name "i1")
	ctx, indexName := e.resolveIndexSchema(s.Name)
	if ctx == nil {
		return &Result{Error: fmt.Errorf("unknown database %s", schemaPrefixOf(s.Name))}
	}

	tableEntry, tableCtx, err := e.resolveIndexTable(ctx, s)
	if err != nil {
		return &Result{Error: err}
	}

	// SQLite refuses to index tables whose names begin with "sqlite_"
	// (src/build.c:4034-4038), except during schema init. Error:
	// "table %s may not be indexed".
	if strings.HasPrefix(strings.ToLower(tableEntry.Name), "sqlite_") {
		return &Result{Error: fmt.Errorf("table %s may not be indexed", tableEntry.Name)}
	}

	// Resolve the table's column definitions up front: DQS validation and
	// collation checks both need them, and both must run before the index
	// entry is written to the schema (an error must not leak a partial index).
	colDefs := e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL)

	// CREATE INDEX IF NOT EXISTS: silently ignore when an index with the
	// same name already exists in the target schema (SQLite checks the
	// schema by name; a table with the name still errors).
	if s.IfNotExists {
		if _, _, findErr := e.ctx.FindIndex(indexName); findErr == nil {
			return &Result{}
		}
	}

	// A duplicate index name in the TARGET schema is an error unless IF NOT
	// EXISTS: "index i1 already exists".
	if !s.IfNotExists {
		if existing, _ := ctx.Schema.FindIndex(indexName); existing != nil {
			return &Result{Error: fmt.Errorf("index %s already exists", indexName)}
		}
	}

	if res := e.validateIndexExpressions(s, colDefs); res != nil {
		return res
	}

	if res := e.validateIndexSchemaSafety(s, tableCtx); res != nil {
		return res
	}

	// Collation validation must run before the index entry is written to the
	// schema: a failed collation check must not leak a partial index entry.
	// This covers both the index columns' declared collations (from the table
	// definition) and explicit COLLATE operators in the key expressions and
	// the partial-index WHERE clause (SQLite resolves these at CREATE INDEX
	// compile time).
	if res := e.validateIndexCollations(s, colDefs); res != nil {
		return res
	}

	// Allocate root page for index
	pg, perr := tableCtx.Pager.AllocateRootPage()
	if perr != nil {
		return &Result{Error: perr}
	}
	initIndexRootPage(pg, tableCtx.Pager.PageSize())
	if err := tableCtx.Pager.WritePage(pg); err != nil {
		return &Result{Error: err}
	}
	sqlStr := e.indexEntrySQL(s, indexName)
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

	tree := e.ctx.TableBTreePg(tableCtx.Pager, tableEntry.Name, e.ctx.RootPagePg(tableCtx.Pager, tableEntry.Name, tableEntry.RootPage), true)

	if res := e.populateIndexFromRows(tableCtx, tableEntry, indexName, s, colDefs, pg, tree); res != nil {
		return res
	}

	return &Result{Changes: 0}
}

// initIndexRootPage initializes a freshly allocated index root page: zero the
// data, set the leaf-index page type, and write a valid header (freeblock=0,
// cellCount=0, contentOffset=pageSize-4) so a reused page (from a dropped
// table/index) does not retain stale cells and ParsePage accepts the page. The
// content-offset header mirrors CREATE TABLE's page initialization; without it
// a fresh page's zeroed content offset fails ParsePage's free-space
// consistency check ("database disk image is malformed").
func initIndexRootPage(pg *pager.Page, pageSize uint32) {
	for i := range pg.Data {
		pg.Data[i] = 0
	}
	pg.Data[0] = storage.PageTypeLeafIndex
	coff := 0
	if pg.PageNum == 1 {
		coff = 100
	}
	// Header: type(1) freeblock(2) cellCount(2)=0 contentOffset(2)=pageSize-4
	binary.BigEndian.PutUint16(pg.Data[coff+3:coff+5], 0)
	binary.BigEndian.PutUint16(pg.Data[coff+5:coff+7], uint16(int(pageSize)-4))
}

// validateIndexCollations resolves each index column's collation from the
// table definition and rejects unknown collation sequences (SQLite does this
// at CREATE INDEX compile time). Index columns may be named or 1-based
// integer positions. It also validates explicit COLLATE operators in the
// index key expressions and the partial-index WHERE clause — SQLite resolves
// those collation names when the index is created, so an unknown collation
// must abort CREATE INDEX before any schema entry is written.
func (e *DDLExecutor) validateIndexCollations(s *sql.CreateIndexStmt, colDefs []sql.ColumnDef) *Result {
	for _, ic := range s.Columns {
		coll := resolveIndexColumnCollation(ic.Name, colDefs)
		if err := e.ctx.CheckCollationString(coll); err != nil {
			return &Result{Error: err}
		}
	}
	// Explicit COLLATE operators in the key expressions and the WHERE clause
	// (e.g. "WHERE b IS 'abc' COLLATE g") are compiled to a collation lookup
	// at CREATE INDEX time. Validate each named collation before allocating
	// the index root page.
	exprs := make([]sql.Expr, 0, len(s.Terms)+1)
	for _, term := range s.Terms {
		exprs = append(exprs, term.Expr)
	}
	if s.Where != nil {
		exprs = append(exprs, s.Where)
	}
	if err := e.validateCollateExprs(exprs); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// validateCollateExprs walks each expression for COLLATE operators and
// rejects unknown collation sequences.
func (e *DDLExecutor) validateCollateExprs(exprs []sql.Expr) error {
	for _, expr := range exprs {
		if err := e.validateCollateExpr(expr); err != nil {
			return err
		}
	}
	return nil
}

// validateCollateExpr walks one expression for COLLATE operators and rejects
// unknown collation sequences.
func (e *DDLExecutor) validateCollateExpr(expr sql.Expr) error {
	var checkErr error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if checkErr != nil {
			return
		}
		bo, ok := n.(*sql.BinaryOp)
		if !ok || bo.Operator != "COLLATE" {
			return
		}
		if sl, ok := bo.Right.(*sql.StringLit); ok {
			if err := e.ctx.CheckCollationString(sl.Value); err != nil {
				checkErr = err
			}
		}
	})
	return checkErr
}

// resolveIndexColumnCollation finds a column's collation by name or 1-based
// position.
func resolveIndexColumnCollation(name string, colDefs []sql.ColumnDef) string {
	if n, err := strconv.Atoi(name); err == nil && n >= 1 && n <= len(colDefs) {
		return colDefs[n-1].Collate
	}
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return cd.Collate
		}
	}
	return ""
}

// populateIndexFromRows scans the table, evaluates index keys per row, and
// builds + inserts the index b-tree, enforcing UNIQUE against existing rows.
func (e *DDLExecutor) populateIndexFromRows(tableCtx *DatabaseContext, tableEntry *schema.Entry, indexName string, s *sql.CreateIndexStmt, colDefs []sql.ColumnDef, pg *pager.Page, tree *btree.BTree) *Result {
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
	keyExprs := e.indexKeyExprs(s)
	allColumnKeys := e.allKeysAreColumns(keyExprs)

	for {
		done, err := e.indexRowStep(tableCtx, tableEntry, indexName, s, colDefs, keyExprs, seenKeys, allColumnKeys, idxTree, cursor)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}

	return nil
}

// indexRowStep reads the next table cell, decides whether the row joins a
// partial index, and inserts its key. It returns (true, nil) at end of scan
// (or a decode error), and an error result on evaluation/insert failure.
func (e *DDLExecutor) indexRowStep(tableCtx *DatabaseContext, tableEntry *schema.Entry, indexName string, s *sql.CreateIndexStmt, colDefs []sql.ColumnDef, keyExprs []string, seenKeys map[string]bool, allColumnKeys bool, idxTree *btree.BTree, cursor *btree.Cursor) (bool, *Result) {
	cell, err := cursor.ReadCell()
	if err != nil {
		return true, nil
	}
	rec, err := storage.DecodeRecord(cell.Payload)
	if err != nil {
		return true, nil
	}
	row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
	action, res := e.indexRowAction(s, row)
	if res != nil {
		// The schema entry was added before the row scan; a failed
		// partial-index WHERE evaluation (e.g. non-deterministic use of
		// date()) must not leak a partial index (SQLite rolls the whole
		// CREATE INDEX statement back).
		_ = tableCtx.Schema.RemoveEntry(indexName)
		return false, res
	}
	if action == indexActionInsert {
		if res := e.insertIndexKeyRow(tableCtx, tableEntry, indexName, s, row, colDefs, keyExprs, cell.RowID, seenKeys, allColumnKeys, idxTree); res != nil {
			return false, res
		}
	}
	ok, err := cursor.Next()
	if err != nil || !ok {
		return true, nil
	}
	return false, nil
}

// insertIndexKeyRow computes a row's index key values, enforces UNIQUE, and
// encodes + inserts the key into the index b-tree.
func (e *DDLExecutor) insertIndexKeyRow(tableCtx *DatabaseContext, tableEntry *schema.Entry, indexName string, s *sql.CreateIndexStmt, row RowMap, colDefs []sql.ColumnDef, keyExprs []string, rowID int64, seenKeys map[string]bool, allColumnKeys bool, idxTree *btree.BTree) *Result {
	indexValues, res := e.buildIndexValues(row, colDefs, keyExprs, rowID)
	if res != nil {
		// SQLite rolls back the CREATE INDEX on an evaluation error
		// (e.g. integer overflow); remove the schema entry first.
		_ = tableCtx.Schema.RemoveEntry(indexName)
		return res
	}
	if res := e.enforceIndexUnique(s, tableCtx, tableEntry, indexName, indexValues, keyExprs, seenKeys, allColumnKeys); res != nil {
		return res
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
	return nil
}

// indexKeyExprs returns the index key expressions: plain column names or
// 1-based positions from s.Columns when the parser captured them (plain-column
// indexes), else the full expression keys from s.Terms (expression indexes).
func (e *DDLExecutor) indexKeyExprs(s *sql.CreateIndexStmt) []string {
	keyExprs := make([]string, 0, len(s.Columns))
	for _, ic := range s.Columns {
		keyExprs = append(keyExprs, ic.Name)
	}
	if len(keyExprs) == 0 && len(s.Terms) > 0 {
		for _, term := range s.Terms {
			keyExprs = append(keyExprs, sql.ExprString(term.Expr))
		}
	}
	return keyExprs
}

// indexEntrySQL builds the SQL string stored in the schema for an index:
// the original statement verbatim when available, falling back to the AST
// rendering. IF NOT EXISTS is stripped, matching SQLite's stored-SQL behavior.
func (e *DDLExecutor) indexEntrySQL(s *sql.CreateIndexStmt, indexName string) string {
	sqlStr := strings.TrimSpace(s.RawSQL)
	if s.IfNotExists {
		sqlStr = stripIfNotExists(sqlStr)
	}
	if sqlStr == "" {
		sqlStr = execquery.BuildIndexSQL(indexName, s.Table, s.Columns, s.Unique, s.Where)
	}
	// A schema-qualified index name (CREATE INDEX temp.t2i1 ...) must be
	// stored UNQUALIFIED in sqlite_master (SQLite strips the schema prefix:
	// the schema is implicit in which sqlite_master the row lives in).
	sqlStr = stripSchemaPrefixFromDDL(sqlStr, indexName)
	return sqlStr
}

// stripSchemaPrefixFromDDL rewrites "CREATE INDEX temp.t2i1 ON ..." to
// "CREATE INDEX t2i1 ON ..." (drop the schema qualifier before the object
// name). It leaves the SQL unchanged when the name is already unqualified.
func stripSchemaPrefixFromDDL(sqlStr, bareName string) string {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, strings.ToUpper(bareName))
	if idx < 0 {
		return sqlStr
	}
	// Check the character before the bare name is a '.' (schema qualifier).
	if idx == 0 || sqlStr[idx-1] != '.' {
		return sqlStr
	}
	// Find the start of the schema qualifier (the token before the dot).
	start := idx - 1
	for start > 0 && isIdentChar(sqlStr[start-1]) {
		start--
	}
	return sqlStr[:start] + sqlStr[idx:]
}

// isIdentChar reports whether c can appear in a SQL identifier.
func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// allKeysAreColumns reports whether every index key is a plain column name or
// 1-based position (as opposed to an expression).
func (e *DDLExecutor) allKeysAreColumns(keyExprs []string) bool {
	for _, ke := range keyExprs {
		if !isPlainColumnKey(ke) {
			return false
		}
	}
	return true
}

// indexActionSkip indicates a row is excluded from a partial index;
// indexActionInsert indicates the row should be inserted into the index.
type indexAction int

const (
	indexActionInsert indexAction = iota
	indexActionSkip
)

// indexRowAction decides whether a row participates in a partial index by
// evaluating the WHERE predicate. Non-partial indexes include every row. An
// evaluation error returns a result.
func (e *DDLExecutor) indexRowAction(s *sql.CreateIndexStmt, row RowMap) (indexAction, *Result) {
	if s.Where == nil {
		return indexActionInsert, nil
	}
	var inIndex bool
	var whereErr error
	function.WithPureContext("index", func() error {
		inIndex, whereErr = e.ctx.EvalBool(s.Where, row)
		return whereErr
	})
	if whereErr != nil {
		return indexActionSkip, &Result{Error: whereErr}
	}
	if inIndex {
		return indexActionInsert, nil
	}
	return indexActionSkip, nil
}

// buildIndexValues computes the index key values for a row:
// [indexed_col1, ..., indexed_colN, rowid].
func (e *DDLExecutor) buildIndexValues(row RowMap, colDefs []sql.ColumnDef, keyExprs []string, rowID int64) ([]interface{}, *Result) {
	indexValues := make([]interface{}, 0, len(keyExprs)+1)
	for _, ke := range keyExprs {
		kv, kerr := e.indexKeyForCreate(row, colDefs, ke)
		if kerr != nil {
			return nil, &Result{Error: kerr}
		}
		indexValues = append(indexValues, kv)
	}
	indexValues = append(indexValues, rowID)
	return indexValues, nil
}

// enforceIndexUnique detects duplicate keys against existing rows for a
// UNIQUE index and records newly-seen keys. NULL keys never conflict.
func (e *DDLExecutor) enforceIndexUnique(s *sql.CreateIndexStmt, tableCtx *DatabaseContext, tableEntry *schema.Entry, indexName string, indexValues []interface{}, keyExprs []string, seenKeys map[string]bool, allColumnKeys bool) *Result {
	if !s.Unique {
		return nil
	}
	hasNull := false
	var keyParts []string
	for _, kv := range indexValues[:len(indexValues)-1] {
		// The row-map value may be a ColumnValue wrapper; unwrap so a stored
		// NULL is detected (multiple NULLs are allowed in a UNIQUE index —
		// SQLite treats NULLs as distinct).
		if util.UnwrapColumnValue(kv) == nil {
			hasNull = true
			break
		}
		keyParts = append(keyParts, util.SQLiteValueString(util.UnwrapColumnValue(kv)))
	}
	if hasNull {
		return nil
	}
	key := strings.Join(keyParts, "\x00")
	if seenKeys[key] {
		// The schema entry was added before the key scan; remove it so a
		// failed CREATE UNIQUE INDEX does not leak a partial index (SQLite
		// rolls the whole statement back).
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
	return nil
}

// resolveIndexSchema resolves the schema prefix from an index name
// (e.g. "aux.i1" -> schema "aux", name "i1"). It returns nil context for an
// unknown non-main/non-temp schema; MAIN/TEMP prefixes resolve to mainDB.
func (e *DDLExecutor) resolveIndexSchema(rawName string) (*DatabaseContext, string) {
	ctx := e.ctx.MainDB()
	indexName := rawName
	if dotIdx := strings.Index(rawName, "."); dotIdx >= 0 {
		prefix := rawName[:dotIdx]
		schemaUpper := strings.ToUpper(prefix)
		if schemaUpper == "TEMP" || schemaUpper == "TEMPORARY" {
			// A temp-qualified index name targets the temp schema (CREATE
			// INDEX temp.t2i1 ...). Resolve it so the duplicate-name check and
			// the schema entry land in temp, not main.
			if db := e.ctx.GetDB("temp"); db != nil {
				ctx = db
			}
		} else if schemaUpper != "MAIN" {
			if db := e.ctx.GetDB(prefix); db != nil {
				ctx = db
			} else {
				return nil, rawName
			}
		}
		indexName = rawName[dotIdx+1:]
	}
	return ctx, indexName
}

// schemaPrefixOf returns the schema prefix portion of a possibly-qualified
// name (everything before the first dot, or the whole name if unqualified).
func schemaPrefixOf(name string) string {
	if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
		return name[:dotIdx]
	}
	return name
}

// resolveIndexTable finds the target table for a CREATE INDEX and, when the
// index names an explicit schema, re-resolves the table within that schema.
func (e *DDLExecutor) resolveIndexTable(ctx *DatabaseContext, s *sql.CreateIndexStmt) (*schema.Entry, *DatabaseContext, error) {
	tableEntry, tableCtx, err := e.ctx.FindTable(s.Table)
	if err != nil {
		return nil, nil, err
	}
	// If the index has an explicit schema prefix, the table must be resolved
	// in that same schema (matching SQLite behaviour: CREATE INDEX aux.i4 ON t4
	// resolves t4 within the aux database). Otherwise tableEntry.RootPage would
	// belong to a different database than tableCtx.Pager, causing "page out of
	// range" errors.
	if ctx != e.ctx.MainDB() && ctx != tableCtx {
		_, objName := execdml.ParseSchemaName(s.Table)
		if entry, findErr := ctx.Schema.FindTable(objName); findErr == nil {
			tableEntry = entry
		}
		tableCtx = ctx
	}
	return tableEntry, tableCtx, nil
}

// validateIndexExpressions runs DQS validation and index-key-expression
// validation for a CREATE INDEX before any schema write.
func (e *DDLExecutor) validateIndexExpressions(s *sql.CreateIndexStmt, colDefs []sql.ColumnDef) *Result {
	// DDL double-quoted-string (DQS) validation: with DQS disabled for DDL, a
	// double-quoted identifier in an index key or WHERE clause that does not
	// resolve to a table column is an error. writable_schema + DQS DML allows
	// the DDL (legacy schema load bypass).
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
	// functions (random(), julianday('now',...)), subqueries, window
	// functions, and other prohibited constructs in index expressions
	// (build.c sqlite3CreateIndex / sqlite3ExprIsConstantOrFunction).
	for _, term := range s.Terms {
		if err := validateIndexKeyExpr(term.Expr); err != nil {
			return &Result{Error: err}
		}
	}
	if s.Where != nil {
		if err := validateIndexKeyExpr(s.Where); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// validateIndexSchemaSafety rejects CREATE INDEX statements whose key
// expressions or partial-index WHERE clause use functions unsafe under the
// current trusted_schema setting (trustschema1-1.410/1.420): a
// SQLITE_DIRECTONLY function is never allowed; a non-innocuous user function
// is allowed only while trusted_schema=ON.
func (e *DDLExecutor) validateIndexSchemaSafety(s *sql.CreateIndexStmt, tableCtx *DatabaseContext) *Result {
	// TEMP-schema objects are always trusted (trustschema1-1.440: a temp
	// index may use the directonly f3 even with trusted_schema=OFF).
	if tableCtx != nil && tableCtx.IsTemp {
		return nil
	}
	check := func(expr sql.Expr) *Result {
		if name := e.schemaUnsafeExpr(expr); name != "" {
			return &Result{Error: fmt.Errorf("unsafe use of %s()", name)}
		}
		return nil
	}
	for _, term := range s.Terms {
		if res := check(term.Expr); res != nil {
			return res
		}
	}
	if res := check(s.Where); res != nil {
		return res
	}
	return nil
}

// validateCheckExpr rejects constructs SQLite prohibits in CHECK constraints
// (resolve.c sqlite3ResolveSelfReference): subqueries. Non-deterministic
// functions are allowed in CHECK (they are evaluated per row at DML time).
func validateCheckExpr(expr sql.Expr) error {
	var err error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		if _, ok := n.(*sql.Subquery); ok {
			err = fmt.Errorf("subqueries prohibited in CHECK constraints")
		}
	})
	return err
}

// validateGeneratedExpr rejects constructs SQLite prohibits in generated
// columns (resolve.c notValidImpl with NC_GenCol): subqueries. The message
// matches SQLite's "%s prohibited in generated columns".
func validateGeneratedExpr(expr sql.Expr) error {
	var err error
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if err != nil {
			return
		}
		switch n.(type) {
		case *sql.Subquery, *sql.ExistsExpr:
			err = fmt.Errorf("subqueries prohibited in generated columns")
		}
	})
	return err
}

// validateIndexKeyExpr rejects constructs SQLite prohibits in index key
// expressions (build.c sqlite3CreateIndex / sqlite3ExprIsConstantOrFunction):
// non-deterministic functions, julianday()/randomblob()/zeroblob() with
// certain arguments, and subqueries.
// isPlainColumnKey reports whether an index key is a plain column name (not
// an expression). Numeric literal keys are 1-based column positions.
func isPlainColumnKey(name string) bool {
	if name == "" {
		return false
	}
	if n, err := strconv.Atoi(name); err == nil && n >= 1 {
		return true
	}
	return !containsIndexKeyOperator(name)
}

// containsIndexKeyOperator reports whether name contains a character that
// marks it as an expression rather than a plain column name (parentheses,
// arithmetic operators, whitespace, commas, comparisons, bitwise operators).
func containsIndexKeyOperator(name string) bool {
	return strings.ContainsAny(name, "()+-*/ ,=<>|&")
}

// indexKeyForCreate computes an index key value for a row during CREATE
// INDEX. Plain column names (and 1-based positions) resolve to the column;
// anything else is treated as an expression evaluated against the row. An
// evaluation error (e.g. integer overflow in ABS) is returned so CREATE
// INDEX can fail like SQLite.
func (e *DDLExecutor) indexKeyForCreate(row RowMap, colDefs []sql.ColumnDef, key string) (interface{}, error) {
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
	// Expression key: evaluate SELECT <expr> against the row. The evaluation
	// runs in a pure context so non-deterministic date functions raise
	// SQLite's "non-deterministic use of %s() in an index" error.
	stmts, perr := parse.ParseSQL("SELECT " + key)
	if perr != nil || len(stmts) == 0 {
		return nil, nil
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok && len(sel.Columns) > 0 {
		var v interface{}
		var kerr error
		function.WithPureContext("index", func() error {
			v, kerr = e.ctx.EvalExpr(sel.Columns[0].Expr, row)
			return kerr
		})
		return v, kerr
	}
	return nil, nil
}
