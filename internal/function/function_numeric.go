package function

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
)

// percentileAgg implements the ordered-set percentile aggregates
// (SQLite ext/misc/percentile.c):
//   - percentile(Y,P)      P in [0,100], continuous
//   - percentile_cont(Y,P) P in [0,1], continuous
//   - percentile_disc(Y,P) P in [0,1], discrete
//   - median(Y)            == percentile(Y,50)
//
// Semantics (from percentile.c):
//   - The fraction argument must be numeric and within range: the error is
//     "the fraction argument to <fn>() is not between 0.0 and <mxFrac>" where
//     mxFrac is 100.0 for percentile() and 1.0 for the others.
//   - Every row must supply the same fraction (within an absolute 0.001
//     tolerance in the normalized 0..1 space); otherwise "the fraction
//     argument to <fn>() is not the same for all input rows".
//   - The result is ix = rPct*(n-1); continuous interpolates between the
//     floor and ceil entries, discrete takes the floor entry. Always a REAL.
type percentileAgg struct {
	discrete bool // percentile_disc: nearest-rank (no interpolation)
	pct100   bool // percentile(): fraction is in [0,100], normalized by /100
	name     string
	rPct     float64
	rPctSet  bool
	values   []float64
}

func newPercentileAgg(discrete, pct100 bool) Aggregator {
	name := "percentile()"
	switch {
	case pct100:
		name = "percentile()"
	case discrete:
		name = "percentile_disc()"
	default:
		name = "percentile_cont()"
	}
	return &percentileAgg{discrete: discrete, pct100: pct100, name: name}
}

// percentileFracUpper returns the upper bound of the fraction argument's range.
func (p *percentileAgg) fracUpper() float64 {
	if p.pct100 {
		return 100.0
	}
	return 1.0
}

// setFraction validates and stores the fraction argument (or the fixed 0.5
// for median). It reports the "not between" / "not the same" errors exactly
// as SQLite's percentile.c percentStep.
func (p *percentileAgg) setFraction(args []interface{}) error {
	mxFrac := p.fracUpper()
	if len(args) > 1 {
		rPct, ok := numericFraction(args[1])
		if !ok {
			return fmt.Errorf("the fraction argument to %s is not between 0.0 and %.1f", p.name, mxFrac)
		}
		rPct /= mxFrac
		if rPct < 0.0 || rPct > 1.0 {
			return fmt.Errorf("the fraction argument to %s is not between 0.0 and %.1f", p.name, mxFrac)
		}
		if !p.rPctSet {
			p.rPct = rPct
			p.rPctSet = true
		} else if !percentSameValue(p.rPct, rPct) {
			return fmt.Errorf("the fraction argument to %s is not the same for all input rows", p.name)
		}
	} else {
		// median(Y): fraction is fixed at 0.5 (requirement 13).
		if !p.rPctSet {
			p.rPct = 0.5
			p.rPctSet = true
		}
	}
	return nil
}

func (p *percentileAgg) Step(args []interface{}) error {
	if err := p.setFraction(args); err != nil {
		return err
	}
	if len(args) == 0 || args[0] == nil {
		return nil // NULL Y is ignored
	}
	f, ok := numericValue(args[0])
	if !ok {
		return fmt.Errorf("input to %s is not numeric", p.name)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("inf input to %s", p.name)
	}
	p.values = append(p.values, f)
	return nil
}

func (p *percentileAgg) Final() (interface{}, error) {
	if len(p.values) == 0 {
		return nil, nil // no non-NULL entries -> NULL
	}
	sort.Float64s(p.values)
	n := len(p.values)
	ix := p.rPct * float64(n-1)
	i1 := int(ix)
	if i1 >= n {
		i1 = n - 1
	}
	if p.discrete {
		return p.values[i1], nil
	}
	i2 := i1
	if ix != float64(i1) && i1 != n-1 {
		i2 = i1 + 1
	}
	v1 := p.values[i1]
	v2 := p.values[i2]
	return v1 + (v2-v1)*(ix-float64(i1)), nil
}

