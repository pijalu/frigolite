// Package exec — derived-table boundary helpers.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// buildSubqueryRowMaps builds right-side RowMaps from a subquery result. When
// the subquery already produced row maps (it joined internally), reuses them;
// otherwise wraps each projected value with its column affinity.
const joinGroupRowidAliasKey = "__frigolite_join_group_rowid_alias__"

func (e *SelectEngine) buildSubqueryRowMaps(subqResult *Result, rightDefs []sql.ColumnDef, subquery *sql.SelectStmt, tableName string, synthetic bool) []RowMap {
	if len(subqResult.rowMaps) > 0 && len(subqResult.rowMaps) == len(subqResult.Rows) {
		maps := projectSubqueryRowMaps(subqResult.rowMaps, rightDefs)
		for rowIndex, row := range subqResult.Rows {
			maps[rowIndex][positionalRowKey] = row
			for colIndex, value := range row {
				if colIndex >= len(rightDefs) {
					continue
				}
				def := rightDefs[colIndex]
				maps[rowIndex][def.Name] = wrapAffinityCollated(def, value)
			}
		}
		return maps
	}
	subqAff := subqueryColumnAffinities(subquery)
	var rightMaps []RowMap
	for _, row := range subqResult.Rows {
		rightRowMap := make(RowMap)
		for i, val := range row {
			if i >= len(rightDefs) {
				continue
			}
			aff := e.subqueryColumnAffinity(subqAff, i, rightDefs[i], subquery)
			cv := &util.ColumnValue{Value: util.UnwrapColumnValue(val), Affinity: aff}
			rightRowMap[rightDefs[i].Name] = cv
			if synthetic {
				rightRowMap[tableName+"."+rightDefs[i].Name] = val
			}
		}
		rightMaps = append(rightMaps, rightRowMap)
	}
	return rightMaps
}

func mergeRightUsingValues(leftValues []interface{}, row RowMap, leftDefs, rightDefs []sql.ColumnDef, join sql.JoinClause, naturalCols map[string]bool, leftTableName string) {
	names := make(map[string]bool, len(join.Using)+len(naturalCols))
	for _, name := range join.Using {
		names[strings.ToLower(name)] = true
	}
	for name := range naturalCols {
		names[strings.ToLower(name)] = true
	}
	positional, _ := row["__frigolite_join_group_values__"].([]interface{})
	for leftIndex, def := range leftDefs {
		name := strings.ToLower(unprefixedColName(def.Name))
		if !names[name] || leftIndex >= len(leftValues) || leftValues[leftIndex] != nil {
			continue
		}
		if leftTableName != "" {
			if value, ok := row.Get(leftTableName + "." + unprefixedColName(def.Name)); ok && util.UnwrapColumnValue(value) != nil {
				continue
			}
		}
		for rightIndex, rightDef := range rightDefs {
			if !strings.EqualFold(unprefixedColName(rightDef.Name), name) {
				continue
			}
			if rightIndex < len(positional) {
				leftValues[leftIndex] = positional[rightIndex]
			} else if value, ok := row.Get(rightDef.Name); ok {
				leftValues[leftIndex] = util.UnwrapColumnValue(unwrapCollatedValue(value))
			}
			break
		}
	}
}

func filteredUsingColumnIndices(defs []sql.ColumnDef, join sql.JoinClause, naturalCols map[string]bool, usingJoin bool) []int {
	using := make(map[string]bool, len(join.Using)+len(naturalCols))
	if usingJoin {
		for _, name := range join.Using {
			using[strings.ToLower(name)] = true
		}
		for name := range naturalCols {
			using[strings.ToLower(name)] = true
		}
	}
	merged := make(map[string]bool, len(using))
	var indices []int
	for i, def := range defs {
		name := strings.ToLower(def.Name)
		if using[name] && !merged[name] {
			merged[name] = true
			continue
		}
		indices = append(indices, i)
	}
	return indices
}

func joinSideValuesByIndices(row RowMap, positionalKey string, defs []sql.ColumnDef, indices []int, tableName string) []interface{} {
	positional, hasPositional := row[positionalKey].([]interface{})
	values := make([]interface{}, 0, len(indices))
	for _, index := range indices {
		if index >= len(defs) || defs[index].Dropped || IsHiddenColumnDef(defs[index]) {
			continue
		}
		visibleIndex := 0
		for i := 0; i < index; i++ {
			if !defs[i].Dropped && !IsHiddenColumnDef(defs[i]) {
				visibleIndex++
			}
		}
		if hasPositional && visibleIndex < len(positional) {
			values = append(values, positional[visibleIndex])
			continue
		}
		name := defs[index].Name
		value, ok := row.Get(tableName + "." + name)
		if !ok {
			value, _ = row.Get(name)
		}
		values = append(values, util.UnwrapColumnValue(unwrapCollatedValue(value)))
	}
	return values
}

func joinSideValues(row RowMap, positionalKey string, defs []sql.ColumnDef, tableName string) []interface{} {
	if positional, ok := row[positionalKey].([]interface{}); ok && len(positional) == len(defs) {
		return positional
	}
	values := make([]interface{}, 0, len(defs))
	for _, def := range defs {
		if def.Dropped || IsHiddenColumnDef(def) {
			continue
		}
		var value interface{}
		var ok bool
		if tableName != "" {
			value, ok = row.Get(tableName + "." + def.Name)
		}
		if !ok {
			value, _ = row.Get(def.Name)
		}
		values = append(values, util.UnwrapColumnValue(unwrapCollatedValue(value)))
	}
	return values
}

// projectSubqueryRowMaps restricts reused subquery row maps to the surface a
// materialized subquery exposes to its enclosing statement (select.c keeps a
// co-routine's visible columns to exactly its result list): the output column
// names, plus internal qualified keys needed by parenthesized join groups.
func projectSubqueryRowMaps(maps []RowMap, rightDefs []sql.ColumnDef) []RowMap {
	keep := make(map[string]bool, len(rightDefs)*2)
	for _, cd := range rightDefs {
		keep[cd.Name] = true
		keep[strings.ToLower(cd.Name)] = true
	}
	out := make([]RowMap, len(maps))
	for i, m := range maps {
		nm := make(RowMap, len(m))
		for k, v := range m {
			if keep[k] || k == positionalRowKey || k == "__frigolite_join_group_values__" || strings.Contains(k, ".") {
				nm[k] = v
			}
		}
		out[i] = nm
	}
	return out
}
