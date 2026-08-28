package execdml

import (
	"fmt"
	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"regexp"
	"strings"
)

// strictCheckValues enforces STRICT table type checking on non-generated
// column values.
func strictCheckValues(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	for i, v := range values {
		if i >= len(colDefs) {
			break
		}
		cd := colDefs[i]
		// Skip generated columns (computed separately)
		if cd.Generated != nil {
			continue
		}
		if err := enforceStrictType(tableEntry.Name, cd.Name, cd.Type, v); err != nil {
			return err
		}
	}
	return nil
}

// strictCheckGeneratedValues enforces STRICT table type checking on generated
// column values (computed from expressions).

// strictCheckGeneratedValues enforces STRICT table type checking on generated
// column values (computed from expressions).
func strictCheckGeneratedValues(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) error {
	for i, v := range values {
		if i >= len(colDefs) {
			break
		}
		cd := colDefs[i]
		if cd.Generated == nil {
			continue // already checked above
		}
		if err := enforceStrictType(tableEntry.Name, cd.Name, cd.Type, v); err != nil {
			return err
		}
	}
	return nil
}

// resolveInsertRowConstraints substitutes REPLACE defaults, computes generated
// columns, and validates constraints for a single row. Returns a non-nil
// Result on failure, and write=true when the row may be written (constraints
// passed or resolved).

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
func (e *DMLExecutor) hasTriggersForTable(tableName string) bool {
	// Check cache first
	if has, ok := e.ctx.CachedTriggerFlag(tableName); ok {
		return has
	}
	has := false
	for _, ctx := range e.ctx.Databases() {
		triggers, err := ctx.Schema.FindTriggersForTable(tableName)
		if err == nil && len(triggers) > 0 {
			has = true
			break
		}
	}
	e.ctx.SetCachedTriggerFlag(tableName, has)
	return has
}

// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.

// compositeUniqueGroups returns groups of column indices that have table-level
// PRIMARY KEY or UNIQUE constraints. Each group must be unique together.
// Single-column PRIMARY KEY (column-level) is excluded since it's handled
// separately by the column-level check.
func (e *DMLExecutor) compositeUniqueGroups(tableName, createSQL string, colDefs []sql.ColumnDef) [][]int {
	constraints := e.ctx.TableConstraints(tableName, createSQL)
	colIndex := buildColumnIndex(colDefs)
	var groups [][]int
	for _, tc := range constraints {
		switch tc.Type {
		case sql.ConstraintPrimaryKey, sql.ConstraintUnique:
			var indices []int
			for _, ic := range tc.Columns {
				if idx, ok := colIndex[ic.Name]; ok && idx >= 0 {
					indices = append(indices, idx)
				}
			}
			if len(indices) > 0 {
				groups = append(groups, indices)
			}
		}
	}
	return groups
}

// checkCompositeUnique scans for an existing row where ALL columns in the group
// match the new row's values (composite uniqueness). NULL values never conflict
// (NULL != NULL in SQL uniqueness semantics).

// allMatch returns true if ALL columns in the group match between the existing
// record and the new values. NULL values never match (NULL != NULL). Each
// column's declared collation is applied to its comparison.
func (e *DMLExecutor) allMatch(colDefs []sql.ColumnDef, recValues []interface{}, group []int, values []interface{}) bool {
	for _, idx := range group {
		if idx >= len(recValues) || idx >= len(values) {
			return false
		}
		if recValues[idx] == nil || values[idx] == nil {
			return false
		}
		coll := ""
		if idx < len(colDefs) {
			coll = colDefs[idx].Collate
		}
		if e.ctx.CompareValuesCollate(recValues[idx], values[idx], coll) != 0 {
			return false
		}
	}
	return true
}

// uniqueIndexDef describes a UNIQUE index on a table: the indexed columns,
// the optional partial-index WHERE clause (empty for a full index), and the
// index name (for expression-key error messages, which SQLite reports as
// "index 'name'" rather than listing the columns).

// uniqueIndexDef is aliased from execquery (see engine.go).

