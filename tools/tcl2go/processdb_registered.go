// Package main implements the tcl2go tool.
//
// This file emits RegisterFunction calls for the collected test-suite proc
// patterns (emitRegisteredFunction): recorder, sleeper, const, string-const,
// string-map, identity, lindex, incr-return, counter, int2str, join,
// my_changes, prefix, predicate and error-raising procs.
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// emitRegisteredFunction emits a RegisterFunction call for a recognized
// test-suite proc pattern. Returns true when a pattern matched. The match
// order (mapped patterns first, then the collected registries) is
// significant — earlier registrations win.
func (tp *transpiler) emitRegisteredFunction(name, procName string, rest []tcl.RawWord) bool {
	if tp.emitRegisteredFuncMapped(name, procName, rest) {
		return true
	}
	return tp.emitRegisteredFuncCollected(name, procName, rest)
}

// emitRegisteredFuncMapped runs the first half of the recognized proc
// patterns (registry-scan based matchers).
func (tp *transpiler) emitRegisteredFuncMapped(name, procName string, rest []tcl.RawWord) bool {
	// Recorder proc: `proc trigfunc {args} { set ::TRIGGER $args }` becomes a
	// scalar SQL function replacing the Go variable with the TCL rendering of
	// its arguments (alter.test alter-3.1.x/3.3.x trigger probes).
	if tp.emitRecorderFunctionIfMatched(name, procName) {
		return true
	}
	if tp.emitSleeperFunction(name, procName) {
		return true
	}
	if tp.emitConstFunction(name, procName) {
		return true
	}
	if tp.emitStringConstFunction(name, procName) {
		return true
	}
	if tp.emitStringMapFunction(name, procName) {
		return true
	}
	if tp.emitIdentityFunction(name, procName, rest) {
		return true
	}
	if tp.emitLIndexFunction(name, procName) {
		return true
	}
	if tp.emitIncrRetFunction(name, procName, rest) {
		return true
	}
	if tp.emitCounterFunction(name, procName) {
		return true
	}
	return false
}

// emitRegisteredFuncCollected runs the second half of the recognized proc
// patterns (fixed-name harness procs and body-shape matchers).
func (tp *transpiler) emitRegisteredFuncCollected(name, procName string, rest []tcl.RawWord) bool {
	// db func int2str int2str — the test-harness int2str builds a
	// 900-char deterministic string from its integer argument.
	if procName == "int2str" && name != "" {
		tp.emitInt2strFunction(name)
		return true
	}
	// db func NAME {joinx PREFIX} — the join proc is called with a
	// literal prefix plus the SQL arguments (func8.test's cross/full/
	// inner/... functions): cross(a,b,c) → "cross-a-b-c".
	if tp.emitJoinFunction(name, procName, rest) {
		return true
	}
	// db func my_changes my_changes — the e_changes.test harness proc:
	// `proc my_changes {x} { set res [db changes]; lappend ::changes $x
	// $res; return $res }`. The SQL function returns the connection's
	// changes() count and records the (arg, count) pair in the ::changes
	// TCL global (verified by do_test 5.1.2).
	if procName == "my_changes" && name != "" {
		tp.emitMyChangesFunction(name)
		return true
	}
	// db func NAME NAME — a prefix proc (window6.test's winproc):
	// window('hello world') → "window: hello world".
	if tp.emitPrefixFunction(name, procName) {
		return true
	}
	// Predicate proc: `proc myfunc {x} {expr $x < 10}` becomes a
	// scalar SQL function applying the comparison to its first
	// argument (numeric).
	if tp.emitPredFunction(name, procName) {
		return true
	}
	// Format proc: `proc int2hex {i} { format %.2X $i }` (rollback2)
	// becomes a scalar SQL function applying the printf verb to its
	// integer argument.
	if tp.emitFormatFunction(name, procName) {
		return true
	}
	// Error-raising proc: `proc NAME {} { error "MSG" }` becomes a
	// scalar SQL function that returns the error (regexp2.test's
	// `proc sql_error {} { error "SQL error!" }` registered as
	// `db func error sql_error`).
	if msg, ok := tp.errorFuncs[procName]; ok && name != "" {
		tp.emitErrorFunction(name, msg)
		return true
	}
	return false
}

// emitSleeperFunction registers the sleeper proc (`proc sleeper {} {after
// 100}`), which pauses 100ms and returns NULL. It is used by date.test to
// verify that 'now' is cached per statement across a user-function sleep.
func (tp *transpiler) emitSleeperFunction(name, procName string) bool {
	if procName != "sleeper" || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { time.Sleep(100 * time.Millisecond); return nil, nil }, 0, -1)", tp.dbVar, name)
	return true
}

// emitMyChangesFunction registers the e_changes.test my_changes harness
// function: `proc my_changes {x} { set res [db changes]; lappend ::changes $x
// $res; return $res }`. The SQL function returns the connection's changes()
// count and appends "(arg, count)" to the ::changes TCL-global variable
// (verified by do_test 5.1.2). The Go variable for ::changes is `changes`.
func (tp *transpiler) emitMyChangesFunction(name string) {
	tp.emitLine("// db func %s: my_changes (returns db changes, logs to ::changes)", name)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tv := db.Changes()")
	tp.emitLine("\targ := \"\"")
	tp.emitLine("\tif len(args) > 0 { arg = tclStr(args[0]) }")
	tp.emitLine("\tchanges = tclListAppend(changes, arg, strconv.FormatInt(v, 10))")
	tp.emitLine("\treturn v, nil")
	tp.emitLine("}, 0, -1)")
}

