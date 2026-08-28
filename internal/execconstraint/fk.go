package execconstraint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execdml"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// FKRefAction describes one FOREIGN KEY constraint with its ON DELETE and
// ON UPDATE actions ("" = NO ACTION). childCols and parentCols are parallel
// arrays (child column i maps to parent column i).
type FKRefAction struct {
	ChildTable  string
	ChildCtx    *DatabaseContext // schema owning the child table
	ChildCols   []string
	ParentTable string
	ParentCols  []string
	OnDelete    string // "", "CASCADE", "SET NULL", "SET DEFAULT", "RESTRICT"
	OnUpdate    string
	Deferred    bool // DEFERRABLE INITIALLY DEFERRED (checked at COMMIT)
}

// colIndexLookup resolves a column name against a column index map built by
// buildColumnIndex (whose keys are lowercased). FK constraint column names keep
// their declared case, so lookups must be case-insensitive (e.g. ParentId
// references Foo(Id)).
func colIndexLookup(idx map[string]int, name string) (int, bool) {
	if idx == nil {
		return 0, false
	}
	i, ok := idx[strings.ToLower(name)]
	return i, ok
}

// fkRefFullRe parses "parentTable(parentCol) ON DELETE X ON UPDATE Y" (any
// subset) from a column's References string, with an optional trailing
// "DEFERRABLE INITIALLY DEFERRED" clause. A bare "parentTable" reference
// (no parent column) implicitly targets the parent's PRIMARY KEY.
var fkRefFullRe = regexp.MustCompile(`(?is)^\s*([^\s(]+)(?:\s*\(([^)]+)\))?((?:\s+ON\s+(?:DELETE|UPDATE)\s+(?:CASCADE|RESTRICT|SET\s+NULL|SET\s+DEFAULT|NO\s+ACTION))*(?:\s+MATCH\s+[^\s]+)?)((?:\s+(?:NOT\s+)?DEFERRABLE(?:\s+INITIALLY\s+(?:DEFERRED|IMMEDIATE))?)?)\s*$`)

// fkActionInRefs extracts the ON DELETE/ON UPDATE action from a References
// string's action segment.
func fkActionInRefs(segment, kind string) string {
	upper := strings.ToUpper(segment)
	marker := "ON " + kind + " "
	idx := strings.Index(upper, marker)
	if idx < 0 {
		return ""
	}
	rest := upper[idx+len(marker):]
	for _, action := range []string{"CASCADE", "RESTRICT", "SET NULL", "SET DEFAULT", "NO ACTION"} {
		if strings.HasPrefix(rest, action) {
			return action
		}
	}
	return ""
}

// fkActionFromText extracts the ON DELETE/ON UPDATE action from a table-level
// FK action string (e.g. "ON DELETE SET NULL ON UPDATE CASCADE").
func fkActionFromText(text, kind string) string {
	upper := strings.ToUpper(text)
	marker := "ON " + kind + " "
	idx := strings.Index(upper, marker)
	if idx < 0 {
		return ""
	}
	rest := upper[idx+len(marker):]
	for _, action := range []string{"CASCADE", "RESTRICT", "SET NULL", "SET DEFAULT", "NO ACTION"} {
		if strings.HasPrefix(rest, action) {
			return action
		}
	}
	return ""
}

// fkChildRefs returns the FOREIGN KEY references whose parent table is the
// given table, across all attached databases. A child's FK is included only
// when its parent reference resolves (in the child's own schema, matching
// SQLite's same-database parent lookup) to the given parent entry.

// fkParentDelete enforces FOREIGN KEY actions when a parent row is deleted:
// RESTRICT/NO ACTION children cause an error; CASCADE children are deleted
// (recursively, since a cascaded child may itself be a parent); SET NULL /
// SET DEFAULT children update their FK column. The NO ACTION error is deferred
// to statement end (deferNoAction=true): SQLite checks it when the statement
// completes, after AFTER triggers have run, so a trigger that re-inserts the
// parent key satisfies the constraint (without_rowid3-12.2.2). The statement-
// end check catches orphans (execCheckDeferredFK).
func (c *ConstraintEnforcer) fkParentDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	const maxDepth = 1000
	var rec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result
	rec = func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result {
		if depth > maxDepth {
			return &Result{}
		}
		return c.fkParentActionRec(entry, colDefs, row, nil, true, 0, true, true, rec, depth)
	}
	return rec(parentTable, parentColDefs, oldRow, 0)
}