// uniqueIndexColsRe matches "CREATE UNIQUE INDEX name ON tbl(col1, col2 ...)".
// The column-list capture is used only for boolean "is this a UNIQUE index"
// checks (pragma index_list); the actual column/expression extraction uses
// indexColumnListText (balanced-paren aware) below.

// uniqueIndexColsRe matches "CREATE UNIQUE INDEX name ON tbl(col1, col2 ...)".
// The column-list capture is used only for boolean "is this a UNIQUE index"
// checks (pragma index_list); the actual column/expression extraction uses
// indexColumnListText (balanced-paren aware) below.
var uniqueIndexColsRe = regexp.MustCompile(`(?is)^\s*CREATE\s+UNIQUE\s+INDEX\b.*?\bON\b\s+[^\s(]+\(.*\)`)

// UniqueIndexColsRe exposes the UNIQUE-index detection regex to internal/exec.
var UniqueIndexColsRe = uniqueIndexColsRe

// indexWhereRe captures the partial-index predicate after the column list.

// indexWhereRe captures the partial-index predicate after the column list.
var indexWhereRe = regexp.MustCompile(`(?is)\)\s*WHERE\s+(.+)$`)

// IndexWhereRe exposes the partial-index predicate regex to internal/exec.
var IndexWhereRe = indexWhereRe

// indexColumnListText extracts the text between the key parentheses of a
// CREATE [UNIQUE] INDEX statement, honoring nested parentheses in expression
// keys (e.g. TYPEOF(a), a GLOB b, (a+b)*2) and stopping before a trailing
// WHERE clause. Returns "" when no balanced key list is found.

// indexColumnListText extracts the text between the key parentheses of a
// CREATE [UNIQUE] INDEX statement, honoring nested parentheses in expression
// keys (e.g. TYPEOF(a), a GLOB b, (a+b)*2) and stopping before a trailing
// WHERE clause. Returns "" when no balanced key list is found.
func indexColumnListText(sqlText string) string {
	upper := strings.ToUpper(sqlText)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return ""
	}
	parenStart := strings.Index(sqlText[onIdx+4:], "(")
	if parenStart < 0 {
		return ""
	}
	parenStart += onIdx + 4
	depth := 0
	for i := parenStart; i < len(sqlText); i++ {
		switch sqlText[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sqlText[parenStart+1 : i]
			}
		}
	}
	return ""
}

// splitIndexCols splits a CREATE INDEX column-list text on top-level commas,
// keeping commas inside parentheses (function calls like substr(b,2,4)) as
// part of the element.

