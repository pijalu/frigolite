package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// fts5 SELECT support: the dedicated scan path materializes the fts5 table's
// documents and runs the generic materialized pipeline (WHERE with MATCH
// evaluation, aggregates, DISTINCT, ORDER BY, LIMIT) over them.

// execFTS5Select executes a single-table SELECT over an fts5 table. A MATCH
// constraint drives the scan universe from the index (xFilter parity), so
// index-only documents of external-content tables are visible.
func (e *SelectEngine) execFTS5Select(s *sql.SelectStmt, t5 *fts5.Table, colDefs []sql.ColumnDef) *Result {
	hasMatch := statementHasFTS5Match(s, t5.Name())
	rowids, rows, err := e.fts5UniverseRows(s.Where, t5, colDefs, hasMatch)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterializedRowids(s, colDefs, rows, rowids)
}

// fts5UniverseRows materializes the documents the statement's WHERE can
// visit: the intersection of the top-level MATCH conjuncts' rowid sets when
// one exists (index-driven scan), otherwise the full document scan.
func (e *SelectEngine) fts5UniverseRows(where sql.Expr, t5 *fts5.Table, colDefs []sql.ColumnDef, hasMatch bool) ([]int64, [][]interface{}, error) {
	set, err := t5.MatchUniverse(where, func(expr sql.Expr) (interface{}, error) {
		return e.ctx.EvalExpr(expr, nil)
	})
	if err != nil {
		return nil, nil, err
	}
	if set != nil {
		ids := t5.SortedMatchRowids(set)
		rowids := make([]int64, 0, len(ids))
		rows := make([][]interface{}, 0, len(ids))
		for _, rowid := range ids {
			vals, verr := t5.DocValues(rowid)
			if verr != nil {
				return nil, nil, verr
			}
			rowids = append(rowids, rowid)
			rows = append(rows, fts5FlatRow(t5, rowid, vals, hasMatch))
		}
		return rowids, rows, nil
	}
	return fts5ScanRows(t5, colDefs, hasMatch)
}

// fts5FlatRow renders one document's flat row in colDefs order.
func fts5FlatRow(t5 *fts5.Table, rowid int64, values []interface{}, hasMatch bool) []interface{} {
	rank := interface{}(nil)
	if hasMatch {
		rank = float64(0)
	}
	nUser := len(t5.ColumnNames())
	row := make([]interface{}, 0, nUser+2)
	for c := 0; c < nUser; c++ {
		var v interface{}
		if c < len(values) {
			v = values[c]
		}
		row = append(row, v)
	}
	row = append(row, rowid, rank)
	return row
}

// execFTS5TableFunc materializes the table-valued form FROM t1('query'): each
// string argument is an implicit MATCH constraint, each integer argument a
// rowid equality (fts5_main.c's TVF handling). Returns handled=false when ref
// does not name an fts5 table.
func (e *SelectEngine) execFTS5TableFunc(ref sql.TableRef, s *sql.SelectStmt) (*Result, bool) {
	t5, ok := e.ctx.FTS5Tables()[ref.Name]
	if !ok {
		return nil, false
	}
	matched := make(map[int64]bool)
	restrictRowid := make(map[int64]bool)
	for _, arg := range ref.Args {
		v, err := e.ctx.EvalExpr(arg, nil)
		if err != nil {
			return &Result{Error: err}, true
		}
		switch x := unwrapTVFArg(v).(type) {
		case string:
			set, merr := t5.MatchRowids(x, -1)
			if merr != nil {
				return &Result{Error: merr}, true
			}
			for rowid := range set {
				matched[rowid] = true
			}
		case int64:
			restrictRowid[x] = true
		default:
			return &Result{Error: errFTS5TVFArg()}, true
		}
	}
	colDefs := fts5ColDefs(t5)
	hasArgs := len(ref.Args) > 0
	if hasArgs {
		// Index-driven universe: the TVF's MATCH/rowid arguments select the
		// documents (external-content index-only rows included).
		rowids := t5.SortedMatchRowids(matched)
		var outIDs []int64
		var rows [][]interface{}
		for _, rowid := range rowids {
			if len(restrictRowid) > 0 && !restrictRowid[rowid] {
				continue
			}
			vals, verr := t5.DocValues(rowid)
			if verr != nil {
				return &Result{Error: verr}, true
			}
			outIDs = append(outIDs, rowid)
			rows = append(rows, fts5FlatRow(t5, rowid, vals, hasArgs))
		}
		return e.execSelectOverMaterializedRowids(s, colDefs, rows, outIDs), true
	}
	rowids, rows, err := fts5ScanRows(t5, colDefs, false)
	if err != nil {
		return &Result{Error: err}, true
	}
	return e.execSelectOverMaterializedRowids(s, colDefs, rows, rowids), true
}

