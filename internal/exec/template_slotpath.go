package exec

import (
	"github.com/pijalu/frigolite/internal/sql"
)

// Precomputed literal-slot paths for template-cache entries (the slot-path
// substitution fast path).
//
// A template-cache hit re-walked and re-cloned the whole template AST through
// the copy-on-write walker on EVERY statement (updateCOW/binaryOp/exprClone
// dominated the point-UPDATE/DELETE allocation profile: the statement structs
// and the dispatch walk repeated per statement even though only one or two
// literal leaves change). The slot-path form instead records, at template
// store time, the POINTER PATH from the statement root to every literal slot
// a hit will substitute, in exactly the COW walker's value-consumption order.
// A hit then validates the values against the precomputed slot classes and
// rewrites only the literal leaves of the entry's live per-depth clone —
// no walk, no allocation.
//
// Correctness contract (mirrors template_clone.go):
//   - The clone stays private to the statement. The TEMPLATE AST is never
//     touched: every mutable node on a slot path is one the COW walker copies
//     (literal leaves and their ancestor chain), so in-place rewriting only
//     ever writes nodes this engine built for the live clone. The live clone
//     per (entry, execDepth) is overwritten only when the previous consumer
//     at that depth finished — the same per-depth sequentiality the
//     clone_scratch rotation relies on (clone_scratch.go).
//   - A substituted AST is byte-equivalent to a fresh parse of the statement
//     text. Value-kind gates mirror numeric()/anyLiteral() (and insertValue()
//     for the INSERT family) exactly; a refused value falls back to the COW
//     path, which refuses identically and falls through to a full parse.
//   - Retained Prepare never takes this path (scratchOK=false keeps the
//     fresh-clone COW form): a retained AST must not alias the live clone a
//     later hit would rewrite (FIX.PREPARE-ALIAS).
//
// templates the walker refuses (unknown statement/expression kinds, blob and
// RAISE literals, dual-slot LIMIT/OFFSET shapes, INSERT tuples with literals
// nested inside non-substituted expressions) get slots==nil and take the
// unchanged COW path — behaviorally identical to today.

// slotClass is one literal slot's value-kind gate (mirroring the COW
// walkers' per-slot acceptance).
type slotClass uint8

const (
	// slotInt is a plain decimal-integer NumericLit slot: accepts int64
	// (canonical rendering) and string (quoted spelling serves any slot);
	// a float64 refuses (numeric()'s isDecimalSlot gate).
	slotInt slotClass = iota
	// slotReal is a decimal REAL NumericLit slot: accepts float64 (finite,
	// not the shape-ambiguous 2^63 double) and string; an int64 refuses.
	slotReal
	// slotStr is a StringLit slot: accepts string and int64 (the fresh node
	// kind follows the value); a float64 refuses (anyLiteral's gate).
	slotStr
	// slotInsAny is an INSERT VALUES tuple slot: the INSERT walker's
	// insertValue accepts int64, float64 (canonical 'g' rendering, REAL kind
	// preserved) and string regardless of the slot's parsed kind.
	slotInsAny
)

// templateSlots is the precomputed slot table of one template entry.
type templateSlots struct {
	paths   []slotPath  // one per literal slot, in value-consumption order
	classes []slotClass // parallel gate per slot
}

// slotPath addresses one literal slot: the root statement index plus the
// field-selector chain from the root to the literal's parent field.
type slotPath struct {
	root  int
	steps []slotStep
}

// slotStep is one field selector. field names the field; idx indexes slices.
// The final step of a path selects the literal's own field (the apply writes
// through it).
type slotStep struct {
	field slotField
	idx   int
}

// slotField enumerates every field a slot path may select. Statement-level
// selectors route into the statement families; expr-level selectors descend
// expression trees; the terminal selector is whichever field holds the
// literal.
type slotField uint8

