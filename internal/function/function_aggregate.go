package function

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// --- Aggregate implementations ---

type countAgg struct {
	count int64
}

func (c *countAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] != nil {
		c.count++
	}
	return nil
}

func (c *countAgg) Final() (interface{}, error) {
	return c.count, nil
}

// sumAgg ports func.c SumCtx + sumStep/sumFinalize: an exact int64 running
// sum that, on int64 overflow or first non-integer input, switches to a
// Kahan-Babuška-Neumaier compensated double sum. SUM() only raises
// "integer overflow" at finalize time when the overflow was never absorbed
// by a later non-integer input (func.c: the non-integer branch clears
// ovrfl); otherwise it yields the double.
type sumAgg struct {
	intSum   int64
	floatSum float64 // KBN running sum (rSum)
	rErr     float64 // KBN compensation term (rErr)
	count    int64
	isFloat  bool // approx: switched to the compensated double sum
	ovrfl    bool // int64 overflow seen and not yet absorbed (func.c ovrfl)
}

// kahanBabuskaNeumaierStep adds r to the running compensated sum
// (func.c kahanBabuskaNeumaierStep).
func (s *sumAgg) kahanBabuskaNeumaierStep(r float64) {
	fl := s.floatSum
	t := fl + r
	if absF(fl) > absF(r) {
		s.rErr += (fl - t) + r
	} else {
		s.rErr += (r - t) + fl
	}
	s.floatSum = t
}

// kahanBabuskaNeumaierStepInt64 adds a possibly large int64, splitting values
// beyond the exact-double range into big+small parts
// (func.c kahanBabuskaNeumaierStepInt64).
func (s *sumAgg) kahanBabuskaNeumaierStepInt64(v int64) {
	const maxExact = 4503599627370496 // 2^52
	if v <= -maxExact || v >= maxExact {
		iSm := v % 16384
		iBig := v - iSm
		s.kahanBabuskaNeumaierStep(float64(iBig))
		s.kahanBabuskaNeumaierStep(float64(iSm))
	} else {
		s.kahanBabuskaNeumaierStep(float64(v))
	}
}

// kahanBabuskaNeumaierInit reseeds the compensated sum from the current
// int64 accumulator (func.c kahanBabuskaNeumaierInit).
func (s *sumAgg) kahanBabuskaNeumaierInit(v int64) {
	const maxExact = 4503599627370496 // 2^52
	if v <= -maxExact || v >= maxExact {
		iSm := v % 16384
		s.floatSum = float64(v - iSm)
		s.rErr = float64(iSm)
	} else {
		s.floatSum = float64(v)
		s.rErr = 0
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// sumNumericArg ports vdbeapi.c sqlite3_value_numeric_type as called by
// func.c sumStep: only TEXT values are converted, to the numeric type their
// text denotes — integer-looking, lossless int64 text becomes INTEGER;
// other numeric text (decimal point, exponent, or beyond int64 range)
// becomes REAL (applyNumericAffinity with bTryForInt=0 never folds a whole
// float back to integer). Everything else passes through unchanged.
func sumNumericArg(v interface{}) interface{} {
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	s, ok := v.(string)
	if !ok {
		return v
	}
	t := strings.TrimSpace(s)
	if t == "" || !numericTextRunes(t) {
		return v // non-numeric text stays TEXT
	}
	if i, err := strconv.ParseInt(t, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f
	}
	return v
}

// numericTextRunes reports whether s only holds the characters sqlite3AtoF
// accepts for a numeric literal (digits, sign, dot, exponent marker). This
// rejects the Go-only spellings strconv would otherwise take ("inf", "nan",
// "1_000"), which SQLite's text-to-double conversion leaves as TEXT.
func numericTextRunes(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.', c == 'e', c == 'E':
		default:
			return false
		}
	}
	return true
}

func (s *sumAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	// func.c sumStep classifies every input with
	// sqlite3_value_numeric_type before accumulating: a TEXT column holding
	// numeric text feeds the exact integer path (misc1-2.2: sum(one) over
	// '1','2',... stays INTEGER), while BLOBs and non-numeric text count as
	// 0.0 on the approx path.
	arg := sumNumericArg(args[0])
	if arg == nil {
		return nil
	}
	s.count++
	if i, ok := arg.(int64); ok {
		if !s.isFloat {
			return s.stepExact(i)
		}
		s.kahanBabuskaNeumaierStepInt64(i)
		return nil
	}
	if !s.isFloat {
		// First non-integer input: seed the compensated sum from the int64
		// accumulator (sumStep's kahanBabuskaNeumaierInit branch). ovrfl
		// cannot be set while approx==0.
		s.kahanBabuskaNeumaierInit(s.intSum)
		s.isFloat = true
	} else {
		// A later non-integer input absorbs an earlier overflow (sumStep
		// clears ovrfl in the approx/non-integer branch).
		s.ovrfl = false
	}
	return s.stepApprox(arg)
}

