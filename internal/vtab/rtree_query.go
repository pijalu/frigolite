package vtab

import (
	"fmt"

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
// applied to the literal — matching rtree.c's xFilter evaluation domain.
func coordPasses[T coordType](op string, c T, value interface{}) bool {
	lhs := coordToOut[T](c)
	rhs := applyColumnAffinity(value, isInt32Coord[T]())
	return numCompare(op, lhs, rhs)
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
// INTEGER-affinity).
func rowidPasses(op string, id int64, value interface{}) bool {
	return numCompare(op, id, applyColumnAffinity(value, true))
}

// ---- filtered scan (priority-ordered MBR descent) ----

// scanDataRowsFiltered walks the tree depth-first honoring the pushed
// constraints. Internal cells prune only when the subtree's bounding box
// cannot satisfy ANY dimension constraint — a plain MBR rejection test — so
// results stay identical to unfiltered enumeration even under mixed operators.
// match (when non-nil) is a MATCH geometry callback: rtree.c invokes it for
// every cell at BOTH levels with that cell's coordinates, pruning it when the
// callback reports zero.
func (v *rtreeVTab[T]) collectDataRows(constraints []rtreeConstraint[T], rowids rtreeRowidSet, match *RtreeGeometry) ([][]interface{}, error) {
	isRowidCol := func(c int) bool { return c == 0 }
	var out [][]interface{}
	hasRowidSet := rowids != nil

	// cellMatches runs the geometry callback against one cell's MBR
	// (rtreeCallbackConstraint: nCoord = 2*nDim REAL-domain values).
	cellMatches := func(coords []T) (bool, error) {
		fl := make([]float64, len(coords))
		for i, c := range coords {
			fl[i] = asFloat64(c)
		}
		res, err := match.Invoke(len(fl), fl)
		if err != nil {
			return false, err
		}
		return res != 0, nil
	}

	descend := func(nodeno int64, depthLeft int) error { return nil }
	descend = func(nodeno int64, depthLeft int) error {
		node, err := v.nodeAcquire(nodeno)
		if err != nil {
			return err
		}
		defer v.nodeRelease(node)
		nCell := node.nCell()
		if depthLeft == 0 {
			for i := 0; i < nCell; i++ {
				cell := v.nodeGetCell(node, i)
				id := cell.iRowid
				if hasRowidSet {
					if _, ok := rowids[id]; !ok {
						continue
					}
				}
				if match != nil {
					okm, merr := cellMatches(cell.aCoord)
					if merr != nil {
						return merr
					}
					if !okm {
						continue
					}
				}
				pass := true
				for _, con := range constraints {
					if isRowidCol(con.col) {
						if !rowidPasses(con.op, id, con.value) {
							pass = false
							break
						}
						continue
					}
					ci := con.col - 1
					if ci >= len(cell.aCoord) {
						continue
					}
					if !coordPasses(con.op, cell.aCoord[ci], con.value) {
						pass = false
						break
					}
				}
				if !pass {
					continue
				}
				row := make([]interface{}, 0, 1+v.nDim2+v.nAux)
				row = append(row, id)
				for j := 0; j < v.nDim2; j++ {
					row = append(row, coordToOut[T](cell.aCoord[j]))
				}
				out = append(out, row)
			}
			return nil
		}
		for i := 0; i < nCell; i++ {
			childRowid := v.nodeGetRowid(node, i)
			if match != nil {
				mbr := make([]T, v.nDim2)
				copy(mbr, v.nodeGetCell(node, i).aCoord)
				okm, merr := cellMatches(mbr)
				if merr != nil {
					return merr
				}
				if !okm {
					continue // subtree pruned (res==0 => NOT_WITHIN)
				}
			}
			if err := descend(childRowid, depthLeft-1); err != nil {
				return err
			}
		}
		return nil
	}

	root, err := v.rootAcquire()
	if err != nil {
		return nil, err
	}
	defer v.nodeRelease(root)
	if err := descend(1, root.depth()); err != nil {
		return nil, err
	}
	return out, nil
}