const (
	// SELECT statement fields (source-order walk: WITH, list, FROM, JOINs,
	// WHERE, GROUP BY, HAVING, WINDOW, ORDER BY, LIMIT, OFFSET, compound).
	sfSelCTE     slotField = iota // CTEs[i].Select (select frame)
	sfSelCol                      // Columns[i].Expr
	sfSelFromSub                  // From.Subquery (select frame)
	sfSelFromArg                  // From.Args[i]
	sfSelJoinOn                   // Joins[i].On
	sfSelJoinSub                  // Joins[i].Table.Subquery (select frame)
	sfSelJoinArg                  // Joins[i].Table.Args[i]
	sfSelWhere
	sfSelGroup // GroupBy[i]
	sfSelHaving
	sfSelWinPart // Windows[i].Partitions[idx]
	sfSelWinOrd  // Windows[i].OrderBy[idx].Expr
	sfSelOrder   // OrderBy[i].Expr
	sfSelLimit
	sfSelOffset
	sfSelUnion // Union (select frame)

	// UPDATE statement fields.
	sfUpdAssign  // Assignments[i].Value
	sfUpdFromSub // From.Subquery (select frame)
	sfUpdFromArg // From.Args[i]
	sfUpdFJOn    // FromJoins[i].On
	sfUpdFJSub   // FromJoins[i].Table.Subquery (select frame)
	sfUpdFJArg   // FromJoins[i].Table.Args[idx]
	sfUpdWhere
	sfUpdOrder // OrderBy[i].Expr
	sfUpdLimit
	sfUpdOffset
	sfUpdReturning // Returning.Expr

	// DELETE statement fields.
	sfDelWhere
	sfDelOrder // OrderBy[i].Expr
	sfDelLimit
	sfDelOffset
	sfDelReturning // Returning.Expr

	// INSERT statement fields (the INSERT walker's consumption contract).
	sfInsTuple // Values[idx1][idx2] (idx = tuple*64+item encoding is NOT
	// used; see sfInsTuplePair)
	sfInsSelect   // Select (select frame)
	sfInsConflict // OnConflict (conflict frame)
	sfOCTgtWhere  // OnConflict.TargetWhere
	sfOCAssign    // OnConflict.Assignments[i].Value
	sfOCWhere     // OnConflict.Where
	sfOCNext      // OnConflict.Next (conflict frame)

	// Expression fields.
	sfBinLeft
	sfBinRight
	sfUnary
	sfParen
	sfBetweenOp
	sfBetweenLow
	sfBetweenHigh
	sfInOperand
	sfInItem // InList.List[idx]
	sfRowVal // RowValue.Values[idx]
	sfCast
	sfIsNull
	sfIsNotNull
	sfDistL
	sfDistR
	sfNotDistL
	sfNotDistR
	sfTrue
	sfFalse
	sfFuncArg // FuncCall.Args[idx]
	sfFuncFilter
	sfFuncOrd // FuncCall.OrderBy[idx].Expr
	sfCaseOp
	sfCaseWhenWhen // Whens[idx].When
	sfCaseWhenThen // Whens[idx].Then
	sfCaseElse
	sfSubq   // Subquery.Select (select frame)
	sfExists // ExistsExpr.Select (select frame)
)

// sfInsTuplePair packs a tuple index and an item index into one step idx
// (tuples are [][]Expr; a single int field cannot address both levels).
// Tuple counts beyond 4096 rows or 64 items per tuple refuse the template
// (unreachable for real statements; the COW path serves them).
const (
	insTupleItemBits  = 6
	insTupleItemMask  = 1<<insTupleItemBits - 1
	insTupleMaxItems  = 1 << insTupleItemBits
	insTupleMaxTuples = 1 << (16 - insTupleItemBits)
)

func insTupleIdx(tuple, item int) int { return tuple<<insTupleItemBits | item }
func insTupleOf(idx int) int          { return idx >> insTupleItemBits }
func insItemOf(idx int) int           { return idx & insTupleItemMask }

// collectTemplateSlots precomputes the slot table for a parsed statement
// list (the template entry's AST). It returns nil when any slot, shape or
// literal placement makes the entry ineligible for the slot-path form — the
// entry then serves hits through the unchanged COW walker.
func collectTemplateSlots(stmts []sql.Stmt) *templateSlots {
	// Only single-statement templates take the live-clone form: a batch of
	// two same-template statements substitutes both clones up front (before
	// the first executes) and they must coexist.
	if len(stmts) != 1 {
		return nil
	}
	sc := &slotCollector{ok: true}
	sc.collectStmt(0, stmts[0])
	if !sc.ok || len(sc.paths) == 0 {
		return nil
	}
	return &templateSlots{paths: sc.paths, classes: sc.classes}
}

// slotCollector accumulates slot paths while mirroring the COW walkers'
// field order. ok=false aborts collection (the entry stays COW-only).
type slotCollector struct {
	paths   []slotPath
	classes []slotClass
	steps   []slotStep
	curRoot int
	ok      bool
}

// slot records the current path as one literal slot of the given class.
func (sc *slotCollector) slot(class slotClass) {
	p := slotPath{root: sc.curRoot, steps: make([]slotStep, len(sc.steps))}
	copy(p.steps, sc.steps)
	sc.paths = append(sc.paths, p)
	sc.classes = append(sc.classes, class)
}

