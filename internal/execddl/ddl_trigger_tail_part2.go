// Package exec implements query execution.
//
// This file holds DDL execution for CREATE TRIGGER, CREATE VIEW, and CREATE
// VIRTUAL TABLE, plus the SQL-text serialization helpers used by stored
// triggers and views. It is the trigger/view/vtable half of the former
// ddl.go, split out so that each file stays within the repository's
// complexity and size budgets. Core CREATE/DROP/ATTACH execution and the
// generic expression serializer live in ddl_core.go.
package execddl

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func ftsValueToInt64(v interface{}) int64 {
	switch x := util.UnwrapColumnValue(v).(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		s := strings.TrimSpace(x)
		neg := false
		if strings.HasPrefix(s, "-") {
			neg = true
			s = s[1:]
		} else if strings.HasPrefix(s, "+") {
			s = s[1:]
		}
		var n int64
		for _, c := range s {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int64(c-'0')
		}
		if neg {
			return -n
		}
		return n
	}
	return 0
}

// ftsRowMaps builds RowMaps for every document in an FTS table.
func (e *DDLExecutor) ftsRowMaps(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef) []RowMap {
	var allRowMaps []RowMap
	for _, docID := range ftsTable.AllRowsMap() {
		doc := ftsTable.GetDoc(docID)
		if doc == nil {
			continue
		}
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		rowMap["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		for i, col := range doc.Columns {
			if i < len(colDefs) {
				rowMap[colDefs[i].Name] = col
			}
		}
		// The languageid=<col> hidden column reads the document's stored
		// language id (fts4langid 1.4: SELECT lang_id returns the doc's
		// language, default 0).
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			rowMap[langCol] = &util.ColumnValue{Value: doc.LangID, Affinity: 'I'}
		}
		allRowMaps = append(allRowMaps, rowMap)
	}
	return allRowMaps
}

// ftsContentTableRowMaps builds RowMaps for an FTS4 content=<table> table by
// scanning the external content table (fts3.c fts3ReadExprList: SELECT reads
// column values from the content table, not the index). When docIDs is
// non-nil, only those docids are read (a MATCH query's result set); when nil,
// every content-table row is returned (an unconstrained SELECT returns all
// rows of the content table).
func (e *DDLExecutor) ftsContentTableRowMaps(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, docIDs []int64) []RowMap {
	ct := ftsTable.ContentTable()
	if ct == "" {
		return nil
	}
	ctEntry, _, err := e.ctx.FindTable(ct)
	if err != nil || ctEntry == nil {
		return nil
	}
	// A virtual-table content source (fts4content 9.x: content=e1 where e1 is
	// an echo module) has no b-tree; materialize its rows through the vtab
	// machinery.
	if ctEntry.RootPage == 0 {
		return e.ftsContentVTabRowMaps(ftsTable, colDefs, docIDs, ctEntry)
	}
	return e.scanFTSContentBTreeRows(colDefs, docIDs, ctEntry)
}

// ftsContentTableRowMapsForDocIDs builds RowMaps for a content=<table> FTS
// table's MATCH result: one RowMap per docID (from the INDEX), with values
// read from the external content table when the row exists and empty values
// when it was deleted after indexing (fts3.c fts3Column: a missing content
// row reads as NULL).
func (e *DDLExecutor) ftsContentTableRowMapsForDocIDs(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, docIDs []int64) []RowMap {
	ct := ftsTable.ContentTable()
	ctEntry, _, err := e.ctx.FindTable(ct)
	if err != nil || ctEntry == nil {
		return nil
	}
	// A virtual-table content source (fts4content 9.x) has no b-tree;
	// materialize its rows through the vtab machinery.
	if ctEntry.RootPage == 0 {
		return e.ftsContentVTabRowMaps(ftsTable, colDefs, docIDs, ctEntry)
	}
	ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
	// Map content column positions (excluding docid/rowid) to FTS colDefs by
	// name.
	contentToFTS := ftsContentNameIndex(colDefs, ctDefs)
	tree := e.ctx.TableBTreeForName(ctEntry.Name, ctEntry.RootPage, true)
	rows := e.scanFTSContentRowsByID(colDefs, ctDefs, contentToFTS, tree)
	out := make([]RowMap, 0, len(docIDs))
	for _, id := range docIDs {
		if rm, ok := rows[id]; ok {
			ftsRowMapSetLangID(ftsTable, rm, id)
			out = append(out, rm)
		} else {
			// Deleted content row: still matches, values are empty. The
			// row-key aliases (rowid/docid/oid) are set above and must not be
			// overwritten (colDefs includes the hidden docid vtab column).
			out = append(out, ftsKeyOnlyRowMap(ftsTable, colDefs, id))
		}
	}
	return out
}

// ftsContentVTabRowMaps builds RowMaps for a content=<table> FTS table whose
// content source is a virtual table (fts4content 9.x: content=e1 where e1 is
// an echo module). The vtab's materialized rows are mapped by column name to
// the FTS columns; the vtab row's rowid (the echo source's rowid) becomes the
// docid. When docIDs is non-nil, only matching docids are kept.
func (e *DDLExecutor) ftsContentVTabRowMaps(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, docIDs []int64, ctEntry *schema.Entry) []RowMap {
	rows, err := e.virtualTableRows(ctEntry, 0, "", false)
	if err != nil || rows == nil {
		return nil
	}
	vtDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
	// Collect the content-column names (excluding rowid/docid) in vtab row
	// order.
	valName := ftsContentValueNames(vtDefs)
	want := ftsDocIDSet(docIDs)
	var out []RowMap
	for i, row := range rows {
		// The vtab rowid is the 1-based row index in the materialization (the
		// echo module mirrors the source table's rowids 1..N).
		docID := int64(i + 1)
		if docIDs != nil && !want[docID] {
			continue
		}
		rowMap := make(RowMap)
		ftsKeyAliases(rowMap, docID)
		for vi, name := range valName {
			if vi < len(row) {
				rowMap[name] = row[vi]
			}
		}
		out = append(out, rowMap)
	}
	return out
}

