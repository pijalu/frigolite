package frigolite

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/pijalu/frigolite/internal/function"
)

// registerTest1Functions installs the SQL functions that testfixture's
// `sqlite3_create_function DB` command registers from src/test1.c
// (test_create_function). Tests that call the command assert C-API behavior,
// so the Go implementations mirror the observable contract of the C functions:
//
//   - x_coalesce (t1_ifnullFunc, test1.c:731) returns the first non-NULL
//     argument converted to text (sqlite3_result_text), not the value itself.
//   - pointer_change (ptrChngFunction, test1.c:914) reports whether the Mem.z
//     buffer pointer differs after applying a second conversion API. The Mem
//     holds a single buffer: value_text re-encodes a UTF-16 buffer to UTF-8,
//     value_text16 re-encodes UTF-8 to UTF-16 (both allocate a new pointer),
//     and value_blob returns the current buffer unchanged whatever its
//     encoding (ptrchng-2.5/5.3 expect 0 for text16->blob).
func registerTest1Functions(t *testing.T, db *DB) {
	t.Helper()
	db.RegisterFunction("x_coalesce", func(args []interface{}) (interface{}, error) {
		for _, arg := range args {
			if arg != nil {
				return test1ValueText(arg), nil
			}
		}
		return nil, nil
	}, -1, -1)
	db.RegisterFunction("pointer_change", pointerChange, 4, 4)
	// SQLITE_TEST UDFs that need the auxdata/context API (test_func.c's
	// test_auxdata) and the process-global counter (test1.c's
	// counter1/counter2 share ONE counter; counter1 non-deterministic,
	// counter2 deterministic).
	db.engine.Functions().RegisterContext("test_auxdata", testAuxdataFunc, -1, -1)
	db.RegisterFunction("test_error", func(args []interface{}) (interface{}, error) {
		if args[0] == nil {
			// sqlite3_result_error with sqlite3_value_text(NULL): the error
			// message is SQL NULL, which TCL renders as an empty string.
			return nil, fmt.Errorf("")
		}
		return nil, fmt.Errorf("%s", test1ValueText(args[0]))
	}, 1, 2)
	db.RegisterFunction("counter1", nondeterministicCounter, -1, -1)
	// test1.c:1149 registers counter2 with SQLITE_DETERMINISTIC: the shared
	// counter is constant-factored, once per statement run (func5-2.3).
	db.engine.Functions().RegisterDeterministic("counter2", nondeterministicCounter, -1, -1, true, false, false)
	// test_func.c's test_destructor attaches a result destructor that
	// decrements test_destructor_count_var; the count therefore reports
	// results not yet destroyed. The harness drains the pending results at
	// each step boundary (a statement's results die with the statement in
	// SQLite's C API as well).
	db.RegisterFunction("test_destructor", func(args []interface{}) (interface{}, error) {
		testDestructorPending++
		if args[0] == nil {
			return nil, nil
		}
		return test1ValueText(args[0]), nil
	}, 1, 1)
	db.RegisterFunction("test_destructor_count", func([]interface{}) (interface{}, error) {
		return int64(testDestructorPending), nil
	}, 0, 0)
	// test_func.c's test_isolation (test_isolation()): type-converts argv[0]
	// between text and UTF-16 repeatedly to prove the conversions do not
	// corrupt later arguments, then returns argv[1]. The conversions are
	// unobservable in the pure-Go value model, so only the return matters.
	db.RegisterFunction("test_isolation", func(args []interface{}) (interface{}, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("wrong number of arguments to function test_isolation()")
		}
		return args[1], nil
	}, 2, 2)
	// test1.c's test_create_aggregate registers legacy_count() to exercise
	// the deprecated sqlite3_aggregate_count() API: the step is a no-op and
	// the finalizer reports the number of aggregated rows.
	db.engine.Functions().RegisterAggregate("legacy_count", 0, 0, func() function.Aggregator {
		return &legacyCountAggregator{}
	})
	// test_func.c's abuse_create_function installs the maximum-length-name
	// no-op with the maximum parameter count: it raises
	// SQLITE_LIMIT_FUNCTION_ARG to 1000000 (sqlite3_limit clamps to the
	// hard max 127) and registers nullx… with that exact arity, so calls
	// with fewer arguments fail with "wrong number of arguments" and calls
	// beyond the limit fail with "too many arguments on function".
	db.RegisterFunction("nullx"+strings.Repeat("_123456789", 25),
		func(args []interface{}) (interface{}, error) { return nil, nil },
		harnessMaxFunctionArg, harnessMaxFunctionArg)
}

