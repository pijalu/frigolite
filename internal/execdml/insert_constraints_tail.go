package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) singleColumnPKMatch(terms []conflictKeyTerm, colDefs []sql.ColumnDef, oc *sql.OnConflictClause) bool {
	if len(terms) != 1 || !terms[0].isCol {
		return false
	}
	for _, cd := range colDefs {
		if !(cd.PrimaryKey || cd.Unique) || !strings.EqualFold(cd.Name, terms[0].name) {
			continue
		}
		if terms[0].collate != "" && !sameCollation(terms[0].collate, cd.Collate) {
			continue
		}
		if !e.conflictWhereMatches(oc, "") {
			continue
		}
		return true
	}
	return false
}

// conflictKeyTerm is one key of a UNIQUE index / constraint, or one term of an
// ON CONFLICT target. isCol distinguishes a plain column key from an expression
// key. collate is the effective collation ("" = BINARY default) for column
// keys; for expression keys it is the explicit COLLATE suffix ("" if none).
type conflictKeyTerm struct {
	isCol   bool
	name    string // column name (isCol) or expression text
	collate string // explicit collation ("" = none/BINARY)
}

// targetKeyTerms converts an ON CONFLICT clause's target into key terms.
// Returns nil when the target has an unparseable mix (a rowid pseudo-column is
// handled by the caller; bare ON CONFLICT has no terms).
func targetKeyTerms(oc *sql.OnConflictClause) []conflictKeyTerm {
	names := oc.ConflictColumn
	exprs := oc.TargetExpr
	if len(names) == 0 && len(exprs) == 0 {
		return nil
	}
	// The two lists are parallel: names[i] is "" when term i is an
	// expression (TargetExpr[i] holds its text).
	if len(names) != len(exprs) {
		return nil
	}
	terms := make([]conflictKeyTerm, len(names))
	for i := range names {
		if exprs[i] == "" && names[i] != "" {
			terms[i] = conflictKeyTerm{isCol: true, name: names[i]}
			continue
		}
		// A COLLATE on a plain column is still a column term with a
		// collation; any other expression is an expression term.
		if base, coll := splitCollateExpr(exprs[i]); coll != "" && isColumnName(base) {
			terms[i] = conflictKeyTerm{isCol: true, name: base, collate: coll}
			continue
		}
		terms[i] = conflictKeyTerm{name: exprs[i]}
	}
	return terms
}

// indexKeyTerms converts a unique index definition's keys into key terms.
// Plain column keys carry the column's declared collation; expression keys
// keep their explicit COLLATE suffix.
func indexKeyTerms(def uniqueIndexDef, colDefs []sql.ColumnDef) []conflictKeyTerm {
	terms := make([]conflictKeyTerm, len(def.Cols))
	for i, c := range def.Cols {
		if base, coll := splitCollateExpr(c); isColumnName(base) {
			// The explicit per-key COLLATE (from the CREATE INDEX text) wins
			// over the column's declared collation.
			if i < len(def.KeyColl) && def.KeyColl[i] != "" {
				coll = def.KeyColl[i]
			}
			terms[i] = conflictKeyTerm{isCol: true, name: base, collate: coll}
			// A plain column key without explicit COLLATE uses the
			// column's declared collation (BINARY when none).
			if coll == "" {
				if cd := colDefAt(colDefs, base); cd != nil {
					terms[i].collate = cd.Collate
				}
			}
			continue
		}
		terms[i] = conflictKeyTerm{name: c}
	}
	return terms
}

// tableConstraintKeys converts a table-level PK/UNIQUE constraint into key
// terms (each IndexedColumn carries its own COLLATE).
func tableConstraintKeys(tc sql.TableConstraint, colDefs []sql.ColumnDef) []conflictKeyTerm {
	terms := make([]conflictKeyTerm, len(tc.Columns))
	for i, ic := range tc.Columns {
		coll := ic.Collate
		if coll == "" {
			if cd := colDefAt(colDefs, ic.Name); cd != nil {
				coll = cd.Collate
			}
		}
		terms[i] = conflictKeyTerm{isCol: true, name: ic.Name, collate: coll}
	}
	return terms
}

