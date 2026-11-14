package execdml

import (
	"bytes"
	"sort"
	"strings"

	"github.com/pijalu/frigolite/internal/btree"
	"github.com/pijalu/frigolite/internal/execexpr"
	"github.com/pijalu/frigolite/internal/execquery"
	"github.com/pijalu/frigolite/internal/schema"
	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/storage"
	"github.com/pijalu/frigolite/internal/util"
)

// orConstraint is one equality constraint usable as an index prefix term:
// column = value. applyAffinity reports whether the comparison applies the
// column's affinity to the literal (a bare column reference). It is false for
// `+col = value`, which compares the raw value with no affinity conversion.

// maxOrTerms caps the number of DNF-flattened OR terms before the OR-index
// optimization is abandoned.
//
// SQLite has no 32-term cap: the OR clause is a normal expression, bounded
// by SQLITE_MAX_EXPR_DEPTH (1000 by default) — where7's "a=11 OR b=11 OR .."
// runs to hundreds of terms and SQLite still uses the multi-index plan.
const maxOrTerms = 1000

// planOrIndexScan builds an OR-index plan for a single-table WHERE clause of
// the form (col1=v1 AND col2=v2) OR (col3=v3 AND col4=v4), where every OR
// term constrains the leading columns of some index on the table. Constant
// equalities from sibling AND terms (e.g. a=(subquery)) can extend each
// branch's index prefix. It returns (branches, true) when the plan applies,
// otherwise (nil, false).
func (e *DMLExecutor) planOrIndexScan(where sql.Expr, tableName string, colDefs []sql.ColumnDef, ctx *DatabaseContext) ([]orBranchPlan, bool) {
	if where == nil {
		return nil, false
	}
	// Split the top-level AND into conjuncts.
	andTerms := splitAndTerms(where)

	// Collect constant equality constraints from non-OR conjuncts (they can
	// extend each OR branch's index prefix, e.g. a=(subquery) AND (b=1 OR c=1)).
	outerCols := make(map[string]bool, len(colDefs))
	for _, cd := range colDefs {
		outerCols[cd.Name] = true
	}
	constConstraints, constSources, nonORIdxs, orTerm, orCount := e.collectOrConstraintTerms(andTerms, outerCols)
	if orCount != 1 || orTerm == nil {
		return nil, false
	}

	// Flatten the OR into DNF terms (distributing AND over nested ORs).
	terms := flattenOrTerms(orTerm)
	if len(terms) == 0 || len(terms) > maxOrTerms {
		return nil, false
	}

	// Load the table's indexes once.
	indexes, err := ctx.Schema.FindIndexesForTable(tableName)
	if err != nil || len(indexes) == 0 {
		return nil, false
	}
	rowidTable := orTableHasRowid(ctx, tableName)

	// whereexpr.c CASE 2 (whereCombineDisjuncts): a two-way OR whose
	// disjuncts hold a subterm pair with the same operands and compatible
	// operators ("a>2 OR (a=2 AND b>2)") adds a virtual combined conjunct
	// ("a>=2"). When an index serves it, SQLite plans ONE index range loop
	// with the full OR evaluated as residual — "SEARCH t USING INDEX i
	// (a>?)" — so the OR plan collapses to that single branch and the rows
	// come out in the index's key order (where the multi-branch union order
	// would differ).
	if branch, consumed, ok := e.combineOrDisjunctsPlan(orTerm, indexes, constConstraints, colDefs, outerCols, rowidTable); ok {
		// C's collapsed plan compiles only the combined conjunct into the
		// seek; the full OR term stays the loop's residual filter.
		branch.Residual = orBranchResidual(andTerms, nonORIdxs, constSources, consumed, []sql.Expr{orTerm})
		return []orBranchPlan{branch}, true
	}

	var branches []orBranchPlan
	for _, term := range terms {
		cand := e.bestIndexForOrBranch(term, outerCols, indexes, constConstraints, colDefs, rowidTable)
		// A branch served by the rowid (an equality or a range on the
		// INTEGER PRIMARY KEY, or on a rowid table's rowid pseudo-column)
		// has no index: SQLite renders it "SEARCH t2 USING INTEGER PRIMARY
		// KEY (rowid=?)" or "(rowid>? AND rowid<?)".
		if cand == nil || cand.k == 0 || (cand.index == nil && cand.rangeBnd == nil && cand.rowidEq == nil && cand.rowidRng == nil) {
			return nil, false
		}
		branch := orBranchPlan{
			IndexCols:  cand.cols,
			Prefix:     cand.prefix,
			Range:      cand.rangeBnd,
			RowidEq:    cand.rowidEq,
			RowidRange: cand.rowidRng != nil,
		}
		if branch.Range == nil && cand.rowidRng != nil {
			branch.Range = cand.rowidRng
		}
		if cand.index != nil {
			branch.IndexName = cand.index.Name
		}
		branch.Residual = orBranchResidual(andTerms, nonORIdxs, constSources, cand.consumedConst, cand.residual)
		branches = append(branches, branch)
	}
	return branches, true
}

// orTableHasRowid reports whether the table is an ordinary rowid table (a
// WITHOUT ROWID table has no rowid pseudo-column to serve an OR branch).
func orTableHasRowid(ctx *DatabaseContext, tableName string) bool {
	entry, err := ctx.Schema.FindTable(tableName)
	if err != nil || entry == nil {
		return true // unresolved: keep the rowid routing available
	}
	return !HasWithoutRowidKeyword(strings.ToUpper(entry.SQL))
}

// orBranchResidual collects a branch's residual WHERE conjuncts: the OR
// term's own unconsumed sub-conjuncts plus every AND-context conjunct whose
// constant equality the branch's prefix did not consume. C compiles the
// consumed terms into the loop's seek and never re-evaluates them per
// candidate row, so the fetch filter evaluates exactly these.
func orBranchResidual(andTerms []sql.Expr, nonORIdxs []int, constSources []int, consumedConst map[int]bool, termResidual []sql.Expr) []sql.Expr {
	out := make([]sql.Expr, 0, len(termResidual)+len(nonORIdxs))
	out = append(out, termResidual...)
	for _, j := range nonORIdxs {
		consumed := false
		for ci, src := range constSources {
			if src == j && consumedConst[ci] {
				consumed = true
				break
			}
		}
		if !consumed {
			out = append(out, andTerms[j])
		}
	}
	return out
}

// mergeOrRangeBounds folds src's missing bound(s) into dst (same column).
// A bound already present stays: two lower bounds on one column cannot both
// constrain the branch (the extractor only emits one comparison per sub).
func mergeOrRangeBounds(dst, src *orRange) {
	if src.Lower != nil && dst.Lower == nil {
		dst.Lower, dst.LowerInclusive = src.Lower, src.LowerInclusive
	}
	if src.Upper != nil && dst.Upper == nil {
		dst.Upper, dst.UpperInclusive = src.Upper, src.UpperInclusive
	}
}