// harnessMaxFunctionArg is the engine's SQLITE_MAX_FUNCTION_ARG (the
// compile-time default 127; see internal/exec's sqliteMaxFunctionArgDefault).
const harnessMaxFunctionArg = 127

// testFuncCounter is the shared process-global counter of test1.c's
// nondeterministicFunction (counter1/counter2 both draw from it).
var testFuncCounter int64

// testDestructorPending counts test_destructor results whose destructor has
// not run yet (test_func.c's test_destructor_count_var). The harness drains
// it at each step boundary — a statement's results are destroyed when the
// statement completes.
var testDestructorPending int

// drainTestDestructors runs the pending result destructors. test_func.c
// attaches the destructor to the result value; the value dies with the
// statement that produced it, so the harness drains between steps.
func drainTestDestructors() { testDestructorPending = 0 }

// legacyCountAggregator ports test1.c's legacyCountStep/legacyCountFinalize:
// the step is a no-op and the finalizer reports sqlite3_aggregate_count() —
// the number of rows aggregated.
type legacyCountAggregator struct{ n int64 }

// Step counts one aggregated row.
func (a *legacyCountAggregator) Step([]interface{}) error { a.n++; return nil }

// Final returns the aggregate row count.
func (a *legacyCountAggregator) Final() (interface{}, error) { return a.n, nil }

// nondeterministicCounter returns an ascending integer per call.
func nondeterministicCounter(args []interface{}) (interface{}, error) {
	n := testFuncCounter
	testFuncCounter++
	return n, nil
}

// testAuxdataFunc ports test_func.c's test_auxdata: for each non-NULL
// argument, report whether the auxdata slot at that index already held a
// value ('1') or not ('0'), then store the argument's text in the slot. The
// C builds one "d " pair per non-NULL argument and trims the final space
// through the result length; pairs for NULL arguments are omitted entirely.
func testAuxdataFunc(ctx *function.FuncContext, args []interface{}) (interface{}, error) {
	var b strings.Builder
	for i, arg := range args {
		if arg == nil {
			continue
		}
		if ctx.GetAuxData(i) != nil {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte(' ')
		ctx.SetAuxData(i, test1ValueText(arg), nil)
	}
	return strings.TrimRight(b.String(), " "), nil
}

// test1ValueText renders a SQL value the way sqlite3_value_text would: numbers
// get their canonical text form and blobs their raw bytes as text.
func test1ValueText(v interface{}) string {
	switch value := v.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		if value == math.Trunc(value) && math.Abs(value) < 1e15 {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		if value {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprint(v)
	}
}

// test1TextArg extracts a text argument for pointer_change; ok is false for
// NULL (the C function returns SQL NULL for NULL api arguments).
func test1TextArg(v interface{}) (string, bool) {
	if v == nil {
		return "", false
	}
	return test1ValueText(v), true
}

// pointerChange mirrors test1.c ptrChngFunction: args are
// (value, api1, middleOp, api2); it returns 1 when the buffer pointer changes
// between api1 and api2, 0 when it does not, and NULL for unknown api names.
func pointerChange(args []interface{}) (interface{}, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("wrong number of arguments to function pointer_change()")
	}
	api1, ok1 := test1TextArg(args[1])
	op, ok2 := test1TextArg(args[2])
	api2, ok3 := test1TextArg(args[3])
	if !ok1 || !ok2 || !ok3 {
		return nil, nil
	}
	knownAPI := func(api string) bool { return api == "text" || api == "blob" || api == "text16" }
	knownOp := op == "noop" || op == "bytes" || op == "bytes16"
	if !knownAPI(api1) || !knownAPI(api2) || !knownOp {
		return nil, nil
	}
	state := -1 // -1 numeric (not yet a string), 0 eight-bit buffer, 1 UTF-16 buffer
	switch args[0].(type) {
	case string, []byte:
		state = 0
	}
	version := 0
	apply := func(api string) {
		switch api {
		case "blob":
			// sqlite3_value_blob returns Mem.z unchanged once the value is a
			// string of any encoding; only a non-string value is stringified.
			if state == -1 {
				state, version = 0, version+1
			}
		case "text":
			if state != 0 {
				state, version = 0, version+1
			}
		case "text16":
			if state != 1 {
				state, version = 1, version+1
			}
		}
	}
	// The middle operation (noop/bytes/bytes16) does not re-encode the values
	// these tests use, so it never changes the pointer.
	apply(api1)
	before := version
	apply(api2)
	if before != version {
		return int64(1), nil
	}
	return int64(0), nil
}
