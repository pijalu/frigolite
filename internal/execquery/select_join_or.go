// Package exec implements query execution.
//
// This file owns the JOIN statement's INNER-loop OR-branch row source
// (where.c's Case 5 OR loop for an inner operand): an OR conjunct usable at
// the operand's loop level whose every arm constrains an indexed column of
// the operand replaces the default nested-loop probe with a per-outer-row
// arm-order pass — the arms evaluated in order, C's RowSetTest dedup by
// right-row identity, and the ON's remaining conjuncts still filtering each
// candidate.
package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/util"
)

// joinInnerOrArms resolves the inner operand's OR-branch constraint: the
// single OR conjunct usable at this loop level whose every arm constrains an
// indexed column of the right operand. arms is nil when the OR optimization
// does not apply (the default nested loop owns the operand).
func (e *SelectEngine) joinInnerOrArms(s *sql.SelectStmt, join sql.JoinClause, currentDefs, rightDefs []sql.ColumnDef, lastTableName, tableName string) []sql.Expr {
	rightTokens := joinOperandRefTokens(join.Table.As, join.Table.Name)
	rightNames := joinOperandColumnNames(rightDefs)
	leftNames := joinOperandColumnNames(currentDefs)
	leftTokens := []string{strings.ToLower(schemaStrippedName(lastTableName))}
	return e.joinOrArmConjunct(join.On, s.Where, leftNames, rightNames, leftTokens, rightTokens, tableName)
}

// joinOrArmConjunctForClaim resolves the same OR constraint for the ORDER BY
// delivery claim (before the join pipeline materializes anything): the left
// side is the base plus the earlier operands' column sets.
func (e *SelectEngine) joinOrArmConjunctForClaim(s *sql.SelectStmt, joinIdx int, ops []joinDeliveryOperand) []sql.Expr {
	join := &s.Joins[joinIdx]
	op := &ops[joinIdx+1]
	if op.real == "" {
		return nil
	}
	leftNames := make(map[string]bool)
	var leftTokens []string
	for i := 0; i <= joinIdx; i++ {
		for name := range ops[i].names {
			leftNames[name] = true
		}
		if i < joinIdx {
			leftTokens = append(leftTokens, ops[i].tokens...)
		}
	}
	rightNames := op.names
	rightTokens := op.tokens
	return e.joinOrArmConjunct(join.On, s.Where, leftNames, rightNames, leftTokens, rightTokens, op.real)
}

// joinOrArmConjunct scans the ON and the WHERE's top-level conjuncts for the
// single OR term usable at this loop level whose every arm constrains an
// indexed column of the right operand.
func (e *SelectEngine) joinOrArmConjunct(on, where sql.Expr, leftNames, rightNames map[string]bool, leftTokens, rightTokens []string, rightReal string) []sql.Expr {
	var cands []sql.Expr
	cands = append(cands, splitAnd(on)...)
	cands = append(cands, splitAnd(where)...)
	var orConj sql.Expr
	for _, conj := range cands {
		if !exprHasOrSubtree(conj) {
			continue
		}
		if exprHasSubquery(conj) {
			continue
		}
		if !e.joinExprReferencesRight(conj, rightNames, rightTokens) {
			continue // an earlier loop's OR — its own loop's constraint set
		}
		if !e.joinExprUsableNow(conj, leftNames, rightNames, leftTokens, rightTokens) {
			continue
		}
		if orConj != nil {
			return nil // more than one usable OR conjunct: decline
		}
		orConj = conj
	}
	if orConj == nil {
		return nil
	}
	arms := splitOrTopLevel(orConj)
	if len(arms) < 2 {
		return nil
	}
	// C's CASE 1 (exprAnalyzeOrTerm chngToIN): every arm a single equality
	// on the SAME column of the operand — the OR becomes ONE index seek with
	// the value list in written arm order (where2-6.14.1's a=c OR b=c over
	// the nocase column).
	if e.joinArmsSameColumn(arms, rightNames, rightTokens) {
		for _, arm := range arms {
			if !e.joinOrArmIndexable(arm, rightNames, rightTokens, rightReal) {
				return nil
			}
		}
		return arms
	}
	return e.joinArmExecutionOrder(arms, leftNames, rightNames, rightTokens, rightReal)
}

