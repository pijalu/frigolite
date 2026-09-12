package vtab

import (
	"fmt"
	"strings"
	"sync"
)

// ---- r-tree geometry callbacks (MATCH right operand) ----
//
// Faithful port of ext/rtree/rtree.c's sqlite3_rtree_geometry contract plus
// src/test_rtree.c's "cube" and "circle" sample callbacks:
//   - The SQL function registered for a geometry name returns an
//     *RtreeGeometry marker. SQLite carries that value opaquely from the
//     function call to xFilter's MATCH constraint.
//   - The geometry callback runs against EVERY cell of the tree (interior
//     bounding boxes included) with nCoord == 2*nDim coordinates in the REAL
//     domain (rtreeCallbackConstraint); a non-zero result keeps the cell,
//     zero prunes it ("res==0 => NOT_WITHIN").
//   - Parameter validation happens lazily inside the first callback call, so
//     a bad argument count surfaces at query time as the generic SQLITE_ERROR
//     wording — this port returns fmt.Errorf("SQL logic error") so the whole
//     statement reports {1 {SQL logic error}} like SQLite does.

// RtreeGeometry is the opaque value produced by a registered geometry SQL
// function and consumed by the r-tree cursor on MATCH constraints. When
// QueryFn is nil the marker drives the 1st-generation xGeom contract;
// otherwise it drives the 2nd-generation xQueryFunc (RtreeQueryInfo) one.
type RtreeGeometry struct {
	Name      string          // registered SQL function name ("cube", "circle")
	Params    []float64       // numeric arguments, sqlite3_rtree_geometry.aParam
	SqlParams []interface{}   // raw arguments (apSqlParam analogue)
	QueryFn   RtreeQueryFunc  // xQueryFunc (nil for 1st-generation callbacks)
	fn        RtreeGeomFunc   // xGeom
	user      interface{}     // per-statement cache slot (pUser/xDelUser analogue)
}

// eWithin / eParentWithin values (rtree.c NOT_WITHIN / PARTLY_WITHIN /
// FULLY_WITHIN). Visibility of a cell for the 2nd-generation callback
// contract: NOT_WITHIN prunes the cell, FULLY_WITHIN lets the search
// short-circuit subtree re-tests (eParentWithin).
const (
	NotWithin    = 0
	PartlyWithin = 1
	FullyWithin  = 2
)

// RtreeQueryInfo mirrors sqlite3_rtree_query_info: the per-cell state handed
// to a 2nd-generation query callback (sqlite3_rtree_query_callback). The
// callback sets EWithin (NotWithin/PartlyWithin/FullyWithin) and RScore
// (smaller = visited earlier by the priority-queue search) for the cell.
type RtreeQueryInfo struct {
	NParam        int           // number of function parameters
	AParam        []float64     // parameter values
	ApSqlParam    []interface{} // original SQL parameter values
	User          interface{}   // pUser: callback-owned cache across invocations
	ACoord        []float64     // coordinates of the node or entry being tested
	AnQueue       []uint32      // pending queue occupancy per level (anQueue)
	NCoord        int           // number of coordinates
	ILevel        int           // level of the current node (0 = entries)
	MxLevel       int           // largest ILevel in the tree (root level)
	IRowid        int64         // rowid of the current entry (leaf cells only)
	RParentScore  float64       // score of the parent node
	EParentWithin int           // visibility of the parent node
	EWithin       int           // OUT: visibility of this cell
	RScore        float64       // OUT: search priority of this cell
}

// RtreeQueryFunc is one 2nd-generation query callback: it inspects info and
// reports the cell's visibility and score through info.EWithin / info.RScore.
type RtreeQueryFunc func(info *RtreeQueryInfo) error

// RtreeGeomFunc is one geometry callback: it receives the cell coordinates
// (x1,x2,y1,y2,... in float64) and reports non-zero when the cell matches.
type RtreeGeomFunc func(g *RtreeGeometry, nCoord int, aCoord []float64) (int, error)

// Invoke evaluates the 1st-generation callback against one cell, lazily
// binding parameters through g.user caching exactly once per marker instance.
func (g *RtreeGeometry) Invoke(nCoord int, aCoord []float64) (int, error) {
	if g == nil || g.fn == nil {
		return 0, fmt.Errorf("SQL logic error")
	}
	return g.fn(g, nCoord, aCoord)
}

