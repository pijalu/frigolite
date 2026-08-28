package exec

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
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
		switch c := conj.(type) {
		case *sql.BinaryOp:
			if c.Operator != "=" && c.Operator != "==" {
				continue
			}
			col, valExpr, bound := matchHiddenEquality(c, names)
			if !bound {
				continue
			}
			// Only a CONSTANT expression is a usable xFilter constraint
			// (sqlite3 xFilter constraint argv). A bare column reference on the other
			// side (t4.id = vt4.root) is JOIN loop machinery, not a binding:
			// sqlite's xBestIndex leaves such terms unprojected and the
			// core filters them per joined row (closure01 6.1 regression:
			// attempting to evaluate the outer column with no row raised
			// "unusable root value").
			if _, isRef := valExpr.(*sql.ColumnRef); isRef {
				continue
			}
			v, err := e.evalExpr(valExpr, nil)
			if err != nil {
				continue
			}
			key := strings.ToLower(col)
			if _, seen := bindings[key]; !seen {
				order = append(order, key)
			}
			bindings[key] = append(bindings[key], v)
		case *sql.InList:
			ref, ok := c.Operand.(*sql.ColumnRef)
			if !ok || c.Negated || !names[strings.ToLower(ref.Name)] {
				continue
			}
			key := strings.ToLower(ref.Name)
			for _, elem := range c.List {
				v, err := e.evalExpr(elem, nil)
				if err != nil {
					continue // partially usable IN: only evaluable elements bind
				}
				if _, seen := bindings[key]; !seen {
					order = append(order, key)
				}
				bindings[key] = append(bindings[key], v)
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
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

// applyVtabConstraints applies one combination of hidden-column bindings to
// the instance and validates it (series.c bStartSeen parity).
func applyVtabConstraints(vt vtab.VirtualTable, combo []vtabBinding) error {
	setter, ok := vt.(vtab.HiddenConstraintSetter)
	if !ok {
		return fmt.Errorf("virtual table does not accept constraints")
	}
	for _, b := range combo {
		if err := setter.SetHiddenConstraint(b.col, b.val); err != nil {
			return err
		}
	}
	if iv, ok := vt.(vtab.InstanceValidator); ok {
		return iv.ValidateInstance()
	}
	return nil
}

// materializeVtabModule generates the rows of a virtual-table function
// reference, applying hidden-column WHERE constraints (one materialization
// per IN-value combination, concatenated — SQLite runs xFilter per value).
// Native rowids are returned alongside (xRowid parity).
//
// bindSchema (optional) is invoked on every instance the materialization
// creates (the primary one and each constraint-combination clone). Created
// virtual tables whose module is schema-bound (rtree) need the resolved
// db+table identity before their first read; table-valued functions pass nil.
// A binder error aborts the materialization.
func (e *Engine) materializeVtabModule(module vtab.Module, strArgs []string, valArgs []interface{}, opts execquery.VtabScanOptions, bindSchema func(vtab.VirtualTable) error) ([][]interface{}, []int64, error) {
	if bindSchema == nil {
		bindSchema = func(vtab.VirtualTable) error { return nil }
	}
	vt, err := createVtabModuleConn(module, strArgs, valArgs)
	if err != nil {
		return nil, nil, err
	}
	// Opted-in modules re-arm this statement's operator-overload probing
	// (vtabH: overridden like()/glob()/regexp() functions are invoked once
	// per TRUE operator evaluation while rows of such an instance feed the
	// query).
	if oc, ok := vt.(vtab.OperatorOverloadCounter); ok && oc.CountOperatorOverloads() {
		e.overloadProbe = true
	}
	if err := bindSchema(vt); err != nil {
		return nil, nil, err
	}
	// series.c narrows the generated range from equality/range constraints on
	// the value column inside xFilter (iMin/iMax); without it a query like
	// FROM generate_series(MinI64, MaxI64, 2) WHERE value BETWEEN 1 AND 5
	// would materialize 2^62 rows before filtering.
	if nr, ok := vt.(vtab.ValueRangeNarrower); ok {
		if b, has := seriesValueRange(opts.Where); has {
			// series.c widens the implicit START/STOP defaults to the full
			// int64 range when only the VALUE column is constrained
			// (xFilter idxNum 0x05/0x06 rules) — e.g. tabfunc01-1520:
			// FROM generate_series(9223372036854774784) WHERE value<=X must
			// not stop at the default STOP of 4294967295.
			if ex, ok2 := vt.(vtab.ValueConstraintExpander); ok2 {
				ex.ExpandValueDefaults(b.lower, b.upper)
			}
			nr.NarrowValueRange(b.min, b.max)
		}
	}
	if opts.Residual != nil {
		rw, changed := residualSeriesWhere(vt, opts.Where)
		if !changed {
			rw, changed = residualHiddenWhere(vt, opts.Where)
		}
		if _, isMS := vt.(vtab.MatchConstraintSetter); isMS {
			if r2, ch2 := residualDropMatch(rw); ch2 {
				rw = r2
				changed = true
			}
		}
		if changed {
			*opts.Residual = rw
		}
	}
	// `col MATCH <literal>` on a module implementing MatchConstraintSetter is
	// consumed by the instance (it drives row generation); drop it from the
	// residual WHERE so the MATCH operator is not re-evaluated as a filter.
	// unionvtab-style source selection from the WHERE rowid interval:
	// chosen sources are scanned per the module's declared ranges and the
	// rowid conjuncts are OMITTED (not re-applied as filters).
	if rc, ok := vt.(vtab.RowidRangeConsumer); ok && rc != nil && opts.Where != nil {
		e.consumeVTabRowidRange(vt, opts.Where)
		if cleaned, changed := e.dropVTabRowidConjuncts(vt, opts.Where); changed {
			opts.Where = cleaned
			if opts.Residual != nil {
				*opts.Residual = cleaned
			}
		}
	}
	// rtree-family coordinate/id pushdown: single-table conjuncts on vtab
	// columns are handed to constraintSink and removed from the residual so
	// the core never re-applies SQL affinity to them (sqlite3 argvConsumed).
	if sink, ok := vt.(vtab.ConstraintSink); ok && opts.Where != nil {
		cols := declaredColumnLower(vt)
		var consumed []sql.Expr
		for _, conj := range splitAndConjuncts(opts.Where) {
			handled, perr := e.rtreePushConjunct(sink, cols, conj)
			if perr != nil {
				return nil, nil, perr
			}
			if handled {
				consumed = append(consumed, conj)
			}
		}
		if len(consumed) > 0 {
			opts.Where = dropConsumedConjuncts(opts.Where, consumed)
			if opts.Residual != nil {
				*opts.Residual = opts.Where
			}
		}
	}
	// fstree-style path narrowing: the FIRST usable GLOB/LIKE/EQ conjunct on
	// the path column hands its value to the instance (xFilter parity).
	e.bindPathConstraint(vt, opts.Where)
	// spellfix1 plan-column constraints (word MATCH, langid/top/scope =,
	// distance </<=, rowid =) bind onto the instance (xBestIndex argv/omit
	// parity) and drop from the residual WHERE.
	if sf, ok := vt.(vtab.SpellfixConstraintSink); ok && opts.Where != nil {
		var consumed []sql.Expr
		conjuncts := splitAndConjuncts(opts.Where)
		// spellfix1BestIndex scans all constraints before deciding the plan;
		// a MATCH term suppresses rowid consumption regardless of order, so
		// MATCH conjuncts are offered to the sink first.
		ordered := make([]sql.Expr, 0, len(conjuncts))
		for _, conj := range conjuncts {
			if bo, isOp := conj.(*sql.BinaryOp); isOp && strings.EqualFold(bo.Operator, "MATCH") {
				ordered = append(ordered, conj)
			}
		}
		for _, conj := range conjuncts {
			bo, isOp := conj.(*sql.BinaryOp)
			if !isOp {
				continue
			}
			if strings.EqualFold(bo.Operator, "MATCH") {
				continue // already offered in pass 1
			}
			ordered = append(ordered, conj)
		}
		for _, conj := range ordered {
			bo, isOp := conj.(*sql.BinaryOp)
			if !isOp {
				continue
			}
			col, flipped := spellfixConstraintColumn(bo)
			if col == -2 {
				continue
			}
			op := strings.ToUpper(bo.Operator)
			if flipped {
				op = map[string]string{"<": ">", ">": "<", "<=": ">=", ">=": "<="}[op]
				if op == "" {
					continue
				}
			}
			var valExpr sql.Expr
			if _, isRef := bo.Left.(*sql.ColumnRef); isRef {
				valExpr = bo.Right
			} else {
				valExpr = bo.Left
			}
			v, verr := e.evalExpr(valExpr, nil)
			if verr != nil {
				continue
			}
			if sf.PushSpellfixConstraint(col, op, util.UnwrapColumnValue(v)) {
				consumed = append(consumed, conj)
			}
		}
		if len(consumed) > 0 {
			opts.Where = dropConsumedConjuncts(opts.Where, consumed)
			if opts.Residual != nil {
				*opts.Residual = opts.Where
			}
		}
	}
	setter, hookOK := vt.(vtab.MatchConstraintSetter)
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "HOOK whereNil=%v setterOK=%v conj=%d\n", opts.Where == nil, hookOK, len(splitAndConjuncts(opts.Where)))
	}
	if hookOK && opts.Where != nil {
		for _, conj := range splitAndConjuncts(opts.Where) {
			if os.Getenv("CL_DBG") != "" {
				fmt.Fprintf(os.Stderr, "HOOKC %T %+v\n", conj, conj)
			}
			if bo, isOp := conj.(*sql.BinaryOp); isOp && strings.EqualFold(bo.Operator, "MATCH") {
				if cr, isRef := bo.Left.(*sql.ColumnRef); isRef {
					// The MATCH argument may be a literal or any constant
					// expression (amatch1-3.0: format('%.81c','a')).
					v, verr := e.evalExpr(bo.Right, nil)
					if verr == nil {
						target := fmt.Sprintf("%v", util.UnwrapColumnValue(v))
						setter.SetMatchConstraint(cr.Name, target)
					}
				}
			}
		}
	}
	combos := e.extractHiddenConstraintCombos(vt, opts.Where)
	if len(combos) == 0 {
		// No pushable constraints: validate the plain instance (series.c
		// bStartSeen rejects an unusable/missing START binding) and read.
		if iv, ok := vt.(vtab.InstanceValidator); ok {
			if verr := iv.ValidateInstance(); verr != nil {
				return nil, nil, verr
			}
		}
		return readVtabRowsWithRowids(vt, opts.MaxRows)
	}
	var all [][]interface{}
	var allRowids []int64
	for _, combo := range combos {
		// Each combination gets a pristine instance so bindings never
		// accumulate across combinations.
		instance, err := createVtabModuleConn(module, strArgs, valArgs)
		if err != nil {
			return nil, nil, err
		}
		if err := bindSchema(instance); err != nil {
			return nil, nil, err
		}
		if err := applyVtabConstraints(instance, combo); err != nil {
			return nil, nil, err
		}
		remaining := opts.MaxRows - int64(len(all))
		rows, rowids, err := readVtabRowsWithRowids(instance, remaining)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, rows...)
		allRowids = append(allRowids, rowids...)
		if opts.MaxRows >= 0 && int64(len(all)) >= opts.MaxRows {
			break
		}
	}
	return all, allRowids, nil
}

