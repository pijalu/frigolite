// Package exec implements query execution.
package execdml

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// --- UPDATE ---

type updateChange struct {
	rowID int64
	// seq is the change's 0-based position in the scan order (collectUpdateChanges
	// assigns it): a stable identity for LIMIT-window survivor sets, unlike
	// rowID which is synthetic 0 for every WITHOUT ROWID row.
	seq       int
	newRowID  *int64 // non-nil when the UPDATE sets rowid/_rowid_/oid to a new value
	values    []interface{}
	oldValues []interface{}
	// rowMap is the original row (as a RowMap) used to re-evaluate the SET
	// expressions per-row. It is only populated when SET evaluation is
	// deferred (the trigger-per-row path), so the changes() counter and user
	// functions observe SQLite's row-by-row interleaving (e_changes 5.1.2).
	rowMap RowMap
}

// schemaFingerprint is the DDL-invalidation token for per-executor caches:
// the main schema manager's cookie/epoch fingerprint (same token the
// index-def cache validates against). Memoized per DML statement (the fpSeq
// statement counter): the point-op paths read it from every memo guard —
// column index, column lookup, row plan, index defs — and each read paid a
// pager-header lock; the schema cannot change mid-statement (no DDL inside
// DML), so one read per statement serves the rest.
func (e *DMLExecutor) schemaFingerprint() uint64 {
	if e.fpSeq == e.dmlStmtSeq {
		return e.fpCache
	}
	var fp uint64
	if sm := e.ctx.Schema(); sm != nil {
		fp = sm.SchemaFingerprint()
	}
	e.fpSeq, e.fpCache = e.dmlStmtSeq, fp
	return fp
}

// bumpDMLStmtSeq advances the per-statement memo epoch (called at every
// DML statement-family entry, trigger bodies included).
func (e *DMLExecutor) bumpDMLStmtSeq() {
	e.dmlStmtSeq++
}

// columnIndexFor returns the column index for colDefs, memoized per DML
// executor and guarded by the schema fingerprint (DDL invalidates it). The
// hot paths build this identical map several times per statement.
func (e *DMLExecutor) columnIndexFor(colDefs []sql.ColumnDef) map[string]int {
	if len(colDefs) == 0 {
		return buildColumnIndex(colDefs)
	}
	fp := e.schemaFingerprint()
	if e.ciCache != nil && e.ciFingerprint == fp && e.ciDefs == &colDefs[0] && e.ciLen == len(colDefs) {
		return e.ciCache
	}
	m := buildColumnIndex(colDefs)
	e.ciFingerprint, e.ciDefs, e.ciLen, e.ciCache = fp, &colDefs[0], len(colDefs), m
	return m
}

func buildColumnIndex(colDefs []sql.ColumnDef) map[string]int {
	colIndex := make(map[string]int)
	for i, cd := range colDefs {
		// SQLite column names are case-insensitive: SET Test=... and
		// SET test=... must resolve to the same column. Key the index by
		// the lowercased name (rowvalue 27.10's UPDATE items SET
		// test='ok' where the column is declared Test).
		colIndex[strings.ToLower(cd.Name)] = i
	}
	// rowid/_rowid_/oid map to the pseudo-rowid unless the table declares a
	// column with one of those names (which shadows the alias).
	if !execquery.RowHasRowIDColumn(colDefs) {
		colIndex["rowid"] = -1
	}
	return colIndex
}

// cdIndex returns the column index for name, or -1 if not found.
func cdIndex(colDefs []sql.ColumnDef, name string) int {
	for i, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return i
		}
	}
	return -1
}

