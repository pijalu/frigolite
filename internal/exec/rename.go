// Package exec implements query execution.
package exec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// RenameContext tracks the rename operation state.
type RenameContext struct {
	OldName   string // the name being replaced
	NewName   string // the replacement name
	QuotedNew string // NewName, double-quoted if needed
	IsTable   bool   // true = rename table, false = rename column
	TableName string // for column renames: which table's column
}

// RenameRange represents a byte range in the original SQL text to replace.
type RenameRange struct {
	Start int
	End   int
}

// FindRenameTokens parses SQL text and returns all byte ranges that should be
// replaced when renaming a table or column. Each range is (start, end) in the original text.
//
// For RENAME TABLE (ctx.IsTable=true):
//   - Finds all TableRef nodes where Name matches OldName
//   - Finds all ColumnRef nodes with Table qualifier matching OldName
//   - Finds trigger ON table names matching OldName
//
// For RENAME COLUMN (ctx.IsTable=false):
//   - Finds all ColumnRef nodes where Name matches OldName AND
//     (Table qualifier is empty or matches ctx.TableName)
func FindRenameTokens(sqlText string, ctx *RenameContext) ([]RenameRange, error) {
	parser := sql.NewParser(sqlText)
	stmts := parser.Parse()
	if parser.Err() != nil {
		return nil, fmt.Errorf("error parsing SQL: %w", parser.Err())
	}

	var ranges []RenameRange

	for _, stmt := range stmts {
		collectRanges(stmt, ctx, &ranges)
	}

	// Deduplicate overlapping ranges
	ranges = deduplicateRanges(ranges)

	return ranges, nil
}

// collectRanges walks a statement and collects rename ranges matching the context.
func collectRanges(stmt sql.Stmt, ctx *RenameContext, ranges *[]RenameRange) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		collectSelectRanges(s, ctx, ranges)
	case *sql.InsertStmt:
		if s.Select != nil {
			collectSelectRanges(s.Select, ctx, ranges)
		}
	case *sql.UpdateStmt:
		// UPDATE table SET ... — the table name
		if ctx.IsTable && strings.EqualFold(s.Table, ctx.OldName) {
			// For UPDATE, we need the table reference position.
			// The update target table name comes from the parser, but UpdateStmt
			// doesn't have TokenInfo on its Table field yet. Fall back to no-op
			// for now — the string replacement will handle this.
		}
	case *sql.DeleteStmt:
		if ctx.IsTable && strings.EqualFold(s.Table, ctx.OldName) {
			// Same as UPDATE — no TokenInfo on DeleteStmt.Table yet.
		}
	case *sql.CreateTriggerStmt:
		// Trigger ON table name
		if ctx.IsTable && strings.EqualFold(s.Table, ctx.OldName) && s.TableTok.Start != 0 {
			*ranges = append(*ranges, RenameRange{Start: s.TableTok.Start, End: s.TableTok.End})
		}
		// Walk trigger body statements
		for _, bodyStmt := range s.Statements {
			collectRanges(bodyStmt, ctx, ranges)
		}
	case *sql.CreateViewStmt:
		if s.Select != nil {
			collectSelectRanges(s.Select, ctx, ranges)
		}
	case *sql.CreateIndexStmt:
		if ctx.IsTable && strings.EqualFold(s.Table, ctx.OldName) && s.TableTok.Start != 0 {
			*ranges = append(*ranges, RenameRange{Start: s.TableTok.Start, End: s.TableTok.End})
		}
	case *sql.CreateTableStmt:
		// CREATE TABLE name — rename the table name in its own CREATE SQL
		if ctx.IsTable && strings.EqualFold(s.Name, ctx.OldName) && s.NameTok.Start != 0 {
			*ranges = append(*ranges, RenameRange{Start: s.NameTok.Start, End: s.NameTok.End})
		}
	}
}

// collectSelectRanges walks a SELECT statement for rename ranges.
func collectSelectRanges(sel *sql.SelectStmt, ctx *RenameContext, ranges *[]RenameRange) {
	if sel == nil {
		return
	}

	// FROM clause table reference
	collectTableRefRange(sel.From, ctx, ranges)

	// JOIN clauses
	for _, join := range sel.Joins {
		collectTableRefRange(join.Table, ctx, ranges)
	}

	// WHERE, HAVING expressions
	if sel.Where != nil {
		collectExprRange(sel.Where, ctx, ranges)
	}
	if sel.Having != nil {
		collectExprRange(sel.Having, ctx, ranges)
	}

	// GROUP BY expressions
	for _, expr := range sel.GroupBy {
		collectExprRange(expr, ctx, ranges)
	}

	// ORDER BY expressions
	for _, ob := range sel.OrderBy {
		collectExprRange(ob.Expr, ctx, ranges)
	}

	// SELECT columns
	for _, col := range sel.Columns {
		collectExprRange(col.Expr, ctx, ranges)
	}

	// UNION subquery
	if sel.Union != nil {
		collectSelectRanges(sel.Union, ctx, ranges)
	}

	// CTEs
	for _, cte := range sel.CTEs {
		if cte.Select != nil {
			collectSelectRanges(cte.Select, ctx, ranges)
		}
	}
}

