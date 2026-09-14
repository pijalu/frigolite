package execquery

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/fts5"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// fts5 SELECT support: the dedicated scan path materializes the fts5 table's
// documents and runs the generic materialized pipeline (WHERE with MATCH
// evaluation, aggregates, DISTINCT, ORDER BY, LIMIT) over them. The statement
// also drives the fts5 auxiliary-function context (bm25/highlight/snippet)
// and the rank pseudo-column.

// execFTS5Select executes a single-table SELECT over an fts5 table. A MATCH
// constraint drives the scan universe from the index (xFilter parity), so
// index-only documents of external-content tables are visible.
func (e *SelectEngine) execFTS5Select(s *sql.SelectStmt, t5 *fts5.Table, colDefs []sql.ColumnDef) *Result {
	hasMatch := statementHasFTS5Match(s, t5)
	override, hasOverride, err := e.fts5RankOverride(s.Where)
	if err != nil {
		return &Result{Error: err}
	}
	aq, err := e.fts5PrepareAux(s.Where, t5, hasMatch)
	if err != nil {
		return &Result{Error: err}
	}
	rankFn, err := e.fts5RankFn(s, t5, aq, override, hasOverride, hasMatch)
	if err != nil {
		return &Result{Error: err}
	}
	e.ctx.SetFTS5Aux(t5.Name(), aq)
	defer e.ctx.ClearFTS5Aux()
	rowids, rows, err := e.fts5UniverseRows(s.Where, t5, colDefs, rankFn)
	if err != nil {
		return &Result{Error: err}
	}
	return e.execSelectOverMaterializedRowids(s, colDefs, rows, rowids)
}

// fts5PrepareAux parses the statement's MATCH constraint query for the
// auxiliary-function context. Every MATCH conjunct on the table merges into
// ONE combined query (C's xFilter receives the merged expression), so the
// aux context sees every phrase. The universe evaluation re-parses
// (memoized by the table's match cache), matching C's per-cursor single
// parse closely enough for observation.
func (e *SelectEngine) fts5PrepareAux(where sql.Expr, t5 *fts5.Table, hasMatch bool) (*fts5.AuxQuery, error) {
	if !hasMatch {
		return t5.NewScanAux(), nil
	}
	var constraints []fts5.AuxConstraint
	for _, conjunct := range fts5TopLevelConjuncts(where) {
		bop, ok := conjunct.(*sql.BinaryOp)
		if !ok || bop.Operator != "MATCH" {
			continue
		}
		col, applies := fts5MatchConstraintColumn(bop.Left, t5)
		if !applies {
			continue
		}
		qv, err := e.ctx.EvalExpr(bop.Right, nil)
		if err != nil {
			return nil, err
		}
		q, ok := util.UnwrapColumnValue(qv).(string)
		if !ok {
			continue
		}
		// A special query ('*reads'/'*id' — fts5SpecialMatch) never reaches
		// the expression parser: xFilter dispatches it before the aux
		// context is built, and aux functions see zero instances.
		if strings.HasPrefix(q, "*") {
			return t5.NewScanAux(), nil
		}
		constraints = append(constraints, fts5.AuxConstraint{Query: q, Col: col})
	}
	if len(constraints) == 0 {
		return t5.NewScanAux(), nil
	}
	return t5.PrepareAuxMulti(constraints)
}

// firstFTS5MatchConstraint extracts the first MATCH conjunct's query string
// and column restriction for the given fts5 table.
func (e *SelectEngine) firstFTS5MatchConstraint(where sql.Expr, t5 *fts5.Table) (string, int, bool) {
	for _, conjunct := range fts5TopLevelConjuncts(where) {
		bop, ok := conjunct.(*sql.BinaryOp)
		if !ok || bop.Operator != "MATCH" {
			continue
		}
		col, applies := fts5MatchConstraintColumn(bop.Left, t5)
		if !applies {
			continue
		}
		qv, err := e.ctx.EvalExpr(bop.Right, nil)
		if err != nil {
			return "", -1, false
		}
		q, ok := util.UnwrapColumnValue(qv).(string)
		if !ok {
			continue
		}
		return q, col, true
	}
	return "", -1, false
}

