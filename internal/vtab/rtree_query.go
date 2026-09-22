package vtab

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/value"
)

// ---- constraint pushdown (xBestIndex/xFilter analogue) ----
//
// The engine inspects the WHERE clause of a query against an rtree virtual
// table and hands every conjunct bound to one of its columns (or its rowid
// column) to the instance below, REMOVING the conjunct from the residual SQL
// the core re-checks afterwards (SQLite sets argvConsumed/omit for exactly
// these). Evaluation happens here with the r-tree's numeric semantics — never
// through generic SQL type affinity — which is observable behavior sqlite3
// relies on (rtree1-18.0: `c1 > '-1'` is a float compare, not an affinity
// cast, so the row IS returned).

// rtreeConstraint is one pushed-down predicate on column idx (declared-column
// numbering, 0 = the rowid/id column) compared against a constant value.
type rtreeConstraint[T coordType] struct {
	col   int
	op    string // "=", "<", "<=", ">", ">=", "<>"
	value interface{}
}

// rtreeRowidSet is a pushed `id IN (...)` membership restriction; nil when no
// such restriction was pushed.
type rtreeRowidSet map[int64]bool

// AddCoordConstraint registers one coordinate/rowid constraint (engine side).
// Constraints accumulate until the next Open().
// ConstraintSink is the engine-side pushdown target implemented by rtree
// instances: conjuncts on declared columns (or id-membership) are evaluated
// inside the r-tree walk and omitted from residual SQL re-checks.
type ConstraintSink interface {
	PushRTreeConstraint(col int, op string, value interface{})
	PushRTreeRowids(ids []int64)
}

// RtreeMatchSink receives `col MATCH <geometry>` conjuncts (SQLite hands the
// MATCH constraint value to xFilter). Values other than *RtreeGeometry mark
// the scan invalid so reading it fails with "SQL logic error" like SQLite's
// xGeom SQLITE_ERROR path (rtree8-3.1 text literal, rtree9-4.x blobs).
type RtreeMatchSink interface {
	PushRTreeMatch(value interface{})
}

// PushRTreeConstraint implements constraintSink.
func (v *rtreeVTab[T]) PushRTreeConstraint(col int, op string, value interface{}) {
	v.pending = append(v.pending, rtreeConstraint[T]{col: col, op: op, value: value})
}

// PushRTreeRowids implements constraintSink.
func (v *rtreeVTab[T]) PushRTreeRowids(ids []int64) {
	if v.pendingRowids == nil {
		v.pendingRowids = rtreeRowidSet{}
	}
	for _, id := range ids {
		v.pendingRowids[id] = true
	}
}

// PushRTreeMatch implements RtreeMatchSink: only geometry markers drive the
// callback walk; anything else invalidates the scan (SQLITE_ERROR parity).
func (v *rtreeVTab[T]) PushRTreeMatch(value interface{}) {
	if m, ok := value.(*RtreeGeometry); ok {
		if v.pendingMatch == nil {
			v.pendingMatch = m
		}
		return
	}
	if v.matchErr == nil {
		v.matchErr = fmt.Errorf("SQL logic error")
	}
}

// resetPending takes ownership of the accumulated constraints for this scan.
func (v *rtreeVTab[T]) resetPending() ([]rtreeConstraint[T], rtreeRowidSet, *RtreeGeometry, error) {
	c := v.pending
	r := v.pendingRowids
	m := v.pendingMatch
	e := v.matchErr
	v.pending = nil
	v.pendingRowids = nil
	v.pendingMatch = nil
	v.matchErr = nil
	return c, r, m, e
}

// ---- coordinate comparison ----

// coordPasses reports whether the constraint holds for coordinate cell c
// against the pushed literal. Both sides go through SQLite's mixed numeric
// ordering (value.CompareValues) AFTER the column's REAL/INTEGER affinity is
// applied to the literal — matching rtree.c's xFilter evaluation domain. The
// constraint is first reduced through rtreeConstraintOp (NULL / non-numeric
// operand rewrites).
func coordPasses[T coordType](op string, c T, value interface{}) bool {
	op, value, always, never := rtreeConstraintOp(op, value)
	if always {
		return true
	}
	if never {
		return false
	}
	lhs := coordToOut[T](c)
	rhs := applyColumnAffinity(value, isInt32Coord[T]())
	return numCompare(op, lhs, rhs)
}

