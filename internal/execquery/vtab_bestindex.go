package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/sql"
	"github.com/pijalu/frigolite/internal/vtab"
)

// This file ports the virtual-table PLANNING side of SQLite's xBestIndex
// contract (where.c allocateIndexInfo + whereexpr.c isAuxiliaryVtabOperator /
// sqlite3WhereAddLimit) for frigolite's single-table vtab scan. It builds the
// vtab.IndexInfo handed to a module's BestIndexPlan and the parallel WHERE
// conjunct slice the engine uses to recover each constraint's source term
// (HiddenIndexInfo.aRhs / iTermOffset parity).

// BuildVtabIndexInfo ports where.c allocateIndexInfo for frigolite's
// single-table vtab scan: it enumerates the top-level AND conjuncts of
// opts.Where that constrain a column of the vtab named by columnNames
// (index = declared column number), maps operators to vtab.IndexConstraintOp,
// appends LIMIT/OFFSET auxiliary constraints per sqlite3WhereAddLimit, and
// fills the ORDER BY + Distinct + ColUsed outputs.
//
// Returns (ii, conjuncts, err) where conjuncts[c.TermOffset] is the WHERE
// conjunct constraint c came from (LIMIT/OFFSET aux constraints get
// dedicated synthetic entries appended too — the LIMIT/OFFSET expression
// itself, matching VtabConjunctValue; the caller must still check Op before
// treating an entry as a WHERE term). ii is never nil; Usage is zeroed; err
// is non-nil only on internal failures.
func BuildVtabIndexInfo(opts *VtabScanOptions, columnNames []string) (*vtab.IndexInfo, []sql.Expr, error) {
	return BuildVtabIndexInfoWithInstance(opts, "", columnNames, nil)
}

// BuildVtabIndexInfoWithInstance is BuildVtabIndexInfo with the module
// hooks a caller holding the virtual-table instance can supply: tableName
// (the name the statement's column qualifiers use — the FROM alias when one
// was written, else the vtab's table name) gates qualified column
// references, and overloader ports xFindFunction so two-argument function
// calls of the form OVERLOADED(vtab_column, expr) can become auxiliary
// constraints. Both may be zero ("", nil) for the plain enumeration.
func BuildVtabIndexInfoWithInstance(opts *VtabScanOptions, tableName string, columnNames []string, overloader vtab.FunctionOverloader) (*vtab.IndexInfo, []sql.Expr, error) {
	ii := vtab.NewIndexInfo()
	if opts == nil {
		return ii, nil, nil
	}
	fillVtabColsUsed(ii, opts, columnNames)
	ii.Distinct = vtabDistinctHint(opts)
	ii.OrderBy = vtabOrderByTerms(opts, tableName, columnNames)

	cols := newVtabColumnIndex(columnNames)
	conjuncts := vtabWhereConjuncts(opts.Where)
	b := newVtabPlanBuilder(ii, len(conjuncts))
	for _, conjunct := range conjuncts {
		b.beginConjunct()
		classifyVtabConjunct(b, conjunct, tableName, cols, overloader)
		b.endConjunct()
	}
	addVtabLimitConstraints(b, opts, cols, tableName)
	ii.Usage = make([]vtab.ConstraintUsage, len(ii.Constraints)) // zeroed
	return ii, b.conjuncts, nil
}

// vtabPlanBuilder accumulates the xBestIndex constraint rows and the
// parallel conjunct slice while the WHERE clause is classified.
type vtabPlanBuilder struct {
	ii        *vtab.IndexInfo
	conjuncts []sql.Expr
	// allQualify stays true while every enumerated WHERE conjunct produced
	// at least one constraint — sqlite3WhereAddLimit's condition (4) that no
	// WHERE term will be withheld from xBestIndex.
	allQualify bool
	// currentQualifies tracks the conjunct being classified.
	currentQualifies bool
}

// newVtabPlanBuilder pre-sizes the constraint and conjunct slices.
func newVtabPlanBuilder(ii *vtab.IndexInfo, n int) *vtabPlanBuilder {
	ii.Constraints = make([]vtab.IndexConstraint, 0, n)
	return &vtabPlanBuilder{
		ii:         ii,
		conjuncts:  make([]sql.Expr, 0, n),
		allQualify: true,
	}
}