// combineOrDisjunctsPlan ports whereexpr.c's CASE 2 for the OR plan: a
// TWO-way OR is scanned for a pair of subterms (one from each disjunct's
// AND-chain) that whereCombineDisjuncts would combine — same left and right
// operands, operators entirely lower-side (=,<,<=) or entirely upper-side
// (=,>,>=). The combined virtual term ("x>A OR (x=A AND y>B)" adds "x>=A")
// drives the collapsed plan when an index (or the INTEGER PRIMARY KEY) can
// serve it; ok is false when no pair combines or no index serves the
// combined term (the multi-branch path stays responsible). The consumed
// map reports which AND-context equalities the branch's prefix took.
func (e *DMLExecutor) combineOrDisjunctsPlan(orTerm sql.Expr, indexes []*schema.Entry, constConstraints []orConstraint, colDefs []sql.ColumnDef, outerCols map[string]bool, rowidTable bool) (orBranchPlan, map[int]bool, bool) {
	orTerm = unwrapParen(orTerm)
	bin, ok := orTerm.(*sql.BinaryOp)
	if !ok || !strings.EqualFold(bin.Operator, "OR") {
		return orBranchPlan{}, nil, false
	}
	// whereNthSubterm's enumeration: each disjunct contributes its
	// AND-conjuncts (the disjunct itself when it is not an AND).
	for _, pOne := range splitAndTerms(bin.Left) {
		for _, pTwo := range splitAndTerms(bin.Right) {
			combined, ok := e.combineDisjunctTerms(pOne, pTwo, colDefs)
			if !ok {
				continue
			}
			cand := e.bestIndexForOrBranch(combined, outerCols, indexes, constConstraints, colDefs, rowidTable)
			if cand == nil || cand.k == 0 || (cand.index == nil && cand.rangeBnd == nil && cand.rowidEq == nil && cand.rowidRng == nil) {
				continue
			}
			branch := orBranchPlan{IndexCols: cand.cols, Prefix: cand.prefix, Range: cand.rangeBnd, RowidEq: cand.rowidEq, RowidRange: cand.rowidRng != nil}
			if branch.Range == nil && cand.rowidRng != nil {
				branch.Range = cand.rowidRng
			}
			if cand.index != nil {
				branch.IndexName = cand.index.Name
			}
			return branch, cand.consumedConst, true
		}
	}
	return orBranchPlan{}, nil, false
}

// combineDisjunctTerms ports whereCombineDisjuncts for one subterm pair: both
// must be column-vs-constant comparisons whose operator set is entirely
// lower-side (=,<,<=) or entirely upper-side (=,>,>=) — mixed directions
// ("x<y OR x>y") do not combine — with structurally equal operands on both
// sides (sqlite3ExprCompare: "2" and "2.0" differ). The combined operator
// widens to <= / >= when the two operators differ; equal operators keep
// their operator ("x=2 OR x=2" combines to "x=2").
func (e *DMLExecutor) combineDisjunctTerms(pOne, pTwo sql.Expr, colDefs []sql.ColumnDef) (sql.Expr, bool) {
	one, ok1 := unwrapParen(pOne).(*sql.BinaryOp)
	two, ok2 := unwrapParen(pTwo).(*sql.BinaryOp)
	if !ok1 || !ok2 {
		return nil, false
	}
	opOne, ok1 := combineOpBit(one.Operator)
	opTwo, ok2 := combineOpBit(two.Operator)
	if !ok1 || !ok2 {
		return nil, false
	}
	eOp := opOne | opTwo
	if eOp&(orOpEq|orOpLt|orOpLe) != eOp && eOp&(orOpEq|orOpGt|orOpGe) != eOp {
		return nil, false
	}
	if !e.exprsStructurallyEqual(colDefs, colDefs, one.Left, two.Left) ||
		!e.exprsStructurallyEqual(colDefs, colDefs, one.Right, two.Right) {
		return nil, false
	}
	op := one.Operator
	if opOne != opTwo {
		if eOp&(orOpLt|orOpLe) != 0 {
			op = "<="
		} else {
			op = ">="
		}
	}
	return &sql.BinaryOp{Operator: op, Left: one.Left, Right: two.Right}, true
}

// orOp comparison-operator bits for the disjunct combination.
const (
	orOpEq = 1 << iota
	orOpLt
	orOpLe
	orOpGt
	orOpGe
)

// combineOpBit maps a comparison operator to its combination bit; ok is
// false for any other operator.
func combineOpBit(op string) (int, bool) {
	switch op {
	case "=", "==":
		return orOpEq, true
	case "<":
		return orOpLt, true
	case "<=":
		return orOpLe, true
	case ">":
		return orOpGt, true
	case ">=":
		return orOpGe, true
	}
	return 0, false
}

// collectOrConstraintTerms splits AND terms into the single OR term and the
// constant equality constraints from the non-OR conjuncts.
// collectOrConstraintTerms splits the WHERE's AND-conjuncts: the OR term,
// the constant equalities usable as index prefixes, and bookkeeping that
// maps each constant equality back to its source conjunct (constSources[i]
// is the andTerms index of constConstraints[i]) and lists the AND indexes of
// every non-OR conjunct (nonORIdxs) so unconsumed ones can stay residual.
func (e *DMLExecutor) collectOrConstraintTerms(andTerms []sql.Expr, outerCols map[string]bool) ([]orConstraint, []int, []int, sql.Expr, int) {
	var constConstraints []orConstraint
	var constSources []int
	var nonORIdxs []int
	var orTerm sql.Expr
	orCount := 0
	for ti, t := range andTerms {
		if isOrExpr(t) {
			orTerm = t
			orCount++
			continue
		}
		nonORIdxs = append(nonORIdxs, ti)
		if col, val, aff, ok := e.extractEquality(t, outerCols); ok {
			constConstraints = append(constConstraints, orConstraint{Col: col, Val: val, ApplyAffinity: aff})
			constSources = append(constSources, ti)
		}
	}
	return constConstraints, constSources, nonORIdxs, orTerm, orCount
}

// orBranchCandidate is bestIndexForOrBranch's resolved shape for one OR
// term: the serving index (or the rowid probe), the constraints compiled
// into the branch, and the bookkeeping the residual split needs.
type orBranchCandidate struct {
	k        int
	index    *schema.Entry
	cols     []string
	prefix   []orConstraint
	rangeBnd *orRange
	rowidEq  *orConstraint
	rowidRng *orRange
	// consumedConst holds the indexes (into the plan's constConstraints
	// slice) of the AND-context equalities the branch's prefix consumed.
	consumedConst map[int]bool
	// residual holds the term's own conjuncts the branch did not consume.
	residual []sql.Expr
}

// orRowidTermCol reports whether a constraint column denotes the table's
// rowid: the declared INTEGER PRIMARY KEY alias, or (on an ordinary rowid
// table) one of SQLite's rowid pseudo-column names. A WITHOUT ROWID table
// has no rowid (its declared PK keeps the old prefix-branch shape); a
// declared column shadows the pseudo-column.
func orRowidTermCol(col string, colDefs []sql.ColumnDef, rowidTable bool) bool {
	if !rowidTable {
		return false
	}
	for _, cd := range colDefs {
		if strings.EqualFold(cd.Name, col) {
			// A declared column shadows the pseudo-column: it is the rowid
			// only when it is the declared INTEGER PRIMARY KEY alias.
			ipk := ipkColumnName(colDefs)
			return ipk != "" && strings.EqualFold(cd.Name, ipk)
		}
	}
	switch strings.ToLower(col) {
	case "rowid", "_rowid_", "oid":
		return true
	}
	return false
}

