// Package main implements the tcl2go tool.
//
// This file registers scalar SQL functions backed by TCL procs whose bodies
// accumulate into a global variable (counters, logs): emitTclVarUDFFromProc
// recognizes four corpus body shapes and emits closures that mutate the same
// generated Go variable the assertions read back.
package main

import (
	"regexp"
)

// emitTclVarUDFFromProc registers a scalar SQL function backed by a TCL proc
// whose body accumulates into a global variable. Four corpus shapes are
// recognized (everything else returns false and keeps the stub registration):
//
//   - selectH.test: proc P {amt} { global V; incr V $amt; return $V }
//     -> UDF adds the (integer) argument to V and returns the new value.
//   - subquery.test: proc P {n} { incr ::V; return $n }
//     -> UDF adds 1 to V and returns its argument.
//   - wherelimit2.test: proc P {args} { lappend ::V {*}$args }
//     -> UDF space-appends every argument to V (TCL list accumulation).
//   - having.test: proc P {args} { incr ::V; expr {$::V % N} }
//     -> UDF adds 1 to V and returns V modulo N (having-4.2/4.3 observe
//     whether HAVING runs once per group while WHERE runs per row, so the
//     call count must be visible to the engine).
//
// The closure mutates the generated Go variable the assertions read back, so
// the side effect is observable exactly like the TCL global.
func (tp *transpiler) emitTclVarUDFFromProc(name, procName string) bool {
	body := tp.procBodies[procName]
	if body == "" {
		return false
	}
	// Shape C: lappend ::V {*}$args
	if m := tclVarUDFLappendRe.FindStringSubmatch(body); m != nil {
		goVar := tclVarToGo(m[1])
		tp.emitTclVarUDF(name, goVar, m[1], "lappend")
		return true
	}
	// Shape A: global V ... incr V $amt ... return $V
	if m := tclVarUDFGlobalIncrRe.FindStringSubmatch(body); m != nil &&
		m[1] == m[2] && m[4] == m[1] {
		goVar := tclVarToGo(m[1])
		tp.emitTclVarUDF(name, goVar, m[1], "incrReturnNew")
		return true
	}
	// Shape B: incr ::V ... return $n
	if m := tclVarUDFIncrReturnArgRe.FindStringSubmatch(body); m != nil {
		goVar := tclVarToGo(m[1])
		tp.emitTclVarUDF(name, goVar, m[1], "incrReturnArg")
		return true
	}
	// Shape D: incr ::V; expr {$::V % N}
	if m := tclVarUDFIncrModRe.FindStringSubmatch(body); m != nil && m[1] == m[2] {
		goVar := tclVarToGo(m[1])
		tp.emitTclVarUDFMod(name, goVar, m[1], m[3])
		return true
	}
	return false
}

// tclVarUDF body-shape patterns (compiled once; bodies are tiny TCL scripts).
var (
	tclVarUDFLappendRe       = regexp.MustCompile(`lappend\s+::?([A-Za-z_][A-Za-z0-9_]*)\s+\{\*\}\$args`)
	tclVarUDFGlobalIncrRe    = regexp.MustCompile(`global\s+([A-Za-z_][A-Za-z0-9_]*)[\s;]+incr\s+([A-Za-z_][A-Za-z0-9_]*)\s+\$([A-Za-z_][A-Za-z0-9_]*)[\s;]+return\s+\$([A-Za-z_][A-Za-z0-9_]*)`)
	tclVarUDFIncrReturnArgRe = regexp.MustCompile(`incr\s+::?([A-Za-z_][A-Za-z0-9_]*)[\s;]*return\s+\$([A-Za-z_][A-Za-z0-9_]*)`)
	tclVarUDFIncrModRe       = regexp.MustCompile(`incr\s+::?([A-Za-z_][A-Za-z0-9_]*)[\s;]+expr\s*\{\s*\$::?([A-Za-z_][A-Za-z0-9_]*)\s*%\s*(\d+)\s*\}`)
)