// rtreeConstraintOp reduces one pushed constraint to its xFilter evaluation
// form (rtree.c xFilter: eType = sqlite3_value_numeric_type(argv[ii]); a NULL
// operand rewrites the operator to RTREE_FALSE for every comparison, while a
// non-numeric text/blob operand rewrites it to RTREE_TRUE for < / <= — any
// number sorts before any text — and RTREE_FALSE for every other operator).
// Returns (op, value, alwaysTrue, alwaysFalse); the value is kept verbatim
// for numeric text so the column-affinity coercion below sees it.
func rtreeConstraintOp(op string, value interface{}) (string, interface{}, bool, bool) {
	switch v := value.(type) {
	case nil:
		return "", nil, false, true
	case string:
		if !rtreeWellFormedNumber(v) {
			return opIfRange(op)
		}
	case []byte:
		return opIfRange(op)
	}
	return op, value, false, false
}

// opIfRange renders the RTREE_TRUE / RTREE_FALSE rewrite for a non-numeric
// comparison operand: < / <= hold for every row, all other operators for none.
func opIfRange(op string) (string, interface{}, bool, bool) {
	if op == "<" || op == "<=" {
		return "", nil, true, false
	}
	return "", nil, false, true
}

// rtreeWellFormedNumber reports whether s is a well-formed integer or real
// literal (sqlite3_value_numeric_type converts exactly these to numeric).
func rtreeWellFormedNumber(s string) bool {
	_, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil
}

// isInt32Coord reports whether T is the int32 coordinate flavor.
func isInt32Coord[T coordType]() bool {
	var z T
	_, ok := any(z).(int32)
	return ok
}

// applyColumnAffinity converts a pushed literal into the comparison domain of
// a REAL or INTEGER column (SQLite applies column affinity to the other
// operand before comparing). int32 marks INTEGER-affinity coordinates.
func applyColumnAffinity(value interface{}, integerCol bool) interface{} {
	s, isText := value.(string)
	if !isText {
		return value
	}
	f := rtreeNumericPrefix(s)
	if integerCol {
		return int64(f)
	}
	return f // REAL affinity: text becomes float
}

// numCompare applies op to two SQL values using the engine's canonical
// numeric ordering; unknown ops pass everything.
func numCompare(op string, lhs, rhs interface{}) bool {
	c := value.CompareValues(lhs, rhs)
	switch op {
	case "=":
		return c == 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	case "<>", "!=":
		return c != 0
	}
	return true
}

// rowidPasses applies the op to an entry id and a pushed constant using the
// same affinity+ordering rules as coordinate constraints (id column is
// INTEGER-affinity). The NULL / non-numeric operand rewrite of
// rtreeConstraintOp applies to the id column identically (rtree.c runs every
// xFilter argv through the same classification).
func rowidPasses(op string, id int64, value interface{}) bool {
	op, value, always, never := rtreeConstraintOp(op, value)
	res := numCompare(op, id, applyColumnAffinity(value, true))
	if always {
		res = true
	}
	if never {
		res = false
	}
	return res
}

// filterAuxConstraints re-checks constraints pushed on AUXILIARY columns
// (declared index > nDim2). The core hands them to the sink and omits them
// from the residual WHERE like every rtree constraint, but an aux column has
// no coordinates — the comparison must run against the %_rowid value the scan
// attached. Aux columns are declared without a type, so no affinity is
// applied (raw comparison, SQL NULL never satisfies a comparison).
func (v *rtreeVTab[T]) filterAuxConstraints(rows [][]interface{}, constraints []rtreeConstraint[T]) [][]interface{} {
	var aux []rtreeConstraint[T]
	for _, con := range constraints {
		if con.col > v.nDim2 {
			aux = append(aux, con)
		}
	}
	if len(aux) == 0 {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if v.rowPassesAux(row, aux) {
			out = append(out, row)
		}
	}
	return out
}

// rowPassesAux reports whether one row's attached aux values satisfy every
// aux-column constraint.
func (v *rtreeVTab[T]) rowPassesAux(row []interface{}, aux []rtreeConstraint[T]) bool {
	for _, con := range aux {
		cell := 1 + v.nDim2 + (con.col - v.nDim2 - 1)
		var val interface{}
		if cell < len(row) {
			val = row[cell]
		}
		if val == nil {
			return false
		}
		if !numCompare(con.op, val, con.value) {
			return false
		}
	}
	return true
}

// ---- filtered scan (priority-ordered MBR descent) ----

// rtreeScan carries the filtered descent's shared state (rtreeGeopolyOverlap's
// filter walk): the pushed constraints, the rowid filter, the MATCH geometry
// callback and the output rows.
type rtreeScan[T coordType] struct {
	v           *rtreeVTab[T]
	constraints []rtreeConstraint[T]
	rowids      rtreeRowidSet
	match       *RtreeGeometry
	out         [][]interface{}
}