// splitIndexCols splits a CREATE INDEX column-list text on top-level commas,
// keeping commas inside parentheses (function calls like substr(b,2,4)) as
// part of the element.
func splitIndexCols(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// uniqueIndexColumns returns the UNIQUE indexes defined on the given table
// (cached per table name).

// parseIndexKeyCollations extracts the explicit COLLATE per index key from a
// CREATE INDEX key column-list text ("" for keys without COLLATE).
func parseIndexKeyCollations(colText string) []string {
	parts := splitIndexCols(colText)
	colls := make([]string, len(parts))
	for i, part := range parts {
		upper := strings.ToUpper(part)
		if idx := strings.Index(upper, " COLLATE "); idx >= 0 {
			name := strings.TrimSpace(part[idx+len(" COLLATE "):])
			name = strings.Trim(name, "'\"")
			colls[i] = name
		}
	}
	return colls
}

// parseIndexKeyCols parses a CREATE INDEX key column-list into stripped key
// expressions (plain names or expression text), removing COLLATE/ASC/DESC
// suffixes where they are not part of an expression.
func parseIndexKeyCols(colText string) []string {
	var cols []string
	for _, part := range splitIndexCols(colText) {
		name := strings.TrimSpace(part)
		upper := strings.ToUpper(name)
		// Strip COLLATE / ASC / DESC suffixes. For a plain column key the
		// collation comes from the table definition, so it is dropped;
		// for an expression key the explicit COLLATE is part of the
		// expression and must be kept (indexKeyValue evaluates it).
		if strings.ContainsAny(name, "()") {
			// expression key: keep COLLATE, strip only ASC/DESC
			if idx := strings.Index(upper, " DESC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " ASC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
		} else {
			if idx := strings.Index(upper, " COLLATE"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " DESC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			} else if idx := strings.Index(upper, " ASC"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
		}
		if name != "" {
			cols = append(cols, name)
		}
	}
	return cols
}

// allTableIndexes returns every index defined on the given table (unique and
// non-unique alike), with their key expressions, partial predicates, and root
// pages. This drives index maintenance on INSERT.

// indexDef describes any (unique or non-unique) index for index maintenance.
type indexDef struct {
	Name     string
	Cols     []string
	Where    string // partial-index predicate ("" for full indexes)
	RootPage uint32
	Ctx      *DatabaseContext
}

// maintainIndexesOnInsert writes the new row's entries into every index on
// the table. Partial-index predicates and expression keys are evaluated in a
// pure context, so a non-deterministic date/time function (e.g. date('now'))
// in an index expression raises SQLite's "non-deterministic use of %s() in an
// index" error, matching OP_PureFunc semantics.

// parseWhereExpr parses a standalone expression string into a sql.Expr.
func parseWhereExpr(exprSQL string) sql.Expr {
	stmts, perr := parse.ParseSQL("SELECT " + exprSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	if sel, ok := stmts[0].(*sql.SelectStmt); ok && len(sel.Columns) > 0 {
		return sel.Columns[0].Expr
	}
	return nil
}

// updateIndexRootPage persists a root page change after an index b-tree split.

// updateIndexRootPage persists a root page change after an index b-tree split.
func (e *DMLExecutor) updateIndexRootPage(indexName string, ctx *DatabaseContext, newRoot uint32) {
	e.ctx.TrackRootPage(indexName, newRoot)
	_ = ctx.Schema.UpdateEntryRoot(indexName, newRoot)
}

// evalIndexWhere evaluates a partial-index predicate against a row.
// A nil/empty predicate always matches.

// evalIndexWhere evaluates a partial-index predicate against a row.
// A nil/empty predicate always matches.
func (e *DMLExecutor) evalIndexWhere(whereSQL string, row RowMap) (bool, error) {
	if strings.TrimSpace(whereSQL) == "" {
		return true, nil
	}
	stmts, perr := parse.ParseSQL("SELECT " + whereSQL)
	if perr != nil || len(stmts) == 0 {
		return true, nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return true, nil
	}
	v, err := e.ctx.EvalExpr(sel.Columns[0].Expr, row)
	if err != nil {
		return true, nil
	}
	if v == nil {
		return false, nil
	}
	return execexpr.ToBool(v), nil
}

// indexKeyValueErr is like indexKeyValue but propagates expression-evaluation
// errors (e.g. non-deterministic date functions in index expressions). The
// bool is false for NULL keys, which never conflict and are skipped.

// indexKeyValueErr is like indexKeyValue but propagates expression-evaluation
// errors (e.g. non-deterministic date functions in index expressions). The
// bool is false for NULL keys, which never conflict and are skipped.
func (e *DMLExecutor) indexKeyValueErr(cn string, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, row RowMap) (interface{}, bool, error) {
	if idx, ok := colIndex[cn]; ok && idx >= 0 && idx < len(values) {
		if values[idx] == nil {
			return nil, false, nil
		}
		return values[idx], true, nil
	}
	// Expression index column: evaluate SELECT <expr> against the row.
	stmts, perr := parse.ParseSQL("SELECT " + cn)
	if perr != nil || len(stmts) == 0 {
		return nil, false, nil
	}
	sel, ok := stmts[0].(*sql.SelectStmt)
	if !ok || len(sel.Columns) == 0 {
		return nil, false, nil
	}
	v, err := e.ctx.EvalExpr(sel.Columns[0].Expr, row)
	if err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, false, nil
	}
	// Keep the collatedValue wrapper (from an explicit COLLATE in the index
	// key, e.g. substr(b,2,4) COLLATE nocase) so the uniqueness comparison
	// uses the collation. Unwrap only the column-affinity wrapper; the
	// comparison helper extracts the raw value and collation itself.
	return v, true, nil
}

// indexKeyValue returns the value of one index column for a row. A plain
// column name resolves through colIndex; any other expression (e.g. "0 | c0")
// is parsed and evaluated against the row. The bool result is false when the
// value cannot be computed (NULL or evaluation error) — callers treat that as
// no-conflict (SQL UNIQUE allows multiple NULLs in an index key).

// indexKeyValue returns the value of one index column for a row. A plain
// column name resolves through colIndex; any other expression (e.g. "0 | c0")
// is parsed and evaluated against the row. The bool result is false when the
// value cannot be computed (NULL or evaluation error) — callers treat that as
// no-conflict (SQL UNIQUE allows multiple NULLs in an index key).
func (e *DMLExecutor) indexKeyValue(cn string, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}, row RowMap) (interface{}, bool) {
	v, ok, _ := e.indexKeyValueErr(cn, colDefs, colIndex, values, row)
	return v, ok
}

// checkUniqueIndex scans the table for a row whose values match the new row
// on all columns of a UNIQUE index. Returns a SQLite-style error on conflict.
// NULL values never conflict (SQL UNIQUE allows multiple NULLs).

// isIPKRowidAliasCol reports whether a column is an INTEGER PRIMARY KEY
// rowid-alias candidate: PRIMARY KEY, declared type exactly INTEGER
// (case-insensitive), and NOT PRIMARY KEY DESC. SQLite treats INTEGER
// PRIMARY KEY DESC as an ordinary (non-rowid) column (build.c
// sqlite3AddPrimaryKey checks pCol->sortOrder), so DESC columns get a
// separate autoindex and their own rowid.
func isIPKRowidAliasCol(cd sql.ColumnDef) bool {
	return cd.PrimaryKey && !cd.PKDesc && strings.EqualFold(strings.TrimSpace(cd.Type), "INTEGER")
}

// rowIDConflictError builds the UNIQUE error for a rowid conflict. SQLite
// names the INTEGER PRIMARY KEY column when one exists, else "rowid".

// rowIDConflictError builds the UNIQUE error for a rowid conflict. SQLite
// names the INTEGER PRIMARY KEY column when one exists, else "rowid".
func (e *DMLExecutor) rowIDConflictError(tableEntry *schema.Entry, colDefs []sql.ColumnDef) error {
	for _, cd := range colDefs {
		if isIPKRowidAliasCol(cd) {
			return fmt.Errorf("UNIQUE constraint failed: %s.%s", tableEntry.Name, cd.Name)
		}
	}
	return fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableEntry.Name)
}

// rowIDExists reports whether the table already has a row with the given rowid.

// rowIDExists reports whether the table already has a row with the given rowid.
func (e *DMLExecutor) rowIDExists(tableName string, rootPage uint32, rowID int64) bool {
	tree := e.dmlTableBTree(tableName, rootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false
	}
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			return false
		}
		if cell.RowID == rowID {
			return true
		}
		ok, err := cursor.Next()
		if err != nil || !ok {
			return false
		}
	}
}

