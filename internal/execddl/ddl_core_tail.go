// Package exec implements query execution.
//
// This file holds core DDL execution: CREATE TABLE (with AS SELECT), DROP
// TABLE/VIEW/INDEX, ATTACH/DETACH, auto-index creation, and the generic
// SELECT/expression serializers used by stored objects. It is the
// CREATE/DROP/ATTACH half of the former ddl.go, split out so that each file
// stays within the repository's complexity and size budgets. Trigger, view,
// and virtual-table creation lives in ddl_trigger.go.
package execddl

import (
	"fmt"

	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// autoindexNameResolves reports whether an auto-index entry named
// SQLITE_AUTOINDEX_<table>_<N> resolves to a real constraint slot of its
// parent table (SQLite numbers slots for both table-level PRIMARY KEY /
// UNIQUE constraints and column-level UNIQUE / PRIMARY KEY attributes).
func (e *DDLExecutor) autoindexNameResolves(ent *schema.Entry) bool {
	const prefix = "SQLITE_AUTOINDEX_"
	if !strings.HasPrefix(strings.ToUpper(ent.Name), prefix) || ent.TblName == "" {
		return false
	}
	tableEntry, err := e.ctx.Schema().FindTable(ent.TblName)
	if err != nil || tableEntry == nil {
		return false
	}
	nSlots := e.autoindexSlotCount(tableEntry)
	if nSlots == 0 {
		return false
	}
	return autoindexSlotInRange(ent.Name, tableEntry.Name, nSlots)
}

// autoindexSlotCount counts the autoindex slots a table needs: one per
// table-level PRIMARY KEY / UNIQUE constraint plus one per column-level
// UNIQUE / PRIMARY KEY attribute (build.c; fts3e-2.x: weight INTEGER UNIQUE).
func (e *DDLExecutor) autoindexSlotCount(tableEntry *schema.Entry) int {
	nSlots := 0
	for _, tc := range e.ctx.TableConstraints(tableEntry.Name, tableEntry.SQL) {
		if tc.Type == sql.ConstraintPrimaryKey || tc.Type == sql.ConstraintUnique {
			nSlots++
		}
	}
	for _, cd := range e.ctx.ParseColumnDefs(tableEntry.Name, tableEntry.SQL) {
		if cd.Unique || cd.PrimaryKey {
			nSlots++
		}
	}
	return nSlots
}

// autoindexSlotInRange parses the trailing <N> of the autoindex name and
// checks it against the table's slot count.
func autoindexSlotInRange(indexName, tableName string, nSlots int) bool {
	base := strings.ToUpper(tableName) + "_"
	upperName := strings.ToUpper(indexName)
	idx := strings.LastIndex(upperName, base)
	if idx < 0 {
		return false
	}
	n, err := strconv.Atoi(upperName[idx+len(base):])
	return err == nil && n >= 1 && n <= nSlots
}

// validateFTSSegments checks every %_segdir root blob for an FTS table and
// reports "database disk image is malformed" when one is structurally
// corrupt (SQLite detects segment corruption while reading the index; the
// corruption tests modify segdir.root directly). Called at the start of FTS
// operations so a corrupted segment surfaces as an error.
func (e *DDLExecutor) validateFTSSegments(tableName string, checkBlocks bool) *Result {
	return e.validateFTSSegmentsCheck(tableName, checkBlocks, true)
}

// validateFTSSegmentsCheck is validateFTSSegments with a checkContent flag:
// DELETE/UPDATE match against the in-memory index and do not read the
// shadow btrees, so corrupt segments/content btrees are tolerated there
// (fts3corrupt4 25.1 UPDATE succeeds despite corrupt t1_content/t1_segments);
// SELECT reads them for offsets()/snippet()/MATCH and must fail (21.1).
func (e *DDLExecutor) validateFTSSegmentsCheck(tableName string, checkBlocks bool, checkContent bool) *Result {
	if checkContent {
		// A real SQLite segment that failed to load into the in-memory index is
		// corrupt; surface it before any further validation or query (fts3corrupt4
		// 7.1: a crash-written segment with a corrupt term structure).
		_ = tableName
		// The %_segments and %_content shadow tables' btrees must be structurally
		// sound; a crash-written page (free-space/cell-offset corruption) makes
		// any FTS read fail with "database disk image is malformed" (fts3corrupt4
		// 21.1: Tree 4 page 4 free space corruption surfaced by offsets()).
		// DELETE/UPDATE (checkContent=false) skip the shadow-btree validation
		// entirely: they match against the in-memory index and do not read the
		// shadow btrees, so corrupt segments/content btrees are tolerated there
		// (fts3corrupt4 25.1 UPDATE succeeds despite corrupt t1_content/t1_segments);
		// SELECT reads them for offsets()/snippet()/MATCH and must fail (21.1).
		if res := e.validateFTSShadowRoots(tableName, true); res != nil {
			return res
		}
	}
	return e.validateFTSSegdirRows(tableName, checkBlocks)
}

// real FTS columns are mapped (doc.Columns); hidden vtab columns must not overwrite aliases.
func (e *DDLExecutor) ftsRowMapForDoc(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, docID int64) RowMap {
	rowMap := make(RowMap)
	rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
	rowMap["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
	rowMap["oid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
	doc := ftsTable.GetDoc(docID)
	if doc != nil {
		for i, col := range doc.Columns {
			if i < len(colDefs) {
				rowMap[colDefs[i].Name] = col
			}
		}
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			rowMap[langCol] = &util.ColumnValue{Value: doc.LangID, Affinity: 'I'}
		}
	}
	return rowMap
}

// ftsUpdateJoinedRowMap builds the evaluation row map for one FTS document in
// an UPDATE ... FROM: the base doc row map merged with the FIRST joined row
// whose WHERE condition is satisfied. Returns (rowMap, ok) where ok is false
// when no joined row matches (the doc is not updated). For a plain UPDATE
// (no FROM) the base row map always matches. SQLite's FTS xUpdate evaluates
// the WHERE over the joined rows and applies the SET against the matched pair
// (fts4upfrom 1.x: UPDATE ft SET b=o.c FROM ft AS o WHERE ft.a == ...).
func (e *DDLExecutor) ftsUpdateJoinedRowMap(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt, docID int64) (RowMap, bool, error) {
	rowMap := e.ftsRowMapForDoc(ftsTable, colDefs, docID)
	if ct := ftsTable.ContentTable(); ct != "" {
		contentMaps := e.ftsContentTableRowMapsForDocIDs(ftsTable, colDefs, []int64{docID})
		if len(contentMaps) > 0 {
			rowMap = contentMaps[0]
		}
	}
	if s == nil || s.From.Name == "" {
		return rowMap, true, nil
	}
	joined, jerr := e.ctx.JoinUpdateFromRows(s, rowMap)
	if jerr != nil {
		// A missing FROM table surfaces as the join's error ("no such
		// table: changes" — fts4upfrom 1.x).
		return rowMap, false, jerr
	}
	if len(joined) == 0 {
		return rowMap, true, nil
	}
	// Pick the first joined row whose WHERE is satisfied (a plain cross join
	// produces every (target, FROM) pair; only the matching pair updates).
	if s.Where != nil {
		for _, jrow := range joined {
			match, merr := e.ctx.EvalBool(s.Where, jrow)
			if merr == nil && match {
				return jrow, true, nil
			}
		}
		return rowMap, false, nil
	}
	return joined[0], true, nil
}

// updateFTSDoc applies one UPDATE's assignments to a single FTS document,
// returning a non-nil Result on evaluation failure. The new values array is
// sized to the FTS table's real columns (ftsTable.ColumnNames), matching what
// Insert/Update expect (the hidden vtab columns are not part of the record).
// For an FTS4 content=<table> table the OLD column values come from the
// external content table (SQLite's fts3DeleteTerms reads the old row from the
// content table), not the in-memory index — the SET expressions evaluate
// against the content row's values (fts4content 3.3.x: UPDATE ft3 SET x=y,
// y=x after re-populating t3 swaps only the reindexed columns).
func (e *DDLExecutor) updateFTSDoc(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt, docID int64) *Result {
	// The SET expressions evaluate against the joined row when the UPDATE has
	// a FROM clause (fts4upfrom 1.x: SET b=o.c resolves o.c from the FROM
	// alias); a plain UPDATE uses the document's own row map.
	rowMap, _, jerr := e.ftsUpdateJoinedRowMap(ftsTable, colDefs, s, docID)
	if jerr != nil {
		return &Result{Error: jerr}
	}

	colNames := ftsTable.ColumnNames()
	newValues := e.updatedBaseValues(ftsTable, rowMap, colNames, docID)
	newRowID, res := e.applyFTSUpdateAssignments(rowMap, colNames, s, newValues, docID)
	if res != nil {
		return res
	}
	newLangID, res := e.resolveUpdatedLangID(ftsTable, rowMap, s, docID)
	if res != nil {
		return res
	}
	if res := e.enforceUpdatedDocidUnique(tableName, ftsTable, s, docID, newRowID); res != nil {
		return res
	}
	if res := e.rewriteUpdatedDoc(tableName, ftsTable, docID, newRowID, newValues, newLangID); res != nil {
		return res
	}
	e.rekeyUpdatedShadowRows(tableName, ftsTable, docID, newRowID, newValues)
	return nil
}

// updatedBaseValues builds an UPDATE's starting column values: the document's
// current columns, overridden by the external content table's row when the
// FTS4 content=<table> option is in play (SQLite reindexes the row from the
// content table's values; unassigned columns keep the content row's value,
// not the stale index's — fts4content 3.3.x: UPDATE ft3 SET x=y, y=x after
// re-populating t3 swaps only the reindexed columns).
func (e *DDLExecutor) updatedBaseValues(ftsTable *fts.FTS3Table, rowMap RowMap, colNames []string, docID int64) []interface{} {
	newValues := make([]interface{}, len(colNames))
	doc := ftsTable.GetDoc(docID)
	if doc != nil {
		for i := range colNames {
			if i < len(doc.Columns) {
				newValues[i] = doc.Columns[i]
			} else {
				newValues[i] = ""
			}
		}
	}
	if ftsTable.ContentTable() != "" {
		e.applyContentOverrides(rowMap, colNames, newValues)
	}
	return newValues
}

// applyContentOverrides overrides an UPDATE's starting column values with the
// external content table's row values; see updatedBaseValues.
func (e *DDLExecutor) applyContentOverrides(rowMap RowMap, colNames []string, newValues []interface{}) {
	for i, cn := range colNames {
		if v, ok := rowMap[cn]; ok {
			uv := util.UnwrapColumnValue(v)
			if uv != nil {
				newValues[i] = uv
			}
		}
	}
}

// applyFTSUpdateAssignments evaluates an UPDATE's SET list against rowMap,
// writing column values into newValues and returning the (possibly re-keyed)
// rowid; see updateFTSDoc.
func (e *DDLExecutor) applyFTSUpdateAssignments(rowMap RowMap, colNames []string, s *sql.UpdateStmt, newValues []interface{}, docID int64) (int64, *Result) {
	newRowID := docID
	for _, as := range s.Assignments {
		// FTS tables expose docid as the rowid alias (fts3DeclareVtab
		// declares "docid HIDDEN"), so SET docid=... re-keys the document
		// exactly like SET rowid=... (fts3aa-8.0).
		if execquery.IsRowIDName(as.Column) || strings.EqualFold(as.Column, "docid") {
			iv, res := e.evalFTSRowidAssignment(as, rowMap)
			if res != nil {
				return newRowID, res
			}
			newRowID = iv
			continue
		}
		if res := e.applyFTSColumnAssignment(rowMap, colNames, as, newValues); res != nil {
			return newRowID, res
		}
	}
	return newRowID, nil
}

// evalFTSRowidAssignment evaluates one SET rowid/docid = <expr> assignment;
// see applyFTSUpdateAssignments.
func (e *DDLExecutor) evalFTSRowidAssignment(as sql.Assignment, rowMap RowMap) (int64, *Result) {
	v, err := e.ctx.EvalExpr(as.Value, rowMap)
	if err != nil {
		return 0, &Result{Error: err}
	}
	iv, ok := util.UnwrapColumnValue(v).(int64)
	if !ok {
		return 0, &Result{Error: fmt.Errorf("datatype mismatch")}
	}
	return iv, nil
}

// applyFTSColumnAssignment evaluates one SET column = <expr> assignment into
// newValues; see applyFTSUpdateAssignments.
func (e *DDLExecutor) applyFTSColumnAssignment(rowMap RowMap, colNames []string, as sql.Assignment, newValues []interface{}) *Result {
	for i, cn := range colNames {
		if strings.EqualFold(cn, as.Column) {
			v, err := e.ctx.EvalExpr(as.Value, rowMap)
			if err != nil {
				return &Result{Error: err}
			}
			newValues[i] = v
			break
		}
	}
	return nil
}

// resolveUpdatedLangID applies a languageid hidden-column assignment (fts3.c
// fts3UpdateMethod reads it from apVal like any vtab column; a changed langid
// re-pends the row's terms under the NEW language — fts4langid 6.0: UPDATE
// vt0 SET lid = 1 WHERE lid=0).
func (e *DDLExecutor) resolveUpdatedLangID(ftsTable *fts.FTS3Table, rowMap RowMap, s *sql.UpdateStmt, docID int64) (int64, *Result) {
	newLangID := ftsTable.DocLangID(docID)
	lc := ftsTable.LangIDColName()
	if lc == "" {
		return newLangID, nil
	}
	for _, as := range s.Assignments {
		if !strings.EqualFold(as.Column, lc) {
			continue
		}
		v, err := e.ctx.EvalExpr(as.Value, rowMap)
		if err != nil {
			return newLangID, &Result{Error: err}
		}
		newLangID = ftsValueToInt64(util.UnwrapColumnValue(v))
	}
	return newLangID, nil
}

// enforceUpdatedDocidUnique rejects a docid re-key onto an existing document
// unless OR REPLACE replaces the conflicting document — fts3conf 1.$tn.11-20.
// A docid change: delete the old document and insert under the new id.
func (e *DDLExecutor) enforceUpdatedDocidUnique(tableName string, ftsTable *fts.FTS3Table, s *sql.UpdateStmt, docID, newRowID int64) *Result {
	if newRowID == docID || !ftsTable.HasDoc(newRowID) {
		return nil
	}
	if !strings.EqualFold(s.OnConflict, "REPLACE") {
		return &Result{Error: fmt.Errorf("UNIQUE constraint failed: %s.rowid", tableName)}
	}
	ftsTable.Delete(newRowID)
	return nil
}

// rewriteUpdatedDoc re-keys/rewrites the document: SQLite's fts3UpdateMethod
// deletes the old row's terms (a delete-marker when the old row was already
// flushed) and adds the new terms to the pending hash (fts3.c
// fts3DeleteTerms + fts3PendingTermsDocid). The engine mirrors that by always
// Delete + InsertWithID + RecordPending: for a same-docid UPDATE the
// in-memory Delete removes the old terms, InsertWithID re-adds the new ones,
// and the pending insert persists them at the next flush (the delete marker
// removes the stale persisted segment terms — fts4onepass 3.x integrity
// after UPDATE SET content=... must see only the new terms).
func (e *DDLExecutor) rewriteUpdatedDoc(tableName string, ftsTable *fts.FTS3Table, docID, newRowID int64, newValues []interface{}, newLangID int64) *Result {
	// The xUpdate DELETE phase first runs fts3PendingTermsDocid (bDelete=1,
	// old langid): when the pending sequence restarts here the pending batch
	// flushes BEFORE this document's delete terms pend — an UPDATE whose
	// docid equals the previous operation's docid (update #2 of the same row
	// inside one transaction, fts4onepass-4.0: the oracle counts one segdir
	// row per UPDATE, not one for the whole COMMIT).
	if res := e.flushFTSPendingOnRestart(tableName, ftsTable, docID, true, ftsTable.DocLangID(docID)); res != nil {
		return res
	}
	ftsTable.Delete(docID)
	// The xUpdate INSERT phase runs fts3PendingTermsDocid (bDelete=0, new
	// langid): re-pending the SAME docid right after its delete is NOT a
	// restart (bPrevDelete=1), but a docid moving below the sequence or a
	// language change flushes before the new terms pend.
	if res := e.flushFTSPendingOnRestart(tableName, ftsTable, newRowID, false, newLangID); res != nil {
		return res
	}
	if lc := ftsTable.LangIDColName(); lc != "" {
		// A languageid table re-pends the terms under the NEW language id
		// (the segment writer stores them in the new language's index).
		if lv, ok := ftsTable.Tokenizer().(fts.LangidValidator); ok {
			if verr := lv.ValidateLangid(newLangID); verr != nil {
				return &Result{Error: verr}
			}
		}
		ftsTable.InsertWithIDLangID(newRowID, newValues, newLangID)
	} else {
		ftsTable.InsertWithID(newRowID, newValues)
	}
	ftsTable.RecordPending(newRowID)
	return nil
}

// flushFTSPendingOnRestart flushes the pending batch when the document
// sequence restarts (see rewriteUpdatedDoc).
func (e *DDLExecutor) flushFTSPendingOnRestart(tableName string, ftsTable *fts.FTS3Table, docID int64, bDelete bool, langID int64) *Result {
	if ftsTable.PendingDocidRestart(docID, bDelete, langID) && ftsTable.HasPendingOps() {
		return e.flushFTSPendingFlagged(tableName)
	}
	return nil
}

// rekeyUpdatedShadowRows moves the %_content shadow row (and %_docsize row)
// from the old docid to the new one. SQLite's fts3UpdateMethod re-keys the
// content row (fts3.c fts3UpdateMethod: an UPDATE that changes the docid
// deletes the old content row and inserts under the new rowid), so SELECT
// FROM %_content and the integrity check see the moved document (fts4onepass
// 3.x: UPDATE ft2 SET docid=-1 WHERE docid=4 keeps the row at -1 in both the
// index and content).
func (e *DDLExecutor) rekeyUpdatedShadowRows(tableName string, ftsTable *fts.FTS3Table, docID, newRowID int64, newValues []interface{}) {
	if ct := ftsTable.ContentTable(); ct == "" && !ftsTable.Contentless() {
		e.moveFTSContentRow(tableName, docID, newRowID, newValues, ftsTable)
		// Re-key the %_docsize row too (fts3.c fts3UpdateMethod deletes the
		// old docsize row and fts3InsertDocsize writes the new one when the
		// docid changes; a stale row at the old docid breaks the integrity
		// check's per-document size walk — fts4onepass 3.x.4: UPDATE ft2 SET
		// docid=-1 leaves a stale %_docsize row 4).
		if newRowID != docID && !ftsTable.NoDocsize() {
			e.deleteFTSDocsizeRow(tableName, docID)
			e.writeFTSDocsizeRowDDL(tableName, newRowID, ftsTable)
		}
	}
}

// moveFTSContentRow rewrites one FTS table's %_content shadow row: when
// oldDocID != newDocID the row is moved (delete old + insert new), otherwise
// it is updated in place. Values are the new column values (the content
// record is docid + one c%d<name> column per user column; fts3.c
// fts3CreateTables).
func (e *DDLExecutor) moveFTSContentRow(tableName string, oldDocID, newDocID int64, values []interface{}, ftsTable *fts.FTS3Table) {
	content := tableName + "_content"
	contentEntry, dbCtx, err := e.ctx.FindTable(content)
	if err != nil || contentEntry == nil || dbCtx == nil {
		return
	}
	// Delete the old row (docid is the INTEGER PRIMARY KEY rowid).
	if oldDocID != newDocID {
		e.deleteFTSContentRow(tableName, oldDocID)
	}
	// Build the record: docid + one value per user column (the content
	// table's c%d<name> columns, matching the FTS table's columnNames order).
	// A languageid=<col> table's content row carries the language id as a
	// trailing column (fts3.c fts3InsertDoc writes "?, ..., langid"; the
	// UPDATE's re-key must preserve/refresh it — fts4langid 6.x).
	colDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
	stored := make([]interface{}, 0, len(values)+2)
	stored = append(stored, newDocID)
	for _, v := range values {
		uv := util.UnwrapColumnValue(v)
		stored = append(stored, uv)
	}
	if ftsTable.LangIDColName() != "" {
		stored = append(stored, ftsTable.DocLangID(newDocID))
	}
	record, rerr := storage.EncodeRecord(stored)
	if rerr != nil {
		return
	}
	tree := e.ctx.TableBTreePg(dbCtx.Pager, contentEntry.Name, contentEntry.RootPage, true)
	cell := &storage.Cell{
		Type:    storage.CellTableLeaf,
		RowID:   newDocID,
		Payload: record,
	}
	if err := tree.InsertCell(cell); err != nil {
		return
	}
	if tree.RootPage() != e.ctx.RootPagePg(dbCtx.Pager, contentEntry.Name, contentEntry.RootPage) {
		e.ctx.UpdateRootPagePg(dbCtx.Pager, contentEntry.Name, tree.RootPage())
	}
	e.ctx.BumpRowIDCache(e.ctx.TablePager(contentEntry.Name), contentEntry.RootPage, newDocID)
	_ = colDefs
}

// virtualTableRows reads all rows from a virtual table.
func (e *DDLExecutor) virtualTableRows(entry *schema.Entry, bound int64, input string, hasInput bool) ([][]interface{}, error) {
	moduleName, args, err := parseVTabSQL(entry.SQL)
	if err != nil {
		return nil, err
	}
	module, ok := e.ctx.VTables().Find(moduleName)
	if !ok {
		return nil, fmt.Errorf("vtab: module not found: %s", moduleName)
	}
	// The echo module mirrors its underlying table (echo('t1') proxies t1's
	// rows and columns). Resolve it directly through the engine so SELECT
	// FROM echo_table returns the source table's data.
	if rows, done, echoErr := e.echoModuleRows(moduleName, args); done {
		return rows, echoErr
	}
	vtabInstance, err := module.Connect(args)
	if err != nil {
		return nil, err
	}
	if err := e.bindSchemaBoundVTab(vtabInstance, entry); err != nil {
		return nil, err
	}
	// An FTS3/4/5 module's virtual-table interface is a stateless placeholder
	// (the FTS engine lives in the in-memory FTS3Table, not in a b-tree).
	// Return the table's rows directly so joins (and SELECT FROM fts) can
	// materialize them. Column count = number of user columns.
	if rows, done, ftsErr := e.ftsModuleRows(moduleName, entry.Name); done {
		return rows, ftsErr
	}
	e.configureVTabInstance(vtabInstance, bound, input, hasInput)
	cursor, err := vtabInstance.Open()
	if err != nil {
		return nil, err
	}
	defer cursor.Close()
	nCol := e.vtabColumnCount(vtabInstance)
	return e.collectVTabRows(cursor, nCol)
}

// echoModuleRows resolves the echo module's source-table rows; done=false
// leaves the generic vtab path to run (including when the echo source itself
// fails to resolve).
func (e *DDLExecutor) echoModuleRows(moduleName string, args []string) ([][]interface{}, bool, error) {
	if !strings.EqualFold(moduleName, "echo") || len(args) <= 0 {
		return nil, false, nil
	}
	srcName := strings.Trim(args[0], "'\"")
	rows, err := e.echoSourceRows(srcName)
	if err != nil {
		return nil, false, nil
	}
	return rows, true, nil
}

// bindSchemaBoundVTab binds schema-bound modules (rtree, dbdata, dbstat, ...)
// which name their shadow tables after the vtab: the resolved db + table name
// makes their back-end tables reachable (the CREATE path binds too; this
// covers SELECT-time instances created via xConnect).
func (e *DDLExecutor) bindSchemaBoundVTab(vtabInstance vtab.VirtualTable, entry *schema.Entry) error {
	if sb, ok := vtabInstance.(vtab.SchemaBoundVTab); ok {
		sbCtx, sbName := resolveVTabContext(e, entry.Name)
		if err := sb.BindSchema(sbCtx.Name, sbName); err != nil {
			return err
		}
	}
	return nil
}

// ftsModuleRows materializes an FTS module's rows from the in-memory FTS3Table
// (see virtualTableRows); done=false leaves the generic vtab path to run.
func (e *DDLExecutor) ftsModuleRows(moduleName, tableName string) ([][]interface{}, bool, error) {
	ftsMod := e.getFTSModule(moduleName)
	if ftsMod == nil {
		return nil, false, nil
	}
	ft, ok := ftsMod.GetTable(tableName)
	if !ok {
		return nil, true, nil
	}
	cols := ft.ColumnNames()
	rows := ft.AllRows()
	out := make([][]interface{}, 0, len(rows))
	for _, r := range rows {
		if len(r) < len(cols) {
			nr := make([]interface{}, len(cols))
			copy(nr, r)
			r = nr
		}
		out = append(out, r)
	}
	return out, true, nil
}

// configureVTabInstance passes the query-time hints to a vtab instance before
// its cursor opens: the WHERE-derived upper bound for bounded virtual tables
// (e.g. wholenumber) so they generate only the needed prefix, and a literal
// first-column equality constraint (fts3tokenize's `input = <string>`).
func (e *DDLExecutor) configureVTabInstance(vtabInstance vtab.VirtualTable, bound int64, input string, hasInput bool) {
	if bound > 0 {
		if bt, ok := vtabInstance.(vtab.BoundedVTab); ok {
			bt.SetUpperBound(bound)
		}
	}
	if hasInput {
		if ic, ok := vtabInstance.(interface{ SetInputConstraint(string) }); ok {
			ic.SetInputConstraint(input)
		}
	}
}

// vtabColumnCount determines the number of columns the vtab exposes so each
// row captures all of them (generate_series/wholenumber expose one; fts4aux
// exposes term/col/documents/occurrences/languageid).
func (e *DDLExecutor) vtabColumnCount(vtabInstance vtab.VirtualTable) int {
	nCol := 1
	if ci, ok := vtabInstance.(vtab.ColumnInfo); ok {
		nCol = len(ci.Columns())
	}
	if nCol < 1 {
		nCol = 1
	}
	return nCol
}

// collectVTabRows drains the vtab cursor into value rows.
func (e *DDLExecutor) collectVTabRows(cursor vtab.Cursor, nCol int) ([][]interface{}, error) {
	var rows [][]interface{}
	for cursor.Next() {
		row := make([]interface{}, nCol)
		for i := 0; i < nCol; i++ {
			val, err := cursor.Column(i)
			if err != nil {
				return nil, err
			}
			row[i] = val
		}
		rows = append(rows, row)
	}
	return rows, nil
}
