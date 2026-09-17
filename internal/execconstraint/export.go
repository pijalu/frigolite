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

// ValidateDMLTableFKs resolves the foreign-key relationships of the table a
// pending INSERT/UPDATE/DELETE targets before any row is read — fk.c's
// sqlite3FkCheck runs during statement compilation, so a broken FK fails the
// statement even when no row is ever touched (e_fkey-20.x: an UPDATE of an
// empty child table must still report "no such table: main.X"; a DELETE on a
// parent whose child's parent-key cannot be located reports
// "foreign key mismatch"). Both directions are checked: this table as a CHILD
// (its own FKs must resolve to a parent table + locatable parent key) and
// this table as a PARENT (each child FK referencing it must be able to locate
// the parent key). Only active when PRAGMA foreign_keys is ON. entry/ownCtx
// resolve the table in the schema that owns it. singleRowInsert mirrors
// pParse->isMultiWrite==0: inserting single rows into a parent cannot cause
// or fix an immediate FK violation, so fkey.c skips the parent-key location
// (and its mismatch error) for non-deferred child FKs in that case
// (e_fkey-19.2's INSERT INTO parent must succeed despite broken child4).
func (c *ConstraintEnforcer) ValidateDMLTableFKs(entry *schema.Entry, ownCtx *DatabaseContext, singleRowInsert bool) *Result {
	if entry == nil || !c.ctx.ForeignKeys() {
		return nil
	}
	if ownCtx == nil {
		ownCtx = c.ctx.CurrentDMLCtx()
	}
	if ownCtx == nil {
		// Fall back to the main schema, mirroring fkResolveParent's default.
		_, ownCtx, _ = c.ctx.FindTable(entry.Name)
	}
	if ownCtx == nil {
		return nil
	}
	// Child side: the table's own FKs must resolve (fkLookupParent).
	colDefs := c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, fk := range c.TableFKConstraints(entry, colDefs) {
		if _, _, _, _, errRes := c.fkResolveParentForCheck(entry, fk, ownCtx); errRes != nil {
			return errRes
		}
	}
	// Parent side: every child FK referencing this table must locate this
	// table's parent key (fkScanChildren).
	if res := c.validateChildrenParentKeys(entry, ownCtx, singleRowInsert); res != nil {
		return res
	}
	return nil
}

// validateChildrenParentKeys checks, for every table whose FK references the
// given parent, that the FK's parent key can be located on the parent: an
// explicit parent column must exist, the parent key cardinality must match
// the child key, and the parent key must be the PK or a qualifying UNIQUE
// index (sqlite3FkLocateIndex). Otherwise the child's mismatch error is
// reported, naming the child table and the parent reference as written.
func (c *ConstraintEnforcer) validateChildrenParentKeys(entry *schema.Entry, ownCtx *DatabaseContext, singleRowInsert bool) *Result {
	parentColDefs := c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, ctx2 := range c.ctx.Databases() {
		entries, err := ctx2.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !strings.Contains(strings.ToUpper(ent.SQL), "REFERENCES") {
				continue
			}
			childColDefs := c.ctx.ParseColumnDefs(ent.Name, ent.SQL)
			for _, fk := range c.TableFKConstraints(ent, childColDefs) {
				pEntry, pCtx, rerr := c.fkResolveParent(ctx2, fk.ParentRef)
				if rerr != nil || pCtx != ownCtx || !strings.EqualFold(pEntry.Name, entry.Name) {
					continue
				}
				// fkey.c: inserting a single row into a parent table cannot
				// cause (or fix) an immediate FK violation — the parent-key
				// location (and mismatch error) is skipped for non-deferred
				// child FKs when DeferFKs is off (e_fkey-19.2 vs 20.6).
				if singleRowInsert && !fk.Deferred && !c.ctx.DeferForeignKeys() {
					continue
				}
				pCols := fk.ParentCols
				if len(pCols) == 0 {
					pCols = c.fkParentPKColumns(entry, parentColDefs)
				}
				if len(pCols) == len(fk.ChildCols) && c.fkParentKeyValid(ownCtx, entry, parentColDefs, pCols) {
					continue
				}
				return &Result{Error: fmt.Errorf("foreign key mismatch - %q referencing %q", ent.Name, fk.ParentRef)}
			}
		}
	}
	return nil
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

// RemoveFKDirtyTable drops ONE dropped table's entry from the deferred-FK
// dirty set: after DROP TABLE the entry's root page may be reused by a later
// CREATE, and re-validating it would decode a different table's rows.
func (c *ConstraintEnforcer) RemoveFKDirtyTable(entry *schema.Entry, ctx *DatabaseContext) {
	c.removeFKDirtyTable(entry, ctx)
}