// numericFraction converts a fraction argument to a float64, reporting whether
// it is numeric. SQLite's percentStep uses sqlite3_value_numeric_type, which
// accepts INTEGER and REAL and converts numeric-looking TEXT ('50' → 50);
// non-numeric text and blobs yield the "not between" error.
func numericFraction(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// numericValue converts a Y argument to a float64, reporting whether it is
// numeric. SQLite's percentile accepts INTEGER and REAL; TEXT/BLOB yield the
// "not numeric" error even if they look numeric.
func numericValue(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// percentSameValue mirrors percentile.c's percentSameValue: two doubles are
// "the same" when they differ by 0.001 or less.
func percentSameValue(a, b float64) bool {
	d := a - b
	return d >= -0.001 && d <= 0.001
}

func fnABS(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		if v < 0 {
			// ABS of the minimum int64 overflows (SQLite: "integer overflow").
			if v == math.MinInt64 {
				return nil, fmt.Errorf("integer overflow")
			}
			return -v, nil
		}
		return v, nil
	case float64:
		return math.Abs(v), nil
	default:
		return 0, nil
	}
}

func fnROUND(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// A NULL digits argument yields NULL (SQLite roundFunc returns on a
	// NULL second argument before converting the value).
	if len(args) > 1 && args[1] == nil {
		return nil, nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return args[0], nil
	}
	places := 0
	if len(args) > 1 {
		places = int(toInt64(args[1]))
	}
	// SQLite's roundFunc formats with %.*f; a negative precision is treated
	// as 0 by printf, so round(x, n<0) behaves like round(x, 0). The digits
	// argument is also clamped to [-30, 30] (beyond that %.*f saturates).
	if places < 0 {
		places = 0
	}
	if places > 30 {
		places = 30
	}
	pow := math.Pow(10, float64(places))
	return math.Round(f*pow) / pow, nil
}

func fnRANDOM(args []interface{}) (interface{}, error) {
	return int64(rand.Int63()), nil
}

func fnRANDOMBLOB(args []interface{}) (interface{}, error) {
	n := int(toInt64(args[0]))
	if n <= 0 {
		return []byte{}, nil
	}
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		buf[i] = byte(rand.Intn(256))
	}
	return buf, nil
}

func fnRANDSTR(args []interface{}) (interface{}, error) {
	n := int(toInt64(args[0]))
	if n <= 0 {
		return "", nil
	}
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		buf[i] = chars[rand.Intn(len(chars))]
	}
	return string(buf), nil
}

func fnZEROBLOB(args []interface{}) (interface{}, error) {
	n := int(toInt64(args[0]))
	// SQLite zeroblob(): a negative length is the empty blob; a length that
	// does not fit the 32-bit MEM_Zero field raises "string or blob too
	// big" instead of allocating (oracle 3.51 accepts lengths above
	// SQLITE_MAX_LENGTH because MEM_Zero payloads are never materialized,
	// but rejects anything >= 1<<31 — zeroblob.test 6.4/11.x).
	if n <= 0 {
		return []byte{}, nil
	}
	if n >= 1<<31 {
		return nil, fmt.Errorf("string or blob too big")
	}
	// Lazy zero blob (SQLite MEM_Zero): content stays unmaterialized until
	// needed; see internal/value/zeroblob.go.
	return value.ZeroBlob{N: n}, nil
}

// fnTestZeroblob is the TCL test-harness test_zeroblob(N): like zeroblob but
// without the SQLITE_MAX_LENGTH check (test1.c test_zeroblob). Negative
// lengths return the empty blob.
func fnTestZeroblob(args []interface{}) (interface{}, error) {
	n := int(toInt64(args[0]))
	if n <= 0 {
		return []byte{}, nil
	}
	return value.ZeroBlob{N: n}, nil
}

func fnLIKELIHOOD(args []interface{}) (interface{}, error) {
	return args[0], nil
}

func fnLIKELY(args []interface{}) (interface{}, error) {
	return args[0], nil
}

func fnUNLIKELY(args []interface{}) (interface{}, error) {
	return args[0], nil
}

func fnTYPEOF(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return "null", nil
	}
	switch args[0].(type) {
	case int64:
		return "integer", nil
	case float64:
		return "real", nil
	case string:
		return "text", nil
	case []byte:
		return "blob", nil
	case value.ZeroBlob:
		return "blob", nil
	default:
		return "text", nil
	}
}