// emitTclVarUDF writes the RegisterFunction emission for the three
// variable-accumulating proc shapes recognized by emitTclVarUDFFromProc.
func (tp *transpiler) emitTclVarUDF(name, goVar, tclVar, shape string) {
	if !tp.isVarDeclared(goVar) {
		tp.emitLine("var %s = \"0\"", goVar)
		tp.vars = append(tp.vars, goVar)
	}
	tp.emitLine("// db func %s %s (TCL proc accumulating ::%s)", name, name, tclVar)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	switch shape {
	case "lappend":
		tp.emitLine("\tparts := []string{}")
		tp.emitLine("\tif %s != \"\" { parts = append(parts, %s) }", goVar, goVar)
		tp.emitLine("\tfor _, a := range args { parts = append(parts, function.ValueText(a)) }")
		tp.emitLine("\t%s = strings.Join(parts, \" \")", goVar)
		tp.emitLine("\tvtab.TclVarSet(%q, \"\", %s)", tclVar, goVar)
		tp.emitLine("\treturn %s, nil", goVar)
	case "incrReturnNew":
		tp.emitLine("\tcur := int64(0)")
		tp.emitLine("\tif n, err := strconv.ParseInt(strings.TrimSpace(%s), 10, 64); err == nil { cur = n }", goVar)
		tp.emitLine("\tamt := int64(1)")
		tp.emitLine("\tif len(args) > 0 {")
		tp.emitLine("\t\tif n, err := strconv.ParseInt(function.ValueText(args[0]), 10, 64); err == nil { amt = n }")
		tp.emitLine("\t}")
		tp.emitLine("\tcur += amt")
		tp.emitLine("\t%s = strconv.FormatInt(cur, 10)", goVar)
		tp.emitLine("\tvtab.TclVarSet(%q, \"\", %s)", tclVar, goVar)
		tp.emitLine("\treturn cur, nil")
	default: // incrReturnArg
		tp.emitLine("\tcur := int64(0)")
		tp.emitLine("\tif n, err := strconv.ParseInt(strings.TrimSpace(%s), 10, 64); err == nil { cur = n }", goVar)
		tp.emitLine("\tcur++")
		tp.emitLine("\t%s = strconv.FormatInt(cur, 10)", goVar)
		tp.emitLine("\tvtab.TclVarSet(%q, \"\", %s)", tclVar, goVar)
		tp.emitLine("\tif len(args) > 0 { return function.ValueText(args[0]), nil }")
		tp.emitLine("\treturn nil, nil")
	}
	tp.emitLine("}, 0, -1)")
}

// emitTclVarUDFMod writes the RegisterFunction emission for the having.test
// counter-modulo proc shape (proc P {args} { incr ::V; expr {$::V % N} }): a
// non-deterministic UDF that increments the generated Go variable V and
// returns the new value modulo N. having-4.2/4.3 compare its call pattern
// under HAVING (once per group) against WHERE (once per row).
func (tp *transpiler) emitTclVarUDFMod(name, goVar, tclVar, mod string) {
	if !tp.isVarDeclared(goVar) {
		tp.emitLine("var %s = \"0\"", goVar)
		tp.vars = append(tp.vars, goVar)
	}
	tp.emitLine("// db func %s %s (TCL counter UDF: incr ::%s; return ::%s %% %s)", name, name, tclVar, tclVar, mod)
	tp.emitLine("%s.RegisterFunction(%q, func(args []interface{}) (interface{}, error) {", tp.dbVar, name)
	tp.emitLine("\tcur := int64(0)")
	tp.emitLine("\tif n, err := strconv.ParseInt(strings.TrimSpace(%s), 10, 64); err == nil { cur = n }", goVar)
	tp.emitLine("\tcur++")
	tp.emitLine("\t%s = strconv.FormatInt(cur, 10)", goVar)
	tp.emitLine("\tvtab.TclVarSet(%q, \"\", %s)", tclVar, goVar)
	tp.emitLine("\treturn cur %% %s, nil", mod)
	tp.emitLine("}, 0, -1)")
}
