// Package exec implements query execution.
package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) pkRowID(tableName string, colDefs []sql.ColumnDef, values []interface{}, rootPage uint32, withoutRowid bool) (int64, error) {
	if r, err := e.explicitPKRowID(tableName, colDefs, values, rootPage, withoutRowid); r != nil || err != nil {
		if err != nil {
			return 0, err
		}
		return *r, nil
	}
	next := e.findNextRowID(tableName, rootPage)
	if e.ctx.TableHasAutoIncrement(tableName) && (next == -1<<63 || next == 0) {
		// AUTOINCREMENT sequence exhausted: SQLite reports "database or
		// disk is full" rather than wrapping the rowid.
		return 0, fmt.Errorf("database or disk is full")
	}
	return next, nil
}

// explicitPKRowID derives the rowid from an explicitly supplied PRIMARY KEY
// value. Returns (nil, nil) when no PK column carries an explicit value and
// the caller must auto-assign one.
func (e *DMLExecutor) explicitPKRowID(tableName string, colDefs []sql.ColumnDef, values []interface{}, rootPage uint32, withoutRowid bool) (*int64, error) {
	for i, cd := range colDefs {
		if !cd.PrimaryKey || i >= len(values) || values[i] == nil {
			continue
		}
		r, found, err := pkRowIDFromColumn(cd, values[i], withoutRowid)
		if !found && err == nil {
			break
		}
		if err != nil {
			return nil, err
		}
		// A WITHOUT ROWID row's synthetic rowid must not collide with an
		// existing row's PK-derived rowid (a text-PK row may hold the
		// same int64 as an int-PK row's key). SQLite's WITHOUT ROWID
		// btree is keyed by the PK record, so two rows never share a
		// key; fall back to a synthetic rowid when the PK-derived one
		// is already taken.
		if withoutRowid && e.rowIDExists(tableName, rootPage, r) {
			fallback := e.findNextRowID(tableName, rootPage)
			return &fallback, nil
		}
		return &r, nil
	}
	return nil, nil
}

// pkRowIDFromColumn derives the rowid from one PRIMARY KEY column value, or
// reports that no rowid was derivable from it.
func pkRowIDFromColumn(cd sql.ColumnDef, v interface{}, withoutRowid bool) (int64, bool, error) {
	if !withoutRowid && isIPKRowidAliasCol(cd) {
		vv := util.ApplyColumnAffinity(v, "NUMERIC")
		if iv, ok := vv.(int64); ok {
			return iv, true, nil
		}
		return 0, true, fmt.Errorf("datatype mismatch")
	}
	if iv, ok := v.(int64); ok && withoutRowid {
		return iv, true, nil
	}
	return 0, false, nil
}

// validateLoadedTriggers checks every trigger loaded from sqlite_master for
// schema references that no longer resolve. SQLite validates triggers at
// schema load and reports "malformed database schema". Validated triggers
// are cached by name to avoid re-parsing on every statement.
func (e *DMLExecutor) validateLoadedTriggers() error {
	e.ctx.InitValidatedTriggers()
	for _, ctx := range e.ctx.Databases() {
		if ctx == nil || ctx.Schema == nil {
			continue
		}
		triggers, err := ctx.Schema.GetEntries(schema.TypeTrigger)
		if err != nil {
			continue
		}
		for _, t := range triggers {
			if _, verr := e.validateLoadedTrigger(t, ctx); verr != nil {
				return verr
			}
		}
	}
	return nil
}

// validateLoadedTrigger validates one trigger's schema references, skipping
// TEMP triggers and already-validated ones.
func (e *DMLExecutor) validateLoadedTrigger(t *schema.Entry, ctx *DatabaseContext) (bool, error) {
	if t == nil {
		return false, nil
	}
	// TEMP triggers may reference any schema; only non-temp triggers
	// are restricted (and thus can become malformed after a reopen).
	if ctx == e.ctx.GetDB("temp") {
		return false, nil
	}
	key := strings.ToUpper(ctx.Name + "." + t.Name)
	if e.ctx.IsTriggerValidated(key) {
		return false, nil
	}
	if err := e.validateLoadedTriggerSchemaCtx(t, ctx); err != nil {
		return false, err
	}
	e.ctx.MarkTriggerValidated(key)
	return true, nil
}

// validateLoadedTriggerSchemaCtx parses a trigger body loaded from sqlite_master
// and checks that every referenced table exists in the trigger's database
// context. A trigger whose references no longer resolve (after a reopen with
// different attachments) is malformed: SQLite reports "malformed database
// schema (NAME) - trigger NAME cannot reference objects in database X".
// Unqualified references resolve in the trigger's owning database context (a
// trigger inside an ATTACHed database references tables there).