// collectStmt dispatches one statement family. root is the template AST
// index; the collector keeps it alongside the step stack.
func (sc *slotCollector) collectStmt(root int, stmt sql.Stmt) {
	sc.curRoot = root
	switch s := stmt.(type) {
	case *sql.InsertStmt:
		sc.collectInsert(s)
	case *sql.SelectStmt:
		sc.collectSelect(s)
	case *sql.UpdateStmt:
		sc.collectUpdate(s)
	case *sql.DeleteStmt:
		sc.collectDelete(s)
	default:
		sc.ok = false
	}
}

// collectSelect walks a SELECT in the COW walker's source order (WITH,
// select list, FROM, JOINs, WHERE, GROUP BY, HAVING, WINDOW, ORDER BY,
// LIMIT, OFFSET, compound tail).
func (sc *slotCollector) collectSelect(sel *sql.SelectStmt) {
	if sel == nil {
		return
	}
	for i := range sel.CTEs {
		sc.pushPop(sfSelCTE, i, func() { sc.collectSelect(sel.CTEs[i].Select) })
	}
	for i := range sel.Columns {
		sc.pushPopExpr(sfSelCol, i, sel.Columns[i].Expr)
	}
	if sel.From.Subquery != nil {
		sc.pushPop(sfSelFromSub, 0, func() { sc.collectSelect(sel.From.Subquery) })
	}
	for i := range sel.From.Args {
		sc.pushPopExpr(sfSelFromArg, i, sel.From.Args[i])
	}
	for i := range sel.Joins {
		j := &sel.Joins[i]
		sc.pushPopExpr(sfSelJoinOn, i, j.On)
		if j.Table.Subquery != nil {
			sc.pushPop(sfSelJoinSub, i, func() { sc.collectSelect(j.Table.Subquery) })
		}
		for k := range j.Table.Args {
			sc.pushPopExpr(sfSelJoinArg, joinArgIdx(i, k), j.Table.Args[k])
		}
	}
	sc.exprField(sfSelWhere, sel.Where)
	for i := range sel.GroupBy {
		sc.pushPopExpr(sfSelGroup, i, sel.GroupBy[i])
	}
	sc.exprField(sfSelHaving, sel.Having)
	for i := range sel.Windows {
		w := &sel.Windows[i]
		for k := range w.Partitions {
			sc.pushPopExpr(sfSelWinPart, joinArgIdx(i, k), w.Partitions[k])
		}
		for k := range w.OrderBy {
			sc.pushPopExpr(sfSelWinOrd, joinArgIdx(i, k), w.OrderBy[k].Expr)
		}
	}
	for i := range sel.OrderBy {
		sc.pushPopExpr(sfSelOrder, i, sel.OrderBy[i].Expr)
	}
	sc.exprField(sfSelLimit, sel.Limit)
	sc.exprField(sfSelOffset, sel.Offset)
	// Dual-slot LIMIT/OFFSET: the COW walk declines both slots present
	// (cross-assignment hazard); the template stays COW-only.
	if sel.Limit != nil && sel.Offset != nil {
		sc.ok = false
		return
	}
	if sel.Union != nil {
		sc.pushPop(sfSelUnion, 0, func() { sc.collectSelect(sel.Union) })
	}
}

// collectUpdate walks an UPDATE in source order (WITH, SET, FROM, FROM
// joins, WHERE, ORDER BY, LIMIT, OFFSET, RETURNING).
func (sc *slotCollector) collectUpdate(s *sql.UpdateStmt) {
	for i := range s.CTEs {
		sc.pushPop(sfSelCTE, i, func() { sc.collectSelect(s.CTEs[i].Select) })
	}
	for i := range s.Assignments {
		sc.pushPopExpr(sfUpdAssign, i, s.Assignments[i].Value)
	}
	if s.From.Subquery != nil {
		sc.pushPop(sfUpdFromSub, 0, func() { sc.collectSelect(s.From.Subquery) })
	}
	for i := range s.From.Args {
		sc.pushPopExpr(sfUpdFromArg, i, s.From.Args[i])
	}
	for i := range s.FromJoins {
		j := &s.FromJoins[i]
		sc.pushPopExpr(sfUpdFJOn, i, j.On)
		if j.Table.Subquery != nil {
			sc.pushPop(sfUpdFJSub, i, func() { sc.collectSelect(j.Table.Subquery) })
		}
		for k := range j.Table.Args {
			sc.pushPopExpr(sfUpdFJArg, joinArgIdx(i, k), j.Table.Args[k])
		}
	}
	sc.exprField(sfUpdWhere, s.Where)
	for i := range s.OrderBy {
		sc.pushPopExpr(sfUpdOrder, i, s.OrderBy[i].Expr)
	}
	sc.exprField(sfUpdLimit, s.Limit)
	sc.exprField(sfUpdOffset, s.Offset)
	if s.Limit != nil && s.Offset != nil {
		sc.ok = false
		return
	}
	sc.exprField(sfUpdReturning, s.Returning.Expr)
}

