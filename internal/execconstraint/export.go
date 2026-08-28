package execconstraint

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// ValidateFKDefinitions validates FOREIGN KEY definitions at CREATE TABLE
// time, mirroring sqlite3FkParseError / sqlite3CreateForeignKey in build.c:
// child key columns must exist and the child/parent key cardinalities must
// match (SQLite R-41062-34431). Parent table existence/columns are NOT
// checked (SQLite R-36018-21755: parent keys are not validated at CREATE).
// The check runs regardless of PRAGMA foreign_keys. Returns nil when valid.
func (c *ConstraintEnforcer) ValidateFKDefinitions(tableName string, colDefs []sql.ColumnDef, createSQL string) error {
	hasCol := func(name string) bool {
		for _, cd := range colDefs {
			if strings.EqualFold(cd.Name, name) {
				return true
			}
		}
		return false
	}
	// Reuse the FK parser: a synthetic entry whose SQL is the CREATE TABLE
	// text and name the table under construction (TableFKConstraints reads
	// colDefs directly and table-level constraints via TableConstraints).
	entry := &schema.Entry{Name: tableName, SQL: createSQL, Type: schema.TypeTable}
	fks := c.TableFKConstraints(entry, colDefs)
	for _, fk := range fks {
		// A column-level REFERENCES with an explicit multi-column parent key
		// is rejected: a single child column cannot map to several parent
		// columns (e_fkey-28.1: CREATE TABLE c(jj REFERENCES p(x, y))).
		if fk.ColumnLevel && len(fk.ChildCols) == 1 && len(fk.ParentCols) > 1 {
			return fmt.Errorf("foreign key on %s should reference only one column of table %s", fk.ChildCols[0], fk.ParentRef)
		}
		// Explicit parent columns: cardinality must match the child key. This
		// is checked BEFORE child-column existence (SQLite reports
		// "number of columns..." for FOREIGN KEY(c,b) REFERENCES p(d) even
		// though c is unknown).
		if len(fk.ParentCols) > 0 && len(fk.ParentCols) != len(fk.ChildCols) {
			return fmt.Errorf("number of columns in foreign key does not match the number of columns in the referenced table")
		}
		// Child key columns must exist in the child table.
		for _, col := range fk.ChildCols {
			if !hasCol(col) {
				return fmt.Errorf("unknown column %q in foreign key definition", col)
			}
		}
	}
	return nil
}

// CheckForeignKeyViolations verifies that every non-NULL column value with a
// FOREIGN KEY clause references an existing parent row. It is only enforced
// when PRAGMA foreign_keys is ON. Returns an error describing the first
// violation. excludeRowID is the rowid of the row being updated (for
// self-referential FKs the row's OLD key value would otherwise falsely
// satisfy the parent lookup); pass 0 for INSERT.
func (c *ConstraintEnforcer) CheckForeignKeyViolations(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, excludeRowID int64) *Result {
	return c.checkForeignKeyViolations(tableEntry, colDefs, values, excludeRowID)
}

// FkParentDelete enforces FOREIGN KEY actions when a parent row is deleted:
// RESTRICT/NO ACTION children cause an error; CASCADE children are deleted
// (recursively, since a cascaded child may itself be a parent); SET NULL /
// SET DEFAULT children update their FK column.
func (c *ConstraintEnforcer) FkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return c.fkParentDelete(parentTable, parentColDefs, oldRow)
}

// FkParentDeleteReplace is FkParentDelete for the implicit delete performed by
// INSERT OR REPLACE. NO ACTION / RESTRICT violations are not reported here:
// the REPLACE may re-insert the same key, so the constraint is checked after
// the new row is written (SQLite defers the REPLACE delete's NO ACTION check
// to statement end).
func (c *ConstraintEnforcer) FkParentDeleteReplace(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return c.fkParentDeleteReplace(parentTable, parentColDefs, oldRow)
}

// FkParentUpdate enforces FOREIGN KEY actions when a parent row's key changes:
// the old key value is checked against children (RESTRICT/NO ACTION error,
// CASCADE propagates the new value, SET NULL/SET DEFAULT update the column).
// skipRowID identifies the parent row being updated; when it is also a child
// row (self-referential FK) whose FK columns are updated consistently, it is
// not a conflict.
func (c *ConstraintEnforcer) FkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result {
	return c.fkParentUpdate(parentTable, parentColDefs, oldRow, newRow, skipRowID)
}

// FkParentDropTable enforces FOREIGN KEY actions when a table is dropped.
// Unlike a DELETE statement, no trigger can re-insert the parent rows, so the
// trigger-reinsert check is disabled.
func (c *ConstraintEnforcer) FkParentDropTable(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return c.fkParentDropTable(parentTable, parentColDefs, oldRow)
}

// FkCheckReplaceChildren verifies that the children of a table replaced by
// INSERT OR REPLACE still reference an existing parent key after the new row
// is written (SQLite checks the implicit delete's NO ACTION constraint at
// statement end, not at COMMIT).
func (c *ConstraintEnforcer) FkCheckReplaceChildren(parentEntry *schema.Entry, parentCtx *DatabaseContext) *Result {
	return c.fkCheckReplaceChildren(parentEntry, parentCtx)
}

// MarkFKDirty records that a table's rows changed; its FK relationships (as
// child or parent) must be re-validated at COMMIT / statement end.
func (c *ConstraintEnforcer) MarkFKDirty(entry *schema.Entry, ctx *DatabaseContext) {
	c.markFKDirty(entry, ctx)
}

// MarkFKParentDirty records that a table's PARENT rows changed (UPDATE or
// DELETE), so its children's FK references are re-validated at COMMIT /
// statement end. INSERT does not orphan children and must not use this.
func (c *ConstraintEnforcer) MarkFKParentDirty(entry *schema.Entry, ctx *DatabaseContext) {
	c.markFKParentDirty(entry, ctx)
}

// ResetFKDirty clears the dirty-table set (at BEGIN, COMMIT, ROLLBACK, and
// after a statement-end check).
func (c *ConstraintEnforcer) ResetFKDirty() {
	c.resetFKDirty()
}

// CheckDeferredFK re-validates the FK relationships of every table modified in
// the current transaction/statement and returns "FOREIGN KEY constraint failed"
// when any violation exists. It is called at COMMIT (and at statement end in
// autocommit mode) for deferred constraints and when PRAGMA
// defer_foreign_keys is ON. When onlyImmediate is true (a statement-end check
// inside an open transaction), DEFERRABLE INITIALLY DEFERRED constraints and
// immediate constraints while PRAGMA defer_foreign_keys is ON are skipped: they
// are checked at COMMIT, not per-statement.
func (c *ConstraintEnforcer) CheckDeferredFK(onlyImmediate bool) error {
	return c.checkDeferredFK(onlyImmediate)
}
