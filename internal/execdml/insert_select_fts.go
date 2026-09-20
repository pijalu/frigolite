// SPDX-License-Identifier: GPL-3.0-or-later

package execdml

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

// insertSelectIntoFTS inserts SELECT result rows directly into an FTS table.
func (e *DMLExecutor) insertSelectIntoFTS(ftsTable *fts.FTS3Table, tableEntry *schema.Entry, colDefs []sql.ColumnDef, s *sql.InsertStmt, selectResult *Result) *Result {
	// Writing to an FTS table whose shadow btrees are structurally corrupt
	// fails (fts3corrupt4 24.1: t1_segments page 4 free-space corruption); a
	// write that allocates pages on a DB with a corrupt freelist also fails
	// (fts3corrupt4 29.1).
	if err := e.ftsWriteShadowGuards(tableEntry); err != nil {
		return &Result{Error: err}
	}
	// Build the column mapping so the SELECT's rowid column (an explicit
	// (rowid, x) INSERT list) is applied via InsertWithID and conflicts are
	// detected (fts3conf 1.$tn.6/8/10: INSERT OR * INTO t1(rowid, x)
	// SELECT * FROM source conflicts when the source rowid already exists).
	colMapping := buildInsertColumnMapping(s.Columns, colDefs)
	isReplace := strings.EqualFold(s.OrConflict, "REPLACE")
	if res := e.insertSelectFTSSpecialCommands(tableEntry, s, selectResult); res != nil {
		return res
	}
	var changes int64
	e.lastFTSDocRowID = 0
	for _, row := range selectResult.Rows {
		values, explicitRowID, hasExplicitRowID := e.buildInsertSelectValues(row, s.Columns, colMapping, colDefs)
		ftsValues, langID := ftsSelectRowValues(ftsTable, values)
		if langID < 0 {
			return &Result{Error: fmt.Errorf("constraint failed")}
		}
		if !hasExplicitRowID {
			nextRowID, res := e.ftsInsertSelectAutoRow(ftsTable, tableEntry, ftsValues, langID)
			if res != nil {
				return res
			}
			changes++
			e.lastFTSDocRowID = nextRowID
			continue
		}
		applied, res := e.ftsInsertSelectExplicitRow(ftsTable, tableEntry, s, isReplace, explicitRowID, ftsValues, langID)
		if res != nil {
			return res
		}
		if !applied {
			continue
		}
		changes++
		e.lastFTSDocRowID = explicitRowID
	}
	if changes > 0 {
		// fts3UpdateDocTotals runs at the end of xUpdate (fts3conf 3.2:
		// matchinfo 'na' after a REPLACE INTO ... SELECT path).
		e.ctx.WriteFTSStat(tableEntry.Name)
	}
	return &Result{Changes: changes, LastInsertRowID: e.lastInsertedFTSRowID()}
}

// ftsWriteShadowGuards validates the FTS shadow btrees and the DB freelist
// before an FTS write that may allocate pages.
func (e *DMLExecutor) ftsWriteShadowGuards(tableEntry *schema.Entry) error {
	if res := e.ctx.ValidateFTSShadowRoots(tableEntry.Name); res != nil {
		return res.Error
	}
	// A write that allocates pages on a DB with a corrupt freelist fails
	// (fts3corrupt4 29.1: an INSERT into t1 on an auto-vacuum DB whose
	// freelist/ptr-map trunk page is corrupt).
	return e.ctx.ValidateFreelistForGrowth()
}

// insertSelectFTSSpecialCommands runs an INSERT ... SELECT into the FTS
// table-name column (INSERT INTO t1(t1) SELECT x FROM t2): each SELECT value
// is a special command (fts3corrupt4 24.7: x='optimize','rebuild',... — the
// rebuild fails on a corrupt DB), processed here before inserting as
// documents. A non-nil result ends the statement (the table-name column was
// targeted, so there are no documents to insert, or a command failed).
func (e *DMLExecutor) insertSelectFTSSpecialCommands(tableEntry *schema.Entry, s *sql.InsertStmt, selectResult *Result) *Result {
	if !(len(s.Columns) > 0 && strings.EqualFold(s.Columns[0], tableEntry.Name)) {
		return nil
	}
	for _, row := range selectResult.Rows {
		if len(row) == 0 {
			continue
		}
		cmdStr, ok := row[0].(string)
		if !ok {
			continue
		}
		special, res := e.handleFTSCommand(tableEntry.Name, cmdStr)
		if special {
			if res != nil && res.Error != nil {
				return res
			}
		}
	}
	// All rows were special commands (no documents to insert).
	return &Result{Changes: 0, LastInsertRowID: 0}
}