// fts5RankOverride resolves the WHERE's `rank MATCH '...'` / `rank = '...'`
// constraint (the per-cursor rank function override; fts5_main.c
// fts5BestIndexMethod treats MATCH and EQ on the rank HIDDEN column as the
// 'r' constraint). The constraint is consumed by the fts5 scan, not a row
// filter.
func (e *SelectEngine) fts5RankOverride(where sql.Expr) (*fts5.RankSpec, bool, error) {
	for _, conjunct := range fts5TopLevelConjuncts(where) {
		bop, ok := conjunct.(*sql.BinaryOp)
		if !ok || (bop.Operator != "MATCH" && bop.Operator != "=" && bop.Operator != "==") {
			continue
		}
		ref, ok := bop.Left.(*sql.ColumnRef)
		if !ok || !strings.EqualFold(ref.Name, "rank") {
			continue
		}
		v, err := e.ctx.EvalExpr(bop.Right, nil)
		if err != nil {
			return nil, false, err
		}
		// The spec text is the value's text form (fts5CursorParseRank's
		// sqlite3_value_text; NULL yields "" and fails the parse).
		text := fts5TVFArgText(v)
		spec, perr := fts5.ParseRankSpec(text)
		if perr != nil {
			return nil, false, &fts5.RankParseError{Text: text}
		}
		return spec, true, nil
	}
	return nil, false, nil
}

// fts5RankFn builds the per-document rank-value function for the statement:
// NULL without a MATCH constraint (C's full-scan cursors have no rank), the
// rank function's value with one. The function resolves lazily — only a
// statement that reads rank pays the resolution cost ("no such function").
func (e *SelectEngine) fts5RankFn(s *sql.SelectStmt, t5 *fts5.Table, aq *fts5.AuxQuery, override *fts5.RankSpec, hasOverride, hasMatch bool) (func(int64) (interface{}, error), error) {
	if !hasMatch {
		return nil, nil
	}
	if !statementReadsFTS5Rank(s, t5.Name()) {
		return nil, nil
	}
	spec := &t5.Config().Rank
	if hasOverride {
		spec = override
	}
	return func(rowid int64) (interface{}, error) {
		return t5.RankValue(spec, aq, rowid)
	}, nil
}

// statementReadsFTS5Rank reports whether the statement projects or orders by
// the rank column of the given fts5 table.
func statementReadsFTS5Rank(s *sql.SelectStmt, tableName string) bool {
	found := false
	check := func(expr sql.Expr) {
		if found || expr == nil {
			return
		}
		WalkExprFull(expr, func(n sql.Expr) {
			if ref, ok := n.(*sql.ColumnRef); ok {
				if strings.EqualFold(ref.Name, "rank") &&
					(ref.Table == "" || strings.EqualFold(ref.Table, tableName)) {
					found = true
				}
			}
		})
	}
	for _, c := range s.Columns {
		check(c.Expr)
	}
	for _, o := range s.OrderBy {
		check(o.Expr)
	}
	check(s.Having)
	return found
}

// fts5UniverseRows materializes the documents the statement's WHERE can
// visit: the intersection of the top-level MATCH conjuncts' rowid sets when
// one exists (index-driven scan), otherwise the full document scan.
func (e *SelectEngine) fts5UniverseRows(where sql.Expr, t5 *fts5.Table, colDefs []sql.ColumnDef, rankFn func(int64) (interface{}, error)) ([]int64, [][]interface{}, error) {
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
			flat, rerr := fts5FlatRow(t5, rowid, vals, rankFn)
			if rerr != nil {
				return nil, nil, rerr
			}
			rowids = append(rowids, rowid)
			rows = append(rows, flat)
		}
		return rowids, rows, nil
	}
	return fts5ScanRows(t5, colDefs, rankFn)
}

