package vtab

import (
	"fmt"
	"strings"
)

// The "geopoly" virtual table module (geopolyInit / geopolyModule in
// ext/rtree/geopoly.c): an rtree subclass (RTREE_COORD_REAL32, nDim=2) whose
// user columns are ALL auxiliary — a0 is the hidden "_shape" polygon, a1..
// the declared user columns — sharing the rtree shadow tables, node codec
// and scan machinery, with geopolyBestIndex's overloaded-function
// constraints (geopoly_overlap / geopoly_within) narrowing scans to the
// query polygon's bounding box.

// ---- the geopoly virtual table module ----

// GeopolyModule implements the "geopoly" virtual table module
// (geopolyModule in geopoly.c): xCreate/xConnect route through the rtree
// machinery with nDim=2 REAL32 coordinates and every user column declared as
// auxiliary. The module is a plain (non-eponymous) module — the name is only
// usable via CREATE VIRTUAL TABLE, matching geopolyInit's registration.
type GeopolyModule struct {
	rtree *RtreeModule[float32]
}

// NewGeopolyModule builds the module bound to the engine's Database handle
// (the rtree subclass needs it for shadow tables and function registration).
func NewGeopolyModule(db Database) *GeopolyModule {
	return &GeopolyModule{rtree: NewRtreeModule[float32](db)}
}

// Create implements Module (xCreate).
func (m *GeopolyModule) Create(args []string) (VirtualTable, error) {
	return m.connect(args, true)
}

// Connect implements Module (xConnect).
func (m *GeopolyModule) Connect(args []string) (VirtualTable, error) {
	return m.connect(args, false)
}

// connect builds one geopoly instance (geopolyInit): the declared schema is
// "CREATE TABLE x(_shape<,user column>...)" — _shape first, then every module
// argument verbatim — and internally nAux counts _shape itself plus the user
// columns, all stored in %_rowid's aN columns.
func (m *GeopolyModule) connect(args []string, isCreate bool) (VirtualTable, error) {
	names := make([]string, 0, len(args))
	for _, a := range args {
		s := rtreeStripComments(a)
		if strings.HasPrefix(s, "+") {
			// declare_vtab rejects the '+' the C code would have copied
			// verbatim into the CREATE TABLE text ("near \"+\": syntax error").
			return nil, fmt.Errorf(`near "+": syntax error`)
		}
		names = append(names, rtreeFirstToken(s))
	}
	declared := append([]string{"_shape"}, names...)
	rt := &rtreeVTab[float32]{
		module:        m.rtree,
		columns:       declared,
		declared:      declared,
		nDim:          2,
		nDim2:         4,
		nAux:          1 + len(names),
		nBytesPerCell: 8 + 4*4,
		coordKind:     RTREE_COORD_REAL32,
		created:       isCreate,
	}
	if err := rt.queryStat1(); err != nil {
		return nil, err
	}
	return &geopolyVTab{rtree: rt, declared: declared}, nil
}

// geopolyFuncQuery records the last pushed geopoly_overlap/geopoly_within
// constraint (geopolyBestIndex keeps only the LAST function term: idxNum is
// overwritten as the constraint array is scanned).
type geopolyFuncQuery struct {
	within bool
	bbox   [4]float32
}

// geopolyVTab is one bound geopoly table. The r-tree state (shadow tables,
// node cache, cells) lives in the embedded float32 rtree instance; the outer
// type adapts it to geopoly's declared layout (user columns are auxiliary,
// there are no exposed coordinate columns).
type geopolyVTab struct {
	rtree     *rtreeVTab[float32]
	declared  []string
	funcQuery *geopolyFuncQuery // last pushed overload constraint (nil = none)
	funcErr   error             // xFilter's geopolyBBox failure ("SQL logic error")
}

// Columns reports the declared schema: _shape followed by the user columns.
func (v *geopolyVTab) Columns() []string { return v.declared }

// ColumnTypes reports empty declared types — geopolyInit declares
// "CREATE TABLE x(_shape,...)" with no type names.
func (v *geopolyVTab) ColumnTypes() []string {
	return make([]string, len(v.declared))
}

// BestIndex implements VirtualTable (geopolyBestIndex runs through the
// engine's constraint-pushdown contract; see PushGeopolyFunc).
func (v *geopolyVTab) BestIndex(input []byte) ([]byte, error) { return nil, nil }

// FindFunction implements FunctionOverloader (geopolyFindFunction):
// geopoly_overlap maps to SQLITE_INDEX_CONSTRAINT_FUNCTION and geopoly_within
// to FUNCTION+1. The legacy pushdown path consumes these through
// PushGeopolyFunc instead; the codes are kept for xFindFunction parity.
func (v *geopolyVTab) FindFunction(name string, nArg int) int {
	switch strings.ToLower(name) {
	case "geopoly_overlap":
		return int(IndexConstraintFunction)
	case "geopoly_within":
		return int(IndexConstraintFunction) + 1
	}
	return 0
}

// BindSchema implements SchemaBoundVTab: the shadow family lifecycle is the
// rtree's (CREATE adopts an existing family — the C-written shadows of a
// reopened database are used verbatim, never re-created or re-seeded).
func (v *geopolyVTab) BindSchema(dbName, tableName string) error {
	return v.rtree.BindSchema(dbName, tableName)
}