// joinArmsSameColumn reports whether every arm is a single equality (no
// AND spine) anchored on the same column of the operand (C's chngToIN
// precondition).
func (e *SelectEngine) joinArmsSameColumn(arms []sql.Expr, rightNames map[string]bool, rightTokens []string) bool {
	col := ""
	for _, arm := range arms {
		if _, isBop := arm.(*sql.BinaryOp); !isBop {
			return false
		}
		conjuncts := 0
		var anchor string
		e.walkAndSpine(arm, func(conj sql.Expr) {
			conjuncts++
			if bop, ok := conj.(*sql.BinaryOp); ok {
				anchor = e.joinArmOperandColumn(bop, rightNames, rightTokens)
			}
		})
		if conjuncts != 1 || anchor == "" {
			return false
		}
		if col == "" {
			col = anchor
		} else if !strings.EqualFold(col, anchor) {
			return false
		}
	}
	return col != ""
}

// joinArmExecutionOrder orders the arms the way C's OR-loop codegen runs
// them: the terms whose normalized leftCursor is the operand (the operand-
// column-left originals, AND-spine arms, literal-left comparisons) run in
// written order; every two-column arm written with an OUTER column on the
// left becomes a commuted virtual copy — sqlite3WhereExprAnalyze walks the
// clause BACKWARD, so those copies sit after the originals in the REVERSE
// written order, and the originals themselves are skipped at codegen
// (their leftCursor stays on the outer table).
func (e *SelectEngine) joinArmExecutionOrder(arms []sql.Expr, leftNames, rightNames map[string]bool, rightTokens []string, rightReal string) []sql.Expr {
	var origs, copies []sql.Expr
	for _, arm := range arms {
		if !e.joinOrArmIndexable(arm, rightNames, rightTokens, rightReal) {
			return nil // an unrunnable arm voids the whole OR plan
		}
		if e.joinArmIsCopied(arm, leftNames, rightNames, rightTokens) {
			copies = append(copies, arm)
			continue
		}
		origs = append(origs, arm)
	}
	for i := len(copies) - 1; i >= 0; i-- {
		origs = append(origs, copies[i])
	}
	return origs
}

// joinArmIsCopied reports whether C turns the arm into a commuted virtual
// copy: a two-column comparison whose written-left belongs to an outer
// table and whose written-right is a bare operand column.
func (e *SelectEngine) joinArmIsCopied(arm sql.Expr, leftNames, rightNames map[string]bool, rightTokens []string) bool {
	bop, ok := arm.(*sql.BinaryOp)
	if !ok || (bop.Operator != "=" && bop.Operator != "==" && bop.Operator != "IS") {
		return false
	}
	refL, okL := bop.Left.(*sql.ColumnRef)
	refR, okR := bop.Right.(*sql.ColumnRef)
	if !okL || !okR || refL.Name == "*" || refR.Name == "*" {
		return false
	}
	if e.joinRefIsOperand(refL, rightNames, rightTokens) {
		return false // the operand column is written left: the original runs
	}
	leftIsOuter := refL.Table != "" || leftNames[strings.ToLower(refL.Name)]
	return leftIsOuter && e.joinRefIsOperand(refR, rightNames, rightTokens)
}

// joinExprReferencesRight reports whether any column reference of the
// conjunct resolves to the right operand: the OR term belongs to the
// operand's loop level only when the operand's tables appear in it
// (where.c assigns each term to the first loop level that can supply it).
func (e *SelectEngine) joinExprReferencesRight(expr sql.Expr, rightNames map[string]bool, rightTokens []string) bool {
	found := false
	WalkExprFull(expr, func(n sql.Expr) {
		if found {
			return
		}
		ref, isRef := n.(*sql.ColumnRef)
		if !isRef || ref.Name == "*" {
			return
		}
		if ref.Table != "" {
			tok := strings.ToLower(schemaStrippedName(ref.Table))
			if joinTokensContain(rightTokens, tok) {
				found = true
			}
			return
		}
		if rightNames[strings.ToLower(ref.Name)] {
			found = true
		}
	})
	return found
}