// bestIndexForOrBranch selects the index with the longest prefix of columns
// covered by the branch's own constraints or a constant AND-context
// constraint. The term's conjuncts are classified once: equality and range
// constraints drive the branch (the rowid routes to the direct table-btree
// probe), everything the branch does not compile into its seek is returned
// as residual.
func (e *DMLExecutor) bestIndexForOrBranch(term sql.Expr, outerCols map[string]bool, indexes []*schema.Entry, constConstraints []orConstraint, colDefs []sql.ColumnDef, rowidTable bool) *orBranchCandidate {
	// Classify this term's conjuncts once: equality constraints, range
	// constraints, and the leftovers that stay residual. An equality or
	// range on the rowid serves the branch without an index ("SEARCH t
	// USING INTEGER PRIMARY KEY (rowid=?)").
	branchCols := make(map[string]orConstraint)
	branchRange := make(map[string]*orRange)
	eqSubs := make(map[string][]sql.Expr)    // equality conjunct exprs per column
	rangeSubs := make(map[string][]sql.Expr) // range conjunct exprs per column
	var residual []sql.Expr
	var rowidEq *orConstraint
	var rowidRng *orRange
	var rowidRangeSubs []sql.Expr
	for _, sub := range splitAndTerms(term) {
		if col, val, aff, ok := e.extractEquality(sub, outerCols); ok {
			c := orConstraint{Col: col, Val: val, ApplyAffinity: aff}
			if orRowidTermCol(col, colDefs, rowidTable) {
				if rowidEq == nil {
					rowidEq = &c
				} else {
					residual = append(residual, sub)
				}
				continue
			}
			if _, dup := branchCols[col]; dup {
				residual = append(residual, sub) // a second eq on one column: re-checked per row
				continue
			}
			branchCols[col] = c
			eqSubs[col] = append(eqSubs[col], sub)
			continue
		}
		if rng, ok := e.extractRange(sub, outerCols); ok {
			if orRowidTermCol(rng.Col, colDefs, rowidTable) {
				if rowidRng == nil {
					rowidRng = rng
				} else {
					mergeOrRangeBounds(rowidRng, rng)
				}
				rowidRangeSubs = append(rowidRangeSubs, sub)
				continue
			}
			if prev, exists := branchRange[rng.Col]; exists {
				mergeOrRangeBounds(prev, rng)
			} else {
				branchRange[rng.Col] = rng
			}
			rangeSubs[rng.Col] = append(rangeSubs[rng.Col], sub)
			continue
		}
		residual = append(residual, sub)
	}
	if len(branchCols) == 0 && len(branchRange) == 0 && rowidEq == nil && rowidRng == nil {
		return nil
	}

	// The rowid serves its own branch: the VDBE probes the table b-tree
	// directly (OP_SeekRowid for the equality, a seek range otherwise).
	// A rowid range beside a rowid equality stays residual (the equality
	// pins the row; the range is re-checked per candidate).
	if rowidEq != nil {
		residual = append(residual, rowidRangeSubs...)
		return &orBranchCandidate{k: 1, rowidEq: rowidEq, residual: residual}
	}
	if rowidRng != nil {
		return &orBranchCandidate{k: 1, rowidRng: rowidRng, residual: residual}
	}
	// A WITHOUT ROWID table's declared PK serves a branch through the row
	// map (its cell rowids are synthetic): the equality arm keeps the
	// prefix-branch shape, the range arm the range-branch shape — matched
	// by column, counted as a seek + Nexts like any branch.
	if !rowidTable {
		if pk := ipkColumnName(colDefs); pk != "" {
			if rng, ok := branchRange[pk]; ok {
				res := append(residual, rowidRangeSubs...)
				for col, subs := range rangeSubs {
					if !strings.EqualFold(col, pk) {
						res = append(res, subs...)
					}
				}
				for _, subs := range eqSubs {
					res = append(res, subs...)
				}
				return &orBranchCandidate{k: 1, rangeBnd: rng, residual: res}
			}
			if eq, ok := branchCols[pk]; ok {
				res := residual
				for col, subs := range eqSubs {
					if !strings.EqualFold(col, pk) {
						res = append(res, subs...)
					}
				}
				for _, subs := range rangeSubs {
					res = append(res, subs...)
				}
				return &orBranchCandidate{k: 1, prefix: []orConstraint{eq}, residual: res}
			}
		}
	}

	bestScore := 0
	cand := &orBranchCandidate{}
	for _, idx := range indexes {
		cols := indexPlainColumns(idx.SQL)
		if len(cols) == 0 {
			continue
		}
		prefix, k, consumed := orBranchPrefix(cols, branchCols, constConstraints)
		// An equality prefix is worth more than a range: the prefix pins
		// the index to one key, the range only bounds it.
		score := k * 2
		if score > bestScore {
			bestScore = score
			cand.k = k
			cand.index = idx
			cand.cols = cols
			cand.prefix = prefix
			cand.rangeBnd = nil
			cand.consumedConst = consumed
		}
		// A range on the index's leading column serves the branch even
		// without an equality prefix.
		if k == 0 && bestScore == 0 {
			if rng, ok := branchRange[cols[0]]; ok {
				bestScore = 1
				cand.k = 1
				cand.index = idx
				cand.cols = cols
				cand.prefix = nil
				cand.rangeBnd = rng
				cand.consumedConst = consumed
			}
		}
	}
	if cand.k == 0 {
		return nil
	}
	// The prefix consumes the branch equalities whose columns it pinned;
	// a range branch consumes the leading-column bounds. Everything else —
	// an equality outside the prefix, a range the branch does not drive —
	// is re-checked per candidate row (C's residual WHERE): dropping one
	// would emit rows the predicate rejects (where7-2.2.1's
	// `g='qponmlk' AND f GLOB 'nopqr*'` planned on t2g leaks the rows the
	// GLOB rejects).
	consumedEq := make(map[string]bool, len(cand.prefix))
	consumedRange := make(map[string]bool, 1)
	if cand.rangeBnd == nil {
		for _, pc := range cand.prefix {
			if _, fromBranch := branchCols[pc.Col]; fromBranch {
				consumedEq[strings.ToLower(pc.Col)] = true
			}
		}
	} else {
		consumedRange[strings.ToLower(cand.rangeBnd.Col)] = true
	}
	for col, subs := range eqSubs {
		if consumedEq[strings.ToLower(col)] {
			continue
		}
		residual = append(residual, subs...)
	}
	for col, subs := range rangeSubs {
		if consumedRange[strings.ToLower(col)] {
			continue
		}
		residual = append(residual, subs...)
	}
	cand.residual = residual
	return cand
}

// extractRange extracts a range constraint on one column: "col > const",
// "col >= const", "col < const", "col <= const" or "col BETWEEN a AND b".
// The comparison the branch imposes is the one SQLite would render in its
// plan ("d>? AND d<?").
func (e *DMLExecutor) extractRange(expr sql.Expr, outerCols map[string]bool) (*orRange, bool) {
	expr = unwrapParen(expr)
	switch v := expr.(type) {
	case *sql.BinaryOp:
		switch v.Operator {
		case "GLOB", "LIKE":
			// A LIKE/GLOB prefix serves the branch as a range scan
			// (SQLite: "SEARCH t2 USING INDEX t2f (f>? AND f<?)").
			col, ok := columnRefName(v.Left)
			if !ok {
				return nil, false
			}
			pattern, ok := execquery.LikeGlobPatternConst(v.Right)
			if !ok {
				return nil, false
			}
			low, high, ok := execquery.LikeGlobRangeBounds(pattern, v.Escape, v.HasEscape, v.Operator == "GLOB")
			if !ok {
				return nil, false
			}
			return &orRange{Col: col, Lower: low, LowerInclusive: true, Upper: high, UpperInclusive: false, ApplyAffinity: false, LikeGlob: true}, true
		case ">", ">=", "<", "<=":
			col, ok := columnRefName(v.Left)
			var bound interface{}
			op := v.Operator
			if ok {
				bound, ok = e.evalConstExpr(v.Right, outerCols)
			}
			if !ok {
				// The constant may be on the left: "1000000 < b" is the
				// range b > 1000000.
				col, ok = columnRefName(v.Right)
				if !ok {
					return nil, false
				}
				bound, ok = e.evalConstExpr(v.Left, outerCols)
				if !ok {
					return nil, false
				}
				op = flipComparison(v.Operator)
			}
			if bound == nil {
				// A NULL-constant bound ("col < NULL") compares NULL: the
				// term is never true and must stay residual, where its
				// evaluation rejects every row (a bound the matcher skips
				// would match everything — rowvalue2-4.IDX2's `b < NULL`).
				return nil, false
			}
			rng := &orRange{Col: col, ApplyAffinity: true}
			switch op {
			case ">":
				rng.Lower, rng.LowerInclusive = bound, false
			case ">=":
				rng.Lower, rng.LowerInclusive = bound, true
			case "<":
				rng.Upper, rng.UpperInclusive = bound, false
			case "<=":
				rng.Upper, rng.UpperInclusive = bound, true
			}
			return rng, true
		}
	case *sql.Between:
		col, ok := columnRefName(v.Operand)
		if !ok {
			return nil, false
		}
		lower, okLower := e.evalConstExpr(v.Low, outerCols)
		upper, okUpper := e.evalConstExpr(v.High, outerCols)
		if !okLower || !okUpper || lower == nil || upper == nil {
			// A NULL BETWEEN bound never compares true: stay residual.
			return nil, false
		}
		return &orRange{Col: col, Lower: lower, LowerInclusive: true, Upper: upper, UpperInclusive: true, ApplyAffinity: true}, true
	}
	return nil, false
}