// ftsIndexRowMapsForDocIDs builds RowMaps for a content=<table> or contentless
// FTS table's MATCH result when the content table is unavailable (dropped or
// contentless): one RowMap per docID with only the row-key aliases and NULL
// content columns. docid/rowid queries work off the index without the content
// table (fts4content 7.1.2/7.2.2: SELECT docid FROM ft8 WHERE ft8 MATCH 'N'
// returns 13 15 even with content=nosuchtable); a query that reads a content
// column fails in the caller.
func (e *DDLExecutor) ftsIndexRowMapsForDocIDs(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, docIDs []int64) []RowMap {
	out := make([]RowMap, 0, len(docIDs))
	for _, id := range docIDs {
		out = append(out, ftsKeyOnlyRowMap(ftsTable, colDefs, id))
	}
	return out
}

// ftsRowMapSetLangID sets the languageid=<col> hidden column in an FTS row
// map from the document's stored language id (fts4langid 1.4, 3.4: the
// langid value must be readable — and constrainable — even for index-only
// rows whose content is unavailable).
func ftsRowMapSetLangID(ftsTable *fts.FTS3Table, rm RowMap, docID int64) {
	langCol := ftsTable.LangIDColName()
	if langCol == "" {
		return
	}
	lv := &util.ColumnValue{Value: ftsTable.DocLangID(docID), Affinity: 'I'}
	rm[langCol] = lv
	rm[ftsTable.Name()+"."+langCol] = lv
}

// selectReadsFTSContentColumn reports whether a SELECT references an FTS user
// content column (directly, through *, or inside an expression). Only the
// row-key aliases (rowid/docid/oid/_rowid_ and the table-name hidden column)
// can be read without the content table; everything else needs the stored
// document text (fts4content 6.2.4: SELECT * FROM ft7 with t7 dropped fails
// while SELECT rowid ... works).
func (e *DDLExecutor) selectReadsFTSContentColumn(s *sql.SelectStmt, ftsTable *fts.FTS3Table) bool {
	userCols := make(map[string]bool)
	for _, cn := range ftsTable.ColumnNames() {
		userCols[strings.ToLower(cn)] = true
	}
	readsContent := false
	for _, col := range s.Columns {
		// count(*) does not read any content column: its "*" is the row
		// counter, not a column expansion (fts3corrupt6 2.1: SELECT count(*)
		// over an index-only table succeeds without %_content rows).
		if ftsCountStarOnly(col.Expr) {
			continue
		}
		execquery.WalkExprFull(col.Expr, func(n sql.Expr) {
			if readsContent {
				return
			}
			if ftsNodeReadsContent(n, userCols) {
				readsContent = true
			}
		})
	}
	return readsContent
}

// filterFTSRows applies a WHERE expression to FTS row maps.
func (e *DDLExecutor) filterFTSRows(where sql.Expr, allRowMaps []RowMap) ([]RowMap, error) {
	var filtered []RowMap
	for _, rowMap := range allRowMaps {
		match, err := e.ctx.EvalBool(where, rowMap)
		if err != nil {
			// A malformed MATCH expression or a corrupt-index read fails the
			// statement (fts3expr 2.x "malformed MATCH expression",
			// fts3corrupt4 11.1 "database disk image is malformed"); other
			// evaluation errors keep the row-excluded tolerance.
			es := err.Error()
			if strings.Contains(es, "malformed MATCH expression") || strings.Contains(es, "database disk image is malformed") {
				return nil, err
			}
			continue
		}
		if match {
		}
		if match {
			filtered = append(filtered, rowMap)
		}
	}
	return filtered, nil
}

// contentBtreeCorrupt reports whether the FTS table's %_content shadow btree
// is structurally unreadable (a crash-written page fails ParsePage). A query
// that reads content columns over such a table fails with "database disk
// image is malformed" (fts3corrupt4 52.1: SELECT * FROM t1, t2 — SQLite's
// full scan steps the corrupt content table); index-only queries still work
// (fts3corrupt4 28.1/28.2: MATCH 'h' returns 0 rows).
func (e *DDLExecutor) contentBtreeCorrupt(tableName string) bool {
	ftsTable, ok := e.ctx.FTSTables()[tableName]
	if !ok || ftsTable.Contentless() || ftsTable.ContentTable() != "" {
		return false
	}
	contentEntry, _, err := e.ctx.FindTable(tableName + "_content")
	if err != nil || contentEntry == nil || contentEntry.RootPage == 0 {
		return false
	}
	tree := e.ctx.TableBTreeForName(contentEntry.Name, contentEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return strings.Contains(cerr.Error(), "malformed")
	}
	// Walk the cells too: a corrupt child page surfaces on cursor.Next
	// (fts3corrupt4 52.1's damage is below the root).
	return contentBtreeCellsMalformed(cursor)
}
