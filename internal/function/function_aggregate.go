package function

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
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

func (s *sumAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	s.count++
	if !s.isFloat {
		return s.stepExact(args[0])
	}
	if _, ok := args[0].(int64); !ok {
		// A non-integer input while already in float mode absorbs any
		// earlier overflow (sumStep clears ovrfl in this branch).
		s.ovrfl = false
	}
	return s.stepApprox(args[0])
}

// stepExact accumulates an exact int64 input, promoting to the compensated
// double sum on int64 overflow (sumStep's sqlite3AddInt64 failure branch).
func (s *sumAgg) stepExact(arg interface{}) error {
	v, ok := arg.(int64)
	if !ok {
		// Non-integer input: switch to the compensated sum seeded from the
		// int64 accumulator (sumStep's kahanBabuskaNeumaierInit branch).
		s.kahanBabuskaNeumaierInit(s.intSum)
		s.isFloat = true
		return s.stepApprox(arg)
	}
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

// stepApprox accumulates one input into the compensated double sum. A BLOB
// input ([]byte) is ignored entirely (SQLite sum()/total() skip non-numeric
// BLOBs without contributing), and a non-numeric string contributes 0.
func (s *sumAgg) stepApprox(arg interface{}) error {
	if _, isBlob := arg.([]byte); isBlob {
		return nil
	}
	if v, ok := arg.(int64); ok {
		s.kahanBabuskaNeumaierStepInt64(v)
		return nil
	}
	f, err := toFloat64(arg)
	if err != nil {
		return err
	}
	s.kahanBabuskaNeumaierStep(f)
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
		r := s.floatSum + s.rErr
		if math.IsInf(r, 0) {
			r = s.floatSum
		}
		return r, nil
	}
	return s.intSum, nil
}

type totalAgg struct {
	sumAgg
}

// Step for TOTAL: promotes to float on int64 overflow (no error), matching
// SQLite's total() which always returns a float and never raises overflow.
func (t *totalAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	t.count++
	if !t.isFloat {
		if v, ok := args[0].(int64); ok {
			newSum := t.intSum + v
			if (v > 0 && newSum < t.intSum) || (v < 0 && newSum > t.intSum) {
				t.isFloat = true
				t.floatSum = float64(t.intSum) + float64(v)
			} else {
				t.intSum = newSum
			}
			return nil
		}
		t.isFloat = true
		t.floatSum = float64(t.intSum)
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return err
	}
	t.floatSum += f
	return nil
}

func (t *totalAgg) Final() (interface{}, error) {
	// TOTAL returns 0.0 for empty sets (unlike SUM which returns NULL)
	if t.isFloat {
		return t.floatSum, nil
	}
	return float64(t.intSum), nil
}

type avgAgg struct {
	sumAgg
}

// Step for AVG: promotes to float on int64 overflow (avg of large ints is
// fractional anyway; SQLite's avg() never raises integer overflow).
func (a *avgAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	a.count++
	if !a.isFloat {
		if v, ok := args[0].(int64); ok {
			newSum := a.intSum + v
			if (v > 0 && newSum < a.intSum) || (v < 0 && newSum > a.intSum) {
				a.isFloat = true
				a.floatSum = float64(a.intSum) + float64(v)
			} else {
				a.intSum = newSum
			}
			return nil
		}
		a.isFloat = true
		a.floatSum = float64(a.intSum)
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return err
	}
	a.floatSum += f
	return nil
}

func (a *avgAgg) Final() (interface{}, error) {
	if a.count == 0 {
		return nil, nil
	}
	if a.isFloat {
		return a.floatSum / float64(a.count), nil
	}
	return float64(a.intSum) / float64(a.count), nil
}

type minAgg struct {
	min interface{}
	set bool
}

func (m *minAgg) Step(args []interface{}) error {
	for _, arg := range args {
		if arg == nil {
			continue
		}
		if !m.set || util.CompareValues(arg, m.min) < 0 {
			m.min = arg
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
}

func (m *maxAgg) Step(args []interface{}) error {
	for _, arg := range args {
		if arg == nil {
			continue
		}
		if !m.set || util.CompareValues(m.max, arg) < 0 {
			m.max = arg
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
	if len(args) > 1 && args[1] != nil {
		sep = textOfEncoding(args[1], g.enc)
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
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	if m.h == nil {
		m.h = md5.New()
	}
	io.WriteString(m.h, textOfEncoding(args[0], m.enc))
	return nil
}

func (m *md5sumAgg) Final() (interface{}, error) {
	if m.h == nil {
		// No rows: md5 of empty input (d41d8cd98f00b204e9800998ecf8427e).
		return "d41d8cd98f00b204e9800998ecf8427e", nil
	}
	return hex.EncodeToString(m.h.Sum(nil)), nil
}
