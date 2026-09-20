// This file holds the MATCH operator evaluation for FTS virtual tables
// (FTS3/4 and fts5): the `expr MATCH expr` / `NOT MATCH` forms, the fts5
// rank-override pseudo constraints, the FTS table/column resolution for a
// MATCH expression, and the MATCH query-string coercion.
package execexpr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/fts"
	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// ftsMatchTable is the MATCH-evaluation contract an FTS table (FTS3/4 or
// fts5) must satisfy: per-document query evaluation plus column resolution.
type ftsMatchTable interface {
	MatchQueryColumn(rowid int64, query, columnName string, langid ...int64) (bool, error)
	ColumnNames() []string
}

// evalBinaryOpMatchPreamble handles the FTS MATCH forms before ordinary
// binary-operator evaluation. done=true returns the MATCH-form result
// directly.
func (ev *Evaluator) evalBinaryOpMatchPreamble(v *sql.BinaryOp, row Row) (interface{}, bool, error) {
	// Handle MATCH and NOT MATCH for FTS virtual tables
	if v.Operator == "MATCH" || v.Operator == "NOT MATCH" {
		val, err := ev.evalMatchOp(v, row)
		return val, true, err
	}
	// The fts5 rank override in EQ form ("WHERE rank = 'bm25(...)'") is a
	// consumed xFilter constraint (fts5_main.c fts5BestIndexMethod's 'r'
	// constraint, omit=1), never a row filter: it passes every visited row.
	if (v.Operator == "=" || v.Operator == "==") && ev.consumesFTS5RankOverride(v) {
		return int64(1), true, nil
	}
	return nil, false, nil
}

// consumesFTS5RankOverride reports whether the EQ comparison's left operand is
// the fts5 rank pseudo-column while an fts5 aux context is active (an fts5
// scan is visiting rows): the constraint is then consumed by the scan.
func (ev *Evaluator) consumesFTS5RankOverride(v *sql.BinaryOp) bool {
	ref, ok := v.Left.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(ref.Name, "rank") {
		return false
	}
	ctxTable, _ := ev.ctx.FTS5Aux()
	return ctxTable != ""
}

// evalMatchOp evaluates a MATCH or NOT MATCH expression for FTS virtual tables.
func (ev *Evaluator) evalMatchOp(v *sql.BinaryOp, row Row) (interface{}, error) {
	// The fts5 rank override ("WHERE rank MATCH 'bm25(...)'") is consumed by
	// the fts5 scan (xFilter's rank constraint), never a row filter: it
	// evaluates to true for every visited row.
	if ev.matchRankOverrideConsumed(v) {
		return int64(1), nil
	}
	queryStr, isNull, ok := ev.matchQueryString(v, row)
	if isNull {
		return nil, nil
	}
	if !ok {
		return int64(0), nil
	}

	// Look up the FTS table context. matchFTSLookup returns the table, its
	// name (for resolving the docid in a joined row), and the column to
	// restrict the match to ("" for a whole-table match).
	ftsTable, tableName, columnName, ok := ev.matchFTSLookup(v, row)
	if !ok {
		return ev.evalUnresolvedMatch(v, row)
	}

	// Get the rowid from the current row. In a single-table FTS SELECT the
	// row map's "rowid" is the docid; in a joined row the FTS table's docid
	// lives under "<table>.rowid" (buildCombinedRowMap prefixes the right
	// side's rowid) and the unqualified "rowid" belongs to the LEFT-most
	// table, which may be a different table. Try the qualified key first so
	// a right-side FTS table resolves its own docid. A docid of 0 is a valid
	// FTS document id (INSERT INTO ft(rowid, x) VALUES(0, ...) is legal), so
	// the resolution must report presence separately from the value
	// (fts4content 3.2.x: a content=<table> table with a rowid-0 document).
	rowidVal, rowidOK := matchRowID(row, tableName)
	if !rowidOK {
		return int64(0), nil
	}

	// Evaluate the match against the FTS index; a query-parse failure is
	// treated as no match (matches SQLite behavior).
	// The query is parsed at the cursor's language id: the FTS4
	// languageid=<col> constraint value from the current row, 0 by default
	// (fts3.c fts3FilterMethod binds pLangid to the expression parser —
	// fts4langid 4.1.3 tokenizes 'Quick' differently at langid 1).
	matched, err := ftsTable.MatchQueryColumn(rowidVal, queryStr, columnName, ev.ftsMatchLangid(ftsTable, row))
	if err != nil {
		if matchErrorIsFatal(err) {
			return nil, err
		}
		return int64(0), nil
	}
	if v.Operator == "NOT MATCH" {
		return boolToInt(!matched), nil
	}
	return boolToInt(matched), nil
}