// termsMatchIndexKeys reports whether the target terms can be matched to the
// index keys one-to-one (order-insensitive).
func (e *DMLExecutor) termsMatchIndexKeys(terms, keys []conflictKeyTerm) bool {
	if len(terms) != len(keys) {
		return false
	}
	used := make([]bool, len(keys))
	for _, t := range terms {
		matched := false
		for j, k := range keys {
			if used[j] {
				continue
			}
			if termMatchesKey(t, k) {
				used[j] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// termMatchesKey reports whether one target term matches one index key.
func termMatchesKey(t, k conflictKeyTerm) bool {
	if t.isCol != k.isCol {
		return false
	}
	if !t.isCol {
		// Expression terms: the base expression text must match (normalized,
		// ignoring whitespace and redundant parens). An explicit COLLATE on
		// the TARGET must equal the index key's explicit COLLATE; a target
		// without COLLATE matches any index collation (SQLite applies the
		// index's collation at runtime).
		tBase, tColl := splitCollateExpr(t.name)
		kBase, kColl := splitCollateExpr(k.name)
		if normalizeKey(trimOuterParens(tBase)) != normalizeKey(trimOuterParens(kBase)) {
			return false
		}
		if tColl != "" {
			return sameCollation(tColl, kColl)
		}
		return true
	}
	if !strings.EqualFold(t.name, k.name) {
		return false
	}
	// Plain target column matches a key with any collation; a COLLATE-
	// qualified target must match the key's effective collation.
	if t.collate == "" {
		return true
	}
	return sameCollation(t.collate, k.collate)
}

// trimOuterParens removes balanced outer parentheses from an expression text
// (SQLite treats ((expr)) and (expr) as the same index key).
func trimOuterParens(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		balanced := true
		for i := 0; i < len(s); i++ {
			if s[i] == '(' {
				depth++
			} else if s[i] == ')' {
				depth--
				if depth == 0 && i < len(s)-1 {
					balanced = false
					break
				}
			}
		}
		if !balanced || depth != 0 {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// splitCollateExpr splits "<base> COLLATE <name>" into (base, name). Returns
// the whole string with collate "" when there is no COLLATE suffix.
func splitCollateExpr(s string) (string, string) {
	upper := strings.ToUpper(s)
	idx := strings.LastIndex(upper, " COLLATE ")
	if idx < 0 {
		return s, ""
	}
	base := strings.TrimSpace(s[:idx])
	coll := strings.Trim(strings.TrimSpace(s[idx+len(" COLLATE "):]), "'\"")
	return base, coll
}

// isColumnName reports whether s looks like a bare column identifier (not a
// parenthesized or compound expression).
func isColumnName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "()+-*/%<>=!&|") {
		return false
	}
	for _, r := range s {
		if r == ' ' || r == '\'' || r == '"' {
			return false
		}
	}
	return true
}

// sameCollation compares two collation names, treating "" and BINARY as equal.
func sameCollation(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" {
		a = "binary"
	}
	if b == "" {
		b = "binary"
	}
	return a == b
}

// conflictWhereMatches reports whether the clause's conflict-target WHERE
// matches the index's partial predicate. A clause with no WHERE matches only
// a full (non-partial) index; a clause with a WHERE matches a full index (the
// WHERE is allowed on a full index) or a partial index with the same
// predicate text (SQLite's partial-index upsert rule).
func (e *DMLExecutor) conflictWhereMatches(oc *sql.OnConflictClause, indexWhere string) bool {
	if oc.TargetWhere == nil {
		return indexWhere == ""
	}
	if indexWhere == "" {
		return true
	}
	return normalizeWhereKey(sql.ExprString(oc.TargetWhere)) == normalizeWhereKey(indexWhere)
}

// normalizeWhereKey normalizes a partial-index / conflict-target WHERE text:
// strips whitespace, lowercases, and removes quotes around COLLATE names
// (ExprString renders them quoted, index SQL does not).
func normalizeWhereKey(s string) string {
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, `"`, "")
	return normalizeKey(s)
}

// normalizeKey strips all whitespace and lowercases for textual comparison of
// expression text (SQLite compares index expression text without regard to
// spacing, e.g. "a+b" matches "a + b").
func normalizeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// findOnConflictRow locates the existing row conflicting with the new values,
// via UNIQUE columns or UNIQUE indexes.

// conflictHit describes one existing row that conflicts with the new values on
// a specific UNIQUE constraint or index. keys are the normalized conflict-key
// terms (column names for plain constraints, expression text for expression
// indexes); they identify which clause's target matches the conflict.
type conflictHit struct {
	rowID  int64
	values []interface{}
	keys   []conflictKeyTerm
	index  string // conflicting index name ("" for table/column constraints)
}

// findOnConflictRow locates every existing row conflicting with the new values,
// via UNIQUE/PK columns, composite constraints, or UNIQUE indexes. Each hit
// carries the conflict source's key terms so execInsertOnConflict can match the
// ON CONFLICT clauses in order.
func (e *DMLExecutor) findOnConflictRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, values []interface{}) []conflictHit {
	var hits []conflictHit

	// Column-level single-column UNIQUE / PK constraints: collect EVERY
	// conflicting column (a row may conflict on several, and the matching
	// ON CONFLICT clause must find its own target's column).
	for _, colConflict := range e.scanAllUniqueConflicts(tableEntry, colDefs, colIndex, values) {
		cd := colDefs[colConflict.colIdx]
		hits = append(hits, conflictHit{
			rowID:  colConflict.rowID,
			values: colConflict.values,
			keys:   []conflictKeyTerm{{isCol: true, name: cd.Name}},
		})
	}

	// Composite table-level PK / UNIQUE constraints.
	for _, group := range e.compositeUniqueGroups(tableEntry.Name, tableEntry.SQL, colDefs) {
		if groupHasNull(group, values) {
			continue
		}
		tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
		cursor, err := tree.OpenCursor()
		if err != nil {
			continue
		}
		if rowID, rv, _, ok := e.scanGroupForMatch(cursor, colDefs, group, values); ok {
			var keys []conflictKeyTerm
			for _, idx := range group {
				keys = append(keys, conflictKeyTerm{isCol: true, name: colDefs[idx].Name})
			}
			hits = append(hits, conflictHit{rowID: rowID, values: rv, keys: keys})
		}
	}

	// UNIQUE indexes (including autoindexes for table constraints). The
	// conflicting index is identified by its def so the error can name it.
	for _, def := range e.uniqueIndexColumns(tableEntry.Name) {
		if rid, rv, ok := e.findRowByIndexCols(tableEntry, colDefs, values, def); ok {
			hits = append(hits, conflictHit{
				rowID:  rid,
				values: rv,
				keys:   indexKeyTerms(def, colDefs),
				index:  def.Name,
			})
		}
	}
	return hits
}

// clauseMatchesHit reports whether one ON CONFLICT clause's target matches one
// conflict hit's source keys. A bare ON CONFLICT (no target) matches any hit.
func (e *DMLExecutor) clauseMatchesHit(oc *sql.OnConflictClause, hit conflictHit) bool {
	terms := targetKeyTerms(oc)
	if terms == nil {
		return true
	}
	if len(terms) != len(hit.keys) {
		return false
	}
	return e.termsMatchIndexKeys(terms, hit.keys)
}

// uniqueConstraintError builds the SQLite error for an unhandled conflict:
// "UNIQUE constraint failed: index 'name'" for an index conflict, or
// "UNIQUE constraint failed: col1, col2" for a constraint conflict.
func (e *DMLExecutor) uniqueConstraintError(hit conflictHit) error {
	if hit.index != "" {
		return fmt.Errorf("UNIQUE constraint failed: index '%s'", hit.index)
	}
	var names []string
	for _, k := range hit.keys {
		names = append(names, k.name)
	}
	return fmt.Errorf("UNIQUE constraint failed: %s", strings.Join(names, ", "))
}

// applyUpsertUpdate applies DO UPDATE SET assignments to the existing row
// and writes the updated row back to the table.

// findRowByUniqueCols searches for a row that conflicts with the given values
// on any UNIQUE column. Returns the RowID, existing values, and whether a
// conflict was found.

// insertSelectWrittenRow encodes, inserts, indexes, and fires AFTER triggers
// for one written INSERT ... SELECT row, then evaluates RETURNING. Returns a
// non-nil Result on failure, and the RETURNING row (or nil) on success.

// unsafeSchemaFunc returns the name of the first function call in expr that
// is unsafe under the current trusted_schema setting, or "" when safe.
func (e *DMLExecutor) unsafeSchemaFunc(expr sql.Expr) string {
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

// evalDefaultExpr evaluates a DEFAULT expression at INSERT time. Aggregate
// functions are rejected with "unknown function: X()" matching SQLite's
// INSERT-time prepare behavior (aggregates are not available in the scalar
// DEFAULT evaluation context).
func (e *DMLExecutor) evalDefaultExpr(expr sql.Expr, colName string) (interface{}, error) {
	if aggName := execquery.FindAggregateInExpr(expr); aggName != "" {
		return nil, fmt.Errorf("unknown function: %s()", strings.ToLower(aggName))
	}
	// trusted_schema=OFF blocks non-innocuous user functions in DEFAULT
	// expressions at USE time (trustschema1-1.310); temp-schema tables are
	// always trusted (1.320).
	if e.currentDMLCtx == nil || !e.currentDMLCtx.IsTemp {
		if name := e.unsafeSchemaFunc(expr); name != "" {
			return nil, fmt.Errorf("unsafe use of %s()", name)
		}
	}
	return e.ctx.EvalExpr(expr, nil)
}

// defaultValuesWithRowID fills every column with its DEFAULT (NULL if none)
// and assigns an auto-generated rowid to an empty INTEGER PRIMARY KEY column.
// Returns an error if a DEFAULT expression contains an aggregate function
// (SQLite rejects these at INSERT-time prepare with "unknown function: X()").
func (e *DMLExecutor) defaultValuesWithRowID(colDefs []sql.ColumnDef, tableName string, rootPage uint32) ([]interface{}, int64, error) {
	values := make([]interface{}, len(colDefs))
	for i, cd := range colDefs {
		if cd.Default != nil {
			dv, err := e.evalDefaultExpr(cd.Default, cd.Name)
			if err != nil {
				return nil, 0, err
			}
			values[i] = dv
		}
	}
	nextRowID := e.findNextRowID(tableName, rootPage)
	for i, cd := range colDefs {
		if cd.PrimaryKey && values[i] == nil {
			values[i] = nextRowID
			break
		}
	}
	return values, nextRowID, nil
}

// resolveInsertDefaultConstraints validates a DEFAULT VALUES row, substituting
// a NOT NULL column's DEFAULT when possible (SQLite REPLACE semantics).

// resolveInsertDefaultConstraints validates a DEFAULT VALUES row, substituting
// a NOT NULL column's DEFAULT when possible (SQLite REPLACE semantics).

// resolveInsertDefaultConstraints validates a DEFAULT VALUES row, substituting
// a NOT NULL column's DEFAULT when possible (SQLite REPLACE semantics).
// resolveInsertDefaultConstraints validates a DEFAULT VALUES row, substituting
// a NOT NULL column's DEFAULT when possible (SQLite REPLACE semantics).
func (e *DMLExecutor) resolveInsertDefaultConstraints(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) (bool, error) {
	// Validate NOT NULL / CHECK / UNIQUE / PRIMARY KEY constraints before
	// inserting (SQLite checks DEFAULT VALUES rows like any other insert;
	// e.g. `CREATE TABLE t(x NOT NULL DEFAULT NULL); REPLACE INTO t
	// DEFAULT VALUES` fails with NOT NULL constraint failed).
	if err := e.checkConstraints(tableEntry, colDefs, values, nextRowID); err != nil {
		return e.trySubstituteNotNullDefault(err, tableEntry, colDefs, values, nextRowID)
	}
	return true, nil
}

// trySubstituteNotNullDefault replaces a NOT NULL violation by substituting
// the column's DEFAULT and re-checking. ok reports whether the row passed.

// trySubstituteNotNullDefault replaces a NOT NULL violation by substituting
// the column's DEFAULT and re-checking. ok reports whether the row passed.

// trySubstituteNotNullDefault replaces a NOT NULL violation by substituting
// the column's DEFAULT and re-checking. ok reports whether the row passed.
// trySubstituteNotNullDefault replaces a NOT NULL violation by substituting
// the column's DEFAULT and re-checking. ok reports whether the row passed.
func (e *DMLExecutor) trySubstituteNotNullDefault(err error, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) (bool, error) {
	// REPLACE on a NOT NULL column substitutes the DEFAULT; here the
	// DEFAULT is NULL, so re-checking still fails and the error stands.
	cd := notNullReplaceColumn(err, colDefs, false)
	if cd == nil || cd.Default == nil {
		return false, err
	}
	dv, derr := e.ctx.EvalExpr(cd.Default, nil)
	if derr != nil {
		return false, err
	}
	idx := cdIndex(colDefs, cd.Name)
	if idx < 0 || idx >= len(values) {
		return false, err
	}
	values[idx] = dv
	if rerr := e.checkConstraints(tableEntry, colDefs, values, nextRowID); rerr != nil {
		return false, rerr
	}
	return true, nil
}

// fireDefaultBeforeTriggers fires BEFORE INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure or RAISE(IGNORE) skip.

// fireDefaultBeforeTriggers fires BEFORE INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure or RAISE(IGNORE) skip.

// fireDefaultBeforeTriggers fires BEFORE INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure or RAISE(IGNORE) skip.
// fireDefaultBeforeTriggers fires BEFORE INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure or RAISE(IGNORE) skip.
func (e *DMLExecutor) fireDefaultBeforeTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}) *Result {
	if !e.hasTriggersForTable(tableEntry.Name) {
		return nil
	}
	newRow := buildTriggerNewRow(colDefs, values)
	// SQLite exposes new.rowid as -1 inside a BEFORE INSERT trigger.
	if !execquery.RowHasRowIDColumn(colDefs) {
		newRow["rowid"] = int64(-1)
		newRow["_rowid_"] = int64(-1)
		newRow["oid"] = int64(-1)
	}
	if trigResult := e.fireBeforeInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
		// RAISE(IGNORE) in a BEFORE trigger aborts the insert (the row is
		// skipped, no error) — SQLite semantics.
		if trigResult.Error == errRaiseIgnore {
			return &Result{Changes: 0}
		}
		return trigResult
	}
	return nil
}