// fkParentDeleteReplace is fkParentDelete for the implicit delete performed by
// INSERT OR REPLACE. NO ACTION / RESTRICT violations are not reported here:
// the REPLACE may re-insert the same key, so the constraint is checked after
// the new row is written (SQLite defers the REPLACE delete's NO ACTION check
// to statement end).
func (c *ConstraintEnforcer) fkParentDeleteReplace(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	const maxDepth = 1000
	var rec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result
	rec = func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result {
		if depth > maxDepth {
			return &Result{}
		}
		return c.fkParentActionRec(entry, colDefs, row, nil, true, 0, true, true, rec, depth)
	}
	return rec(parentTable, parentColDefs, oldRow, 0)
}

// fkParentDropTable enforces FOREIGN KEY actions when a table is dropped.
// Unlike a DELETE statement, no trigger can re-insert the parent rows, so the
// trigger-reinsert check is disabled.
func (c *ConstraintEnforcer) fkParentDropTable(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow RowMap) *Result {
	return c.fkParentAction(parentTable, parentColDefs, oldRow, nil, true, 0, false, false)
}

// fkParentUpdate enforces FOREIGN KEY actions when a parent row's key changes:
// the old key value is checked against children (RESTRICT errors immediately,
// NO ACTION is deferred to statement end so an AFTER UPDATE trigger may repair
// the children, CASCADE propagates the new value, SET NULL/SET DEFAULT update
// the column). skipRowID identifies the parent row being updated; when it is
// also a child row (self-referential FK) whose FK columns are updated
// consistently, it is not a conflict.
func (c *ConstraintEnforcer) fkParentUpdate(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, skipRowID int64) *Result {
	// NO ACTION is deferred to statement end (SQLite checks immediate FKs
	// after the statement completes — AFTER triggers may repair the children
	// by cascading the new parent key, e_fkey-42.3). The statement-end check
	// runs via execPostFK's CheckDeferredFK on the parent-dirty children.
	return c.fkParentAction(parentTable, parentColDefs, oldRow, newRow, false, skipRowID, true, true)
}

// fkParentAction is the shared implementation for parent DELETE/UPDATE FK
// enforcement. newRow is non-nil for UPDATE (CASCADE propagates the new key).
// deferNoAction suppresses the immediate NO ACTION/RESTRICT error (used by
// INSERT OR REPLACE, whose implicit delete may be followed by a re-insert of
// the same key; the constraint is then checked after the new row is written).
func (c *ConstraintEnforcer) fkParentAction(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, oldRow, newRow RowMap, isDelete bool, skipRowID int64, checkTriggerReinsert, deferNoAction bool) *Result {
	return c.fkParentActionRec(parentTable, parentColDefs, oldRow, newRow, isDelete, skipRowID, checkTriggerReinsert, deferNoAction, nil, 0)
}

// fkParentActionRec is fkParentAction with a recursive CASCADE callback
// (cascadeRec, depth) used when a CASCADE delete removes a row that is itself
// a parent. The existing public entry points pass nil.

// fkParentKeyIndices resolves the child/parent column index pairs for a FK
// reference, extracts the old parent key values, and reports whether the key
// changed (for UPDATE) and whether all key values are non-NULL.

// fkFindChildMatches scans the child table for rows whose FK columns equal the
// old parent key values, returning the matched rowids and decoded values. The
// parent row being updated/deleted (skipRowID) is skipped only for
// self-referential FKs.

// fkParentRefAction applies one foreign-key ON action (RESTRICT / NO ACTION,
// CASCADE, SET NULL, SET DEFAULT) to the matched child rows. Returns a
// non-nil Result when the action failed.

// fkChildMatch is one matched child row (its rowid and decoded values).
type fkChildMatch struct {
	rowID  int64
	values []interface{}
}