// collectTableRefRange checks a TableRef for matching rename targets.
func collectTableRefRange(ref sql.TableRef, ctx *RenameContext, ranges *[]RenameRange) {
	if ref.Name == "" {
		return
	}

	// Strip schema prefix for comparison
	compareName := ref.Name
	if dotIdx := strings.LastIndex(compareName, "."); dotIdx >= 0 {
		compareName = compareName[dotIdx+1:]
	}

	if ctx.IsTable && strings.EqualFold(compareName, ctx.OldName) {
		// For schema-qualified names, NameTok covers the last part (the table name itself)
		if ref.NameTok.Start != 0 || ref.NameTok.End != 0 {
			*ranges = append(*ranges, RenameRange{Start: ref.NameTok.Start, End: ref.NameTok.End})
		} else {
			// Fallback: if no TokenInfo, try to find it in the full name
			// This handles schema-qualified names where we only want the last part
			if dotIdx := strings.LastIndex(ref.Name, "."); dotIdx >= 0 {
				// Can't determine position without TokenInfo
				return
			}
		}
	}

	// Walk subquery
	if ref.Subquery != nil {
		collectSelectRanges(ref.Subquery, ctx, ranges)
	}
}

// collectExprRange walks an expression tree for rename targets.
func collectExprRange(expr sql.Expr, ctx *RenameContext, ranges *[]RenameRange) {
	if expr == nil {
		return
	}

	switch e := expr.(type) {
	case *sql.ColumnRef:
		collectColumnRefRange(e, ctx, ranges)
	case *sql.BinaryOp:
		collectExprRange(e.Left, ctx, ranges)
		collectExprRange(e.Right, ctx, ranges)
	case *sql.UnaryOp:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.FuncCall:
		for _, arg := range e.Args {
			collectExprRange(arg, ctx, ranges)
		}
	case *sql.ParenExpr:
		collectExprRange(e.Expr, ctx, ranges)
	case *sql.CaseExpr:
		if e.Operand != nil {
			collectExprRange(e.Operand, ctx, ranges)
		}
		for _, w := range e.Whens {
			collectExprRange(w.When, ctx, ranges)
			collectExprRange(w.Then, ctx, ranges)
		}
		if e.Else != nil {
			collectExprRange(e.Else, ctx, ranges)
		}
	case *sql.CastExpr:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.IsNull:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.IsNotNull:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.IsDistinctFrom:
		collectExprRange(e.Left, ctx, ranges)
		collectExprRange(e.Right, ctx, ranges)
	case *sql.IsNotDistinctFrom:
		collectExprRange(e.Left, ctx, ranges)
		collectExprRange(e.Right, ctx, ranges)
	case *sql.IsTrue:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.IsFalse:
		collectExprRange(e.Operand, ctx, ranges)
	case *sql.Between:
		collectExprRange(e.Operand, ctx, ranges)
		collectExprRange(e.Low, ctx, ranges)
		collectExprRange(e.High, ctx, ranges)
	case *sql.InList:
		collectExprRange(e.Operand, ctx, ranges)
		for _, item := range e.List {
			collectExprRange(item, ctx, ranges)
		}
	case *sql.Subquery:
		if e.Select != nil {
			collectSelectRanges(e.Select, ctx, ranges)
		}
	case *sql.ExistsExpr:
		if e.Select != nil {
			collectSelectRanges(e.Select, ctx, ranges)
		}
	case *sql.RowValue:
		for _, v := range e.Values {
			collectExprRange(v, ctx, ranges)
		}
	}
}

// collectColumnRefRange checks a ColumnRef for matching rename targets.
func collectColumnRefRange(ref *sql.ColumnRef, ctx *RenameContext, ranges *[]RenameRange) {
	if ref == nil {
		return
	}

	if ctx.IsTable {
		// RENAME TABLE: rename the table qualifier in qualified column references
		if ref.Table != "" && ref.TableTok.Start != 0 {
			compareTable := ref.Table
			if dotIdx := strings.LastIndex(compareTable, "."); dotIdx >= 0 {
				compareTable = compareTable[dotIdx+1:]
			}
			if strings.EqualFold(compareTable, ctx.OldName) {
				*ranges = append(*ranges, RenameRange{Start: ref.TableTok.Start, End: ref.TableTok.End})
			}
		}
	} else {
		// RENAME COLUMN: rename the column name
		if strings.EqualFold(ref.Name, ctx.OldName) {
			// Check table qualifier: if specified, it must match ctx.TableName
			if ref.Table == "" || strings.EqualFold(ref.Table, ctx.TableName) {
				if ref.NameTok.Start != 0 || ref.NameTok.End != 0 {
					*ranges = append(*ranges, RenameRange{Start: ref.NameTok.Start, End: ref.NameTok.End})
				}
			}
		}
	}
}