// fts5ScanRows materializes the table's documents into flat rows in colDefs
// order (user columns, hidden table-name column = rowid, rank) plus the
// parallel rowid slice. hasMatch drives the rank pseudo-column: NULL without
// a MATCH constraint in the statement, 0.0 with one (full bm25 ranking is
// slice 5).
func fts5ScanRows(t5 *fts5.Table, colDefs []sql.ColumnDef, hasMatch bool) ([]int64, [][]interface{}, error) {
	rowids, values, err := t5.ScanDocs()
	if err != nil {
		return nil, nil, err
	}
	rank := interface{}(nil)
	if hasMatch {
		rank = float64(0)
	}
	nUser := len(t5.ColumnNames())
	rows := make([][]interface{}, len(rowids))
	for i, rowid := range rowids {
		row := make([]interface{}, 0, len(colDefs))
		for c := 0; c < nUser; c++ {
			var v interface{}
			if i < len(values) && c < len(values[i]) {
				v = values[i][c]
			}
			row = append(row, v)
		}
		row = append(row, rowid, rank)
		rows[i] = row
	}
	return rowids, rows, nil
}

// unwrapTVFArg unwraps a ColumnValue wrapper for TVF argument inspection.
func unwrapTVFArg(v interface{}) interface{} {
	return util.UnwrapColumnValue(v)
}

// errFTS5TVFArg renders the error for an unusable TVF argument (fts5: a
// non-text, non-integer argument cannot seed a MATCH constraint).
func errFTS5TVFArg() error {
	return &fts5TVFArgError{}
}

type fts5TVFArgError struct{}

func (e *fts5TVFArgError) Error() string { return "argument type not supported" }

// fts5ColDefs renders the declared column definitions of an fts5 table: user
// columns followed by the hidden table-name and rank columns
// (fts5ConfigDeclareVtab's "CREATE TABLE x(cols, name HIDDEN, rank HIDDEN)").
func fts5ColDefs(t5 *fts5.Table) []sql.ColumnDef {
	defs := make([]sql.ColumnDef, 0, len(t5.ColumnNames())+2)
	for _, c := range t5.ColumnNames() {
		defs = append(defs, sql.ColumnDef{Name: c})
	}
	defs = append(defs,
		sql.ColumnDef{Name: t5.Name(), Hidden: true},
		sql.ColumnDef{Name: "rank", Hidden: true})
	return defs
}

// statementHasFTS5Match reports whether the statement's WHERE contains a
// MATCH constraint against the given fts5 table.
func statementHasFTS5Match(s *sql.SelectStmt, tableName string) bool {
	return walkForFTS5Match(s.Where, tableName)
}

// walkForFTS5Match walks expressions for MATCH ops whose left operand
// references the table (bare or qualified).
func walkForFTS5Match(expr sql.Expr, tableName string) bool {
	if expr == nil {
		return false
	}
	switch n := expr.(type) {
	case *sql.BinaryOp:
		if n.Operator == "MATCH" {
			if ref, ok := n.Left.(*sql.ColumnRef); ok {
				if strings.EqualFold(ref.Name, tableName) || strings.EqualFold(ref.Table, tableName) {
					return true
				}
			}
		}
		if walkForFTS5Match(n.Left, tableName) || walkForFTS5Match(n.Right, tableName) {
			return true
		}
	case *sql.UnaryOp:
		return walkForFTS5Match(n.Operand, tableName)
	}
	return false
}