// fnAFFINITY implements the test-only affinity() function from the SQLite TCL
// test suite. It reports the affinity of the column its argument refers to
// (integer/real/text/blob/none): a ColumnValue wrapper carries the declared
// column affinity (from a table scan or materialized subquery/CTE column),
// while a bare value reports its storage class (the fallback for literals).
func fnAFFINITY(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return "none", nil
	}
	// A ColumnValue wrapper carries the declared column affinity; report it
	// even when the stored value has a different storage class (SQLite's
	// affinity() reports the column affinity, not the value type).
	if cv, ok := args[0].(*util.ColumnValue); ok {
		switch cv.Affinity {
		case 'I':
			return "integer", nil
		case 'R':
			return "real", nil
		case 'T':
			return "text", nil
		case 'N':
			return "numeric", nil
		default:
			return "blob", nil
		}
	}
	switch args[0].(type) {
	case int64:
		return "integer", nil
	case float64:
		return "real", nil
	case string:
		return "text", nil
	case []byte:
		return "blob", nil
	default:
		return "none", nil
	}
}

func fnTOINTEGER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return v, nil
	case float64:
		if i, ok := intFromReal(v); ok {
			return i, nil
		}
		return nil, nil
	case string:
		if i, ok := intFromText(v); ok {
			return i, nil
		}
		return nil, nil
	case []byte:
		// SQLite's tointeger() reads an 8-byte BLOB as the little-endian
		// bytes of an integer (func4-6.2: x'0102030405060708' →
		// 0x0807060504030201). Blobs of any other length return NULL
		// (func4-6.1: tointeger(x'01') is NULL).
		if len(v) == 8 {
			return int64(binary.LittleEndian.Uint64(v)), nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// intFromReal converts a REAL to int64, reporting false when the value is
// outside the int64 range (r<=-2^63 or r>=2^63) or NOT an integer
// (1234.56 → NULL; only integral REALs like 1234.0 convert). NaN/Inf → NULL.
func intFromReal(v float64) (int64, bool) {
	if v != math.Trunc(v) || v <= math.MinInt64 || v >= -math.MinInt64 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return int64(v), true
}

// intFromText converts TEXT to int64 for tointeger(): strict integer first,
// then a REAL that is exactly integral (SQLite's tointeger accepts "1234" and
// "1234.0" but not "1234.56" or " 1234").
func intFromText(s string) (int64, bool) {
	if i, err := parseInt64(s); err == nil {
		return i, true
	}
	if f, err := parseFloat64(s); err == nil {
		return intFromReal(f)
	}
	return 0, false
}

func parseInt64(s string) (int64, error) {
	// SQLite's tointeger() is strict: no leading/trailing whitespace, no
	// trailing garbage, no sign-separating spaces. Go's strconv.ParseInt
	// accepts a leading '+'/'-' and rejects everything else.
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return i, nil
}

func parseFloat64(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

func mathOneArg(args []interface{}, fn func(float64) float64) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite's math functions only accept numeric input (math1Func checks
	// sqlite3_value_numeric_type); non-numeric text yields NULL, not 0.
	f, ok := numericArg(args[0])
	if !ok {
		return nil, nil
	}
	r := fn(f)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

// numericArg converts an argument to a float64, reporting whether it is
// numeric (INTEGER, REAL, or numeric-looking TEXT — sqlite3_value_numeric_type
// semantics used by SQLite's math functions). Non-numeric text/blobs return
// false so the caller yields NULL.
func numericArg(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// mathLogArg evaluates the log-family functions, which additionally return
// NULL for x<=0 (SQLite: log of zero or a negative number is NULL, not -Inf
// or NaN).
func mathLogArg(args []interface{}, fn func(float64) float64) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	f, ok := numericArg(args[0])
	if !ok {
		return nil, nil
	}
	if f <= 0 {
		return nil, nil
	}
	r := fn(f)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

// mathRoundArg evaluates floor/ceil, which SQLite returns as INTEGER when the
// input was INTEGER (floor(17) → 17) and REAL otherwise (floor(17.5) → 17.0,
// ceil(99.9) → 100.0, ceil('-99.99') → -99.0).
func mathRoundArg(args []interface{}, fn func(float64) float64) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	_, isInt := args[0].(int64)
	f, ok := numericArg(args[0])
	if !ok {
		return nil, nil
	}
	r := fn(f)
	if math.IsNaN(r) {
		return nil, nil
	}
	if isInt && r == math.Trunc(r) && r >= -9.223372036854776e18 && r < 9.223372036854776e18 {
		return int64(r), nil
	}
	return r, nil
}

func fnACOS(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Acos)
}

func fnACOSH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Acosh)
}

