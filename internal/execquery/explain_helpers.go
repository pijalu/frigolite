package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
)

// indexSQLColumnCollation extracts an explicit COLLATE clause applied to the
// named column in a CREATE INDEX statement (e.g. "CREATE INDEX i ON t(x
// COLLATE nocase)"). Returns "" when the column has no explicit collation.
func indexSQLColumnCollation(sqlStr, colName string) string {
	upper := strings.ToUpper(sqlStr)
	start := strings.Index(upper, "(")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(upper, ")")
	if end < 0 || end <= start {
		return ""
	}
	colsStr := sqlStr[start+1 : end]
	for _, c := range strings.Split(colsStr, ",") {
		col := strings.TrimSpace(c)
		// Split "name COLLATE nocase" on the COLLATE keyword.
		ci := strings.Index(strings.ToUpper(col), " COLLATE ")
		if ci < 0 {
			continue
		}
		name := strings.TrimSpace(col[:ci])
		coll := strings.TrimSpace(col[ci+len(" COLLATE "):])
		if strings.EqualFold(name, colName) && coll != "" {
			return strings.ToUpper(coll)
		}
	}
	return ""
}

// likeIndexCompatible reports whether an index with the given collation can
// drive the LIKE optimization under the current case_sensitive_like setting.
func (e *SelectEngine) likeIndexCompatible(coll string) bool {
	if e.ctx.CaseSensitiveLike() {
		// Case-sensitive LIKE: a BINARY index works (no case folding needed);
		// a NOCASE index cannot (its keys are folded, the comparison is not).
		return coll == "" || strings.EqualFold(coll, "BINARY")
	}
	// Default case-insensitive LIKE: only a NOCASE index can range over the
	// case variants; a BINARY index cannot.
	return strings.EqualFold(coll, "NOCASE")
}

// collectAllColumnRefs walks a WHERE expression and returns an indexedRef for
// every column-to-constant predicate, regardless of whether the column has an
// index. Used to render the full set of search constraints in EXPLAIN output.
func collectAllColumnRefs(expr sql.Expr, tableName string) []indexedRef {
	var refs []indexedRef
	_, _ = walkExpr, walkExpr(expr, func(e2 sql.Expr) {
		if binop, ok := e2.(*sql.BinaryOp); ok {
			colRef, constVal := findColAndConst(binop)
			if colRef != nil && constVal != nil {
				refs = append(refs, indexedRef{
					indexName: "",
					colName:   colRef.Name,
					constant:  constVal,
					op:        binop.Operator,
				})
			}
		}
	})
	return refs
}

func computeBetweenSelectivity(bt *sql.Between) float64 {
	// String-literal bounds (e.g. date ranges like
	// datetime(b) BETWEEN '2017-07-04' AND '2017-07-08') narrow the search
	// substantially, so treat them as a narrow range. Numeric bounds use the
	// range-width heuristics below.
	if _, ok := bt.Low.(*sql.StringLit); ok {
		if _, ok2 := bt.High.(*sql.StringLit); ok2 {
			return 0.05
		}
	}
	// Extract low and high values
	lowVal, lowOk := numericLitValue(bt.Low)
	highVal, highOk := numericLitValue(bt.High)
	if !lowOk || !highOk {
		return 0.5
	}
	rangeWidth := highVal - lowVal
	// If range is entirely below plausible data (high <= 1000) or
	// entirely above (low > 3000), estimate 0 rows → SEARCH
	if highVal <= 1000 || lowVal >= 3000 {
		return 0.01
	}
	if rangeWidth <= 200 {
		return 0.05 // narrow range
	}
	return 0.5 // wide range → SCAN
}

func numericLitValue(e sql.Expr) (float64, bool) {
	lit, ok := e.(*sql.NumericLit)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(lit.Value, 64)
	return f, err == nil
}

