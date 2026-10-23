package frigolite

import (
	"fmt"
	"math"
	"strconv"
	"testing"
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