// fireDefaultAfterTriggers fires AFTER INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure.

// fireDefaultAfterTriggers fires AFTER INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure.

// fireDefaultAfterTriggers fires AFTER INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure.
// fireDefaultAfterTriggers fires AFTER INSERT triggers for a DEFAULT VALUES
// row, returning a Result on failure.
func (e *DMLExecutor) fireDefaultAfterTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) *Result {
	if !e.hasTriggersForTable(tableEntry.Name) {
		return nil
	}
	newRow := buildTriggerNewRow(colDefs, values)
	// AFTER INSERT triggers see the assigned rowid.
	if !execquery.RowHasRowIDColumn(colDefs) {
		newRow["rowid"] = &util.ColumnValue{Value: nextRowID, Affinity: 'I'}
		newRow["_rowid_"] = &util.ColumnValue{Value: nextRowID, Affinity: 'I'}
		newRow["oid"] = &util.ColumnValue{Value: nextRowID, Affinity: 'I'}
	}
	if trigResult := e.fireAfterInsertTriggers(tableEntry.Name, newRow); trigResult.Error != nil {
		return trigResult
	}
	return nil
}

// buildTriggerNewRow builds a column-name-to-value map for trigger NEW-row
// evaluation.

// buildTriggerNewRow builds a column-name-to-value map for trigger NEW-row
// evaluation.

