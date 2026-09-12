package exec

import (
	"errors"

	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// errVtabBestIndexMalfunction is the xBestIndex contract-violation error.
// where.c substitutes the virtual table's name ("%s.xBestIndex malfunction",
// where.c:4366/4388); this FROM-materialization path has no vtab name
// available, so the "<vtab>" placeholder stands in (documented
// simplification of where.c:4366).
var errVtabBestIndexMalfunction = errors.New("<vtab>.xBestIndex malfunction")

// vtabInSlot is one IN-constrained xFilter argv position: the engine runs
// xFilter once per IN element, substituting each element's value into that
// argv slot and concatenating the streams (wherecode.c: every IN term handled
// as == gets its own loop nesting level).
type vtabInSlot struct {
	slot   int           // 0-based xFilter argv position
	values []interface{} // one evaluated value per IN list element
}

// RegisterVtabModule registers a virtual-table module under the given name in
// the engine's registry (sqlite3_create_module parity), making it available
// to CREATE VIRTUAL TABLE ... USING <name> and FROM <name>(...) statements.
func (e *Engine) RegisterVtabModule(name string, m vtab.Module) {
	e.vtabs.Register(name, m)
}

// readVtabWithBestIndexPlan is the runtime half of the virtual-table
// xBestIndex contract (where.c whereLoopAddVirtualOne): it offers the WHERE
// conjuncts to a PlanBestIndexer module, binds the chosen plan's constraint
// values into xFilter argv, and drains the resulting row streams.
//
//   - ErrVtabConstraint (SQLITE_CONSTRAINT) rejects the plan: the statement
//     proceeds with the UNMODIFIED WHERE over a plain materialization
//     (where.c:4310-4319 "non-viable plan rejected"); any other module error
//     aborts the statement (where.c:4322).
//   - Constraints with Usage[].ArgvIndex>0 bind their value expression into
//     argv[ArgvIndex-1]; the argvIndex values must form the contiguous
//     sequence 1..N without duplicates or the plan is a malfunction
//     (where.c:4360-4400).
//   - Constraints marked Omit are consumed by the module and dropped from the
//     residual WHERE; argv-bound NON-omit constraints stay in the residual and
//     are re-checked per row (series.c's SQLITE_SERIES_CONSTRAINT_VERIFY
//     relies on the re-check).
//   - IN-mapped constraints run xFilter once per list element on a fresh
//     cursor (this engine's equivalent of re-filtering the same cursor).
//   - OrderByConsumed is accepted as-is: the engine's later ORDER BY
//     application degenerates to an idempotent re-sort of ordered rows.
func (e *Engine) readVtabWithBestIndexPlan(vt vtab.VirtualTable, pbi vtab.PlanBestIndexer, opts execquery.VtabScanOptions) ([][]interface{}, []int64, error) {
	columns, ok := vtabDeclaredColumns(vt)
	if !ok {
		// No declared-column contract to plan against: legacy full read.
		return readVtabRowsWithRowids(vt, opts.MaxRows)
	}
	var fo vtab.FunctionOverloader
	if f, isFo := vt.(vtab.FunctionOverloader); isFo {
		fo = f
	}
	// tableName "": the builder's ColumnRef qualification rules accept
	// unqualified references unconditionally, and a FROM-materialized scan is
	// single-table, so "" cannot mis-qualify.
	ii, conjuncts, err := execquery.BuildVtabIndexInfoWithInstance(&opts, "", columns, fo)
	if err != nil {
		return nil, nil, err
	}
	if perr := pbi.BestIndexPlan(ii); perr != nil {
		if errors.Is(perr, vtab.ErrVtabConstraint) {
			// Plan rejected (where.c:4310): no argv binding, the core
			// re-checks every constraint.
			return readVtabRowsWithRowids(vt, opts.MaxRows)
		}
		return nil, nil, perr
	}
	slots, n, merr := validateVtabArgvSlots(ii)
	if merr != nil {
		return nil, nil, merr
	}
	argv := e.buildVtabFilterArgv(ii, conjuncts, &opts, slots, n)
	combos := vtabStreamCombos(argv, e.vtabInSlots(ii, conjuncts, slots))
	vtabResidualAfterOmit(&opts, ii, conjuncts)
	return e.readVtabPlanStreams(vt, ii, combos, opts)
}

// vtabDeclaredColumns returns the instance's declared column names in order.
func vtabDeclaredColumns(vt vtab.VirtualTable) ([]string, bool) {
	if ci, ok := vt.(vtab.ColumnInfo); ok {
		return ci.Columns(), true
	}
	return nil, false
}

// validateVtabArgvSlots ports where.c:4360-4400: the argvIndex values chosen
// by xBestIndex must form a contiguous 1..N sequence, may not bind an
// unusable constraint, and may not duplicate a slot. slots returns the
// constraint indexes in argv order; n is the argv size.
func validateVtabArgvSlots(ii *vtab.IndexInfo) (slots []int, n int, err error) {
	// slotOf[a] is the constraint index bound to 1-based argv position a
	// (0 = free).
	slotOf := make([]int, len(ii.Constraints)+1)
	used := 0
	for i, c := range ii.Constraints {
		a := ii.Usage[i].ArgvIndex
		if a <= 0 {
			continue
		}
		used++
		if a > len(ii.Constraints) || slotOf[a] != 0 || !c.Usable {
			// where.c:4364-4366: argvIndex beyond the constraint table, a
			// duplicated slot, or an unusable constraint bound.
			return nil, 0, errVtabBestIndexMalfunction
		}
		slotOf[a] = i + 1
	}
	slots = make([]int, 0, used)
	for a := 1; a <= used; a++ {
		if slotOf[a] == 0 {
			// where.c:4384-4392: the non-zero argvIdx values must be
			// contiguous.
			return nil, 0, errVtabBestIndexMalfunction
		}
		slots = append(slots, slotOf[a]-1)
	}
	return slots, used, nil
}

// buildVtabFilterArgv evaluates each bound constraint's value expression into
// the xFilter argv (wherecode.c binds every aConstraintUsage[].argvIndex>0
// term as argv[argvIndex-1]). A constraint with no value side binds nil:
// IS NULL / IS NOT NULL carry no value, IN constraints are driven per element
// by vtabInSlots, and an un-evaluable value leaves nil — the core still
// re-checks non-omit terms. The raw (unwrapped) evaluated value is kept:
// sqlite's xFilter argv carries sqlite3_value handles, not coerced scalars.
func (e *Engine) buildVtabFilterArgv(ii *vtab.IndexInfo, conjuncts []sql.Expr, opts *execquery.VtabScanOptions, slots []int, n int) []interface{} {
	argv := make([]interface{}, n)
	for pos, ci := range slots {
		c := ii.Constraints[ci]
		conj := vtabConjunct(conjuncts, c.TermOffset)
		if c.Op == vtab.IndexConstraintLimit || c.Op == vtab.IndexConstraintOffset {
			// LIMIT/OFFSET aux terms take their value from the scan options:
			// normalize the builder's raw-expression entry to nil so
			// VtabConjunctValue resolves them through its documented
			// opts-based path (where.c isLimitTerm terms).
			conj = nil
		}
		valExpr, ok := execquery.VtabConjunctValue(opts, conj, c.Op)
		if !ok {
			continue // no value side: bind nil (already the zero slot value)
		}
		v, err := e.evalExpr(valExpr, nil)
		if err != nil {
			continue // bind nil
		}
		argv[pos] = v
	}
	return argv
}

// vtabInSlots collects the IN-mapped constraints (IsIn) whose conjunct is an
// IN(...) list: each contributes one argv slot cycled through the list's
// evaluated element values (one xFilter call per element). An un-evaluable
// element binds nil; a constraint that is not backed by an InList conjunct
// keeps the nil base binding (VtabConjunctValue resolves no single value for
// IN terms).
func (e *Engine) vtabInSlots(ii *vtab.IndexInfo, conjuncts []sql.Expr, slots []int) []vtabInSlot {
	var ins []vtabInSlot
	for pos, ci := range slots {
		c := ii.Constraints[ci]
		if !c.IsIn {
			continue
		}
		inl, ok := vtabConjunct(conjuncts, c.TermOffset).(*sql.InList)
		if !ok || inl.Negated || len(inl.List) == 0 {
			continue
		}
		vals := make([]interface{}, 0, len(inl.List))
		for _, elem := range inl.List {
			v, err := e.evalExpr(elem, nil)
			if err != nil {
				vals = append(vals, nil)
				continue
			}
			vals = append(vals, v)
		}
		ins = append(ins, vtabInSlot{slot: pos, values: vals})
	}
	return ins
}

// vtabConjunct returns the conjunct a constraint's TermOffset points at, or
// nil when the offset is out of range (LIMIT/OFFSET aux constraints have no
// WHERE conjunct at all).
func vtabConjunct(conjuncts []sql.Expr, termOffset int) sql.Expr {
	if termOffset < 0 || termOffset >= len(conjuncts) {
		return nil
	}
	return conjuncts[termOffset]
}

// vtabStreamCombos expands the base xFilter argv across the IN slots: one
// combination per element, cartesian product across multiple IN constraints
// (wherecode.c gives every IN term its own loop nesting level and
// concatenates the resulting streams).
func vtabStreamCombos(base []interface{}, ins []vtabInSlot) [][]interface{} {
	combos := [][]interface{}{base}
	for _, in := range ins {
		var next [][]interface{}
		for _, combo := range combos {
			for _, v := range in.values {
				ext := make([]interface{}, len(combo))
				copy(ext, combo)
				ext[in.slot] = v
				next = append(next, ext)
			}
		}
		combos = next
	}
	return combos
}

// vtabResidualAfterOmit drops the WHERE conjuncts whose constraints xBestIndex
// marked omit — the module fully handles them and the core must not re-check
// (where.c omitMask parity). Argv-bound NON-omit constraints stay (sqlite
// re-checks them per row). The residual is written through opts.Residual only
// when at least one conjunct was omitted; the caller swaps the WHERE only when
// the residual differs from the original clause.
func vtabResidualAfterOmit(opts *execquery.VtabScanOptions, ii *vtab.IndexInfo, conjuncts []sql.Expr) {
	if opts == nil || opts.Residual == nil || opts.Where == nil {
		return
	}
	drop := map[sql.Expr]bool{}
	for i := range ii.Constraints {
		if !ii.Usage[i].Omit {
			continue
		}
		if conj := vtabConjunct(conjuncts, ii.Constraints[i].TermOffset); conj != nil {
			drop[conj] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	kept := make([]sql.Expr, 0, len(conjuncts))
	changed := false
	for _, conj := range splitAndConjuncts(opts.Where) {
		if drop[conj] {
			changed = true
			continue
		}
		kept = append(kept, conj)
	}
	if !changed {
		return
	}
	*opts.Residual = joinConjuncts(kept)
}

// readVtabPlanStreams opens one cursor per xFilter combination (an IN
// combination means one cursor per element) and concatenates their rows up
// to opts.MaxRows total.
func (e *Engine) readVtabPlanStreams(vt vtab.VirtualTable, ii *vtab.IndexInfo, combos [][]interface{}, opts execquery.VtabScanOptions) ([][]interface{}, []int64, error) {
	var all [][]interface{}
	var allRowids []int64
	for _, argv := range combos {
		remaining := opts.MaxRows - int64(len(all))
		rows, rowids, err := readVtabPlanStream(vt, ii, argv, remaining)
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

// readVtabPlanStream drains one xFilter call's row stream: Open (xOpen),
// FilterPlan (xFilter), then read (xNext/xColumn parity). The cursor is
// closed by readCursorRowsWithRowids, including on the FilterPlan error path.
func readVtabPlanStream(vt vtab.VirtualTable, ii *vtab.IndexInfo, argv []interface{}, maxRows int64) ([][]interface{}, []int64, error) {
	cur, err := vt.Open()
	if err != nil {
		return nil, nil, err
	}
	if pf, ok := cur.(vtab.PlanFilterer); ok {
		if ferr := pf.FilterPlan(ii.IdxNum, ii.IdxStr, argv); ferr != nil {
			cur.Close()
			return nil, nil, ferr
		}
	}
	return readCursorRowsWithRowids(cur, maxRows)
}