// PushRTreeConstraint implements ConstraintSink, remapping declared column
// numbers (0=_shape, 1..=user columns) to the rtree's auxiliary constraint
// slots so the re-check runs against the %_rowid-attached values.
func (v *geopolyVTab) PushRTreeConstraint(col int, op string, value interface{}) {
	v.rtree.PushRTreeConstraint(v.rtree.nDim2+1+col, op, value)
}

// PushRTreeRowids implements ConstraintSink (an id-membership restriction;
// geopoly has no declared id column, so this stays unused in practice).
func (v *geopolyVTab) PushRTreeRowids(ids []int64) { v.rtree.PushRTreeRowids(ids) }

// GeopolyFuncSink receives `geopoly_overlap(_shape, X)` /
// `geopoly_within(_shape, X)` WHERE conjuncts (geopolyBestIndex idxNum 2/3
// parity). The receiver narrows the scan to X's bounding-box candidate set;
// the conjunct itself is NOT consumed — C leaves
// aConstraintUsage[].omit = 0 so the core re-checks the true polygon
// predicate per candidate row.
type GeopolyFuncSink interface {
	PushGeopolyFunc(name string, col int, value interface{})
}

// PushGeopolyFunc implements GeopolyFuncSink. Only the two overloaded names
// anchored on column 0 (_shape) participate; each push replaces the previous
// state. An unusable polygon is remembered as the scan error xFilter's
// geopolyBBox path would raise ("SQL logic error").
func (v *geopolyVTab) PushGeopolyFunc(name string, col int, value interface{}) {
	var within bool
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "geopoly_overlap":
		within = false
	case "geopoly_within":
		within = true
	default:
		return
	}
	if col != 0 {
		return
	}
	poly, rc := geopolyFuncParam(value)
	if rc == geoError {
		v.funcQuery = nil
		v.funcErr = fmt.Errorf("SQL logic error")
		return
	}
	v.funcErr = nil
	v.funcQuery = &geopolyFuncQuery{within: within, bbox: geopolyBBoxOf(poly)}
}

// applyFuncQuery translates the pushed overload constraint into the four
// r-tree coordinate constraints of geopolyFilter (ops 'B'/'D' = <=/>=):
// an overlap query keeps cells whose box intersects the query bbox; a within
// query keeps cells whose box is contained in it. Values are widened to
// float64, the comparison domain of the rtree constraint sink.
func (v *geopolyVTab) applyFuncQuery(q *geopolyFuncQuery) {
	b := q.bbox
	f := func(x float32) float64 { return float64(x) }
	if q.within {
		v.rtree.PushRTreeConstraint(1, ">=", f(b[0]))
		v.rtree.PushRTreeConstraint(2, "<=", f(b[1]))
		v.rtree.PushRTreeConstraint(3, ">=", f(b[2]))
		v.rtree.PushRTreeConstraint(4, "<=", f(b[3]))
	} else {
		v.rtree.PushRTreeConstraint(1, "<=", f(b[1]))
		v.rtree.PushRTreeConstraint(2, ">=", f(b[0]))
		v.rtree.PushRTreeConstraint(3, "<=", f(b[3]))
		v.rtree.PushRTreeConstraint(4, ">=", f(b[2]))
	}
}

// Open implements VirtualTable (geopolyFilter): the query-constraint bbox
// narrowing is applied first, then the rtree scan runs and the cursor is
// re-projected onto the declared (_shape, user...) columns.
func (v *geopolyVTab) Open() (Cursor, error) {
	if v.funcErr != nil {
		return nil, v.funcErr
	}
	if v.funcQuery != nil {
		v.applyFuncQuery(v.funcQuery)
	}
	inner, err := v.rtree.openCursor()
	if err != nil {
		return nil, err
	}
	rc, ok := inner.(*rtreeCursor[float32])
	if !ok {
		return nil, fmt.Errorf("geopoly: unexpected cursor type %T", inner)
	}
	return &geopolyCursor{inner: rc}, nil
}

// rtreeDeleteRowid delegates to the embedded rtree instance (kept as a
// method so the write path reads like the other operations).
func (v *geopolyVTab) rtreeDeleteRowid(rowid int64) error {
	return v.rtree.rtreeDeleteRowid(rowid)
}

// geopolyCursor projects the rtree scan rows [id, coords..., aux...] onto
// geopoly's declared columns (aux only).
type geopolyCursor struct {
	inner *rtreeCursor[float32]
}

// Next implements Cursor.
func (c *geopolyCursor) Next() bool { return c.inner.Next() }

// Close implements Cursor.
func (c *geopolyCursor) Close() error { return c.inner.Close() }

// Rowid implements RowidCursor: the entry id.
func (c *geopolyCursor) Rowid() int64 { return c.inner.Rowid() }

// Column implements Cursor: declared column i reads auxiliary value i from
// the underlying scan row [id, 4 coordinates, aux...] — the C xColumn reads
// sqlite3_column_value(pReadAux, i+2).
func (c *geopolyCursor) Column(idx int) (interface{}, error) {
	return c.inner.Column(1 + 4 + idx)
}