// originalColumnName returns the column name as spelled in the CREATE TABLE
// SQL (preserving original case for error messages), or the uppercased name
// if it cannot be found.
func (e *DMLExecutor) originalColumnName(createSQL, colName string) string {
	start := strings.IndexByte(createSQL, '(')
	end := strings.LastIndexByte(createSQL, ')')
	if start < 0 || end <= start {
		return colName
	}
	body := createSQL[start+1 : end]
	for _, part := range splitColumnDefs(body) {
		fields := strings.FieldsFunc(part, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
		if len(fields) == 0 {
			continue
		}
		first := strings.Trim(fields[0], "`\"[]")
		if strings.EqualFold(first, colName) {
			return first
		}
	}
	return colName
}

// splitColumnDefs splits a CREATE TABLE column list on top-level commas
// (ignoring commas inside parentheses such as CHECK(...) or DEFAULT(...)).
func splitColumnDefs(body string) []string {
	var parts []string
	depth := 0
	last := 0
	for i, r := range body {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, body[last:])
	return parts
}

// primaryKeyColIndices returns the set of column indices that are PRIMARY KEY
// columns: column-level PRIMARY KEY declarations plus table-level PRIMARY KEY
// constraints (honoring integer column positions).
func (e *DMLExecutor) primaryKeyColIndices(tableName, createSQL string, colDefs []sql.ColumnDef) map[int]bool {
	idx := make(map[int]bool)
	colIndex := e.columnIndexFor(colDefs)
	for i, cd := range colDefs {
		if cd.PrimaryKey {
			idx[i] = true
		}
	}
	for _, tc := range e.ctx.TableConstraints(tableName, createSQL) {
		if tc.Type != sql.ConstraintPrimaryKey {
			continue
		}
		for _, ic := range tc.Columns {
			if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
				idx[n-1] = true
				continue
			}
			if i, ok := colIndex[strings.ToLower(ic.Name)]; ok {
				idx[i] = true
			}
		}
	}
	return idx
}

// sortUpdateChanges sorts updateChange entries by the ORDER BY expressions
// evaluated against each row's original values.
func (e *DMLExecutor) sortUpdateChanges(changes []updateChange, rowMaps []Row, orderBy []sql.OrderByTerm) {
	if len(changes) <= 1 {
		return
	}
	type pair struct {
		ch  updateChange
		row Row
	}
	pairs := make([]pair, len(changes))
	for i := range changes {
		pairs[i] = pair{ch: changes[i], row: rowMaps[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		for _, ob := range orderBy {
			left, _ := e.ctx.EvalExpr(ob.Expr, pairs[i].row)
			right, _ := e.ctx.EvalExpr(ob.Expr, pairs[j].row)
			cmp := e.ctx.CompareOrderByValues(left, right, ob)
			if cmp < 0 {
				return true
			} else if cmp > 0 {
				return false
			}
		}
		return false
	})
	for i := range pairs {
		changes[i] = pairs[i].ch
	}
}

// limitUpdateChanges applies UPDATE ... LIMIT n [OFFSET m] to the change list,
// keeping the first n entries after skipping m (SQLite semantics for UPDATE
// LIMIT: the first N rows matched by the scan order are updated).
func (e *DMLExecutor) limitUpdateChanges(changes []updateChange, s *sql.UpdateStmt) []updateChange {
	limit, err := e.evalConstInt(s.Limit)
	if err != nil || limit < 0 {
		return changes
	}
	offset := int64(0)
	if s.Offset != nil {
		if v, err := e.evalConstInt(s.Offset); err == nil && v > 0 {
			offset = v
		}
	}
	if offset >= int64(len(changes)) {
		return nil
	}
	end := offset + limit
	if end > int64(len(changes)) {
		end = int64(len(changes))
	}
	return changes[offset:end]
}

// evalConstInt evaluates an expression that must be a constant integer,
// returning -1 on error or non-integer values.
func (e *DMLExecutor) evalConstInt(expr sql.Expr) (int64, error) {
	v, err := e.ctx.EvalExpr(expr, nil)
	if err != nil {
		return -1, err
	}
	v = util.UnwrapColumnValue(v)
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		// SQLite accepts integer-valued floats (LIMIT 1.0 == LIMIT 1) but
		// rejects non-integral floats (LIMIT 1.2 → datatype mismatch).
		if n == math.Trunc(n) {
			return int64(n), nil
		}
	case string:
		// SQLite casts the LIMIT expression to integer: LIMIT '4' == LIMIT 4,
		// LIMIT '1.0' == LIMIT 1, LIMIT '1.2' and LIMIT 'abc' → datatype
		// mismatch. Only integral-valued strings are accepted.
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil && f == math.Trunc(f) {
			return int64(f), nil
		}
	}
	return -1, fmt.Errorf("datatype mismatch")
}

// dedupeUpdateChanges removes duplicate-rowID entries from an update
// change list, keeping the LAST entry per rowid (sequential-overwrite
// semantics). A change scan over a table holding a physical duplicate
// rowid (corrupt or legacy file) reports the row twice; re-inserting it
// twice would recreate the duplicate.
func dedupeUpdateChanges(changes []updateChange) []updateChange {
	if len(changes) < 2 {
		return changes
	}
	// WITHOUT ROWID rows share synthetic RowID 0: dedupe by rowid would
	// collapse every change into one. Only dedupe nonzero rowids.
	allZero := true
	for _, c := range changes {
		if c.rowID != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return changes
	}
	last := make(map[int64]int, len(changes))
	for i, c := range changes {
		last[c.rowID] = i
	}
	out := make([]updateChange, 0, len(last))
	seen := make(map[int64]bool, len(last))
	for i, c := range changes {
		if last[c.rowID] == i && !seen[c.rowID] {
			out = append(out, c)
			seen[c.rowID] = true
		}
	}
	return out
}