// replaceDeleteConflicts deletes every row that conflicts with the new values
// on a UNIQUE/PRIMARY KEY column, a UNIQUE index, or the explicit rowid
// (replaceRowID), firing BEFORE and AFTER DELETE triggers for each deleted row
// (SQLite REPLACE semantics).

// columnValue returns the value for a named column from a values array.
func columnValue(values []interface{}, colDefs []sql.ColumnDef, name string) interface{} {
	for i, cd := range colDefs {
		if cd.Name == name && i < len(values) {
			return values[i]
		}
	}
	return nil
}

// contains returns true if the slice contains the value.

// contains returns true if the slice contains the value.
func contains(s []int, v int) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// execInsertOnConflict handles INSERT ... ON CONFLICT by attempting the
// insert and falling back to the conflict action when a conflict is detected.

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.
func (e *DMLExecutor) applyUpsertUpdate(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingRowID int64, existingValues []interface{}, values []interface{}, oc *sql.OnConflictClause, alias string) *Result {
	// Evaluate the DO UPDATE WHERE condition against the existing row and the
	// excluded pseudo-table. When false, the update is skipped entirely
	// (SQLite: 0 changes, no BEFORE/AFTER UPDATE triggers — only the
	// BEFORE INSERT trigger that ran before conflict detection).
	if oc.Where != nil {
		allowed, err := e.upsertWhereAllows(tableEntry, colDefs, colIndex, existingValues, values, oc, alias)
		if err != nil {
			return &Result{Error: err}
		}
		if !allowed {
			return &Result{Changes: 0, Row: nil}
		}
	}

	dmlName := tableEntry.Name
	if alias != "" {
		dmlName = alias
	}
	updated := e.buildUpdatedRow(dmlName, colDefs, colIndex, existingValues, values, oc)
	// Unwrap collation/affinity wrappers a subquery assignment may produce so
	// the stored row holds raw values (mirrors the insert path).
	unwrapCollationWrappers(updated)

	// The DO UPDATE may create a new UNIQUE conflict (e.g. SET c='one' when
	// another row already has c='one'). Check the updated row's unique
	// constraints, excluding the row being updated itself.
	if err := e.checkUniqueConstraintsExcluding(tableEntry, colDefs, updated, existingRowID, true); err != nil {
		return &Result{Error: err}
	}

	// Enforce FOREIGN KEY constraints on the updated row.
	if res := e.ctx.CheckForeignKeyViolations(tableEntry, colDefs, updated, 0); res.Error != nil {
		return res
	}

	return e.writeUpdatedRow(tableEntry, colDefs, updated, existingRowID, existingValues)
}