// flipComparison returns the comparison that is equivalent to the given one
// with the operands swapped.
func flipComparison(op string) string {
	switch op {
	case ">":
		return "<"
	case ">=":
		return "<="
	case "<":
		return ">"
	case "<=":
		return ">="
	}
	return op
}

// ipkColumnName returns the table's INTEGER PRIMARY KEY alias column name
// ("" when the table has none).
func ipkColumnName(colDefs []sql.ColumnDef) string {
	for i := range colDefs {
		if IsIPKRowidAliasCol(colDefs[i]) {
			return colDefs[i].Name
		}
	}
	return ""
}

// columnRefName returns the bare column name of an expression that is a plain
// column reference (a unary "+" wrapper does not qualify: it makes the term
// unusable for an index).
func columnRefName(expr sql.Expr) (string, bool) {
	expr = unwrapParen(expr)
	ref, ok := expr.(*sql.ColumnRef)
	if !ok {
		return "", false
	}
	return ref.Name, true
}

// orBranchPrefix builds the constraint prefix for one index column list,
// falling back to a constant equality from a sibling AND term when the branch
// has no constraint for a column. The prefix stops at the first uncovered
// column. The returned set holds the indexes (into constConstraints) the
// prefix consumed — an unconsumed constant equality stays residual.
func orBranchPrefix(cols []string, branchCols map[string]orConstraint, constConstraints []orConstraint) ([]orConstraint, int, map[int]bool) {
	var prefix []orConstraint
	k := 0
	consumed := make(map[int]bool)
	for _, ic := range cols {
		c, ok := branchCols[ic]
		if !ok {
			found := false
			for ci, cc := range constConstraints {
				if consumed[ci] {
					continue
				}
				if cc.Col == ic {
					c = cc
					found = true
					consumed[ci] = true
					break
				}
			}
			if !found {
				break
			}
		}
		prefix = append(prefix, c)
		k++
	}
	return prefix, k, consumed
}

// unwrapParen strips any number of enclosing parentheses.
func unwrapParen(expr sql.Expr) sql.Expr {
	for {
		p, ok := expr.(*sql.ParenExpr)
		if !ok {
			return expr
		}
		expr = p.Expr
	}
}

// isOrExpr reports whether the expression is a top-level OR.
func isOrExpr(expr sql.Expr) bool {
	b, ok := unwrapParen(expr).(*sql.BinaryOp)
	return ok && strings.EqualFold(b.Operator, "OR")
}

// splitAndTerms splits a top-level AND chain into its conjuncts.
func splitAndTerms(expr sql.Expr) []sql.Expr {
	return appendAndTerms(nil, expr)
}

// splitAndTermsInto decomposes expr into its top-level AND conjuncts,
// appending into dst (the DML executor's reusable scratch — callers consume
// the terms before the next decomposition; nothing retains the slice).
func (e *DMLExecutor) splitAndTermsInto(expr sql.Expr) []sql.Expr {
	e.andTerms = appendAndTerms(e.andTerms[:0], expr)
	return e.andTerms
}

// appendAndTerms appends expr's top-level AND conjuncts to dst in
// splitAndTerms' left-to-right order.
func appendAndTerms(dst []sql.Expr, expr sql.Expr) []sql.Expr {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return append(dst, expr)
	}
	if strings.EqualFold(b.Operator, "AND") {
		dst = appendAndTerms(dst, b.Left)
		return appendAndTerms(dst, b.Right)
	}
	return append(dst, expr)
}

// andTermCount counts expr's top-level AND conjuncts without materializing
// the term list (the point-op gates only test the count).
func andTermCount(expr sql.Expr) int {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return 1
	}
	if strings.EqualFold(b.Operator, "AND") {
		return andTermCount(b.Left) + andTermCount(b.Right)
	}
	return 1
}

// flattenOrTerms converts an expression into DNF OR terms, distributing AND
// over OR. The result is a flat list where each element is a conjunction.
func flattenOrTerms(expr sql.Expr) []sql.Expr {
	expr = unwrapParen(expr)
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return []sql.Expr{expr}
	}
	switch strings.ToUpper(b.Operator) {
	case "OR":
		return append(flattenOrTerms(b.Left), flattenOrTerms(b.Right)...)
	case "AND":
		lefts := flattenOrTerms(b.Left)
		rights := flattenOrTerms(b.Right)
		out := make([]sql.Expr, 0, len(lefts)*len(rights))
		for _, l := range lefts {
			for _, r := range rights {
				out = append(out, &sql.BinaryOp{Operator: "AND", Left: l, Right: r})
			}
		}
		return out
	}
	return []sql.Expr{expr}
}

// extractEquality extracts a col=value equality usable as an index prefix
// term. Returns the column name, the constant value, whether the comparison
// applies the column's affinity, and ok. The value side may be a literal or a
// constant scalar subquery (one that does not reference the outer table).
func (e *DMLExecutor) extractEquality(expr sql.Expr, outerCols map[string]bool) (string, interface{}, bool, bool) {
	expr = unwrapParen(expr)
	// "col IS NULL" is an equality on NULL: SQLite serves it with an index
	// seek ("SEARCH t2 USING INDEX t2e (e=?)"). No affinity applies.
	if isNull, ok := expr.(*sql.IsNull); ok {
		col, ok := columnRefName(isNull.Operand)
		if !ok {
			return "", nil, false, false
		}
		return col, nil, false, true
	}
	b, ok := expr.(*sql.BinaryOp)
	if !ok {
		return "", nil, false, false
	}
	if b.Operator == "IS" {
		if _, isNull := b.Right.(*sql.NullLit); !isNull {
			return "", nil, false, false
		}
		col, ok := columnRefName(b.Left)
		if !ok {
			return "", nil, false, false
		}
		return col, nil, false, true
	}
	if b.Operator != "=" {
		return "", nil, false, false
	}
	// A unary "+" wrapper makes the term unusable for an index SEEK: an
	// index constrains a term only when its operand is a bare column
	// reference, so "+c=6" cannot drive t1c (SQLite falls back to a scan -
	// where.c leaves WHERE_CONSTRAINT unset for such a term). The "+" still
	// strips the column's affinity (columnRefSide's second result), but the
	// term must not pick an index.
	if isUnaryPlusColumn(b.Left) || isUnaryPlusColumn(b.Right) {
		return "", nil, false, false
	}
	if col, aff, ok := columnRefSide(b.Left); ok {
		val, ok := e.evalConstExpr(b.Right, outerCols)
		if !ok || val == nil {
			// A NULL constant on the value side ("col = NULL") is never
			// true in SQL: the term is not an index constraint and must
			// stay residual, where its evaluation rejects every row (the
			// prefix matcher's nil Val means "col IS NULL" — different
			// semantics; rowvalue2-4.IDX2's `b == NULL` arm pinned this).
			return "", nil, false, false
		}
		return col, val, aff, true
	}
	if col, aff, ok := columnRefSide(b.Right); ok {
		val, ok := e.evalConstExpr(b.Left, outerCols)
		if !ok || val == nil {
			return "", nil, false, false
		}
		return col, val, aff, true
	}
	return "", nil, false, false
}

