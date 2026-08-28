// Package function provides SQL scalar and aggregate functions.
package function

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"hash/crc32"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/internal/util"
	"github.com/pijalu/frigolite/internal/value"
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

	// Builtin reports whether the function was registered by the engine (true)
	// or by the application via Register (false). pragma_function_list reports
	// this as the "builtin" column.
	Builtin bool
	// Innocuous reports whether the function may be used in schema objects
	// (generated columns, CHECK, DEFAULT, index WHERE, views, triggers) even
	// when PRAGMA trusted_schema=OFF (SQLITE_INNOCUOUS).
	Innocuous bool
	// DirectOnly reports whether the function may only be called directly by
	// SQL (SQLITE_DIRECTONLY): it is never allowed in schema objects,
	// regardless of trusted_schema.
	DirectOnly bool
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

// List returns all registered functions (for pragma_function_list).
func (r *Registry) List() []*Func {
	out := make([]*Func, 0, len(r.funcs))
	for _, f := range r.funcs {
		out = append(out, f)
	}
	return out
}

func (r *Registry) register(f *Func) {
	r.funcs[strings.ToUpper(f.Name)] = f
}

// Register adds a scalar function to the registry (used for engine-specific
// functions like SQLite's internal sqlite_rename_quotefix). Application
// functions are not "builtin" in pragma_function_list.
func (r *Registry) Register(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int) {
	r.register(&Func{Name: name, Type: TypeScalar, MinArgs: minArgs, MaxArgs: maxArgs, ScalarFn: fn})
}

// RegisterFlags adds a scalar function with SQLite function-safety flags
// (SQLITE_INNOCUOUS / SQLITE_DIRECTONLY), which control whether the function
// may appear in schema objects under PRAGMA trusted_schema (trustschema1).
func (r *Registry) RegisterFlags(name string, fn func(args []interface{}) (interface{}, error), minArgs, maxArgs int, innocuous, directOnly bool) {
	r.register(&Func{Name: name, Type: TypeScalar, MinArgs: minArgs, MaxArgs: maxArgs, ScalarFn: fn, Innocuous: innocuous, DirectOnly: directOnly})
}

// RegisterAggregate adds an aggregate function: newAgg creates per-group
// state, Step consumes each input row and Final produces the group result
// (zipfile.c's zipfile() aggregate form).
func (r *Registry) RegisterAggregate(name string, minArgs, maxArgs int, newAgg func() Aggregator) {
	r.register(&Func{Name: name, Type: TypeAggregate, MinArgs: minArgs, MaxArgs: maxArgs, AggregateFn: newAgg})
}