// joinExprUsableNow reports whether every column reference of the conjunct
// resolves within the tables positioned at this loop level (the left side
// plus the right operand) — a reference to a later operand delays the term
// past this loop (where.c's prereq masks).
func (e *SelectEngine) joinExprUsableNow(expr sql.Expr, leftNames, rightNames map[string]bool, leftTokens, rightTokens []string) bool {
	usable := true
	WalkExprFull(expr, func(n sql.Expr) {
		if !usable {
			return
		}
		ref, isRef := n.(*sql.ColumnRef)
		if !isRef || ref.Name == "*" {
			return
		}
		if ref.Table != "" {
			tok := strings.ToLower(schemaStrippedName(ref.Table))
			if !joinTokensContain(rightTokens, tok) && !joinTokensContain(leftTokens, tok) {
				usable = false
			}
			return
		}
		name := strings.ToLower(ref.Name)
		if !rightNames[name] && !leftNames[name] {
			usable = false
		}
	})
	return usable
}

// splitOrTopLevel splits an expression's top-level OR spine (parentheses
// peeled) into its disjuncts.
func splitOrTopLevel(expr sql.Expr) []sql.Expr {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		return splitOrTopLevel(v.Expr)
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "OR") {
			return append(splitOrTopLevel(v.Left), splitOrTopLevel(v.Right)...)
		}
	}
	return []sql.Expr{expr}
}

// joinOrArmIndexable reports whether the arm constrains an indexed column of
// the right operand by equality with the operand's column on the written
// LEFT, or as a commutable two-column comparison (outer column left, operand
// column right — C's exprAnalyze inserts a commuted virtual copy whose
// leftCursor is the operand). An arm whose operand column sits on the right
// of a non-column expression never runs in the OR loop (its normalized
// leftCursor stays on the outer table) and voids the plan.
func (e *SelectEngine) joinOrArmIndexable(arm sql.Expr, rightNames map[string]bool, rightTokens []string, rightReal string) bool {
	found := false
	e.walkAndSpine(arm, func(conj sql.Expr) {
		if found {
			return
		}
		bop, ok := conj.(*sql.BinaryOp)
		if !ok {
			return
		}
		if bop.Operator != "=" && bop.Operator != "==" && bop.Operator != "IS" {
			return
		}
		col := e.joinArmOperandColumn(bop, rightNames, rightTokens)
		if col == "" {
			return
		}
		if e.joinColumnHasIndex(rightReal, col) {
			found = true
		}
	})
	return found
}

// joinArmOperandColumn returns the operand column that anchors the arm's
// normalized leftCursor: a bare operand column on the written LEFT, or —
// when the left is an outer table's column — a bare operand column on the
// written RIGHT (the commuted copy's leftCursor). "" when neither holds.
func (e *SelectEngine) joinArmOperandColumn(bop *sql.BinaryOp, rightNames map[string]bool, rightTokens []string) string {
	ref, isRef := bop.Left.(*sql.ColumnRef)
	if !isRef || ref.Name == "*" {
		// A literal or expression on the written left: the ORIGINAL term
		// runs with the right operand's column as its leftCursor (C's
		// exprAnalyze else-branch normalization).
		if ref2, isRef2 := bop.Right.(*sql.ColumnRef); isRef2 && ref2.Name != "*" &&
			e.joinRefIsOperand(ref2, rightNames, rightTokens) {
			return ref2.Name
		}
		return ""
	}
	if e.joinRefIsOperand(ref, rightNames, rightTokens) {
		return ref.Name
	}
	// The written-left is an outer column: the commuted copy runs when the
	// written-right is a bare operand column.
	if ref2, isRef2 := bop.Right.(*sql.ColumnRef); isRef2 && ref2.Name != "*" &&
		e.joinRefIsOperand(ref2, rightNames, rightTokens) {
		return ref2.Name
	}
	return ""
}

// joinRefIsOperand reports whether the reference names a column of the OR
// operand.
func (e *SelectEngine) joinRefIsOperand(ref *sql.ColumnRef, rightNames map[string]bool, rightTokens []string) bool {
	if ref.Table != "" {
		tok := strings.ToLower(schemaStrippedName(ref.Table))
		return joinTokensContain(rightTokens, tok)
	}
	return rightNames[strings.ToLower(ref.Name)]
}

// walkAndSpine visits every AND-spine conjunct (parentheses peeled, nested
// ANDs flattened; other nodes visited whole).
func (e *SelectEngine) walkAndSpine(expr sql.Expr, visit func(sql.Expr)) {
	switch v := expr.(type) {
	case *sql.ParenExpr:
		e.walkAndSpine(v.Expr, visit)
	case *sql.BinaryOp:
		if strings.EqualFold(v.Operator, "AND") {
			e.walkAndSpine(v.Left, visit)
			e.walkAndSpine(v.Right, visit)
			return
		}
	}
	visit(expr)
}