func (e *DMLExecutor) rowMatchesWhere(where sql.Expr, row Row) (bool, error) {
	if where == nil {
		return true, nil
	}
	match, err := e.ctx.EvalBool(where, row)
	if err != nil {
		return false, err
	}
	return match, nil
}

// deleteUpdateIndexEntries removes the OLD row's entries from every index an
// UPDATE change touches (the delete phase; call BEFORE the row's table cell
// is removed). SQLite maintains an index only when the statement assigns one
// of its key columns, a column its expression keys or partial predicate
// references, or the rowid (update.c UXF); the same touch rule applies here
// via indexTouchesChangedCols, with a rowid re-key touching every index.
func (e *DMLExecutor) deleteUpdateIndexEntries(tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, writeRowID int64) error {
	return e.deleteUpdateIndexEntriesFor(tableEntry, colDefs, []updateChange{c})
}

// deleteUpdateIndexEntriesFor removes the OLD row entries of every change
// from the indexes the change touches, batching the per-index cell deletions
// into ONE btree walk per index (a statement's delete cost is O(index), not
// O(changes x index). The trigger paths evaluate SET per row after the scan,
// so old/new values arrive per change.
func (e *DMLExecutor) deleteUpdateIndexEntriesFor(tableEntry *schema.Entry, colDefs []sql.ColumnDef, changes []updateChange) error {
	if len(changes) == 0 {
		return nil
	}
	// A table with no indexes maintains nothing: skip the batch maps the
	// collect loop below builds (every point UPDATE/DELETE on an unindexed
	// table paid two maps per statement for nothing).
	if len(e.allTableIndexes(tableEntry.Name)) == 0 {
		return nil
	}
	// Union of touched indexes across changes, per index name (a rowid re-key
	// touches every index; see maintainedUpdateIndexes).
	defsByName := make(map[string]indexDef)
	changeTargets := make(map[string]map[int64][]interface{}, len(changes)) // defName -> rowid -> key values
	colIndex := e.columnIndexFor(colDefs)
	for _, c := range changes {
		if err := e.collectUpdateIndexDeleteTarget(tableEntry, colDefs, colIndex, c, defsByName, changeTargets); err != nil {
			return err
		}
	}
	for name, targets := range changeTargets {
		def := defsByName[name]
		if err := e.deleteIndexCellsBatch(def, targets); err != nil {
			return err
		}
	}
	return nil
}

// indexDefNeedsRowMaps reports whether one index definition's maintenance
// needs a name-keyed row map: a partial index's WHERE predicate evaluates
// against the row, and an expression/qualified key (or a rowid alias, which
// maps to the negative pseudo-slot) evaluates its expression against the
// row. Plain declared-column keys read the positional values directly.
func indexDefNeedsRowMaps(def indexDef, colIndex map[string]int) bool {
	if def.Where != "" {
		return true
	}
	for _, cn := range def.Cols {
		if strings.ContainsAny(cn, "(.") {
			return true
		}
		if idx, ok := colIndex[strings.ToLower(cn)]; !ok || idx < 0 {
			return true
		}
	}
	return false
}

// collectUpdateIndexDeleteTarget adds one change's OLD row entry to the
// delete targets of every index the change touches (registered in defsByName
// so the batched deletions below can resolve the index definitions). The OLD
// row's name-keyed map is built lazily — only when a touched definition's
// predicate or keys evaluate against a row.
func (e *DMLExecutor) collectUpdateIndexDeleteTarget(tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int, c updateChange, defsByName map[string]indexDef, changeTargets map[string]map[int64][]interface{}) error {
	defs, _ := e.maintainedUpdateIndexes(tableEntry, colDefs, c, updateWriteRowID(c))
	if len(defs) == 0 {
		return nil
	}
	var oldRow RowMap
	for _, def := range defs {
		defsByName[def.Name] = def
		if indexDefNeedsRowMaps(def, colIndex) && oldRow == nil {
			oldRow = buildRowMapFromValues(c.oldValues, colDefs, c.rowID)
		}
		if inIndex, werr := e.indexRowIncluded(def, oldRow); werr != nil {
			return werr
		} else if !inIndex {
			continue
		}
		// The raw values with the rowid-alias substitution — exactly what
		// rowMapColumnValues extracted from the name-keyed map.
		oldValues := ipkRowidSubstituted(c.oldValues, colDefs, c.rowID)
		indexValues, kerr := e.indexKeyValuesForRow(def, colDefs, colIndex, oldValues, oldRow)
		if kerr != nil {
			return kerr
		}
		targets := changeTargets[def.Name]
		if targets == nil {
			targets = make(map[int64][]interface{})
			changeTargets[def.Name] = targets
		}
		targets[c.rowID] = append(indexValues, c.rowID)
	}
	return nil
}

