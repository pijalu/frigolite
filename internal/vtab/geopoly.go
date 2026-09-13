package vtab

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/pijalu/frigolite/internal/value"
)

// This file is the core of the geopoly port (ext/rtree/geopoly.c, #include-ed
// onto rtree.c under SQLITE_ENABLE_GEOPOLY): the GeoPoly value type with its
// GeoJSON-text and on-disk-blob representations (geopolyParseJson,
// geopolyFuncParam), the bounding-box derivation used both by geopoly_bbox()
// and by xUpdate's cell construction (geopolyBBox), and the "geopoly" virtual
// table module — an rtree (RTREE_COORD_REAL32, nDim=2) subclass whose user
// columns are ALL auxiliary (a0 = the hidden "_shape" polygon, a1.. = the
// declared user columns), exactly as geopolyInit wires them.

// geopolyRC mirrors the two SQLITE_* result classes geopolyFuncParam
// distinguishes for its callers: SQLITE_OK (input recognized — a NULL poly
// with rc==OK means "not a polygon, but not an error either": geopolyBBox
// stores an all-zero cell) and SQLITE_ERROR (input of an unusable TYPE —
// NULL / short blob / non-text scalar — which xUpdate renders as
// "_shape does not contain a valid polygon").
const (
	geoOK    = 0
	geoError = 1
)

// geoPoly is the internal polygon: nVertex vertexes, X then Y per vertex
// (GeoCoord = float32). There is a segment between consecutive vertexes and
// one closing the last vertex back to the first (GeoJSON's repeated first
// vertex is NOT stored).
type geoPoly struct {
	nVertex int
	a       []float32
}

// geoX / geoY widen one coordinate to float64 for the C code's double-precision
// arithmetic (every geopoly computation promotes GeoCoord to double).
func geoX(p *geoPoly, i int) float64 { return float64(p.a[2*i]) }
func geoY(p *geoPoly, i int) float64 { return float64(p.a[2*i+1]) }

// ---- GeoJSON text parsing (geopolyParseJson / geopolyParseNumber) ----

// geopolyIsSpace is geopoly.c's fast_isspace table: only \t \n \r and space
// count as whitespace (\v and \f do NOT).
var geopolyIsSpace = [256]byte{
	'\t': 1, '\n': 1, '\r': 1, ' ': 1,
}

// geoParse is the state of one GeoJSON parse (GeoParse).
type geoParse struct {
	z       string
	pos     int
	nVertex int
	nErr    int
	a       []float32
}

// skipSpace advances past whitespace and returns the next byte (0 at end of
// input — geopolySkipSpace's NUL terminator).
func (p *geoParse) skipSpace() byte {
	for p.pos < len(p.z) && geopolyIsSpace[p.z[p.pos]] == 1 {
		p.pos++
	}
	if p.pos >= len(p.z) {
		return 0
	}
	return p.z[p.pos]
}

// geoZat reads z[i] with C's out-of-bounds behavior approximated: a read at
// index -1 (the '.'/'e' branches read z[j-1] before any digit) sees the byte
// that ordinarily precedes the number inside "[...]", i.e. '['; a read at or
// past the end sees the NUL terminator.
func geoZat(z string, i int) byte {
	switch {
	case i < 0:
		return '['
	case i >= len(z):
		return 0
	default:
		return z[i]
	}
}

// geopolyParseNumber ports geopolyParseNumber: it scans one JSON number at
// the parse cursor, storing its float32 value into pVal when pVal is non-nil
// (values past the first two of a vertex are validated but discarded), and
// advancing the cursor past the number. It returns 1 on success and 0 when
// the next token is not a number (the C code's -1 failure return is
// equivalent for every caller: the parse aborts with a NULL polygon either
// way).
func geopolyParseNumber(p *geoParse, pVal *float32) int {
	// C advances the cursor past whitespace (geopolySkipSpace) BEFORE
	// capturing z = p->z, so the scan window starts at the number itself.
	c := p.skipSpace()
	z := p.z[p.pos:]
	j := 0
	if c == '-' {
		j = 1
	}
	if geoJSONLeadingZero(z, j) {
		return 0 // JSON leading-zero rule ("00" is not a number)
	}
	end, rc := geoScanNumberBody(z, j)
	if rc != 1 {
		return rc
	}
	return geoCommitNumber(z, end, p, pVal)
}

// geoJSONLeadingZero reports the "0 followed by a digit" rejection of
// geopolyParseNumber.
func geoJSONLeadingZero(z string, j int) bool {
	return geoZat(z, j) == '0' && j+1 < len(z) && z[j+1] >= '0' && z[j+1] <= '9'
}