// isUnaryPlusColumn reports whether an expression is a column wrapped in the
// unary "+" operator (SQLite's "+col", which strips the column's affinity and
// makes the term unusable for an index seek).
func isUnaryPlusColumn(expr sql.Expr) bool {
	expr = unwrapParen(expr)
	u, ok := expr.(*sql.UnaryOp)
	if !ok || u.Operator != "+" {
		return false
	}
	_, ok = unwrapParen(u.Operand).(*sql.ColumnRef)
	return ok
}

// columnRefSide extracts a column reference from one side of a comparison,
// allowing a unary + wrapper (+col). Returns the column name, whether the
// comparison applies the column's affinity (false for +col), and ok.
func columnRefSide(expr sql.Expr) (string, bool, bool) {
	expr = unwrapParen(expr)
	switch v := expr.(type) {
	case *sql.ColumnRef:
		return v.Name, true, true
	case *sql.UnaryOp:
		if v.Operator == "+" {
			if inner, ok := unwrapParen(v.Operand).(*sql.ColumnRef); ok {
				return inner.Name, false, true
			}
		}
	}
	return "", false, false
}

// evalConstExpr evaluates an expression expected to be constant: a literal, a
// simple unary-op expression, or a scalar subquery that does not reference the
// outer table's columns (a correlated subquery is not constant).
func (e *DMLExecutor) evalConstExpr(expr sql.Expr, outerCols map[string]bool) (interface{}, bool) {
	expr = unwrapParen(expr)
	switch v := expr.(type) {
	case *sql.NumericLit:
		if c := v.Cached(); c != nil {
			return c, true
		}
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	case *sql.StringLit:
		return v.Value, true
	case *sql.BlobLit:
		return v.Value, true
	case *sql.NullLit:
		return nil, true
	case *sql.UnaryOp:
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	case *sql.Subquery:
		if e.selectRefsOuterTable(v.Select, outerCols) {
			return nil, false
		}
		val, err := e.ctx.EvalExpr(v, nil)
		if err != nil {
			return nil, false
		}
		return val, true
	}
	return nil, false
}

// selectRefsOuterTable reports whether any column reference inside the SELECT
// (select list, WHERE, GROUP BY, HAVING, ORDER BY) resolves to one of
// outerCols — i.e. the subquery is correlated to the outer table.
func (e *DMLExecutor) selectRefsOuterTable(s *sql.SelectStmt, outerCols map[string]bool) bool {
	var refs []string
	collect := func(expr sql.Expr) {
		if expr == nil {
			return
		}
		execquery.WalkExprFull(expr, func(sub sql.Expr) {
			if cr, ok := sub.(*sql.ColumnRef); ok {
				refs = append(refs, cr.Name)
			}
		})
	}
	for _, col := range s.Columns {
		collect(col.Expr)
	}
	collect(s.Where)
	for _, g := range s.GroupBy {
		collect(g)
	}
	collect(s.Having)
	for _, ob := range s.OrderBy {
		collect(ob.Expr)
	}
	for _, r := range refs {
		name := r
		if dot := strings.Index(r, "."); dot >= 0 {
			name = r[dot+1:]
		}
		if outerCols[name] {
			return true
		}
	}
	return false
}

// indexPlainColumns extracts the plain column names of an index from its SQL.
// Expression columns (containing parens) or non-identifier text cause the
// index to be skipped (returning an empty slice), because the engine does not
// build correct values for them.
func indexPlainColumns(sqlStr string) []string {
	cols := parseIndexColumns(sqlStr)
	if len(cols) == 0 {
		return nil
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(c)
		// Strip a trailing COLLATE clause.
		if i := strings.Index(strings.ToUpper(c), " COLLATE "); i >= 0 {
			c = strings.TrimSpace(c[:i])
		}
		// Strip a trailing ASC / DESC marker.
		upper := strings.ToUpper(c)
		if strings.HasSuffix(upper, " DESC") {
			c = strings.TrimSpace(c[:len(c)-len(" DESC")])
		} else if strings.HasSuffix(upper, " ASC") {
			c = strings.TrimSpace(c[:len(c)-len(" ASC")])
		}
		c = strings.TrimSpace(strings.Trim(c, "`\"[]"))
		if c == "" || strings.ContainsAny(c, "()+-*/%<>= ") {
			return nil // expression or compound column — not usable as a plain prefix
		}
		out = append(out, c)
	}
	return out
}

// execSelectWithOrPlan executes a single-table SELECT whose WHERE was planned
// by planOrIndexScan. Rows are produced in SQLite's OR-index union order:
// for each OR term in order, the matching rows in that term's index scan
// order (deduplicated against rows already emitted).
func (e *DMLExecutor) execSelectWithOrPlan(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) *Result {
	allRows, allRowMaps, coreRes := e.orPlanSelectRows(s, tableEntry, dbCtx, colDefs, branches)
	if coreRes != nil {
		return coreRes
	}
	// Aggregate and finalize exactly like the regular scan path.
	if result := e.ctx.HandleSelectAggregates(s, allRowMaps, colDefs); result != nil {
		return result
	}
	result := &Result{Columns: e.ctx.BuildColumnNames(s.Columns, colDefs, s), Rows: allRows}
	// The rows come out in OR-branch order (each branch contributes its own
	// index order), so an ORDER BY needs a comparator sort — SQLite's
	// MULTI-INDEX OR always adds a temp b-tree for one.
	result.RowsInBranchOrder = true
	return e.ctx.FinalizeSelectResult(result, s, allRowMaps)
}