// writeUpdateIndexEntries writes the NEW row's entries to every index an
// UPDATE change touches (the insert phase; call AFTER the re-inserted cell is
// visible).
func (e *DMLExecutor) writeUpdateIndexEntries(tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, writeRowID int64) error {
	return e.writeUpdateIndexEntriesFor(tableEntry, colDefs, c.oldValues, c.rowID, c.values, writeRowID)
}

// writeUpdateIndexEntriesFor is writeUpdateIndexEntries with explicit old/new
// values (writeUpdateCell's trigger path passes the post-trigger values).
func (e *DMLExecutor) writeUpdateIndexEntriesFor(tableEntry *schema.Entry, colDefs []sql.ColumnDef, oldValues []interface{}, oldRowID int64, newValues []interface{}, writeRowID int64) error {
	c := updateChange{rowID: oldRowID, oldValues: oldValues, values: newValues}
	defs, colIndex := e.maintainedUpdateIndexes(tableEntry, colDefs, c, writeRowID)
	if len(defs) == 0 {
		return nil
	}
	var newRow RowMap
	for _, def := range defs {
		if indexDefNeedsRowMaps(def, colIndex) && newRow == nil {
			newRow = buildRowMapFromValues(newValues, colDefs, writeRowID)
		}
		if inIndex, werr := e.indexRowIncluded(def, newRow); werr != nil {
			return werr
		} else if !inIndex {
			continue
		}
		subValues := ipkRowidSubstituted(newValues, colDefs, writeRowID)
		indexValues, kerr := e.indexKeyValuesForRow(def, colDefs, colIndex, subValues, newRow)
		if kerr != nil {
			return kerr
		}
		if err := e.writeIndexCell(def, colDefs, append(indexValues, writeRowID)); err != nil {
			return err
		}
	}
	return nil
}

// maintainedUpdateIndexes returns the indexes a change touches plus the
// resolved column index; an empty result skips both phases.
func (e *DMLExecutor) maintainedUpdateIndexes(tableEntry *schema.Entry, colDefs []sql.ColumnDef, c updateChange, writeRowID int64) ([]indexDef, map[string]int) {
	defs := e.allTableIndexes(tableEntry.Name)
	if len(defs) == 0 {
		return nil, nil
	}
	changed := make(map[string]bool, len(colDefs))
	for i := range c.oldValues {
		if i >= len(c.values) || i >= len(colDefs) {
			continue
		}
		if !valuesIdenticalForIndex(c.oldValues[i], c.values[i]) {
			changed[strings.ToLower(colDefs[i].Name)] = true
		}
	}
	rowidChanged := writeRowID != c.rowID
	var maintained []indexDef
	for _, def := range defs {
		if rowidChanged || indexTouchesChangedCols(def, colDefs, changed) {
			maintained = append(maintained, def)
		}
	}
	if len(maintained) == 0 {
		return nil, nil
	}
	return maintained, e.columnIndexFor(colDefs)
}

// valuesIdenticalForIndex reports whether a column's old and new values are
// the same index key value (type-aware: an int64->float64 rewrite changes the
// stored key encoding even when the values compare equal).
func valuesIdenticalForIndex(a, b interface{}) bool {
	au, bu := util.UnwrapColumnValue(a), util.UnwrapColumnValue(b)
	if au == nil && bu == nil {
		return true
	}
	if au == nil || bu == nil {
		return false
	}
	if util.CompareValues(au, bu) != 0 {
		return false
	}
	af, aIsFloat := au.(float64)
	bf, bIsFloat := bu.(float64)
	if aIsFloat != bIsFloat {
		return false
	}
	if aIsFloat && (af == float64(int64(af))) != (bf == float64(int64(bf))) {
		return false
	}
	return true
}

