package execddl

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// This file exposes the DDL sub-package's DDLExecutor methods to
// internal/exec, which keeps thin forwarders so existing call sites (DML,
// SELECT, PRAGMA, FK checks) stay unchanged. Package-level exports
// (MaxAttachedDatabases, FnSQLiteRenameQuoteFix, TrimGenerationType) are
// declared in their source files.

// ftsMergeThreshold is the %_segdir row count at which a flush validates the
// existing roots (mirroring SQLite's auto-incr-merge reading existing
// segments; the corruption tests fail the 17th insert after a root was
// modified — fts3corrupt 1.3).
const ftsMergeThreshold = 16

// --- DDLExecutor method exports (forwarded from internal/exec) ---

// EchoVTabSource resolves the underlying table name of an echo virtual table.
func (e *DDLExecutor) EchoVTabSource(name string) (string, bool) {
	return e.echoVTabSource(name)
}

// RewriteEchoInsert rewrites an INSERT INTO <echo vtab> statement to target
// the source table.
func (e *DDLExecutor) RewriteEchoInsert(s *sql.InsertStmt, srcName string) {
	e.rewriteEchoInsert(s, srcName)
}

// ExecFTSDelete handles DELETE from an FTS virtual table.
func (e *DDLExecutor) ExecFTSDelete(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.DeleteStmt) *Result {
	return e.execFTSDelete(tableName, ftsTable, colDefs, s)
}

// ValidateFTSSegments checks an FTS table's %_segdir roots for structural
// corruption, returning "database disk image is malformed" when one is
// corrupt. checkBlocks selects whether the %_segments blocks referenced by
// each row's end_block are read and validated too: the merge command reads
// them (fts3corrupt 6.10/8.3), while a MATCH/SELECT only reads the root
// (fts3corrupt4 9.1: a real-SQLite deserialized DB has a leaf root whose
// stale end_block='0 835' references no %_segments rows, yet MATCH succeeds).
// RebuildFTSIndex rebuilds an FTS table's in-memory index from its %_content
// table (the FTS3 'rebuild' special command). It first validates the shadow
// btrees: a corrupt one fails with "database disk image is malformed"
// (fts3corrupt4 24.7: INSERT INTO t1(t1) SELECT 'rebuild' FROM ...).
func (e *DDLExecutor) RebuildFTSIndex(tableName string) *Result {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok {
		return nil
	}
	if res := e.validateFTSShadowRoots(tableName, true); res != nil {
		return res
	}
	ftsTable.Reset()
	// A content=<table> source that cannot supply document text fails the
	// rebuild with "SQL logic error": a missing content table
	// (content=nosuchtable) or an unsupported virtual-table module
	// (sqlite_dbpage in the engine; fts4content 13.1).
	if ct := ftsTable.ContentTable(); ct != "" {
		ctEntry, _, cerr := e.ctx.FindTable(ct)
		if cerr != nil || ctEntry == nil {
			return &Result{Error: fmt.Errorf("SQL logic error")}
		}
		if ctEntry.RootPage == 0 {
			if modName, _, perr := parseVTabSQL(ctEntry.SQL); perr == nil {
				if module, ok := e.ctx.VTables().Find(modName); ok {
					if _, isNoop := module.(*vtab.NoopModule); isNoop {
						return &Result{Error: fmt.Errorf("SQL logic error")}
					}
				}
			}
		}
	}
	// REBUILD drops the old index entirely: SQLite's fts3RebuildMethod deletes
	// every %_segdir row (and the %_segments blocks they reference) and the
	// %_docsize rows before re-reading the content table (fts3.c
	// fts3RebuildMethod: SQL_DELETE_ALL_SEGDIR). Without this the old
	// segments/docsize rows would survive and a segment reload or a
	// SELECT ... FROM <name>_docsize would expose stale documents
	// (fts4content 4.2.5: after rebuild on an empty content table the docsize
	// table is empty).
	e.clearFTSShadowIndex(tableName)
	e.rebuildFTSFromContent(tableName, ftsTable)
	// REBUILD rewrites the whole index, so the %_stat doctotal row is
	// rewritten to match (fts4content 4.1.2/4.2.5: after
	// INSERT INTO ft4(ft4) VALUES('rebuild') on an empty content table,
	// SELECT ... FROM ft4_stat returns 0 X'000000') and every rebuilt
	// document gets a fresh %_docsize row (4.2.4/4.2.6).
	e.writeFTSStat(tableName, ftsTable)
	e.rebuildFTSDocsize(tableName, ftsTable)
	return nil
}