// fkCascadeDelete deletes a matched child row as the CASCADE ON DELETE
// action, firing before/after delete triggers and recursing into deeper FK
// chains.
func (c *ConstraintEnforcer) fkCascadeDelete(m fkChildMatch, childEntry *schema.Entry, childColDefs []sql.ColumnDef, tree *btree.BTree, cascadeRec func(entry *schema.Entry, colDefs []sql.ColumnDef, row RowMap, depth int) *Result, depth int) *Result {
	oldChildRow := execdml.BuildRowMapFromValues(m.values, childColDefs, m.rowID)
	hasTriggers := c.ctx.HasTriggersForTable(childEntry.Name)
	if hasTriggers {
		if trigResult := c.ctx.FireBeforeDeleteTriggers(childEntry.Name, oldChildRow); trigResult.Error != nil {
			return trigResult
		}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == m.rowID
	}); err != nil {
		return &Result{Error: err}
	}
	c.ctx.InvalidateRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage)
	// Recursively enforce FK actions for the deleted child: it
	// may itself be a parent (self-referential FK chains). The
	// depth guard breaks cycles (SQLite allows CASCADE cycles
	// only when the chain terminates).
	if cascadeRec != nil {
		if res := cascadeRec(childEntry, childColDefs, oldChildRow, depth+1); res.Error != nil {
			return res
		}
	}
	if hasTriggers {
		if trigResult := c.ctx.FireAfterDeleteTriggers(childEntry.Name, oldChildRow); trigResult.Error != nil {
			return trigResult
		}
	}
	c.ctx.BumpTotalChanges(1)
	return nil
}

// fkCascadeUpdate propagates the parent's new key values to a matched child
// row as the CASCADE ON UPDATE action.
func (c *ConstraintEnforcer) fkCascadeUpdate(m fkChildMatch, ref FKRefAction, childEntry *schema.Entry, childIdxs, parentIdxs []int, newRow RowMap, parentColDefs []sql.ColumnDef, tree *btree.BTree) *Result {
	vals := make([]interface{}, len(m.values))
	copy(vals, m.values)
	for i, cidx := range childIdxs {
		if i < len(ref.ParentCols) && cidx < len(vals) {
			newValRaw, _ := newRow.Get(ref.ParentCols[i])
			newVal := unwrapRowValue(newValRaw)
			vals[cidx] = util.ApplyColumnAffinity(newVal, parentColDefs[parentIdxs[i]].Type)
		}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == m.rowID
	}); err != nil {
		return &Result{Error: err}
	}
	c.ctx.InvalidateRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage)
	newRecord, err := storage.EncodeRecord(vals)
	if err != nil {
		return &Result{Error: err}
	}
	newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
	if err := tree.InsertCell(newCell); err != nil {
		return &Result{Error: err}
	}
	c.ctx.BumpRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage, m.rowID)
	c.ctx.BumpTotalChanges(1)
	return nil
}

// fkSetNull sets the child FK columns to NULL as the SET NULL action.
func (c *ConstraintEnforcer) fkSetNull(m fkChildMatch, childEntry *schema.Entry, childIdxs []int, tree *btree.BTree) *Result {
	vals := make([]interface{}, len(m.values))
	copy(vals, m.values)
	for _, cidx := range childIdxs {
		if cidx < len(vals) {
			vals[cidx] = nil
		}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == m.rowID
	}); err != nil {
		return &Result{Error: err}
	}
	c.ctx.InvalidateRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage)
	newRecord, err := storage.EncodeRecord(vals)
	if err != nil {
		return &Result{Error: err}
	}
	newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
	if err := tree.InsertCell(newCell); err != nil {
		return &Result{Error: err}
	}
	c.ctx.BumpRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage, m.rowID)
	c.ctx.BumpTotalChanges(1)
	return nil
}

// fkSetDefault sets the child FK columns to their declared DEFAULT values as
// the SET DEFAULT action.
func (c *ConstraintEnforcer) fkSetDefault(m fkChildMatch, childEntry *schema.Entry, childIdxs []int, childColDefs []sql.ColumnDef, tree *btree.BTree) *Result {
	vals := make([]interface{}, len(m.values))
	copy(vals, m.values)
	for _, cidx := range childIdxs {
		if cidx < len(vals) {
			if childColDefs[cidx].Default != nil {
				if dv, err := c.ctx.EvalExpr(childColDefs[cidx].Default, nil); err == nil {
					vals[cidx] = dv
				}
			} else {
				vals[cidx] = nil
			}
		}
	}
	if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
		return cell.RowID == m.rowID
	}); err != nil {
		return &Result{Error: err}
	}
	c.ctx.InvalidateRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage)
	newRecord, err := storage.EncodeRecord(vals)
	if err != nil {
		return &Result{Error: err}
	}
	newCell := &storage.Cell{Type: storage.CellTableLeaf, RowID: m.rowID, Payload: newRecord}
	if err := tree.InsertCell(newCell); err != nil {
		return &Result{Error: err}
	}
	c.ctx.BumpRowIDCache(c.ctx.TablePager(childEntry.Name), childEntry.RootPage, m.rowID)
	c.ctx.BumpTotalChanges(1)
	return nil
}