// emitConstFunction registers a constant-returning proc as a scalar SQL
// function returning the constant.
func (tp *transpiler) emitConstFunction(name, procName string) bool {
	constVal, ok := tp.constFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return int64(%s), nil }, 0, -1)", tp.dbVar, name, constVal)
	return true
}

// emitStringConstFunction registers a fixed-string-returning proc
// (`proc target {} { return "test.db2" }` — vacuum-into-410's VACUUM INTO
// target() filename) as a scalar SQL function returning the constant. The
// proc body is resolved by the pre-pass (collectStringConstFuncs), so the
// registration may appear BEFORE the proc definition in the test file.
func (tp *transpiler) emitStringConstFunction(name, procName string) bool {
	constVal, ok := tp.stringConstFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { return %q, nil }, 0, -1)", tp.dbVar, name, constVal)
	return true
}

// emitIdentityFunction registers an identity proc (`proc NAME {x} {return
// $x}`) as a scalar SQL function returning its first argument. It honors the
// SQLite function-safety flags in `db function NAME [-innocuous]
// [-directonly] [-deterministic] PROC` by emitting RegisterFunctionFlags
// (trustschema1's f1/f2/f3).
func (tp *transpiler) emitIdentityFunction(name, procName string, rest []tcl.RawWord) bool {
	if !tp.identityFuncs[procName] || name == "" {
		return false
	}
	innocuous, directOnly := dbFunctionSafetyFlags(rest)
	flags := "false, false"
	if innocuous && !directOnly {
		flags = "true, false"
	} else if directOnly {
		flags = "false, true"
	}
	tp.emitLine("%s.RegisterFunctionFlags(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn args[0], nil")
	tp.emitLine("}, 0, -1, %s)", flags)
	return true
}

// dbFunctionSafetyFlags extracts the SQLite function-safety flags from a
// `db function NAME [flags] PROC` argument list: -innocuous and -directonly
// (SQLITE_INNOCUOUS / SQLITE_DIRECTONLY).
func dbFunctionSafetyFlags(rest []tcl.RawWord) (innocuous, directOnly bool) {
	for _, a := range rest[1:] {
		switch strings.ToLower(strings.TrimSpace(a.Text)) {
		case "-innocuous":
			innocuous = true
		case "-directonly":
			directOnly = true
		}
	}
	return innocuous, directOnly
}

