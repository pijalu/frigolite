package vtab

import (
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// geopoly.c's scalar and aggregate SQL functions (sqlite3_geopoly_init).
// Every function interprets its polygon argument through geopolyFuncParam:
// an unusable input renders as SQL NULL (geopolyFuncParam never reports an
// error for ordinary values). geopoly_debug is GEOPOLY_ENABLE_DEBUG-gated in
// the C source and is intentionally not registered.

// GEOPOLY_PI is geopoly.c's pi constant.
const GEOPOLY_PI = 3.1415926535897932385

// RegisterGeopolyFunctions installs the geopoly scalar functions and the
// geopoly_group_bbox aggregate on db (sqlite3_geopoly_init's aFunc/aAgg
// tables, minus the debug-gated geopoly_debug).
func RegisterGeopolyFunctions(db Database) {
	db.RegisterScalar("geopoly_area", 1, 1, geopolyAreaFn)
	db.RegisterScalar("geopoly_blob", 1, 1, geopolyBlobFn)
	db.RegisterScalar("geopoly_json", 1, 1, geopolyJSONFn)
	db.RegisterScalar("geopoly_svg", 1, -1, geopolySVGFn)
	db.RegisterScalar("geopoly_within", 2, 2, geopolyWithinFn)
	db.RegisterScalar("geopoly_contains_point", 3, 3, geopolyContainsPointFn)
	db.RegisterScalar("geopoly_overlap", 2, 2, geopolyOverlapFn)
	db.RegisterScalar("geopoly_bbox", 1, 1, geopolyBBoxFn)
	db.RegisterScalar("geopoly_xform", 7, 7, geopolyXformFn)
	db.RegisterScalar("geopoly_regular", 4, 4, geopolyRegularFn)
	db.RegisterScalar("geopoly_ccw", 1, 1, geopolyCcwFn)
	db.RegisterAggregate("geopoly_group_bbox", 1, 1, func() Aggregator {
		return &geopolyGroupBBox{}
	})
}

// ---- shared helpers ----

// geoParam unwraps and interprets one function argument.
func geoParam(args []interface{}, i int) (*geoPoly, int) {
	return geopolyFuncParam(util.UnwrapColumnValue(args[i]))
}

// geoBlobResult encodes a polygon as the function result blob.
func geoBlobResult(p *geoPoly) interface{} { return p.encodeBlob() }

// geoValueDouble coerces an argument with sqlite3_value_double semantics
// (numeric text takes its numeric prefix; NULL and other types give 0).
func geoValueDouble(v interface{}) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case string:
		return rtreeNumericPrefix(x)
	case []byte:
		return rtreeNumericPrefix(string(x))
	default:
		return 0
	}
}

// geoValueInt coerces an argument with sqlite3_value_int semantics: the
// int64-domain value (rtreeRowidFromValues) truncated to C int.
func geoValueInt(v interface{}) int {
	return int(int32(rtreeRowidFromValues([]interface{}{v})))
}

// ---- SQLite printf %g / %!g rendering ----

// sqliteFormatG renders v like SQLite's printf %g (alt2=false) or %!g
// (alt2=true) with the default precision of 6: both strip trailing zeros,
// but %g drops a bare trailing decimal point while %!g keeps one digit
// after it ("0" vs "0.0", "1e+20" vs "1.0e+20"). Exponents use at least two
// digits; the switch to exponential form happens for exp<-4 or exp>5.
func sqliteFormatG(v float64, alt2 bool) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	if v == 0 {
		// -0.0 renders unsigned (sqlite3FpDecode treats it as zero).
		if alt2 {
			return "0.0"
		}
		return "0"
	}
	neg := ""
	if v < 0 {
		neg = "-"
		v = -v
	}
	const prec = 6
	s := strconv.FormatFloat(v, 'e', prec-1, 64)
	eIdx := strings.IndexByte(s, 'e')
	exp, _ := strconv.Atoi(s[eIdx+1:])
	digits := strings.Replace(s[:eIdx], ".", "", 1) // exactly prec digits
	if exp < -4 || exp > prec-1 {
		return neg + formatGExp(trimGZeros(digits), exp, alt2)
	}
	return neg + formatGFixed(digits, exp, alt2)
}

// trimGZeros strips trailing zero digits (printf flag_rtz).
func trimGZeros(d string) string {
	for len(d) > 0 && d[len(d)-1] == '0' {
		d = d[:len(d)-1]
	}
	return d
}