// ---- geometry function registry (MATCH pushdown lookup) ----
//
// In SQLite the SQL function created by sqlite3_rtree_geometry_callback /
// sqlite3_rtree_query_callback returns a sqlite3_result_pointer value: it
// reads as an ordinary NULL in every context (rendering, typeof, CAST) and
// only sqlite3_value_pointer(v, "RtreeMatchArg") — the MATCH constraint
// deserialization (rtree.c deserializeGeometry) — recovers the callback.
// This port mirrors that split: the SQL function returns SQL NULL, and the
// MATCH pushdown rebuilds the marker by function name via
// RtreeGeometryForFunc.

// rtreeGeomFactory builds one marker from the SQL call's arguments.
type rtreeGeomFactory func(args []interface{}) *RtreeGeometry

var (
	rtreeGeomMu       sync.Mutex
	rtreeGeomRegistry = map[string]rtreeGeomFactory{} // key: lowercased SQL name
)

// registerRTreeGeom installs fn/qfn under name: the SQL scalar function
// (returning NULL like a pointer value would) plus the MATCH-pushdown
// factory. Arity is unchecked (SQLite registers with nArg==-1); validation
// belongs to the callback.
func registerRTreeGeom(db Database, name string, fn RtreeGeomFunc, qfn RtreeQueryFunc) {
	rtreeGeomMu.Lock()
	rtreeGeomRegistry[strings.ToLower(name)] = func(args []interface{}) *RtreeGeometry {
		g := &RtreeGeometry{Name: name, fn: fn, QueryFn: qfn}
		for _, a := range args {
			g.SqlParams = append(g.SqlParams, a)
			g.Params = append(g.Params, sqlArgToDouble(a))
		}
		return g
	}
	rtreeGeomMu.Unlock()
	db.RegisterScalar(name, 0, -1, func(args []interface{}) (interface{}, error) {
		return nil, nil // sqlite3_result_pointer: NULL to every ordinary reader
	})
}

// RtreeGeometryForFunc rebuilds the geometry marker a call to the registered
// geometry/query function name with args would have produced (the
// deserializeGeometry analogue for the MATCH pushdown). ok is false when the
// name is not a registered r-tree geometry function; the caller then treats
// the MATCH operand as a non-geometry value ("SQL logic error" parity).
func RtreeGeometryForFunc(name string, args []interface{}) (*RtreeGeometry, bool) {
	rtreeGeomMu.Lock()
	factory, ok := rtreeGeomRegistry[strings.ToLower(name)]
	rtreeGeomMu.Unlock()
	if !ok {
		return nil, false
	}
	return factory(args), true
}

// RegisterRTreeGeometry installs a 1st-generation geometry callback
// (sqlite3_rtree_geometry_callback) under the given SQL function name.
func RegisterRTreeGeometry(db Database, name string, fn RtreeGeomFunc) {
	registerRTreeGeom(db, name, fn, nil)
}

// RegisterRTreeQueryGeometry installs a 2nd-generation query callback
// (sqlite3_rtree_query_callback) under the given SQL function name. The
// cursor runs it through the priority-queue search with full
// RtreeQueryInfo state (rScore/eWithin contract).
func RegisterRTreeQueryGeometry(db Database, name string, fn RtreeQueryFunc) {
	registerRTreeGeom(db, name, nil, fn)
}

// RegisterRTreeCubeGeometry installs test_rtree.c's "cube(x,y,z,w,h,d)"
// intersection callback (rtree9.test sections 1-4).
func RegisterRTreeCubeGeometry(db Database) {
	RegisterRTreeGeometry(db, "cube", rtreeCubeGeom)
}

// RegisterRTreeCircleGeometry installs test_rtree.c's register_circle_geom
// bundle: the 1st-generation 2-D "circle(cx,cy,r)" intersection callback
// (rtree9.test section 5) plus its 2nd-generation companions "Qcircle"
// (circle_query_func: scored search, eScoreType 1-5) and
// "breadthfirstsearch" (bfs_query_func: box-overlap scoring).
func RegisterRTreeCircleGeometry(db Database) {
	RegisterRTreeGeometry(db, "circle", rtreeCircleGeom)
	RegisterRTreeQueryGeometry(db, "Qcircle", rtreeCircleQueryFunc)
	RegisterRTreeQueryGeometry(db, "breadthfirstsearch", rtreeBFSQueryFunc)
}