// bindPathConstraint hands the FIRST usable GLOB/LIKE/EQ conjunct on the
// path column to a filesystem-tree instance (fstree xFilter parity —
// fstreeBestIndex returns at the first matching constraint and fstreeFilter
// derives the recursion root from the pattern prefix). The conjunct STAYS in
// the residual WHERE: sqlite does not set omit for this module, so the core
// still filters rows.
func (e *Engine) bindPathConstraint(vt vtab.VirtualTable, where sql.Expr) {
	pf, ok := vt.(vtab.PathFilterSink)
	if !ok || where == nil {
		return
	}
	for _, conj := range splitAndConjuncts(where) {
		bo, isOp := conj.(*sql.BinaryOp)
		if !isOp {
			continue
		}
		var valExpr sql.Expr
		var op vtab.PathConstraintOp
		has := false
		switch strings.ToUpper(bo.Operator) {
		case "GLOB":
			if strings.EqualFold(colName(bo.Left), "path") {
				valExpr, op, has = bo.Right, vtab.PathConstraintGlob, true
			}
		case "LIKE":
			if strings.EqualFold(colName(bo.Left), "path") {
				valExpr, op, has = bo.Right, vtab.PathConstraintLike, true
			}
		case "=", "==":
			if strings.EqualFold(colName(bo.Left), "path") {
				valExpr, op, has = bo.Right, vtab.PathConstraintEq, true
			} else if strings.EqualFold(colName(bo.Right), "path") {
				valExpr, op, has = bo.Left, vtab.PathConstraintEq, true
			}
		}
		if !has {
			continue
		}
		if v, err := e.evalExpr(valExpr, nil); err == nil {
			if text, ok3 := util.UnwrapColumnValue(v).(string); ok3 {
				pf.SetPathConstraint(text, op)
			}
		}
		return // xBestIndex parity: only the first usable constraint binds
	}
}

