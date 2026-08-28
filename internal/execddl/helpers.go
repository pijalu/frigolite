package execddl

import (
	"strconv"

	"github.com/pijalu/frigolite/internal/sql"
)

// constraintColumnNames resolves a table-level UNIQUE/PRIMARY KEY constraint's
// column list to declared column names (handling positional 1-based ordinals).
func constraintColumnNames(tc sql.TableConstraint, colIndex map[string]int, colDefs []sql.ColumnDef) []string {
	var names []string
	for _, ic := range tc.Columns {
		if n, err := strconv.Atoi(ic.Name); err == nil && n >= 1 && n <= len(colDefs) {
			names = append(names, colDefs[n-1].Name)
			continue
		}
		if idx, ok := colIndex[ic.Name]; ok {
			names = append(names, colDefs[idx].Name)
		} else {
			names = append(names, ic.Name)
		}
	}
	return names
}