// ftsSelectRowValues re-maps the values array onto the FTS table's real
// columns and resolves the row's language id. The values array from
// buildInsertSelectValues is indexed by ParseColumnDefs (which includes the
// hidden docid/table-name vtab columns), while Insert/InsertWithID expect
// one value per ftsTable.ColumnNames() (insertFTSRow applies the same
// trimming for VALUES inserts).
func ftsSelectRowValues(ftsTable *fts.FTS3Table, values []interface{}) ([]interface{}, int64) {
	colNames := ftsTable.ColumnNames()
	ftsValues := make([]interface{}, len(colNames))
	for i := range colNames {
		if i < len(values) {
			ftsValues[i] = values[i]
		} else {
			ftsValues[i] = ""
		}
	}
	langID := int64(0)
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		if lv := ftsLangIDFromValues(ftsTable, values, langCol); lv != nil {
			langID = sqlValueToInt64(lv)
		}
	}
	return ftsValues, langID
}

// ftsInsertSelectAutoRow inserts one auto-rowid document and its shadow rows
// (the FTS module auto-assigns 1..N; the content/docsize shadow rows are
// written to match).
func (e *DMLExecutor) ftsInsertSelectAutoRow(ftsTable *fts.FTS3Table, tableEntry *schema.Entry, ftsValues []interface{}, langID int64) (int64, *Result) {
	var nextRowID int64
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		nextRowID = ftsTable.InsertLangID(ftsValues, langID)
	} else {
		nextRowID = ftsTable.Insert(ftsValues)
	}
	e.ctx.SetLastRowID(nextRowID)
	ftsTable.RecordPending(nextRowID)
	if res := e.writeFTSContentRow(tableEntry.Name, nextRowID, ftsValues, ftsTable.CompressFn(), ftsTable, langID); res != nil {
		return 0, res
	}
	if res := e.writeFTSDocsizeRow(tableEntry.Name, nextRowID, ftsTable); res != nil {
		return 0, res
	}
	return nextRowID, nil
}

// ftsInsertSelectExplicitRow inserts one explicit-rowid document: the docid
// UNIQUE constraint is enforced like insertFTSRow (fts3.c fts3UpdateMethod),
// an OR IGNORE conflict skips the row, OR REPLACE deletes the old document
// first. applied=false means the row was skipped.
func (e *DMLExecutor) ftsInsertSelectExplicitRow(ftsTable *fts.FTS3Table, tableEntry *schema.Entry, s *sql.InsertStmt, isReplace bool, explicitRowID int64, ftsValues []interface{}, langID int64) (bool, *Result) {
	if ftsTable.HasDoc(explicitRowID) && !isReplace {
		if strings.EqualFold(s.OrConflict, "IGNORE") {
			return false, nil
		}
		return false, &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableEntry.Name)}
	}
	if ftsTable.HasDoc(explicitRowID) {
		ftsTable.Delete(explicitRowID)
	}
	if langCol := ftsTable.LangIDColName(); langCol != "" {
		ftsTable.InsertWithIDLangID(explicitRowID, ftsValues, langID)
	} else {
		ftsTable.InsertWithID(explicitRowID, ftsValues)
	}
	e.ctx.SetLastRowID(explicitRowID)
	// SQLite's xUpdate records every inserted docid as pending, also
	// for an OR REPLACE that deleted a flushed row (delete-marker
	// segments are handled by DeletedFlush).
	ftsTable.RecordPending(explicitRowID)
	if res := e.writeFTSContentRow(tableEntry.Name, explicitRowID, ftsValues, ftsTable.CompressFn(), ftsTable, langID); res != nil {
		return false, res
	}
	if res := e.writeFTSDocsizeRow(tableEntry.Name, explicitRowID, ftsTable); res != nil {
		return false, res
	}
	return true, nil
}

// lastInsertedFTSRowID re-asserts the connection's last_insert_rowid after
// an FTS insert-select: the loop's internal %_content/%_docsize/%_stat
// shadow writes run through nested Exec calls whose results clobber
// lastRowID (a %_stat REPLACE at id=0 sets it to 0). SQLite's OP_VUpdate
// stores the module's final rowid AFTER its internal writes; the engine
// mirrors that by restoring the last DOCUMENT rowid here.
func (e *DMLExecutor) lastInsertedFTSRowID() int64 {
	if id := e.lastFTSDocRowID; id != 0 {
		e.ctx.SetLastRowID(id)
		return id
	}
	return e.ctx.LastRowID()
}