// geoScanNumberBody walks the digits/'.'/'e' portions of a JSON number from
// index j (geopolyParseNumber's main loop). It returns the end index with
// rc=1 on success, rc=0 for a malformed number and rc=-1 for the C code's
// double-exponent return (equivalent to a parse abort for every caller).
func geoScanNumberBody(z string, j int) (end, rc int) {
	seenDP := false
	seenE := false
	for ; ; j++ {
		c := geoZat(z, j)
		var twice, ok bool
		switch {
		case c >= '0' && c <= '9':
			continue
		case c == '.':
			j, ok = geoScanDot(z, j, seenDP)
		case c == 'e' || c == 'E':
			j, twice, ok = geoScanExponent(z, j, seenE)
			seenE = true
		default:
			return j, 1
		}
		if !ok {
			return j, 0
		}
		if twice {
			return j, -1
		}
		seenDP = true
	}
}

// geoScanDot validates a '.' at z[j] (geopolyParseNumber's '.' branch).
func geoScanDot(z string, j int, seenDP bool) (int, bool) {
	if geoZat(z, j-1) == '-' || seenDP {
		return j, false
	}
	return j, true
}

// geoCommitNumber performs the tail of geopolyParseNumber: the final
// "previous byte was a digit" guard, the value conversion and the cursor
// advance.
func geoCommitNumber(z string, j int, p *geoParse, pVal *float32) int {
	if geoZat(z, j-1) < '0' {
		return 0
	}
	if pVal != nil {
		// atof/sqlite3AtoF semantics: the scanned prefix is a valid decimal
		// number; overflow yields ±Inf which ParseFloat reports as ErrRange
		// together with the saturated value — keep it, like atof.
		f, _ := strconv.ParseFloat(string(z[:j]), 64)
		*pVal = float32(f)
	}
	p.pos += j
	return 1
}

// geoScanExponent handles the 'e'/'E' branch of geopolyParseNumber: on a
// well-formed exponent it returns the j position to continue from (twice is
// false); a second exponent sets twice (the C code's -1 return); a malformed
// one reports ok=false.
func geoScanExponent(z string, j int, seenE bool) (adv int, twice, ok bool) {
	if geoZat(z, j-1) < '0' {
		return 0, false, false
	}
	if seenE {
		return 0, true, true
	}
	c := geoZat(z, j+1)
	if c == '+' || c == '-' {
		j++
		c = geoZat(z, j+1)
	}
	if c < '0' || c > '9' {
		return 0, false, false
	}
	return j, false, true
}

// geopolyParseJSON ports geopolyParseJson: it converts a well-formed GeoJSON
// array of at least four [x,y] pairs whose first and last vertexes coincide
// into a geoPoly. Any other input returns (nil, rc) where rc distinguishes
// the caller-visible error classes (see geopolyRC).
func geopolyParseJSON(z string) (*geoPoly, int) {
	s := &geoParse{z: z}
	if s.skipSpace() != '[' {
		// C jumps straight to parse_json_err without touching rc: a
		// non-'[' TEXT input is "not a polygon" but NOT an error.
		return nil, geoOK
	}
	s.pos++
	for s.skipSpace() == '[' {
		s.pos++
		if !s.parseVertexList() {
			return nil, geoError
		}
		if s.skipSpace() == ',' {
			s.pos++
			continue
		}
		break
	}
	if !s.closedPolygon() {
		s.nErr++
		return nil, geoError
	}
	s.pos++
	if s.skipSpace() != 0 {
		s.nErr++
		return nil, geoError
	}
	s.nVertex-- // remove the redundant closing vertex
	out := &geoPoly{nVertex: s.nVertex, a: append([]float32(nil), s.a[:s.nVertex*2]...)}
	return out, geoOK
}

// parseVertexList scans one "[num, num(, num...)]" vertex list (the inner
// while loop of geopolyParseJson). The first two numbers of a vertex are
// stored; extras are validated but discarded. It reports false on a
// malformed list (the C code's error goto).
func (p *geoParse) parseVertexList() bool {
	ii := 0
	for {
		var pVal *float32
		if ii <= 1 {
			need := p.nVertex*2 + ii + 1
			for len(p.a) < need {
				p.a = append(p.a, 0)
			}
			pVal = &p.a[p.nVertex*2+ii]
		}
		if geopolyParseNumber(p, pVal) == 0 {
			break
		}
		ii++
		if ii == 2 {
			p.nVertex++
		}
		c := p.skipSpace()
		p.pos++
		if c == ',' {
			continue
		}
		if c == ']' && ii >= 2 {
			return true
		}
		p.nErr++
		return false
	}
	return p.skipSpace() == ','
}