func (e *DMLExecutor) applyUpdateChanges(tableName string, rootPage uint32, changes []updateChange) *Result {
	if len(changes) == 0 {
		return &Result{}
	}
	// A pre-existing physical duplicate rowid (corrupt/legacy file) makes the
	// scan collect the row twice; SQLite updates rows one at a time in place,
	// so write each distinct rowid exactly once — last write wins.
	changes = dedupeUpdateChanges(changes)

	// Build a set of rowIDs to update
	toUpdate := rowidsToUpdate(changes)

	// WITHOUT ROWID tables live in an index btree: rows are addressed by
	// PK key (cell.RowID is a synthetic 0 shared by every row), so match
	// the OLD PK payloads instead of rowids. Snapshot each change's old
	// PK key (declared order) for the delete predicate below.
	wrOldKeys, wrEntry := e.wrSnapshotOldKeys(tableName, changes)

	if res := e.deleteUpdateOldIndexEntries(tableName, changes); res != nil {
		return res
	}

	tree := e.updateApplyTree(tableName, rootPage, wrEntry)

	// Step 0: in-place same-size overwrites (sqlite3BtreeInsert's loc==0
	// fast path, src/btree.c:9596-9614). A same-rowid change whose new cell
	// is the same byte size as the stored one overwrites the cell bytes at
	// the SAME offset: the cell pointer array, content start and page layout
	// stay byte-identical and no dropCell/insertCell/balance runs. Handled
	// changes are excluded from the bulk delete/reinsert below — its page
	// compaction rewrites the pointer array, which is externally observable
	// through corruption detection (corrupt.test 7.2/7.3 craft a cell
	// pointer at a record body, rewrite that body with a same-size UPDATE,
	// and expect the NEXT statement's page initialization to detect it).
	inPlace := map[int64]bool{}
	if wrEntry == nil {
		var res *Result
		if inPlace, res = e.applyInPlaceUpdates(tableName, tree, rootPage, changes); res != nil {
			return res
		}
	}

	// Step 1: Delete all existing rows in a single pass. When every change
	// was written by the Step-0 in-place pass, the predicate excludes all
	// their rowids and can match nothing — skip the O(table) sweep entirely.
	if len(inPlace) != len(toUpdate) {
		_, delErr := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
			if wrEntry != nil {
				return wrCellMatchesOldKey(cell, wrOldKeys, wrEntry, e.ctx)
			}
			return toUpdate[cell.RowID] && !inPlace[cell.RowID]
		})
		if delErr != nil {
			return &Result{Error: delErr}
		}
		e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)
	}

	// Step 2: Insert all new rows, firing the preupdate hook per row.
	if res := e.reinsertUpdatedRows(tableName, tree, rootPage, changes, inPlace, wrEntry); res != nil {
		return res
	}

	// The re-insert loop above bumps the rowid cache with each re-inserted
	// row's rowid. When the UPDATE touched only a subset of rows (e.g. an FTS
	// segment truncate), that leaves the cached max LOWER than the table's
	// real maximum (a concurrent insert's higher rowid is forgotten), so the
	// next auto-rowid insert COLLIDES and overwrites the missing row
	// (fts4merge 5.7: the L2 output insert at rowid 1067 is clobbered by the
	// next level-0 segment because the truncate's re-insert bumped the cache
	// only to 1066). SQLite recomputes the rowid counter after any
	// DELETE/UPDATE, so drop the cache again and let the next allocation
	// rescan the true max.
	e.ctx.InvalidateRowIDCache(e.dmlPager(tableName), rootPage)

	return &Result{Changes: int64(len(changes))}
}

// rowidsToUpdate builds the set of rowids the changes rewrite.
func rowidsToUpdate(changes []updateChange) map[int64]bool {
	toUpdate := make(map[int64]bool, len(changes))
	for _, c := range changes {
		toUpdate[c.rowID] = true
	}
	return toUpdate
}

// reinsertUpdatedRows writes every non-in-place change back to the tree and
// fires its preupdate hook (applyUpdateChanges Step 2). Changes already
// written by the Step-0 in-place pass (preupdate hook included there) are
// skipped.
func (e *DMLExecutor) reinsertUpdatedRows(tableName string, tree *btree.BTree, rootPage uint32, changes []updateChange, inPlace map[int64]bool, wrEntry *schema.Entry) *Result {
	for _, c := range changes {
		if inPlace[c.rowID] {
			continue
		}
		if err := e.writeUpdatedCellWR(tableName, tree, rootPage, c, wrEntry); err != nil {
			return &Result{Error: err}
		}
		if res := e.fireUpdatePreupdate(tableName, c); res != nil {
			return res
		}
	}
	return nil
}