// collectDelete walks a DELETE in source order (WITH, WHERE, ORDER BY,
// LIMIT, OFFSET, RETURNING).
func (sc *slotCollector) collectDelete(s *sql.DeleteStmt) {
	for i := range s.CTEs {
		sc.pushPop(sfSelCTE, i, func() { sc.collectSelect(s.CTEs[i].Select) })
	}
	sc.exprField(sfDelWhere, s.Where)
	for i := range s.OrderBy {
		sc.pushPopExpr(sfDelOrder, i, s.OrderBy[i].Expr)
	}
	sc.exprField(sfDelLimit, s.Limit)
	sc.exprField(sfDelOffset, s.Offset)
	if s.Limit != nil && s.Offset != nil {
		sc.ok = false
		return
	}
	sc.exprField(sfDelReturning, s.Returning.Expr)
}

// collectInsert walks an INSERT under the INSERT walker's consumption
// contract (insertStmtValues/insertValue): WITH bodies and the INSERT
// SELECT consume like a SELECT; a VALUES tuple consumes ONLY a direct
// NumericLit/StringLit — any literal nested inside a non-substituted tuple
// expression makes the normalized value count diverge (the COW walk then
// refuses every hit), so such templates stay COW-only.
func (sc *slotCollector) collectInsert(s *sql.InsertStmt) {
	for i := range s.CTEs {
		sc.pushPop(sfSelCTE, i, func() { sc.collectSelect(s.CTEs[i].Select) })
	}
	for ti, tuple := range s.Values {
		if len(tuple) >= insTupleMaxItems || len(s.Values) >= insTupleMaxTuples {
			sc.ok = false
			return
		}
		for vi, expr := range tuple {
			switch expr.(type) {
			case *sql.NumericLit, *sql.StringLit:
				sc.slotTuple(ti, vi, slotInsAny)
			default:
				if exprHasLiteral(expr) {
					sc.ok = false
					return
				}
			}
		}
	}
	if s.Select != nil {
		sc.pushPop(sfInsSelect, 0, func() { sc.collectSelect(s.Select) })
	}
	if s.OnConflict != nil {
		sc.pushPop(sfInsConflict, 0, func() { sc.collectConflict(s.OnConflict) })
	}
	if s.HasReturning && exprHasLiteral(s.Returning.Expr) {
		// The INSERT walker never substitutes RETURNING expressions, but the
		// normalizer extracted their literals: the value counts diverge and
		// the COW walk refuses — keep the template COW-only.
		sc.ok = false
		return
	}
}

// collectConflict walks an upsert ON CONFLICT chain in the COW walker's
// conflictClause order (target WHERE, DO UPDATE assignments, WHERE, next
// chained clause).
func (sc *slotCollector) collectConflict(oc *sql.OnConflictClause) {
	if oc == nil {
		return
	}
	sc.exprField(sfOCTgtWhere, oc.TargetWhere)
	for i := range oc.Assignments {
		sc.pushPopExpr(sfOCAssign, i, oc.Assignments[i].Value)
	}
	sc.exprField(sfOCWhere, oc.Where)
	if oc.Next != nil {
		sc.pushPop(sfOCNext, 0, func() { sc.collectConflict(oc.Next) })
	}
}

// slotTuple records a VALUES tuple slot (two-level index).
func (sc *slotCollector) slotTuple(tuple, item int, class slotClass) {
	sc.steps = append(sc.steps, slotStep{field: sfInsTuple, idx: insTupleIdx(tuple, item)})
	sc.slot(class)
	sc.steps = sc.steps[:len(sc.steps)-1]
}

// pushPop descends into a non-expression child (a nested SELECT frame),
// running body with the selector pushed.
func (sc *slotCollector) pushPop(f slotField, idx int, body func()) {
	sc.steps = append(sc.steps, slotStep{field: f, idx: idx})
	if body != nil {
		body()
	}
	sc.steps = sc.steps[:len(sc.steps)-1]
}

