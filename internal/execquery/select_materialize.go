package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

func materializedAlias(s *sql.SelectStmt) string {
	if s.From.Subquery != nil && s.From.As != "" {
		return s.From.As
	}
	if isPragmaTableFunc(s.From.Name) && s.From.As != "" {
		return s.From.As
	}
	if s.From.As != "" {
		return s.From.As
	}
	return ""
}

// viewColDefsFromResult builds view column definitions from the view result,
// preferring typed defs (viewColumnDefs) for affinity, then the declared
// column list (CREATE VIEW v(c0, c1) AS ...), then the result's column names.
func (e *SelectEngine) viewColDefsFromResult(viewEntry *schema.Entry, viewResult *Result) ([]sql.ColumnDef, error) {
	typed, terr := e.ctx.ViewColumnDefs(viewEntry)
	if terr != nil {
		return nil, terr
	}
	if len(typed) > 0 {
		applyDeclaredViewNames(typed, viewEntry.SQL)
		return typed, nil
	}
	if declared := ViewDeclaredColumns(viewEntry.SQL); len(declared) > 0 {
		return declaredColumnsAsDefs(declared), nil
	}
	return resultColumnsAsDefs(viewResult.Columns), nil
}

// applyDeclaredViewNames overrides the first N typed column names with the
// view's declared column list, appending any extras.
func applyDeclaredViewNames(viewColDefs []sql.ColumnDef, viewSQL string) {
	for i, colName := range ViewDeclaredColumns(viewSQL) {
		if i < len(viewColDefs) {
			viewColDefs[i].Name = colName
		} else {
			viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
		}
	}
}

// declaredColumnsAsDefs converts a declared column name list to ColumnDefs.
func declaredColumnsAsDefs(declared []string) []sql.ColumnDef {
	viewColDefs := make([]sql.ColumnDef, 0, len(declared))
	for _, colName := range declared {
		viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
	}
	return viewColDefs
}

// resultColumnsAsDefs converts result column names to ColumnDefs.
func resultColumnsAsDefs(colNames []string) []sql.ColumnDef {
	viewColDefs := make([]sql.ColumnDef, 0, len(colNames))
	for _, colName := range colNames {
		viewColDefs = append(viewColDefs, sql.ColumnDef{Name: colName})
	}
	return viewColDefs
}

// viewRowMapsFromResult builds RowMaps from view result rows for expression
// evaluation, wrapping each value with its column affinity and adding qualified
// keys (viewQual.col). The positional row is retained for SELECT *.
func viewRowMapsFromResult(rows [][]interface{}, viewColDefs []sql.ColumnDef, viewQual string) []RowMap {
	var rowMaps []RowMap
	for _, row := range rows {
		rowMap := make(RowMap)
		for i, val := range row {
			if i >= len(viewColDefs) {
				continue
			}
			cd := viewColDefs[i]
			aff := util.Affinity(cd.Type)
			cv := &util.ColumnValue{Value: val, Affinity: aff}
			var mapped interface{} = cv
			if coll := cd.Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
				mapped = &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
			}
			rowMap[cd.Name] = mapped
			rowMap[viewQual+"."+cd.Name] = mapped
		}
		rowMap[positionalRowKey] = row
		rowMaps = append(rowMaps, rowMap)
	}
	return rowMaps
}

// buildMaterializedRowMaps builds RowMaps from materialized rows (subquery-in-
// FROM, pragma table-valued functions, virtual-table functions), wrapping each
// value with its column affinity and adding qualified alias keys + implicit rowids.
func buildMaterializedRowMaps(s *sql.SelectStmt, colDefs []sql.ColumnDef, allRows [][]interface{}, rowids []int64) []RowMap {
	allRowMaps := make([]RowMap, len(allRows))
	subqAff := subqueryColumnAffinities(s)
	alias := materializedAlias(s)
	for i, row := range allRows {
		rowMap := make(RowMap)
		for j, val := range row {
			if j >= len(colDefs) {
				continue
			}
			rowMap[colDefs[j].Name] = materializedValue(val, j, subqAff, colDefs)
			if alias != "" {
				rowMap[alias+"."+colDefs[j].Name] = rowMap[colDefs[j].Name]
			}
			for _, q := range materializedQualifiers(s) {
				rowMap[q+"."+colDefs[j].Name] = rowMap[colDefs[j].Name]
			}
		}
		addMaterializedRowID(rowMap, alias, i, rowids)
		allRowMaps[i] = rowMap
	}
	return allRowMaps
}

// materializedQualifiers returns extra table-name qualifiers (besides the
// explicit alias) under which a materialized FROM source's columns resolve:
// its own relation name — "rt0.b" over a created vtab, or the table-valued
// function's own name ("json_tree.value").
func materializedQualifiers(s *sql.SelectStmt) []string {
	name := s.From.Name
	if name == "" {
		return nil
	}
	lower := strings.ToLower(name)
	if lower == name {
		return []string{name}
	}
	return []string{name, lower}
}

// materializedValue wraps a materialized row value with its affinity and
// collation for expression evaluation.
func materializedValue(val interface{}, j int, subqAff []rune, colDefs []sql.ColumnDef) interface{} {
	aff := 'B'
	if j < len(subqAff) && subqAff[j] != 0 {
		aff = subqAff[j]
	} else {
		aff = util.Affinity(colDefs[j].Type)
	}
	cv := &util.ColumnValue{Value: val, Affinity: aff}
	if coll := colDefs[j].Collate; coll != "" && !strings.EqualFold(coll, "BINARY") {
		return &CollatedValue{Value: cv, Collation: strings.ToUpper(coll)}
	}
	return cv
}

// addMaterializedRowID adds an implicit rowid (and qualified alias key) to a
// materialized row map when it has no explicit rowid column. rowids carries
// the source's native rowids (virtual-table xRowid parity, e.g.
// generate_series rows have rowid == value); nil falls back to 1-based
// positions.
func addMaterializedRowID(rowMap RowMap, alias string, i int, rowids []int64) {
	if _, hasRowIDCol := rowMap["rowid"]; hasRowIDCol {
		return
	}
	var rid int64
	if i < len(rowids) {
		rid = rowids[i]
	} else {
		rid = int64(i + 1)
	}
	rowMap["rowid"] = rid
	if alias != "" {
		rowMap[alias+".rowid"] = rid
	}
}
