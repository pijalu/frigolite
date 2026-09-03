package execdml

import (
	"fmt"
	"strings"

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
	// recordTooBig reports whether a row's values exceed the
	// SQLITE_LIMIT_LENGTH setting. SQLite checks the RECORD size (header
	// varint + per-value serial-type varints + data), not just the data sum
	// (e_createtable-3.11.5: a 30001+30000+30000 blob row with the limit
	// lowered to 90010 errors, while 3×30000 passes because the record is
	// exactly 90010). Replicate the serial-type varint overhead.
	recordTooBig := func(values []interface{}) bool {
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

	// Determine rowID: if an INTEGER PRIMARY KEY column has an explicit non-nil
	// value, use that value as the rowid (the column IS the rowid). Otherwise
	// auto-assign the next available rowid. REPLACE passes a rowid computed
	// before its conflict deletes (SQLite keeps it through the retry).
	nextRowID, err := e.pkRowID(tableEntry.Name, colDefs, values, tableEntry.RootPage, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL)))
	if err != nil {
		return 0, &Result{Error: err}
	}
	if fixedRowID != nil {
		nextRowID = *fixedRowID
	}
	e.ctx.SetLastRowID(nextRowID)

	// If INTEGER PRIMARY KEY column value is nil, set it to the auto-assigned rowid.
	// SQLite behavior: inserting NULL into an INTEGER PRIMARY KEY column causes
	// the column to contain the auto-generated rowid.
	withoutRowid := hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))
	isStrict := isStrictTable(tableEntry.SQL)
	ipkWasNil, ipkIndex := e.fillIPKRowID(colDefs, values, nextRowID, withoutRowid, isStrict)

	if res := e.strictCheckAndAffinity(tableEntry, colDefs, values, isStrict); res != nil {
		return 0, res
	}

	// SQLite enforces SQLITE_LIMIT_LENGTH on the total record size: a
	// string/blob value (or the sum of a row's values) longer than the limit
	// errors "string or blob too big" (e_createtable-3.11.5). The limit can
	// be lowered via sqlite3_limit SQLITE_LIMIT_LENGTH.
	if recordTooBig(values) {
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
	// only build the row map when triggers exist for this table.
	if res := e.fireInsertBeforeTriggersSafe(tableEntry, colDefs, values, &nextRowID, withoutRowid, ipkWasNil, ipkIndex); res != nil {
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
	ipkWasNil := false
	ipkIndex := -1
	for i, cd := range colDefs {
		if !withoutRowid && isIPKRowidAliasCol(cd) &&
			i < len(values) && values[i] == nil {
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
			ipkWasNil = true
			ipkIndex = i
			values[i] = nextRowID
			break
		}
	}
	return ipkWasNil, ipkIndex
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
	applyColumnAffinities(values, colDefs)
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

// applyColumnAffinities applies each column's type affinity to its value.

// applyColumnAffinities applies each column's type affinity to its value.
// applyColumnAffinities applies each column's type affinity to its value.
func applyColumnAffinities(values []interface{}, colDefs []sql.ColumnDef) {
	// Apply type affinity to each value based on column type. This must run
	// BEFORE the constraint checks so UNIQUE/PRIMARY KEY index comparisons
	// (which may involve expressions over the columns, e.g. "a GLOB b") see
	// the stored, affinity-converted values — SQLite applies affinity when
	// writing the row, before validating constraints.
	for i, v := range values {
		if i < len(colDefs) {
			values[i] = util.ApplyColumnAffinity(v, colDefs[i].Type)
		}
	}
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
// triggers exist, mapping RAISE(IGNORE) to a zero-change skip.
// fireInsertBeforeTriggersSafe fires BEFORE INSERT triggers for a row when
// triggers exist, mapping RAISE(IGNORE) to a zero-change skip.
func (e *DMLExecutor) fireInsertBeforeTriggersSafe(tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID *int64, withoutRowid, ipkWasNil bool, ipkIndex int) *Result {
	if !e.hasTriggersForTable(tableEntry.Name) {
		return nil
	}
	if res := e.fireInsertRowBeforeTriggers(tableEntry, colDefs, values, nextRowID, withoutRowid, ipkWasNil, ipkIndex); res != nil {
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

// writeTableRow encodes and inserts a table row, returning the tree (for
// index-failure cleanup) and any write result.

// writeTableRow encodes and inserts a table row, returning the tree (for
// index-failure cleanup) and any write result.
// writeTableRow encodes and inserts a table row, returning the tree (for
// index-failure cleanup) and any write result.
func (e *DMLExecutor) writeTableRow(pg *pager.Pager, tableEntry *schema.Entry, colDefs []sql.ColumnDef, values []interface{}, nextRowID int64) (*btree.BTree, *Result) {
	record, err := storage.EncodeRecord(NullIPKAliasForWrite(colDefs, values, hasWithoutRowidKeyword(strings.ToUpper(tableEntry.SQL))))
	if err != nil {
		return nil, &Result{Error: err}
	}
	tree := e.ctx.TableBTreePg(pg, tableEntry.Name, tableEntry.RootPage, true)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   nextRowID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return tree, &Result{Error: err}
	}
	// Track root page changes (after splits)
	if tree.RootPage() != e.ctx.RootPagePg(pg, tableEntry.Name, tableEntry.RootPage) {
		e.ctx.UpdateRootPagePg(pg, tableEntry.Name, tree.RootPage())
	}
	e.ctx.BumpRowIDCache(pg, tableEntry.RootPage, nextRowID)
	return tree, nil
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