// fts5FlatRow renders one document's flat row in colDefs order.
func fts5FlatRow(t5 *fts5.Table, rowid int64, values []interface{}, rankFn func(int64) (interface{}, error)) ([]interface{}, error) {
	var rank interface{}
	if rankFn != nil {
		v, err := rankFn(rowid)
		if err != nil {
			return nil, err
		}
		rank = v
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
	return row, nil
}

// execFTS5TableFunc materializes the table-valued form FROM t1('query').
// Arguments bind positionally to the fts5 vtab's two HIDDEN columns
// (fts5ConfigDeclareVtab's "CREATE TABLE x(cols, name HIDDEN, rank HIDDEN)";
// sqlite3VtabOverloadFunction via whereexpr.c): the first is the MATCH query,
// the second a per-cursor rank function specification (fts5_main.c
// fts5CursorParseRank). Returns handled=false when ref does not name an fts5
// table.
func (e *SelectEngine) execFTS5TableFunc(ref sql.TableRef, s *sql.SelectStmt) (*Result, bool) {
	t5, ok := e.ctx.FTS5Tables()[ref.Name]
	if !ok {
		return nil, false
	}
	// whereexpr.c sqlite3ErrorMsg "too many arguments on %s() - max %d":
	// more arguments than HIDDEN columns is a parse-time error.
	if len(ref.Args) > 2 {
		return &Result{Error: fmt.Errorf("too many arguments on %s() - max 2", t5.Name())}, true
	}
	matched := make(map[int64]bool)
	var firstQuery string
	var rankOverride *fts5.RankSpec
	for i, arg := range ref.Args {
		v, err := e.ctx.EvalExpr(arg, nil)
		if err != nil {
			return &Result{Error: err}, true
		}
		switch i {
		case 0:
			// The table-name hidden column: the argument's text form is the
			// MATCH query (xFilter's fts5ExtractExprText renders the value
			// with sqlite3_value_text; NULL becomes "").
			q := fts5TVFArgText(v)
			set, merr := t5.MatchRowids(q, -1)
			if merr != nil {
				return &Result{Error: merr}, true
			}
			firstQuery = q
			for rowid := range set {
				matched[rowid] = true
			}
		case 1:
			// The rank hidden column: the per-cursor rank function spec.
			spec, rerr := fts5TVFRankSpec(v)
			if rerr != nil {
				return &Result{Error: rerr}, true
			}
			rankOverride = spec
		}
	}
	hasArgs := len(ref.Args) > 0
	aq := t5.NewScanAux()
	if firstQuery != "" {
		prepared, perr := t5.PrepareAux(firstQuery, -1)
		if perr != nil {
			return &Result{Error: perr}, true
		}
		aq = prepared
	}
	// A WHERE rank override applies to the TVF form too ("FROM tt('a') WHERE
	// rank = 'bm25()'"); the TVF's own rank argument wins when both exist.
	if rankOverride == nil {
		whereOverride, hasOverride, werr := e.fts5RankOverride(s.Where)
		if werr != nil {
			return &Result{Error: werr}, true
		}
		if hasOverride {
			rankOverride = whereOverride
		}
	}
	var rankFn func(int64) (interface{}, error)
	if firstQuery != "" && statementReadsFTS5Rank(s, t5.Name()) {
		spec := &t5.Config().Rank
		if rankOverride != nil {
			spec = rankOverride
		}
		rankFn = func(rowid int64) (interface{}, error) {
			return t5.RankValue(spec, aq, rowid)
		}
	}
	e.ctx.SetFTS5Aux(t5.Name(), aq)
	defer e.ctx.ClearFTS5Aux()
	colDefs := fts5ColDefs(t5)
	if hasArgs {
		// Index-driven universe: the TVF's MATCH arguments select the
		// documents (external-content index-only rows included).
		rowids := t5.SortedMatchRowids(matched)
		var outIDs []int64
		var rows [][]interface{}
		for _, rowid := range rowids {
			vals, verr := t5.DocValues(rowid)
			if verr != nil {
				return &Result{Error: verr}, true
			}
			flat, rerr := fts5FlatRow(t5, rowid, vals, rankFn)
			if rerr != nil {
				return &Result{Error: rerr}, true
			}
			outIDs = append(outIDs, rowid)
			rows = append(rows, flat)
		}
		return e.execSelectOverMaterializedRowids(s, colDefs, rows, outIDs), true
	}
	rowids, rows, err := fts5ScanRows(t5, colDefs, rankFn)
	if err != nil {
		return &Result{Error: err}, true
	}
	return e.execSelectOverMaterializedRowids(s, colDefs, rows, rowids), true
}

// fts5TVFArgText renders a TVF argument value the way xFilter's
// fts5ExtractExprText does for non-locale values: sqlite3_value_text (NULL
// yields "").
func fts5TVFArgText(v interface{}) string {
	x := unwrapTVFArg(v)
	if x == nil {
		return ""
	}
	if b, ok := x.([]byte); ok {
		return string(b)
	}
	return util.SQLiteValueString(x)
}

// fts5TVFRankSpec parses the rank hidden column's TVF argument as a rank
// function specification (fts5_main.c fts5CursorParseRank): every value is
// rendered as text (NULL yields ""); a malformed spec fails with C's
// "parse error in rank function: %s" message.
func fts5TVFRankSpec(v interface{}) (*fts5.RankSpec, error) {
	text := fts5TVFArgText(v)
	spec, err := fts5.ParseRankSpec(text)
	if err != nil {
		return nil, &fts5.RankParseError{Text: text}
	}
	return spec, nil
}

// fts5ScanRows materializes the table's documents into flat rows in colDefs
// order (user columns, hidden table-name column = rowid, rank) plus the
// parallel rowid slice. rankFn drives the rank pseudo-column: NULL without a
// MATCH constraint, the rank function's value with one.
func fts5ScanRows(t5 *fts5.Table, colDefs []sql.ColumnDef, rankFn func(int64) (interface{}, error)) ([]int64, [][]interface{}, error) {
	rowids, values, err := t5.ScanDocs()
	if err != nil {
		return nil, nil, err
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
		rank := interface{}(nil)
		if rankFn != nil {
			v, rerr := rankFn(rowid)
			if rerr != nil {
				return nil, nil, rerr
			}
			rank = v
		}
		row = append(row, rowid, rank)
		rows[i] = row
	}
	return rowids, rows, nil
}

// fts5TopLevelConjuncts splits an expression into its top-level AND operands.
func fts5TopLevelConjuncts(where sql.Expr) []sql.Expr {
	if where == nil {
		return nil
	}
	if bop, ok := where.(*sql.BinaryOp); ok && bop.Operator == "AND" {
		return append(fts5TopLevelConjuncts(bop.Left), fts5TopLevelConjuncts(bop.Right)...)
	}
	return []sql.Expr{where}
}

// fts5MatchConstraintColumn resolves a MATCH left operand to a column index of
// the given table: -1 for a whole-table match. applies=false when the operand
// references a different table.
func fts5MatchConstraintColumn(left sql.Expr, t5 *fts5.Table) (col int, applies bool) {
	ref, ok := left.(*sql.ColumnRef)
	if !ok {
		return -1, false
	}
	switch {
	case ref.Table != "":
		if strings.EqualFold(ref.Table, t5.Name()) {
			if idx := t5.ColumnIndex(ref.Name); idx >= 0 {
				return idx, true
			}
			return -1, true
		}
		return -1, false
	case strings.EqualFold(ref.Name, t5.Name()):
		return -1, true // t1 MATCH — whole table
	case strings.EqualFold(ref.Name, "rank"):
		return -1, false // the rank override is not a row filter
	default:
		if idx := t5.ColumnIndex(ref.Name); idx >= 0 {
			return idx, true // a MATCH — column restricted
		}
		return -1, false
	}
}

// unwrapTVFArg unwraps a ColumnValue wrapper for TVF argument inspection.
func unwrapTVFArg(v interface{}) interface{} {
	return util.UnwrapColumnValue(v)
}

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
// MATCH constraint against the given fts5 table (the table-name reference or
// any of the table's user columns).
func statementHasFTS5Match(s *sql.SelectStmt, t5 *fts5.Table) bool {
	return walkForFTS5Match(s.Where, t5)
}

// walkForFTS5Match walks expressions for MATCH ops whose left operand
// references the table (bare, qualified or a user column of it).
func walkForFTS5Match(expr sql.Expr, t5 *fts5.Table) bool {
	if expr == nil {
		return false
	}
	switch n := expr.(type) {
	case *sql.BinaryOp:
		if n.Operator == "MATCH" {
			if ref, ok := n.Left.(*sql.ColumnRef); ok {
				if strings.EqualFold(ref.Name, t5.Name()) || strings.EqualFold(ref.Table, t5.Name()) {
					return true
				}
				// A column-restricted MATCH (a MATCH 'x') is a table
				// constraint too (fts5MatchConstraintColumn's col resolution).
				if t5.ColumnIndex(ref.Name) >= 0 {
					return true
				}
			}
		}
		if walkForFTS5Match(n.Left, t5) || walkForFTS5Match(n.Right, t5) {
			return true
		}
	case *sql.UnaryOp:
		return walkForFTS5Match(n.Operand, t5)
	}
	return false
}