// beginConjunct starts classification of the next WHERE conjunct.
func (b *vtabPlanBuilder) beginConjunct() { b.currentQualifies = false }

// endConjunct closes classification of the current conjunct, updating the
// sqlite3WhereAddLimit condition-(4) flag.
func (b *vtabPlanBuilder) endConjunct() {
	if !b.currentQualifies {
		b.allQualify = false
	}
}

// add records one offered constraint. The conjunct (its value source for
// VtabConjunctValue) is appended to the parallel slice so TermOffset keeps
// the two aligned; LIMIT/OFFSET aux constraints append the LIMIT/OFFSET
// expression itself.
func (b *vtabPlanBuilder) add(conjunct sql.Expr, column int, op vtab.IndexConstraintOp, usable, isIn bool) {
	b.ii.Constraints = append(b.ii.Constraints, vtab.IndexConstraint{
		Column:     column,
		Op:         op,
		Usable:     usable,
		IsIn:       isIn,
		TermOffset: len(b.conjuncts),
	})
	b.conjuncts = append(b.conjuncts, conjunct)
	b.currentQualifies = true
}

// classifyVtabConjunct offers the xBestIndex constraints of one top-level
// WHERE conjunct. Negated forms (NOT IN, NOT BETWEEN, NOT LIKE family) offer
// nothing: SQLite keeps them out of the constraint set (whereexpr.c
// exprAnalyze only adds WO_AUX terms via isAuxiliaryVtabOperator, and WO_NOT
// terms are not indexable).
func classifyVtabConjunct(b *vtabPlanBuilder, conjunct sql.Expr, tableName string, cols vtabColumnIndex, overloader vtab.FunctionOverloader) {
	switch e := conjunct.(type) {
	case *sql.BinaryOp:
		classifyVtabBinaryOp(b, conjunct, e, tableName, cols)
	default:
		classifyVtabOperandConjunct(b, conjunct, e, tableName, cols, overloader)
	}
}

// classifyVtabOperandConjunct classifies the non-binary conjunct forms:
// IS NULL / IS NOT NULL, IS DISTINCT FROM, IN, BETWEEN, and function calls.
func classifyVtabOperandConjunct(b *vtabPlanBuilder, conjunct sql.Expr, e sql.Expr, tableName string, cols vtabColumnIndex, overloader vtab.FunctionOverloader) {
	switch t := e.(type) {
	case *sql.IsNull, *sql.IsNotNull, *sql.IsDistinctFrom, *sql.IsNotDistinctFrom:
		classifyVtabNullForm(b, conjunct, e, tableName, cols)
	case *sql.InList:
		classifyVtabInList(b, conjunct, t, tableName, cols)
	case *sql.Between:
		classifyVtabBetween(b, conjunct, t, tableName, cols)
	case *sql.FuncCall:
		classifyVtabFuncCall(b, conjunct, t, tableName, cols, overloader)
	}
}

// classifyVtabNullForm classifies the IS NULL / IS NOT NULL predicates and
// the IS [NOT] DISTINCT FROM spellings (which carry no value side).
func classifyVtabNullForm(b *vtabPlanBuilder, conjunct sql.Expr, e sql.Expr, tableName string, cols vtabColumnIndex) {
	switch t := e.(type) {
	case *sql.IsNull:
		if col, ok := cols.resolve(t.Operand, tableName); ok {
			b.add(conjunct, col, vtab.IndexConstraintIsNull, true, false)
		}
	case *sql.IsNotNull:
		if col, ok := cols.resolve(t.Operand, tableName); ok {
			b.add(conjunct, col, vtab.IndexConstraintIsNotNull, true, false)
		}
	case *sql.IsDistinctFrom:
		// SQLite parses IS DISTINCT FROM as IS NOT (whereexpr.c TK_ISNOT aux
		// form).
		classifyVtabEitherSide(b, conjunct, t.Left, t.Right, vtab.IndexConstraintIsNot, tableName, cols)
	case *sql.IsNotDistinctFrom:
		classifyVtabEitherSide(b, conjunct, t.Left, t.Right, vtab.IndexConstraintIs, tableName, cols)
	}
}