// matchRankOverrideConsumed reports whether the MATCH expression is the fts5
// rank override (its left operand is the rank pseudo-column while an fts5 aux
// context is active).
func (ev *Evaluator) matchRankOverrideConsumed(v *sql.BinaryOp) bool {
	ref, ok := v.Left.(*sql.ColumnRef)
	if !ok || !strings.EqualFold(ref.Name, "rank") {
		return false
	}
	ctxTable, _ := ev.ctx.FTS5Aux()
	return ctxTable != ""
}

// evalUnresolvedMatch handles `expr MATCH expr` when no FTS table resolves.
// SQLite compiles it to the two-argument function match(RIGHT, LEFT) —
// like()'s argument order. A REAL registration (an application UDF, e.g.
// like.test 2.3/2.4 `db function match -argcount 2 test_match`) takes
// precedence over the modules' match/2 overload, so call it with (right,
// left). No real registration: the FTS/rtree modules' match/2 overload
// (sqlite3_overload_function → sqlite3InvalidFunction) fails when EVALUATED.
// A literal left operand can never reach a vtab MATCH constraint, so it
// always reaches evaluation (func-4.3/4.4: SELECT 'abc' MATCH 'xyz'). A
// column/expression left operand may belong to a statement whose MATCH
// constraint a virtual table's xBestIndex/xFilter already consumed (echo
// module vtab1-3.14/10-5, rtree geometry MATCH): SQLite emits no per-row code
// for the consumed term, so the residual row evaluation stays inert.
func (ev *Evaluator) evalUnresolvedMatch(v *sql.BinaryOp, row Row) (interface{}, error) {
	if fn, found := ev.ctx.Functions().Find("match"); found && fn.ScalarFn != nil && !fn.Builtin {
		left, lerr := ev.evalExprWithCollation(v.Left, row)
		if lerr != nil {
			return nil, lerr
		}
		right, rerr := ev.evalExprWithCollation(v.Right, row)
		if rerr != nil {
			return nil, rerr
		}
		out, ferr := fn.ScalarFn([]interface{}{right, left})
		if ferr != nil {
			return nil, ferr
		}
		return boolToInt(out != nil && ToBool(out)), nil
	}
	switch v.Left.(type) {
	case *sql.StringLit, *sql.NumericLit, *sql.NullLit, *sql.BlobLit:
		return nil, fmt.Errorf("unable to use function MATCH in the requested context")
	}
	return int64(0), nil
}

// ftsMatchLangColumn returns the languageid column of an FTS3 table (fts5
// tables have none; the interface cannot expose LangIDColName directly).
func ftsMatchLangColumn(t ftsMatchTable) string {
	if f3, ok := t.(*fts.FTS3Table); ok {
		return f3.LangIDColName()
	}
	return ""
}

// ftsMatchLangid resolves the MATCH query's language id: the FTS4
// languageid=<col> value from the current row, 0 by default.
func (ev *Evaluator) ftsMatchLangid(ftsTable ftsMatchTable, row Row) int64 {
	if langCol := ftsMatchLangColumn(ftsTable); langCol != "" {
		if lv, ok := row.Get(langCol); ok {
			return ToIntValue(util.UnwrapColumnValue(lv))
		}
	}
	return 0
}

// matchErrorIsFatal reports whether a match evaluation error fails the
// statement: a corrupt-term MATCH fails with "database disk image is
// malformed" (fts3corrupt4 11.1/19.1); a query-parse failure is treated as
// no match (matches SQLite behavior) EXCEPT a malformed MATCH expression,
// which SQLite reports at prepare and fails the statement (fts3expr 2.x,
// fts3ag 4.x). fts5 query errors ("fts5: syntax error near ...", detail
// restrictions, unknown query columns) are statement errors too.
func matchErrorIsFatal(err error) bool {
	return strings.Contains(err.Error(), "database disk image is malformed") ||
		strings.Contains(err.Error(), "malformed MATCH expression") ||
		strings.HasPrefix(err.Error(), "fts5:") ||
		strings.Contains(err.Error(), "no such column: ")
}