// upsertWhereAllows evaluates the DO UPDATE WHERE against the existing row and
// excluded pseudo-table.
func (e *DMLExecutor) upsertWhereAllows(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, values []interface{}, oc *sql.OnConflictClause, alias string) (bool, error) {
	row := make(RowMap)
	dmlName := tableEntry.Name
	if alias != "" {
		dmlName = alias
	}
	// The pseudo-table "excluded" is shadowed only when the statement's
	// target table is itself named "excluded" and NOT aliased.
	excludedShadowed := alias == "" && strings.EqualFold(tableEntry.Name, "excluded")
	for _, col := range colDefs {
		if idx, ok := colIndex[col.Name]; ok && idx < len(existingValues) {
			row[col.Name] = existingValues[idx]
		}
		if !excludedShadowed {
			if idx, ok := colIndex[col.Name]; ok && idx < len(values) {
				row["excluded."+col.Name] = values[idx]
			}
		}
	}
	// The WHERE may reference the table by name (t1.b): resolve against
	// the row's unqualified keys via the current DML table context.
	prevDML := e.currentDMLTable
	e.currentDMLTable = dmlName
	ok, err := e.ctx.EvalBool(oc.Where, row)
	e.currentDMLTable = prevDML
	return ok, err
}

// writeUpdatedRow deletes the old row, fires BEFORE UPDATE triggers, writes the
// updated row, and fires AFTER UPDATE triggers.
func (e *DMLExecutor) writeUpdatedRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, updated []interface{}, existingRowID int64, existingValues []interface{}) *Result {
	record, err := storage.EncodeRecord(updated)
	if err != nil {
		return &Result{Error: err}
	}

	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	deleted, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == existingRowID
	})
	if err != nil || deleted == 0 {
		return &Result{Error: fmt.Errorf("upsert: row not found for update")}
	}
	dmlPg := e.dmlPager(tableEntry.Name)
	if tree.RootPage() != e.ctx.RootPagePg(dmlPg, tableEntry.Name, tableEntry.RootPage) {
		e.ctx.UpdateRootPagePg(dmlPg, tableEntry.Name, tree.RootPage())
	}
	e.ctx.InvalidateRowIDCache(dmlPg, tableEntry.RootPage)

	// Fire BEFORE UPDATE triggers before writing the updated row.
	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := buildRowMapFromValues(updated, colDefs, existingRowID)
		oldRow := buildRowMapFromValues(existingValues, colDefs, existingRowID)
		if trigResult := e.fireBeforeUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}

	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   existingRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	// A split during InsertCell may have changed the tree root; persist it so
	// later statements (e.g. the next VALUES tuple) write to the new root.
	if tree.RootPage() != e.ctx.RootPagePg(dmlPg, tableEntry.Name, tableEntry.RootPage) {
		e.ctx.UpdateRootPagePg(dmlPg, tableEntry.Name, tree.RootPage())
	}
	// The re-inserted row already existed, so its rowid cannot extend the
	// largest-rowid cache; the delete invalidated it and the next
	// findNextRowID rescans (bumping here to the re-inserted rowid would
	// drop the true maximum if this rowid is lower, e.g. rowid 1 after a
	// rowid 2 was inserted by a previous VALUES tuple).

	if e.hasTriggersForTable(tableEntry.Name) {
		newRow := buildRowMapFromValues(updated, colDefs, existingRowID)
		oldRow := buildRowMapFromValues(existingValues, colDefs, existingRowID)
		if trigResult := e.fireAfterUpdateTriggers(tableEntry.Name, newRow, oldRow); trigResult.Error != nil {
			return trigResult
		}
	}
	// Carry the updated row back so RETURNING can project against it.
	return &Result{Changes: 1, Row: updated}
}