// classifyVtabInList offers the IN-list constraint: IN maps to EQ with the
// IsIn flag (allocateIndexInfo: op==WO_IN → op = WO_EQ, mIn bit); the engine
// runs one xFilter per element. Negated IN is not offered.
func classifyVtabInList(b *vtabPlanBuilder, conjunct sql.Expr, t *sql.InList, tableName string, cols vtabColumnIndex) {
	if t.Negated {
		return
	}
	if col, ok := cols.resolve(t.Operand, tableName); ok {
		b.add(conjunct, col, vtab.IndexConstraintEq, vtabUsable(t.List...), true)
	}
}

// classifyVtabBetween offers the BETWEEN constraints: BETWEEN splits into
// >= AND <= virtual terms sharing the conjunct (where.c exprAnalyze
// TK_BETWEEN decomposition); NOT BETWEEN is not offered.
func classifyVtabBetween(b *vtabPlanBuilder, conjunct sql.Expr, t *sql.Between, tableName string, cols vtabColumnIndex) {
	if t.Negated {
		return
	}
	if col, ok := cols.resolve(t.Operand, tableName); ok {
		b.add(conjunct, col, vtab.IndexConstraintGe, vtabUsable(t.Low), false)
		b.add(conjunct, col, vtab.IndexConstraintLe, vtabUsable(t.High), false)
	}
}

// classifyVtabBinaryOp maps one binary comparison conjunct. Operators are
// the parser's forms: "=", "<>" (plus the tokenizer aliases "==" and "!="),
// the four range operators, "IS"/"IS NOT", and the infix MATCH/LIKE/GLOB/
// REGEXP family.
func classifyVtabBinaryOp(b *vtabPlanBuilder, conjunct sql.Expr, bo *sql.BinaryOp, tableName string, cols vtabColumnIndex) {
	switch op := strings.ToUpper(bo.Operator); op {
	case "=", "==":
		classifyVtabEitherSide(b, conjunct, bo.Left, bo.Right, vtab.IndexConstraintEq, tableName, cols)
	case "<>", "!=":
		// <> / != become the NE auxiliary constraint (whereexpr.c
		// isAuxiliaryVtabOperator TK_NE path); the value side may be on
		// either side.
		classifyVtabEitherSide(b, conjunct, bo.Left, bo.Right, vtab.IndexConstraintNe, tableName, cols)
	case "<", "<=", ">", ">=":
		classifyVtabRange(b, conjunct, op, bo.Left, bo.Right, tableName, cols)
	case "IS", "IS NOT":
		op2 := vtab.IndexConstraintIs
		if op == "IS NOT" {
			op2 = vtab.IndexConstraintIsNot
		}
		classifyVtabEitherSide(b, conjunct, bo.Left, bo.Right, op2, tableName, cols)
	case "MATCH", "LIKE", "GLOB", "REGEXP":
		// Infix MATCH/LIKE/GLOB/REGEXP: SQLite's parser rewrites these into
		// the two-argument function form op(left, right) (parse.y likeop
		// rule), which attaches on the vtab-column argument with the other
		// operand as the value — so both operand orders offer a constraint.
		// "NOT <op>" (rule 209) never reaches here and is not offered.
		if aux, ok := vtabAuxOpForName(op); ok {
			classifyVtabEitherSide(b, conjunct, bo.Left, bo.Right, aux, tableName, cols)
		}
	}
}

// classifyVtabEitherSide offers one constraint whose column operand may sit
// on either side of the term (whereexpr.c isAuxiliaryVtabOperator's SWAP
// loop): the first operand resolving to a vtab column anchors the
// constraint, the other operand is the value.
func classifyVtabEitherSide(b *vtabPlanBuilder, conjunct sql.Expr, left, right sql.Expr, op vtab.IndexConstraintOp, tableName string, cols vtabColumnIndex) {
	if col, ok := cols.resolve(left, tableName); ok {
		b.add(conjunct, col, op, vtabUsable(right), false)
		return
	}
	if col, ok := cols.resolve(right, tableName); ok {
		b.add(conjunct, col, op, vtabUsable(left), false)
	}
}