// formatGExp renders the exponential form "d[.ddd]e±NN" (at least two
// exponent digits); alt2 keeps one digit after a bare decimal point.
func formatGExp(mant string, exp int, alt2 bool) string {
	out := mant[:1]
	if len(mant) > 1 || alt2 {
		out += "."
		if len(mant) > 1 {
			out += mant[1:]
		} else {
			out += "0"
		}
	}
	e := strconv.Itoa(exp)
	if len(e) < 2 {
		e = "0" + e
	}
	if exp < 0 {
		e = "-" + e
	} else {
		e = "+" + e
	}
	return out + "e" + e
}

// formatGFixed renders the fixed-precision form with prec-1-exp digits after
// the decimal point; trailing zeros are stripped and a bare trailing point
// becomes ".0" under alt2, else is dropped.
func formatGFixed(digits string, exp int, alt2 bool) string {
	const prec = 6
	after := prec - 1 - exp
	var out string
	if exp < 0 {
		out = "0." + strings.Repeat("0", -exp-1) + digits
	} else {
		frac := digits[exp+1:]
		out = digits[:exp+1] + "." + frac + strings.Repeat("0", maxInt(0, after-len(frac)))
	}
	out = trimGZeros(out)
	if strings.HasSuffix(out, ".") {
		if alt2 {
			out += "0"
		} else {
			out = out[:len(out)-1]
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- scalar functions ----

// geopolyAreaFn implements geopoly_area(X): the enclosed area (negative for
// clockwise windings), or NULL for an unusable input.
func geopolyAreaFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	return geopolyArea(p), nil
}

// geopolyArea computes the signed enclosed area (geopoly.c geopolyArea).
func geopolyArea(p *geoPoly) float64 {
	rArea := 0.0
	ii := 0
	for ; ii < p.nVertex-1; ii++ {
		rArea += (geoX(p, ii) - geoX(p, ii+1)) * (geoY(p, ii) + geoY(p, ii+1)) * 0.5
	}
	rArea += (geoX(p, ii) - geoX(p, 0)) * (geoY(p, ii) + geoY(p, 0)) * 0.5
	return rArea
}

// geopolyBlobFn implements geopoly_blob(X).
func geopolyBlobFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	return geoBlobResult(p), nil
}

// geopolyJSONFn implements geopoly_json(X): the GeoJSON rendering with the
// closing vertex repeated, coordinates formatted with printf "%!g".
func geopolyJSONFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	var out strings.Builder
	out.WriteByte('[')
	for i := 0; i < p.nVertex; i++ {
		out.WriteString("[")
		out.WriteString(sqliteFormatG(geoX(p, i), true))
		out.WriteString(",")
		out.WriteString(sqliteFormatG(geoY(p, i), true))
		out.WriteString("],")
	}
	out.WriteString("[")
	out.WriteString(sqliteFormatG(geoX(p, 0), true))
	out.WriteString(",")
	out.WriteString(sqliteFormatG(geoY(p, 0), true))
	out.WriteString("]]")
	return out.String(), nil
}

// geopolySVGFn implements geopoly_svg(X, ...): an SVG <polyline> element
// with the points attribute, extra arguments appended verbatim as
// attributes when non-empty text.
func geopolySVGFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("<polyline points=")
	cSep := byte('\'')
	for i := 0; i < p.nVertex; i++ {
		b.WriteByte(cSep)
		b.WriteString(sqliteFormatG(geoX(p, i), false))
		b.WriteByte(',')
		b.WriteString(sqliteFormatG(geoY(p, i), false))
		cSep = ' '
	}
	b.WriteString(" ")
	b.WriteString(sqliteFormatG(geoX(p, 0), false))
	b.WriteByte(',')
	b.WriteString(sqliteFormatG(geoY(p, 0), false))
	b.WriteByte('\'')
	for i := 1; i < len(args); i++ {
		if s, ok := util.UnwrapColumnValue(args[i]).(string); ok && s != "" {
			b.WriteString(" ")
			b.WriteString(s)
		}
	}
	b.WriteString("></polyline>")
	return b.String(), nil
}