// closedPolygon mirrors the C closure check: the ']' terminator followed by
// at least four vertexes whose first and last coordinate pairs coincide.
func (p *geoParse) closedPolygon() bool {
	return p.skipSpace() == ']' && p.nVertex >= 4 &&
		len(p.a) >= p.nVertex*2 &&
		p.a[0] == p.a[p.nVertex*2-2] &&
		p.a[1] == p.a[p.nVertex*2-1]
}

// ---- input coercion (geopolyFuncParam) ----

// geopolyFuncParam interprets one SQL value as a polygon: BLOB inputs use the
// on-disk format (4-byte header: encoding flag + 3-byte big-endian vertex
// count, then float32 coordinates in the flagged byte order), TEXT inputs go
// through geopolyParseJSON, and anything else — including NULL, short blobs
// and a TEXT payload rejected by the parser — yields a NULL polygon. The rc
// result follows geopolyFuncParam exactly: only the unparseable-TYPE paths
// report geoError; a malformed BLOB or malformed GeoJSON text leaves rc at
// geoOK so callers like geopolyBBoxStep still record an all-zero box.
func geopolyFuncParam(v interface{}) (*geoPoly, int) {
	switch x := v.(type) {
	case []byte:
		if len(x) >= 4+6*4 {
			return geopolyFromBlob(x)
		}
		return nil, geoError
	case value.ZeroBlob:
		b := x.Bytes()
		if len(b) >= 4+6*4 {
			return geopolyFromBlob(b)
		}
		return nil, geoError
	case string:
		return geopolyParseJSON(x)
	default:
		return nil, geoError
	}
}

// geopolyFromBlob decodes the on-disk representation. A header/size mismatch
// yields (nil, geoOK) — the C blob branch always reports SQLITE_OK.
func geopolyFromBlob(b []byte) (*geoPoly, int) {
	nVertex := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if (b[0] == 0 || b[0] == 1) && nVertex*2*4+4 == len(b) {
		p := &geoPoly{nVertex: nVertex, a: make([]float32, nVertex*2)}
		for i := 0; i < nVertex*2; i++ {
			off := 4 + i*4
			// Encoding 1 = little-endian coordinates, 0 = big-endian (the
			// C code byte-swaps when the flag disagrees with the host).
			var u uint32
			if b[0] == 1 {
				u = binary.LittleEndian.Uint32(b[off:])
			} else {
				u = binary.BigEndian.Uint32(b[off:])
			}
			p.a[i] = math.Float32frombits(u)
		}
		return p, geoOK
	}
	return nil, geoOK
}

// encodeBlob renders the polygon in the canonical on-disk format (encoding
// flag 1, little-endian coordinates — geopolyRegularFunc/geopolyCcwFunc's
// result_blob output).
func (p *geoPoly) encodeBlob() []byte {
	out := make([]byte, 4+p.nVertex*8)
	out[0] = 1
	out[1] = byte(p.nVertex >> 16)
	out[2] = byte(p.nVertex >> 8)
	out[3] = byte(p.nVertex)
	for i, c := range p.a {
		binary.LittleEndian.PutUint32(out[4+i*4:], math.Float32bits(c))
	}
	return out
}

// ---- bounding box (geopolyBBox) ----

// geopolyBBoxOf computes the bounding box as the 4-cell coordinate vector
// [minX,maxX,minY,maxY] stored in the r-tree cell. Comparisons run in
// double precision while the stored extrema stay float32, per geopolyBBox.
// A nil polygon yields the all-zero box (geopolyBBox's memset path).
func geopolyBBoxOf(p *geoPoly) [4]float32 {
	if p == nil || p.nVertex < 1 {
		return [4]float32{}
	}
	mnX, mxX := float64(p.a[0]), float64(p.a[0])
	mnY, mxY := float64(p.a[1]), float64(p.a[1])
	for ii := 1; ii < p.nVertex; ii++ {
		r := geoX(p, ii)
		if r < mnX {
			mnX = r
		} else if r > mxX {
			mxX = r
		}
		r = geoY(p, ii)
		if r < mnY {
			mnY = r
		} else if r > mxY {
			mxY = r
		}
	}
	return [4]float32{float32(mnX), float32(mxX), float32(mnY), float32(mxY)}
}

// geopolyBBoxPoly builds the 4-vertex bbox polygon blob (geopoly_bbox's
// result): (mnX,mnY),(mxX,mnY),(mxX,mxY),(mnX,mxY).
func geopolyBBoxPoly(c [4]float32) []byte {
	p := &geoPoly{nVertex: 4, a: []float32{
		c[0], c[2], c[1], c[2], c[1], c[3], c[0], c[3],
	}}
	return p.encodeBlob()
}