// cellMatches runs the geometry callback against one cell's MBR
// (rtreeCallbackConstraint: nCoord = 2*nDim REAL-domain values).
func (s *rtreeScan[T]) cellMatches(coords []T) (bool, error) {
	fl := make([]float64, len(coords))
	for i, c := range coords {
		fl[i] = asFloat64(c)
	}
	res, err := s.match.Invoke(len(fl), fl)
	if err != nil {
		return false, err
	}
	return res != 0, nil
}

// rowidPassesAll applies every rowid-column constraint to id.
func (s *rtreeScan[T]) rowidPassesAll(id int64) bool {
	for _, con := range s.constraints {
		if con.col == 0 && !rowidPasses(con.op, id, con.value) {
			return false
		}
	}
	return true
}

// coordPassesAll applies every coordinate constraint to the cell's MBR
// (a coordinate-column constraint indexes aCoord[col-1]).
func (s *rtreeScan[T]) coordPassesAll(cell RtreeCell[T]) bool {
	for _, con := range s.constraints {
		if con.col == 0 {
			continue
		}
		ci := con.col - 1
		if ci >= len(cell.aCoord) {
			continue
		}
		if !coordPasses(con.op, cell.aCoord[ci], con.value) {
			return false
		}
	}
	return true
}

// collectLeafRow appends the output row for one matching leaf cell.
func (s *rtreeScan[T]) collectLeafRow(cell RtreeCell[T]) {
	row := make([]interface{}, 0, 1+s.v.nDim2+s.v.nAux)
	row = append(row, cell.iRowid)
	for j := 0; j < s.v.nDim2; j++ {
		row = append(row, coordToOut[T](cell.aCoord[j]))
	}
	s.out = append(s.out, row)
}

// collectDataRows walks the tree depth-first honoring the pushed
// constraints. Internal cells prune only when the subtree's bounding box
// cannot satisfy ANY dimension constraint — a plain MBR rejection test — so
// results stay identical to unfiltered enumeration even under mixed operators.
// match (when non-nil) is a MATCH geometry callback: rtree.c invokes it for
// every cell at BOTH levels with that cell's coordinates, pruning it when the
// callback reports zero.
func (v *rtreeVTab[T]) collectDataRows(constraints []rtreeConstraint[T], rowids rtreeRowidSet, match *RtreeGeometry) ([][]interface{}, error) {
	s := &rtreeScan[T]{v: v, constraints: constraints, rowids: rowids, match: match}
	root, err := v.rootAcquire()
	if err != nil {
		return nil, err
	}
	defer v.nodeRelease(root)
	if err := s.descend(1, root.depth()); err != nil {
		return nil, err
	}
	return s.out, nil
}

// descend walks one node: leaf cells run the rowid/MATCH/constraint filters;
// internal cells run the MATCH MBR prune and recurse.
func (s *rtreeScan[T]) descend(nodeno int64, depthLeft int) error {
	v := s.v
	node, err := v.nodeAcquire(nodeno)
	if err != nil {
		return err
	}
	defer v.nodeRelease(node)
	nCell := node.nCell()
	if depthLeft == 0 {
		return s.descendLeaf(node, nCell)
	}
	for i := 0; i < nCell; i++ {
		childRowid := v.nodeGetRowid(node, i)
		if s.match != nil {
			mbr := make([]T, v.nDim2)
			copy(mbr, v.nodeGetCell(node, i).aCoord)
			okm, merr := s.cellMatches(mbr)
			if merr != nil {
				return merr
			}
			if !okm {
				continue // subtree pruned (res==0 => NOT_WITHIN)
			}
		}
		if err := s.descend(childRowid, depthLeft-1); err != nil {
			return err
		}
	}
	return nil
}

// descendLeaf filters and collects one leaf node's cells.
func (s *rtreeScan[T]) descendLeaf(node *rtreeNode[T], nCell int) error {
	for i := 0; i < nCell; i++ {
		cell := s.v.nodeGetCell(node, i)
		keep, err := s.leafCellPasses(cell)
		if err != nil {
			return err
		}
		if keep {
			s.collectLeafRow(cell)
		}
	}
	return nil
}

// leafCellPasses applies the rowid-set filter, the MATCH geometry callback
// and the pushed constraints to one leaf cell.
func (s *rtreeScan[T]) leafCellPasses(cell RtreeCell[T]) (bool, error) {
	if s.rowids != nil {
		if _, ok := s.rowids[cell.iRowid]; !ok {
			return false, nil
		}
	}
	if s.match != nil {
		okm, merr := s.cellMatches(cell.aCoord)
		if merr != nil || !okm {
			return false, merr
		}
	}
	return s.rowidPassesAll(cell.iRowid) && s.coordPassesAll(cell), nil
}
