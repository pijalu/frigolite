package exec

import (
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// vtabBinding is one hidden-column binding extracted from a WHERE clause.
type vtabBinding struct {
	col string
	val interface{}
}

// hiddenColumnNames collects the HIDDEN column names of a virtual-table
// instance (lowercased). ok is false when the instance declares no hidden
// columns, in which case constraint extraction is skipped entirely.
func hiddenColumnNames(vt vtab.VirtualTable) (map[string]bool, bool) {
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, false
	}
	hc, ok := vt.(vtab.HiddenColumnInfo)
	if !ok {
		return nil, false
	}
	hidden := hc.HiddenColumns()
	if len(hidden) == 0 {
		return nil, false
	}
	names := make(map[string]bool)
	for i, c := range ci.Columns() {
		if hidden[i] {
			names[strings.ToLower(c)] = true
		}
	}
	return names, true
}

// extractHiddenConstraintCombos walks the top-level AND conjuncts of the
// WHERE clause collecting equality and IN bindings on the virtual table's
// hidden columns, then expands multi-valued bindings (IN lists) into one
// combination per element — SQLite runs xFilter once per IN value and
// concatenates the resulting streams.
//
// A nil result means "no pushable constraints": the caller materializes the
// instance once with its plain argument-derived state.
func (e *Engine) extractHiddenConstraintCombos(vt vtab.VirtualTable, where sql.Expr) [][]vtabBinding {
	names, ok := hiddenColumnNames(vt)
	if !ok || where == nil {
		return nil
	}
	if _, ok := vt.(vtab.HiddenConstraintSetter); !ok {
		return nil
	}
	bindings := map[string][]interface{}{}
	var order []string
	for _, conj := range splitAndConjuncts(where) {
		e.collectHiddenConjunctBinding(bindings, &order, conj, names)
	}
	if len(order) == 0 {
		return nil
	}
	return expandHiddenCombos(order, bindings)
}

// collectHiddenConjunctBinding dispatches one AND conjunct to the equality or
// IN collector.
func (e *Engine) collectHiddenConjunctBinding(bindings map[string][]interface{}, order *[]string, conj sql.Expr, names map[string]bool) {
	switch c := conj.(type) {
	case *sql.BinaryOp:
		e.collectHiddenEqualityBinding(c, bindings, order, names)
	case *sql.InList:
		e.collectHiddenInListBinding(c, bindings, order, names)
	}
}

// collectHiddenEqualityBinding records one hidden-column equality binding.
//
// Only a CONSTANT expression is a usable xFilter constraint (sqlite3 xFilter
// constraint argv). A bare column reference on the other side (t4.id =
// vt4.root) is JOIN loop machinery, not a binding: sqlite's xBestIndex leaves
// such terms unprojected and the core filters them per joined row (closure01
// 6.1 regression: attempting to evaluate the outer column with no row raised
// "unusable root value").
func (e *Engine) collectHiddenEqualityBinding(op *sql.BinaryOp, bindings map[string][]interface{}, order *[]string, names map[string]bool) {
	if op.Operator != "=" && op.Operator != "==" {
		return
	}
	col, valExpr, bound := matchHiddenEquality(op, names)
	if !bound {
		return
	}
	if _, isRef := valExpr.(*sql.ColumnRef); isRef {
		return
	}
	v, err := e.evalExpr(valExpr, nil)
	if err != nil {
		return
	}
	recordHiddenBinding(strings.ToLower(col), v, bindings, order)
}

// collectHiddenInListBinding records one binding per evaluable IN element
// (a partially usable IN binds only its evaluable elements).
func (e *Engine) collectHiddenInListBinding(list *sql.InList, bindings map[string][]interface{}, order *[]string, names map[string]bool) {
	ref, ok := list.Operand.(*sql.ColumnRef)
	if !ok || list.Negated || !names[strings.ToLower(ref.Name)] {
		return
	}
	key := strings.ToLower(ref.Name)
	for _, elem := range list.List {
		v, err := e.evalExpr(elem, nil)
		if err != nil {
			continue
		}
		recordHiddenBinding(key, v, bindings, order)
	}
}

// recordHiddenBinding appends one binding value, tracking first-seen column
// order so combination expansion is deterministic.
func recordHiddenBinding(key string, v interface{}, bindings map[string][]interface{}, order *[]string) {
	if _, seen := bindings[key]; !seen {
		*order = append(*order, key)
	}
	bindings[key] = append(bindings[key], v)
}