// matchQueryString evaluates the right-hand side of a MATCH expression and
// coerces it to the query string. SQLite coerces the MATCH RHS to text; a
// column-backed value arrives wrapped in a *util.ColumnValue and is unwrapped
// here. Returns ("", true, false) for a NULL RHS (MATCH evaluates to NULL),
// ("", false, false) for an unusable non-text RHS (no match), and
// (query, false, true) for a usable text query string.
func (ev *Evaluator) matchQueryString(v *sql.BinaryOp, row Row) (string, bool, bool) {
	right, err := ev.evalExpr(v.Right, row)
	if err != nil {
		return "", false, false
	}
	if right == nil {
		return "", true, false
	}
	if queryStr, ok := right.(string); ok {
		return queryStr, false, true
	}
	if unwrapped := util.UnwrapColumnValue(right); unwrapped != nil {
		if s, isStr := unwrapped.(string); isStr {
			return s, false, true
		}
		// SQLite coerces the MATCH RHS to text (sqlite3_value_text):
		// MATCH 1 matches documents containing the token "1", and MATCH
		// x'...' treats the blob's raw bytes as the query string
		// (fts3matchinfo2 1.0 passes a binary blob as the MATCH RHS).
		return matchQueryStringCoerced(unwrapped)
	}
	return "", false, false
}

// matchQueryStringCoerced coerces a non-string MATCH RHS to its text form:
// integers and reals print as decimal, blobs use their raw bytes.
func matchQueryStringCoerced(unwrapped interface{}) (string, bool, bool) {
	if i, isInt := unwrapped.(int64); isInt {
		return strconv.FormatInt(i, 10), false, true
	}
	if f, isFloat := unwrapped.(float64); isFloat {
		return strconv.FormatFloat(f, 'g', -1, 64), false, true
	}
	if b, isBlob := unwrapped.([]byte); isBlob {
		return string(b), false, true
	}
	return "", false, false
}

// matchRowID resolves the FTS table's docid for a MATCH evaluation. In a
// single-table FTS SELECT the row map's "rowid" is the docid; in a joined row
// the FTS table's docid lives under "<table>.rowid". Returns (0, false) when
// no rowid can be resolved (no match); a docid of 0 is a valid FTS document id
// and is reported as (0, true).
func matchRowID(row Row, tableName string) (int64, bool) {
	if tableName != "" {
		if v, ok := row.Get(tableName + ".rowid"); ok {
			if r, ok := util.UnwrapColumnValue(v).(int64); ok {
				return r, true
			}
		}
	}
	return getRowIDPresence(row)
}

// getRowIDPresence extracts the rowid from a Row value, reporting presence
// separately from the value so a rowid of 0 is distinguishable from "no rowid".
func getRowIDPresence(row Row) (int64, bool) {
	if row == nil {
		return 0, false
	}
	if v, ok := row.Get("rowid"); ok {
		if r, ok := util.UnwrapColumnValue(v).(int64); ok {
			return r, true
		}
	}
	return 0, false
}

// matchFTSLookup resolves the FTS table for a MATCH expression, returning the
// table, its name (for docid resolution in a joined row), the column to
// restrict to ("" for a whole-table match), and whether the expression is an
// FTS MATCH at all. Resolution order:
//
//  1. The engine's current FTS match context (set by a single-table FTS
//     SELECT); the left-side column restricts the match when present.
//  2. A qualified left operand (T.x MATCH q): T is the FTS table.
//  3. A bare left identifier: if it names an FTS table, it is a whole-table
//     match (ft1 MATCH q); otherwise, if it names a column of an FTS table,
//     it is a column-restricted match on that table (x MATCH q in a join).
//     When several FTS tables declare the same column, the row's qualified
//     <table>.col key disambiguates which table the column belongs to (the
//     row is built from the joined tables in the query's FROM clause).
//
// Both FTS3/4 tables and fts5 tables resolve here.
func (ev *Evaluator) matchFTSLookup(v *sql.BinaryOp, row Row) (ftsMatchTable, string, string, bool) {
	// 1. Current FTS match context (single-table FTS SELECT).
	if name := ev.ctx.CurrentFTSMatch(); name != "" {
		if ft, ok := ev.matchFTSTableByName(name); ok {
			return ft, name, ev.leftMatchColumnName(v), true
		}
	}
	colRef, isColRef := v.Left.(*sql.ColumnRef)
	if !isColRef {
		return nil, "", "", false
	}
	// 2. Qualified left operand: the qualifier is the FTS table.
	if colRef.Table != "" {
		if ft, ok := ev.matchFTSTableByName(colRef.Table); ok {
			return ft, colRef.Table, colRef.Name, true
		}
		return nil, "", "", false
	}
	// 3a. Whole-table match: the name IS an FTS table.
	if ft, ok := ev.matchFTSTableByName(colRef.Name); ok {
		return ft, colRef.Name, "", true
	}
	// 3b. Column match: find the FTS table that declares this column. Prefer
	// the table whose qualified <table>.col key exists in the row, so a
	// column shared by several FTS tables resolves to the joined table that
	// actually carries it (e.g. ft1.x and ft2.x in the same query).
	if ft, tname, ok := ev.matchFTSTableByColumn(colRef.Name, row); ok {
		return ft, tname, colRef.Name, true
	}
	return nil, "", "", false
}