// buildUpdatedRow applies ON CONFLICT DO UPDATE SET assignments to the
// existing values and returns the updated row. values holds the attempted
// insert row; its columns are exposed to the SET expressions through the
// "excluded" pseudo-table (e.g. excluded.b).

// buildUpdatedRow applies ON CONFLICT DO UPDATE SET assignments to the
// existing values and returns the updated row. values holds the attempted
// insert row; its columns are exposed to the SET expressions through the
// "excluded" pseudo-table (e.g. excluded.b).
func (e *DMLExecutor) buildUpdatedRow(tableName string, colDefs []sql.ColumnDef, colIndex map[string]int, existingValues []interface{}, values []interface{}, oc *sql.OnConflictClause) []interface{} {
	// Pad to the full column count: storage trims trailing NULLs from records,
	// so existingValues may be shorter than colDefs.
	n := len(existingValues)
	if len(colDefs) > n {
		n = len(colDefs)
	}
	updated := make([]interface{}, n)
	copy(updated, existingValues)

	row := make(RowMap)
	// When the table itself is named "excluded", the pseudo-table name is
	// shadowed: an "excluded.c" reference resolves to the table's column
	// (the current row), not the attempted row (SQLite upsert semantics).
	excludedShadowed := strings.EqualFold(tableName, "excluded")
	for _, col := range colDefs {
		if idx, ok := colIndex[col.Name]; ok && idx < len(existingValues) {
			row[col.Name] = existingValues[idx]
		}
		// The excluded pseudo-table carries the row that would have been
		// inserted (values).
		if !excludedShadowed {
			if idx, ok := colIndex[col.Name]; ok && idx < len(values) {
				row["excluded."+col.Name] = values[idx]
			}
		}
	}

	for _, assign := range oc.Assignments {
		if idx, ok := colIndex[assign.Column]; ok {
			prevDML := e.currentDMLTable
			e.currentDMLTable = tableName
			val, err := e.ctx.EvalExpr(assign.Value, row)
			e.currentDMLTable = prevDML
			if err == nil && idx < len(updated) {
				updated[idx] = val
			}
		}
	}

	// Recompute generated columns after the assignments: a DO UPDATE that
	// changes a base column must refresh columns generated from it (SQLite
	// recomputes generated columns on UPSERT DO UPDATE).
	if hasGeneratedCols(colDefs) {
		updated = recomputeUpsertGenerated(e.ctx, colDefs, updated)
	}
	return updated
}

