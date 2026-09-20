// Package exec implements query execution.
//
// This file holds the content-table row materialization helpers for FTS3/4
// content=<table> tables: b-tree and per-rowid scans mapped by column name
// onto the FTS column list. Extracted from ddl_trigger_tail_part2.go so each
// file stays within the repository's complexity budgets.
package execddl

import (
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// ftsDocIDSet builds the wanted-docid membership set for a docIDs filter. The
// set is only consulted by callers that checked docIDs != nil first (a nil
// docIDs means "keep every row").
func ftsDocIDSet(docIDs []int64) map[int64]bool {
	want := make(map[int64]bool, len(docIDs))
	for _, id := range docIDs {
		want[id] = true
	}
	return want
}

// ftsCursorStop steps the cursor and reports whether row iteration must stop
// (the cursor errored or reached the end of the b-tree).
func ftsCursorStop(cursor *btree.Cursor) bool {
	ok, nerr := cursor.Next()
	return nerr != nil || !ok
}

// nextFTSContentCell reads the cursor's current cell and decodes its record
// payload; ok is false when iteration must stop (a read or decode error, or
// the end of the b-tree).
func nextFTSContentCell(cursor *btree.Cursor) (*storage.Cell, []interface{}, bool) {
	cell, rerr := cursor.ReadCell()
	if rerr != nil || cell == nil {
		return nil, nil, false
	}
	rec, derr := storage.DecodeRecord(cell.Payload)
	if derr != nil || rec == nil {
		return nil, nil, false
	}
	return cell, rec.Values, true
}

// ftsKeyAliases sets the row-key alias columns (rowid/docid/oid) on a fresh
// row map.
func ftsKeyAliases(rowMap RowMap, rowID int64) {
	rowMap["rowid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	rowMap["docid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
	rowMap["oid"] = &util.ColumnValue{Value: rowID, Affinity: 'I'}
}

// ftsContentValueNames lists a vtab content source's value column names in
// row order (rowid/docid excluded).
func ftsContentValueNames(defs []sql.ColumnDef) []string {
	var names []string
	for _, cd := range defs {
		if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
			continue
		}
		names = append(names, cd.Name)
	}
	return names
}

// ftsContentRowMap maps one content-table record onto the FTS column list by
// NAME (fts3.c fts3ReadExprList reads the content table's column matching
// each FTS column name). With content=t1, b the FTS column b maps to the
// content table's b column even when other content columns precede it.
func ftsContentRowMap(rowID int64, ctDefs []sql.ColumnDef, colDefs []sql.ColumnDef, values []interface{}) RowMap {
	rowMap := make(RowMap)
	ftsKeyAliases(rowMap, rowID)
	for ci, cd := range ctDefs {
		if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
			continue
		}
		for _, fcd := range colDefs {
			if strings.EqualFold(fcd.Name, cd.Name) && ci < len(values) {
				rowMap[fcd.Name] = values[ci]
				break
			}
		}
	}
	return rowMap
}

// scanFTSContentBTreeRows walks the content table's b-tree and maps every row
// onto the FTS column list. When docIDs is non-nil only those rowids are
// returned (a MATCH query's result set); when nil every row is returned (an
// unconstrained SELECT reads the whole content table).
func (e *DDLExecutor) scanFTSContentBTreeRows(colDefs []sql.ColumnDef, docIDs []int64, ctEntry *schema.Entry) []RowMap {
	ctDefs := e.ctx.ParseColumnDefs(ctEntry.Name, ctEntry.SQL)
	tree := e.ctx.TableBTreeForName(ctEntry.Name, ctEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil
	}
	want := ftsDocIDSet(docIDs)
	var out []RowMap
	for {
		cell, values, ok := nextFTSContentCell(cursor)
		if !ok {
			break
		}
		if docIDs != nil && !want[cell.RowID] {
			if ftsCursorStop(cursor) {
				break
			}
			continue
		}
		out = append(out, ftsContentRowMap(cell.RowID, ctDefs, colDefs, values))
		if ftsCursorStop(cursor) {
			break
		}
	}
	return out
}

// ftsContentNameIndex maps content column positions (excluding docid/rowid)
// to FTS colDefs indexes by name.
func ftsContentNameIndex(colDefs []sql.ColumnDef, ctDefs []sql.ColumnDef) map[string]int {
	contentToFTS := make(map[string]int)
	for fi, fcd := range colDefs {
		for _, cd := range ctDefs {
			if strings.EqualFold(cd.Name, fcd.Name) {
				contentToFTS[fcd.Name] = fi
			}
		}
	}
	return contentToFTS
}