// checkConstraintText extracts the original CHECK constraint expression text
// from a CREATE TABLE SQL for the given column. Falls back to the re-rendered
// expression when the raw text cannot be located.
func (e *DMLExecutor) checkConstraintText(createSQL, colName string, check sql.Expr) string {
	upper := strings.ToUpper(createSQL)
	start := strings.Index(upper, "(")
	end := strings.LastIndex(upper, ")")
	if start < 0 || end <= start {
		return sql.ExprString(check)
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		if text := checkConstraintTextFromPart(part, colName); text != "" {
			return text
		}
	}
	return sql.ExprString(check)
}

// checkConstraintTextFromPart extracts the constraint name or verbatim CHECK
// expression from one column-definition fragment.
func checkConstraintTextFromPart(part, colName string) string {
	if !strings.HasPrefix(strings.TrimSpace(part), colName) {
		return ""
	}
	pUpper := strings.ToUpper(part)
	ci := strings.Index(pUpper, "CHECK")
	if ci < 0 {
		return ""
	}
	// A named constraint (CONSTRAINT 'b-check' CHECK(...)) reports the
	// NAME in the error text, not the expression (SQLite semantics:
	// "CHECK constraint failed: b-check").
	if cn := constraintNameBefore(part, ci); cn != "" {
		return cn
	}
	return checkParenExpr(part, ci)
}

// checkParenExpr extracts the top-level parenthesized expression starting at
// the CHECK keyword position ci.
func checkParenExpr(part string, ci int) string {
	lp := strings.Index(part[ci:], "(")
	if lp < 0 {
		return ""
	}
	lp += ci
	depth := 0
	for i := lp; i < len(part); i++ {
		switch part[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(part[lp+1 : i])
			}
		}
	}
	return ""
}

// tableCheckConstraintText extracts the VERBATIM expression text for the
// idx-th table-level CHECK constraint from the CREATE TABLE SQL. SQLite
// reports the expression exactly as written ("a<>+a", "NOT(a=+a)", "a NOT
// BETWEEN 0 AND +a"), never the re-rendered AST. Named table-level checks
// already carry their name; this is the fallback for unnamed ones. The
// constraint is matched by scanning top-level CHECK(...) fragments in the SQL
// and taking the idx-th one that corresponds to a table-level (not column-
// level) CHECK, in the same order the parser produced tcs. Returns the
// re-rendered text when no verbatim match is found.

// tableCheckConstraintText extracts the VERBATIM expression text for the
// idx-th table-level CHECK constraint from the CREATE TABLE SQL. SQLite
// reports the expression exactly as written ("a<>+a", "NOT(a=+a)", "a NOT
// BETWEEN 0 AND +a"), never the re-rendered AST. Named table-level checks
// already carry their name; this is the fallback for unnamed ones. The
// constraint is matched by scanning top-level CHECK(...) fragments in the SQL
// and taking the idx-th one that corresponds to a table-level (not column-
// level) CHECK, in the same order the parser produced tcs. Returns the
// re-rendered text when no verbatim match is found.
func (e *DMLExecutor) tableCheckConstraintText(createSQL string, idx int, tcs []sql.TableConstraint) string {
	// Count how many CHECK constraints in tcs are table-level (type CHECK);
	// idx is the position among ALL table constraints, so the CHECK index is
	// the number of CHECK-type entries at or before idx.
	checkIdx := checkIndexAmong(tcs, idx)
	upper := strings.ToUpper(createSQL)
	start := strings.Index(upper, "(")
	end := strings.LastIndex(upper, ")")
	if start < 0 || end <= start {
		return sql.ExprString(tcs[idx].Expr)
	}
	body := createSQL[start+1 : end]
	tableCheckCount := 0
	for _, part := range splitColumnDefs(body) {
		trimmed := strings.TrimSpace(part)
		pUpper := strings.ToUpper(trimmed)
		// Column-level defs start with a column name; table-level CHECKs
		// start with CHECK or CONSTRAINT. Match the checkIdx-th table-level
		// CHECK in SQL order.
		if !strings.HasPrefix(pUpper, "CHECK") && !strings.HasPrefix(pUpper, "CONSTRAINT") {
			continue
		}
		if !hasTableLevelCheck(part) {
			continue
		}
		if tableCheckCount != checkIdx {
			tableCheckCount++
			continue
		}
		if exprText := checkExprText(part); exprText != "" {
			return exprText
		}
	}
	return sql.ExprString(tcs[idx].Expr)
}

// checkIndexAmong returns the zero-based index of the idx-th table constraint
// among the CHECK-type constraints at or before idx.
func checkIndexAmong(tcs []sql.TableConstraint, idx int) int {
	checkIdx := 0
	for i := 0; i <= idx && i < len(tcs); i++ {
		if tcs[i].Type == sql.ConstraintCheck {
			checkIdx++
		}
	}
	return checkIdx - 1
}

// hasTableLevelCheck reports whether a column-definition fragment is a
// table-level (not column-level) CHECK: the fragment's first keyword is
// CHECK or CONSTRAINT (a column-level CHECK is preceded by the column name
// and type).