// recomputeUpsertGenerated forces recomputation of every generated column for
// an upsert DO UPDATE row (the assignments may have changed a base column).
func recomputeUpsertGenerated(ctx DMLContext, colDefs []sql.ColumnDef, updated []interface{}) []interface{} {
	rowMap := generatedRowMap(colDefs, updated)
	for i, cd := range colDefs {
		if cd.Generated == nil {
			continue
		}
		var v interface{}
		var gerr error
		function.WithPureContext("gencol", func() error {
			v, gerr = ctx.EvalExpr(cd.Generated, rowMap)
			return gerr
		})
		if gerr != nil {
			continue
		}
		if i >= len(updated) {
			for len(updated) <= i {
				updated = append(updated, nil)
			}
		}
		updated[i] = util.ApplyColumnAffinity(v, cd.Type)
		rowMap[cd.Name] = updated[i]
	}
	return updated
}

// hasGeneratedCols reports whether any column is generated.
func hasGeneratedCols(colDefs []sql.ColumnDef) bool {
	for _, cd := range colDefs {
		if cd.Generated != nil {
			return true
		}
	}
	return false
}

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.

// scanForConflict iterates through all rows and looks for a value match
// on any of the given UNIQUE column indices. It returns the conflicting row's
// rowid, its values, and the column index that conflicted.
func scanForConflict(cursor *btree.Cursor, uniqueCols []int, values []interface{}, colDefs []sql.ColumnDef) (int64, []interface{}, int, bool) {
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}

		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}

		if idx := hasConflictAt(rec.Values, uniqueCols, values, colDefs); idx >= 0 {
			return cell.RowID, rec.Values, idx, true
		}

		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return 0, nil, -1, false
}

// hasConflictAt returns true if any of the UNIQUE column values match.
// Per SQL standard, NULL != NULL for UNIQUE constraint purposes.
// hasConflictAt returns the first UNIQUE column index whose value matches the
// new row (or -1 if the row does not conflict).

// hasConflictAt returns true if any of the UNIQUE column values match.
// hasConflictAt returns the first UNIQUE column index whose value matches the
// new row (or -1 if the row does not conflict).
// colConflict describes a row conflicting with the new values on one UNIQUE
// column.
type colConflict struct {
	colIdx int
	rowID  int64
	values []interface{}
}

// scanAllUniqueConflicts scans the table once and collects the FIRST row that
// conflicts on each UNIQUE/PK column (a row may conflict on several columns,
// and different rows may conflict on different columns).
func (e *DMLExecutor) scanAllUniqueConflicts(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []colConflict {
	uniqueCols := collectUniqueColsWithPK(colDefs, colIndex, values)
	if len(uniqueCols) == 0 {
		return nil
	}
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil
	}
	var result []colConflict
	foundCols := make(map[int]bool, len(uniqueCols))
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		result = collectRowConflicts(result, foundCols, uniqueCols, rec.Values, values, cell)
		hasNext, err := cursor.Next()
		if err != nil || !hasNext {
			break
		}
	}
	return result
}

// collectRowConflicts records a conflict for each UNIQUE column whose value in
// recValues equals the new row's value.
func collectRowConflicts(result []colConflict, foundCols map[int]bool, uniqueCols []int, recValues, values []interface{}, cell *storage.Cell) []colConflict {
	for _, idx := range uniqueCols {
		if foundCols[idx] {
			continue
		}
		if idx >= len(recValues) || idx >= len(values) {
			continue
		}
		if recValues[idx] == nil || values[idx] == nil {
			continue
		}
		if util.CompareValues(recValues[idx], values[idx]) == 0 {
			foundCols[idx] = true
			result = append(result, colConflict{colIdx: idx, rowID: cell.RowID, values: recValues})
		}
	}
	return result
}

