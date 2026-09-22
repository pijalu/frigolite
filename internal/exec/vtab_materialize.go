package exec

import (
	"fmt"
	"os"
	"strings"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

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
//
// The body is a fixed pipeline of module-family phases (Open/Closed: each
// phase keys off one optional vtab interface):
//
//	create + schema-bind + input/value-range narrowing
//	→ residual-WHERE narrowing (series / hidden / MATCH consumption)
//	→ rowid-range consumption (unionvtab-style source selection)
//	→ rtree-family constraint pushdown (ConstraintSink)
//	→ fstree path binding, spellfix1 plan pushdown, MATCH binding
//	→ PlanBestIndexer runtime contract, else hidden-combination reads
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
	setVtabInputConstraint(vt, opts)
	narrowSeriesValueRange(vt, opts.Where)
	narrowResidualWhere(vt, &opts)
	e.consumeRowidRangeConjuncts(vt, &opts)
	if err := e.pushConjunctsToConstraintSink(vt, &opts); err != nil {
		return nil, nil, err
	}
	// fstree-style path narrowing: the FIRST usable GLOB/LIKE/EQ conjunct on
	// the path column hands its value to the instance (xFilter parity).
	e.bindPathConstraint(vt, opts.Where)
	e.pushSpellfixConstraints(vt, &opts)
	e.bindMatchConstraints(vt, opts.Where)
	// Modules implementing PlanBestIndexer drive the full xBestIndex/xFilter
	// runtime contract (sqlite3_module parity, vtab_bestindex.go); legacy
	// modules keep the hidden-combination path below (Open/Closed).
	if pbi, ok := vt.(vtab.PlanBestIndexer); ok {
		return e.readVtabWithBestIndexPlan(vt, pbi, opts)
	}
	combos := e.extractHiddenConstraintCombos(vt, opts.Where)
	if len(combos) == 0 {
		// No pushable constraints: validate the plain instance (series.c
		// bStartSeen rejects an unusable/missing START binding) and read.
		return readPlainVtabInstance(vt, opts.MaxRows)
	}
	return e.readVtabHiddenCombos(module, strArgs, valArgs, opts, bindSchema, combos)
}

// setVtabInputConstraint forwards fts3tokenize's `input = <string>` constraint
// to the instance before its cursor opens (fts3_tokenize_vtab.c xBestIndex
// sets idxNum=1 for it); fts3tokFilterMethod errors with SQLITE_ERROR
// ("SQL logic error") when the query reaches the vtab without an input
// binding (fts3tok1 1.x, fts4unicode 11.1).
func setVtabInputConstraint(vt vtab.VirtualTable, opts execquery.VtabScanOptions) {
	ic, ok := vt.(interface{ SetInputConstraint(string) })
	if !ok {
		return
	}
	if in, has := execquery.VtabInputConstraint(opts.Where); has {
		ic.SetInputConstraint(in)
	}
}

// narrowSeriesValueRange lets series.c narrow the generated range from
// equality/range constraints on the value column inside xFilter (iMin/iMax);
// without it a query like FROM generate_series(MinI64, MaxI64, 2) WHERE
// value BETWEEN 1 AND 5 would materialize 2^62 rows before filtering.
func narrowSeriesValueRange(vt vtab.VirtualTable, where sql.Expr) {
	nr, ok := vt.(vtab.ValueRangeNarrower)
	if !ok {
		return
	}
	b, has := seriesValueRange(where)
	if !has {
		return
	}
	// series.c widens the implicit START/STOP defaults to the full int64
	// range when only the VALUE column is constrained (xFilter idxNum
	// 0x05/0x06 rules) — e.g. tabfunc01-1520: FROM
	// generate_series(9223372036854774784) WHERE value<=X must not stop at
	// the default STOP of 4294967295.
	if ex, ok2 := vt.(vtab.ValueConstraintExpander); ok2 {
		ex.ExpandValueDefaults(b.lower, b.upper)
	}
	nr.NarrowValueRange(b.min, b.max)
}

