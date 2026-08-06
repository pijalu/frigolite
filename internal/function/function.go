// Package function provides SQL scalar and aggregate functions.
package function

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pijalu/frigolite/internal/util"
)

// Type is the function type.
type Type int

const (
	TypeScalar Type = iota
	TypeAggregate
)

// Func is a registered SQL function.
type Func struct {
	Name        string
	Type        Type
	MinArgs     int
	MaxArgs     int
	ScalarFn    func(args []interface{}) (interface{}, error)
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

// Register adds a scalar function to the registry (used for engine-specific
// functions like SQLite's internal sqlite_rename_quotefix).
func (r *Registry) Register(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	r.register(&Func{Name: name, Type: TypeScalar, MinArgs: minArgs, MaxArgs: maxArgs, ScalarFn: fn})
}

func (r *Registry) registerDefaults() {
	// Aggregate functions
	r.register(&Func{Name: "COUNT", Type: TypeAggregate, MinArgs: 0, MaxArgs: 1, AggregateFn: func() Aggregator { return &countAgg{} }})
	r.register(&Func{Name: "SUM", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &sumAgg{} }})
	r.register(&Func{Name: "AVG", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &avgAgg{} }})
	r.register(&Func{Name: "MIN", Type: TypeAggregate, MinArgs: 1, MaxArgs: -1, AggregateFn: func() Aggregator { return &minAgg{} }})
	r.register(&Func{Name: "MAX", Type: TypeAggregate, MinArgs: 1, MaxArgs: -1, AggregateFn: func() Aggregator { return &maxAgg{} }})
	r.register(&Func{Name: "TOTAL", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return &totalAgg{} }})
	r.register(&Func{Name: "GROUP_CONCAT", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return &groupConcatAgg{} }})
	r.register(&Func{Name: "STRING_AGG", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return &groupConcatAgg{} }})

	// Scalar functions
	r.register(&Func{Name: "ABS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnABS})
	r.register(&Func{Name: "UPPER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUPPER})
	r.register(&Func{Name: "LOWER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLOWER})
	r.register(&Func{Name: "LENGTH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnLENGTH})
	r.register(&Func{Name: "OCTET_LENGTH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnOCTETLENGTH})
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
	r.register(&Func{Name: "AFFINITY", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnAFFINITY})
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
	r.register(&Func{Name: "REGEXP", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnREGEXP})
	r.register(&Func{Name: "REGEXPI", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnREGEXPI})
	r.register(&Func{Name: "ERROR", Type: TypeScalar, MinArgs: 0, MaxArgs: 1, ScalarFn: fnERROR})
	// Test-support functions from the SQLite TCL test suite. The original
	// tests register these via `db func`/`db function` TCL helpers which the
	// transpiler cannot reproduce, so they are available by default.
	//   - trigfunc(args...) records its arguments and returns them (alter.test)
	//   - set_val(x) records x and returns it (alter2.test)
	r.register(&Func{Name: "TRIGFUNC", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnTRIGFUNC})
	r.register(&Func{Name: "SET_VAL", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnSETVAL})

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
	r.register(&Func{Name: "CHANGES", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnCHANGES})
	r.register(&Func{Name: "TOTAL_CHANGES", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnCHANGES})
	r.register(&Func{Name: "REPEAT", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnREPEAT})
	r.register(&Func{Name: "LIKELIHOOD", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnIDENTITY2})
	r.register(&Func{Name: "VALUES", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnVALUES})
	r.register(&Func{Name: "Ieee754", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnIeee754})
	r.register(&Func{Name: "Ieee754_from_blob", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754FromBlob})
	r.register(&Func{Name: "Ieee754_inc", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnIeee754Inc})
	// if()/iif(): SQLite compiles these as a CASE expression.
	// if(c1,v1,c2,v2,...,default) = CASE WHEN c1 THEN v1 WHEN c2 THEN v2 ... ELSE default END
	r.register(&Func{Name: "IF", Type: TypeScalar, MinArgs: 3, MaxArgs: -1, ScalarFn: fnIfIIf})
	r.register(&Func{Name: "IIF", Type: TypeScalar, MinArgs: 3, MaxArgs: -1, ScalarFn: fnIfIIf})
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
			// Check for overflow when adding to intSum
			newSum := s.intSum + v
			if (v > 0 && newSum < s.intSum) || (v < 0 && newSum > s.intSum) {
				// Overflow: switch to float
				s.isFloat = true
				s.floatSum = float64(s.intSum) + float64(v)
			} else {
				s.intSum = newSum
			}
			return nil
		}
		// Non-int input: switch to float mode
		s.isFloat = true
		s.floatSum = float64(s.intSum)
	}

	// Float mode: add as float64
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
		if !m.set || less(arg, m.min) {
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
		if !m.set || less(m.max, arg) {
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
	// SQLite's built-in upper() folds ASCII only; non-ASCII characters are
	// left unchanged (no ICU Unicode case mapping).
	return toUpperASCII(toString(args[0])), nil
}

func fnLOWER(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return toLowerASCII(toString(args[0])), nil
}

// toUpperASCII uppercases only the ASCII letters a-z, leaving every other
// byte (including multi-byte UTF-8 sequences) untouched.
func toUpperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// toLowerASCII lowercases only the ASCII letters A-Z.
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

func fnLENGTH(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite length(): character count for text, byte count for blobs, and
	// the length of the text form for numbers.
	switch v := args[0].(type) {
	case []byte:
		return int64(len(v)), nil
	default:
		return int64(utf8CharLen(toString(args[0]))), nil
	}
}

// fnOCTETLENGTH returns the number of bytes in the argument. For text values
// this is the UTF-8 byte length; for numbers the byte length of their text
// representation (SQLite octet_length semantics). NULL returns NULL.
func fnOCTETLENGTH(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case []byte:
		return int64(len(v)), nil
	default:
		return int64(len(toString(args[0]))), nil
	}
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
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	// A NULL length argument yields NULL (SQLite substrFunc checks the
	// argument TYPE, not just the numeric value).
	if len(args) > 2 && args[2] == nil {
		return nil, nil
	}
	p1 := toInt64(args[1])
	if b, ok := args[0].([]byte); ok {
		// Blob: byte-based offsets, result stays a blob. As with text, the
		// two-argument default length is SQLite's huge LIMIT_LENGTH so that
		// a start pushed before the beginning returns the whole blob.
		n := int64(len(b))
		p2 := int64(1000000000) // SQLITE_LIMIT_LENGTH default
		if len(args) > 2 {
			p2 = toInt64(args[2])
		}
		start, length := sqliteSubstrBounds(n, p1, p2)
		if length <= 0 || start >= n {
			return []byte{}, nil
		}
		if start+length > n {
			length = n - start
		}
		return b[start : start+length], nil
	}
	// Text: character-based offsets. With two arguments SQLite uses a huge
	// default length (SQLITE_LIMIT_LENGTH), which matters when a negative
	// start pushes p1 before the beginning: substr('abcdefg',-100) returns
	// the whole string.
	s := toString(args[0])
	nChars := int64(utf8CharLen(s))
	p2 := int64(1000000000) // SQLITE_LIMIT_LENGTH default
	if len(args) > 2 {
		p2 = toInt64(args[2])
	}
	start, length := sqliteSubstrBounds(nChars, p1, p2)
	byteStart := charOffsetToByte(s, start)
	byteEnd := charOffsetToByte(s, start+length)
	if byteEnd < byteStart {
		byteEnd = byteStart
	}
	return s[byteStart:byteEnd], nil
}

// sqliteSubstrBounds computes the 0-based (start, length) pair for SQLite's
// 1-based substr(X, p1[, p2]) over a sequence of total units (bytes for
// blobs, characters for text). It mirrors src/func.c substrFunc: a negative
// p1 counts back from the end, p1==0 with p2>0 consumes one unit of length,
// and a negative p2 returns the |p2| units preceding p1.
func sqliteSubstrBounds(total, p1, p2 int64) (int64, int64) {
	if p1 < 0 {
		p1 += total
		if p1 < 0 {
			if p2 < 0 {
				p2 = 0
			} else {
				p2 += p1
			}
			p1 = 0
		}
	} else if p1 > 0 {
		p1--
	} else if p2 > 0 {
		p2--
	}
	if p2 < 0 {
		if p2 < -p1 {
			p2 = p1
		} else {
			p2 = -p2
		}
		p1 -= p2
	}
	if p1 < 0 {
		p1 = 0
	}
	if p2 < 0 {
		p2 = 0
	}
	return p1, p2
}

// utf8CharLen counts the characters in s using SQLite's SQLITE_SKIP_UTF8
// rule: a character is a lead byte plus any following continuation bytes
// (bytes with the top two bits 10).
func utf8CharLen(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i]&0xC0 != 0x80 {
			n++
		}
	}
	return n
}

// charOffsetToByte returns the byte offset in s after n characters, walking
// whole UTF-8 characters (clamped to the end of s).
func charOffsetToByte(s string, n int64) int {
	i := 0
	for c := int64(0); c < n && i < len(s); c++ {
		i++
		for i < len(s) && s[i]&0xC0 == 0x80 {
			i++
		}
	}
	return i
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

// fnAFFINITY implements the test-only affinity() function from the SQLite TCL
// test suite. It reports the storage-class affinity of its argument's value
// (integer/real/text/blob/none), matching the column-affinity reports the
// test suite expects for values that survived column affinity conversion.
func fnAFFINITY(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return "none", nil
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

func fnREPLACE(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil || args[2] == nil {
		return nil, nil
	}
	s := toString(args[0])
	old := toString(args[1])
	new := toString(args[2])
	if old == "" {
		// SQLite: an empty find string returns the original unchanged.
		return s, nil
	}
	return strings.ReplaceAll(s, old, new), nil
}

func fnINSTR(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	// Mirror sqlite src/func.c instrFunc: the result is one more than the
	// number of characters (or bytes, when BOTH arguments are blobs) in the
	// haystack prior to the first occurrence of the needle, or 0 if absent.
	// An empty needle matches at position 1 (N stays 1); an empty haystack
	// gives 0.
	var hay, needle []byte
	hayIsBlob, needleIsBlob := isBlob(args[0]), isBlob(args[1])
	if hayIsBlob && needleIsBlob {
		// Byte search.
		hay = args[0].([]byte)
		needle = args[1].([]byte)
	} else {
		// Text search: any non-blob argument is used as text; a blob
		// argument is decoded as UTF-8 text.
		hay = []byte(toString(args[0]))
		needle = []byte(toString(args[1]))
	}
	if len(needle) == 0 {
		return int64(1), nil
	}
	n := 1 // characters/bytes consumed (1-based)
	charMode := !(hayIsBlob && needleIsBlob)
	for len(needle) <= len(hay) && !bytes.HasPrefix(hay, needle) {
		n++
		// Advance one unit: skip a whole UTF-8 character in text mode.
		hay = hay[1:]
		if charMode {
			for len(hay) > 0 && hay[0]&0xC0 == 0x80 {
				hay = hay[1:]
			}
		}
	}
	if len(needle) > len(hay) {
		return int64(0), nil
	}
	return int64(n), nil
}

// isBlob reports whether v is a raw []byte (SQLite BLOB) value.
func isBlob(v interface{}) bool {
	_, ok := v.([]byte)
	return ok
}

func fnHEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	// SQLite hex(): uppercase hex of the raw bytes, or of the TEXT form for
	// numbers (hex(65) = hex of '65' = 3635).
	return fmt.Sprintf("%X", toString(args[0])), nil
}

func fnQUOTE(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		// Quote(NULL) returns SQL NULL (not the string 'NULL').
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%d", v), nil
	case float64:
		// Format float like SQLite: use %g but ensure .0 for whole numbers
		s := fmt.Sprintf("%g", v)
		// Handle negative zero: SQLite shows -0.0
		if s == "-0" {
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
		// Blob: X'HEX' with uppercase hex digits
		return fmt.Sprintf("X'%X'", v), nil
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
	if len(s) == 0 {
		// unicode('') returns NULL.
		return nil, nil
	}
	r, _ := utf8.DecodeRuneInString(s)
	return int64(r), nil
}

func fnCHAR(args []interface{}) (interface{}, error) {
	// SQLite char(): each argument is a Unicode codepoint encoded as UTF-8;
	// NULL arguments are skipped; values above U+10FFFF become U+FFFD.
	var sb strings.Builder
	for _, a := range args {
		if a == nil {
			continue
		}
		c := toInt64(a)
		if c > 0x10FFFF {
			c = 0xFFFD
		}
		sb.WriteRune(rune(c))
	}
	return sb.String(), nil
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
	// Unwrap ColumnValue if present
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		// Try to parse as integer
		if i, err := strconv.ParseInt(x, 10, 64); err == nil {
			return i
		}
		// Try to parse as float (truncate)
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return int64(f)
		}
		return 0
	case []byte:
		// For blob, try to convert from hex or treat as 0
		return 0
	default:
		return 0
	}
}

func toFloat64(v interface{}) (float64, error) {
	// Unwrap ColumnValue if present
	if cv, ok := v.(*util.ColumnValue); ok {
		v = cv.Value
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0.0, nil // SQLite: non-numeric strings contribute 0
		}
		return f, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to number", v)
	}
}

func less(a, b interface{}) bool {
	// Unwrap ColumnValue if present
	if cv, ok := a.(*util.ColumnValue); ok {
		a = cv.Value
	}
	if cv, ok := b.(*util.ColumnValue); ok {
		b = cv.Value
	}
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
	if err != nil {
		return nil, nil
	}
	return math.Acos(f), nil
}

func fnACOSH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Acosh(f), nil
}

func fnASIN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Asin(f), nil
}

func fnASINH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Asinh(f), nil
}

func fnATAN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Atan(f), nil
}

func fnATAN2(args []interface{}) (interface{}, error) {
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	return math.Atan2(f1, f2), nil
}

func fnCEIL(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Ceil(f), nil
}

func fnCOS(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Cos(f), nil
}

func fnCOSH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Cosh(f), nil
}