// pushPopExpr descends into an expression child: exprField with a pushed
// selector (the expression may itself consume slots deeper down).
func (sc *slotCollector) pushPopExpr(f slotField, idx int, e sql.Expr) {
	sc.steps = append(sc.steps, slotStep{field: f, idx: idx})
	sc.expr(e)
	sc.steps = sc.steps[:len(sc.steps)-1]
}

// exprField walks an optional expression field, with the field selector
// pushed so deeper slots record the full path.
func (sc *slotCollector) exprField(f slotField, e sql.Expr) {
	if e == nil {
		return
	}
	sc.steps = append(sc.steps, slotStep{field: f})
	sc.expr(e)
	sc.steps = sc.steps[:len(sc.steps)-1]
}

// expr mirrors the COW walker's expression dispatch: literal slots are
// recorded with their gate class, everything else descends in field order.
// Unknown kinds, blob literals and RAISE abort collection (ok=false).
func (sc *slotCollector) expr(e sql.Expr) {
	switch v := e.(type) {
	case nil:
		sc.ok = false
	case *sql.NumericLit:
		switch {
		case isDecimalSlot(v.Value):
			sc.slot(slotInt)
		case isRealSlot(v.Value):
			sc.slot(slotReal)
		default:
			// Folded unary-minus and hex slots refuse every value (the COW
			// numeric() gate) — the template stays COW-only.
			sc.ok = false
		}
	case *sql.StringLit:
		sc.slot(slotStr)
	case *sql.NullLit, *sql.ColumnRef, *sql.ParameterExpr:
	case *sql.FuncCall:
		for i := range v.Args {
			sc.pushPopExpr(sfFuncArg, i, v.Args[i])
		}
		sc.exprField(sfFuncFilter, v.Filter)
		for i := range v.OrderBy {
			sc.pushPopExpr(sfFuncOrd, i, v.OrderBy[i].Expr)
		}
	case *sql.CaseExpr:
		sc.exprField(sfCaseOp, v.Operand)
		for i := range v.Whens {
			sc.pushPopExpr(sfCaseWhenWhen, i, v.Whens[i].When)
			sc.pushPopExpr(sfCaseWhenThen, i, v.Whens[i].Then)
		}
		sc.exprField(sfCaseElse, v.Else)
	case *sql.BinaryOp:
		sc.pushPopExpr(sfBinLeft, 0, v.Left)
		sc.pushPopExpr(sfBinRight, 0, v.Right)
	case *sql.UnaryOp:
		sc.pushPopExpr(sfUnary, 0, v.Operand)
	case *sql.ParenExpr:
		sc.pushPopExpr(sfParen, 0, v.Expr)
	case *sql.Between:
		sc.pushPopExpr(sfBetweenOp, 0, v.Operand)
		sc.pushPopExpr(sfBetweenLow, 0, v.Low)
		sc.pushPopExpr(sfBetweenHigh, 0, v.High)
	case *sql.InList:
		sc.pushPopExpr(sfInOperand, 0, v.Operand)
		for i := range v.List {
			sc.pushPopExpr(sfInItem, i, v.List[i])
		}
	case *sql.RowValue:
		for i := range v.Values {
			sc.pushPopExpr(sfRowVal, i, v.Values[i])
		}
	case *sql.CastExpr:
		sc.pushPopExpr(sfCast, 0, v.Operand)
	case *sql.IsNull:
		sc.pushPopExpr(sfIsNull, 0, v.Operand)
	case *sql.IsNotNull:
		sc.pushPopExpr(sfIsNotNull, 0, v.Operand)
	case *sql.IsDistinctFrom:
		sc.pushPopExpr(sfDistL, 0, v.Left)
		sc.pushPopExpr(sfDistR, 0, v.Right)
	case *sql.IsNotDistinctFrom:
		sc.pushPopExpr(sfNotDistL, 0, v.Left)
		sc.pushPopExpr(sfNotDistR, 0, v.Right)
	case *sql.IsTrue:
		sc.pushPopExpr(sfTrue, 0, v.Operand)
	case *sql.IsFalse:
		sc.pushPopExpr(sfFalse, 0, v.Operand)
	case *sql.Subquery:
		sc.pushPop(sfSubq, 0, func() { sc.collectSelect(v.Select) })
	case *sql.ExistsExpr:
		sc.pushPop(sfExists, 0, func() { sc.collectSelect(v.Select) })
	case *sql.BlobLit, *sql.RaiseExpr:
		sc.ok = false
	default:
		sc.ok = false
	}
}