// classifyVtabRange offers the four range operators, mirroring the operator
// when the column is the RIGHT operand (5 < col ⇔ col > 5 — where.c
// exprAnalyze commutes the term so the column leads).
func classifyVtabRange(b *vtabPlanBuilder, conjunct sql.Expr, op string, left, right sql.Expr, tableName string, cols vtabColumnIndex) {
	if col, ok := cols.resolve(left, tableName); ok {
		b.add(conjunct, col, vtabRangeOp(op), vtabUsable(right), false)
		return
	}
	if col, ok := cols.resolve(right, tableName); ok {
		b.add(conjunct, col, vtabRangeOp(vtabMirrorRangeOp(op)), vtabUsable(left), false)
	}
}

// classifyVtabFuncCall ports isAuxiliaryVtabOperator's function-call forms
// (whereexpr.c:373): match/glob/like/regexp(expr, vtab_column) attach on the
// SECOND argument with the first as the value; any other two-argument
// function attaches on the FIRST argument when the module's xFindFunction
// (overloader) returns at least vtab.IndexConstraintFunction, with the
// second argument as the value.
func classifyVtabFuncCall(b *vtabPlanBuilder, conjunct sql.Expr, fn *sql.FuncCall, tableName string, cols vtabColumnIndex, overloader vtab.FunctionOverloader) {
	if len(fn.Args) != 2 {
		return
	}
	if col, ok := cols.resolve(fn.Args[1], tableName); ok {
		if aux, isAux := vtabAuxOpForName(fn.Name); isAux {
			b.add(conjunct, col, aux, vtabUsable(fn.Args[0]), false)
			return
		}
	}
	if overloader == nil {
		return
	}
	col, ok := cols.resolve(fn.Args[0], tableName)
	if !ok {
		return
	}
	if code := overloader.FindFunction(fn.Name, 2); code >= int(vtab.IndexConstraintFunction) {
		b.add(conjunct, col, vtab.IndexConstraintOp(code), vtabUsable(fn.Args[1]), false)
	}
}

// addVtabLimitConstraints ports sqlite3WhereAddLimit (whereexpr.c:1652):
// LIMIT/OFFSET are offered as auxiliary constraints (OFFSET first, then
// LIMIT) only when
//
//	(1) the statement has a LIMIT clause, and
//	(2) it is not a GROUP BY / DISTINCT / aggregate query, and
//	(4) every WHERE conjunct qualified as a vtab constraint (SQLite: no
//	    WHERE term will be withheld from xBestIndex), and
//	(5) the ORDER BY clause, if any, is all plain column references of this
//	    vtab (SQLite checks TK_COLUMN strictly — COLLATE-wrapped terms
//	    disqualify here even when allocateIndexInfo accepted them).
//
// Condition (3) — exactly one FROM term holding the vtab — is the caller's
// contract (VtabScanOptions materializes one vtab). Compound members never
// reach this path in frigolite. The aux terms carry Column 0: SQLite inserts
// them with a zeroed WhereTerm (whereClauseInsert's memset), so iColumn is
// 0 and consumers dispatch on Op alone.
func addVtabLimitConstraints(b *vtabPlanBuilder, opts *VtabScanOptions, cols vtabColumnIndex, tableName string) {
	if opts.Limit == nil || opts.GroupBy != nil || opts.Distinct || opts.HasAggregate {
		return
	}
	if !b.allQualify || !vtabPlainOrderBy(opts, cols, tableName) {
		return
	}
	if opts.Offset != nil {
		b.add(opts.Offset, 0, vtab.IndexConstraintOffset, vtabConstIntOk(opts.Offset), false)
	}
	b.add(opts.Limit, 0, vtab.IndexConstraintLimit, vtabConstIntOk(opts.Limit), false)
}

// vtabPlainOrderBy implements sqlite3WhereAddLimit's condition (5): every
// ORDER BY term must be a plain (non-COLLATE-wrapped) column reference of
// this vtab (rowid included); any other expression disqualifies.
func vtabPlainOrderBy(opts *VtabScanOptions, cols vtabColumnIndex, tableName string) bool {
	for _, term := range opts.OrderBy {
		cr, ok := term.Expr.(*sql.ColumnRef)
		if !ok {
			return false
		}
		if _, isCol := cols.resolve(cr, tableName); !isCol {
			return false
		}
	}
	return true
}