func fnDEGREES(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return f * 180.0 / math.Pi, nil
}

func fnEXP(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Exp(f), nil
}

func fnFLOOR(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Floor(f), nil
}

func fnLN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Log(f), nil
}

func fnLOG(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	if len(args) >= 2 {
		base, err2 := toFloat64(args[1])
		if err2 != nil {
			return nil, nil
		}
		return math.Log(f) / math.Log(base), nil
	}
	return math.Log10(f), nil
}

func fnLOG10(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Log10(f), nil
}

func fnLOG2(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Log2(f), nil
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
	return math.Mod(a, b), nil
}

func fnPI(args []interface{}) (interface{}, error) {
	return math.Pi, nil
}

func fnPOW(args []interface{}) (interface{}, error) {
	f1, err1 := toFloat64(args[0])
	f2, err2 := toFloat64(args[1])
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	return math.Pow(f1, f2), nil
}

func fnRADIANS(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
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
			return float64(1), nil
		}
		if v < 0 {
			return float64(-1), nil
		}
		return float64(0), nil
	default:
		return nil, nil
	}
}

func fnSIN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Sin(f), nil
}

func fnSINH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Sinh(f), nil
}

func fnSQRT(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Sqrt(f), nil
}

func fnTAN(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Tan(f), nil
}