// expandHiddenCombos expands multi-valued bindings (IN lists) into one
// combination per element — SQLite runs xFilter once per IN value and
// concatenates the resulting streams.
func expandHiddenCombos(order []string, bindings map[string][]interface{}) [][]vtabBinding {
	combos := [][]vtabBinding{{}}
	for _, key := range order {
		var next [][]vtabBinding
		for _, combo := range combos {
			for _, v := range bindings[key] {
				ext := make([]vtabBinding, len(combo)+1)
				copy(ext, combo)
				ext[len(combo)] = vtabBinding{col: key, val: v}
				next = append(next, ext)
			}
		}
		combos = next
	}
	return combos
}

// matchHiddenEquality recognizes col=const / const=col over a hidden column.
func matchHiddenEquality(op *sql.BinaryOp, names map[string]bool) (col string, valExpr sql.Expr, bound bool) {
	// Any evaluable RHS/LHS expression binds (literals, parameters and — via
	// the evaluator — uncorrelated scalar subqueries); the caller falls back
	// to plain filtering when evaluation fails.
	if ref, ok := op.Left.(*sql.ColumnRef); ok && names[strings.ToLower(ref.Name)] {
		return ref.Name, op.Right, true
	}
	if ref, ok := op.Right.(*sql.ColumnRef); ok && names[strings.ToLower(ref.Name)] {
		return ref.Name, op.Left, true
	}
	return "", nil, false
}

// tryMaterializeEponymousVtab resolves a bare FROM reference to an
// eponymous-only module's implicit table (series.c: FROM generate_series
// with hidden-column constraints). handled is false when the reference is
// not an eponymous module, letting the caller fall back to ordinary table
// resolution; schema-prefixed names (main.generate_series) are resolved by
// stripping the prefix because an eponymous table exists in every schema.
func (e *Engine) tryMaterializeEponymousVtab(ref sql.TableRef, opts execquery.VtabScanOptions) ([]sql.ColumnDef, [][]interface{}, []int64, error, bool) {
	name := strings.ToLower(ref.Name)
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	module, ok := e.vtabs.Find(name)
	if !ok {
		return nil, nil, nil, nil, false
	}
	if !vtab.ModuleIsEponymous(module) {
		return nil, nil, nil, nil, false
	}
	args, err := evalVtabArgs(e, ref, nil)
	if err != nil {
		return nil, nil, nil, err, true
	}
	valArgs, verr := evalVtabArgValues(e, ref, nil)
	if verr != nil {
		return nil, nil, nil, verr, true
	}
	rows, rowids, rerr := e.materializeVtabModule(module, args, valArgs, opts, nil)
	if rerr != nil {
		return nil, nil, nil, rerr, true
	}
	defs, derr := e.vtabColumnDefsFromModule(module, args, valArgs, rows)
	if derr != nil {
		return nil, nil, nil, derr, true
	}
	return defs, rows, rowids, nil, true
}

// vtabColumnDefsFromModule builds projected column definitions by
// instantiating a module's default instance and reading its declared schema.
// rows backs the full-width detection that enables HIDDEN column defs.
func (e *Engine) vtabColumnDefsFromModule(module vtab.Module, strArgs []string, valArgs []interface{}, rows [][]interface{}) ([]sql.ColumnDef, error) {
	vt, err := createVtabModuleConn(module, strArgs, valArgs)
	if err != nil {
		return nil, err
	}
	return vtabColumnDefs(vt, rows), nil
}

// eponymousVtabColDefs reports the column definitions (hidden columns
// included and flagged) of an eponymous-only module's implicit table for
// PRAGMA table_xinfo / table_info. found is false when tableName does not
// name an eponymous module.
func (e *Engine) eponymousVtabColDefs(tableName string) (colDefs []sql.ColumnDef, found bool, err error) {
	name := strings.ToLower(tableName)
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	module, ok := e.vtabs.Find(name)
	if !ok {
		return nil, false, nil
	}
	if !vtab.ModuleIsEponymous(module) {
		return nil, false, nil
	}
	vt, cerr := createVtabModule(module, nil, nil)
	if cerr != nil {
		return nil, true, cerr
	}
	ci, ok := vt.(vtab.ColumnInfo)
	if !ok {
		return nil, true, nil
	}
	var hidden map[int]bool
	if hc, ok := vt.(vtab.HiddenColumnInfo); ok {
		hidden = hc.HiddenColumns()
	}
	for i, c := range ci.Columns() {
		cd := sql.ColumnDef{Name: c}
		if hidden[i] {
			cd.Hidden = true
		}
		colDefs = append(colDefs, cd)
	}
	return colDefs, true, nil
}