func fnASIN(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Asin)
}

func fnASINH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Asinh)
}

func fnATAN(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Atan)
}

func fnATAN2(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	r := math.Atan2(f1, f2)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

func fnCEIL(args []interface{}) (interface{}, error) {
	return mathRoundArg(args, math.Ceil)
}

func fnCOS(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Cos)
}

func fnCOSH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Cosh)
}

func fnDEGREES(args []interface{}) (interface{}, error) {
	return mathOneArg(args, func(f float64) float64 { return f * 180.0 / math.Pi })
}

func fnEXP(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Exp)
}

func fnFLOOR(args []interface{}) (interface{}, error) {
	return mathRoundArg(args, math.Floor)
}

func fnLN(args []interface{}) (interface{}, error) {
	return mathLogArg(args, math.Log)
}

func fnLOG(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) >= 2 {
		if args[1] == nil {
			return nil, nil
		}
		// SQLite log(B, X): base-B logarithm of X = ln(X)/ln(B).
		base, err1 := toFloat64(args[0])
		x, err2 := toFloat64(args[1])
		if err1 != nil || err2 != nil {
			return nil, nil
		}
		// SQLite log(B, X): NULL when X<=0, B<=0, or B==1 (log base 1 is
		// undefined; the result is NaN or +-Inf).
		if x <= 0 || base <= 0 || base == 1 {
			return nil, nil
		}
		r := math.Log(x) / math.Log(base)
		if math.IsNaN(r) {
			return nil, nil
		}
		return r, nil
	}
	return mathLogArg(args, math.Log10)
}

func fnLOG10(args []interface{}) (interface{}, error) {
	return mathLogArg(args, math.Log10)
}

func fnLOG2(args []interface{}) (interface{}, error) {
	return mathLogArg(args, math.Log2)
}

func fnMOD(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	a, err1 := toFloat64(args[0])
	b, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil || b == 0 {
		return nil, nil
	}
	r := math.Mod(a, b)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

func fnPI(args []interface{}) (interface{}, error) {
	return math.Pi, nil
}

func fnPOW(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	r := math.Pow(f1, f2)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

func fnRADIANS(args []interface{}) (interface{}, error) {
	// radians/degrees convert exactly (no result rounding): the downstream
	// trig function applies SQLite-style rounding, and rounding the angle
	// would change the result (cos(radians(60)) must see the precise angle).
	if args[0] == nil {
		return nil, nil
	}
	f, ok := numericArg(args[0])
	if !ok {
		return nil, nil
	}
	return f * math.Pi / 180.0, nil
}

func fnSIGN(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		if v > 0 {
			return int64(1), nil
		}
		if v < 0 {
			return int64(-1), nil
		}
		return int64(0), nil
	case float64:
		if v > 0 {
			return int64(1), nil
		}
		if v < 0 {
			return int64(-1), nil
		}
		return int64(0), nil
	default:
		return nil, nil
	}
}

func fnSIN(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Sin)
}

func fnSINH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Sinh)
}

func fnSQRT(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Sqrt)
}

func fnTAN(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Tan)
}

func fnTANH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Tanh)
}

func fnATANH(args []interface{}) (interface{}, error) {
	return mathOneArg(args, math.Atanh)
}

func fnTRUNC(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	if len(args) >= 2 {
		digits, err2 := toFloat64(args[1])
		if err2 != nil {
			return nil, nil
		}
		pow := math.Pow(10, digits)
		r := math.Trunc(f*pow) / pow
		if math.IsNaN(r) {
			return nil, nil
		}
		return r, nil
	}
	if f >= 0 {
		return math.Floor(f), nil
	}
	return math.Ceil(f), nil
}

