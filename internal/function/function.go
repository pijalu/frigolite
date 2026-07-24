// Package function provides SQL scalar and aggregate functions.
package function

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"strings"
	"time"

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
	r.register(&Func{Name: "RANDOMBLOB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnRANDOMBLOB})
	r.register(&Func{Name: "RANDSTR", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnRANDSTR})
	r.register(&Func{Name: "ZEROBLOB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnZEROBLOB})
	r.register(&Func{Name: "LIKELIHOOD", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnLIKELIHOOD})
	r.register(&Func{Name: "LIKELY", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLIKELY})
	r.register(&Func{Name: "UNLIKELY", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNLIKELY})
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

	// Date/time functions
	r.register(&Func{Name: "DATE", Type: TypeScalar, MinArgs: 1, MaxArgs: 3, ScalarFn: fnDATE})
	r.register(&Func{Name: "TIME", Type: TypeScalar, MinArgs: 1, MaxArgs: 3, ScalarFn: fnTIME})
	r.register(&Func{Name: "DATETIME", Type: TypeScalar, MinArgs: 1, MaxArgs: 3, ScalarFn: fnDATETIME})
	r.register(&Func{Name: "STRFTIME", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnSTRFTIME})
	r.register(&Func{Name: "JULIANDAY", Type: TypeScalar, MinArgs: 1, MaxArgs: 3, ScalarFn: fnJULIANDAY})

	// Compression functions (using Go stdlib compress/zlib and hash/crc32)
	r.register(&Func{Name: "COMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCOMPRESS})
	r.register(&Func{Name: "UNCOMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnUNCOMPRESS})
	r.register(&Func{Name: "CRC32", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCRC32})

	// Extension/compat functions
	r.register(&Func{Name: "TOINTEGER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOINTEGER})
	r.register(&Func{Name: "FORMAT", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnPRINTF})
	r.register(&Func{Name: "CONCAT_WS", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnCONCATWS})
	r.register(&Func{Name: "EDITDIST3", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnEDITDIST3})
	r.register(&Func{Name: "SPELLFIX1_SCRIPTCODE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSPELLFIX1SCRIPTCODE})
	// Decimal extension (stub — returns string representation)
	r.register(&Func{Name: "DECIMAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnDECIMAL})
	// JSON functions (stubs — return input as-is)
	r.register(&Func{Name: "JSON", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "JSONB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "JSON_OBJECT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "JSONB_OBJECT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "JSON_ARRAY", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "JSONB_ARRAY", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSONIDENTITY})

	// Math functions
	r.register(&Func{Name: "ACOS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnACOS})
	r.register(&Func{Name: "ACOSH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnACOSH})
	r.register(&Func{Name: "ASIN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnASIN})
	r.register(&Func{Name: "ASINH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnASINH})
	r.register(&Func{Name: "ATAN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnATAN})
	r.register(&Func{Name: "ATAN2", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnATAN2})
	r.register(&Func{Name: "CEIL", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCEIL})
	r.register(&Func{Name: "CEILING", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCEIL})
	r.register(&Func{Name: "COS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCOS})
	r.register(&Func{Name: "COSH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCOSH})
	r.register(&Func{Name: "DEGREES", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnDEGREES})
	r.register(&Func{Name: "EXP", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnEXP})
	r.register(&Func{Name: "FLOOR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnFLOOR})
	r.register(&Func{Name: "LN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLN})
	r.register(&Func{Name: "LOG", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnLOG})
	r.register(&Func{Name: "LOG10", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLOG10})
	r.register(&Func{Name: "LOG2", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLOG2})
	r.register(&Func{Name: "MOD", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnMOD})
	r.register(&Func{Name: "PI", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnPI})
	r.register(&Func{Name: "POW", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnPOW})
	r.register(&Func{Name: "POWER", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnPOW})
	r.register(&Func{Name: "RADIANS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnRADIANS})
	r.register(&Func{Name: "SIGN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSIGN})
	r.register(&Func{Name: "SIN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSIN})
	r.register(&Func{Name: "SINH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSINH})
	r.register(&Func{Name: "SQRT", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSQRT})
	r.register(&Func{Name: "TAN", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTAN})
	r.register(&Func{Name: "TANH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTANH})
	r.register(&Func{Name: "TRUNC", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnTRUNC})

	// More extension/compat functions
	r.register(&Func{Name: "TOREAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOREAL})
	r.register(&Func{Name: "TOCHAR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOCHAR})
	r.register(&Func{Name: "TOBLOB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOBLOB})
	r.register(&Func{Name: "TOHEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOHEX})
	r.register(&Func{Name: "UNHEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNHEX})
	r.register(&Func{Name: "CONCAT", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnCONCAT})
	r.register(&Func{Name: "SUBSTRING", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnSUBSTR})
	r.register(&Func{Name: "UNISTR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNISTR})
	r.register(&Func{Name: "NEXT_CHAR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnNEXTCHAR})
	r.register(&Func{Name: "INT2HEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnINT2HEX})
	r.register(&Func{Name: "REGEXPI", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnREGEXPI})
	r.register(&Func{Name: "PREFIX_LENGTH", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnPREFIXLENGTH})
	r.register(&Func{Name: "DECIMAL_MUL", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALMUL})
	r.register(&Func{Name: "DECIMAL_ADD", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALMUL})
	r.register(&Func{Name: "DECIMAL_SUB", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALMUL})
	r.register(&Func{Name: "DECIMAL_DIV", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALMUL})
	r.register(&Func{Name: "JSONB_REMOVE", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnJSONIDENTITY})
	r.register(&Func{Name: "FIRST_VALUE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnFIRSTVALUE})
	r.register(&Func{Name: "LAST_INSERT_ROWID", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnLASTINSERTROWID})
	r.register(&Func{Name: "LOAD_EXTENSION", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnLOADEXTENSION})
	r.register(&Func{Name: "EVAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnEVALSTUB})
	r.register(&Func{Name: "Ieee754", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnIeee754})
	r.register(&Func{Name: "Ieee754_from_blob", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754FromBlob})
	r.register(&Func{Name: "Ieee754_inc", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754Inc})
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
	if n <= 0 {
		return []byte{}, nil
	}
	return make([]byte, n), nil
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
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%d", v), nil
	case float64:
		// Format float like SQLite: use %g but ensure .0 for whole numbers
		s := fmt.Sprintf("%g", v)
		// Handle negative zero: SQLite shows -0 as 0
		if v == 0 {
			s = "0"
		}
		// If no decimal point and no exponent, add .0
		if !strings.Contains(s, ".") && !strings.ContainsAny(s, "eE") {
			s += ".0"
		}
		return s, nil
	case string:
		// Escape single quotes by doubling them, wrap in single quotes
		escaped := strings.ReplaceAll(v, "'", "''")
		return "'" + escaped + "'", nil
	case []byte:
		// Blob: X'hex'
		return fmt.Sprintf("X'%x'", v), nil
	default:
		// For bool and other types
		return fmt.Sprintf("'%v'", v), nil
	}
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
	// GLOB(pattern, string) — pattern is first arg, string is second arg
	pattern := toString(args[0])
	s := toString(args[1])
	return GlobMatch(s, pattern), nil
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

// GlobMatch implements SQLite GLOB matching (* and ? wildcards).
func GlobMatch(s, pattern string) bool {
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
			if ok, np, ns := globMatchChar(s, c, px, sx); ok {
				px, sx = np, ns
				continue
			}
		}
		if 0 < nextPx && nextPx <= len(pattern) && nextSx <= len(s) {
			px, sx = nextPx, nextSx
			nextSx++
			continue
		}
		return false
	}
	return true
}

// globMatchChar handles ? and exact character matching for GLOB.
func globMatchChar(s string, c byte, px, sx int) (bool, int, int) {
	if c == '?' && sx < len(s) {
		return true, px + 1, sx + 1
	}
	if sx < len(s) && s[sx] == c {
		return true, px + 1, sx + 1
	}
	return false, px, sx
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

// --- Date/Time functions ---

func toTimestamp(args []interface{}) (time.Time, error) {
	s := toString(args[0])
	if s == "now" {
		return time.Now(), nil
	}
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"15:04:05",
		"15:04",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date/time: %s", s)
}

func fnDATE(args []interface{}) (interface{}, error) {
	t, err := toTimestamp(args)
	if err != nil {
		return nil, err
	}
	return t.Format("2006-01-02"), nil
}

func fnTIME(args []interface{}) (interface{}, error) {
	t, err := toTimestamp(args)
	if err != nil {
		return nil, err
	}
	return t.Format("15:04:05"), nil
}

func fnDATETIME(args []interface{}) (interface{}, error) {
	t, err := toTimestamp(args)
	if err != nil {
		return nil, err
	}
	return t.Format("2006-01-02 15:04:05"), nil
}

func fnSTRFTIME(args []interface{}) (interface{}, error) {
	format := toString(args[0])
	t, err := toTimestamp(args[1:])
	if err != nil {
		return nil, err
	}
	// Convert SQLite strftime format to Go format
	format = strings.ReplaceAll(format, "%Y", "2006")
	format = strings.ReplaceAll(format, "%m", "01")
	format = strings.ReplaceAll(format, "%d", "02")
	format = strings.ReplaceAll(format, "%H", "15")
	format = strings.ReplaceAll(format, "%M", "04")
	format = strings.ReplaceAll(format, "%S", "05")
	format = strings.ReplaceAll(format, "%j", "002")
	format = strings.ReplaceAll(format, "%W", "")
	format = strings.ReplaceAll(format, "%w", "")
	return t.Format(format), nil
}

func fnJULIANDAY(args []interface{}) (interface{}, error) {
	t, err := toTimestamp(args)
	if err != nil {
		return nil, err
	}
	// Julian day calculation
	unix := t.Unix()
	julian := float64(unix)/86400.0 + 2440587.5
	return julian, nil
}

// --- Extension/stub functions ---

func fnTOINTEGER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case string:
		if i, err := parseInt64(v); err == nil {
			return i, nil
		}
		if f, err := parseFloat64(v); err == nil {
			return int64(f), nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func parseInt64(s string) (int64, error) {
	var i int64
	_, err := fmt.Sscanf(s, "%d", &i)
	return i, err
}

func parseFloat64(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

func fnCONCATWS(args []interface{}) (interface{}, error) {
	sep := ""
	if len(args) > 0 && args[0] != nil {
		sep = fmt.Sprintf("%v", args[0])
	}
	var parts []string
	for i := 1; i < len(args); i++ {
		if args[i] != nil {
			parts = append(parts, fmt.Sprintf("%v", args[i]))
		}
	}
	return strings.Join(parts, sep), nil
}

func fnEDITDIST3(args []interface{}) (interface{}, error) {
	// Stub: return 0 for edit distance
	return int64(0), nil
}

func fnSPELLFIX1SCRIPTCODE(args []interface{}) (interface{}, error) {
	// Stub: return empty string
	return "", nil
}

func fnDECIMAL(args []interface{}) (interface{}, error) {
	// Stub: return string representation of the input
	if args[0] == nil {
		return nil, nil
	}
	return fmt.Sprintf("%v", args[0]), nil
}

func fnJSONIDENTITY(args []interface{}) (interface{}, error) {
	// Stub: JSON functions return values as-is
	if len(args) == 0 {
		return nil, nil
	}
	return args[0], nil
}

// --- Math function implementations ---

func fnACOS(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Acos(f), nil
}

func fnACOSH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Acosh(f), nil
}

func fnASIN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Asin(f), nil
}

func fnASINH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Asinh(f), nil
}

func fnATAN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Atan(f), nil
}

func fnATAN2(args []interface{}) (interface{}, error) {
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil { return nil, nil }
	return math.Atan2(f1, f2), nil
}

func fnCEIL(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Ceil(f), nil
}

func fnCOS(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Cos(f), nil
}

func fnCOSH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Cosh(f), nil
}

func fnDEGREES(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return f * 180.0 / math.Pi, nil
}

func fnEXP(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Exp(f), nil
}

func fnFLOOR(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Floor(f), nil
}

func fnLN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Log(f), nil
}

func fnLOG(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	if len(args) >= 2 {
		base, err2 := toFloat64(args[1])
		if err2 != nil { return nil, nil }
		return math.Log(f) / math.Log(base), nil
	}
	return math.Log10(f), nil
}

func fnLOG10(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Log10(f), nil
}

func fnLOG2(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Log2(f), nil
}

func fnMOD(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil { return nil, nil }
	a, err1 := toFloat64(args[0])
	b, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil || b == 0 { return nil, nil }
	return math.Mod(a, b), nil
}

func fnPI(args []interface{}) (interface{}, error) {
	return math.Pi, nil
}

func fnPOW(args []interface{}) (interface{}, error) {
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil { return nil, nil }
	return math.Pow(f1, f2), nil
}

func fnRADIANS(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return f * math.Pi / 180.0, nil
}

func fnSIGN(args []interface{}) (interface{}, error) {
	if args[0] == nil { return nil, nil }
	switch v := args[0].(type) {
	case int64:
		if v > 0 { return int64(1), nil }
		if v < 0 { return int64(-1), nil }
		return int64(0), nil
	case float64:
		if v > 0 { return float64(1), nil }
		if v < 0 { return float64(-1), nil }
		return float64(0), nil
	default:
		return nil, nil
	}
}

func fnSIN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Sin(f), nil
}

func fnSINH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Sinh(f), nil
}

func fnSQRT(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Sqrt(f), nil
}

func fnTAN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Tan(f), nil
}

func fnTANH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	return math.Tanh(f), nil
}

func fnTRUNC(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil { return nil, nil }
	if len(args) >= 2 {
		digits, err2 := toFloat64(args[1])
		if err2 != nil { return nil, nil }
		pow := math.Pow(10, digits)
		return math.Trunc(f*pow) / pow, nil
	}
	if f >= 0 {
		return math.Floor(f), nil
	}
	return math.Ceil(f), nil
}

// --- More extension functions ---

func fnTOREAL(args []interface{}) (interface{}, error) {
	if args[0] == nil { return nil, nil }
	f, err := toFloat64(args[0])
	if err != nil { return int64(0), nil }
	return int64(f), nil
}

func fnTOCHAR(args []interface{}) (interface{}, error) {
	if args[0] == nil { return nil, nil }
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
	if args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnTOHEX(args []interface{}) (interface{}, error) {
	if args[0] == nil { return nil, nil }
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

func fnUNHEX(args []interface{}) (interface{}, error) {
	// Stub: return input as-is
	if args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnCONCAT(args []interface{}) (interface{}, error) {
	var parts []string
	for _, a := range args {
		if a != nil {
			parts = append(parts, fmt.Sprintf("%v", a))
		}
	}
	return strings.Join(parts, ""), nil
}

func fnUNISTR(args []interface{}) (interface{}, error) {
	// Stub: return input as-is
	if args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnNEXTCHAR(args []interface{}) (interface{}, error) {
	// Stub: return input character + 1
	if args[0] == nil { return nil, nil }
	s := fmt.Sprintf("%v", args[0])
	if len(s) > 0 {
		return string([]byte{byte(s[0] + 1)}), nil
	}
	return "", nil
}

func fnINT2HEX(args []interface{}) (interface{}, error) {
	if args[0] == nil { return nil, nil }
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%x", v), nil
	case float64:
		return fmt.Sprintf("%x", int64(v)), nil
	default:
		return fmt.Sprintf("%x", v), nil
	}
}

func fnREGEXPI(args []interface{}) (interface{}, error) {
	// Case-insensitive regexp: reuse GLOB logic
	return fnGLOB(args)
}

func fnPREFIXLENGTH(args []interface{}) (interface{}, error) {
	// Stub: return 0 for prefix length
	return int64(0), nil
}

func fnDECIMALMUL(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnFIRSTVALUE(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if len(args) == 0 || args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnLASTINSERTROWID(args []interface{}) (interface{}, error) {
	// Stub: return 0
	return int64(0), nil
}

func fnLOADEXTENSION(args []interface{}) (interface{}, error) {
	// Stub: return error (extension loading not supported)
	return nil, fmt.Errorf("extension loading not supported")
}

func fnEVALSTUB(args []interface{}) (interface{}, error) {
	// Stub: return NULL
	return nil, nil
}

func fnIeee754(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if args[0] == nil { return nil, nil }
	return args[0], nil
}

func fnIeee754FromBlob(args []interface{}) (interface{}, error) {
	// Stub: return 0.0
	return float64(0), nil
}

func fnIeee754Inc(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if args[0] == nil { return nil, nil }
	return args[0], nil
}