// residualHiddenWhere strips WHERE conjuncts that bind hidden columns of an
// instance implementing HiddenConstraintSetter (equality / IN). Those
// bindings are consumed by xFilter parity; leaving them in the residual WHERE
// would double-filter against echoed values that are intentionally NULL
// (e.g. transitive_closure's root column).
func residualHiddenWhere(vt vtab.VirtualTable, where sql.Expr) (sql.Expr, bool) {
	names, ok := hiddenColumnNames(vt)
	if !ok || where == nil {
		return where, false
	}
	if _, ok := vt.(vtab.HiddenConstraintSetter); !ok {
		return where, false
	}
	var kept []sql.Expr
	stripped := false
	for _, conj := range splitAndConjuncts(where) {
		if bindsHiddenColumn(conj, names) {
			stripped = true
			continue
		}
		kept = append(kept, conj)
	}
	if !stripped {
		return where, false
	}
	return joinConjuncts(kept), true
}

// bindsHiddenColumn reports whether expr is an equality or IN constraint on a
// hidden column name.
func bindsHiddenColumn(expr sql.Expr, names map[string]bool) bool {
	switch c := expr.(type) {
	case *sql.BinaryOp:
		if c.Operator != "=" && c.Operator != "==" {
			return false
		}
		if cr, ok := c.Left.(*sql.ColumnRef); ok && names[strings.ToLower(cr.Name)] {
			return true
		}
		if cr, ok := c.Right.(*sql.ColumnRef); ok && names[strings.ToLower(cr.Name)] {
			return true
		}
	case *sql.InList:
		if cr, ok := c.Operand.(*sql.ColumnRef); ok && names[strings.ToLower(cr.Name)] {
			return true
		}
	}
	return false
}

// joinConjuncts rebuilds an AND chain from the kept conjuncts.
func joinConjuncts(list []sql.Expr) sql.Expr {
	switch len(list) {
	case 0:
		return nil
	case 1:
		return list[0]
	default:
		cur := list[0]
		for _, nxt := range list[1:] {
			cur = &sql.BinaryOp{Left: cur, Right: nxt, Operator: "AND"}
		}
		return cur
	}
}

// spellfixConstraintColumn resolves the spellfix column index of a
// constraint's column side: the ColumnRef on either side of the operator
// mapping through vtab.SpellfixColumnIndex, or -1 for rowid/_rowid_/oid.
// Returns col=-2 when neither side is a spellfix column; flipped reports a
// value-on-the-left comparison (the caller mirrors the operator).
func spellfixConstraintColumn(bo *sql.BinaryOp) (col int, flipped bool) {
	if cr, ok := bo.Left.(*sql.ColumnRef); ok {
		if idx, found := vtab.SpellfixColumnIndex(cr.Name); found {
			return idx, false
		}
		if isRowidName(cr.Name) {
			return -1, false
		}
	}
	if cr, ok := bo.Right.(*sql.ColumnRef); ok {
		if idx, found := vtab.SpellfixColumnIndex(cr.Name); found {
			return idx, true
		}
		if isRowidName(cr.Name) {
			return -1, true
		}
	}
	return -2, false
}

// isRowidName reports whether name is one of the rowid aliases.
func isRowidName(name string) bool {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return true
	}
	return false
}

// MatchConstraintSetter is implemented by virtual-table instances whose rows
// are generated from a `column MATCH <target>` constraint (approximate_match).
type MatchConstraintSetter interface {
	SetMatchConstraint(column, target string)
}

// residualDropMatch removes `col MATCH <literal>` conjuncts consumed by a
// MatchConstraintSetter instance.
func residualDropMatch(where sql.Expr) (sql.Expr, bool) {
	if where == nil {
		return where, false
	}
	var kept []sql.Expr
	changed := false
	for _, conj := range splitAndConjuncts(where) {
		consumed := false
		if bo, ok := conj.(*sql.BinaryOp); ok && strings.EqualFold(bo.Operator, "MATCH") {
			if _, isRef := bo.Left.(*sql.ColumnRef); isRef {
				consumed = true
			}
		}
		if consumed {
			changed = true
			continue
		}
		kept = append(kept, conj)
	}
	if !changed {
		return where, false
	}
	return joinConjuncts(kept), true
}