// sqlArgToDouble mirrors sqlite3_value_double coercion used to fill aParam:
// integers and reals pass through, text takes its numeric prefix, NULL is 0.
func sqlArgToDouble(a interface{}) float64 {
	switch v := a.(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	case string:
		return rtreeNumericPrefix(v) // numeric-prefix coercion (text → 0.0 fallback)
	default:
		return 0
	}
}

// ---- cube callback (test_rtree.c cube_geom) ----

type rtreeCubeState struct {
	x, y, z            float64
	width, height, dep float64
}

// rtreeCubeGeom keeps cells whose bounding box intersects the axis-aligned
// cube defined by (x,y,z,width,height,depth). Width/height/depth must be > 0.
func rtreeCubeGeom(g *RtreeGeometry, nCoord int, aCoord []float64) (int, error) {
	st, ok := g.user.(*rtreeCubeState)
	if !ok {
		// First invocation: validate parameters and build the cached state
		// (test_rtree.c validates p->nParam/nCoord/w>0/h>0/d>0).
		if len(g.Params) != 6 || nCoord != 6 ||
			g.Params[3] <= 0.0 || g.Params[4] <= 0.0 || g.Params[5] <= 0.0 {
			return 0, fmt.Errorf("SQL logic error")
		}
		st = &rtreeCubeState{
			x: g.Params[0], y: g.Params[1], z: g.Params[2],
			width: g.Params[3], height: g.Params[4], dep: g.Params[5],
		}
		g.user = st
	}
	res := 0
	if overlap(aCoord[0], aCoord[1], st.x, st.x+st.width) &&
		overlap(aCoord[2], aCoord[3], st.y, st.y+st.height) &&
		overlap(aCoord[4], aCoord[5], st.z, st.z+st.dep) {
		res = 1
	}
	return res, nil
}

// overlap reports whether [lo,hi] intersects [bLo,bHi] using the C code's
// inclusive endpoint comparisons (aCoord lo <= bound-hi && hi >= bound-lo).
func overlap(lo, hi, bLo, bHi float64) bool {
	return lo <= bHi && hi >= bLo
}

// ---- circle callback (test_rtree.c circle_geom) ----

type rtreeCircleState struct {
	centerx, centery, radius float64
	aBox                     [2][4]float64 // xmin,xmax,ymin,ymax covering boxes
	mxArea                   float64
}

// rtreeCircleGeom keeps cells whose bounding box intersects the circular
// region: a corner inside the circle, or the box covering one of the two
// infinite "cross" boxes split at the center point.
func rtreeCircleGeom(g *RtreeGeometry, nCoord int, aCoord []float64) (int, error) {
	st, ok := g.user.(*rtreeCircleState)
	if !ok {
		var err error
		if st, err = circleStateFromParams(g, nCoord, aCoord); err != nil {
			return 0, err
		}
		g.user = st
	}
	minx, maxx := aCoord[0], aCoord[1]
	miny, maxy := aCoord[2], aCoord[3]

	// Corner-inside test: any box corner within radius of the center counts
	// as intersecting (strict d2 < r*r comparison from the C source).
	for i := 0; i < 4; i++ {
		x := minx
		if i&0x01 != 0 {
			x = maxx
		}
		y := miny
		if i&0x02 != 0 {
			y = maxy
		}
		dx, dy := x-st.centerx, y-st.centery
		if dx*dx+dy*dy < st.radius*st.radius {
			return 1, nil
		}
	}
	// Box-covering test: the cell's rectangle contains one whole cross arm.
	for _, b := range st.aBox {
		if minx <= b[0] && maxx >= b[1] && miny <= b[2] && maxy >= b[3] {
			return 1, nil
		}
	}
	return 0, nil
}