// vtabOrderByTerms ports the ORDER BY eligibility loop of where.c
// allocateIndexInfo: constant terms are skipped; a term qualifies when it is
// a (possibly COLLATE-wrapped) plain column reference of this vtab, and a
// COLLATE wrapper must match the column's declared collation (frigolite vtab
// declared columns carry no COLLATE, so BINARY — where.c falls back to
// sqlite3StrBINARY when the column has no declared collation). The first
// non-qualifying term cancels aOrderBy entirely (SQLite breaks the loop); a
// NULLS-ordering override that makes NULL sort largest
// (KEYINFO_ORDER_BIGNULL: NULLS LAST under ASC, NULLS FIRST under DESC)
// does the same, because virtual tables cannot honor it. Returns nil when
// the terms do not qualify.
func vtabOrderByTerms(opts *VtabScanOptions, tableName string, columnNames []string) []vtab.IndexOrderBy {
	if len(opts.OrderBy) == 0 {
		return nil
	}
	cols := newVtabColumnIndex(columnNames)
	var out []vtab.IndexOrderBy
	for _, term := range opts.OrderBy {
		if isVtabConstExpr(term.Expr) {
			continue // constant ORDER BY terms are skipped in both loops
		}
		if vtabBigNullOrdering(term) {
			return nil
		}
		colExpr, collateName, ok := vtabOrderColumn(term.Expr)
		if !ok {
			return nil
		}
		col, ok := cols.resolve(colExpr, tableName)
		if !ok {
			return nil
		}
		if collateName != "" && col >= 0 && !strings.EqualFold(collateName, "BINARY") {
			return nil
		}
		out = append(out, vtab.IndexOrderBy{Column: col, Desc: term.Desc})
	}
	return out
}

// vtabDistinctHint computes the sqlite3_vtab_distinct() hint (where.c
// allocateIndexInfo eDistinct): 2 for a DISTINCT scan, 1 for a GROUP BY
// scan. (SQLite's 2+WHERE_SORTBYGROUP state and the rowidUsed guard are not
// modeled.)
func vtabDistinctHint(opts *VtabScanOptions) int {
	switch {
	case opts.Distinct:
		return 2
	case len(opts.GroupBy) > 0:
		return 1
	}
	return 0
}

// fillVtabColsUsed computes the colUsed bitmask (where.c allocateIndexInfo:
// pIdxInfo->colUsed = pSrc->colUsed) as the OR of opts.ColUsed (when the
// caller computed it directly) and the RefColumnNames ∩ columnNames
// intersection. A rowid reference (rowid/_rowid_/oid) sets every declared
// column bit — a simplification of where.c's set-all-PK-bits rule for
// WITHOUT ROWID tables, mirroring the "rowid use may touch any column"
// contract. RefAllColumns (SELECT * / t.*) sets all bits. Bit 62 represents
// column >=62 (sqlite3 colUsed semantics; its bit 63 is unused).
func fillVtabColsUsed(ii *vtab.IndexInfo, opts *VtabScanOptions, columnNames []string) {
	ii.ColsUsed = opts.ColUsed
	all := opts.RefAllColumns
	for _, name := range opts.RefColumnNames {
		if isRowIDName(strings.ToLower(name)) {
			all = true
			continue
		}
		for i, cn := range columnNames {
			if strings.EqualFold(cn, name) {
				vtabSetColUsedBit(ii, i)
				break
			}
		}
	}
	if all {
		n := len(columnNames)
		if n > 63 {
			n = 63
		}
		for i := 0; i < n; i++ {
			vtabSetColUsedBit(ii, i)
		}
	}
}

// vtabSetColUsedBit sets colUsed bit col, clamping to bit 62.
func vtabSetColUsedBit(ii *vtab.IndexInfo, col int) {
	if col > 62 {
		col = 62
	}
	ii.ColsUsed |= uint64(1) << uint(col)
}
