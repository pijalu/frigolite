package function

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
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

type sumAgg struct {
	intSum   int64
	floatSum float64
	count    int64
	isFloat  bool // true if we've switched to float mode (non-int input or overflow)
}

func (s *sumAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	s.count++

	if !s.isFloat {
		if v, ok := args[0].(int64); ok {
			// SQLite's sum() raises "integer overflow" when the int64
			// accumulator overflows (total() promotes to float instead).
			newSum := s.intSum + v
			if (v > 0 && newSum < s.intSum) || (v < 0 && newSum > s.intSum) {
				return fmt.Errorf("integer overflow")
			}
			s.intSum = newSum
			return nil
		}
		// Non-int input: switch to float mode
		s.isFloat = true
		s.floatSum = float64(s.intSum)
	}

	// Float mode: add as float64. A BLOB input ([]byte) is ignored entirely
	// (SQLite sum()/total() skip non-numeric BLOBs without contributing), and
	// a non-numeric string contributes 0.
	if _, isBlob := args[0].([]byte); isBlob {
		return nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return err
	}
	s.floatSum += f
	return nil
}

func (s *sumAgg) Final() (interface{}, error) {
	if s.count == 0 {
		return nil, nil
	}
	if s.isFloat {
		return s.floatSum, nil
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
	sep    string
}

func (g *groupConcatAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	g.values = append(g.values, toString(args[0]))
	if len(args) > 1 && args[1] != nil {
		g.sep = toString(args[1])
	} else {
		g.sep = ","
	}
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
	return strings.Join(g.values, g.sep), nil
}

// md5sumAgg implements the test-harness MD5SUM aggregate: it concatenates the
// text of each row's first argument and returns the lowercase hex MD5 of the
// concatenation (SQLite's test_config.c md5sum registers the same behavior).
type md5sumAgg struct {
	h hash.Hash
}

func (m *md5sumAgg) Step(args []interface{}) error {
	if len(args) == 0 || args[0] == nil {
		return nil
	}
	if m.h == nil {
		m.h = md5.New()
	}
	io.WriteString(m.h, toString(args[0]))
	return nil
}

func (m *md5sumAgg) Final() (interface{}, error) {
	if m.h == nil {
		// No rows: md5 of empty input (d41d8cd98f00b204e9800998ecf8427e).
		return "d41d8cd98f00b204e9800998ecf8427e", nil
	}
	return hex.EncodeToString(m.h.Sum(nil)), nil
}

func fnMD5SUM() Aggregator {
	return &md5sumAgg{}
}
