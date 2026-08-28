package vtab

import (
	"fmt"
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
// function and consumed by the r-tree cursor on MATCH constraints.
type RtreeGeometry struct {
	Name      string        // registered SQL function name ("cube", "circle")
	Params    []float64     // numeric arguments, sqlite3_rtree_geometry.aParam
	SqlParams []interface{} // raw arguments (apSqlParam analogue)
	fn        RtreeGeomFunc // xGeom
	user      interface{}   // per-statement cache slot (pUser/xDelUser analogue)
}

// RtreeGeomFunc is one geometry callback: it receives the cell coordinates
// (x1,x2,y1,y2,... in float64) and reports non-zero when the cell matches.
type RtreeGeomFunc func(g *RtreeGeometry, nCoord int, aCoord []float64) (int, error)

// Invoke evaluates the callback against one cell, lazily binding parameters
// through g.user caching exactly once per marker instance.
func (g *RtreeGeometry) Invoke(nCoord int, aCoord []float64) (int, error) {
	if g == nil || g.fn == nil {
		return 0, fmt.Errorf("SQL logic error")
	}
	return g.fn(g, nCoord, aCoord)
}

// RegisterRTreeGeometry installs a geometry callback under the given SQL
// function name. Arity is unchecked here (SQLite registers with nArg==-2);
// validation belongs to the callback.
func RegisterRTreeGeometry(db Database, name string, fn RtreeGeomFunc) {
	db.RegisterScalar(name, 0, -1, func(args []interface{}) (interface{}, error) {
		g := &RtreeGeometry{Name: name, fn: fn}
		for _, a := range args {
			g.SqlParams = append(g.SqlParams, a)
			g.Params = append(g.Params, sqlArgToDouble(a))
		}
		return g, nil
	})
}

// RegisterRTreeCubeGeometry installs test_rtree.c's "cube(x,y,z,w,h,d)"
// intersection callback (rtree9.test sections 1-4).
func RegisterRTreeCubeGeometry(db Database) {
	RegisterRTreeGeometry(db, "cube", rtreeCubeGeom)
}

// RegisterRTreeCircleGeometry installs test_rtree.c's 2-D "circle(cx,cy,r)"
// intersection callback (rtree9.test section 5).
func RegisterRTreeCircleGeometry(db Database) {
	RegisterRTreeGeometry(db, "circle", rtreeCircleGeom)
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
		// 2-dimensional table only, exactly three parameters, radius >= 0.
		if nCoord != 4 || len(g.Params) != 3 || g.Params[2] < 0.0 {
			return 0, fmt.Errorf("SQL logic error")
		}
		st = &rtreeCircleState{
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