// matchFTSTableByName resolves an FTS3/4 or fts5 table by name.
func (ev *Evaluator) matchFTSTableByName(name string) (ftsMatchTable, bool) {
	if ft, ok := ev.ctx.FTSTables()[name]; ok {
		return ft, true
	}
	t5, ok := ev.ctx.FTS5Tables()[name]
	return t5, ok
}

// matchFTSTableByColumn finds the FTS table declaring the given column,
// preferring the table whose qualified <table>.col key exists in the row
// (several FTS tables may declare the same column in a join) and falling
// back to the first declaring table (single-table context without a
// qualified row key).
func (ev *Evaluator) matchFTSTableByColumn(name string, row Row) (ftsMatchTable, string, bool) {
	if row != nil {
		if ft, tname, ok := fts3TableByColumnQualified(ev.ctx.FTSTables(), name, row); ok {
			return ft, tname, true
		}
		if t5, tname, ok := fts5TableByColumnQualified(ev.ctx.FTS5Tables(), name, row); ok {
			return t5, tname, true
		}
	}
	if ft, tname, ok := fts3TableByColumnFirst(ev.ctx.FTSTables(), name); ok {
		return ft, tname, true
	}
	if t5, tname, ok := fts5TableByColumnFirst(ev.ctx.FTS5Tables(), name); ok {
		return t5, tname, true
	}
	return nil, "", false
}

// fts3TableByColumnQualified scans FTS3/4 tables for one declaring the column
// whose qualified <table>.col key exists in the row.
func fts3TableByColumnQualified(tables map[string]*fts.FTS3Table, name string, row Row) (ftsMatchTable, string, bool) {
	for tname, ft := range tables {
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, name) {
				if _, ok := row.Get(tname + "." + name); ok {
					return ft, tname, true
				}
			}
		}
	}
	return nil, "", false
}

// fts5TableByColumnQualified scans fts5 tables for one declaring the column
// whose qualified <table>.col key exists in the row.
func fts5TableByColumnQualified(tables map[string]*fts5.Table, name string, row Row) (ftsMatchTable, string, bool) {
	for tname, t5 := range tables {
		for _, col := range t5.ColumnNames() {
			if strings.EqualFold(col, name) {
				if _, ok := row.Get(tname + "." + name); ok {
					return t5, tname, true
				}
			}
		}
	}
	return nil, "", false
}

// fts3TableByColumnFirst returns the first FTS3/4 table declaring the column.
func fts3TableByColumnFirst(tables map[string]*fts.FTS3Table, name string) (ftsMatchTable, string, bool) {
	for tname, ft := range tables {
		for _, col := range ft.ColumnNames() {
			if strings.EqualFold(col, name) {
				return ft, tname, true
			}
		}
	}
	return nil, "", false
}

// fts5TableByColumnFirst returns the first fts5 table declaring the column.
func fts5TableByColumnFirst(tables map[string]*fts5.Table, name string) (ftsMatchTable, string, bool) {
	for tname, t5 := range tables {
		for _, col := range t5.ColumnNames() {
			if strings.EqualFold(col, name) {
				return t5, tname, true
			}
		}
	}
	return nil, "", false
}

// leftMatchColumnName returns the column name restricting a MATCH whose left
// operand is a column reference, or "" for a whole-table match (the left
// operand is the FTS table name itself).
func (ev *Evaluator) leftMatchColumnName(v *sql.BinaryOp) string {
	if colRef, ok := v.Left.(*sql.ColumnRef); ok {
		// In single-table FTS SELECTs the left operand is the table name
		// (e.g. ft1 MATCH 'abc'); the table name is not a column restriction.
		if colRef.Table == "" {
			if _, isTable := ev.ctx.FTSTables()[colRef.Name]; isTable {
				return ""
			}
			if _, isTable := ev.ctx.FTS5Tables()[colRef.Name]; isTable {
				return ""
			}
		}
		return colRef.Name
	}
	return ""
}
