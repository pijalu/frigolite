package execconstraint

// The compile-time authorizer events of foreign-key processing.
//
// SQLite's authorizer fires at CODE GENERATION: sqlite3FkCheck's two loops
// (fkey.c:888-1046) and sqlite3FkActions' action-trigger compilation
// (fkey.c:1416-1440) run once per statement, and every permission request
// they issue is observable through sqlite3_set_authorizer
// (without_rowid3-18.x / fkey2-18.x record the exact sequences).
//
// Per call site:
//   - fkey.c:977-990 (first loop, the modified table as CHILD): one
//     SQLITE_READ (parent table, parent key column, db) per FK column,
//     issued after the parent and its key resolve. Skipped for an UPDATE
//     whose child key columns are all unmodified (fkChildIsModified,
//     fkey.c:798) unless the constraint is self-referential.
//   - resolve.c:842 via fkScanChildren (fkey.c:547, second loop, the
//     modified table as PARENT): the scan's WHERE `parent.key = child.key`
//     resolves the CHILD-side column references (the parent side is a
//     TK_REGISTER and reads no auth), so one SQLITE_READ (child table,
//     child key column, db) per FK column per scan. Skipped when the
//     parent key is unmodified (fkParentIsModified, fkey.c:884) and for an
//     immediate constraint on a single-row parent INSERT (fkey.c:1025-1036
//     — "inserting a single row into a parent table cannot cause (or fix)
//     an immediate foreign key violation").
//   - fkey.c:1215-1407 fkActionTrigger + trigger.c:1272-1348 codeRowTrigger:
//     the ON UPDATE/DELETE action trigger compiles once per statement
//     (trigger.c getRowTrigger cache) with zAuthContext = pTrigger->zName =
//     NULL (the malloc-zeroed anonymous trigger), so its events carry
//     arg4 = "". Resolution order: WHEN `NOT(old.pk IS new.pk)` (UPDATE
//     only, two parent-col reads), then the step: SET resolve (one
//     parent-col read for CASCADE-UPDATE), the child statement's per-column
//     auth (update.c:508), WHERE `old.pk = <ck>` (parent col + child col),
//     the child statement's own FkCheck first-loop reads (two passes for a
//     trigger step compiled from UPDATE, one for DELETE), then the child's
//     own FkActions (recursion).