// orPlanSelectRows runs the OR-branch walk and returns the raw output rows
// and full row maps in branch scan order (no aggregate/finalize pass — the
// join base loop feeds them into the join pipeline, which owns the
// statement-level handling). A non-nil *Result carries an error.
func (e *DMLExecutor) orPlanSelectRows(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) ([][]interface{}, []RowMap, *Result) {
	prevScanTable := e.ctx.CurrentScanTable()
	e.ctx.SetCurrentScanTable(tableEntry.Name)
	if s.From.As != "" {
		e.ctx.SetCurrentScanTable(s.From.As)
	}
	defer func() { e.ctx.SetCurrentScanTable(prevScanTable) }()

	tree := e.ctx.TableBTreePg(dbCtx.Pager, tableEntry.Name, tableEntry.RootPage, true)
	defer tree.Close() // scan tree is function-local
	cursor, err := tree.OpenCursor()
	if err != nil {
		return nil, nil, &Result{Error: err}
	}

	colIndex := make(map[string]int, len(colDefs))
	for i, cd := range colDefs {
		colIndex[cd.Name] = i
	}

	// Pass 1: scan the table once, collecting for every branch the rowids of
	// rows whose prefix columns match, along with the index key values used
	// to reproduce the index scan order.
	matches, beyond := e.collectOrBranchMatches(cursor, branches, tableEntry, colDefs, colIndex)

	// The search counter for the plan SQLite would run (its MULTI-INDEX OR
	// walks each branch's index): one seek per branch, one successful Next
	// per consumed entry except the branch range's EOF step, and a second
	// (blob-pass) seek for each LIKE/GLOB branch (vdbe.c:4973, 6530;
	// wherecode.c's LIKE loop). A rowid-equality branch runs OP_SeekRowid —
	// not counted, with no Next loop. The movetos are counted per EMITTED
	// row in the walk below (C's deferred row fetches fire per arm,
	// OP_DeferredSeek + the aAltMap redirect, vdbe.c:3022).
	searchDelta := int64(0)
	for bi, br := range branches {
		if br.RowidEq != nil {
			continue // OP_SeekRowid: the counter does not track it
		}
		searchDelta++ // the branch seek
		if n := len(matches[bi]); n > 0 {
			nexts := int64(n)
			if !beyond[bi] {
				nexts-- // the branch's last Next steps to EOF and fails
			}
			searchDelta += nexts
		}
		if br.Range != nil && br.Range.LikeGlob {
			searchDelta++ // the blob-pass seek of the LIKE-optimization loop
		}
	}

	// Pass 2: walk the branch matches in branch scan order (the order C's
	// RowSet iterates), fetch each candidate, and apply the OWNING branch's
	// residual WHERE conjuncts — the terms the plan did not compile into
	// its seek (C never re-evaluates a consumed constraint per candidate).
	// The dedup mirrors C's RowSetTest placement: the rowid enters the
	// rowset only after the arm's residual filter accepts the row, so a
	// row one arm rejects stays available to a later arm (where7-2.4.1:
	// `g='rqponml' AND f GLOB 'hijkl*'` rejects row 37, then `b=407`
	// emits it).
	outputRefs := orPlanOutputRefs(s, colDefs)
	movetos := int64(0)
	needMaps := e.ctx.SelectNeedsRowMaps(s, tableEntry.Name)
	// C's deferred row fetch (OP_DeferredSeek -> sqlite3VdbeFinishMoveto)
	// fires at the first table-cursor read of a candidate — at most once
	// per candidate, since the read positions the cursor. Two read phases
	// exist per arm: the residual terms (evaluated BEFORE C's RowSetTest,
	// so rejected and duplicate candidates count) and the output block
	// (after the RowSetTest, so only emitted rows count). A branch whose
	// index covers a phase's columns serves it from the index cursor
	// without the fetch (the aAltMap redirect, vdbe.c:3022); a rowid
	// branch's cursor is positioned directly and never fetches
	// (where8-1.10's rejected row 10 counts, its duplicate in 1.4a's GLOB
	// arm does not).
	residualCovers := make([]bool, len(branches))
	outputCovers := make([]bool, len(branches))
	for bi := range branches {
		residualCovers[bi] = orBranchCovers(branches[bi], orBranchResidualRefs(branches[bi]))
		outputCovers[bi] = orBranchCovers(branches[bi], outputRefs)
	}
	var allRows [][]interface{}
	var allRowMaps []RowMap
	seen := make(map[int64]bool)
	seenWR := make(map[string]bool)
	for bi, br := range branches {
		ms := matches[bi]
		sort.Slice(ms, func(i, j int) bool { return compareValueLists(ms[i].key, ms[j].key) < 0 })
		for _, m := range ms {
			switch {
			case br.IsRowidBranch():
				// the seek positioned the table cursor directly
			case !residualCovers[bi]:
				movetos++ // the residual read fetches, even on rejection
			default:
				// the output read decides (emitted rows only)
			}
			if m.rowID == 0 {
				if seenWR[m.rawKey] {
					continue
				}
			} else if seen[m.rowID] {
				continue
			}
			row, ok, res := e.fetchOrPlanRow(cursor, m, branches, s, tableEntry, colDefs)
			if res != nil {
				return nil, nil, res
			}
			if !ok {
				continue // the owning branch's residual rejected the row
			}
			if m.rowID == 0 {
				seenWR[m.rawKey] = true
			} else {
				seen[m.rowID] = true
			}
			if !br.IsRowidBranch() && residualCovers[bi] && !outputCovers[bi] {
				movetos++ // the output read fetches (the residual did not)
			}
			allRows = append(allRows, e.ctx.BuildOutputRow(s.Columns, colDefs, row))
			if needMaps {
				allRowMaps = append(allRowMaps, row)
			}
		}
	}
	e.ctx.IncSearchCount(searchDelta + movetos)
	return allRows, allRowMaps, nil
}

// ExecSelectWithOrPlanRows runs the OR plan and returns the raw branch-order
// rows and full row maps (no finalize) — the join base loop's row source.
func (e *DMLExecutor) ExecSelectWithOrPlanRows(s *sql.SelectStmt, tableEntry *schema.Entry, dbCtx *DatabaseContext, colDefs []sql.ColumnDef, branches []orBranchPlan) ([][]interface{}, []RowMap, error) {
	rows, maps, res := e.orPlanSelectRows(s, tableEntry, dbCtx, colDefs, branches)
	if res != nil {
		return nil, nil, res.Error
	}
	return rows, maps, nil
}

// branchMatch records one row that matched an OR branch's prefix, together
// with the index key values used to reproduce the index scan order and the
// branch that matched it (the per-arm moveto model keys on the owning
// branch's index coverage; the first branch to reach a rowid owns the
// emitted row, matching C's RowSetTest dedup). For WITHOUT ROWID tables
// rowID is synthetic 0 for every row, so rawKey (the storage-order cell
// payload) identifies the row instead.
type branchMatch struct {
	rowID  int64
	rawKey string
	key    []interface{}
	branch int
}

// collectOrBranchMatches scans the table once, collecting for every branch the
// rowids of rows whose prefix columns match, plus their index key values.
func (e *DMLExecutor) collectOrBranchMatches(cursor *btree.Cursor, branches []orBranchPlan, tableEntry *schema.Entry, colDefs []sql.ColumnDef, colIndex map[string]int) ([][]branchMatch, []bool) {
	matches := make([][]branchMatch, len(branches))
	// beyond[bi]: some row's key on the branch's leading column sorts after
	// the branch's bound — SQLite's Next would step onto that boundary
	// entry (successfully) before the bound check exits the loop.
	beyond := make([]bool, len(branches))
	for {
		cell, err := cursor.ReadCell()
		if err != nil || cell == nil {
			break
		}
		rec, err := storage.DecodeRecord(cell.Payload)
		if err != nil || rec == nil {
			break
		}
		// WITHOUT ROWID records are PK-first storage order; remap so the
		// positional prefix matcher sees declared column order.
		if tableEntry != nil {
			e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
		}
		e.matchRowAgainstBranches(rec, cell.RowID, string(cell.Payload), branches, colDefs, colIndex, matches)

		row := e.ctx.BuildRowMap(rec, colDefs, cell.RowID)
		for bi, br := range branches {
			switch {
			case br.RowidEq != nil:
				// OP_SeekRowid opens no loop: no boundary Next exists.
			case br.RowidRange:
				// A rowid-range branch walks the table b-tree: the boundary
				// Next's successor exists when any rowid lies past the
				// range's upper bound.
				if orRowidSortsAfter(cell.RowID, br.Range) {
					beyond[bi] = true
				}
			case valueSortsAfterBranchBound(rowValueForBranch(row, br), br):
				beyond[bi] = true
			}
		}

		ok, err := cursor.Next()
		if err != nil || !ok {
			break
		}
	}
	return matches, beyond
}

