// Package function provides SQL scalar and aggregate functions.
package function

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
	// WrongArgMsg selects SQLite's per-function "wrong number of arguments
	// to function X()" error instead of the generic "function X expects
	// N-M arguments, got K" message. SQLite emits the former for functions
	// that validate their own argument count (unhex, percentile, etc.).
	WrongArgMsg bool
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
	// Ordered-set percentile aggregates (SQLite ext/misc/percentile.c):
	//   percentile(Y,P)      P in [0,100], continuous
	//   percentile_cont(Y,P) P in [0,1], continuous
	//   percentile_disc(Y,P) P in [0,1], discrete
	//   median(Y)            == percentile(Y,50)
	r.register(&Func{Name: "PERCENTILE", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return newPercentileAgg(false, true) }, WrongArgMsg: true})
	r.register(&Func{Name: "PERCENTILE_CONT", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return newPercentileAgg(false, false) }, WrongArgMsg: true})
	r.register(&Func{Name: "PERCENTILE_DISC", Type: TypeAggregate, MinArgs: 1, MaxArgs: 2, AggregateFn: func() Aggregator { return newPercentileAgg(true, false) }, WrongArgMsg: true})
	r.register(&Func{Name: "MEDIAN", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: func() Aggregator { return newPercentileAgg(false, true) }, WrongArgMsg: true})

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
	// test_zeroblob: the TCL test-harness variant of zeroblob() that skips
	// the SQLITE_MAX_LENGTH check (test1.c test_zeroblob). Negative lengths
	// produce the empty blob.
	r.register(&Func{Name: "TEST_ZEROBLOB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTestZeroblob})
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
	// add_text_type/add_int_type/add_real_type force a value to a specific
	// storage class (SQLite types3.test registers these via the TCL harness).
	r.register(&Func{Name: "ADD_TEXT_TYPE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnAddTextType})
	r.register(&Func{Name: "ADD_INT_TYPE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnAddIntType})
	r.register(&Func{Name: "ADD_REAL_TYPE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnAddRealType})
	// intreal(N) forces N to the REAL storage class with an integer value
	// (SQLite's MEM_IntReal test helper): renders as "N.0", typeof "real",
	// but compares numerically like the integer N.
	r.register(&Func{Name: "INTREAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIntReal})

	// Date/time functions (port of SQLite src/date.c; see datetime.go)
	r.register(&Func{Name: "DATE", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnDATE})
	r.register(&Func{Name: "TIME", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnTIME})
	r.register(&Func{Name: "DATETIME", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnDATETIME})
	r.register(&Func{Name: "JULIANDAY", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJULIANDAY})
	r.register(&Func{Name: "UNIXEPOCH", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnUNIXEPOCH})
	r.register(&Func{Name: "STRFTIME", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnSTRFTIME})
	r.register(&Func{Name: "TIMEDIFF", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnTIMEDIFF})

	// Compression functions (using Go stdlib compress/zlib and hash/crc32)
	r.register(&Func{Name: "COMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCOMPRESS})
	r.register(&Func{Name: "UNCOMPRESS", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnUNCOMPRESS})
	r.register(&Func{Name: "CRC32", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCRC32})
	// md5sum is a test-harness aggregate (SQLite's test_config.c registers it
	// as an aggregate that MD5-hashes the concatenation of its arguments per
	// row). Used by trans/trans2 signature checks: SELECT md5sum(u1) ...
	r.register(&Func{Name: "MD5SUM", Type: TypeAggregate, MinArgs: 1, MaxArgs: -1, AggregateFn: fnMD5SUM})

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
	r.register(&Func{Name: "ATANH", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnATANH})
	r.register(&Func{Name: "TRUNC", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnTRUNC})

	// More extension/compat functions
	r.register(&Func{Name: "TOREAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOREAL})
	r.register(&Func{Name: "TOCHAR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOCHAR})
	r.register(&Func{Name: "TOBLOB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOBLOB})
	r.register(&Func{Name: "TOHEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOHEX})
	r.register(&Func{Name: "UNHEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnUNHEX, WrongArgMsg: true})
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

// percentileAgg implements the ordered-set percentile family from SQLite's
// ext/misc/percentile.c:
//   percentile(Y,P)      P in [0,100], continuous (linear interpolation)
//   percentile_cont(Y,P) P in [0,1], continuous
//   percentile_disc(Y,P) P in [0,1], discrete (nearest-rank)
//   median(Y)            == percentile(Y,50)
//
// Semantics (mirroring percentStep/percentCompute in percentile.c):
//   - NULL Y values are ignored.
//   - A non-NULL, non-numeric Y yields "input to <fn>() is not numeric".
//   - An Inf or NaN Y yields "Inf input to <fn>()".
//   - The fraction P must be numeric and within [0,mxFrac]; otherwise "the
//     fraction argument to <fn>() is not between 0.0 and <mxFrac>" where
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

func (p *percentileAgg) Step(args []interface{}) error {
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
	if len(args) == 0 || args[0] == nil {
		return nil // NULL Y is ignored
	}
	f, ok := numericValue(args[0])
	if !ok {
		return fmt.Errorf("input to %s is not numeric", p.name)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("Inf input to %s", p.name)
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

// --- Scalar function implementations ---

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
	// SQLite zeroblob(): a negative length is the empty blob, and a length
	// exceeding SQLITE_MAX_LENGTH (default 1e9) raises "string or blob too
	// big" instead of allocating.
	if n <= 0 {
		return []byte{}, nil
	}
	if n > 1000000000 { // SQLITE_MAX_LENGTH default
		return nil, fmt.Errorf("string or blob too big")
	}
	return make([]byte, n), nil
}

// fnTestZeroblob is the TCL test-harness test_zeroblob(N): like zeroblob but
// without the SQLITE_MAX_LENGTH check (test1.c test_zeroblob). Negative
// lengths return the empty blob.
func fnTestZeroblob(args []interface{}) (interface{}, error) {
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
		// Quote(NULL) returns the 4-character text string 'NULL' (not SQL
		// NULL), matching SQLite: SELECT quote(NULL) -> 'NULL' (text).
		return "NULL", nil
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
	s := fmt.Sprintf(format, goArgs...)
	// SQLite's printf renders +-Inf as "Inf"/"-Inf" and NaN as "NaN"
	// (Go's fmt renders "+Inf"/"-Inf"/"NaN").
	s = strings.ReplaceAll(s, "+Inf", "Inf")
	return s, nil
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
	if f, ok := v.(float64); ok {
		// SQLite renders Inf as "Inf"/"-Inf" and NaN as "NaN" (Go's %v
		// would render "+Inf").
		if math.IsInf(f, 1) {
			return "Inf"
		}
		if math.IsInf(f, -1) {
			return "-Inf"
		}
		if math.IsNaN(f) {
			return "NaN"
		}
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

// mathOneArg evaluates a one-argument math function the way SQLite's math
// extension does: NULL input yields NULL, a NaN result (e.g. sqrt(-1),
// asin(2)) yields NULL, and +-Inf results pass through (exp(1000) is Inf).
func mathOneArg(args []interface{}, fn func(float64) float64) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
		return nil, nil
	}
	r := fn(f)
	if math.IsNaN(r) {
		return nil, nil
	}
	return r, nil
}

// mathLogArg evaluates the log-family functions, which additionally return
// NULL for x<=0 (SQLite: log of zero or a negative number is NULL, not -Inf
// or NaN).
func mathLogArg(args []interface{}, fn func(float64) float64) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	f, err := toFloat64(args[0])
	if err != nil {
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
	return mathOneArg(args, math.Ceil)
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
	return mathOneArg(args, math.Floor)
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
		x, err1 := toFloat64(args[0])
		base, err2 := toFloat64(args[1])
		if err1 != nil || err2 != nil {
			return nil, nil
		}
		// SQLite log(X, B): NULL when X<=0, B<=0, or B==1 (log base 1 is
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
	return mathOneArg(args, func(f float64) float64 { return f * math.Pi / 180.0 })
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
	// SQLite unhex(X, S): parse the hex string X into a BLOB. Any character
	// in the optional second argument S is ignored (used as a separator, e.g.
	// unhex('FFFF ABCD', ' -') -> X'FFFFABCD'). Returns NULL if the input
	// (after removing separators) contains anything other than 0-9A-Fa-f or
	// has odd length, or if either argument is NULL.
	if args[0] == nil || (len(args) > 1 && args[1] == nil) {
		return nil, nil
	}
	s := toString(args[0])
	var sep string
	if len(args) > 1 {
		sep = toString(args[1])
	}
	// Remove separator characters: SQLite's unhex skips any input byte that
	// appears in the separator string (byte-wise; a multi-byte separator
	// character contributes each of its UTF-8 bytes to the skip set).
	if sep != "" {
		sepSet := make(map[byte]bool, len(sep))
		for i := 0; i < len(sep); i++ {
			sepSet[sep[i]] = true
		}
		b := make([]byte, 0, len(s))
		for i := 0; i < len(s); i++ {
			if !sepSet[s[i]] {
				b = append(b, s[i])
			}
		}
		s = string(b)
	}
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
		t := strings.TrimSpace(x)
		end := 0
		if end < len(t) && (t[end] == '+' || t[end] == '-') {
			end++
		}
		for end < len(t) && t[end] >= '0' && t[end] <= '9' {
			end++
		}
		if end > 0 {
			if i, err := strconv.ParseInt(t[:end], 10, 64); err == nil {
				return i, nil
			}
			if t[0] == '-' {
				return int64(math.MinInt64), nil
			}
			return int64(math.MaxInt64), nil
		}
		return int64(0), nil
	default:
		return int64(0), nil
	}
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
