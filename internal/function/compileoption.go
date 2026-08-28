// Compile-time option diagnostics, mirroring SQLite's ctime.c
// (sqlite3_compileoption_used / sqlite3_compileoption_get and the
// PRAGMA compile_options report).
//
// The list advertises the options that reflect how Frigolite is actually
// built: pure Go with a mutex-serialized pager (THREADSAFE=2), the FTS
// modules, the math functions, and direct overflow-page reads.
package function

import (
	"strings"
)

// CompileOptions is the ordered list of compile-time options advertised by
// the engine. It is the single source of truth for sqlite_compileoption_used,
// sqlite_compileoption_get, and PRAGMA compile_options.
var CompileOptions = []string{
	"DIRECT_OVERFLOW_READ",
	"ENABLE_FTS3",
	"ENABLE_FTS3_PARENTHESIS",
	"ENABLE_FTS3_TOKENIZER",
	"ENABLE_FTS4",
	"ENABLE_FTS5",
	"ENABLE_MATH_FUNCTIONS",
	"THREADSAFE=2",
}

// compileOptionUsed reports whether the named compile-time option was used,
// mirroring SQLite's sqlite3_compileoption_used(): a leading "SQLITE_" prefix
// is optional, the comparison is case-insensitive, and the match must fall at
// an identifier boundary (the option string may continue with '=', '-', or
// end of string, but not with another identifier character).
func compileOptionUsed(zOptName string) bool {
	if strings.HasPrefix(strings.ToUpper(zOptName), "SQLITE_") {
		zOptName = zOptName[7:]
	}
	n := len(zOptName)
	if n == 0 {
		return false
	}
	for _, opt := range CompileOptions {
		if len(opt) >= n && strings.EqualFold(opt[:n], zOptName) {
			if len(opt) == n || !isIdChar(opt[n]) {
				return true
			}
		}
	}
	return false
}

// compileOptionGet returns the N-th compile-time option string, or "" when N
// is out of range (SQLite returns NULL).
func compileOptionGet(n int) string {
	if n >= 0 && n < len(CompileOptions) {
		return CompileOptions[n]
	}
	return ""
}

// isIdChar reports whether c is an SQLite identifier character: alphanumeric
// or underscore (sqlite3IsIdChar: isalnum || '_').
func isIdChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// fnCompileOptionUsed implements sqlite_compileoption_used(X): an integer 0
// or 1 identifying whether the compiler option was used to build the engine.
// NULL argument yields NULL (SQLite's wrapper leaves the result unset).
func fnCompileOptionUsed(args []interface{}) (interface{}, error) {
	if args[0] == nil {
		return nil, nil
	}
	return compileOptionBool(args[0]), nil
}

// compileOptionBool converts an argument to its text form and matches it
// against the compile options list.
func compileOptionBool(v interface{}) int64 {
	if compileOptionUsed(toString(v)) {
		return 1
	}
	return 0
}

// fnCompileOptionGet implements sqlite_compileoption_get(N): the N-th
// compile-time option string, or NULL when N is out of range.
func fnCompileOptionGet(args []interface{}) (interface{}, error) {
	n := 0
	switch x := args[0].(type) {
	case int64:
		n = int(x)
	case float64:
		n = int(x)
	case nil:
		n = 0
	default:
		return nil, nil
	}
	s := compileOptionGet(n)
	if s == "" {
		return nil, nil
	}
	return s, nil
}