func fnTANH(args []interface{}) (interface{}, error) {
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	return math.Tanh(f), nil
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
		return math.Trunc(f*pow) / pow, nil
	}
	if f >= 0 {
		return math.Floor(f), nil
	}
	return math.Ceil(f), nil
}

// --- More extension functions ---

func fnTOREAL(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return int64(0), nil
	}
	return int64(f), nil
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

func fnUNHEX(args []interface{}) (interface{}, error) {
	// SQLite unhex(): parse a hex string into a BLOB. Returns NULL if the
	// input contains anything other than 0-9A-Fa-f or has odd length.
	if args[0] == nil {
		return nil, nil
	}
	s := toString(args[0])
	if len(s)%2 != 0 {
		return nil, nil
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := unhexDigit(s[i])
		lo, ok2 := unhexDigit(s[i+1])
		if !ok1 || !ok2 {
			return nil, nil
		}
		out[i/2] = hi<<4 | lo
	}
	return out, nil
}

// unhexDigit decodes a single hexadecimal digit byte to 0-15.
func unhexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
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
	if args[0] == nil {
		return nil, nil
	}
	return args[0], nil
}

func fnNEXTCHAR(args []interface{}) (interface{}, error) {
	// Stub: return input character + 1
	if args[0] == nil {
		return nil, nil
	}
	s := fmt.Sprintf("%v", args[0])
	if len(s) > 0 {
		return string([]byte{byte(s[0] + 1)}), nil
	}
	return "", nil
}