// residualSeriesWhere returns where minus the value-column comparisons the
// series narrowing consumed. seriesBestIndex marks those constraints omit
// (argvConsumed), so SQLite's core never re-checks them at run time; the
// narrowed range already encodes them, including the saturation semantics at
// ±2^63 where re-checking would wrongly drop rows (tabfunc01-1504).
func residualSeriesWhere(vt vtab.VirtualTable, where sql.Expr) (sql.Expr, bool) {
	if _, ok := vt.(vtab.ValueRangeNarrower); !ok {
		return where, false
	}
	if _, has := seriesValueRange(where); !has {
		return where, false
	}
	var kept []sql.Expr
	stripped := false
	for _, conj := range splitAndConjuncts(where) {
		if isSeriesValueConstraint(conj) {
			stripped = true
			continue
		}
		kept = append(kept, conj)
	}
	if !stripped {
		return where, false
	}
	switch len(kept) {
	case 0:
		return nil, true
	case 1:
		return kept[0], true
	default:
		cur := kept[0]
		for _, nxt := range kept[1:] {
			cur = &sql.BinaryOp{Left: cur, Right: nxt, Operator: "AND"}
		}
		return cur, true
	}
}

// isSeriesValueConstraint reports whether expr is a value-column comparison
// with a constant bound — exactly the conjunct shape seriesValueRange
// consumes.
func isSeriesValueConstraint(expr sql.Expr) bool {
	if bt, ok := expr.(*sql.Between); ok {
		if bt.Negated || !strings.EqualFold(colName(bt.Operand), "value") {
			return false
		}
		return seriesConstBound(bt.Low) && seriesConstBound(bt.High)
	}
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return false
	}
	switch strings.ToUpper(b.Operator) {
	case "=", "==", ">", ">=", "<", "<=":
	default:
		return false
	}
	var valExpr sql.Expr
	if strings.EqualFold(colName(b.Left), "value") {
		valExpr = b.Right
	} else if strings.EqualFold(colName(b.Right), "value") {
		valExpr = b.Left
	}
	return valExpr != nil && seriesConstBound(valExpr)
}

