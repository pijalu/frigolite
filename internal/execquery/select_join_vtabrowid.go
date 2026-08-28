package execquery

import (
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/vtab"
)

// Per-outer-row virtual-table rowid seeks in joins.
//
// SQLite's planner keeps a vtab operand innermost when a rowid equi-constraint
// binds it to an outer alias: xBestIndex marks the constraint omitted and
// unique (unionvtab.c xBestIndex — EQ on rowid/IPK -> estimatedRows=1,
// SQLITE_INDEX_SCAN_UNIQUE, cost 3.0, omit=1) and xFilter re-runs with the
// outer row's value per loop iteration. Materializing the operand once and
// pair-filtering instead is O(N^k) in the join depth (swarmvtab.test 1.5.2:
// a 3-way 400-row rowid equi-join takes minutes as a nested loop).
//
// The engine equivalent of "re-run xFilter per outer row" is re-materializing
// the vtab with a synthesized literal constraint `rowid = <value>` per left
// row. Modules implementing RowidRangeConsumer (unionvtab/swarmvtab) consume
// it for source selection exactly like a literal range; the generic
// materializer reports what it consumed via VtabScanOptions.Residual, which
// makes the optimization self-limiting: if the module cannot consume the
// synthesized constraint, the legacy once-only materialization is used.

// vtabRowidAliasNames mirrors consumeVTabRowidRange's rowid aliases.
var vtabRowidAliasNames = map[string]bool{"rowid": true, "_rowid_": true, "oid": true}

// vtabRowidEquiOuters collects the outer-side references of WHERE
// equi-conjuncts `alias.rowid = <outer.col>` (either orientation) where the
// outer side is a table-qualified reference whose key exists in the already
// materialized left rows. Conjuncts whose outer side is not yet materialized
// (a later join operand) are skipped — they keep working as residual filters
// or when that operand's turn comes. Returns nil when no conjunct qualifies.
func vtabRowidEquiOuters(where sql.Expr, alias string, left []RowMap) []*sql.ColumnRef {
	if alias == "" || len(left) == 0 {
		return nil
	}
	var outers []*sql.ColumnRef
	for _, conj := range splitANDConjunctsLocal(where) {
		bo, ok := conj.(*sql.BinaryOp)
		if !ok || !strings.EqualFold(bo.Operator, "=") {
			continue
		}
		lcr, lok := bo.Left.(*sql.ColumnRef)
		rcr, rok := bo.Right.(*sql.ColumnRef)
		if !lok || !rok {
			continue
		}
		switch {
		case isVtabAliasRowid(lcr, alias) && isResolvableOuterRef(rcr, alias, left):
			outers = append(outers, rcr)
		case isVtabAliasRowid(rcr, alias) && isResolvableOuterRef(lcr, alias, left):
			outers = append(outers, lcr)
		}
	}
	return outers
}

// isVtabAliasRowid reports whether ref is `<alias>.<rowid alias>`.
func isVtabAliasRowid(ref *sql.ColumnRef, alias string) bool {
	return ref.Table != "" && strings.EqualFold(ref.Table, alias) &&
		vtabRowidAliasNames[strings.ToLower(ref.Name)]
}

// isResolvableOuterRef reports whether ref is a table-qualified reference to
// another operand whose qualified key exists in the left rows.
func isResolvableOuterRef(ref *sql.ColumnRef, alias string, left []RowMap) bool {
	if ref.Table == "" || strings.EqualFold(ref.Table, alias) || ref.Name == "" {
		return false
	}
	_, ok := left[0].Get(ref.Table + "." + ref.Name)
	return ok
}

// splitANDConjunctsLocal splits an expression into its top-level AND-chain
// conjuncts (no descent into OR / subqueries — only top-level AND terms are
// usable as vtab constraints).
func splitANDConjunctsLocal(expr sql.Expr) []sql.Expr {
	if expr == nil {
		return nil
	}
	if bin, ok := expr.(*sql.BinaryOp); ok && strings.EqualFold(bin.Operator, "AND") {
		return append(splitANDConjunctsLocal(bin.Left), splitANDConjunctsLocal(bin.Right)...)
	}
	return []sql.Expr{expr}
}

// vtabOuterIntValues resolves every outer reference in every left row.
// Returns one value-set per left row; a row whose reference is missing or
// NULL yields nil (rowid = NULL matches nothing). usable is false when any
// resolvable value is not integer-representable — the legacy path keeps
// those semantics instead of synthesizing a wrong literal.
func vtabOuterIntValues(refs []*sql.ColumnRef, left []RowMap) (vals [][]int64, usable bool) {
	vals = make([][]int64, len(left))
	for li, row := range left {
		for _, ref := range refs {
			v, ok := row.Get(ref.Table + "." + ref.Name)
			if !ok || util.UnwrapColumnValue(v) == nil {
				continue // NULL / unresolvable: row matches nothing
			}
			n, ok := vtab.AsVtabInt64(util.UnwrapColumnValue(v))
			if !ok {
				return nil, false
			}
			vals[li] = append(vals[li], n)
		}
	}
	return vals, true
}