// ReloadFTSIndex drops an FTS table's in-memory index and reloads it from
// the persisted %_segdir/%_segments rows (see DMLContext.ReloadFTSIndex). A
// direct user write to the shadow tables outside the FTS flush (UPDATE
// t1_segdir SET root=..., DELETE FROM t1_segments) makes the in-memory cache
// stale; SQLite always reads the index from the segments, so the next
// MATCH/SELECT must reflect the edited rows — including a corrupt root that
// fails the segment load ("database disk image is malformed", fts4record
// 1.3-1.5).
func (e *DDLExecutor) ReloadFTSIndex(tableName string) *Result {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok {
		return nil
	}
	// Clear drops the in-memory index, the pending/deleted flush lists, and
	// the segment-writer caches (a direct shadow edit invalidates all of
	// them). The next MATCH/SELECT re-reads the persisted segments.
	ftsTable.Clear()
	// Reload the segments into the fresh index (which carries the delete
	// markers and postings); a corrupt segment records a load error that
	// surfaces on the next FTS operation.
	e.loadFTSSegments(tableName, ftsTable)
	return nil
}

// rebuildFTSDocsize rewrites every document's %_docsize row after a rebuild
// (fts3.c fts3RebuildMethod: the docsize table is dropped and repopulated as
// the content table is re-read).
func (e *DDLExecutor) rebuildFTSDocsize(tableName string, ftsTable *fts.FTS3Table) {
	docsize := tableName + "_docsize"
	if _, _, err := e.ctx.FindTable(docsize); err != nil {
		return
	}
	for _, docID := range ftsTable.AllRowsMap() {
		e.writeFTSDocsizeRowDDL(tableName, docID, ftsTable)
	}
}

// writeFTSDocsizeRowDDL writes one document's %_docsize row from the DDL side
// (the DML layer has the same-named method; rebuild runs in DDL). The size
// blob is the FTS3-varint array of per-column token counts (fts3.c
// fts3InsertDocsize / fts3EncodeIntArray).
func (e *DDLExecutor) writeFTSDocsizeRowDDL(tableName string, docID int64, ftsTable *fts.FTS3Table) {
	docsize := tableName + "_docsize"
	if _, _, err := e.ctx.FindTable(docsize); err != nil {
		return
	}
	counts, _ := ftsTable.DocSize(docID)
	blob := encodeFTSIntArrayDDL(counts)
	_ = e.ctx.Exec(&sql.DeleteStmt{Table: docsize, Where: &sql.BinaryOp{
		Operator: "=",
		Left:     &sql.ColumnRef{Name: "docid"},
		Right:    &sql.NumericLit{Value: fmt.Sprintf("%d", docID)},
	}})
	_ = e.ctx.Exec(&sql.InsertStmt{
		Table:   docsize,
		Columns: []string{"docid", "size"},
		Values: [][]sql.Expr{
			{
				&sql.NumericLit{Value: fmt.Sprintf("%d", docID)},
				&sql.BlobLit{Value: blob},
			},
		},
	})
}

// encodeFTSIntArrayDDL encodes a slice of integers as an FTS3 varint array
// (fts3.c fts3EncodeIntArray).
func encodeFTSIntArrayDDL(values []int) []byte {
	var out []byte
	for _, v := range values {
		out = fts.AppendFTS3Varint(out, uint64(v))
	}
	return out
}

// clearFTSShadowIndex deletes every %_segdir row, %_segments block, and
// %_docsize row for an FTS table (fts3.c fts3RebuildMethod drops the whole
// segment index before rebuilding). The b-trees are CLEARED (reset to an empty
// root) rather than per-row deleted: a per-row DELETE leaves stale interior
// boundary keys that make SeekToRowID miss rows after the table is
// repopulated ("database disk image is malformed" reading a block,
// fts4merge4 2.2.1.2).
func (e *DDLExecutor) clearFTSShadowIndex(tableName string) {
	for _, suffix := range []string{"_segdir", "_segments", "_docsize", "_stat"} {
		name := tableName + suffix
		entry, _, err := e.ctx.FindTable(name)
		if err != nil || entry == nil {
			continue
		}
		tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
		if cerr := tree.Clear(); cerr != nil {
			// Fall back to a SQL delete if the direct clear fails.
			_ = e.ctx.Exec(&sql.DeleteStmt{Table: name})
		}
	}
}