// rowValueForBranch returns the row's value on the branch's leading index
// column (the range column, else the first prefix column).
func rowValueForBranch(row map[string]interface{}, br orBranchPlan) interface{} {
	col := ""
	if br.Range != nil {
		col = br.Range.Col
	} else if len(br.Prefix) > 0 {
		col = br.Prefix[0].Col
	}
	if col == "" {
		return nil
	}
	return util.UnwrapColumnValue(row[col])
}

// valueSortsAfterBranchBound reports whether v sorts after the branch's
// upper scope in index order (NULL < numbers < text < blobs): such an entry
// is the successful Next step SQLite takes before its bound check fails.
// An eq-prefix branch's scope is "not after the eq value".
func valueSortsAfterBranchBound(v interface{}, br orBranchPlan) bool {
	var bound interface{}
	inclusive := false
	if br.Range != nil {
		if br.Range.Upper == nil {
			return false // open-ended: entries never exit via the bound
		}
		bound, inclusive = br.Range.Upper, br.Range.UpperInclusive
	} else if len(br.Prefix) > 0 {
		bound, inclusive = br.Prefix[0].Val, true
	} else {
		return false
	}
	cmp := indexValueOrder(v, bound)
	if inclusive {
		return cmp > 0
	}
	return cmp >= 0
}

// indexValueOrder compares two SQL values by storage-class rank then value
// (NULL < numeric < text < blob; BINARY within text/blob).
func indexValueOrder(a, b interface{}) int {
	ra, rb := storageClassRank(a), storageClassRank(b)
	if ra != rb {
		return ra - rb
	}
	switch ra {
	case 1: // numeric
		af, aok := toFloatForOrder(a)
		bf, bok := toFloatForOrder(b)
		if !aok || !bok {
			return 0
		}
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		}
		return 0
	case 2, 3: // text / blob
		as, aok := toStringForOrder(a)
		bs, bok := toStringForOrder(b)
		if !aok || !bok {
			return 0
		}
		return bytes.Compare([]byte(as), []byte(bs))
	}
	return 0
}

// storageClassRank orders NULL=0, numeric=1, text=2, blob=3 (index order).
func storageClassRank(v interface{}) int {
	switch x := v.(type) {
	case nil:
		return 0
	case int64, float64, int, float32:
		return 1
	case string:
		_ = x
		return 2
	case []byte:
		return 3
	}
	return 2
}

// toFloatForOrder extracts a float from a numeric value for ordering.
func toFloatForOrder(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case float32:
		return float64(x), true
	}
	return 0, false
}