// geopolyWithinFn implements geopoly_within(P1,P2): 1 when P1 is inside P2
// (overlap result 2), 2 when identical (result 4), else 0.
func geopolyWithinFn(args []interface{}) (interface{}, error) {
	p1, _ := geoParam(args, 0)
	p2, _ := geoParam(args, 1)
	if p1 != nil && p2 != nil {
		x := geopolyOverlap(p1, p2)
		switch x {
		case 2:
			return int64(1), nil
		case 4:
			return int64(2), nil
		default:
			return int64(0), nil
		}
	}
	return nil, nil
}

// geopolyOverlapFn implements geopoly_overlap(P1,P2): 0 disjoint, 1 partial
// overlap, 2 P1 inside P2, 3 P2 inside P1, 4 identical; NULL when either
// input is not a polygon.
func geopolyOverlapFn(args []interface{}) (interface{}, error) {
	p1, _ := geoParam(args, 0)
	p2, _ := geoParam(args, 1)
	if p1 != nil && p2 != nil {
		return int64(geopolyOverlap(p1, p2)), nil
	}
	return nil, nil
}

// pointBeneathLine ports geopoly.c pointBeneathLine: +2 when (x0,y0) lies on
// the segment, +1 when strictly beneath it, 0 otherwise. The left-most
// endpoint is excluded from the segment.
func pointBeneathLine(x0, y0, x1, y1, x2, y2 float64) int {
	if x0 == x1 && y0 == y1 {
		return 2
	}
	switch {
	case x1 < x2:
		if x0 <= x1 || x0 > x2 {
			return 0
		}
	case x1 > x2:
		if x0 <= x2 || x0 > x1 {
			return 0
		}
	default:
		return pointBeneathVertical(x0, y0, x1, y1, y2)
	}
	y := y1 + (y2-y1)*(x0-x1)/(x2-x1)
	if y0 == y {
		return 2
	}
	if y0 < y {
		return 1
	}
	return 0
}

// pointBeneathVertical handles the vertical-segment case of
// pointBeneathLine: only points on the segment itself count (+2).
func pointBeneathVertical(x0, y0, x1, y1, y2 float64) int {
	if x0 != x1 {
		return 0
	}
	if y0 < y1 && y0 < y2 {
		return 0
	}
	if y0 > y1 && y0 > y2 {
		return 0
	}
	return 2
}

// geopolyContainsPointFn implements geopoly_contains_point(P,X,Y): 1 when
// the point is on the boundary, 2 when inside, 0 when outside; NULL when P
// is not a polygon.
func geopolyContainsPointFn(args []interface{}) (interface{}, error) {
	p1, _ := geoParam(args, 0)
	if p1 == nil {
		return nil, nil
	}
	x0 := geoValueDouble(util.UnwrapColumnValue(args[1]))
	y0 := geoValueDouble(util.UnwrapColumnValue(args[2]))
	v := 0
	cnt := 0
	ii := 0
	for ; ii < p1.nVertex-1; ii++ {
		v = pointBeneathLine(x0, y0, geoX(p1, ii), geoY(p1, ii), geoX(p1, ii+1), geoY(p1, ii+1))
		if v == 2 {
			break
		}
		cnt += v
	}
	if v != 2 {
		v = pointBeneathLine(x0, y0, geoX(p1, ii), geoY(p1, ii), geoX(p1, 0), geoY(p1, 0))
	}
	switch {
	case v == 2:
		return int64(1), nil
	case (v+cnt)&1 == 0:
		return int64(0), nil
	default:
		return int64(2), nil
	}
}

// geopolyBBoxFn implements geopoly_bbox(X): the 4-vertex bounding-box polygon.
func geopolyBBoxFn(args []interface{}) (interface{}, error) {
	p, rc := geoParam(args, 0)
	if rc == geoError {
		return nil, nil
	}
	return geopolyBBoxPoly(geopolyBBoxOf(p)), nil
}

// geopolyXformFn implements geopoly_xform(poly,A,B,C,D,E,F):
// x1 = A*x0 + B*y0 + E, y1 = C*x0 + D*y0 + F (computed in double, stored
// back as float32), returned as the transformed blob.
func geopolyXformFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	a := geoValueDouble(util.UnwrapColumnValue(args[1]))
	b := geoValueDouble(util.UnwrapColumnValue(args[2]))
	c := geoValueDouble(util.UnwrapColumnValue(args[3]))
	d := geoValueDouble(util.UnwrapColumnValue(args[4]))
	e := geoValueDouble(util.UnwrapColumnValue(args[5]))
	f := geoValueDouble(util.UnwrapColumnValue(args[6]))
	for ii := 0; ii < p.nVertex; ii++ {
		x0, y0 := geoX(p, ii), geoY(p, ii)
		p.a[2*ii] = float32(a*x0 + b*y0 + e)
		p.a[2*ii+1] = float32(c*x0 + d*y0 + f)
	}
	return geoBlobResult(p), nil
}