// deleteUpdateOldIndexEntries removes every change's OLD entries from the
// indexes the UPDATE touches (update.c maintains indexes over assigned
// columns; see deleteUpdateIndexEntries). The NEW entries are written per
// row by writeUpdatedCellWR below.
func (e *DMLExecutor) deleteUpdateOldIndexEntries(tableName string, changes []updateChange) *Result {
	var idxColDefs []sql.ColumnDef
	var idxTableEntry *schema.Entry
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil && te != nil {
		idxTableEntry = te
		idxColDefs = e.ctx.ParseColumnDefs(te.Name, te.SQL)
	}
	if idxTableEntry == nil {
		return nil
	}
	if err := e.deleteUpdateIndexEntriesFor(idxTableEntry, idxColDefs, changes); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// updateApplyTree builds the delete/re-insert tree for a bulk UPDATE: the WR
// storage tree (index btree + PK-aware comparator) for WITHOUT ROWID tables
// so the re-insert keeps PK ordering, the plain table btree otherwise.
func (e *DMLExecutor) updateApplyTree(tableName string, rootPage uint32, wrEntry *schema.Entry) *btree.BTree {
	if wrEntry == nil {
		return e.dmlTableBTree(tableName, rootPage)
	}
	return e.wrTableBTree(e.dmlPager(tableName), wrEntry, e.ctx.ParseColumnDefs(tableName, wrEntry.SQL))
}

// wrSnapshotOldKeys snapshots each change's OLD PK key (declared order) for
// a WITHOUT ROWID table's delete phase; wrEntry is nil for rowid tables.
func (e *DMLExecutor) wrSnapshotOldKeys(tableName string, changes []updateChange) ([][]interface{}, *schema.Entry) {
	te, _, ferr := e.ctx.FindTable(tableName)
	if ferr != nil || te == nil || !tableIsWithoutRowid(te.SQL) {
		return nil, nil
	}
	wrColDefs := e.ctx.ParseColumnDefs(tableName, te.SQL)
	idx := WRPKIndices(te.SQL, wrColDefs)
	if len(idx) == 0 {
		return nil, te
	}
	keys := make([][]interface{}, 0, len(changes))
	for _, c := range changes {
		keys = append(keys, wrPkKeyFromDeclared(c.oldValues, idx))
	}
	return keys, te
}

// applyInPlaceUpdates attempts the Step-0 in-place same-size overwrite for
// every same-rowid change (rowid tables only; sqlite3BtreeInsert's loc==0
// fast path, src/btree.c:9596-9614). Returns the set of rowids written in
// place — they are excluded from the bulk delete/reinsert pass.
func (e *DMLExecutor) applyInPlaceUpdates(tableName string, tree *btree.BTree, rootPage uint32, changes []updateChange) (map[int64]bool, *Result) {
	inPlace := make(map[int64]bool)
	for _, c := range changes {
		if updateWriteRowID(c) != c.rowID {
			continue
		}
		handled, res := e.tryUpdateCellInPlaceBulk(tableName, tree, rootPage, c)
		if res != nil {
			return nil, res
		}
		if handled {
			inPlace[c.rowID] = true
		}
	}
	return inPlace, nil
}

// tryUpdateCellInPlaceBulk applies the in-place same-size cell overwrite
// (btree.BTree.OverwriteCellByRowID — sqlite3BtreeInsert's loc==0 fast path,
// src/btree.c:9596-9614) to one rowid-table change that keeps its rowid.
// handled=true means the cell was rewritten at its original offset (or the
// attempt failed with res) and the change must skip the bulk delete/reinsert
// pass; handled=false means a guard declined (row not found, different cell
// size, non-local payload) and the change rejoins the normal path. The NEW
// index entries, rowid-cache bump and preupdate hook fire here exactly as
// writeUpdatedCellWR does after the reinsert.
func (e *DMLExecutor) tryUpdateCellInPlaceBulk(tableName string, tree *btree.BTree, rootPage uint32, c updateChange) (bool, *Result) {
	newRecord, err := storage.EncodeRecord(c.values)
	if err != nil {
		return false, &Result{Error: err}
	}
	cellData := storage.EncodeCell(&storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   c.rowID,
		Payload: newRecord,
	})
	done, oerr := tree.OverwriteCellByRowID(c.rowID, cellData)
	if oerr != nil {
		return false, &Result{Error: oerr}
	}
	if !done {
		return false, nil
	}
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil && te != nil {
		if err := e.writeUpdateIndexEntries(te, e.ctx.ParseColumnDefs(te.Name, te.SQL), c, c.rowID); err != nil {
			return false, &Result{Error: err}
		}
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableName), rootPage, c.rowID)
	if res := e.fireUpdatePreupdate(tableName, c); res != nil {
		return false, res
	}
	return true, nil
}