func fnINT2HEX(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	switch v := args[0].(type) {
	case int64:
		return fmt.Sprintf("%x", v), nil
	case float64:
		return fmt.Sprintf("%x", int64(v)), nil
	default:
		return fmt.Sprintf("%x", v), nil
	}
}

// fnERROR raises a "SQL error!" exception. Registered to support the test
// harness's `error()` user-defined function (regexp2.test).
func fnERROR(args []interface{}) (interface{}, error) {
	return nil, fmt.Errorf("SQL error!")
}

// fnTRIGFUNC is a test-support scalar function from the SQLite TCL test
// suite (alter.test). The original TCL helper records its arguments in the
// ::TRIGGER global and returns them; the generated tests only rely on the
// function existing (it is invoked inside triggers), so returning the
// space-joined arguments is sufficient.
func fnTRIGFUNC(args []interface{}) (interface{}, error) {
	var parts []string
	for _, a := range args {
		parts = append(parts, toString(a))
	}
	return strings.Join(parts, " "), nil
}

// fnSETVAL is a test-support scalar function from the SQLite TCL test suite
// (alter2.test). The original `db function set_val {set ::val}` helper
// records its argument in the ::val global and returns it.
func fnSETVAL(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return args[0], nil
}

func fnREGEXP(args []interface{}) (interface{}, error) {
	// regexp(P,X) — true if string X matches pattern P (SQLite's regexp
	// extension, case-sensitive). Returns integer 1/0.
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	pattern := toString(args[0])
	s := toString(args[1])
	re, err := compileSQLiteRegexp(pattern)
	if err != nil {
		return int64(0), nil
	}
	if re.MatchString(s) {
		return int64(1), nil
	}
	return int64(0), nil
}

