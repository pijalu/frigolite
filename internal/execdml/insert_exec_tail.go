package execdml

import (
	"fmt"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/pager"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

func (e *DMLExecutor) prepareInsertRowValues(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, fixedRowID *int64, orConflict string) (int64, *Result) {
	// The table-shape memo carries the WITHOUT ROWID / STRICT flags (pure
	// functions of the CREATE text) so a bulk load does not re-scan the same
	// declaration once per row.
	sh := e.insertShapeFor(tableEntry, colDefs)
	withoutRowid := sh != nil && sh.withoutRowid
	isStrict := sh != nil && sh.strict
	// Determine rowID: if an INTEGER PRIMARY KEY column has an explicit non-nil
	// value, use that value as the rowid (the column IS the rowid). Otherwise
	// auto-assign the next available rowid. REPLACE passes a rowid computed
	// before its conflict deletes (SQLite keeps it through the retry). The
	// explicit/auto source is remembered for the BEFORE-trigger realloc: only
	// an AUTO rowid may be re-allocated when a trigger consumes it — decided
	// on the PRE-fill values (fillIPKRowID overwrites an auto IPK below, and
	// reports -1 for an explicit one, erasing the distinction).
	nextRowID, rowidExplicit, err := e.pkRowIDSource(tableEntry.Name, colDefs, values, tableEntry.RootPage, withoutRowid)
	if err != nil {
		return 0, &Result{Error: err}
	}
	if fixedRowID != nil {
		nextRowID = *fixedRowID
		rowidExplicit = true
	}
	e.ctx.SetLastRowID(nextRowID)

	// If INTEGER PRIMARY KEY column value is nil, set it to the auto-assigned rowid.
	// SQLite behavior: inserting NULL into an INTEGER PRIMARY KEY column causes
	// the column to contain the auto-generated rowid.
	// fillIdx: the auto-filled IPK's index, or -1 for an explicit IPK / no
	// IPK column — exactly what explicitTriggerRowid has always consumed.
	_, fillIdx := e.fillIPKRowID(colDefs, values, nextRowID, withoutRowid, isStrict)

	if res := e.strictCheckAndAffinity(tableEntry, colDefs, values, isStrict); res != nil {
		return 0, res
	}

	// SQLite enforces SQLITE_LIMIT_LENGTH on the total record size: a
	// string/blob value (or the sum of a row's values) longer than the limit
	// errors "string or blob too big" (e_createtable-3.11.5). The limit can
	// be lowered via sqlite3_limit SQLITE_LIMIT_LENGTH.
	if e.insertRecordTooBig(values) {
		return 0, &Result{Error: fmt.Errorf("string or blob too big")}
	}

	// For statement-level REPLACE, substitute the DEFAULT for NULL values in
	// NOT NULL columns BEFORE computing generated columns, then compute
	// generated values and validate constraints (with ON CONFLICT resolution).
	if res, write := e.resolveInsertRowConstraints(tableEntry, colDefs, values, nextRowID, orConflict); res != nil {
		return 0, res
	} else if !write {
		return 0, &Result{Changes: 0}
	}
	if res := e.strictCheckGenerated(tableEntry, colDefs, values, isStrict); res != nil {
		return 0, res
	}

	// FOREIGN KEY constraints are enforced AFTER the row is written and the
	// AFTER triggers fire (SQLite checks immediate FKs at statement end, so
	// an AFTER INSERT trigger may repair the violation by inserting the
	// parent row — e_fkey-31.3). The statement-end check lives in insertRow.

	// Fire BEFORE INSERT triggers — the row is not in the table yet, so
	// only build the row map when triggers exist for this table. The
	// trigger-visible new.rowid is the EXPLICIT rowid (statement rowid
	// column or explicit IPK value); an auto-assigned rowid reads -1.
	expRowID := explicitTriggerRowid(fixedRowID, values, fillIdx, withoutRowid)
	if res := e.fireInsertBeforeTriggersSafe(tableEntry, colDefs, values, &nextRowID, withoutRowid, rowidExplicit, expRowID); res != nil {
		return 0, res
	}
	return nextRowID, nil
}

// fillIPKRowID fills a nil INTEGER PRIMARY KEY column with the assigned rowid,
// reporting whether it was nil and its index (a BEFORE INSERT trigger sees
// new.<ipk> as -1).

// fillIPKRowID fills a nil INTEGER PRIMARY KEY column with the assigned rowid,
// reporting whether it was nil and its index (a BEFORE INSERT trigger sees
// new.<ipk> as -1).

// fillIPKRowID fills a nil INTEGER PRIMARY KEY column with the assigned rowid,
// reporting whether it was nil and its index (a BEFORE INSERT trigger sees
// new.<ipk> as -1).
// fillIPKRowID fills a nil INTEGER PRIMARY KEY column with the assigned rowid,
// reporting whether it was nil and its index (a BEFORE INSERT trigger sees
// new.<ipk> as -1).
func (e *DMLExecutor) fillIPKRowID(colDefs []sql.ColumnDef, values []interface{}, nextRowID int64, withoutRowid bool, isStrict bool) (bool, int) {
	// Record whether an INTEGER PRIMARY KEY column was NULL (auto-assigned):
	// SQLite's BEFORE INSERT trigger sees new.<ipk> as -1 for an auto-assigned
	// rowid (the value is not set until the row is written), so the trigger
	// must not see the pre-assigned rowid (tkt3832).
	if withoutRowid {
		return false, -1
	}
	ipkIndex := e.ipkAliasIndex(colDefs)
	if ipkIndex < 0 || ipkIndex >= len(values) || values[ipkIndex] != nil {
		return false, -1
	}
	// A NULL INTEGER PRIMARY KEY is always auto-filled with the
	// assigned rowid — even when the column declares NOT NULL or the
	// table is STRICT. For a rowid-alias column the value IS the
	// rowid, so the auto-assigned rowid satisfies NOT NULL (verified
	// against sqlite3 3.51: INSERT INTO t(id INTEGER PRIMARY KEY
	// AUTOINCREMENT NOT NULL, x) VALUES('a') auto-assigns; explicit
	// NULL likewise). The e_createtable-4.5.5/4.5.6/4.5.7 NOT NULL
	// rejections use INT PRIMARY KEY (a regular PK column, not a
	// rowid alias) or STRICT non-rowid columns, which this branch
	// does not reach.
	values[ipkIndex] = nextRowID
	return true, ipkIndex
}

// strictCheckAndAffinity runs the STRICT pre/post-affinity value checks and
// applies column type affinity in between.

// strictCheckAndAffinity runs the STRICT pre/post-affinity value checks and
// applies column type affinity in between.

// strictCheckAndAffinity runs the STRICT pre/post-affinity value checks and
// applies column type affinity in between.
// strictCheckAndAffinity runs the STRICT pre/post-affinity value checks and
// applies column type affinity in between.
func (e *DMLExecutor) strictCheckAndAffinity(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, isStrict bool) *Result {
	// STRICT table enforcement: check each value against its column's declared
	// type BEFORE affinity is applied (affinity would convert the value to
	// match the column type, defeating the STRICT check). In STRICT tables,
	// only values compatible with the declared type are allowed.
	if isStrict {
		if err := strictCheckValues(tableEntry, colDefs, values); err != nil {
			return &Result{Error: err}
		}
	}
	applyColumnAffinities(e, values, colDefs)
	// In STRICT mode, affinity may have converted the value — re-check that
	// the converted value still matches the declared type (e.g. integer '42'
	// was accepted as a string but affinity converted it to int64 42).
	if isStrict {
		if err := strictCheckValues(tableEntry, colDefs, values); err != nil {
			return &Result{Error: err}
		}
	}
	return nil
}

// applyColumnAffinities applies each column's type affinity to its value.
// The per-column affinity classes come memoized (columnAffinityClasses);
// a class-0 column (BLOB / no declared type) stores its values as-is and
// skips the conversion entirely.
func applyColumnAffinities(e *DMLExecutor, values []interface{}, colDefs []sql.ColumnDef) {
	// Apply type affinity to each value based on column type. This must run
	// BEFORE the constraint checks so UNIQUE/PRIMARY KEY index comparisons
	// (which may involve expressions over the columns, e.g. "a GLOB b") see
	// the stored, affinity-converted values — SQLite applies affinity when
	// writing the row, before validating constraints.
	affs := e.columnAffinityClasses(colDefs)
	for i, v := range values {
		if i < len(affs) {
			if affs[i] != 0 {
				values[i] = util.ApplyColumnAffinityClass(v, rune(affs[i]))
			}
		} else if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}
}

// columnAffinityClasses returns the memoized affinity class per column of
// colDefs ('I','R','T','N', or 0 for BLOB/none), keyed on the schema
// fingerprint + colDefs identity like columnIndexFor's cache.
func (e *DMLExecutor) columnAffinityClasses(colDefs []sql.ColumnDef) []byte {
	if len(colDefs) == 0 {
		return nil
	}
	fp := e.schemaFingerprint()
	if e.affClassCache != nil && e.affClassFingerprint == fp && e.affClassDefs == &colDefs[0] && e.affClassLen == len(colDefs) {
		return e.affClassCache
	}
	affs := make([]byte, len(colDefs))
	for i := range colDefs {
		affs[i] = byte(util.Affinity(colDefs[i].Type))
	}
	e.affClassFingerprint, e.affClassDefs, e.affClassLen, e.affClassCache = fp, &colDefs[0], len(colDefs), affs
	return affs
}

// ipkAliasIndex returns the memoized index of the table's INTEGER PRIMARY KEY
// rowid-alias column (-1 when none), keyed on the schema fingerprint +
// colDefs identity like columnIndexFor's cache.
func (e *DMLExecutor) ipkAliasIndex(colDefs []sql.ColumnDef) int {
	if len(colDefs) == 0 {
		return -1
	}
	fp := e.schemaFingerprint()
	if e.ipkIdxDefs != nil && e.ipkIdxFingerprint == fp && e.ipkIdxDefs == &colDefs[0] && e.ipkIdxLen == len(colDefs) {
		return e.ipkIdxCache
	}
	idx := -1
	for i := range colDefs {
		if isIPKRowidAliasCol(colDefs[i]) {
			idx = i
			break
		}
	}
	e.ipkIdxFingerprint, e.ipkIdxDefs, e.ipkIdxLen, e.ipkIdxCache = fp, &colDefs[0], len(colDefs), idx
	return idx
}

// strictCheckGenerated enforces STRICT type checking on generated column
// values.

// strictCheckGenerated enforces STRICT type checking on generated column
// values.

// strictCheckGenerated enforces STRICT type checking on generated column
// values.
// strictCheckGenerated enforces STRICT type checking on generated column
// values.
func (e *DMLExecutor) strictCheckGenerated(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, isStrict bool) *Result {
	if !isStrict {
		return nil
	}
	// Generated columns compute values from expressions, and those values must
	// conform to the column's declared type (e.g., REAL column can't have TEXT).
	if err := strictCheckGeneratedValues(tableEntry, colDefs, values); err != nil {
		return &Result{Error: err}
	}
	return nil
}

// fireInsertBeforeTriggersSafe fires BEFORE INSERT triggers for a row when
// triggers exist, mapping RAISE(IGNORE) to a zero-change skip.

// fireInsertBeforeTriggersSafe fires BEFORE INSERT triggers for a row when
// triggers exist, mapping RAISE(IGNORE) to a zero-change skip.

// fireInsertBeforeTriggersSafe fires BEFORE INSERT triggers for a row when
// triggers exist, mapping RAISE(IGNORE) to a zero-change skip. rowidExplicit
// is the pkRowIDSource verdict (an explicit PK/rowid value) and gates the
// post-trigger rowid re-allocation in fireInsertRowBeforeTriggers.
func (e *DMLExecutor) fireInsertBeforeTriggersSafe(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID *int64, withoutRowid, rowidExplicit bool, explicitRowID *int64) *Result {
	if !e.hasTriggersForTable(tableEntry.Name) {
		return nil
	}
	if res := e.fireInsertRowBeforeTriggers(tableEntry, colDefs, values, nextRowID, withoutRowid, rowidExplicit, explicitRowID); res != nil {
		if res.Error == errRowSkipped {
			return &Result{Changes: 0}
		}
		return res
	}
	return nil
}

// unwrapCollationWrappers strips collation wrappers from a values slice so
// only raw values are stored.

// unwrapCollationWrappers strips collation wrappers from a values slice so
// only raw values are stored.

// unwrapCollationWrappers strips collation wrappers from a values slice so
// only raw values are stored.
// unwrapCollationWrappers strips collation wrappers from a values slice so
// only raw values are stored.
func unwrapCollationWrappers(values []interface{}) {
	// Unwrap collation wrappers (a trigger body may pass a column value
	// wrapped with its collation) so only raw values are stored.
	for i := range values {
		if values[i] != nil {
			values[i] = execexpr.UnwrapCollatedValue(values[i])
		}
	}
}

// writeTableRow encodes and inserts a table row, returning the tree (for
// index-failure cleanup) and any write result.
func (e *DMLExecutor) writeTableRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) (*btree.BTree, *Result) {
	sh := e.insertShapeFor(tableEntry, colDefs)
	withoutRowid := sh != nil && sh.withoutRowid
	stored := values
	if withoutRowid {
		// WITHOUT ROWID rows live in an index btree in PK-first storage
		// order (index_xinfo iField layout); see wr_order.go.
		stored = ReorderToStorage(values, WithoutRowidStorageOrder(tableEntry.SQL, colDefs))
	}
	// The on-disk record lands in the executor's reusable record buffer and
	// the cell in the executor's reusable cell: the btree copies payload
	// bytes into pages (or overflow chains) synchronously inside InsertCell
	// and never retains either, and a statement owns the executor for its
	// whole run (no trigger can interleave inside the encode+insert window).
	record, err := e.appendEncodedInsertRecord(e.ipkAliasForWrite(colDefs, stored, withoutRowid))
	if err != nil {
		return nil, &Result{Error: err}
	}
	cell := &e.insCell
	// Reset EVERY wire-format field: prepareCell stamps PayloadLen/LocalLen on
	// an overflow-bearing row and deliberately does not clear them when the
	// next row's payload fits locally (cellPlen/localOrFull trust them when
	// set). A stale PayloadLen/LocalLen leaked into the next row's cell and
	// encoded a corrupt wire cell (fts5prefix doubling: "database disk image
	// is malformed" on the shadow %_data tree).
	cell.Type = storage.CellTableLeaf
	cell.RowID = nextRowID
	cell.Payload = record
	cell.LeftPtr = 0
	cell.Overflow = 0
	cell.PayloadLen = 0
	cell.LocalLen = 0
	if withoutRowid {
		cell.Type = storage.CellIndexLeaf
	}
	tree := e.insertWriteTree(pg, tableEntry, withoutRowid)
	if withoutRowid {
		// Index-leaf insertion order must follow PK value ordering, not
		// raw record bytes (serial-type bytes break memcmp once values
		// differ in magnitude class). Install a PK-aware comparator over
		// the storage-order record (PK slots first).
		if order := WithoutRowidStorageOrder(tableEntry.SQL, colDefs); len(order) == len(colDefs) {
			npk := WRPKSlotCount(tableEntry.SQL, colDefs)
			if npk > 0 {
				tree.SetKeyCompare(WRRecordComparator(npk, colDefs, order))
			}
		}
	}
	if err := tree.InsertCell(cell); err != nil {
		return tree, &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != e.ctx.RootPagePg(pg, tableEntry.Name, tableEntry.RootPage) {
		e.ctx.UpdateRootPagePg(pg, tableEntry.Name, tree.RootPage())
	}
	// The wrapper tracks its own post-split root; the cache key follows so
	// the next row's resolved-root lookup hits the same wrapper.
	e.insertWriteTreeSync(tree.RootPage())
	e.ctx.BumpRowIDCache(pg, tableEntry.RootPage, nextRowID)
	return tree, nil
}

// ipkAliasForWrite returns the values to encode for on-disk storage: the
// INTEGER PRIMARY KEY rowid-alias column nulled (SQLite stores NULL in the
// record for the alias — the value IS the cell rowid; btree.c, autovacuum-9.3
// packing density). WITHOUT ROWID tables have no alias: values pass through.
// The no-alias and already-NULL cases return values itself; the substitution
// copies into the executor's reusable slice, consumed synchronously by the
// record encoder (never mutated: the input slice is left untouched).
func (e *DMLExecutor) ipkAliasForWrite(colDefs []sql.ColumnDef, values []interface{}, withoutRowid bool) []interface{} {
	if withoutRowid {
		return values
	}
	for i, cd := range colDefs {
		if i < len(values) && isIPKRowidAliasCol(cd) {
			if values[i] == nil {
				return values
			}
			out := append(e.insIPKVals[:0], values...)
			out[i] = nil
			e.insIPKVals = out
			return out
		}
	}
	return values
}

// appendEncodedInsertRecord encodes a row's values into the insert path's
// reusable record buffer (the insert twin of appendEncodedRecord): the btree
// write path copies the payload bytes into pages synchronously and never
// retains the slice.
func (e *DMLExecutor) appendEncodedInsertRecord(values []interface{}) ([]byte, error) {
	buf, err := storage.AppendEncodeRecord(e.insRecBuf[:0], values)
	if err != nil {
		return nil, err
	}
	e.insRecBuf = buf
	return buf, nil
}

// fireAfterInsertRowTriggers fires AFTER INSERT triggers for a written row
// when triggers exist.

// fireAfterInsertRowTriggers fires AFTER INSERT triggers for a written row
// when triggers exist.

// fireAfterInsertRowTriggers fires AFTER INSERT triggers for a written row
// when triggers exist.
// fireAfterInsertRowTriggers fires AFTER INSERT triggers for a written row
// when triggers exist.
func (e *DMLExecutor) fireAfterInsertRowTriggers(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) *Result {
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

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.

// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.

// hasTriggersForTable returns true if any AFTER INSERT/UPDATE/DELETE triggers
// exist for the given table across all databases. This is a fast check to avoid
// building trigger row maps when no triggers are registered.
// checkConstraints validates NOT NULL, CHECK, UNIQUE, and PRIMARY KEY
// constraints for a row being inserted.

// hasInsertConstraints reports whether the table imposes any constraints at
// all: column-level NOT NULL/CHECK/PRIMARY KEY/UNIQUE, UNIQUE indexes, or
// table-level constraints.
// hasInsertConstraints reports whether the table imposes any constraints at
// all: column-level NOT NULL/CHECK/PRIMARY KEY/UNIQUE, UNIQUE indexes, or

// insertRecordTooBig reports whether a row's values exceed the
// SQLITE_LIMIT_LENGTH setting. SQLite checks the RECORD size (header
// varint + per-value serial-type varints + data), not just the data sum
// (e_createtable-3.11.5: a 30001+30000+30000 blob row with the limit
// lowered to 90010 errors, while 3×30000 passes because the record is
// exactly 90010). Replicate the serial-type varint overhead.
func (e *DMLExecutor) insertRecordTooBig(values []interface{}) bool {
	limit := e.ctx.LengthLimit()
	if limit <= 0 {
		return false
	}
	var data int
	var serialVarints int
	for _, v := range values {
		serialType, dataLen := storage.EncodeValueSize(v)
		data += dataLen
		serialVarints += util.VarintLen(serialType)
	}
	// header size = 1 (header-size varint itself) + serial varints; the
	// header-size varint may grow, iterate to a fixed point.
	hdrSize := serialVarints + 1
	for {
		hdrLen := util.VarintLen(uint64(hdrSize))
		newHdr := serialVarints + hdrLen
		if newHdr == hdrSize {
			break
		}
		hdrSize = newHdr
	}
	return int64(hdrSize+data) > int64(limit)
}
