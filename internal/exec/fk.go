package exec

import (
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// fkCascadeRef describes a column-level FOREIGN KEY with ON DELETE CASCADE:
// childTable.childCol references parentTable.parentCol.
type fkCascadeRef struct {
	childTable  string
	childCol    string
	parentTable string
	parentCol   string
}

// fkRefRe parses "parentTable(parentCol) ON DELETE CASCADE" from a column's
// References string.
var fkRefRe = regexp.MustCompile(`(?is)^\s*([^\s(]+)\(([^)]+)\)\s+ON\s+DELETE\s+CASCADE`)

// fkCascadeRefs returns the FK CASCADE references whose parent is the given
// table (cached per parent table; invalidated by invalidateTableCaches).
func (e *Engine) fkCascadeRefs(parentTable string) []fkCascadeRef {
	if e.fkCache == nil {
		e.fkCache = make(map[string][]fkCascadeRef)
	}
	if refs, ok := e.fkCache[parentTable]; ok {
		return refs
	}
	var refs []fkCascadeRef
	for _, ctx := range e.databases {
		entries, err := ctx.Schema.GetEntries(schema.TypeTable)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if ent.Name == "sqlite_schema" || ent.Name == "sqlite_master" {
				continue
			}
			colDefs := e.parseColumnDefs(ent.Name, ent.SQL)
			for _, cd := range colDefs {
				m := fkRefRe.FindStringSubmatch(cd.References)
				if m == nil {
					continue
				}
				if strings.EqualFold(m[1], parentTable) {
					refs = append(refs, fkCascadeRef{
						childTable:  ent.Name,
						childCol:    cd.Name,
						parentTable: m[1],
						parentCol:   strings.TrimSpace(m[2]),
					})
				}
			}
		}
	}
	e.fkCache[parentTable] = refs
	return refs
}

// cascadeDelete deletes child rows whose FK column matches the deleted parent
// row's referenced column value (ON DELETE CASCADE), firing BEFORE/AFTER
// DELETE triggers for each child row.
func (e *Engine) cascadeDelete(parentTable *schema.Entry, parentColDefs []sql.ColumnDef, parentValues []interface{}) *Result {
	refs := e.fkCascadeRefs(parentTable.Name)
	if len(refs) == 0 {
		return &Result{}
	}
	parentIndex := buildColumnIndex(parentColDefs)
	for _, ref := range refs {
		parentIdx, ok := parentIndex[ref.parentCol]
		if !ok || parentIdx >= len(parentValues) || parentValues[parentIdx] == nil {
			continue
		}
		parentVal := parentValues[parentIdx]
		childEntry, err := e.schema.FindTable(ref.childTable)
		if err != nil {
			continue
		}
		childColDefs := e.parseColumnDefs(childEntry.Name, childEntry.SQL)
		childIndex := buildColumnIndex(childColDefs)
		childIdx, ok := childIndex[ref.childCol]
		if !ok {
			continue
		}
		tree := e.tableBTree(childEntry.Name, childEntry.RootPage, true)
		cursor, err := tree.OpenCursor()
		if err != nil {
			continue
		}
		type match struct {
			rowID  int64
			values []interface{}
		}
		var matches []match
		for {
			cell, err := cursor.ReadCell()
			if err != nil || cell == nil {
				break
			}
			rec, err := storage.DecodeRecord(cell.Payload)
			if err != nil || rec == nil {
				break
			}
			if childIdx < len(rec.Values) && rec.Values[childIdx] != nil && util.CompareValues(rec.Values[childIdx], parentVal) == 0 {
				matches = append(matches, match{cell.RowID, rec.Values})
			}
			ok, err := cursor.Next()
			if err != nil || !ok {
				break
			}
		}
		hasTriggers := e.hasTriggersForTable(childEntry.Name)
		for _, m := range matches {
			oldRow := buildRowMapFromValues(m.values, childColDefs, m.rowID)
			if hasTriggers {
				if trigResult := e.fireBeforeDeleteTriggers(childEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
			if _, err := tree.DeleteCellsWhere(func(cell *storage.Cell) bool {
				return cell.RowID == m.rowID
			}); err != nil {
				return &Result{Error: err}
			}
			e.invalidateRowIDCache(childEntry.RootPage)
			if hasTriggers {
				if trigResult := e.fireAfterDeleteTriggers(childEntry.Name, oldRow); trigResult.Error != nil {
					return trigResult
				}
			}
		}
	}
	return &Result{}
}