func walkExpr(expr sql.Expr, fn func(sql.Expr)) error {
	if expr == nil {
		return nil
	}
	fn(expr)
	switch e := expr.(type) {
	case *sql.ParenExpr:
		walkExpr(e.Expr, fn)
	case *sql.BinaryOp:
		walkExpr(e.Left, fn)
		walkExpr(e.Right, fn)
	case *sql.UnaryOp:
		walkExpr(e.Operand, fn)
	case *sql.Between:
		walkExpr(e.Operand, fn)
		walkExpr(e.Low, fn)
		walkExpr(e.High, fn)
	case *sql.InList:
		walkExpr(e.Operand, fn)
	}
	return nil
}

func findColAndConst(b *sql.BinaryOp) (*sql.ColumnRef, interface{}) {
	// op: colRef = const OR const = colRef
	if colRef, ok := b.Left.(*sql.ColumnRef); ok {
		return colRef, extractConst(b.Right)
	}
	if colRef, ok := b.Right.(*sql.ColumnRef); ok {
		return colRef, extractConst(b.Left)
	}
	return nil, nil
}

// extractConst extracts a constant value from an expression node.
func extractConst(e sql.Expr) interface{} {
	switch v := e.(type) {
	case *sql.ParenExpr:
		return extractConst(v.Expr)
	case *sql.NumericLit:
		f, err := strconv.ParseFloat(v.Value, 64)
		if err == nil {
			return f
		}
		return nil
	case *sql.StringLit:
		return v.Value
	case *sql.NullLit:
		return nil
	case *sql.ParameterExpr:
		// A bound parameter is a runtime constant: SQLite plans "col=?" via
		// an index search exactly like a literal (analyze7-3.2.1 "WHERE
		// c=?"). The node marks constrainedness only — the EQP condition
		// renders every operator as "col=?" regardless.
		return v
	default:
		return nil
	}
}

// isWithoutRowidPKColumn reports whether colName is a PRIMARY KEY column of a
// WITHOUT ROWID table (whose PK is the implicit storage index). An integer
// column position in the PK constraint also counts.
func (e *SelectEngine) isWithoutRowidPKColumn(tableName, colName string) bool {
	entry, err := e.ctx.Schema().FindTable(tableName)
	if err != nil || !e.ctx.HasWithoutRowidKeyword(strings.ToUpper(entry.SQL)) {
		return false
	}
	colDefs := e.ctx.ParseColumnDefs(entry.Name, entry.SQL)
	for _, c := range e.ctx.WithoutRowidPKColumns(entry.Name, entry, colDefs, false) {
		if strings.EqualFold(c.Name, colName) {
			return true
		}
	}
	return false
}

// findColDefByName returns the column definition matching a name.
func findColDefByName(colDefs []sql.ColumnDef, name string) (sql.ColumnDef, bool) {
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, name) {
			return cd, true
		}
	}
	return sql.ColumnDef{}, false
}

// parseStatSZ extracts the sz value from a stat string like "12345 3 2 sz=20".
// Returns 0 if no sz hint is found.
func parseStatSZ(stat string) int {
	if stat == "" {
		return 0
	}
	upper := strings.ToUpper(stat)
	idx := strings.Index(upper, "SZ=")
	if idx < 0 {
		return 0
	}
	// Parse the value after "sz="
	valStr := stat[idx+3:] // "20" or "20 ..."
	endIdx := strings.IndexAny(valStr, " \t")
	if endIdx > 0 {
		valStr = valStr[:endIdx]
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return 0
	}
	return val
}

// findIndexOnLeadingColumn returns the first index on the table whose FIRST
// key column is colName (case-insensitive), or "".
func (e *SelectEngine) findIndexOnLeadingColumn(tableName, colName string) string {
	entries, err := e.ctx.Schema().GetEntries("")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type != "index" || entry.TblName != tableName {
			continue
		}
		cols := e.indexEntryColumns(entry)
		if len(cols) > 0 && strings.EqualFold(cols[0], colName) {
			return indexLookupToken(entry.Name)
		}
	}
	return ""
}