// buildTriggerNewRow builds a column-name-to-value map for trigger NEW-row
// evaluation.
// buildTriggerNewRow builds a column-name-to-value map for trigger NEW-row
// evaluation.
func buildTriggerNewRow(colDefs []sql.ColumnDef, values []interface{}) RowMap {
	newRow := make(RowMap)
	for i, v := range values {
		if i < len(colDefs) {
			newRow[colDefs[i].Name] = v
		}
	}
	return newRow
}

// insertDefaultRow encodes and writes one DEFAULT VALUES row, bumping the
// rowid cache.

// insertDefaultRow encodes and writes one DEFAULT VALUES row, bumping the
// rowid cache.

// insertDefaultRow encodes and writes one DEFAULT VALUES row, bumping the
// rowid cache.
// insertDefaultRow encodes and writes one DEFAULT VALUES row, bumping the
// rowid cache.
func (e *DMLExecutor) insertDefaultRow(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) *Result {
	record, err := storage.EncodeRecord(values)
	if err != nil {
		return &Result{Error: err}
	}
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	tree := e.dmlTableBTree(tableEntry.Name, tableEntry.RootPage)
	if err := tree.InsertCell(cell); err != nil {
		return &Result{Error: err}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableEntry.Name), tableEntry.RootPage, nextRowID)
	e.ctx.SetLastRowID(nextRowID)
	return nil
}

// defaultReturningResult evaluates RETURNING against the written DEFAULT
// VALUES row when present.

// defaultReturningResult evaluates RETURNING against the written DEFAULT
// VALUES row when present.

// defaultReturningResult evaluates RETURNING against the written DEFAULT
// VALUES row when present.
// defaultReturningResult evaluates RETURNING against the written DEFAULT
// VALUES row when present.
func (e *DMLExecutor) defaultReturningResult(s *sql.InsertStmt, colDefs []sql.ColumnDef, tableEntry *schema.Entry, values []interface{}, nextRowID int64) *Result {
	if !s.HasReturning {
		return nil
	}
	row := buildRowMapFromValues(values, colDefs, nextRowID)
	vals, err := e.evalReturningStrict(s.Returning, row, colDefs, tableEntry.Name)
	if err != nil {
		return &Result{Error: err}
	}
	columns := e.ctx.BuildColumnNames([]sql.SelectColumn{s.Returning}, colDefs, nil)
	return &Result{Columns: columns, Rows: [][]interface{}{vals}}
}

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.

// fireTriggers fires triggers matching the given event and timing for the table.

// fireAfterInsertTriggers fires AFTER INSERT triggers for the given table.
// fireTriggers fires triggers matching the given event and timing for the table.