// synthRowidEq builds `rowid = <n>`; synthRowidWhere folds one literal per
// outer value into the constraint handed to the materializer and returns the
// individual conjunct pointers for consumption checking (removeConsumed
// drops consumed nodes but leaves the rest pointer-identical).
func synthRowidEq(n int64) *sql.BinaryOp {
	return &sql.BinaryOp{
		Operator: "=",
		Left:     &sql.ColumnRef{Name: "rowid"},
		Right:    &sql.NumericLit{Value: strconv.FormatInt(n, 10)},
	}
}

func synthRowidWhere(ns []int64) (where sql.Expr, conjuncts []*sql.BinaryOp) {
	for _, n := range ns {
		eq := synthRowidEq(n)
		conjuncts = append(conjuncts, eq)
		if where == nil {
			where = eq
		} else {
			where = &sql.BinaryOp{Operator: "AND", Left: where, Right: eq}
		}
	}
	return where, conjuncts
}

// tryVtabRowidSeek guards and runs the per-outer-row vtab rowid seek for a
// join operand; handled=false means the legacy materialization applies.
func (e *SelectEngine) tryVtabRowidSeek(
	s *sql.SelectStmt, join sql.JoinClause, entry *schema.Entry, currentMaps []RowMap,
) (maps []RowMap, defs []sql.ColumnDef, tableName string, leftIdx []int, handled bool, err error) {
	if entry.RootPage != 0 || s.Where == nil || len(currentMaps) == 0 ||
		joinTypeHas(join.JoinType, "RIGHT") || joinTypeHas(join.JoinType, "FULL") {
		return nil, nil, "", nil, false, nil
	}
	alias := aliasOrName(join.Table.Name, join.Table.As)
	outers := vtabRowidEquiOuters(s.Where, alias, currentMaps)
	if len(outers) == 0 {
		return nil, nil, "", nil, false, nil
	}
	return e.materializeVtabRowidSeek(entry, alias, outers, currentMaps)
}

// materializeVtabRowidSeek materializes a vtab join operand per left row with
// a synthesized `rowid = <literal>` constraint per outer value (the engine
// equivalent of SQLite's per-loop-iteration xFilter seek). Returns
// handled=false whenever the seek cannot be done safely; the caller falls
// back to the legacy once-only materialization.
func (e *SelectEngine) materializeVtabRowidSeek(
	entry *schema.Entry, alias string, outers []*sql.ColumnRef, currentMaps []RowMap,
) (maps []RowMap, defs []sql.ColumnDef, tableName string, leftIdx []int, handled bool, err error) {
	vals, usable := vtabOuterIntValues(outers, currentMaps)
	if !usable {
		return nil, nil, "", nil, false, nil
	}
	for li, rowVals := range vals {
		if len(rowVals) < len(outers) {
			continue // NULL outer value: no vtab rows match this left row
		}
		rowMaps, rowDefs, ok, merr := e.seekLeftRow(entry, alias, rowVals)
		if merr != nil {
			return nil, nil, "", nil, false, merr
		}
		if !ok {
			return nil, nil, "", nil, false, nil
		}
		if defs == nil {
			defs = rowDefs
		}
		maps = append(maps, rowMaps...)
		for range rowMaps {
			leftIdx = append(leftIdx, li)
		}
	}
	return maps, defs, alias, leftIdx, true, nil
}

// seekLeftRow materializes the vtab for ONE left row's outer values and
// reports ok=false when the module did not consume the synthesized
// constraints (per-row full scans — the legacy path is better).
func (e *SelectEngine) seekLeftRow(
	entry *schema.Entry, alias string, rowVals []int64,
) (rowMaps []RowMap, defs []sql.ColumnDef, ok bool, err error) {
	where, conjuncts := synthRowidWhere(rowVals)
	residual := where
	opts := VtabScanOptions{Where: where, MaxRows: -1}
	opts.Residual = &residual
	d, rows, rowids, merr, found := e.ctx.MaterializeCreatedVTab(entry.Name, opts)
	if !found {
		return nil, nil, false, nil
	}
	if merr != nil {
		return nil, nil, false, merr
	}
	// Self-limiting guard: the module must have consumed EVERY synthesized
	// constraint (removeConsumed drops consumed nodes and leaves the rest
	// pointer-identical). An unconsumed constraint means per-row full scans.
	for _, s := range conjuncts {
		if residualContainsNode(residual, s) {
			return nil, nil, false, nil
		}
	}
	rowMaps = buildScanRowMaps(rows, d, alias)
	for i := range rowMaps {
		if i < len(rowids) {
			rowMaps[i]["rowid"] = rowids[i]
			rowMaps[i][alias+".rowid"] = rowids[i]
		}
	}
	return rowMaps, d, true, nil
}

// residualContainsNode reports whether target (by pointer identity) survives
// in the residual expression — i.e. the module did NOT consume it.
func residualContainsNode(where sql.Expr, target *sql.BinaryOp) bool {
	if where == nil {
		return false
	}
	switch w := where.(type) {
	case *sql.BinaryOp:
		if w == target {
			return true
		}
		if strings.EqualFold(w.Operator, "AND") {
			return residualContainsNode(w.Left, target) || residualContainsNode(w.Right, target)
		}
	}
	return false
}