// toStringForOrder extracts the bytes of a text/blob value for ordering.
func toStringForOrder(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

// matchRowAgainstBranches checks one decoded record against every OR branch
// and appends a branchMatch for each branch whose constraint it satisfies:
// the prefix equalities and range for an index branch, the rowid probe or
// rowid range for a branch served by the table b-tree.
func (e *DMLExecutor) matchRowAgainstBranches(rec *storage.Record, rowID int64, rawKey string, branches []orBranchPlan, colDefs []sql.ColumnDef, colIndex map[string]int, matches [][]branchMatch) {
	row := e.ctx.BuildRowMap(rec, colDefs, rowID)
	for bi, br := range branches {
		switch {
		case br.RowidEq != nil:
			if !e.orRowMatchesRowidEq(rowID, br.RowidEq) {
				continue
			}
		case br.RowidRange:
			if !e.orRowMatchesRowidRange(rowID, br.Range) {
				continue
			}
		default:
			if !e.orRowMatchesPrefix(row, br.Prefix) || !e.orRowMatchesRange(row, br.Range) {
				continue
			}
		}
		matches[bi] = append(matches[bi], branchMatch{rowID: rowID, rawKey: rawKey, key: branchKeyValues(rec, br, colIndex, rowID), branch: bi})
	}
}

// orRowMatchesRowidEq reports whether a cell's rowid satisfies the branch's
// rowid equality probe. The rowid carries INTEGER affinity, so the probe
// value coerces like C's seek register (sqlite3VdbeIntegerAffinity): the
// text '4' matches rowid 4.
func (e *DMLExecutor) orRowMatchesRowidEq(rowID int64, c *orConstraint) bool {
	if c == nil || c.Val == nil {
		return false
	}
	return e.ctx.CompareValuesWithCollate(rowID, util.ApplyColumnAffinity(c.Val, "INTEGER")) == 0
}

// orRowMatchesRowidRange reports whether a cell's rowid lies within the
// branch's rowid range bounds.
func (e *DMLExecutor) orRowMatchesRowidRange(rowID int64, rng *orRange) bool {
	if rng == nil {
		return true
	}
	if rng.Lower != nil {
		cmp := e.ctx.CompareValuesWithCollate(rowID, util.ApplyColumnAffinity(rng.Lower, "INTEGER"))
		if cmp < 0 || (cmp == 0 && !rng.LowerInclusive) {
			return false
		}
	}
	if rng.Upper != nil {
		cmp := e.ctx.CompareValuesWithCollate(rowID, util.ApplyColumnAffinity(rng.Upper, "INTEGER"))
		if cmp > 0 || (cmp == 0 && !rng.UpperInclusive) {
			return false
		}
	}
	return true
}

// orRowidSortsAfter reports whether a rowid sorts past a rowid range's upper
// scope — the successful boundary Next of the branch's table-btree walk.
func orRowidSortsAfter(rowID int64, rng *orRange) bool {
	if rng == nil || rng.Upper == nil {
		return false
	}
	cmp := indexValueOrder(rowID, rng.Upper)
	if rng.UpperInclusive {
		return cmp > 0
	}
	return cmp >= 0
}

// branchKeyValues builds the index key value list for one branch match,
// appending the rowid as the final key component.
func branchKeyValues(rec *storage.Record, br orBranchPlan, colIndex map[string]int, rowID int64) []interface{} {
	keyVals := make([]interface{}, 0, len(br.IndexCols)+1)
	for _, ic := range br.IndexCols {
		idx, ok := colIndex[ic]
		if !ok || idx < 0 || idx >= len(rec.Values) {
			keyVals = append(keyVals, nil)
			continue
		}
		keyVals = append(keyVals, rec.Values[idx])
	}
	keyVals = append(keyVals, rowID)
	return keyVals
}

// fetchOrPlanRow seeks to a rowid, decodes the record, and applies the
// branch's residual WHERE conjuncts. Returns ok=false when the rowid is not
// found or a residual conjunct rejects the row.
func (e *DMLExecutor) fetchOrPlanRow(cursor *btree.Cursor, m branchMatch, branches []orBranchPlan, s *sql.SelectStmt, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (RowMap, bool, *Result) {
	residual := []sql.Expr(nil)
	if m.branch >= 0 && m.branch < len(branches) {
		residual = branches[m.branch].Residual
	}
	if m.rowID == 0 && tableEntry != nil {
		return e.fetchOrPlanWRRow(cursor, m, residual, tableEntry, colDefs)
	}
	return e.fetchOrPlanRowidRow(cursor, m.rowID, residual, colDefs)
}

// fetchOrPlanWRRow fetches one WITHOUT ROWID candidate (its rowid is the
// synthetic 0 and rawKey holds the encoded PK-first record).
func (e *DMLExecutor) fetchOrPlanWRRow(cursor *btree.Cursor, m branchMatch, residual []sql.Expr, tableEntry *schema.Entry, colDefs []sql.ColumnDef) (RowMap, bool, *Result) {
	rec, err := storage.DecodeRecord([]byte(m.rawKey))
	if err != nil || rec == nil {
		return nil, false, nil
	}
	e.ctx.RemapWRRecordToDeclared(rec, tableEntry.SQL, colDefs)
	row := e.ctx.BuildRowMap(rec, colDefs, 0)
	return e.orRowAfterResidual(cursor, residual, row)
}

// fetchOrPlanRowidRow fetches one candidate by rowid seek.
func (e *DMLExecutor) fetchOrPlanRowidRow(cursor *btree.Cursor, rowID int64, residual []sql.Expr, colDefs []sql.ColumnDef) (RowMap, bool, *Result) {
	found, err := cursor.SeekToRowID(rowID)
	if err != nil || !found {
		return nil, false, nil
	}
	payload, rowID, err := cursor.ReadCellData()
	if err != nil {
		return nil, false, nil
	}
	rec, err := storage.DecodeRecord(payload)
	if err != nil || rec == nil {
		return nil, false, nil
	}
	row := e.ctx.BuildRowMap(rec, colDefs, rowID)
	return e.orRowAfterResidual(cursor, residual, row)
}

// orRowAfterResidual applies a branch's residual WHERE conjuncts (ANDed —
// they were AND-siblings in the source predicate) to a fetched candidate
// row: res non-nil reports an evaluation error, ok=false rejects the row.
func (e *DMLExecutor) orRowAfterResidual(cursor *btree.Cursor, residual []sql.Expr, row RowMap) (RowMap, bool, *Result) {
	for _, expr := range residual {
		pass, err := e.ctx.RowPassesWhere(expr, row, cursor)
		if err != nil {
			return nil, false, &Result{Error: err}
		}
		if !pass {
			return nil, false, nil
		}
	}
	return row, true, nil
}

// orRowMatchesPrefix reports whether the row satisfies every prefix
// constraint of an OR branch (all equalities; NULL never matches).
func (e *DMLExecutor) orRowMatchesPrefix(row RowMap, prefix []orConstraint) bool {
	for _, pc := range prefix {
		colVal, exists := row[pc.Col]
		if !exists {
			return false
		}
		raw := util.UnwrapColumnValue(execexpr.UnwrapCollatedValue(colVal))
		if pc.Val == nil {
			// "col IS NULL": the row matches when the column is NULL.
			if raw == nil {
				return true
			}
			return false
		}
		if raw == nil {
			return false
		}
		var cmp int
		if pc.ApplyAffinity {
			cmp = e.ctx.CompareValuesWithCollate(colVal, pc.Val)
		} else {
			cmp = e.ctx.CompareValuesWithCollate(raw, pc.Val)
		}
		if cmp != 0 {
			return false
		}
	}
	return true
}

// orRowMatchesRange reports whether the row's value for the range's column
// lies between the range's bounds.
func (e *DMLExecutor) orRowMatchesRange(row RowMap, rng *orRange) bool {
	if rng == nil {
		return true
	}
	colVal, exists := row[rng.Col]
	if !exists || colVal == nil {
		return false
	}
	raw := util.UnwrapColumnValue(execexpr.UnwrapCollatedValue(colVal))
	if raw == nil {
		return false
	}
	compare := func(bound interface{}) int {
		if rng.ApplyAffinity {
			return e.ctx.CompareValuesWithCollate(colVal, bound)
		}
		return e.ctx.CompareValuesWithCollate(raw, bound)
	}
	if rng.Lower != nil {
		cmp := compare(rng.Lower)
		if cmp < 0 || (cmp == 0 && !rng.LowerInclusive) {
			return false
		}
	}
	if rng.Upper != nil {
		cmp := compare(rng.Upper)
		if cmp > 0 || (cmp == 0 && !rng.UpperInclusive) {
			return false
		}
	}
	return true
}

// compareValueLists compares two index key value lists element-wise using
// SQLite's value comparison (NULL < INTEGER/REAL < TEXT < BLOB; INTEGER and
// REAL compare numerically). This reproduces the order a correctly-built
// index b-tree would produce, without relying on the serial-type byte
// encoding (which is not value-ordered across int 0/1 and other small ints).
func compareValueLists(a, b []interface{}) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := util.CompareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

// orPlanOutputRefs collects the lowercase column names the OR-planned
// SELECT reads through the plan's row fetches: its output columns, with a
// bare (or table-qualified) "*" expanding to every table column — C's
// DeferredSeek alt-map covers whatever the output block reads through the
// table cursor.
func orPlanOutputRefs(s *sql.SelectStmt, colDefs []sql.ColumnDef) map[string]bool {
	refs := make(map[string]bool)
	add := func(x sql.Expr) {
		ref, isRef := x.(*sql.ColumnRef)
		if !isRef {
			return
		}
		if ref.Name == "*" {
			for _, cd := range colDefs {
				refs[strings.ToLower(cd.Name)] = true
			}
			return
		}
		refs[strings.ToLower(ref.Name)] = true
	}
	for _, col := range s.Columns {
		walkSelectExprs(col.Expr, add)
	}
	return refs
}

// orBranchResidualRefs collects the lowercase column names a branch's
// residual terms read through the plan's row fetches.
func orBranchResidualRefs(br orBranchPlan) map[string]bool {
	refs := make(map[string]bool)
	for _, expr := range br.Residual {
		walkSelectExprs(expr, func(x sql.Expr) {
			if ref, isRef := x.(*sql.ColumnRef); isRef && ref.Name != "*" {
				refs[strings.ToLower(ref.Name)] = true
			}
		})
	}
	return refs
}

// orBranchCovers reports whether one branch serves the SELECT without
// touching the table b-tree: every referenced column — the outputs plus the
// branch's residual terms' reads — lives in the branch index's key columns,
// with the rowid always available from any index entry (OP_IdxRowid). C
// models this per arm through OP_DeferredSeek's alt-map redirect
// (vdbe.c:3022): a row of a covered arm never fires the deferred moveto.
func orBranchCovers(br orBranchPlan, outputRefs map[string]bool) bool {
	if br.IndexName == "" {
		return br.IsRowidBranch() // rowid branches position the cursor directly
	}
	covered := make(map[string]bool, len(br.IndexCols)+3)
	for _, c := range br.IndexCols {
		covered[strings.ToLower(c)] = true
	}
	covered["rowid"] = true
	covered["_rowid_"] = true
	covered["oid"] = true
	for name := range outputRefs {
		if !covered[name] {
			return false
		}
	}
	for _, expr := range br.Residual {
		coveredByResidual := true
		walkSelectExprs(expr, func(x sql.Expr) {
			if ref, isRef := x.(*sql.ColumnRef); isRef && ref.Name != "*" && !covered[strings.ToLower(ref.Name)] {
				coveredByResidual = false
			}
		})
		if !coveredByResidual {
			return false
		}
	}
	return true
}

// walkSelectExprs walks one expression tree.
func walkSelectExprs(expr sql.Expr, fn func(sql.Expr)) {
	if expr == nil {
		return
	}
	fn(expr)
	switch v := expr.(type) {
	case *sql.BinaryOp:
		walkSelectExprs(v.Left, fn)
		walkSelectExprs(v.Right, fn)
	case *sql.UnaryOp:
		walkSelectExprs(v.Operand, fn)
	case *sql.InList:
		walkSelectExprs(v.Operand, fn)
		for _, item := range v.List {
			walkSelectExprs(item, fn)
		}
	}
}