// clearFTSContent deletes every row from an FTS table's %_content shadow table
// (the fts3DeleteAll path for a full DELETE FROM <fts>).
func (e *DDLExecutor) clearFTSContent(tableName string) {
	content := tableName + "_content"
	entry, _, err := e.ctx.FindTable(content)
	if err != nil || entry == nil {
		return
	}
	tree := e.ctx.TableBTreeForName(entry.Name, entry.RootPage, true)
	if cerr := tree.Clear(); cerr != nil {
		_ = e.ctx.Exec(&sql.DeleteStmt{Table: content})
	}
}

// ValidateFTSShadowRoots checks an FTS table's %_segments and %_content
// shadow btrees for structural corruption (used by FTS writes, which real
// SQLite reads during an insert).
func (e *DDLExecutor) ValidateFTSShadowRoots(tableName string) *Result {
	// The INSERT path checks only %_segments: an INSERT writes to the index
	// but does not read %_content, so a corrupt content btree is tolerated
	// (fts3corrupt4 24.4: the content cell-23 offset is corrupt but the
	// INSERT succeeds). SELECT (offsets/snippet) checks content via
	// validateFTSSegments.
	return e.validateFTSShadowRoots(tableName, false)
}

// RunFTSIntegrityCheck verifies an FTS table's in-memory index against its
// content rows (SQLite's FTS3 integrity-check, fts3.c
// sqlite3Fts3IntegrityCheck). It reads every content document (from %_content
// for a normal table, from the external content table for content=<table>),
// re-tokenizes them, and compares the postings against the index. A mismatch
// is "database disk image is malformed" (fts4check/fts4intck1).
func (e *DDLExecutor) RunFTSIntegrityCheck(tableName string) *Result {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok {
		return nil
	}
	// Validate the segment roots too (a corrupt root fails the check).
	if res := e.validateFTSSegments(tableName, true); res != nil {
		return res
	}
	docs := make(map[int64][]interface{})
	content := tableName + "_content"
	if ct := ftsTable.ContentTable(); ct != "" {
		content = ct
	}
	contentEntry, _, err := e.ctx.FindTable(content)
	if err != nil || contentEntry == nil {
		// A contentless (content=) table or a missing content table: nothing
		// to validate against.
		return nil
	}
	if contentEntry.RootPage == 0 {
		// A virtual-table content source (echo): materialize its rows.
		rows, rerr := e.virtualTableRows(contentEntry, 0, "", false)
		if rerr != nil || rows == nil {
			return nil
		}
		vtDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
		var names []string
		for _, cd := range vtDefs {
			if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
				continue
			}
			names = append(names, cd.Name)
		}
		for i, row := range rows {
			docID := int64(i + 1)
			vals := make([]interface{}, len(ftsTable.ColumnNames()))
			for vi, name := range names {
				for fi, cn := range ftsTable.ColumnNames() {
					if strings.EqualFold(cn, name) && vi < len(row) {
						vals[fi] = row[vi]
					}
				}
			}
			docs[docID] = vals
		}
	} else {
		tree := e.ctx.TableBTreeForName(contentEntry.Name, contentEntry.RootPage, true)
		cursor, cerr := tree.OpenCursor()
		if cerr != nil {
			return &Result{Error: fmt.Errorf("database disk image is malformed")}
		}
		colDefs := e.ctx.ParseColumnDefs(contentEntry.Name, contentEntry.SQL)
		isContentExternal := ftsTable.ContentTable() != ""
		// Map the content columns to FTS column positions.
		var colIdx []int
		if isContentExternal {
			// Match by name: for each FTS column, its position in the content
			// record's non-rowid values.
			for _, fname := range ftsTable.ColumnNames() {
				found := -1
				vi := 0
				for _, cd := range colDefs {
					if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
						continue
					}
					if strings.EqualFold(cd.Name, fname) {
						found = vi
						break
					}
					vi++
				}
				colIdx = append(colIdx, found)
			}
		} else {
			// A normal FTS table's %_content record is (docid, c0x, c1y, ...)
			// with one c<i><name> column per FTS column. Map only the first
			// len(ColumnNames()) non-docid values (a languageid= column
			// follows the FTS columns and is not part of the document text).
			nFTS := len(ftsTable.ColumnNames())
			vi := 0
			for i, cd := range colDefs {
				if i == 0 || strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
					continue
				}
				_ = cd
				if vi >= nFTS {
					break
				}
				colIdx = append(colIdx, i-1)
				vi++
			}
		}
		for {
			cell, rerr := cursor.ReadCell()
			if rerr != nil || cell == nil {
				break
			}
			rec, derr := storage.DecodeRecord(cell.Payload)
			if derr != nil || rec == nil {
				break
			}
			var docID int64
			if isContentExternal {
				docID = cell.RowID
			} else if len(rec.Values) > 0 {
				if iv, ok := rec.Values[0].(int64); ok {
					docID = iv
				}
			}
			// Build a row map of the record's stored values keyed by column
			// name. The engine's records store every declared column
			// (including the INTEGER PRIMARY KEY rowid alias and the
			// VIRTUAL generated column), so the values align with colDefs.
			rowMap := make(RowMap)
			for i, cd := range colDefs {
				if i < len(rec.Values) {
					rowMap[cd.Name] = rec.Values[i]
				}
			}
			vals := make([]interface{}, len(ftsTable.ColumnNames()))
			if isContentExternal {
				for fi, vi := range colIdx {
					if vi >= 0 && vi < len(rec.Values) {
						vals[fi] = rec.Values[vi]
					}
				}
			} else {
				for fi, vi := range colIdx {
					if vi+1 < len(rec.Values) {
						vals[fi] = rec.Values[vi+1]
					}
				}
			}
			// Recompute any FTS column whose content source is a VIRTUAL
			// generated column.
			for fi, cn := range ftsTable.ColumnNames() {
				for _, cd := range colDefs {
					if !strings.EqualFold(cd.Name, cn) || cd.Generated == nil {
						continue
					}
					genVal, gerr := e.ctx.EvalExpr(cd.Generated, rowMap)
					if gerr != nil {
						return &Result{Error: gerr}
					}
					vals[fi] = genVal
					break
				}
			}
			docs[docID] = vals
			if ok, nerr := cursor.Next(); nerr != nil || !ok {
				break
			}
		}
	}
	// Compare the CONTENT-derived postings against the SEGMENT index
	// (what is persisted in %_segdir/%_segments), not the live in-memory
	// index: SQLite's integrity-check reads the index from the segments, so a
	// hand DELETE FROM %_segdir (fts4check 1.2 case 3) makes it fail even
	// though the in-memory state is intact. Load the segments into a fresh
	// table and compare its index against the content.
	// Per-index comparison (fts3_write.c fts3ChecksumIndex runs once per
	// iIndex): the main index and every prefix band are INDEPENDENT indexes
	// whose doclists never share a key space, so each band is loaded and
	// verified separately. Loading all bands into one key space lets a
	// delete-marker applied while reading a main-band segment erase a prefix
	// band's contribution to the same string key.
	nIndexes := 1 + len(ftsTable.PrefixLengths())
	// SQLite's integrity check reads the index from BOTH the persisted
	// segments AND the in-memory pending-terms hash (fts3.c
	// sqlite3Fts3IntegrityCheck → fts3ChecksumIndex over segments +
	// fts3PendingList). The live table's unflushed inserts (inside an open
	// transaction) are not in %_segdir yet; add them to the fresh index so an
	// uncommitted FTS table still validates (fts4check 5.1: four inserts in
	// a BEGIN are integrity-checked "ok").
	// Unflushed DELETIONS (an UPDATE's Delete+reinsert inside an open
	// transaction) leave the old postings in the persisted segments; drop
	// them from the fresh index BEFORE re-adding the pending documents so
	// the mid-transaction integrity check compares only LIVE terms
	// (fts4langid 6.1: integrity-check after UPDATE vt0 SET lid=1 inside
	// BEGIN must report "ok").
	var firstErr error
	for iIndex := 0; iIndex < nIndexes; iIndex++ {
		fresh, ferr := e.freshFTSFromSegmentsForIndex(tableName, ftsTable, iIndex)
		if ferr != nil {
			return &Result{Error: ferr}
		}
		// Unflushed DELETIONS drop the docid from this band's fresh index;
		// unflushed INSERTS feed this band's expectation exactly as the
		// flush writes one pending segment per index (fts3PendingList).
		for _, delID := range ftsTable.DeletedSnapshot() {
			fresh.DeleteForIndex(delID, iIndex)
		}
		for _, pendingID := range ftsTable.PendingSnapshot() {
			if doc := ftsTable.GetDoc(pendingID); doc != nil {
				fresh.InsertWithIDForIndex(pendingID, doc.Columns, iIndex)
			}
		}
		if err := fresh.IntegrityCheckIndex(docs, iIndex); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return &Result{Error: firstErr}
	}
	return nil
}