// circleStateFromParams validates the 1st-generation circle parameters and
// builds the cached state (test_rtree.c circle_geom's pUser==0 branch):
// 2-dimensional table only, exactly three parameters, radius >= 0.
func circleStateFromParams(g *RtreeGeometry, nCoord int, aCoord []float64) (*rtreeCircleState, error) {
	if nCoord != 4 || len(g.Params) != 3 || g.Params[2] < 0.0 {
		return nil, fmt.Errorf("SQL logic error")
	}
	st := &rtreeCircleState{
		centerx: g.Params[0],
		centery: g.Params[1],
		radius:  g.Params[2],
		mxArea:  (aCoord[1] - aCoord[0]) * (aCoord[3] - aCoord[2]),
	}
	st.mxArea += 1.0
	// Two degenerate boxes crossing at the circle center: box[0]
	// spans X=[cx,cx] Y=[cy-r,cy+r]; box[1] spans X=[cx-r,cx+r]
	// Y=[cy,cy] (note the C source stores ymax < ymin).
	st.aBox[0] = [4]float64{st.centerx, st.centerx, st.centery + st.radius, st.centery - st.radius}
	st.aBox[1] = [4]float64{st.centerx + st.radius, st.centerx - st.radius, st.centery, st.centery}
	return st, nil
}

// ---- 2nd-generation callbacks (test_rtree.c circle_query_func / bfs_query_func) ----

// rtreeCircleQueryState caches the parsed parameters of one Qcircle marker
// (test_rtree.c's Circle for the 2nd-generation interface).
type rtreeCircleQueryState struct {
	centerx, centery, radius float64
	eScoreType               int
	aBox                     [2][4]float64 // xmin,xmax,ymin,ymax cross boxes
	mxArea                   float64
}

// rtreeCircleQueryFunc ports test_rtree.c circle_query_func: the callback
// behind Qcircle. Two calling forms — Qcircle(X,Y,Radius,eType) with four
// doubles, or Qcircle('x:X y:Y r:R e:ETYPE') with one text parameter — and
// five scoring modes (eScoreType): 1 depth-first, 2 breadth-first, 3
// depth-first with leaf nodes sorted by area (largest first), 4/5 same but
// excluding odd rowids.
func rtreeCircleQueryFunc(info *RtreeQueryInfo) error {
	st, _ := info.User.(*rtreeCircleQueryState)
	if st == nil {
		var err error
		if st, err = parseCircleQueryState(info); err != nil {
			return err
		}
		info.User = st
	}
	nWithin := circleCornersWithin(st, info)
	applyCircleScore(st, info, &nWithin)
	switch {
	case nWithin == 0:
		info.EWithin = NotWithin
	case nWithin >= 4:
		info.EWithin = FullyWithin
	default:
		info.EWithin = PartlyWithin
	}
	return nil
}

// parseCircleQueryState validates the first invocation's parameters and
// builds the cached circle state (test_rtree.c validates nCoord==4,
// nParam∈{1,4}, radius>=0).
func parseCircleQueryState(info *RtreeQueryInfo) (*rtreeCircleQueryState, error) {
	if info.NCoord != 4 || (info.NParam != 4 && info.NParam != 1) {
		return nil, fmt.Errorf("SQL logic error")
	}
	st := &rtreeCircleQueryState{}
	if info.NParam == 4 {
		st.centerx = info.AParam[0]
		st.centery = info.AParam[1]
		st.radius = info.AParam[2]
		st.eScoreType = int(info.AParam[3])
	} else if err := parseCircleSpec(info, st); err != nil {
		return nil, err
	}
	if st.radius < 0.0 {
		// test_rtree.c returns SQLITE_NOMEM here ("out of memory").
		return nil, errCapitalized{"out of memory"}
	}
	st.aBox[0] = [4]float64{st.centerx, st.centerx, st.centery + st.radius, st.centery - st.radius}
	st.aBox[1] = [4]float64{st.centerx + st.radius, st.centerx - st.radius, st.centery, st.centery}
	// Note: unlike circle_geom (root-cell area + 1), the scored variant uses
	// the FIXED 200x200 reference area from test_rtree.c.
	st.mxArea = 200.0 * 200.0
	return st, nil
}