// stepExact accumulates an exact int64 input, promoting to the compensated
// double sum on int64 overflow (sumStep's sqlite3AddInt64 failure branch).
func (s *sumAgg) stepExact(v int64) error {
	newSum := s.intSum + v
	if (v > 0 && newSum < s.intSum) || (v < 0 && newSum > s.intSum) {
		// Overflow: promote to the compensated double sum, flag ovrfl.
		s.ovrfl = true
		s.kahanBabuskaNeumaierInit(s.intSum)
		s.kahanBabuskaNeumaierStepInt64(v)
		s.isFloat = true
		return nil
	}
	s.intSum = newSum
	return nil
}

// stepApprox accumulates one input into the compensated double sum. BLOB and
// non-numeric TEXT inputs convert to 0.0 (sqlite3_value_double on a value
// with no numeric representation) but are still counted by Step: sum over
// x'4142' is 0.0 real, and avg's denominator includes them.
func (s *sumAgg) stepApprox(arg interface{}) error {
	switch v := arg.(type) {
	case int64:
		s.kahanBabuskaNeumaierStepInt64(v)
	case float64:
		s.kahanBabuskaNeumaierStep(v)
	default:
		s.kahanBabuskaNeumaierStep(0)
	}
	return nil
}

func (s *sumAgg) Final() (interface{}, error) {
	if s.count == 0 {
		return nil, nil
	}
	if s.isFloat {
		if s.ovrfl {
			// sumFinalize: an unabsorbed int64 overflow errors even though
			// the double sum kept running.
			return nil, fmt.Errorf("integer overflow")
		}
		// sumFinalize folds the compensation term in only when it is finite
		// (func.c: `if( !sqlite3IsOverflow(p->rErr) )` — sqlite3IsOverflow
		// is true for NaN and ±Inf). An input of ±Inf leaves rErr NaN
		// ((Inf-t)+s) or ±Inf, and rSum alone is the correct IEEE result:
		// sum(9e999) = Inf, not NaN (func-8.x / func-38.100).
		r := s.floatSum
		if !isOverflowDouble(s.rErr) {
			r += s.rErr
		}
		return r, nil
	}
	return s.intSum, nil
}

// isOverflowDouble ports util.c sqlite3IsOverflow: true for NaN and ±Inf.
func isOverflowDouble(f float64) bool {
	return math.IsNaN(f) || math.IsInf(f, 0)
}

type totalAgg struct {
	sumAgg
}

// Step for TOTAL: func.c's total() shares sumStep with sum() (same
// classification and Kahan-Babuška-Neumaier accumulation); only the
// finalizer differs — totalFinalize always yields a double and never raises
// the integer-overflow error.
func (t *totalAgg) Step(args []interface{}) error {
	return t.sumAgg.Step(args)
}

func (t *totalAgg) Final() (interface{}, error) {
	// TOTAL returns 0.0 for empty sets (unlike SUM which returns NULL)
	// (func.c totalFinalize).
	if !t.isFloat {
		return float64(t.intSum), nil
	}
	r := t.floatSum
	if !isOverflowDouble(t.rErr) {
		r += t.rErr
	}
	return r, nil
}

type avgAgg struct {
	sumAgg
}

// Step for AVG: func.c's avg() shares sumStep with sum() (the classification
// and accumulation are identical, including BLOB / non-numeric text counting
// as 0.0 in the denominator); only the finalizer differs — avgFinalize
// divides by the count and never raises the integer-overflow error.
func (a *avgAgg) Step(args []interface{}) error {
	return a.sumAgg.Step(args)
}

func (a *avgAgg) Final() (interface{}, error) {
	if a.count == 0 {
		return nil, nil
	}
	// avgFinalize: r = approx ? (rSum + rErr when finite) : (double)iSum.
	var r float64
	if a.isFloat {
		r = a.floatSum
		if !isOverflowDouble(a.rErr) {
			r += a.rErr
		}
	} else {
		r = float64(a.intSum)
	}
	return r / float64(a.count), nil
}

// collatedArg is satisfied by the argument collation markers the evaluator
// keeps for min()/max() calls (execexpr.CollatedValue): min()/max() take
// their comparison collation from the LEFTMOST argument that carries one
// (func.c minmaxStep: sqlite3GetFuncCollSeq, fed by the SQLITE_FUNC_NEEDCOLL
// argument scan in expr.c). Declared through an interface so internal/
// function does not import internal/execexpr.
type collatedArg interface {
	AggCollation() string
	AggValue() interface{}
}

// aggArgValue unwraps a potentially collated aggregate argument into its raw
// value and collation name.
func aggArgValue(v interface{}) (interface{}, string) {
	if c, ok := v.(collatedArg); ok {
		return c.AggValue(), c.AggCollation()
	}
	return v, ""
}

