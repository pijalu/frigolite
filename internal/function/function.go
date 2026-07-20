// Package function provides SQL scalar and aggregate functions.
package function

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"hash/crc32"
	"math"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
)

// Type is the function type.
type Type int

const (
	TypeScalar   Type = iota
	TypeAggregate
)

// Func is a registered SQL function.
type Func struct {
	Name      string
	Type      Type
	MinArgs   int
	MaxArgs   int
	ScalarFn  func(args []interface{}) (interface{}, error)
	AggregateFn func() Aggregator
}

// Aggregator is the interface for aggregate functions.
type Aggregator interface {
	Step(args []interface{}) error
	Final() (interface{}, error)
}

// Registry holds all registered functions.
type Registry struct {
	funcs map[string]*Func
}

// NewRegistry creates a new function registry with default functions.
func NewRegistry() *Registry {
	r := &Registry{funcs: make(map[string]*Func)}
	r.registerDefaults()
	return r
}

// Find looks up a function by name.
func (r *Registry) Find(name string) (*Func, bool) {
	f, ok := r.funcs[strings.ToUpper(name)]
	return f, ok
}

func (r *Registry) register(f *Func) {
	r.funcs[strings.ToUpper(f.Name)] = f
}

func (r *Registry) registerDefaults() {
	// Aggregate functions
	r.register(&Func{Name: "COUNT", Type: TypeAggregate, MinArgs: 0, MaxArgs: 1, AggregateFn: func() Aggregator { return &countAgg{} }})
	r.register(&Func{Name: "SUM", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &sumAgg{} }})
	r.register(&Func{Name: "AVG", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &avgAgg{} }})
	r.register(&Func{Name: "MIN", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &minAgg{} }})
	r.register(&Func{Name: "MAX", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &maxAgg{} }})
	r.register(&Func{Name: "TOTAL", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &totalAgg{} }})
	r.register(&Func{Name: "GROUP_CONCAT", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return &groupConcatAgg{} }})

	// Scalar functions
	r.register(&Func{Name: "ABS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnABS})
	r.register(&Func{Name: "UPPER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUPPER})
	r.register(&Func{Name: "LOWER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLOWER})
	r.register(&Func{Name: "LENGTH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLENGTH})
	r.register(&Func{Name: "TRIM", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnTRIM})
	r.register(&Func{Name: "LTRIM", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnLTRIM})
	r.register(&Func{Name: "RTRIM", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnRTRIM})
	r.register(&Func{Name: "SUBSTR", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnSUBSTR})
	r.register(&Func{Name: "IFNULL", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnIFNULL})
	r.register(&Func{Name: "COALESCE", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnCOALESCE})
	r.register(&Func{Name: "ROUND", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnROUND})
	r.register(&Func{Name: "RANDOM", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnRANDOM})
	r.register(&Func{Name: "TYPEOF", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTYPEOF})
	r.register(&Func{Name: "SUBSTR", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnSUBSTR})
	r.register(&Func{Name: "REPLACE", Type: TypeScalar, MinArgs: 3, MaxArgs: 3, ScalarFn: fnREPLACE})
	r.register(&Func{Name: "INSTR", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnINSTR})
	r.register(&Func{Name: "HEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnHEX})
	r.register(&Func{Name: "QUOTE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnQUOTE})
	r.register(&Func{Name: "UNICODE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNICODE})
	r.register(&Func{Name: "CHAR", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnCHAR})
	r.register(&Func{Name: "NULLIF", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnNULLIF})
	r.register(&Func{Name: "PRINTF", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnPRINTF})
	r.register(&Func{Name: "GLOB", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnGLOB})

	// Compression functions (using Go stdlib compress/zlib and hash/crc32)
	r.register(&Func{Name: "COMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCOMPRESS})
	r.register(&Func{Name: "UNCOMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnUNCOMPRESS})
	r.register(&Func{Name: "CRC32", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCRC32})
}

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
	sum   float64
	count int64
}

func (s *sumAgg) Step(args []interface{}) error {
	if args[0] == nil {
		return nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return err
	}
	s.sum += f
	s.count++
	return nil
}

func (s *sumAgg) Final() (interface{}, error) {
	if s.count == 0 {
		return nil, nil
	}
	return s.sum, nil
}

type totalAgg struct {
	sumAgg
}

func (t *totalAgg) Final() (interface{}, error) {
	// TOTAL returns 0.0 for empty sets (unlike SUM which returns NULL)
	return t.sum, nil
}

type avgAgg struct {
	sumAgg
}

func (a *avgAgg) Final() (interface{}, error) {
	if a.count == 0 {
		return nil, nil
	}
	return a.sum / float64(a.count), nil
}

type minAgg struct {
	min interface{}
	set bool
}

func (m *minAgg) Step(args []interface{}) error {
	if args[0] == nil {
		return nil
	}
	if !m.set || less(args[0], m.min) {
		m.min = args[0]
		m.set = true
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
	if args[0] == nil {
		return nil
	}
	if !m.set || less(m.max, args[0]) {
		m.max = args[0]
		m.set = true
	}
	return nil
}

func (m *maxAgg) Final() (interface{}, error) {
	return m.max, nil
}

type groupConcatAgg struct {
	values  []string
	sep     string
}

func (g *groupConcatAgg) Step(args []interface{}) error {
	if args[0] == nil {
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
	return strings.Join(g.values, g.sep), nil
}

// --- Scalar function implementations ---

func fnABS(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		if v < 0 {
			return -v, nil
		}
		return v, nil
	case float64:
		return math.Abs(v), nil
	default:
		return 0, nil
	}
}

func fnUPPER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return strings.ToUpper(toString(args[0])), nil
}

func fnLOWER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return strings.ToLower(toString(args[0])), nil
}

func fnLENGTH(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return int64(len(toString(args[0]))), nil
}

func fnTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return strings.Trim(toString(args[0]), toString(args[1])), nil
	}
	return strings.TrimSpace(toString(args[0])), nil
}

func fnLTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return strings.TrimLeft(toString(args[0]), toString(args[1])), nil
	}
	return strings.TrimLeft(toString(args[0]), " \t\n\r"), nil
}

func fnRTRIM(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	if len(args) > 1 && args[1] != nil {
		return strings.TrimRight(toString(args[0]), toString(args[1])), nil
	}
	return strings.TrimRight(toString(args[0]), " \t\n\r"), nil
}

func fnSUBSTR(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	start := toInt64(args[1])
	if start < 0 {
		start = int64(len(s)) + start
	} else {
		start-- // SQLite is 1-based
	}
	if start < 0 {
		start = 0
	}
	if int(start) >= len(s) {
		return "", nil
	}
	if len(args) > 2 && args[2] != nil {
		length := toInt64(args[2])
		if length < 0 {
			return "", nil
		}
		if int(start+length) > len(s) {
			length = int64(len(s)) - start
		}
		return s[start : start+length], nil
	}
	return s[start:], nil
}

func fnIFNULL(args []interface{}) (interface{}, error) {
	if args[0] != nil {
		return args[0], nil
	}
	return args[1], nil
}

func fnCOALESCE(args []interface{}) (interface{}, error) {
	for _, a := range args {
		if a != nil {
			return a, nil
		}
	}
	return nil, nil
}

func fnROUND(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return args[0], nil
	}
	places := 0
	if len(args) > 1 && args[1] != nil {
		places = int(toInt64(args[1]))
	}
	pow := math.Pow(10, float64(places))
	return math.Round(f*pow) / pow, nil
}

func fnRANDOM(args []interface{}) (interface{}, error) {
	return int64(math.Float64bits(math.Abs(math.Sin(float64(math.Float64bits(math.Sin(float64(math.Float64bits(math.Sin(float64(42))))))))))), nil
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
	default:
		return "text", nil
	}
}

func fnREPLACE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	old := toString(args[1])
	new := toString(args[2])
	return strings.ReplaceAll(s, old, new), nil
}

func fnINSTR(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	sub := toString(args[1])
	return int64(strings.Index(s, sub) + 1), nil
}

func fnHEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return fmt.Sprintf("%X", args[0]), nil
}

func fnQUOTE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return "NULL", nil
	}
	return fmt.Sprintf("%q", args[0]), nil
}

func fnUNICODE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	if len(s) > 0 {
		return int64(s[0]), nil
	}
	return int64(0), nil
}

func fnCHAR(args []interface{}) (interface{}, error) {
	var b []byte
	for _, a := range args {
		if a != nil {
			if v, ok := a.(int64); ok {
				b = append(b, byte(v))
			}
		}
	}
	return string(b), nil
}

func fnNULLIF(args []interface{}) (interface{}, error) {
	if util.CompareValues(args[0], args[1]) == 0 {
		return nil, nil
	}
	return args[0], nil
}

func fnPRINTF(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return "", nil
	}
	format := toString(args[0])
	goArgs := make([]interface{}, len(args)-1)
	copy(goArgs, args[1:])
	return fmt.Sprintf(format, goArgs...), nil
}

func fnGLOB(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	s := toString(args[0])
	pattern := toString(args[1])
	return globMatch(s, pattern), nil
}

// --- Helpers ---

func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	default:
		return 0
	}
}

func toFloat64(v interface{}) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case string:
		return 0, fmt.Errorf("cannot convert string to number")
	default:
		return 0, fmt.Errorf("cannot convert %T to number", v)
	}
}