// parseCircleSpec fills st from Qcircle's single text parameter form
// ('x:X y:Y r:R e:ETYPE'), mirroring circle_query_func's token walk: tokens
// are runs of non-space characters and only the two-character key prefixes
// r:/x:/y:/e: are significant.
func parseCircleSpec(info *RtreeQueryInfo, st *rtreeCircleQueryState) error {
	z, _ := info.ApSqlParam[0].(string)
	st.centerx, st.centery, st.radius, st.eScoreType = 0, 0, 0, 0
	for _, tok := range strings.Fields(z) {
		if len(tok) < 2 || tok[1] != ':' {
			continue
		}
		switch tok[0] {
		case 'r':
			st.radius = rtreeNumericPrefix(tok[2:])
		case 'x':
			st.centerx = rtreeNumericPrefix(tok[2:])
		case 'y':
			st.centery = rtreeNumericPrefix(tok[2:])
		case 'e':
			st.eScoreType = int(rtreeNumericPrefix(tok[2:]))
		}
	}
	return nil
}

// circleCornersWithin counts how many of the cell box's corners lie inside
// the circle, falling back to the whole-cross-arm containment test
// (test_rtree.c's two corner/box loops).
func circleCornersWithin(st *rtreeCircleQueryState, info *RtreeQueryInfo) int {
	xmin, xmax := info.ACoord[0], info.ACoord[1]
	ymin, ymax := info.ACoord[2], info.ACoord[3]
	nWithin := 0
	for i := 0; i < 4; i++ {
		x := xmin
		if i&0x01 != 0 {
			x = xmax
		}
		y := ymin
		if i&0x02 != 0 {
			y = ymax
		}
		dx, dy := x-st.centerx, y-st.centery
		if dx*dx+dy*dy < st.radius*st.radius {
			nWithin++
		}
	}
	if nWithin == 0 {
		for _, b := range st.aBox {
			if xmin <= b[0] && xmax >= b[1] && ymin <= b[2] && ymax >= b[3] {
				nWithin = 1
				break
			}
		}
	}
	return nWithin
}

// applyCircleScore sets the callback's rScore per eScoreType, applying the
// odd-rowid exclusion of modes 4/5 (test_rtree.c's scoring switch).
func applyCircleScore(st *rtreeCircleQueryState, info *RtreeQueryInfo, nWithin *int) {
	switch st.eScoreType {
	case 1: // depth-first search
		info.RScore = float64(info.ILevel)
	case 2: // breadth-first search
		info.RScore = 100 - float64(info.ILevel)
	case 3: // depth-first, leaf nodes sorted by area with the largest first
		if info.ILevel == 1 {
			info.RScore = 1.0 - (info.ACoord[1]-info.ACoord[0])*(info.ACoord[3]-info.ACoord[2])/st.mxArea
			if info.RScore < 0.01 {
				info.RScore = 0.01
			}
		} else {
			info.RScore = 0.0
		}
	case 4: // depth-first, excluding odd rowids
		info.RScore = float64(info.ILevel)
		if info.IRowid&1 != 0 {
			*nWithin = 0
		}
	default: // breadth-first, excluding odd rowids
		info.RScore = 100 - float64(info.ILevel)
		if info.IRowid&1 != 0 {
			*nWithin = 0
		}
	}
}

// rtreeBFSQueryFunc ports test_rtree.c bfs_query_func, the callback behind
// "breadthfirstsearch": it returns every entry whose box overlaps the query
// box, scored breadth-first (rScore = 100 - iLevel). Exactly four parameters.
func rtreeBFSQueryFunc(info *RtreeQueryInfo) error {
	if info.NParam != 4 {
		return fmt.Errorf("SQL logic error")
	}
	x0, x1 := info.ACoord[0], info.ACoord[1]
	y0, y1 := info.ACoord[2], info.ACoord[3]
	bx0, bx1 := info.AParam[0], info.AParam[1]
	by0, by1 := info.AParam[2], info.AParam[3]
	info.RScore = 100 - float64(info.ILevel)
	switch {
	case info.EParentWithin == FullyWithin:
		info.EWithin = FullyWithin
	case x0 >= bx0 && x1 <= bx1 && y0 >= by0 && y1 <= by1:
		info.EWithin = FullyWithin
	case x1 >= bx0 && x0 <= bx1 && y1 >= by0 && y0 <= by1:
		info.EWithin = PartlyWithin
	default:
		info.EWithin = NotWithin
	}
	return nil
}