func fnREGEXPI(args []interface{}) (interface{}, error) {
	// regexpi(P,X) — true if string X matches pattern P case-insensitively
	// (SQLite's regexp extension). Returns integer 1/0.
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	pattern := toString(args[0])
	s := toString(args[1])
	re, err := compileSQLiteRegexp("(?i)" + pattern)
	if err != nil {
		return int64(0), nil
	}
	if re.MatchString(s) {
		return int64(1), nil
	}
	return int64(0), nil
}

// compileSQLiteRegexp compiles a pattern written for SQLite's regexp
// extension using Go's regexp engine, translating escapes Go does not
// support (\uXXXX and \UXXXXXXXX) into \x{...} form and mirroring the
// extension's "pattern too big" / "unclosed '['" errors.
func compileSQLiteRegexp(pattern string) (*regexp.Regexp, error) {
	return util.CompileRegexp(pattern)
}

func fnPREFIXLENGTH(args []interface{}) (interface{}, error) {
	// Stub: return 0 for prefix length
	return int64(0), nil
}

func fnDECIMALMUL(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if args[0] == nil {
		return nil, nil
	}
	return args[0], nil
}

func fnFIRSTVALUE(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if len(args) == 0 || args[0] == nil {
		return nil, nil
	}
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

func fnCHANGES(args []interface{}) (interface{}, error) {
	// Stub: return 0 (no row change tracking)
	return int64(0), nil
}

func fnREPEAT(args []interface{}) (interface{}, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		if b, ok := args[0].([]byte); ok {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", args[0])
		}
	}
	var n int
	switch v := args[1].(type) {
	case int64:
		n = int(v)
	case int:
		n = v
	case float64:
		n = int(v)
	case string:
		fmt.Sscanf(v, "%d", &n)
	default:
		return nil, nil
	}
	if n <= 0 {
		return "", nil
	}
	return strings.Repeat(s, n), nil
}

func fnIDENTITY(args []interface{}) (interface{}, error) {
	return args[0], nil
}

func fnIDENTITY2(args []interface{}) (interface{}, error) {
	return args[0], nil
}

// fnIfIIf implements if()/iif() as a CASE expression.
// if(c1,v1,c2,v2,...,default): evaluate condition/value pairs left to right,
// return the first value whose condition is truthy. If odd args, last is default.
// Standard 3-arg form: iif(cond, yes, no).
func fnIfIIf(args []interface{}) (interface{}, error) {
	// Process condition/value pairs
	i := 0
	for i+1 < len(args) {
		cond := args[i]
		val := args[i+1]
		if isTruthyValue(cond) {
			return val, nil
		}
		i += 2
	}
	// If there's a trailing default (odd number of args), return it
	if i < len(args) {
		return args[i], nil
	}
	return nil, nil
}

// isTruthyValue reports whether a value is truthy (non-zero, non-NULL, non-empty).
func isTruthyValue(v interface{}) bool {
	v = unwrap(v)
	switch x := v.(type) {
	case nil:
		return false
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []byte:
		return len(x) > 0
	default:
		return true
	}
}

func unwrap(v interface{}) interface{} {
	// ColumnValue unwrap
	if cv, ok := v.(*util.ColumnValue); ok {
		return util.UnwrapColumnValue(cv)
	}
	return v
}

func fnIeee754(args []interface{}) (interface{}, error) {
	// Stub: return first argument
	if args[0] == nil {
		return nil, nil
	}
	return args[0], nil
}

func fnIeee754FromBlob(args []interface{}) (interface{}, error) {
	// Stub: return 0.0
	return float64(0), nil
}

func fnIeee754Inc(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	x, _ := toFloat64(args[0])
	n := int64(-1) // default: next representable float toward zero
	if len(args) >= 2 && args[1] != nil {
		n = toInt64(args[1])
	}
	// Manipulate the IEEE 754 bits directly
	bits := math.Float64bits(x)
	if n >= 0 {
		bits += uint64(n)
	} else {
		bits -= uint64(-n)
	}
	return math.Float64frombits(bits), nil
}

// fnVALUES implements the VALUES(...) function used as a scalar expression.
// SQLite's VALUES returns the first value when used as a scalar function.
func fnVALUES(args []interface{}) (interface{}, error) {
	if len(args) >= 1 {
		return args[0], nil
	}
	return nil, nil
}