// unwrapRowValue extracts the raw value from a ColumnValue or CollatedValue
// wrapper (row-map values are wrapped with their column's affinity and
// collation — wrapValueForRowMap). The collation wrapper is a pointer to
// execexpr.CollatedValue holding a *util.ColumnValue inside; unwrap both
// layers so FK matching compares the raw value (e_fkey-52.x's NOCASE
// parent-key CASCADE).
func unwrapRowValue(v interface{}) interface{} {
	for {
		if cv, ok := v.(*execexpr.CollatedValue); ok {
			v = cv.Value
			continue
		}
		if cv, ok := v.(*util.ColumnValue); ok {
			v = cv.Value
			continue
		}
		return v
	}
}

// fkParentPKColumns returns the parent's PRIMARY KEY column names in order.
func (c *ConstraintEnforcer) fkParentPKColumns(parentEntry *schema.Entry, parentColDefs []sql.ColumnDef) []string {
	var cols []string
	for _, cd := range parentColDefs {
		if cd.PrimaryKey {
			cols = append(cols, cd.Name)
		}
	}
	if len(cols) > 0 {
		return cols
	}
	for _, c := range c.ctx.TableConstraints(parentEntry.Name, parentEntry.SQL) {
		if c.Type == sql.ConstraintPrimaryKey {
			for _, ic := range c.Columns {
				cols = append(cols, ic.Name)
			}
			return cols
		}
	}
	return nil
}

// FKConstraint describes one FOREIGN KEY constraint of a child table, in
// fkid order (column-level FKs in column order, then table-level FKs in
// constraint order, matching SQLite's FKey list).
type FKConstraint struct {
	ChildCols  []string // child column names (positional)
	ParentRef  string   // parent table reference (may be schema-qualified)
	ParentCols []string // explicit parent columns (nil = implicit parent PK)
	OnDelete   string   // "", "CASCADE", "SET NULL", "SET DEFAULT", "RESTRICT"
	OnUpdate   string
	Deferred   bool // DEFERRABLE INITIALLY DEFERRED

	// ColumnLevel is true when the FK comes from a column-level REFERENCES
	// clause (a single child column). SQLite rejects a column-level FK whose
	// REFERENCES lists multiple parent columns with a distinct error
	// ("should reference only one column") before the cardinality check.
	ColumnLevel bool
}

// tableFKConstraints returns the table's FOREIGN KEY constraints (column-level
// then table-level) in fkid order. SQLite stores FKs in a linked list that
// PREPENDS each new constraint (sqlite3AddForeignKey), so PRAGMA
// foreign_key_list reports them in reverse declaration order; the returned
// slice mirrors that (last-declared FK first).
func (c *ConstraintEnforcer) TableFKConstraints(entry *schema.Entry, colDefs []sql.ColumnDef) []FKConstraint {
	var fks []FKConstraint
	for _, cd := range colDefs {
		if cd.References == "" {
			continue
		}
		m := fkRefFullRe.FindStringSubmatch(cd.References)
		if m == nil {
			continue
		}
		var parentCols []string
		if pc := strings.TrimSpace(m[2]); pc != "" {
			// The parent column list may hold several columns (a single child
			// column REFERENCES p(x, y) is an error — SQLite requires a
			// one-to-one column mapping); split on commas like the parser does
			// for table-level FOREIGN KEY clauses.
			for _, part := range strings.Split(pc, ",") {
				if name := strings.TrimSpace(part); name != "" {
					parentCols = append(parentCols, name)
				}
			}
		}
		fks = append(fks, FKConstraint{
			ChildCols:   []string{cd.Name},
			ParentRef:   strings.TrimSpace(m[1]),
			ParentCols:  parentCols,
			OnDelete:    fkActionInRefs(m[3], "DELETE"),
			OnUpdate:    fkActionInRefs(m[3], "UPDATE"),
			Deferred:    strings.Contains(strings.ToUpper(m[4]), "INITIALLY DEFERRED"),
			ColumnLevel: true,
		})
	}
	for _, tc := range c.ctx.TableConstraints(entry.Name, entry.SQL) {
		if tc.Type != sql.ConstraintForeignKey || tc.RefTable == "" {
			continue
		}
		var cols []string
		for _, ic := range tc.Columns {
			cols = append(cols, ic.Name)
		}
		fks = append(fks, FKConstraint{
			ChildCols:  cols,
			ParentRef:  tc.RefTable,
			ParentCols: tc.RefCols,
			OnDelete:   fkActionFromText(tc.RefAction, "DELETE"),
			OnUpdate:   fkActionFromText(tc.RefAction, "UPDATE"),
			Deferred:   tc.Deferred,
		})
	}
	// SQLite prepends each FK to the head of its linked list; reverse to
	// match PRAGMA foreign_key_list output (last declared = id 0).
	for i, j := 0, len(fks)-1; i < j; i, j = i+1, j-1 {
		fks[i], fks[j] = fks[j], fks[i]
	}
	return fks
}