// writeUpdatedCellWR re-inserts one updated row; for WITHOUT ROWID tables
// (wrEntry != nil) values are reordered PK-first into an index-leaf cell,
// otherwise the legacy table-leaf path is used.
func (e *DMLExecutor) writeUpdatedCellWR(tableName string, tree *btree.BTree, rootPage uint32, c updateChange, wrEntry *schema.Entry) error {
	vals := c.values
	cellType := storage.CellTableLeaf
	if wrEntry != nil {
		colDefs := e.ctx.ParseColumnDefs(tableName, wrEntry.SQL)
		vals = ReorderToStorage(c.values, WithoutRowidStorageOrder(wrEntry.SQL, colDefs))
		cellType = storage.CellIndexLeaf
	}
	newRecord, err := storage.EncodeRecord(vals)
	if err != nil {
		return err
	}
	writeRowID := c.rowID
	if c.newRowID != nil {
		writeRowID = *c.newRowID
	}
	newCell := &storage.Cell{
		Type:    cellType,
		RowID:   writeRowID,
		Payload: newRecord,
	}
	if err := tree.InsertCell(newCell); err != nil {
		return err
	}
	e.ctx.BumpRowIDCache(e.dmlPager(tableName), rootPage, writeRowID)
	// Write the NEW row's entries into every index the change touches (the
	// insert phase; the OLD entries were removed by deleteUpdateIndexEntries
	// before the table cell delete).
	if te, _, terr := e.ctx.FindTable(tableName); terr == nil && te != nil {
		if err := e.writeUpdateIndexEntries(te, e.ctx.ParseColumnDefs(te.Name, te.SQL), c, writeRowID); err != nil {
			return err
		}
	}
	return nil
}

// fireUpdatePreupdate fires the preupdate hook with the old and new row
// values for one updated row.
func (e *DMLExecutor) fireUpdatePreupdate(tableName string, c updateChange) *Result {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil {
		return nil
	}
	return e.fireUpdatePreupdateEntry(entry, c)
}

// fireUpdatePreupdateEntry is fireUpdatePreupdate with the target table's
// schema entry already in hand — the per-row FindTable (applyPreupdateAffinity
// re-resolves the table again inside the engine) stays out of the point
// write path. A no-op when no hook consumes the event (PreupdateNeeded):
// the per-row Old/New copies stay off the un-hooked UPDATE floor.
func (e *DMLExecutor) fireUpdatePreupdateEntry(entry *schema.Entry, c updateChange) *Result {
	if !e.ctx.PreupdateNeeded() {
		return nil
	}
	rowidTable := !tableIsWithoutRowid(entry.SQL)
	rowID := c.rowID
	if !rowidTable {
		rowID = 0
	}
	return e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "UPDATE",
		DB:    e.schemaNameForPager(e.dmlPager(entry.Name)),
		Table: entry.Name,
		RowID: rowID, RowID2: rowID,
		RowidTable: rowidTable,
		Old:        append([]interface{}(nil), c.oldValues...),
		New:        append([]interface{}(nil), c.values...),
	})
}

// fireConflictDeletePreupdate fires the preupdate DELETE hook for one
// conflict-resolution deleted row (REPLACE / OR REPLACE), suppressing the
// update hook (NoUpdateHook, replace.c disposal deletes). It is distinct
// from delete.go's fireDeletePreupdate, which serves the DELETE statement
// pipeline (RowMap-based, update hook NOT suppressed). WITHOUT ROWID tables
// report rowid 0 (SQLite uses the key columns instead).
func (e *DMLExecutor) fireConflictDeletePreupdate(tableEntry *schema.Entry, rowID int64, oldValues []interface{}) *Result {
	if !e.ctx.PreupdateNeeded() {
		return nil
	}
	tableName := tableEntry.Name
	wr := tableIsWithoutRowid(tableEntry.SQL)
	delRowID := rowID
	if wr {
		delRowID = 0
	}
	return e.ctx.FirePreupdate(PreupdateEvent{
		Type:  "DELETE",
		DB:    e.schemaNameForPager(e.dmlPager(tableName)),
		Table: tableName,
		RowID: delRowID, RowID2: delRowID,
		RowidTable:   !wr,
		NoUpdateHook: true,
		Old:          oldValues,
		New:          nil,
	})
}