// minMaxCompare compares two raw values under the aggregate's resolved
// collation (BINARY when empty).
func minMaxCompare(a, b interface{}, collation string) int {
	if collation != "" {
		return util.CompareValuesCollate(a, b, collation)
	}
	return util.CompareValues(a, b)
}

type minAgg struct {
	min interface{}
	set bool
	// coll is the comparison collation taken from the first collation-bearing
	// argument value (the leftmost argument with a collation).
	coll string
}

func (m *minAgg) Step(args []interface{}) error {
	for _, arg := range args {
		if arg == nil {
			continue
		}
		v, coll := aggArgValue(arg)
		if v == nil {
			continue
		}
		if coll != "" && m.coll == "" {
			m.coll = coll
		}
		if !m.set || minMaxCompare(v, m.min, m.coll) < 0 {
			m.min = v
			m.set = true
		}
	}
	return nil
}

func (m *minAgg) Final() (interface{}, error) {
	return m.min, nil
}

type maxAgg struct {
	max interface{}
	set bool
	// coll is the comparison collation taken from the first collation-bearing
	// argument value (the leftmost argument with a collation).
	coll string
}

func (m *maxAgg) Step(args []interface{}) error {
	for _, arg := range args {
		if arg == nil {
			continue
		}
		v, coll := aggArgValue(arg)
		if v == nil {
			continue
		}
		if coll != "" && m.coll == "" {
			m.coll = coll
		}
		if !m.set || minMaxCompare(m.max, v, m.coll) < 0 {
			m.max = v
			m.set = true
		}
	}
	return nil
}

func (m *maxAgg) Final() (interface{}, error) {
	return m.max, nil
}

type groupConcatAgg struct {
	values []string
	// seps[i] is the separator that joins values[i-1] and values[i]: the
	// separator argument evaluated on the row contributing values[i]. Window
	// frames with a per-row separator (windowB-20.x: group_concat('-', x)
	// OVER (... ROWS 1 PRECEDING)) use a DIFFERENT separator per junction —
	// storing one separator applied the last row's value to every junction.
	seps []string
	// enc is the database text encoding: BLOB values render as text by
	// decoding their bytes per the encoding (sqlite3_value_text on a disk
	// blob, which carries the OP_Column encoding tag; windowC-2.x).
	enc string
}

func (g *groupConcatAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	sep := ","
	if len(args) > 1 {
		if args[1] == nil {
			// func.c groupConcatStep: a NULL separator argument appends
			// nothing (zSep NULL skips the append) — the values concatenate
			// with no separator (func-24.5: group_concat(t1,NULL)).
			sep = ""
		} else {
			sep = textOfEncoding(args[1], g.enc)
		}
	}
	g.values = append(g.values, textOfEncoding(args[0], g.enc))
	g.seps = append(g.seps, sep)
	return nil
}

func (g *groupConcatAgg) Final() (interface{}, error) {
	// SQLite's group_concat over zero input rows returns NULL, not the empty
	// string (a window frame or GROUP BY group with no rows → NULL; window1
	// 78.2: group_concat(x) OVER (RANGE ... FOLLOWING) over an empty frame
	// → NULL, so quote() renders "NULL").
	if len(g.values) == 0 {
		return nil, nil
	}
	// func.c groupConcatFinalize: each element after the first is prefixed
	// with ITS OWN row's separator argument.
	var b strings.Builder
	b.WriteString(g.values[0])
	for i := 1; i < len(g.values); i++ {
		b.WriteString(g.seps[i])
		b.WriteString(g.values[i])
	}
	return b.String(), nil
}

// md5sumAgg implements the test-harness MD5SUM aggregate: it concatenates the
// text of each row's first argument and returns the lowercase hex MD5 of the
// concatenation (SQLite's test_config.c md5sum registers the same behavior).
type md5sumAgg struct {
	h   hash.Hash
	enc string
}

func (m *md5sumAgg) Step(args []interface{}) error {
	if len(args) == 0 {
		return nil
	}
	// test_md5.c md5step: EVERY argument's text is hashed per row; a NULL
	// argument contributes nothing (sqlite3_value_text returns NULL).
	for _, arg := range args {
		if arg == nil {
			continue
		}
		if m.h == nil {
			m.h = md5.New()
		}
		io.WriteString(m.h, textOfEncoding(arg, m.enc))
	}
	return nil
}

func (m *md5sumAgg) Final() (interface{}, error) {
	if m.h == nil {
		// No rows: md5 of empty input (d41d8cd98f00b204e9800998ecf8427e).
		return "d41d8cd98f00b204e9800998ecf8427e", nil
	}
	return hex.EncodeToString(m.h.Sum(nil)), nil
}
