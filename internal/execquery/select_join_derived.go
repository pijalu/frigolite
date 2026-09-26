// Derived-table materialization: the boundary between a subquery's internal
// row state and the surface it exposes to an enclosing statement.

package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// buildSubqueryRowMaps builds right-side RowMaps from a subquery result. When
// the subquery already produced row maps (it joined internally), reuses them;
// otherwise wraps each projected value with its column affinity.
func (e *SelectEngine) buildSubqueryRowMaps(subqResult *Result, rightDefs []sql.ColumnDef, subquery *sql.SelectStmt, tableName string, synthetic bool) []RowMap {
	if len(subqResult.rowMaps) > 0 && len(subqResult.rowMaps) == len(subqResult.Rows) {
		return projectSubqueryRowMaps(subqResult.rowMaps, rightDefs)
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
				// Also store under the synthetic qualified key so the USING ON
				// clause (id = _subq.id) can match the right side independently.
				rightRowMap[tableName+"."+rightDefs[i].Name] = val
			}
		}
		rightMaps = append(rightMaps, rightRowMap)
	}
	return rightMaps
}

// projectSubqueryRowMaps restricts reused subquery row maps to the surface a
// materialized subquery exposes to its enclosing statement (select.c keeps a
// co-routine's visible columns to exactly its result list): the output column
// names, plus internal qualified keys ("t4.a") that a parenthesized join
// group's outer references resolve through. Anything else is statement-
// internal state — e.g. a window pass merges unprojected source columns into
// its row maps so a trailing ORDER BY can cite them — and must not leak: a
// non-output key like "b" would shadow a same-named output column of another
// FROM source in the enclosing statement (unionall-4.3).
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
			if keep[k] || strings.Contains(k, ".") {
				nm[k] = v
			}
		}
		out[i] = nm
	}
	return out
}