// deduplicateRanges removes overlapping duplicate ranges.
func deduplicateRanges(ranges []RenameRange) []RenameRange {
	if len(ranges) <= 1 {
		return ranges
	}

	// Sort by start position
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Start < ranges[j].Start
	})

	// Deduplicate
	result := make([]RenameRange, 0, len(ranges))
	for _, r := range ranges {
		if len(result) == 0 || result[len(result)-1].Start != r.Start || result[len(result)-1].End != r.End {
			result = append(result, r)
		}
	}
	return result
}

// ApplyRenames applies a set of byte-range replacements to a SQL text.
// Ranges must be sorted in ascending order (they will be sorted if not).
// Each range [start, end) is replaced with the replacement string.
func ApplyRenames(sqlText string, ranges []RenameRange, replacement string) string {
	if len(ranges) == 0 {
		return sqlText
	}

	// Sort ranges in reverse order (to not invalidate offsets)
	sorted := make([]RenameRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Start > sorted[j].Start
	})

	// Apply replacements from last to first
	result := sqlText
	for _, r := range sorted {
		if r.Start < 0 || r.End > len(result) || r.Start >= r.End {
			continue
		}
		result = result[:r.Start] + replacement + result[r.End:]
	}

	return result
}

// quoteNameIfNeeded quotes a name with double quotes if it contains special
// characters, starts with a digit, or is a reserved keyword. Matches SQLite's
// sqlite3MaybeQuote behavior.
func quoteNameIfNeeded(name string) string {
	if name == "" {
		return name
	}
	// Check if quoting is needed
	if needsQuoting(name) {
		return `"` + name + `"`
	}
	return name
}

// needsQuoting checks if a name needs to be double-quoted.
func needsQuoting(name string) bool {
	if name == "" {
		return false
	}
	// Check first character
	if name[0] >= '0' && name[0] <= '9' {
		return true
	}
	// Check for special characters
	for _, ch := range name {
		if !isIdentChar(byte(ch)) {
			return true
		}
	}
	// Check for reserved keywords
	if isReservedKeyword(strings.ToUpper(name)) {
		return true
	}
	return false
}

// isIdentChar checks if a byte is a valid unquoted identifier character.
func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '_' || ch == '$'
}

// isReservedKeyword checks if a name is a SQLite reserved keyword that requires quoting.
func isReservedKeyword(word string) bool {
	// SQLite 3.x reserved words (subset — the ones that commonly cause issues)
	switch word {
	case "ABORT", "ACTION", "ADD", "AFTER", "ALL", "ALTER", "ANALYZE", "AND",
		"AS", "ASC", "ATTACH", "AUTOINCREMENT", "BEFORE", "BEGIN", "BETWEEN",
		"BY", "CASCADE", "CASE", "CAST", "CHECK", "COLLATE", "COLUMN", "COMMIT",
		"CONFLICT", "CONSTRAINT", "CREATE", "CROSS", "CURRENT", "DATABASE",
		"DEFAULT", "DEFERRABLE", "DEFERRED", "DELETE", "DESC", "DETACH",
		"DISTINCT", "DO", "DROP", "EACH", "ELSE", "END", "ESCAPE", "EXCEPT",
		"EXCLUSIVE", "EXISTS", "EXPLAIN", "FAIL", "FILTER", "FOLLOWING",
		"FOR", "FOREIGN", "FROM", "FULL", "GLOB", "GROUP", "HAVING", "IF",
		"IGNORE", "IMMEDIATE", "IN", "INDEX", "INDEXED", "INITIALLY", "INNER",
		"INSERT", "INSTEAD", "INTERSECT", "INTO", "IS", "ISNULL", "JOIN",
		"KEY", "LEFT", "LIKE", "LIMIT", "MATCH", "NATURAL", "NO", "NOT",
		"NOTHING", "NOTNULL", "NULL", "OF", "OFFSET", "ON", "OR", "ORDER",
		"OUTER", "OVER", "PARTITION", "PLAN", "PRAGMA", "PRECEDING", "PRIMARY",
		"QUERY", "RAISE", "RANGE", "RECURSIVE", "REFERENCES", "REGEXP",
		"REINDEX", "RELEASE", "RENAME", "REPLACE", "RESTRICT", "RIGHT",
		"ROLLBACK", "ROW", "ROWS", "SAVEPOINT", "SELECT", "SET", "TABLE",
		"TEMP", "TEMPORARY", "THEN", "TO", "TRANSACTION", "TRIGGER", "UNBOUNDED",
		"UNION", "UNIQUE", "UPDATE", "USING", "VACUUM", "VALUES", "VIEW",
		"VIRTUAL", "WHEN", "WHERE", "WINDOW", "WITH", "WITHOUT":
		return true
	}
	return false
}