import (
	"strings"

	"github.com/pijalu/frigolite/internal/auth"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// fkAuthDepthCap bounds the recursive action-trigger event walk (a cascade
// child may itself be a parent with an action). C recurses through nested
// trigger sub-programs; the cap keeps malformed self-referential schemas
// from looping.
const fkAuthDepthCap = 8

// AuthFKEvents fires the compile-time authorizer events of one INSERT,
// UPDATE or DELETE's FK processing, in code-generation order. kind is
// "INSERT" / "UPDATE" / "DELETE"; changedCols lists the UPDATE's SET columns
// ("ROWID" when the statement rewrites the rowid) and is nil for
// INSERT/DELETE; singleRowInsert mirrors fkey.c's !pParse->isMultiWrite for
// a VALUES-tuple INSERT (engine_core_tail.go validateDMLFKPrepare's rule).
// The statement's own SQLITE_INSERT/UPDATE/DELETE event is the executor's
// (insert.c:969 / update.c:508 / delete.c:393) and fires before this walk.
// A no-op when PRAGMA foreign_keys is off or no authorizer is registered
// (C gates the walk on db->flags & SQLITE_ForeignKeys and every request on
// db->xAuth).
func (c *ConstraintEnforcer) AuthFKEvents(kind string, entry *schema.Entry, ownCtx *DatabaseContext, changedCols []string, singleRowInsert bool) {
	if entry == nil || !c.ctx.ForeignKeys() || !c.ctx.HasAuthorizer() {
		return
	}
	if ownCtx == nil {
		ownCtx = c.ctx.CurrentDMLCtx()
	}
	if ownCtx == nil {
		_, ownCtx, _ = c.ctx.FindTable(entry.Name)
	}
	if ownCtx == nil {
		return
	}
	c.authFKStatement(kind, entry, ownCtx, changedCols, singleRowInsert, 0)
}

// authFKStatement fires one statement's full compile-time event stream:
// the FkCheck passes (one for INSERT/DELETE, two for UPDATE — update.c:1049
// and update.c:1093 each run both FK loops) then the action-trigger
// compilation (update.c:1107 / delete.c:865).
func (c *ConstraintEnforcer) authFKStatement(kind string, entry *schema.Entry, ownCtx *DatabaseContext, changedCols []string, singleRowInsert bool, depth int) {
	if depth > fkAuthDepthCap {
		return
	}
	passes := 1
	if kind == "UPDATE" {
		passes = 2
	}
	for i := 0; i < passes; i++ {
		c.authFKCheckPass(kind, entry, ownCtx, changedCols, singleRowInsert, depth)
	}
	c.authFKActions(kind, entry, ownCtx, changedCols, depth)
}

// authFKCheckPass fires one FkCheck pass: the modified table's own FK
// constraints (fkey.c:913-1014, the child side) then every child constraint
// referencing it (fkey.c:1020-1046, the parent side).
func (c *ConstraintEnforcer) authFKCheckPass(kind string, entry *schema.Entry, ownCtx *DatabaseContext, changedCols []string, singleRowInsert bool, depth int) {
	if depth > fkAuthDepthCap {
		return
	}
	colDefs := c.ctx.ParseColumnDefs(entry.Name, entry.SQL)

	// First loop: pTab as child. UPDATE skips constraints whose child key
	// is unmodified (fkey.c:920-926); INSERT/DELETE pass aChange=0 and
	// never skip.
	for _, fk := range c.TableFKConstraints(entry, colDefs) {
		if kind == "UPDATE" && !strings.EqualFold(fk.ParentRef, entry.Name) && !fkChildKeyModified(fk, changedCols) {
			continue
		}
		parentEntry, parentCtx, parentCols, _, errRes := c.fkResolveParentForCheck(entry, fk, ownCtx)
		if errRes != nil {
			continue
		}
		c.authReadColumns(parentEntry.Name, parentCtx.Name, parentCols)
	}

	// Second loop: pTab as parent. The scan's WHERE resolves the child key
	// columns (resolve.c:842 via fkScanChildren).
	for _, childCtx := range c.ctx.DBList() {
		if childCtx == nil || childCtx.Schema == nil {
			continue
		}
		entries, err := childCtx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !strings.Contains(strings.ToUpper(ent.SQL), "REFERENCES") {
				continue
			}
			childColDefs := c.ctx.ParseColumnDefs(ent.Name, ent.SQL)
			for _, fk := range c.TableFKConstraints(ent, childColDefs) {
				pEntry, pCtx, rerr := c.fkResolveParent(childCtx, fk.ParentRef)
				if rerr != nil || pCtx != ownCtx || !strings.EqualFold(pEntry.Name, entry.Name) {
					continue
				}
				// fkey.c:1025-1036: a single-row INSERT into a parent
				// cannot cause or fix an immediate violation. UPDATE and
				// DELETE statements pass isMultiWrite-equivalent (the
				// row loop is a multi-write-capable statement), so the
				// scan fires.
				if kind == "INSERT" && singleRowInsert && !fk.Deferred && !c.ctx.DeferForeignKeys() {
					continue
				}
				parentCols := fk.ParentCols
				if len(parentCols) == 0 {
					parentCols = c.fkParentPKColumns(entry, colDefs)
				}
				if kind == "UPDATE" && !columnsOverlap(parentCols, changedCols) {
					continue
				}
				c.authReadColumns(ent.Name, childCtx.Name, fk.ChildCols)
			}
		}
	}
}

// authFKActions fires the action-trigger compilation events (fkey.c:1416
// sqlite3FkActions): for every child constraint referencing the modified
// table whose parent key the statement changes, the ON UPDATE / ON DELETE
// action trigger compiles once per statement.
func (c *ConstraintEnforcer) authFKActions(kind string, entry *schema.Entry, ownCtx *DatabaseContext, changedCols []string, depth int) {
	if depth > fkAuthDepthCap {
		return
	}
	if kind != "UPDATE" && kind != "DELETE" {
		return
	}
	colDefs := c.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, childCtx := range c.ctx.DBList() {
		if childCtx == nil || childCtx.Schema == nil {
			continue
		}
		entries, err := childCtx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if !strings.Contains(strings.ToUpper(ent.SQL), "REFERENCES") {
				continue
			}
			childColDefs := c.ctx.ParseColumnDefs(ent.Name, ent.SQL)
			for _, fk := range c.TableFKConstraints(ent, childColDefs) {
				pEntry, pCtx, rerr := c.fkResolveParent(childCtx, fk.ParentRef)
				if rerr != nil || pCtx != ownCtx || !strings.EqualFold(pEntry.Name, entry.Name) {
					continue
				}
				action := fk.OnDelete
				if kind == "UPDATE" {
					action = fk.OnUpdate
				}
				if kind == "UPDATE" {
					parentCols := fk.ParentCols
					if len(parentCols) == 0 {
						parentCols = c.fkParentPKColumns(entry, colDefs)
					}
					if !columnsOverlap(parentCols, changedCols) {
						continue
					}
				}
				c.authActionTriggerEvents(kind, action, entry, ownCtx, colDefs, ent, childCtx, childColDefs, fk, depth)
			}
		}
	}
}