// seriesConstBound reports whether e is a constant integer or real literal.
func seriesConstBound(e sql.Expr) bool {
	if _, ok := constInt64(e); ok {
		return true
	}
	_, ok := constNum(e)
	return ok
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

// seriesValueBounds is the narrowed [min,max] range for the "value" column
// plus which constraint kinds were seen (series.c xFilter idxNum bits).
type seriesValueBounds struct {
	min, max *int64
	lower    bool // equality or >= / > constraint seen (0x0080/0x0100/0x0200)
	upper    bool // equality or <= / < constraint seen (0x0080/0x1000/0x2000)
}

// seriesValueRange extracts [min,max] bounds for the virtual table's "value"
// column from a WHERE clause (series.c xBestIndex value-constraint flags):
// value = X, value >/>= X, value </<= X, reversed operand orders, and
// BETWEEN. ANDed conjuncts combine. ok is false when no bound was found.
func seriesValueRange(where sql.Expr) (b seriesValueBounds, ok bool) {
	empty := false
	apply := func(op string, valExpr sql.Expr) {
		if empty {
			return
		}
		if n, iok := constInt64(valExpr); iok {
			// Exact integer bounds avoid float precision loss near 2^63.
			switch op {
			case "=", "==":
				b.min = setMin(b.min, n)
				b.max = setMax(b.max, n)
				b.lower, b.upper = true, true
			case ">":
				if n == math.MaxInt64 {
					empty = true
				} else {
					b.min = setMin(b.min, n+1)
					b.lower = true
				}
			case ">=":
				b.min = setMin(b.min, n)
				b.lower = true
			case "<":
				if n == math.MinInt64 {
					empty = true
				} else {
					b.max = setMax(b.max, n-1)
					b.upper = true
				}
			case "<=":
				b.max = setMax(b.max, n)
				b.upper = true
			}
			return
		}
		r, konst := constNum(valExpr)
		if !konst {
			return
		}
		var fop bool
		b.min, b.max, fop = applySeriesFloatBound(b.min, b.max, op, r)
		if !fop {
			empty = true
			return
		}
		switch op {
		case "=", "==", ">", ">=":
			b.lower = true
		}
		switch op {
		case "=", "==", "<", "<=":
			b.upper = true
		}
	}
	for _, conj := range splitAndConjuncts(where) {
		switch c := conj.(type) {
		case *sql.Between:
			if !strings.EqualFold(colName(c.Operand), "value") || c.Negated {
				continue
			}
			apply(">=", c.Low)
			apply("<=", c.High)
		case *sql.BinaryOp:
			op := strings.ToUpper(c.Operator)
			var valExpr sql.Expr
			var flipped bool
			switch op {
			case "=", "==", ">", ">=", "<", "<=":
				if strings.EqualFold(colName(c.Left), "value") {
					valExpr = c.Right
				} else if strings.EqualFold(colName(c.Right), "value") {
					valExpr, flipped = c.Left, true
				}
			default:
				continue
			}
			if valExpr == nil {
				continue
			}
			if flipped {
				switch op {
				case "<":
					op = ">="
				case "<=":
					op = ">"
				case ">":
					op = "<="
				case ">=":
					op = "<"
				}
			}
			apply(op, valExpr)
		}
	}
	if empty {
		// Provably unsatisfiable: an inverted range yields zero rows
		// (NarrowValueRange/init detect start>stop).
		lo, hi := int64(1), int64(0)
		return seriesValueBounds{min: &lo, max: &hi}, true
	}
	return b, b.min != nil || b.max != nil
}

// seriesTwo63 is 2^63 as a double. (double)LARGEST_INT64 rounds up to exactly
// this value and (double)SMALLEST_INT64 equals its negation.
var seriesTwo63 = float64(1 << 63)

// seriesRealToI64 ports trunk series.c's seriesRealToI64: doubles beyond
// ±(2^63-1024) saturate instead of relying on the platform-defined C cast.
func seriesRealToI64(r float64) int64 {
	const edge = float64(9223372036854774784) // 2^63-1024
	if r < -edge {
		return math.MinInt64
	}
	if r > edge {
		return math.MaxInt64
	}
	return int64(r)
}

// applySeriesFloatBound narrows [min,max] for one REAL-bound comparison on
// the value column, porting trunk series.c xFilter's float branches:
// bounds at/beyond ±2^63 saturate rather than vanish, and strict operators
// adjust by +/-1 in INTEGER space after the conversion (the old ceil(r±1.0)
// form lost the +1 to double rounding). Returns ok=false when no integer row
// can satisfy the constraint.
func applySeriesFloatBound(min, max *int64, op string, r float64) (*int64, *int64, bool) {
	switch op {
	case "=", "==":
		ce := math.Ceil(r)
		if r != ce || r < -seriesTwo63 || r > seriesTwo63 {
			return min, max, false
		}
		v := seriesRealToI64(r)
		return setMin(min, v), setMax(max, v), true
	case ">", ">=":
		if r <= -seriesTwo63 {
			return setMin(min, math.MinInt64), max, true
		}
		if r > seriesTwo63 {
			return min, max, false
		}
		m := seriesRealToI64(math.Ceil(r))
		if op == ">" && r == math.Ceil(r) {
			if m == math.MaxInt64 {
				return min, max, false
			}
			m++
		}
		return setMin(min, m), max, true
	case "<", "<=":
		if r >= seriesTwo63 {
			return min, setMax(max, math.MaxInt64), true
		}
		if r <= -seriesTwo63 {
			return min, max, false
		}
		m := seriesRealToI64(math.Floor(r))
		if op == "<" && r == math.Floor(r) {
			if m == math.MinInt64 {
				return min, max, false
			}
			m--
		}
		return min, setMax(max, m), true
	}
	return min, max, true
}

func colName(e sql.Expr) string {
	if cr, ok := e.(*sql.ColumnRef); ok {
		return cr.Name
	}
	return ""
}

// constInt64 evaluates a constant integer expression (literal or -literal).
func constInt64(expr sql.Expr) (int64, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		if n, err := strconv.ParseInt(v.Value, 10, 64); err == nil {
			return n, true
		}
	case *sql.UnaryOp:
		if v.Operator == "-" {
			if inner, ok := v.Operand.(*sql.NumericLit); ok {
				if n, err := strconv.ParseInt("-"+inner.Value, 10, 64); err == nil {
					return n, true
				}
			}
		}
	}
	return 0, false
}

func setMin(cur *int64, n int64) *int64 {
	if cur == nil || n > *cur {
		v := n
		return &v
	}
	return cur
}

func setMax(cur *int64, n int64) *int64 {
	if cur == nil || n < *cur {
		v := n
		return &v
	}
	return cur
}

// constNum evaluates a constant numeric expression.
func constNum(expr sql.Expr) (float64, bool) {
	switch v := expr.(type) {
	case *sql.NumericLit:
		f, err := strconv.ParseFloat(strings.TrimPrefix(v.Value, "+"), 64)
		return f, err == nil
	case *sql.UnaryOp:
		if v.Operator == "-" {
			if f, ok := constNum(v.Operand); ok {
				return -f, true
			}
		}
	}
	return 0, false
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
	switch len(kept) {
	case 0:
		return nil, true
	case 1:
		return kept[0], true
	default:
		cur := kept[0]
		for _, nxt := range kept[1:] {
			cur = &sql.BinaryOp{Left: cur, Right: nxt, Operator: "AND"}
		}
		return cur, true
	}
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