// SchemaSafe reports whether a function may be used in a schema object under
// the given trusted_schema setting: builtin and innocuous functions are
// always allowed; directonly functions are never allowed; other user
// functions are allowed only when trusted_schema is ON.
func (r *Registry) SchemaSafe(name string, trustedSchema bool) bool {
	f, ok := r.Find(name)
	if !ok {
		// Unknown functions resolve at runtime to NULL in SQLite; schema
		// objects referencing them are not rejected here.
		return true
	}
	if f.DirectOnly {
		return false
	}
	if f.Builtin || f.Innocuous || trustedSchema {
		return true
	}
	return false
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
	r.register(&Func{Name: "CHAR", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnCHAR})
	r.register(&Func{Name: "NULLIF", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnNULLIF})
	r.register(&Func{Name: "PRINTF", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnPRINTF})
	r.register(&Func{Name: "GLOB", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnGLOB})
	// LIKE is both an operator and a function (LIKE(X, Y [, Z])). The scalar
	// registration makes the name resolvable; the evaluator routes the
	// function form through the operator implementation (case-sensitivity and
	// ESCAPE handling).
	r.register(&Func{Name: "LIKE", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnLIKE})
	// prefix_length(A, B) — the SQLite test-harness function that returns the
	// length of the common prefix of A and B (used by prefixes.test).
	r.register(&Func{Name: "PREFIX_LENGTH", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnPREFIXLENGTH})
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
	r.register(&Func{Name: "STRDUP", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSTRDUP, Innocuous: true})

	// Compile-time option diagnostics (sqlite_compileoption_used/get), ported
	// from SQLite's ctime.c. Fixed arity 1; SQLite enforces this at prepare.
	r.register(&Func{Name: "SQLITE_COMPILEOPTION_USED", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCompileOptionUsed, WrongArgMsg: true})
	r.register(&Func{Name: "SQLITE_COMPILEOPTION_GET", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnCompileOptionGet, WrongArgMsg: true})
	// md5sum is a test-harness aggregate (SQLite's test_config.c registers it
	// as an aggregate that MD5-hashes the concatenation of its arguments per
	// row). Used by trans/trans2 signature checks: SELECT md5sum(u1) ...
	r.register(&Func{Name: "MD5SUM", Type: TypeAggregate, MinArgs: 1, MaxArgs: -1, AggregateFn: fnMD5SUM})

	// Extension/compat functions
	r.register(&Func{Name: "TOINTEGER", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnTOINTEGER})
	r.register(&Func{Name: "FORMAT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnPRINTF})
	r.register(&Func{Name: "CONCAT_WS", Type: TypeScalar, MinArgs: 2, MaxArgs: -1, ScalarFn: fnCONCATWS, WrongArgMsg: true})
	r.register(&Func{Name: "EDITDIST3", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnEDITDIST3})
	r.register(&Func{Name: "SPELLFIX1_SCRIPTCODE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSPELLFIX1SCRIPTCODE})
	// Decimal extension (ext/misc/decimal.c port, see decimal.go)
	r.register(&Func{Name: "DECIMAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnDECIMAL})
	r.register(&Func{Name: "DECIMAL_EXP", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnDECIMALEXP})
	r.register(&Func{Name: "DECIMAL_CMP", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALCMP})
	r.register(&Func{Name: "DECIMAL_ADD", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALADD})
	r.register(&Func{Name: "DECIMAL_SUB", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALSUB})
	r.register(&Func{Name: "DECIMAL_MUL", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnDECIMALMUL})
	r.register(&Func{Name: "DECIMAL_POW2", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnDECIMALPOW2})
	r.register(&Func{Name: "DECIMAL_SUM", Type: TypeAggregate, MinArgs: 1, MaxArgs: 1, AggregateFn: newDecimalSumAgg})
	// JSON constructors and validation follow SQLite JSON1 compact serialization.
	r.register(&Func{Name: "JSON", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSON})
	r.register(&Func{Name: "JSONB", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSON_B})
	r.register(&Func{Name: "JSON_OBJECT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_OBJECT})
	r.register(&Func{Name: "JSONB_OBJECT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_OBJECT})
	r.register(&Func{Name: "JSON_ARRAY", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_ARRAY})
	r.register(&Func{Name: "JSONB_ARRAY", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_ARRAY})
	// JSON1 core functions (see json.go). Argument counts are validated
	// inside the functions to match SQLite: json_extract accepts 0+ args
	// (0/1 args validate-and-return-NULL, multiple paths return an array)
	// and json_insert requires an odd number of args ("needs an odd number
	// of arguments"). Innocuous so they may be used in expression indexes
	// (CREATE INDEX ... ON t(json_extract(j,'$.x'))).
	r.register(&Func{Name: "JSON_EXTRACT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_EXTRACT, Innocuous: true})
	r.register(&Func{Name: "JSON_INSERT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_INSERT, Innocuous: true})
	r.register(&Func{Name: "JSON_SET", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_SET, Innocuous: true})
	r.register(&Func{Name: "JSON_REPLACE", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: fnJSON_REPLACE, Innocuous: true})
	r.register(&Func{Name: "JSON_REMOVE", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnJSON_REMOVE, Innocuous: true})
	r.register(&Func{Name: "JSON_ARRAY_INSERT", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnJSON_ARRAY_INSERT, Innocuous: true})
	// subtype(X): test-suite helper mirroring sqlite3_value_subtype —
	// 1 when X carries the returned-JSON (or JSONB blob) subtype, else 0.
	r.register(&Func{Name: "SUBTYPE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnSUBTYPE, Innocuous: true})
	r.register(&Func{Name: "JSON_VALID", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnJSON_VALID, Innocuous: true})
	r.register(&Func{Name: "JSON_ERROR_POSITION", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSON_ERROR_POSITION, Innocuous: true})
	r.register(&Func{Name: "JSON_TYPE", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnJSON_TYPE, Innocuous: true})
	r.register(&Func{Name: "JSON_QUOTE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnJSON_QUOTE, Innocuous: true, WrongArgMsg: true})
	r.register(&Func{Name: "JSON_ARRAY_LENGTH", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnJSON_ARRAY_LENGTH, Innocuous: true})
	r.register(&Func{Name: "JSON_GROUP_ARRAY", Type: TypeAggregate, MinArgs: 0, MaxArgs: 1, AggregateFn: func() Aggregator { return &jsonGroupArrayAgg{} }, Innocuous: true})
	r.register(&Func{Name: "JSON_GROUP_OBJECT", Type: TypeAggregate, MinArgs: 2, MaxArgs: 2, AggregateFn: func() Aggregator { return &jsonGroupObjectAgg{} }, Innocuous: true})
	r.register(&Func{Name: "JSON_PRETTY", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnJSON_PRETTY, Innocuous: true})
	r.register(&Func{Name: "JSON_PATCH", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnJSON_PATCH, Innocuous: true})
	// jsonb_ variants of the edit/extract functions return a JSONB BLOB
	// instead of JSON text (src/json.c JFUNCTION table).
	r.register(&Func{Name: "JSONB_INSERT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: jsonbWrap(fnJSON_INSERT), Innocuous: true})
	r.register(&Func{Name: "JSONB_SET", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: jsonbWrap(fnJSON_SET), Innocuous: true})
	r.register(&Func{Name: "JSONB_REPLACE", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: jsonbWrap(fnJSON_REPLACE), Innocuous: true})
	r.register(&Func{Name: "JSONB_REMOVE", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: jsonbWrap(fnJSON_REMOVE), Innocuous: true})
	r.register(&Func{Name: "JSONB_PATCH", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: jsonbWrap(fnJSON_PATCH), Innocuous: true})
	r.register(&Func{Name: "JSONB_EXTRACT", Type: TypeScalar, MinArgs: 0, MaxArgs: -1, ScalarFn: jsonbWrap(fnJSON_EXTRACT), Innocuous: true})
	r.register(&Func{Name: "JSONB_GROUP_ARRAY", Type: TypeAggregate, MinArgs: 0, MaxArgs: 1, AggregateFn: func() Aggregator { return &jsonbAgg{inner: &jsonGroupArrayAgg{}} }, Innocuous: true})
	r.register(&Func{Name: "JSONB_GROUP_OBJECT", Type: TypeAggregate, MinArgs: 2, MaxArgs: 2, AggregateFn: func() Aggregator { return &jsonbAgg{inner: &jsonGroupObjectAgg{}} }, Innocuous: true})
	// random_json(SEED)/random_json5(SEED): Go port of the randomjson.c
	// loadable extension (ext/misc/randomjson.c) used by json106/json108.
	r.register(&Func{Name: "RANDOM_JSON", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnRANDOM_JSON(false), Innocuous: true})
	r.register(&Func{Name: "RANDOM_JSON5", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnRANDOM_JSON(true), Innocuous: true})

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
	r.register(&Func{Name: "CONCAT", Type: TypeScalar, MinArgs: 1, MaxArgs: -1, ScalarFn: fnCONCAT, WrongArgMsg: true})
	r.register(&Func{Name: "SUBSTRING", Type: TypeScalar, MinArgs: 2, MaxArgs: 3, ScalarFn: fnSUBSTR})
	r.register(&Func{Name: "UNISTR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNISTR})
	r.register(&Func{Name: "UNISTR_QUOTE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnUNISTRQUOTE})
	r.register(&Func{Name: "NEXT_CHAR", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnNEXTCHAR})
	r.register(&Func{Name: "INT2HEX", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnINT2HEX})
	r.register(&Func{Name: "PREFIX_LENGTH", Type: TypeScalar, MinArgs: 2, MaxArgs: 2, ScalarFn: fnPREFIXLENGTH})
	// Loadable-extension functions (ports of SQLite ext/misc/*.c, see
	// extension.go). basexx.c: base64/base85/is_base85; fileio.c:
	// readfile/writefile; ieee754.c: ieee754 family; decimal.c: decimal
	// family; src/test_loadext.c: sqlite3_status.
	r.register(&Func{Name: "BASE64", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnBASE64})
	r.register(&Func{Name: "BASE85", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnBASE85})
	r.register(&Func{Name: "IS_BASE85", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnISBASE85})
	r.register(&Func{Name: "READFILE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnREADFILE})
	r.register(&Func{Name: "WRITEFILE", Type: TypeScalar, MinArgs: 2, MaxArgs: 4, ScalarFn: fnWRITEFILE})
	r.register(&Func{Name: "Ieee754_mantissa", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754Mantissa})
	r.register(&Func{Name: "Ieee754_exponent", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754Exponent})
	r.register(&Func{Name: "Ieee754_to_blob", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754ToBlob})
	r.register(&Func{Name: "Ieee754_to_int", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754ToInt})
	r.register(&Func{Name: "Ieee754_from_int", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnIeee754FromInt})
	r.register(&Func{Name: "SQLITE3_STATUS", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnSQLite3Status})
	r.register(&Func{Name: "FIRST_VALUE", Type: TypeScalar, MinArgs: 1, MaxArgs: 1, ScalarFn: fnFIRSTVALUE})
	r.register(&Func{Name: "LAST_INSERT_ROWID", Type: TypeScalar, MinArgs: 0, MaxArgs: 0, ScalarFn: fnLASTINSERTROWID})
	r.register(&Func{Name: "LOAD_EXTENSION", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnLOADEXTENSION})
	r.register(&Func{Name: "EVAL", Type: TypeScalar, MinArgs: 1, MaxArgs: 2, ScalarFn: fnEVALSTUB})
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
	r.register(&Func{Name: "IF", Type: TypeScalar, MinArgs: 2, MaxArgs: -1, ScalarFn: fnIfIIf})
	r.register(&Func{Name: "IIF", Type: TypeScalar, MinArgs: 2, MaxArgs: -1, ScalarFn: fnIfIIf})
	// All functions registered by registerDefaults are builtin.
	for _, f := range r.funcs {
		f.Builtin = true
	}
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

// fnTestZeroblob is the TCL test-harness test_zeroblob(N): like zeroblob but
// without the SQLITE_MAX_LENGTH check (test1.c test_zeroblob). Negative
// lengths return the empty blob.

// fnAFFINITY implements the test-only affinity() function from the SQLite TCL
// test suite. It reports the affinity of the column its argument refers to
// (integer/real/text/blob/none): a ColumnValue wrapper carries the declared
// column affinity (from a table scan or materialized subquery/CTE column),
// while a bare value reports its storage class (the fallback for literals).

func fnNULLIF(args []interface{}) (interface{}, error) {
	if util.CompareValues(args[0], args[1]) == 0 {
		return nil, nil
	}
	return args[0], nil
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

// fnLIKE is the registered scalar for the LIKE(X, Y [, Z]) function form.
// The evaluator routes this name to the operator implementation (which honors
// case-sensitivity and the optional ESCAPE argument); this fallback returns
// NULL when the evaluator did not intercept (defensive only).
func fnLIKE(args []interface{}) (interface{}, error) {
	return nil, nil
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
	if z, ok := v.(value.ZeroBlob); ok {
		return string(z.Bytes())
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

// fnSTRDUP implements the strdup loadable extension (ext/misc/strdup.c):
// it returns a duplicate of the argument via sqlite3_value_dup semantics —
// the copy PRESERVES the value's type (a JSONB BLOB stays a BLOB, so
// json_each(strdup(x'CC..')) keeps the Bug-2026-07-04 corrupt-JSONB path,
// json101-26.1b). NULL stays NULL.
func fnSTRDUP(args []interface{}) (interface{}, error) {
	v := util.UnwrapColumnValue(args[0])
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	return v, nil
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
	case value.ZeroBlob:
		return x.Bytes()
	default:
		return []byte(fmt.Sprintf("%v", x))
	}
}

// --- Extension/stub functions ---

func fnSPELLFIX1SCRIPTCODE(args []interface{}) (interface{}, error) {
	// Stub: return empty string
	return "", nil
}

func fnJSON(args []interface{}) (interface{}, error) {
	if len(args) != 1 || args[0] == nil {
		return nil, nil
	}
	n, err := parseJSONArg(args[0])
	if err != nil {
		return nil, err
	}
	return JSONText(jsonSerialize(n)), nil
}

func fnJSON_ARRAY(args []interface{}) (interface{}, error) {
	n := &jsonNode{kind: jsonArray, arr: make([]*jsonNode, 0, len(args))}
	for _, arg := range args {
		v, err := jsonInsertValue(arg)
		if err != nil {
			return nil, err
		}
		n.arr = append(n.arr, v)
	}
	return JSONText(jsonSerialize(n)), nil
}

func fnJSON_OBJECT(args []interface{}) (interface{}, error) {
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("json_object() requires an even number of arguments")
	}
	n := &jsonNode{kind: jsonObject, obj: make([]jsonPair, 0, len(args)/2)}
	for i := 0; i < len(args); i += 2 {
		switch util.UnwrapColumnValue(args[i]).(type) {
		case nil:
			return nil, fmt.Errorf("json_object() labels must be TEXT")
		case string, JSONText, []byte:
			// text labels are fine
		default:
			// INTEGER/REAL labels are rejected (src/json.c jsonObjectFunc).
			return nil, fmt.Errorf("json_object() labels must be TEXT")
		}
		v, err := jsonInsertValue(args[i+1])
		if err != nil {
			return nil, err
		}
		n.obj = append(n.obj, jsonPair{key: fmt.Sprint(args[i]), value: v})
	}
	return JSONText(jsonSerialize(n)), nil
}

// --- Math function implementations ---

// mathOneArg evaluates a one-argument math function the way SQLite's math
// extension does: NULL input yields NULL, a NaN result (e.g. sqrt(-1),
// asin(2)) yields NULL, and +-Inf results pass through (exp(1000) is Inf).
// numericArg converts an argument to a float64, reporting whether it is
// numeric (INTEGER, REAL, or numeric-looking TEXT — sqlite3_value_numeric_type
// semantics used by SQLite's math functions). Non-numeric text/blobs return
// false so the caller yields NULL.
// mathLogArg evaluates the log-family functions, which additionally return
// NULL for x<=0 (SQLite: log of zero or a negative number is NULL, not -Inf
// or NaN).
// mathRoundArg evaluates floor/ceil, which SQLite returns as INTEGER when the
// input was INTEGER (floor(17) → 17) and REAL otherwise (floor(17.5) → 17.0,
// ceil(99.9) → 100.0, ceil('-99.99') → -99.0).

// fnERROR raises a "SQL error!" exception. Registered to support the test
// harness's `error()` user-defined function (regexp2.test). The exact message
// "SQL error!" is asserted by regexp2.test ({1 {SQL error!}}) — keep it.
func fnERROR(args []interface{}) (interface{}, error) {
	//lint:ignore ST1005 message text is test-asserted (SQLite test-compat)
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
// fnAddIntType forces its argument to the INTEGER storage class
// (sqlite3_value_int64 in the SQLite test harness). NULL passes through.
// fnAddRealType forces its argument to the REAL storage class
// (sqlite3_value_double in the SQLite test harness). NULL passes through.
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

// fnVALUES implements the VALUES(...) function used as a scalar expression.
// SQLite's VALUES returns the first value when used as a scalar function.
func fnVALUES(args []interface{}) (interface{}, error) {
	if len(args) >= 1 {
		return args[0], nil
	}
	return nil, nil
}