func fnTOREAL(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite's toreal(): INTEGER converts to REAL only when the value is
	// exactly representable as a double (round-trip (int64)(double)i == i);
	// otherwise NULL. MinInt64 is always rejected (the C cast of -2^63 is
	// out of range). REAL passes through (incl. Inf), TEXT must be a strict
	// decimal; anything else is NULL.
	switch v := args[0].(type) {
	case int64:
		if v == math.MinInt64 {
			return nil, nil
		}
		r := float64(v)
		// Go's int64(float) saturates at MaxInt64, so the round-trip check
		// must also reject values that round UP to >= 2^63 (SQLite's C cast
		// wraps them, and toreal returns NULL when the conversion is lossy).
		if int64(r) != v || r >= 9223372036854775808.0 {
			return nil, nil
		}
		return r, nil
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, nil
		}
		return f, nil
	case []byte:
		// SQLite's toreal() reads an 8-byte BLOB as the BIG-endian bytes of
		// an IEEE754 double (func4-6.3: x'ffefffffffffffff' →
		// -1.7976931348623157e+308, bits 0xffefffffffffffff read big-endian).
		// NaN bit patterns yield NULL (func4-6.3.18: x'fff0000000000001').
		if len(v) == 8 {
			r := math.Float64frombits(binary.BigEndian.Uint64(v))
			if math.IsNaN(r) {
				return nil, nil
			}
			return r, nil
		}
		return nil, nil
	}
	return nil, nil
}

func fnTOCHAR(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		if v >= 0 && v < 256 {
			return string([]byte{byte(v)}), nil
		}
	case float64:
		if v >= 0 && v < 256 {
			return string([]byte{byte(int64(v))}), nil
		}
	}
	return nil, nil
}

func fnTOBLOB(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return args[0], nil
}

func fnTOHEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%X", v), nil
	case string:
		return fmt.Sprintf("%X", v), nil
	case []byte:
		return fmt.Sprintf("%X", v), nil
	default:
		return fmt.Sprintf("%X", v), nil
	}
}

// fnAddTextType forces its argument to the TEXT storage class
// (sqlite3_value_text in the SQLite test harness). NULL passes through.
func fnAddTextType(args []interface{}) (interface{}, error) {
	v := args[0]
	if v == nil {
		return nil, nil
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		// SQLite renders whole-number REALs with a trailing .0.
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s, nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// fnAddIntType forces its argument to the INTEGER storage class
// (sqlite3_value_int64 in the SQLite test harness). NULL passes through.
func fnAddIntType(args []interface{}) (interface{}, error) {
	v := args[0]
	if v == nil {
		return nil, nil
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	case string:
		if i, ok := parseIntPrefix(strings.TrimSpace(x)); ok {
			return i, nil
		}
		return int64(0), nil
	default:
		return int64(0), nil
	}
}

// parseIntPrefix parses the leading integer of a string (optional sign plus
// digits), mirroring SQLite's sqlite3Atoi64 fallback used by the test-harness
// value coercion: overflow saturates to Min/MaxInt64, no digits yields 0.
func parseIntPrefix(t string) (int64, bool) {
	end := 0
	if end < len(t) && (t[end] == '+' || t[end] == '-') {
		end++
	}
	for end < len(t) && t[end] >= '0' && t[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if i, err := strconv.ParseInt(t[:end], 10, 64); err == nil {
		return i, true
	}
	if t[0] == '-' {
		return int64(math.MinInt64), true
	}
	return int64(math.MaxInt64), true
}

// fnAddRealType forces its argument to the REAL storage class
// (sqlite3_value_double in the SQLite test harness). NULL passes through.
func fnAddRealType(args []interface{}) (interface{}, error) {
	v := args[0]
	if v == nil {
		return nil, nil
	}
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case string:
		t := strings.TrimSpace(x)
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f, nil
		}
		return float64(0), nil
	default:
		return float64(0), nil
	}
}

// fnIntReal implements the test-only intreal() function from the SQLite TCL
// test suite. It forces its argument to the REAL storage class while keeping
// the integer value (SQLite's MEM_IntReal): the value renders as "N.0",
// typeof() reports "real", and comparisons treat it numerically as N.
// NULL passes through.
func fnIntReal(args []interface{}) (interface{}, error) {
	return fnAddRealType(args)
}