// emitLIndexFunction registers a list-index proc (`proc NAME {x} { lindex $x
// N }`) as a scalar SQL function returning the N-th element of its first
// argument split as a TCL list (fts4growth.test's second: "0 114" → "114").
func (tp *transpiler) emitLIndexFunction(name, procName string) bool {
	idx, ok := tp.lindexFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn tclLIndex(tclStr(args[0]), %d), nil", idx)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitStringMapFunction registers a string-map proc (`proc NAME {x} {
// return [string map {OLD NEW ...} $x] }`) as a scalar SQL function that
// applies each OLD→NEW replacement in order (fts4intck1.test's slang:
// th→d, e→eh makes 'the' → 'deh'). TCL string map applies left-to-right on
// the current value, so chained strings.ReplaceAll is faithful.
func (tp *transpiler) emitStringMapFunction(name, procName string) bool {
	pairs, ok := tp.stringMapFuncs[procName]
	if !ok || name == "" {
		return false
	}
	items := tclCmdWords(pairs)
	if len(items) < 2 || len(items)%2 != 0 {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\ts := tclStr(args[0])")
	for i := 0; i+1 < len(items); i += 2 {
		tp.emitLine("\ts = strings.ReplaceAll(s, %q, %q)", items[i], items[i+1])
	}
	tp.emitLine("\treturn s, nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// emitCounterFunction registers a counter proc as a scalar SQL function that
// increments a dedicated Go counter var.
func (tp *transpiler) emitCounterFunction(name, procName string) bool {
	goVar, ok := tp.counterFuncs[procName]
	if !ok || name == "" {
		return false
	}
	counterVar := goVar + "Counter"
	tp.emitLine("var %s int64", counterVar)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) { %s++; return %s, nil }, 0, -1)", tp.dbVar, name, counterVar, counterVar)
	return true
}

// emitJoinFunction registers a join proc called with a literal prefix plus
// the SQL arguments (func8.test's cross/full/inner/... functions).
func (tp *transpiler) emitJoinFunction(name, procName string, rest []tcl.RawWord) bool {
	sep, ok := tp.joinFuncs[procName]
	if !ok || name == "" {
		return false
	}
	prefix := joinPrefixFromRest(rest, procName)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tvar parts []string")
	tp.emitLine("\tparts = append(parts, %q)", prefix)
	tp.emitLine("\tfor _, a := range args { if a != nil { parts = append(parts, tclStr(a)) } }")
	tp.emitLine("\treturn strings.Join(parts, %q), nil", sep)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitPrefixFunction registers a prefix proc as a scalar SQL function that
// joins its args with a space and prepends the fixed prefix (window6.test's
// winproc: window('hello world') → "window: hello world").
func (tp *transpiler) emitPrefixFunction(name, procName string) bool {
	prefix, ok := tp.prefixFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tvar parts []string")
	tp.emitLine("\t// TCL \"$args\" substitutes the argument LIST's string form:")
	tp.emitLine("\t// each element is list-quoted — a multi-word element arrives")
	tp.emitLine("\t// braced (Tcl list rendering), e.g. ('hello world') → {hello world}.")
	tp.emitLine("\tfor _, a := range args {")
	tp.emitLine("\t\tif a == nil { continue }")
	tp.emitLine("\t\tel := tclStr(a)")
	tp.emitLine("\t\tif el != \"\" && !strings.ContainsAny(el, \" \\t{}\\\\\") {")
	tp.emitLine("\t\t\tparts = append(parts, el)")
	tp.emitLine("\t\t} else if !strings.ContainsAny(el, \"\\n\") && tclBracesBalanced(el) {")
	tp.emitLine("\t\t\tparts = append(parts, \"{\"+el+\"}\")")
	tp.emitLine("\t\t} else {")
	tp.emitLine("\t\t\tparts = append(parts, el)")
	tp.emitLine("\t\t}")
	tp.emitLine("\t}")
	tp.emitLine("\treturn %q + strings.Join(parts, \" \"), nil", prefix)
	tp.emitLine("}, 0, -1)")
	return true
}

// emitPredFunction registers a predicate proc as a scalar SQL function
// applying the comparison to its first argument (numeric).
func (tp *transpiler) emitPredFunction(name, procName string) bool {
	pred, ok := tp.predFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\targ, _ := strconv.ParseFloat(tclStr(args[0]), 64)")
	tp.emitLine("\tif %s { return int64(1), nil }", pred)
	tp.emitLine("\treturn int64(0), nil")
	tp.emitLine("}, 0, -1)")
	return true
}

// emitInt2strFunction registers the test-harness int2str scalar function.
func (tp *transpiler) emitInt2strFunction(name string) {
	tp.emitLine("%s.RegisterFunction(\"int2str\", func(args []interface{}) (interface{}, error) {", tp.dbVar)
	tp.emitLine("\tif len(args) < 1 || args[0] == nil { return nil, nil }")
	tp.emitLine("\treturn tclInt2str(args[0]), nil")
	tp.emitLine("}, 0, -1)")
}

// emitErrorFunction registers an error-raising scalar SQL function.
func (tp *transpiler) emitErrorFunction(name, msg string) {
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\treturn nil, fmt.Errorf(%q)", msg)
	tp.emitLine("}, 0, -1)")
}

// emitIncrRetFunction registers `db func OP -argcount N PROC` where PROC's
// body is `incr ::VAR [AMOUNT]; return RET`: the closure increments the Go
// variable mirroring ::VAR and returns RET, so harness counters observe one
// invocation per TRUE operator evaluation (vtabH 2.x).
func (tp *transpiler) emitIncrRetFunction(name, procName string, rest []tcl.RawWord) bool {
	info, ok := tp.incrRetFuncs[procName]
	if !ok || name == "" {
		return false
	}
	arityLo, arityHi := 0, -1
	if n, has := dbFuncArgCount(rest); has {
		arityLo, arityHi = n, n
	}
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tif n, err := strconv.Atoi(%s); err == nil { %s = strconv.Itoa(n + %d) }", info.GoVar, info.GoVar, info.Amount)
	tp.emitLine("\treturn int64(%d), nil", info.Ret)
	tp.emitLine("}, %d, %d)", arityLo, arityHi)
	return true
}

// emitRecorderFunctionIfMatched emits the recorder closure when procName is
// a recognized `proc P {args} { set ::V $args }` kind. Returns false when not
// a match (the caller keeps scanning other kinds).
func (tp *transpiler) emitRecorderFunctionIfMatched(name, procName string) bool {
	goVar, ok := tp.recorderFuncs[procName]
	if !ok || name == "" {
		return false
	}
	tp.emitRecorderFunction(name, goVar)
	return true
}

// emitRecorderFunction emits a scalar SQL function that replaces the named
// Go variable with the TCL list rendering of its arguments on every call
// (`proc trigfunc {args} { set ::TRIGGER $args }`, alter.test).
func (tp *transpiler) emitRecorderFunction(name, goVar string) {
	tp.emitLine("// db function %s: replaces %s with the TCL rendering of its args", name, goVar)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tparts := make([]string, 0, len(args))")
	tp.emitLine("\tfor _, a := range args {")
	tp.emitLine("\t\tparts = append(parts, tclListElem(tclStr(a)))")
	tp.emitLine("\t}")
	tp.emitLine("\t%s = tclList(parts)", goVar)
	tp.emitLine("\treturn nil, nil")
	tp.emitLine("}, 0, -1)")
}
