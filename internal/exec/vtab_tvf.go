// Vtab (virtual-table) materialization helpers shared by table-valued
// function execution: module lookup, argument evaluation, cursor draining,
// and column-definition derivation (pragma_table.go split; behavior unchanged).
package exec

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// materializeVtabTableFunc materializes a table-valued virtual-table function
// reference (e.g. FROM generate_series(1,256)) into column definitions and
// rows. where carries the enclosing WHERE clause so hidden-column equality /
// IN constraints can be pushed into the instance before row generation
// (series.c xBestIndex/xFilter parity). It returns an error wrapping "no such
// module" when the name is not a registered vtab module, so callers can fall
// back to ordinary table lookup.
func (e *Engine) materializeVtabTableFunc(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	args, err := evalVtabArgs(e, ref, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	valArgs, verr := evalVtabArgValues(e, ref, nil)
	if verr != nil {
		return nil, nil, nil, verr
	}
	rows, rowids, err := e.materializeVtabModule(module, args, valArgs, opts, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	vt, err := createVtabModuleConn(module, args, valArgs)
	if err != nil {
		return nil, nil, nil, err
	}
	return vtabColumnDefs(vt, rows), rows, rowids, nil
}

// materializeVtabTableFuncInRow is MaterializeVtabTableFunc with argument
// expressions evaluated against a specific outer row (correlation).
func (e *Engine) materializeVtabTableFuncInRow(ref sql.TableRef, row Row) ([]sql.ColumnDef, [][]interface{}, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	args, err := evalVtabArgs(e, ref, row)
	if err != nil {
		return nil, nil, err
	}
	valArgs, verr := evalVtabArgValues(e, ref, row)
	if verr != nil {
		return nil, nil, verr
	}
	vt, err := createVtabModuleConn(module, args, valArgs)
	if err != nil {
		return nil, nil, err
	}
	rows, err := readVtabRows(vt)
	if err != nil {
		return nil, nil, err
	}
	return vtabColumnDefs(vt, rows), rows, nil
}

// materializeCorrelatedVTabFunc materializes a table-valued vtab function
// whose arguments reference left-side columns (e.g.
// FROM t, json_each(t.json) AS jx): one Connect per left row with that row
// as the argument evaluation context, all right rows concatenated with the
// index of the left row each batch came from (SQLite correlation).
func (e *Engine) materializeCorrelatedVTabFunc(ref sql.TableRef, leftRows []RowMap, where sql.Expr) ([]sql.ColumnDef, []RowMap, []int, error) {
	module, ok := e.vtabs.Find(strings.ToLower(ref.Name))
	if !ok {
		return nil, nil, nil, fmt.Errorf("no such module: %s", ref.Name)
	}
	// WHERE pushdown parity: conjuncts referencing only outer-side columns
	// gate TVF materialization per row. sqlite evaluates such terms before
	// entering the inner loop, so a failing json_valid() check prevents
	// json_each from ever seeing an invalid argument (json102-1011). A
	// conjunct that cannot be evaluated against the outer row alone
	// (references TVF or later-join columns) is not a gating term; it still
	// applies in the outer WHERE pass.
	conjuncts := splitAndConjuncts(where)
	var colDefs []sql.ColumnDef
	var allMaps []RowMap
	var leftIdx []int
	for li, left := range leftRows {
		if !e.outerConjunctsPass(conjuncts, left) {
			continue
		}
		defs, maps, err := e.materializeCorrelatedRow(module, ref, left, li)
		if err != nil {
			return nil, nil, nil, err
		}
		if colDefs == nil {
			colDefs = defs
		}
		for range maps {
			leftIdx = append(leftIdx, li)
		}
		allMaps = append(allMaps, maps...)
	}
	return colDefs, allMaps, leftIdx, nil
}

// materializeCorrelatedRow connects the TVF module once for one outer row
// (that row as the argument-evaluation context) and returns the connection's
// column defs plus its rows as row maps (SQLite correlation).
func (e *Engine) materializeCorrelatedRow(module vtab.Module, ref sql.TableRef, left RowMap, li int) ([]sql.ColumnDef, []RowMap, error) {
	args, err := evalVtabArgs(e, ref, left)
	if err != nil {
		return nil, nil, err
	}
	valArgs, verr := evalVtabArgValues(e, ref, left)
	if verr != nil {
		return nil, nil, verr
	}
	vt, err := createVtabModuleConn(module, args, valArgs)
	if err != nil {
		return nil, nil, err
	}
	rows, rerr := readVtabRows(vt)
	if rerr != nil {
		return nil, nil, rerr
	}
	defs := vtabColumnDefs(vt, rows)
	maps := make([]RowMap, 0, len(rows))
	for _, row := range rows {
		m := make(RowMap)
		for i, val := range row {
			if i < len(defs) {
				m[defs[i].Name] = val
			}
		}
		maps = append(maps, m)
	}
	return defs, maps, nil
}

// splitAndConjuncts flattens an AND-tree into its conjuncts.
func splitAndConjuncts(expr sql.Expr) []sql.Expr {
	if expr == nil {
		return nil
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		return append(splitAndConjuncts(bin.Left), splitAndConjuncts(bin.Right)...)
	}
	return []sql.Expr{expr}
}

// outerConjunctsPass reports whether every conjunct fully bound by the
// outer row passes. sqlite pushes only WHERE terms whose column
// references are ALL provided by the outer loop (whereLoop term masking):
// a conjunct referencing TVF or later-join columns resolves to NULL on
// the left row (evaluator fallback) and must NOT gate materialization.
func (e *Engine) outerConjunctsPass(conjuncts []sql.Expr, left RowMap) bool {
	for _, c := range conjuncts {
		if !exprBoundInRow(c, left) {
			continue // references unavailable columns: not a gating term
		}
		pass, err := e.evalBool(c, left)
		if err != nil {
			continue // evaluation against the outer row alone failed
		}
		if !pass {
			return false
		}
	}
	return true
}

// exprBoundInRow reports whether every column reference in expr resolves
// to a key present in row. Subqueries (scalar or EXISTS) are conservatively
// unbound. This mirrors sqlite's rule that a pushed-down WHERE term must be
// completely determined by the outer loop's cursor set.
func exprBoundInRow(expr sql.Expr, row RowMap) bool {
	bound := true
	execquery.WalkExprFull(expr, func(n sql.Expr) {
		if !bound {
			return
		}
		switch v := n.(type) {
		case *sql.ColumnRef:
			if !rowHasColumn(v, row) {
				bound = false
			}
		case *sql.Subquery, *sql.ExistsExpr:
			bound = false
		}
	})
	return bound
}

// rowHasColumn reports whether the row map provides the column reference.
// Row maps are keyed by bare column names; a qualified "t.c" (or "db.t.c")
// resolves through the evaluator's unqualified-name fallback for the table
// currently being scanned, so the bare key is checked as a last resort.
func rowHasColumn(v *sql.ColumnRef, row RowMap) bool {
	if _, ok := row[v.Name]; ok {
		return true
	}
	if v.Table == "" {
		return false
	}
	if _, ok := row[v.Table+"."+v.Name]; ok {
		return true
	}
	parts := strings.SplitN(v.Table, ".", 2)
	if len(parts) == 2 {
		if _, ok := row[parts[1]+"."+v.Name]; ok {
			return true
		}
	}
	return false
}

// evalVtabArgs evaluates a vtab reference's argument expressions to strings.
// A NULL argument becomes the empty string (e.g. json_each(NULL) yields no
// rows, matching SQLite's NULL handling).
func evalVtabArgs(e *Engine, ref sql.TableRef, row Row) ([]string, error) {
	args := make([]string, 0, len(ref.Args))
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		u := util.UnwrapColumnValue(v)
		if u == nil {
			args = append(args, "")
			continue
		}
		if b, ok := u.([]byte); ok {
			// BLOB argument: pass the raw image through verbatim
			// (zipfile(X'504b...') binds an in-memory archive).
			args = append(args, string(b))
			continue
		}
		args = append(args, fmt.Sprintf("%v", u))
	}
	return args, nil
}

// evalVtabArgValues evaluates a vtab reference's argument expressions to SQL
// values, preserving types (BLOB/JSONB inputs survive intact).
func evalVtabArgValues(e *Engine, ref sql.TableRef, row Row) ([]interface{}, error) {
	args := make([]interface{}, 0, len(ref.Args))
	for _, a := range ref.Args {
		v, err := e.evalExpr(a, row)
		if err != nil {
			return nil, err
		}
		args = append(args, util.UnwrapColumnValue(v))
	}
	return args, nil
}

// createVtabModule materializes vt rows via the ValueModule interface when
// typed argument VALUES are available; created-virtual-table re-instantiation
// (DML target resolution, schema SQL re-parse) supplies only the stored TEXT
// argv and must take the xCreate(string argv) path — preferring the typed
// constructor with a nil value list would drop every argument
// (zipfile INSERT reported "constructor requires one argument").
//
// Only a NON-NIL value list indicates runtime-typed arguments (table-valued
// function call sites); a created virtual table re-instantiated from stored
// schema SQL has TEXT argv only.
func createVtabModule(module vtab.Module, strArgs []string, valArgs []interface{}) (vtab.VirtualTable, error) {
	if vm, ok := module.(vtab.ValueModule); ok && valArgs != nil {
		return vm.CreateWithValues(valArgs)
	}
	return module.Create(strArgs)
}

// createVtabModuleConn is the xConnect-side instance constructor: like
// createVtabModule it prefers typed argument VALUES when available and falls
// back to the stored TEXT argv otherwise (preferring the ValueModule
// value constructor with a nil value list would drop every argument).
// CREATE-VIRTUAL-TABLE-form connections bind through Create so the module can
// give create-specific diagnostics (e.g. missing-file archives are legal on
// create, an error on plain connect), while table-valued-function contexts go
// through Connect.
func createVtabModuleConn(module vtab.Module, strArgs []string, valArgs []interface{}) (vtab.VirtualTable, error) {
	if vm, ok := module.(vtab.ValueModule); ok && valArgs != nil {
		// Connect-side sites use the xConnect analogue even for typed args,
		// so a module can give function-specific diagnostics
		// (zipfile: FROM zipfile() must report the function-arity message).
		return vm.ConnectWithValues(valArgs)
	}
	if strArgs != nil {
		return module.Connect(strArgs)
	}
	return module.Create(strArgs)
}

// readVtabRows reads every row from an opened virtual table.
func readVtabRows(vt vtab.VirtualTable) ([][]interface{}, error) {
	rows, _, err := readVtabRowsWithRowids(vt, -1)
	return rows, err
}

// readVtabRowsWithRowids reads every row plus native rowids when the cursor
// exposes them (vtab xRowid parity, e.g. generate_series rowid == value).
// rowids is nil when the cursor has no rowid support. maxRows caps the row
// count (LIMIT pushdown parity); negative means unlimited.
func readVtabRowsWithRowids(vt vtab.VirtualTable, maxRows int64) ([][]interface{}, []int64, error) {
	cur, err := vt.Open()
	if err != nil {
		return nil, nil, err
	}
	return readCursorRowsWithRowids(cur, maxRows)
}

// readCursorRowsWithRowids drains an ALREADY-OPENED cursor (and closes it),
// collecting every row plus native rowids when the cursor exposes them (vtab
// xRowid parity). It is the cursor-side half of readVtabRowsWithRowids, split
// so the xBestIndex/xFilter glue (vtab_bestindex.go) can run FilterPlan on a
// cursor before its rows are read. maxRows caps the row count (LIMIT pushdown
// parity); negative means unlimited.
func readCursorRowsWithRowids(cur vtab.Cursor, maxRows int64) ([][]interface{}, []int64, error) {
	defer cur.Close()
	ridCur, hasRowids := cur.(vtab.RowidCursor)
	var rows [][]interface{}
	var rowids []int64
	for cur.Next() {
		var row []interface{}
		for i := 0; ; i++ {
			val, err := cur.Column(i)
			if err != nil {
				break
			}
			row = append(row, val)
		}
		rows = append(rows, row)
		if hasRowids {
			rowids = append(rowids, ridCur.Rowid())
		}
		if maxRows >= 0 && int64(len(rows)) >= maxRows {
			break
		}
	}
	return rows, rowids, nil
}

// vtabColumnDefs builds column definitions from the vtab's declared columns,
// falling back to c0/c1/... names derived from the first row's width.
// HIDDEN columns are included but flagged Hidden (excluded from SELECT *
// and PRAGMA table_info, still resolvable by explicit references — this
// matches SQLite's series.c tests compiled with
// SQLITE_SERIES_CONSTRAINT_VERIFY=1, where the core re-verifies hidden
// constraints per row). Hidden columns are only declared when the cursor
// actually serves their values (row width == full declared schema).
func vtabColumnDefs(vt vtab.VirtualTable, rows [][]interface{}) []sql.ColumnDef {
	var colDefs []sql.ColumnDef
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		colDefs = declaredVtabColumnDefs(vt, ci, rows)
	}
	if len(colDefs) == 0 && len(rows) > 0 {
		for i := range rows[0] {
			colDefs = append(colDefs, sql.ColumnDef{Name: fmt.Sprintf("c%d", i)})
		}
	}
	return colDefs
}

// declaredVtabColumnDefs builds defs from the instance's declared columns,
// flagging HIDDEN ones. Hidden columns are only declared when the cursor
// actually serves their values (row width == full declared schema).
func declaredVtabColumnDefs(vt vtab.VirtualTable, ci vtab.ColumnInfo, rows [][]interface{}) []sql.ColumnDef {
	var hidden map[int]bool
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden = hc.HiddenColumns()
	}
	// The cursor must provide one value per declared column (including
	// HIDDEN ones) for the hidden defs to be backed by data.
	fullWidth := len(rows) > 0 && len(rows[0]) == len(ci.Columns())
	var colDefs []sql.ColumnDef
	for i, c := range ci.Columns() {
		cd := sql.ColumnDef{Name: c}
		if hidden[i] {
			if !fullWidth {
				continue
			}
			cd.Hidden = true
		}
		colDefs = append(colDefs, cd)
	}
	return colDefs
}