// authActionTriggerEvents fires one action trigger's compile events, in
// codeRowTrigger's resolution order (trigger.c:1312 WHEN, then the step:
// SET resolve, per-column statement auth, WHERE resolve, the child
// statement's own FkCheck first-loop passes, the child's own actions).
func (c *ConstraintEnforcer) authActionTriggerEvents(kind, action string, parentEntry *schema.Entry, parentCtx *DatabaseContext, parentColDefs []sql.ColumnDef, childEntry *schema.Entry, childCtx *DatabaseContext, childColDefs []sql.ColumnDef, fk FKConstraint, depth int) {
	if depth > fkAuthDepthCap || action == "" || strings.EqualFold(action, "NO ACTION") {
		return
	}
	if strings.EqualFold(action, "RESTRICT") {
		// C compiles a synthetic "SELECT raise(FAIL,...) FROM child WHERE
		// old.pk = ck" whose SQLITE_SELECT/READ events ride the select
		// expression path (carded residual — see PLAN_S37 §Residuals).
		return
	}
	parentCols := fk.ParentCols
	if len(parentCols) == 0 {
		parentCols = c.fkParentPKColumns(parentEntry, parentColDefs)
	}
	childKey := fk.ChildCols
	childKind := "UPDATE"
	passes := 2
	if kind == "DELETE" {
		// CASCADE-DELETE compiles `DELETE FROM child WHERE old.pk = ck`
		// (fkey.c:1391-1396); SET NULL/SET DEFAULT on a DELETE still
		// compile an UPDATE step (fkey.c:1399-1403), just without a WHEN
		// clause (fkActionTrigger builds pWhen only for pChanges != 0).
		if strings.EqualFold(action, "CASCADE") {
			childKind = "DELETE"
			passes = 1
		}
	}

	if kind == "UPDATE" {
		// WHEN `NOT(old.pk IS new.pk)` (fkey.c:1279-1291): one read per
		// OLD./NEW. reference.
		c.authReadColumns(parentEntry.Name, parentCtx.Name, parentCols)
		c.authReadColumns(parentEntry.Name, parentCtx.Name, parentCols)
	}
	if strings.EqualFold(action, "CASCADE") && kind == "UPDATE" {
		// SET `ck = new.pk` (fkey.c:1303-1310): the SET expression's
		// parent-key reference resolves (update.c:470).
		c.authReadColumns(parentEntry.Name, parentCtx.Name, parentCols)
	}
	// SET DEFAULT resolves the child column's default expression — a
	// literal or NULL in the accepted grammar, no column reads.

	// The step's per-column statement auth (update.c:508).
	if childKind == "DELETE" {
		c.ctx.AuthorizeResult(auth.ActionDelete, childEntry.Name, "", childCtx.Name, "")
	} else {
		for _, ck := range childKey {
			c.ctx.AuthorizeResult(auth.ActionUpdate, childEntry.Name, ck, childCtx.Name, "")
		}
	}

	// WHERE `old.pk = ck` (fkey.c:1259-1272): the parent-key reference
	// resolves first (the OLD. term is the LHS), then the child column.
	c.authReadColumns(parentEntry.Name, parentCtx.Name, parentCols)
	c.authReadColumns(childEntry.Name, childCtx.Name, childKey)

	// The child statement's own FkCheck passes (its first loop re-reads
	// the parent key; its second loop and actions recurse).
	childChanged := childKey
	if childKind == "DELETE" {
		childChanged = nil
	}
	for i := 0; i < passes; i++ {
		c.authFKCheckPass(childKind, childEntry, childCtx, childChanged, false, depth+1)
	}
	c.authFKActions(childKind, childEntry, childCtx, childChanged, depth+1)
}

// authReadColumns issues one SQLITE_READ per column
// (sqlite3AuthReadCol's per-column loop, fkey.c:981-984).
func (c *ConstraintEnforcer) authReadColumns(table, dbName string, cols []string) {
	for _, col := range cols {
		c.ctx.AuthorizeResult(auth.ActionRead, table, col, dbName, "")
	}
}

// fkChildKeyModified reports whether any child-key column (or the rowid
// alias standing in for one) is assigned by the statement
// (fkChildIsModified, fkey.c:798).
func fkChildKeyModified(fk FKConstraint, changedCols []string) bool {
	if len(changedCols) == 0 {
		return false
	}
	rowidChanged := false
	for _, c := range changedCols {
		if strings.EqualFold(c, "ROWID") {
			rowidChanged = true
		}
	}
	for _, cc := range fk.ChildCols {
		for _, ch := range changedCols {
			if strings.EqualFold(cc, ch) {
				return true
			}
			if rowidChanged && isRowidAliasName(cc) {
				return true
			}
		}
	}
	return false
}

// isRowidAliasName reports whether name is one of the rowid pseudo-aliases.
func isRowidAliasName(name string) bool {
	n := strings.ToLower(name)
	return n == "rowid" || n == "_rowid_" || n == "oid"
}

// columnsOverlap reports whether any name in a matches any name in b
// (case-insensitive; empty b = nothing matches).
func columnsOverlap(a, b []string) bool {
	if len(b) == 0 {
		return false
	}
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}