// narrowResidualWhere rewrites the residual WHERE for the constraints the
// instance consumed: series value comparisons, hidden-column equality/IN
// bindings and `col MATCH <literal>` on a MatchConstraintSetter module (the
// MATCH drives row generation, so the operator must not be re-evaluated as a
// filter). unionvtab-style rowid intervals are dropped by the rowid-range
// phase below.
func narrowResidualWhere(vt vtab.VirtualTable, opts *execquery.VtabScanOptions) {
	if opts.Residual == nil {
		return
	}
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

// consumeRowidRangeConjuncts performs unionvtab-style source selection from
// the WHERE rowid interval: chosen sources are scanned per the module's
// declared ranges and the rowid conjuncts are OMITTED (not re-applied as
// filters).
func (e *Engine) consumeRowidRangeConjuncts(vt vtab.VirtualTable, opts *execquery.VtabScanOptions) {
	rc, ok := vt.(vtab.RowidRangeConsumer)
	if !ok || rc == nil || opts.Where == nil {
		return
	}
	e.consumeVTabRowidRange(vt, opts.Where)
	if cleaned, changed := e.dropVTabRowidConjuncts(vt, opts.Where); changed {
		opts.Where = cleaned
		if opts.Residual != nil {
			*opts.Residual = cleaned
		}
	}
}

// pushConjunctsToConstraintSink performs rtree-family coordinate/id pushdown:
// single-table conjuncts on vtab columns are handed to constraintSink and
// removed from the residual so the core never re-applies SQL affinity to them
// (sqlite3 argvConsumed). A push error aborts the materialization.
func (e *Engine) pushConjunctsToConstraintSink(vt vtab.VirtualTable, opts *execquery.VtabScanOptions) error {
	sink, ok := vt.(vtab.ConstraintSink)
	if !ok || opts.Where == nil {
		return nil
	}
	cols := declaredColumnLower(vt)
	var consumed []sql.Expr
	for _, conj := range splitAndConjuncts(opts.Where) {
		handled, perr := e.rtreePushConjunct(sink, cols, conj)
		if perr != nil {
			return perr
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
	return nil
}

// pushSpellfixConstraints handles spellfix1 plan-column constraints (word
// MATCH, langid/top/scope =, distance </<=, rowid =): they bind onto the
// instance (xBestIndex argv/omit parity) and drop from the residual WHERE.
func (e *Engine) pushSpellfixConstraints(vt vtab.VirtualTable, opts *execquery.VtabScanOptions) {
	sf, ok := vt.(vtab.SpellfixConstraintSink)
	if !ok || opts.Where == nil {
		return
	}
	// spellfix1BestIndex scans all constraints before deciding the plan; a
	// MATCH term suppresses rowid consumption regardless of order, so MATCH
	// conjuncts are offered to the sink first.
	ordered := orderedSpellfixConjuncts(splitAndConjuncts(opts.Where))
	var consumed []sql.Expr
	for _, conj := range ordered {
		if e.pushSpellfixConjunct(sf, conj) {
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

// orderedSpellfixConjuncts reorders conjuncts MATCH-first (pass 1) and keeps
// the remaining binary comparisons in declaration order (pass 2).
func orderedSpellfixConjuncts(conjuncts []sql.Expr) []sql.Expr {
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
	return ordered
}

// pushSpellfixConjunct offers one conjunct to a spellfix1 instance; consumed
// reports whether the instance took the constraint (the caller drops the
// conjunct from the residual WHERE).
func (e *Engine) pushSpellfixConjunct(sf vtab.SpellfixConstraintSink, conj sql.Expr) bool {
	bo, isOp := conj.(*sql.BinaryOp)
	if !isOp {
		return false
	}
	col, flipped := spellfixConstraintColumn(bo)
	if col == -2 {
		return false
	}
	op := strings.ToUpper(bo.Operator)
	if flipped {
		op = spellfixFlippedOp(op)
		if op == "" {
			return false
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
		return false
	}
	return sf.PushSpellfixConstraint(col, op, util.UnwrapColumnValue(v))
}

// spellfixFlippedOp mirrors the operator for a value-on-the-left comparison;
// "" means the operator has no mirrored form (the constraint is not pushed).
func spellfixFlippedOp(op string) string {
	return map[string]string{"<": ">", ">": "<", "<=": ">=", ">=": "<="}[op]
}

// bindMatchConstraints binds `col MATCH <literal>` conjuncts onto a
// MatchConstraintSetter instance (they drive row generation).
func (e *Engine) bindMatchConstraints(vt vtab.VirtualTable, where sql.Expr) {
	setter, hookOK := vt.(vtab.MatchConstraintSetter)
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "HOOK whereNil=%v setterOK=%v conj=%d\n", where == nil, hookOK, len(splitAndConjuncts(where)))
	}
	if !hookOK || where == nil {
		return
	}
	for _, conj := range splitAndConjuncts(where) {
		e.bindMatchConjunct(setter, conj)
	}
}

// bindMatchConjunct binds one MATCH conjunct (literal or any constant
// expression — amatch1-3.0: format('%.81c','a')).
func (e *Engine) bindMatchConjunct(setter vtab.MatchConstraintSetter, conj sql.Expr) {
	if os.Getenv("CL_DBG") != "" {
		fmt.Fprintf(os.Stderr, "HOOKC %T %+v\n", conj, conj)
	}
	bo, isOp := conj.(*sql.BinaryOp)
	if !isOp || !strings.EqualFold(bo.Operator, "MATCH") {
		return
	}
	cr, isRef := bo.Left.(*sql.ColumnRef)
	if !isRef {
		return
	}
	v, verr := e.evalExpr(bo.Right, nil)
	if verr != nil {
		return
	}
	target := fmt.Sprintf("%v", util.UnwrapColumnValue(v))
	setter.SetMatchConstraint(cr.Name, target)
}

// readPlainVtabInstance validates the constraint-free instance (series.c
// bStartSeen rejects an unusable/missing START binding) and reads it.
func readPlainVtabInstance(vt vtab.VirtualTable, maxRows int64) ([][]interface{}, []int64, error) {
	if iv, ok := vt.(vtab.InstanceValidator); ok {
		if verr := iv.ValidateInstance(); verr != nil {
			return nil, nil, verr
		}
	}
	return readVtabRowsWithRowids(vt, maxRows)
}

// readVtabHiddenCombos materializes one pristine instance per hidden-column
// constraint combination and concatenates the streams (SQLite runs xFilter
// once per IN value), honoring the row budget.
func (e *Engine) readVtabHiddenCombos(module vtab.Module, strArgs []string, valArgs []interface{}, opts execquery.VtabScanOptions, bindSchema func(vtab.VirtualTable) error, combos [][]vtabBinding) ([][]interface{}, []int64, error) {
	var all [][]interface{}
	var allRowids []int64
	for _, combo := range combos {
		remaining := opts.MaxRows - int64(len(all))
		rows, rowids, err := e.readVtabComboRows(module, strArgs, valArgs, bindSchema, combo, remaining)
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

// readVtabComboRows reads one constraint combination from a fresh instance so
// bindings never accumulate across combinations.
func (e *Engine) readVtabComboRows(module vtab.Module, strArgs []string, valArgs []interface{}, bindSchema func(vtab.VirtualTable) error, combo []vtabBinding, maxRows int64) ([][]interface{}, []int64, error) {
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
	return readVtabRowsWithRowids(instance, maxRows)
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
		if e.bindPathConjunct(pf, conj) {
			return // xBestIndex parity: only the first usable constraint binds
		}
	}
}

// bindPathConjunct classifies and binds one candidate path conjunct; matched
// reports a usable shape, which stops the scan even when evaluation failed
// (fstreeBestIndex parity).
func (e *Engine) bindPathConjunct(pf vtab.PathFilterSink, conj sql.Expr) bool {
	bo, isOp := conj.(*sql.BinaryOp)
	if !isOp {
		return false
	}
	valExpr, op, has := pathConstraintFromOp(bo)
	if !has {
		return false
	}
	if v, err := e.evalExpr(valExpr, nil); err == nil {
		if text, ok := util.UnwrapColumnValue(v).(string); ok {
			pf.SetPathConstraint(text, op)
		}
	}
	return true
}

// pathConstraintFromOp resolves the fstree constraint of one binary conjunct:
// path GLOB/LIKE <const> or path = <const> (either operand order for =).
func pathConstraintFromOp(bo *sql.BinaryOp) (valExpr sql.Expr, op vtab.PathConstraintOp, has bool) {
	switch strings.ToUpper(bo.Operator) {
	case "GLOB", "LIKE":
		if strings.EqualFold(colName(bo.Left), "path") {
			return bo.Right, pathConstraintOpFor(bo.Operator), true
		}
	case "=", "==":
		if valExpr, has := pathEqConstraint(bo); has {
			return valExpr, vtab.PathConstraintEq, true
		}
	}
	return nil, 0, false
}

// pathConstraintOpFor maps the SQL operator to the fstree constraint kind.
func pathConstraintOpFor(operator string) vtab.PathConstraintOp {
	if strings.EqualFold(operator, "LIKE") {
		return vtab.PathConstraintLike
	}
	return vtab.PathConstraintGlob
}

// pathEqConstraint recognizes path = <expr> in either operand order.
func pathEqConstraint(bo *sql.BinaryOp) (sql.Expr, bool) {
	if strings.EqualFold(colName(bo.Left), "path") {
		return bo.Right, true
	}
	if strings.EqualFold(colName(bo.Right), "path") {
		return bo.Left, true
	}
	return nil, false
}