func less(a, b interface{}) bool {
	// Simple comparison for aggregates
	switch x := a.(type) {
	case int64:
		switch y := b.(type) {
		case int64:
			return x < y
		case float64:
			return float64(x) < y
		}
	case float64:
		switch y := b.(type) {
		case int64:
			return x < float64(y)
		case float64:
			return x < y
		}
	case string:
		if y, ok := b.(string); ok {
			return x < y
		}
	}
	return false
}

// globMatch implements SQLite GLOB matching (* and ? wildcards).
func globMatch(s, pattern string) bool {
	px, sx := 0, 0
	nextPx, nextSx := 0, 0
	for px < len(pattern) || sx < len(s) {
		if px < len(pattern) {
			c := pattern[px]
			if c == '*' {
				nextPx, nextSx = px+1, sx+1
				px++
				continue
			}
			if c == '?' && sx < len(s) {
				px, sx = px+1, sx+1
				continue
			}
			if sx < len(s) && s[sx] == c {
				px, sx = px+1, sx+1
				continue
			}
		}
		if 0 < nextPx && nextPx <= len(pattern) {
			px, sx = nextPx, nextSx
			nextSx++
			continue
		}
		return false
	}
	return true
}

// --- Compression functions (using Go stdlib) ---

func fnCOMPRESS(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	input := toBytes(args[0])
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(input); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fnUNCOMPRESS(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	input := toBytes(args[0])
	r, err := zlib.NewReader(bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fnCRC32(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	input := toBytes(args[0])
	return int64(crc32.ChecksumIEEE(input)), nil
}

func toBytes(v interface{}) []byte {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	default:
		return []byte(fmt.Sprintf("%v", x))
	}
}
