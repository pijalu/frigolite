// Package execquery implements SELECT execution.
// This file holds FTS-table helpers for the generic join pipeline: row maps
// built from the in-memory FTS index (with the docid as rowid) so MATCH
// evaluation and column resolution work over combined join rows.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// ftsJoinRowMaps builds RowMaps for every document in an FTS table, mirroring
// the DDL executor's ftsRowMaps: rowid/docid aliases carry the real docid and
// each user column is stored under its declared name (plus a qualified
// table.col key). Used when an FTS table participates in a JOIN, where the
// generic pipeline needs per-row maps that MATCH evaluation can resolve.
func (e *SelectEngine) ftsJoinRowMaps(ftsTable *fts.FTS3Table, colDefs []sql.ColumnDef, tableName string) []RowMap {
	var allRowMaps []RowMap
	for _, docID := range ftsTable.AllRowsMap() {
		doc := ftsTable.GetDoc(docID)
		if doc == nil {
			continue
		}
		rowMap := make(RowMap)
		rowMap["rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		rowMap["docid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		rowMap[tableName+".rowid"] = &util.ColumnValue{Value: docID, Affinity: 'I'}
		for i, col := range doc.Columns {
			if i < len(colDefs) {
				rowMap[colDefs[i].Name] = col
				rowMap[tableName+"."+colDefs[i].Name] = col
			}
		}
		// The languageid=<col> hidden column reads the document's stored
		// language id (fts4langid 1.4).
		if langCol := ftsTable.LangIDColName(); langCol != "" {
			lv := &util.ColumnValue{Value: doc.LangID, Affinity: 'I'}
			rowMap[langCol] = lv
			rowMap[tableName+"."+langCol] = lv
		}
		allRowMaps = append(allRowMaps, rowMap)
	}
	return allRowMaps
}

// rowMapToValues converts a RowMap into a flat value slice ordered by colDefs,
// for the initial allRows passed to the post-scan pipeline. The join path
// rebuilds output rows from the combined row maps, so this only needs to
// produce a reasonable placeholder consistent with the base scan's shape.
func rowMapToValues(rowMap RowMap, colDefs []sql.ColumnDef) []interface{} {
	out := make([]interface{}, len(colDefs))
	for i, cd := range colDefs {
		if v, ok := rowMap[cd.Name]; ok {
			out[i] = v
		}
	}
	return out
}

// ftsReadsContentColumns reports whether the SELECT's output columns include
// the FTS table's user columns (a bare "*" or any column reference), so the
// scan needs the document text from %_content.
func (e *SelectEngine) ftsReadsContentColumns(s *sql.SelectStmt, ftsTable *fts.FTS3Table) bool {
	userCols := make(map[string]bool)
	for _, cn := range ftsTable.ColumnNames() {
		userCols[strings.ToLower(cn)] = true
	}
	reads := false
	for _, col := range s.Columns {
		WalkExprFull(col.Expr, func(n sql.Expr) {
			if reads {
				return
			}
			if ref, ok := n.(*sql.ColumnRef); ok {
				if ref.Name == "*" || userCols[strings.ToLower(ref.Name)] {
					reads = true
				}
			}
		})
	}
	return reads
}

// contentBtreeCorrupt reports whether the FTS table's %_content shadow btree
// is structurally unreadable (a crash-written page fails page parsing).
func (e *SelectEngine) contentBtreeCorrupt(tableName string) bool {
	contentEntry, _, err := e.ctx.FindTable(tableName + "_content")
	if err != nil || contentEntry == nil || contentEntry.RootPage == 0 {
		return false
	}
	tree := e.ctx.TableBTreeForName(contentEntry.Name, contentEntry.RootPage, true)
	cursor, cerr := tree.OpenCursor()
	if cerr != nil {
		return strings.Contains(cerr.Error(), "malformed")
	}
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