// freshFTSFromSegments builds a fresh FTS table whose in-memory index is
// populated solely from the persisted %_segdir/%_segments rows, mirroring how
// a reopened connection sees the index (fts3.c fts3SegReaderNext). The fresh
// table shares the original's schema options (tokenizer, columns, content=).
// freshFTSFromSegments loads ALL bands into one index (reopen semantics).
func (e *DDLExecutor) freshFTSFromSegments(tableName string, orig *fts.FTS3Table) (*fts.FTS3Table, error) {
	return e.freshFTSFromSegmentsForIndex(tableName, orig, -1)
}

// freshFTSFromSegmentsForIndex builds a fresh FTS table from ONE band's
// segments (iIndex -1 loads every band).
func (e *DDLExecutor) freshFTSFromSegmentsForIndex(tableName string, orig *fts.FTS3Table, iIndex int) (*fts.FTS3Table, error) {
	entry, _, err := e.ctx.FindTable(tableName)
	if err != nil || entry == nil {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	moduleName, args, perr := parseVTabSQL(entry.SQL)
	if perr != nil {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	fresh, cerr := fts.NewFTS3Table(tableName, moduleName, args)
	if cerr != nil {
		return nil, fmt.Errorf("database disk image is malformed")
	}
	// Re-derive content=<table> columns by name (like the reopen path).
	if ct := orig.ContentTable(); ct != "" && len(fresh.ColumnNames()) == 0 {
		if ctEntry, _, ferr := e.ctx.FindTable(ct); ferr == nil && ctEntry != nil {
			ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
			var names []string
			for _, cd := range ctDefs {
				if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
					continue
				}
				names = append(names, cd.Name)
			}
			fresh.SetColumnNames(names)
		}
	}
	e.loadFTSSegmentsForIndex(tableName, fresh, iIndex)
	return fresh, nil
}

func (e *DDLExecutor) ValidateFTSSegments(tableName string, checkBlocks bool) *Result {
	return e.validateFTSSegments(tableName, checkBlocks)
}

// WriteFTSStatPublic exposes writeFTSStat to the exec engine (the DML layer
// refreshes %_stat inside xUpdate, mirroring fts3.c fts3UpdateDocTotals).
func (e *DDLExecutor) WriteFTSStatPublic(tableName string) {
	e.writeFTSStat(tableName, e.ctx.FTSTables()[tableName])
}

// ExecFTSUpdate handles UPDATE on an FTS virtual table.
func (e *DDLExecutor) ExecFTSUpdate(tableName string, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, s *sql.UpdateStmt) *Result {
	return e.execFTSUpdate(tableName, ftsTable, colDefs, s)
}

// ExecFTSSelect implements SELECT from an FTS virtual table.
func (e *DDLExecutor) ExecFTSSelect(s *sql.SelectStmt, tableEntry *schema.Entry, ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) *Result {
	return e.execFTSSelect(s, tableEntry, ftsTable, colDefs)
}

// ValidateIndexedBy validates an INDEXED BY clause against a table.
func (e *DDLExecutor) ValidateIndexedBy(tableEntry *schema.Entry, indexName string, s *sql.SelectStmt) error {
	return e.validateIndexedBy(tableEntry, indexName, s)
}

// TableHasColumn reports whether a table declares the named column.
func (e *DDLExecutor) TableHasColumn(tableName, colName string) bool {
	return e.tableHasColumn(tableName, colName)
}

// VirtualTableRows reads all rows of a virtual table.
func (e *DDLExecutor) VirtualTableRows(entry *schema.Entry, bound int64, input string, hasInput bool) ([][]interface{}, error) {
	return e.virtualTableRows(entry, bound, input, hasInput)
}

// MaterializeCorrelatedVTab materializes an input-constrained virtual table
// (fts3tokenize) per left row: for each left row, the value of leftColName
// becomes the vtab's input constraint, and the vtab's rows are collected with
// the producing left-row index (fts3tok1 1.13.2: `WHERE input = x AND
// c1.rowid=t1.rowid`).
func (e *DDLExecutor) MaterializeCorrelatedVTab(entry *schema.Entry, leftColName string, leftRows []RowMap) ([]sql.ColumnDef, []RowMap, []int, error) {
	moduleName, args, err := parseVTabSQL(entry.SQL)
	if err != nil {
		return nil, nil, nil, err
	}
	module, ok := e.ctx.VTables().Find(moduleName)
	if !ok {
		return nil, nil, nil, fmt.Errorf("vtab: module not found: %s", moduleName)
	}
	var colDefs []sql.ColumnDef
	var allMaps []RowMap
	var leftIdx []int
	for li, left := range leftRows {
		rowCount := 0
		vtabInstance, cerr := module.Connect(args)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		if sb, ok := vtabInstance.(vtab.SchemaBoundVTab); ok {
			sbCtx, sbName := resolveVTabContext(e, entry.Name)
			if err := sb.BindSchema(sbCtx.Name, sbName); err != nil {
				return nil, nil, nil, err
			}
		}
		ic, ok := vtabInstance.(interface{ SetInputConstraint(string) })
		if !ok {
			return nil, nil, nil, fmt.Errorf("vtab: module %s is not input-constrained", moduleName)
		}
		// Resolve the left column value from the row map (the column may be
		// stored under its bare name or a qualified key).
		val, ok := left.Get(leftColName)
		if !ok {
			val, ok = left.Get("rowid")
		}
		input := ""
		if ok {
			input = sqlValueString(util.UnwrapColumnValue(val))
		}
		ic.SetInputConstraint(input)
		ci, cok := vtabInstance.(vtab.ColumnInfo)
		if cok && colDefs == nil {
			var types []string
			if cti, tok := vtabInstance.(vtab.ColumnTypeInfo); tok {
				types = cti.ColumnTypes()
			}
			for i, name := range ci.Columns() {
				typ := ""
				if i < len(types) {
					typ = types[i]
				}
				colDefs = append(colDefs, sql.ColumnDef{Name: name, Type: typ})
			}
		}
		cursor, oerr := vtabInstance.Open()
		if oerr != nil {
			return nil, nil, nil, oerr
		}
		nCol := 1
		if ci, ok := vtabInstance.(vtab.ColumnInfo); ok {
			nCol = len(ci.Columns())
		}
		if nCol < 1 {
			nCol = 1
		}
		for cursor.Next() {
			m := make(RowMap)
			for i := 0; i < nCol; i++ {
				cv, cerr := cursor.Column(i)
				if cerr != nil {
					cursor.Close()
					return nil, nil, nil, cerr
				}
				name := ""
				if i < len(colDefs) {
					name = colDefs[i].Name
				}
				m[name] = cv
			}
			// The vtab's rowid is the 1-based row index within this left row's
			// materialization (fts3tok1 1.13.2: c1.rowid = t1.rowid pairs a c1
			// row with the token at that position).
			rowCount++
			m["rowid"] = int64(rowCount)
			allMaps = append(allMaps, m)
			leftIdx = append(leftIdx, li)
		}
		cursor.Close()
	}
	return colDefs, allMaps, leftIdx, nil
}

// sqlValueString renders a SQL value as the text the FTS tokenizer would see
// (used to bind a correlated left column value as an fts3tokenize input).
func sqlValueString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return fmt.Sprintf("%d", t)
	case int:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%v", t)
	}
	return fmt.Sprintf("%v", v)
}

// EnsureFTSForTable lazily re-creates the in-memory FTS table for a virtual
// table entry.
func (e *DDLExecutor) EnsureFTSForTable(entry *schema.Entry) {
	e.ensureFTSForTable(entry)
}

// IsVirtualTable reports whether the schema entry is a virtual table.
func (e *DDLExecutor) IsVirtualTable(entry *schema.Entry) bool {
	return e.isVirtualTable(entry)
}

// ReadCellByRowID reads the cell with the given rowid from a btree.
func (e *DDLExecutor) ReadCellByRowID(tree *btree.BTree, rowID int64) (*storage.Cell, error) {
	return e.readCellByRowID(tree, rowID)
}