// ftsContentRowMapByID maps one content-table record onto the FTS columns via
// the content→FTS position index; unmatched content columns are skipped and
// the value position walks non-key columns in declaration order.
func ftsContentRowMapByID(rowID int64, ctDefs []sql.ColumnDef, colDefs []sql.ColumnDef, contentToFTS map[string]int, values []interface{}) RowMap {
	rowMap := make(RowMap)
	ftsKeyAliases(rowMap, rowID)
	vi := 0
	for _, cd := range ctDefs {
		if strings.EqualFold(cd.Name, "docid") || strings.EqualFold(cd.Name, "rowid") {
			continue
		}
		if fi, ok := contentToFTS[cd.Name]; ok && vi < len(values) {
			rowMap[colDefs[fi].Name] = values[vi]
		}
		vi++
	}
	return rowMap
}

// scanFTSContentRowsByID reads the content table's b-tree into a rowid →
// row-map table, mapping content column positions onto the FTS column list by
// name.
func (e *DDLExecutor) scanFTSContentRowsByID(colDefs []sql.ColumnDef, ctDefs []sql.ColumnDef, contentToFTS map[string]int, tree *btree.BTree) map[int64]RowMap {
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return nil
	}
	rows := map[int64]RowMap{}
	for {
		cell, values, ok := nextFTSContentCell(cursor)
		if !ok {
			break
		}
		rows[cell.RowID] = ftsContentRowMapByID(cell.RowID, ctDefs, colDefs, contentToFTS, values)
		if ftsCursorStop(cursor) {
			break
		}
	}
	return rows
}

// ftsKeyOnlyRowMap builds a row map holding only the row-key aliases, NULL
// content columns and the hidden langid column: a deleted content row still
// matches from the index with empty values (fts4content 7.x index-only rows;
// fts3.c fts3Column: a missing content row reads as NULL). The row-key
// aliases must not be overwritten by the column walk (colDefs includes the
// hidden docid vtab column).
func ftsKeyOnlyRowMap(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, id int64) RowMap {
	rm := make(RowMap)
	ftsKeyAliases(rm, id)
	for _, fcd := range colDefs {
		if fcd.Name == "rowid" || fcd.Name == "docid" || fcd.Name == "oid" || fcd.Name == "_rowid_" {
			continue
		}
		rm[fcd.Name] = nil
	}
	ftsRowMapSetLangID(ftsTable, rm, id)
	return rm
}

// ftsCountStarOnly reports whether expr is count(*): its "*" is the row
// counter, not a column expansion (fts3corrupt6 2.1: SELECT count(*) over an
// index-only table succeeds without %_content rows).
func ftsCountStarOnly(expr sql.Expr) bool {
	fc, ok := expr.(*sql.FuncCall)
	if !ok || !strings.EqualFold(fc.Name, "count") {
		return false
	}
	if len(fc.Args) != 1 {
		return false
	}
	ref, ok := fc.Args[0].(*sql.ColumnRef)
	return ok && ref.Name == "*"
}

// ftsNodeReadsContent reports whether one expression node reads an FTS
// content column: snippet()/offsets() read the document's content columns
// (fts3_snippet.c reads the content row for the column text), and a bare "*"
// expands to every user column. When the FTS table has no derived columns (a
// content=<table> whose content table was missing at connection time) SELECT *
// still needs the content table to resolve its column list (fts4content
// 6.2.4).
func ftsNodeReadsContent(n sql.Expr, userCols map[string]bool) bool {
	if fc, ok := n.(*sql.FuncCall); ok {
		upper := strings.ToUpper(fc.Name)
		if upper == "SNIPPET" || upper == "OFFSETS" {
			return true
		}
	}
	ref, ok := n.(*sql.ColumnRef)
	if !ok {
		return false
	}
	if ref.Name == "*" {
		return true
	}
	return userCols[strings.ToLower(ref.Name)]
}

// contentBtreeCellsMalformed walks a %_content cursor's cells and reports
// whether any step surfaces structural corruption ("malformed"); end-of-tree
// is normal termination.
func contentBtreeCellsMalformed(cursor *btree.Cursor) bool {
	for {
		if _, rerr := cursor.ReadCell(); rerr != nil && !strings.Contains(rerr.Error(), "cursor at end") {
			return strings.Contains(rerr.Error(), "malformed")
		}
		ok, nerr := cursor.Next()
		if nerr != nil {
			return strings.Contains(nerr.Error(), "malformed")
		}
		if !ok {
			break
		}
	}
	return false
}