// fkResolveParent resolves an FK's parent table. SQLite looks up unqualified
// FK parents only in the schema containing the child table
// (sqlite3LocateTable(db, 0, zTo, zDb) in fkey.c); a schema-qualified parent
// reference is looked up in the named schema.
func (c *ConstraintEnforcer) fkResolveParent(childCtx *DatabaseContext, ref string) (*schema.Entry, *DatabaseContext, error) {
	schemaName, objName := execdml.ParseSchemaName(ref)
	if schemaName != "" {
		ctx := c.ctx.GetDB(schemaName)
		if ctx == nil {
			return nil, nil, fmt.Errorf("no such table: %s", ref)
		}
		ent, err := ctx.Schema.FindTable(objName)
		if err != nil {
			return nil, nil, err
		}
		return ent, ctx, nil
	}
	if childCtx != nil {
		ent, err := childCtx.Schema.FindTable(objName)
		if err != nil {
			return nil, nil, err
		}
		return ent, childCtx, nil
	}
	return c.ctx.FindTable(objName)
}

// fkParentKeyValid reports whether parentCols form a valid parent key for the
// parent table: the PRIMARY KEY, a UNIQUE column/constraint, or a full
// (non-partial, default-collation, non-expression) UNIQUE index — mirroring
// sqlite3FkLocateIndex in fkey.c. parentCols must already be resolved (explicit
// list or the parent's PK for implicit references).