// geopolySine is geopoly.c's fast polynomial sine approximation for
// -0.5*pi <= r <= 2*pi.
func geopolySine(r float64) float64 {
	if r >= 1.5*GEOPOLY_PI {
		r -= 2.0 * GEOPOLY_PI
	}
	if r >= 0.5*GEOPOLY_PI {
		return -geopolySine(r - GEOPOLY_PI)
	}
	r2 := r * r
	r3 := r2 * r
	r5 := r3 * r2
	return 0.9996949*r - 0.1656700*r3 + 0.0075134*r5
}

// geopolyRegularFn implements geopoly_regular(X,Y,R,N): a convex regular
// N-gon centered at (X,Y) with circumradius R; NULL for N<3 or R<=0, with N
// clamped at 1000.
func geopolyRegularFn(args []interface{}) (interface{}, error) {
	x := geoValueDouble(util.UnwrapColumnValue(args[0]))
	y := geoValueDouble(util.UnwrapColumnValue(args[1]))
	r := geoValueDouble(util.UnwrapColumnValue(args[2]))
	n := geoValueInt(util.UnwrapColumnValue(args[3]))
	if n < 3 || r <= 0.0 {
		return nil, nil
	}
	if n > 1000 {
		n = 1000
	}
	p := &geoPoly{nVertex: n, a: make([]float32, 2*n)}
	for i := 0; i < n; i++ {
		rAngle := 2.0 * GEOPOLY_PI * float64(i) / float64(n)
		p.a[2*i] = float32(x - r*geopolySine(rAngle-0.5*GEOPOLY_PI))
		p.a[2*i+1] = float32(y + r*geopolySine(rAngle))
	}
	return geoBlobResult(p), nil
}

// geopolyCcwFn implements geopoly_ccw(X): the polygon with a
// counter-clockwise winding (clockwise inputs are reversed), as a blob.
func geopolyCcwFn(args []interface{}) (interface{}, error) {
	p, _ := geoParam(args, 0)
	if p == nil {
		return nil, nil
	}
	if geopolyArea(p) < 0.0 {
		for ii, jj := 1, p.nVertex-1; ii < jj; ii, jj = ii+1, jj-1 {
			p.a[2*ii], p.a[2*jj] = p.a[2*jj], p.a[2*ii]
			p.a[2*ii+1], p.a[2*jj+1] = p.a[2*jj+1], p.a[2*ii+1]
		}
	}
	return geoBlobResult(p), nil
}

// ---- aggregate ----

// geopolyGroupBBox is the geopoly_group_bbox(X) aggregate state (GeoBBox):
// running min/max over every row's bounding box; rows whose _shape has an
// unusable TYPE are skipped, while rows whose _shape merely fails to parse
// contribute an all-zero box (geopolyBBox's rc==SQLITE_OK path).
type geopolyGroupBBox struct {
	isInit bool
	a      [4]float32
}

// Step implements vtab.Aggregator (geopolyBBoxStep).
func (g *geopolyGroupBBox) Step(args []interface{}) error {
	poly, rc := geoParam(args, 0)
	if rc != geoOK {
		return nil
	}
	c := geopolyBBoxOf(poly)
	if !g.isInit {
		g.isInit = true
		g.a = c
		return nil
	}
	if c[0] < g.a[0] {
		g.a[0] = c[0]
	}
	if c[1] > g.a[1] {
		g.a[1] = c[1]
	}
	if c[2] < g.a[2] {
		g.a[2] = c[2]
	}
	if c[3] > g.a[3] {
		g.a[3] = c[3]
	}
	return nil
}

// Final implements vtab.Aggregator (geopolyBBoxFinal): the combined bbox as
// a polygon blob, NULL when no row initialized the accumulator.
func (g *geopolyGroupBBox) Final() (interface{}, error) {
	if !g.isInit {
		return nil, nil
	}
	return geopolyBBoxPoly(g.a), nil
}
