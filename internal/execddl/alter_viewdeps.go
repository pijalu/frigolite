package execddl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/parse"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
)

func skipViewSelectToken(col string, i int, viewCols []string, fromTables []string) bool {
	// An identifier following AS is an output alias, not a column reference.
	if i > 0 && strings.EqualFold(strings.TrimSpace(viewCols[i-1]), "AS") {
		return true
	}
	if strings.Contains(col, "(") || strings.Contains(col, ")") {
		return true
	}
	upperCol := strings.ToUpper(col)
	if isViewSelectKeyword(upperCol) {
		return true
	}
	if _, err := strconv.ParseFloat(upperCol, 64); err == nil {
		return true
	}
	// Skip tokens that are FROM table names (e.g. e1 in e1.* — a
	// table-qualified wildcard reference, not a column).
	return fromTablesContains(fromTables, upperCol)
}

// isViewSelectKeyword reports whether the token is a SELECT keyword that is
// not a column reference.
func isViewSelectKeyword(upperCol string) bool {
	switch upperCol {
	case "DISTINCT", "ALL", "AS", "ORDER", "BY", "COLLATE":
		return true
	}
	return false
}

// fromTablesContains reports whether upperCol matches any FROM table name.
func fromTablesContains(fromTables []string, upperCol string) bool {
	for _, ft := range fromTables {
		if strings.EqualFold(ft, upperCol) {
			return true
		}
	}
	return false
}

// normalizeViewColRef reduces a bare identifier token to a column name,
// stripping table qualifiers and trailing dots ("e1.*" -> "e1").
func normalizeViewColRef(upperCol string) string {
	if strings.Contains(upperCol, ".") {
		parts := strings.Split(upperCol, ".")
		if len(parts) == 2 {
			if parts[1] == "" {
				upperCol = parts[0]
			} else {
				upperCol = parts[1]
			}
		}
	}
	return strings.TrimSuffix(upperCol, ".")
}

// checkViewRenameAmbiguity reports whether a renamed column reference becomes
// ambiguous because the new name also exists in another FROM table.
func (e *DDLExecutor) checkViewRenameAmbiguity(view *schema.Entry, fromTables []string, tableCols map[string]map[string]bool, tableName, oldColName, newColName, upperCol string) *Result {
	if oldColName != "" && strings.EqualFold(upperCol, oldColName) &&
		newColName != "" && newColName != oldColName {
		count := 1 // the renamed table will have the new column after rename
		for _, ft := range fromTables {
			if strings.EqualFold(ft, tableName) {
				continue
			}
			if cols, ok := tableCols[strings.ToUpper(ft)]; ok && cols[strings.ToUpper(newColName)] {
				count++
			}
		}
		if count > 1 {
			return &Result{Error: fmt.Errorf("error in view %s after rename: ambiguous column name: %s", view.Name, newColName)}
		}
	}
	return nil
}

// splitFromTables splits a FROM clause into its table names, handling commas
// ("FROM t1, t2") and stopping at the first keyword that ends the list
// (WHERE, JOIN, GROUP, ORDER, LIMIT, etc.).
func splitFromTables(fromRest string) []string {
	// Stop at common clause keywords.
	stopIdx := len(fromRest)
	for _, kw := range []string{" WHERE ", " JOIN ", " GROUP ", " ORDER ", " LIMIT ", " HAVING ", " ON "} {
		if idx := strings.Index(fromRest, kw); idx >= 0 && idx < stopIdx {
			stopIdx = idx
		}
	}
	fromRest = fromRest[:stopIdx]
	parts := strings.Split(fromRest, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Strip trailing AS alias and schema prefixes.
		fields := strings.Fields(p)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if dotIdx := strings.Index(name, "."); dotIdx >= 0 {
			name = name[dotIdx+1:]
		}
		out = append(out, name)
	}
	return out
}

// collectColumnRefs collects column references from a SELECT statement.
//
//lint:ignore U1000  Utility for future use
func collectColumnRefs(sel *sql.SelectStmt) []string {
	var refs []string
	for _, col := range sel.Columns {
		collectExprRefs(col.Expr, &refs)
	}
	if sel.Where != nil {
		collectExprRefs(sel.Where, &refs)
	}
	return refs
}

// checkTriggerDependencies checks if any triggers on the table reference the dropped column.
func (e *DDLExecutor) checkTriggerDependencies(tableName, columnName string) *Result {
	triggers, err := e.ctx.Schema().FindTriggersForTable(tableName)
	if err != nil {
		return nil
	}
	for _, trig := range triggers {
		// Check if the trigger body references the dropped column
		upperSQL := strings.ToUpper(trig.SQL)
		upperCol := strings.ToUpper(columnName)
		// Check for NEW.column and OLD.column references
		if strings.Contains(upperSQL, "NEW."+upperCol) || strings.Contains(upperSQL, "OLD."+upperCol) {
			// The trigger references the column being dropped
			// Extract the trigger's SQL to find other issues
			errMsg := e.validateTriggerSQL(trig.SQL)
			if errMsg != "" {
				return &Result{Error: fmt.Errorf("error in trigger %s: %s", trig.Name, errMsg)}
			}
		}
	}
	return nil
}

// isAlpha checks if a byte is an ASCII letter.
func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// reference the given column and returns an error if so.
func (e *DDLExecutor) checkTableConstraintDependencies(createSQL, tableName, columnName string) *Result {
	stmts, perr := parse.ParseSQL(createSQL)
	if perr != nil || len(stmts) == 0 {
		return nil
	}
	ct, ok := stmts[0].(*sql.CreateTableStmt)
	if !ok || ct == nil {
		return nil
	}
	// Check table-level constraints (not column-level)
	for _, tc := range ct.Constraints {
		if tc.Type == sql.ConstraintCheck && tc.Expr != nil {
			if exprReferencesColumn(tc.Expr, columnName) {
				return &Result{Error: fmt.Errorf("error in table %s after drop column: no such column: %s",
					tableName, columnName)}
			}
		}
	}
	return nil
}