// fkSameColumnSet reports whether two column-name lists contain the same names
// (case-insensitive, order-independent — fkey.c matches the parent key columns
// as a set and maps values positionally).
func fkSameColumnSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for j, y := range b {
			if !used[j] && strings.EqualFold(x, y) {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// fkIndexPlainCols parses an index key column list. It returns the plain
// column names and false when any key is an expression or uses an explicit
// non-default COLLATE (expression keys and non-default collations make the
// index unusable as an FK parent key, matching fkey.c's checks).
func fkIndexPlainCols(colText string) ([]string, bool) {
	var cols []string
	for _, part := range execdml.SplitIndexCols(colText) {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, false
		}
		upper := strings.ToUpper(name)
		// Expression keys are not usable.
		if strings.ContainsAny(name, "()") {
			return nil, false
		}
		// Explicit COLLATE must be the column's default (BINARY) to qualify;
		// any other explicit collation disqualifies the index. We only
		// recognize COLLATE BINARY / no COLLATE as usable.
		if ci := strings.Index(upper, " COLLATE"); ci >= 0 {
			coll := strings.TrimSpace(name[ci+len(" COLLATE"):])
			if !strings.EqualFold(coll, "BINARY") {
				return nil, false
			}
			name = strings.TrimSpace(name[:ci])
		}
		// Strip ASC/DESC.
		if di := strings.Index(upper, " DESC"); di >= 0 {
			name = strings.TrimSpace(name[:di])
		} else if ai := strings.Index(upper, " ASC"); ai >= 0 {
			name = strings.TrimSpace(name[:ai])
		}
		if name == "" {
			return nil, false
		}
		cols = append(cols, name)
	}
	return cols, true
}

// FKViolation is one row of PRAGMA foreign_key_check output: the child table,
// the child rowid (nil for WITHOUT ROWID child tables), the parent table, and
// the FK constraint id.
type FKViolation struct {
	ChildTable  string
	RowID       interface{}
	ParentTable string
	FKID        int
}

// findFKViolations scans FK constraints and returns the violations in the same
// order SQLite reports them (child tables in schema order, rows in rowid
// order). When onlyTable is non-empty it is resolved like an ordinary table
// reference and only that child is scanned; when schemaName is non-empty only
// child tables in that schema are scanned. A missing parent table yields one
// violation per child row (with the referenced parent name); a parent key that
// is not a PRIMARY KEY or UNIQUE index yields a "foreign key mismatch" error
// that aborts the whole check, matching SQLite.

// resolvedFK is one foreign-key constraint with its resolved parent table
// metadata, used by fkCheckChildTable.
type resolvedFK struct {
	fk          FKConstraint
	parentEntry *schema.Entry
	parentCtx   *DatabaseContext
	parentCols  []string
	parentDefs  []sql.ColumnDef
}

// fkResolveChildFKs resolves each FK's parent and validates the parent key up
// front (a mismatch aborts the whole check). Parent tables missing from the
// child's schema are reported as violations below, not errors.
func (c *ConstraintEnforcer) fkResolveChildFKs(ctx *DatabaseContext, fks []FKConstraint, entry *schema.Entry) ([]resolvedFK, error) {
	resolved := make([]resolvedFK, 0, len(fks))
	for _, fk := range fks {
		parentEntry, parentCtx, err := c.fkResolveParent(ctx, fk.ParentRef)
		if err != nil {
			// Missing parent table: every non-NULL-keyed row is a violation.
			resolved = append(resolved, resolvedFK{fk: fk, parentEntry: nil})
			continue
		}
		parentColDefs := c.ctx.ParseColumnDefs(parentEntry.Name, parentEntry.SQL)
		parentCols := fk.ParentCols
		if len(parentCols) == 0 {
			parentCols = c.fkParentPKColumns(parentEntry, parentColDefs)
		}
		if len(parentCols) != len(fk.ChildCols) ||
			!c.fkParentKeyValid(parentCtx, parentEntry, parentColDefs, parentCols) {
			return nil, fmt.Errorf("foreign key mismatch - %q referencing %q", entry.Name, fk.ParentRef)
		}
		resolved = append(resolved, resolvedFK{
			fk:          fk,
			parentEntry: parentEntry,
			parentCtx:   parentCtx,
			parentCols:  parentCols,
			parentDefs:  parentColDefs,
		})
	}
	return resolved, nil
}

// fkCheckChildTable scans one child table's FK constraints and returns its
// violations (rows in rowid order).

// fkDirtyKey identifies a table whose FK relationships changed during the
// current transaction/statement (schema context + table name, so main.c1 and
// aux.c1 are distinct).
type fkDirtyKey struct {
	ctx  *DatabaseContext
	name string
}

// markFKDirty records that a table's rows changed; its FK relationships (as
// child or parent) must be re-validated at COMMIT / statement end.
func (c *ConstraintEnforcer) markFKDirty(entry *schema.Entry, ctx *DatabaseContext) {
	if !c.ctx.ForeignKeys() || entry == nil {
		return
	}
	if c.fkDirty == nil {
		c.fkDirty = make(map[fkDirtyKey]bool)
	}
	c.fkDirty[fkDirtyKey{ctx: ctx, name: entry.Name}] = true
}

// markFKParentDirty records that a table's PARENT rows changed (UPDATE or
// DELETE, never INSERT): its children's FK references must be re-validated at
// COMMIT / statement end because a parent key may have disappeared. INSERTing
// a parent row cannot invalidate child references, so children are only
// scanned for parent-dirty tables (SQLite only checks a parent's children on
// UPDATE/DELETE of parent rows).
func (c *ConstraintEnforcer) markFKParentDirty(entry *schema.Entry, ctx *DatabaseContext) {
	if !c.ctx.ForeignKeys() || entry == nil {
		return
	}
	if c.fkParentDirty == nil {
		c.fkParentDirty = make(map[fkDirtyKey]bool)
	}
	c.fkParentDirty[fkDirtyKey{ctx: ctx, name: entry.Name}] = true
}

// resetFKDirty clears the dirty-table set (at BEGIN, COMMIT, ROLLBACK, and
// after a statement-end check).
func (c *ConstraintEnforcer) resetFKDirty() {
	c.fkDirty = nil
	c.fkParentDirty = nil
}

// checkDeferredFK re-validates the FK relationships of every table modified in
// the current transaction/statement and returns "FOREIGN KEY constraint failed"
// when any violation exists. It is called at COMMIT (and at statement end in
// autocommit mode) for deferred constraints and when PRAGMA
// defer_foreign_keys is ON. Only tables whose rows changed (or whose children
// reference a changed parent) are checked, mirroring SQLite's incremental
// deferred-FK counters — pre-existing violations in unrelated tables do not
// fail a COMMIT. When onlyImmediate is true (a statement-end check inside an
// open transaction), constraints deferred to COMMIT (DEFERRABLE INITIALLY
// DEFERRED, or immediate while PRAGMA defer_foreign_keys is ON) are skipped.
func (c *ConstraintEnforcer) checkDeferredFK(onlyImmediate bool) error {
	if len(c.fkDirty) == 0 {
		return nil
	}
	var viols []FKViolation
	for key := range c.fkDirty {
		entry, err := key.ctx.Schema.FindTable(key.name)
		if err != nil {
			// The table was dropped during the transaction; its FKs are gone.
			continue
		}
		// The table's own FK constraints (as a child).
		v, err := c.fkCheckChildTable(entry, key.ctx, onlyImmediate)
		if err != nil {
			return err
		}
		viols = append(viols, v...)
		// Children of this table (as a parent): a parent row deleted/updated
		// may leave children referencing a missing key. Only UPDATE/DELETE of
		// parent rows can do this; a pure INSERT into the parent cannot
		// invalidate child references and must not fail a statement because a
		// child has a (pre-existing) FK mismatch (alterlegacy-8.2).
		if !c.fkParentDirty[key] {
			continue
		}
		for _, ref := range c.ChildRefs(entry, key.ctx) {
			childEntry, cerr := ref.ChildCtx.Schema.FindTable(ref.ChildTable)
			if cerr != nil {
				continue
			}
			cv, cerr := c.fkCheckChildTable(childEntry, ref.ChildCtx, onlyImmediate)
			if cerr != nil {
				// A child whose FK cannot be resolved (foreign key mismatch)
				// is tolerated when scanning a PARENT's children: SQLite only
				// reports the mismatch when the child row is written or on
				// PRAGMA foreign_key_check, never while checking a parent
				// DELETE/UPDATE (the child's stale FK cannot orphan rows).
				continue
			}
			viols = append(viols, cv...)
		}
	}
	if len(viols) > 0 {
		return fmt.Errorf("FOREIGN KEY constraint failed")
	}
	return nil
}

// fkCheckReplaceChildren verifies that the children of a table replaced by
// INSERT OR REPLACE still reference an existing parent key after the new row
// is written (SQLite checks the implicit delete's NO ACTION constraint at
// statement end, not at COMMIT).
func (c *ConstraintEnforcer) fkCheckReplaceChildren(parentEntry *schema.Entry, parentCtx *DatabaseContext) *Result {
	for _, ref := range c.ChildRefs(parentEntry, parentCtx) {
		childEntry, err := ref.ChildCtx.Schema.FindTable(ref.ChildTable)
		if err != nil {
			continue
		}
		viols, err := c.fkCheckChildTable(childEntry, ref.ChildCtx, false)
		if err != nil {
			return &Result{Error: err}
		}
		if len(viols) > 0 {
			return &Result{Error: fmt.Errorf("FOREIGN KEY constraint failed")}
		}
	}
	return &Result{}
}

// checkForeignKeyViolations verifies that every non-NULL column value with a
// FOREIGN KEY clause references an existing parent row. It is only enforced
// when PRAGMA foreign_keys is ON. Returns an error describing the first
// violation. excludeRowID is the rowid of the row being updated (for
// self-referential FKs the row's OLD key value would otherwise falsely
// satisfy the parent lookup); pass 0 for INSERT.

// fkSelfReferentialOK reports whether a row's own parent-key columns satisfy
// its self-referential FK constraint (the row is its own parent).
func fkSelfReferentialOK(values []interface{}, fk FKConstraint, childIndex, parentIndex map[string]int, parentCols []string) bool {
	for i := range fk.ChildCols {
		cidx, _ := colIndexLookup(childIndex, fk.ChildCols[i])
		pidx, _ := colIndexLookup(parentIndex, parentCols[i])
		if cidx < 0 || cidx >= len(values) || pidx < 0 || pidx >= len(values) ||
			values[cidx] == nil || values[pidx] == nil ||
			util.CompareValues(values[cidx], values[pidx]) != 0 {
			return false
		}
	}
	return true
}

// fkParentRowExists scans the parent table for a row whose key columns match
// the child key values. The excludeRowID skip applies only to self-referential
// FKs (child == parent table).