// applyUpdateWithTriggers processes a plain UPDATE with triggers, matching
// SQLite's ordering: BEFORE UPDATE triggers fire per-row before the row is
// written, and a BEFORE trigger may delete the row being updated — SQLite then
// skips writing rows that no longer exist and checks UNIQUE/PK constraints
// against the live table state. AFTER UPDATE triggers fire phase-based after
// the writes (matching the engine's existing behavior for non-trigger rows).
// tableBTreeForDML builds the btree for a table being modified, using the
// modified table's context pager when known (a table in an ATTACHed database
// lives on the attached pager even when a same-named table exists in main).
func (e *DMLExecutor) tableBTreeForDML(tableEntry *schema.Entry, rootPage uint32) *btree.BTree {
	return e.dmlTableBTree(tableEntry.Name, rootPage)
}

// rowExists reports whether a table contains a cell with the given rowID.
func (e *DMLExecutor) rowExists(tableName string, rootPage uint32, rowID int64) (bool, error) {
	// The table may share its short name with a table in another schema
	// (main.t1 vs aux.t1); use the modified table's context pager when known
	// AND the name carries a schema prefix (an unqualified name resolves
	// temp/main-first, which the fallback handles correctly).
	var pg *pager.Pager
	if e.currentDMLCtx != nil && e.currentDMLCtx.Pager != nil && e.currentDMLCtx != e.ctx.MainDB() {
		pg = e.currentDMLCtx.Pager
	} else {
		pg = e.dmlPager(tableName)
	}
	tree := e.ctx.TableBTreePg(pg, tableName, rootPage, true)
	defer tree.Close() // scan tree is function-local
	cursor, err := tree.OpenCursor()
	if err != nil {
		return false, err
	}
	// Direct b-tree descent (sqlite3BtreeMoveto shape); a from-start scan
	// made re-key-heavy UPDATE statements quadratic in the table size.
	found, serr := cursor.SeekToRowID(rowID)
	if serr != nil {
		return false, nil
	}
	return found, nil
}

// updateRowConflicts reports whether an existing row's values conflict with a
// change's new values on any UNIQUE/PRIMARY KEY column or UNIQUE index.
func updateRowConflicts(e *DMLExecutor, rowValues, newValues []interface{}, colDefs []sql.ColumnDef, colIndex map[string]int, uniqueCols []int, idxColsList []uniqueIndexDef, rowID, newRowID int64) bool {
	if uniqueColsMatch(rowValues, newValues, colDefs, rowID, newRowID, uniqueCols) {
		return true
	}
	return indexDefsMatch(e, rowValues, newValues, colDefs, colIndex, idxColsList, rowID, newRowID)
}

// uniqueColsMatch reports whether two value sets agree on any UNIQUE/PRIMARY
// KEY column index. colDefs and rowids perform the INTEGER PRIMARY KEY
// rowid-alias substitution (a stored NULL becomes the rowid before comparison).
func uniqueColsMatch(a, b []interface{}, colDefs []sql.ColumnDef, rowIDa, rowIDb int64, uniqueCols []int) bool {
	for _, idx := range uniqueCols {
		if uniqueColValuesMatch(a, b, colDefs, rowIDa, rowIDb, idx) {
			return true
		}
	}
	return false
}

// uniqueColValuesMatch reports whether two value sets agree on one indexed
// column (rowid-alias substitution applied; NULL never matches).
func uniqueColValuesMatch(a, b []interface{}, colDefs []sql.ColumnDef, rowIDa, rowIDb int64, idx int) bool {
	if idx >= len(a) || idx >= len(b) || idx >= len(colDefs) {
		return false
	}
	av, bv := a[idx], b[idx]
	if isIPKRowidAliasCol(colDefs[idx]) {
		if av == nil {
			if bv == nil {
				// Both slots NULL (the IPK storage rule): the value is the
				// rowid on both sides — compare directly, no boxing (the
				// per-row int64→interface{} conversions dominated this
				// gate's allocation profile on rowid-pinned updates).
				return rowIDa == rowIDb
			}
			av = rowIDa
		}
		if bv == nil {
			bv = rowIDb
		}
	}
	if av == nil || bv == nil {
		return false
	}
	return util.CompareValues(av, bv) == 0
}