func hasConflictAt(recValues []interface{}, uniqueCols []int, values []interface{}, colDefs []sql.ColumnDef) int {
	for _, idx := range uniqueCols {
		if idx < len(recValues) && idx < len(values) {
			// NULL != NULL — two NULLs never violate a UNIQUE constraint
			if recValues[idx] == nil || values[idx] == nil {
				continue
			}
			// Apply the column's declared collation (e.g. `a COLLATE nocase
			// PRIMARY KEY` matches 'def' against 'DEF', hook.test 12.6).
			coll := ""
			if idx < len(colDefs) {
				coll = colDefs[idx].Collate
			}
			if util.CompareValuesCollate(recValues[idx], values[idx], coll) == 0 {
				return idx
			}
		}
	}
	return -1
}

// isIgnoreableConflict checks if a constraint error should be silently ignored
// due to a column-level ON CONFLICT IGNORE clause or a table-level constraint's
// ON CONFLICT IGNORE clause.

// notNullReplaceColumn returns the column (with ON CONFLICT REPLACE) whose NOT
// NULL constraint was violated, or nil. Used to substitute the column DEFAULT.
// When stmtReplace is true (statement-level INSERT OR REPLACE), any NOT NULL
// column with a DEFAULT qualifies (SQLite substitutes the default for the NULL
// on REPLACE).
func notNullReplaceColumn(err error, colDefs []sql.ColumnDef, stmtReplace bool) *sql.ColumnDef {
	if err == nil {
		return nil
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "NOT NULL constraint failed") {
		return nil
	}
	for i := range colDefs {
		cd := &colDefs[i]
		if (cd.OnConflict == "REPLACE" || stmtReplace) && strings.HasSuffix(errStr, "."+cd.Name) {
			return cd
		}
	}
	return nil
}

// isReplaceableConflict checks if a UNIQUE/PRIMARY KEY constraint error should

// isReplaceableConflict checks if a UNIQUE/PRIMARY KEY constraint error should
func isReplaceableConflict(err error, colDefs []sql.ColumnDef) bool {
	if err == nil {
		return false
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false
	}
	for _, cd := range colDefs {
		if cd.OnConflict == "REPLACE" {
			return true
		}
	}
	return false
}

// isUniqueConflictError reports whether the error is a UNIQUE/PRIMARY KEY
// constraint violation (used for INSERT OR IGNORE).

// isUniqueConflictError reports whether the error is a UNIQUE/PRIMARY KEY
// constraint violation (used for INSERT OR IGNORE).
func isUniqueConflictError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isIgnoreableConstraintError reports whether the error is a constraint
// violation that statement-level OR IGNORE silently skips: UNIQUE, NOT NULL,
// and CHECK failures all yield a row skip under INSERT OR IGNORE / UPDATE OR
// IGNORE (verified against sqlite3 3.51: INSERT OR IGNORE suppresses UNIQUE,
// NOT NULL, and CHECK violations; only ABORT/FAIL/REPLACE/ROLLBACK error).

// isIgnoreableConstraintError reports whether the error is a constraint
// violation that statement-level OR IGNORE silently skips: UNIQUE, NOT NULL,
// and CHECK failures all yield a row skip under INSERT OR IGNORE / UPDATE OR
// IGNORE (verified against sqlite3 3.51: INSERT OR IGNORE suppresses UNIQUE,
// NOT NULL, and CHECK violations; only ABORT/FAIL/REPLACE/ROLLBACK error).
func isIgnoreableConstraintError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "NOT NULL constraint failed") ||
		strings.Contains(s, "CHECK constraint failed")
}

// gatherUniqueColIndices returns the column indices that have UNIQUE constraints
// and are present in both the column definitions and the provided values.

// gatherUniqueColIndices returns the column indices that have UNIQUE constraints
// and are present in both the column definitions and the provided values.
func gatherUniqueColIndices(colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []int {
	var uniqueCols []int
	for _, cd := range colDefs {
		if cd.Unique {
			if idx, ok := colIndex[cd.Name]; ok && idx < len(values) {
				uniqueCols = append(uniqueCols, idx)
			}
		}
	}
	return uniqueCols
}