// joinOperandColumnSide returns the column name when one side of the
// equality references a bare column of the right operand.
func (e *SelectEngine) joinOperandColumnSide(bop *sql.BinaryOp, rightNames map[string]bool, rightTokens []string) string {
	for _, side := range []sql.Expr{bop.Left, bop.Right} {
		ref, isRef := side.(*sql.ColumnRef)
		if !isRef || ref.Name == "*" {
			continue
		}
		if ref.Table != "" {
			tok := strings.ToLower(schemaStrippedName(ref.Table))
			if joinTokensContain(rightTokens, tok) {
				return ref.Name
			}
			continue
		}
		if rightNames[strings.ToLower(ref.Name)] {
			return ref.Name
		}
	}
	return ""
}

// joinColumnHasIndex reports whether the column is reachable through an
// index of the table: any secondary index (unique or not) containing it,
// the INTEGER PRIMARY KEY alias, a WITHOUT ROWID PRIMARY KEY column, or a
// rowid pseudo-name.
func (e *SelectEngine) joinColumnHasIndex(realTable, col string) bool {
	if isRowIDName(col) {
		return true
	}
	if ipk := e.ipkAliasColumn(realTable); ipk != "" && strings.EqualFold(ipk, col) {
		return true
	}
	if pks := e.withoutRowidPKCols(realTable); len(pks) > 0 {
		for _, pk := range pks {
			if strings.EqualFold(pk, col) {
				return true
			}
		}
	}
	indexes, err := e.ctx.Schema().FindIndexesForTable(realTable)
	if err != nil {
		return false
	}
	for _, entry := range indexes {
		for _, c := range e.indexColumns(entry.Name) {
			if strings.EqualFold(c, col) {
				return true
			}
		}
	}
	return false
}

// joinOperandRefTokens lists an operand's reference tokens (alias first,
// then the schema-stripped name), lower-cased.
func joinOperandRefTokens(alias, name string) []string {
	var tokens []string
	if alias != "" {
		tokens = append(tokens, strings.ToLower(alias))
	}
	if name != "" {
		tokens = append(tokens, strings.ToLower(schemaStrippedName(name)))
	}
	return tokens
}

// orArmJoinRows runs the OR-branch row source for one outer row: the arms
// evaluated in order over the right rows, each candidate also passing the
// full match predicate, the emitted set deduped by right-row identity (C's
// RowSetTest). Returns whether any row matched.
func (e *SelectEngine) orArmJoinRows(leftMap RowMap, rightMaps []RowMap, arms []sql.Expr, on sql.Expr, combinedMaps *[]RowMap, leftTableName, tableName string) bool {
	matched := false
	seenRowid := make(map[int64]bool, len(rightMaps))
	seenIdx := make(map[int]bool)
	for _, arm := range arms {
		for ri, rightMap := range rightMaps {
			rid, hasRowid := lookupRowidOfMap(rightMap)
			if hasRowid {
				if seenRowid[rid] {
					continue
				}
			} else if seenIdx[ri] {
				continue
			}
			combinedMap := e.buildCombinedRowMap(leftMap, rightMap, tableName, leftTableName)
			if !e.evalOnCondition(arm, combinedMap) {
				continue
			}
			if on != nil && !e.evalOnCondition(on, combinedMap) {
				// The arm accepted but the ON's remaining conjuncts reject:
				// no emission (C's arm loop runs inside the ON filter).
				continue
			}
			if hasRowid {
				seenRowid[rid] = true
			} else {
				seenIdx[ri] = true
			}
			matched = true
			*combinedMaps = append(*combinedMaps, combinedMap)
		}
	}
	return matched
}

// lookupRowidOfMap returns the row's rowid (ok=false when the map carries
// none — a WITHOUT ROWID operand's rows dedup by identity instead).
func lookupRowidOfMap(m RowMap) (int64, bool) {
	v := lookupRowMapValue(m, "rowid")
	if v == nil {
		return 0, false
	}
	rid, isInt := util.UnwrapColumnValue(v).(int64)
	return rid, isInt
}
