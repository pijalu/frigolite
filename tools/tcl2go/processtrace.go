// Package main implements the tcl2go tool.
//
// This file contains the named-connection trace/profile/busy handlers
// (dbN busy/trace/profile/trace_v2) and their proc-body recognizers.
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

const exec_trace_stmt = 1
const exec_trace_profile = 2
const exec_trace_row = 4
const exec_trace_close = 8

// procBodyFor resolves the body of a named proc for hook registration: the
// global body mirror first, then the transpiler's per-test map.
func (tp *transpiler) procBodyFor(name string) string {
	body := globalProcBodies[name]
	if body == "" && tp.procBodies != nil {
		body = tp.procBodies[name]
	}
	return body
}

// emitBusyBodyAppend emits the busy-callback body statement that records the
// retry count into the TCL global's Go variable (set overwrites, lappend
// appends).
func (tp *transpiler) emitBusyBodyAppend(goVar string, lappend bool) {
	if lappend {
		tp.emitLine("%s = tclListAppend(%s, strconv.Itoa(count))", goVar, goVar)
	} else {
		tp.emitLine("%s = strconv.Itoa(count)", goVar)
	}
}

// emitBusyHandlerSimple emits the unconditional busy handler: record the
// retry count, then always retry unless the proc ended in a bare `break`
// (a TCL_BREAK aborts the retry loop).
func (tp *transpiler) emitBusyHandlerSimple(goName, goVar string, lappend, aborts bool) {
	tp.emitLine("%s.SetBusyHandler(func(count int) bool {", goName)
	tp.emitBusyBodyAppend(goVar, lappend)
	if aborts {
		tp.emitLine("return false")
	} else {
		tp.emitLine("return true")
	}
	tp.emitLine("})")
}

// emitBusyIfBreakHandler emits the busy handler for a second statement of
// the shape `if {$param OP num} break` (no spaces required: lock.test writes
// `if {$count>4} break`). Unrecognized comparison shapes emit the if-break
// comment.
func (tp *transpiler) emitBusyIfBreakHandler(goName, name, goVar string, lappend bool, param, cond string) {
	expr := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cond, "if {"), "} break"))
	op := ""
	opIdx := -1
	for _, candidate := range []string{">=", "<=", ">", "<"} {
		if i := strings.Index(expr, candidate); i >= 0 {
			op, opIdx = candidate, i
			break
		}
	}
	if op == "" || strings.TrimSpace(expr[:opIdx]) != "$"+param {
		tp.emitLine("// %s.busy %s (if-break shape not recognized, not transpiled)", goName, name)
		return
	}
	tp.emitLine("%s.SetBusyHandler(func(count int) bool {", goName)
	tp.emitBusyBodyAppend(goVar, lappend)
	// Emit the condition with count substituted for $param; aborting
	// (break) stops the retry loop like a TCL_BREAK from the proc.
	tp.emitLine("if count %s %s { return false }", op, strings.TrimSpace(expr[opIdx+len(op):]))
	tp.emitLine("return true")
	tp.emitLine("})")
}

// busyAppendShape recognizes the first statement of a busy-callback proc
// body: `set ::VAR $param` or `lappend ::VAR $param` (lock.test's two
// busy-callback shapes). Returns the TCL variable name, the parameter name,
// and whether the body appends (lappend) rather than overwrites (set).
func busyAppendShape(first string) (tclVar, param string, lappend, ok bool) {
	var rest1 string
	if strings.HasPrefix(first, "set ::") {
		rest1 = strings.TrimSpace(strings.TrimPrefix(first, "set ::"))
	} else if strings.HasPrefix(first, "lappend ::") {
		lappend = true
		rest1 = strings.TrimSpace(strings.TrimPrefix(first, "lappend ::"))
	} else {
		return "", "", false, false
	}
	// rest1: "<var> $<param>"
	parts := strings.Fields(rest1)
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "$") {
		return "", "", false, false
	}
	return strings.TrimPrefix(parts[0], "::"), strings.TrimPrefix(parts[1], "$"), lappend, true
}

// processNamedDBBusy handles `dbN busy <proc>` — the TCL binding of
// sqlite3_busy_handler (tclsqlite.c DbBusyHandler): the proc runs with the
// retry count; a `break` (TCL_BREAK) or a truthy integer result ABORTS the
// retry loop, any other normal result retries (atoi(result)==0 → retry).
// Recognizes lock.test's two busy-callback shapes (a `set ::global $param`
// or `lappend ::global $param` body with an optional trailing `break` /
// `if {$param > N} break`); anything else keeps the previous no-op
// emission. The emitted closure mutates the generated test's Go variable
// for the TCL global and registers through DB.SetBusyHandler.
func (tp *transpiler) processNamedDBBusy(goName string, rest []tcl.RawWord) {
	if len(rest) == 0 {
		return // getter form: unused by the suite
	}
	name := strings.TrimSpace(rest[0].Text)
	body := tp.procBodyFor(name)
	if body == "" {
		tp.emitLine("// %s.busy %s (proc body unknown, not transpiled)", goName, name)
		return
	}
	stmts := splitProcBodyStmts(body)
	if len(stmts) < 1 || len(stmts) > 2 {
		tp.emitLine("// %s.busy %s (body shape not recognized, not transpiled)", goName, name)
		return
	}
	tclVar, param, lappend, ok := busyAppendShape(stmts[0])
	if !ok {
		tp.emitLine("// %s.busy %s (body shape not recognized, not transpiled)", goName, name)
		return
	}
	goVar := tclVarToGo(tclVar)
	if len(stmts) == 2 {
		cond := stmts[1]
		if cond == "break" {
			tp.emitBusyHandlerSimple(goName, goVar, lappend, true)
			return
		}
		if strings.HasPrefix(cond, "if {") && strings.HasSuffix(cond, "} break") {
			tp.emitBusyIfBreakHandler(goName, name, goVar, lappend, param, cond)
			return
		}
		tp.emitLine("// %s.busy %s (body shape not recognized, not transpiled)", goName, name)
		return
	}
	tp.emitBusyHandlerSimple(goName, goVar, lappend, false)
}

// emitTraceWrongArgs emits tclsqlite.c's Tcl_WrongNumArgs error for a
// trace/profile registration with two or more arguments (trace-1.1 /
// trace-3.1).
func (tp *transpiler) emitTraceWrongArgs(goName, kind string) {
	if tp.catchMode {
		tp.emitLine("_catchErr = tclWrongNumArgs(%q)", kind)
	} else {
		tp.emitLine("// %s.%s (wrong # args)", goName, kind)
	}
}

// emitTraceClear handles the "{}" (clear) form: reset the registered proc
// name and uninstall the hook.
func (tp *transpiler) emitTraceClear(goName, kind string) {
	tp.emitLine("tclTraceNameSet(%s, %q, \"\")", goName, kind)
	if kind == "profile" {
		tp.emitLine("%s.SetProfileHook(nil)", goName)
	} else {
		tp.emitLine("%s.SetTraceHook(nil)", goName)
	}
}

// emitTraceHook registers the trace/profile hook. The hook resolves the
// proc body at call time (TCL redefinition semantics: trace.test 2.1
// redefines trace_proc AFTER registering it).
func (tp *transpiler) emitTraceHook(goName, kind, name string) {
	if kind == "profile" {
		tp.emitLine("%s.SetProfileHook(func(sqlText string, ns int64) {", goName)
		tp.emitLine("if impl := tclProfileImpl(%q); impl != nil {", name)
		tp.emitLine("impl(sqlText, ns)")
		tp.emitLine("}")
	} else {
		tp.emitLine("%s.SetTraceHook(func(sqlText string) {", goName)
		tp.emitLine("if impl := tclTraceImpl(%q); impl != nil {", name)
		tp.emitLine("impl(sqlText)")
		tp.emitLine("}")
	}
	tp.emitLine("})")
}

// processNamedDBTraceProfile handles `dbN trace <proc>` and
// `dbN profile <proc>` — the TCL bindings of sqlite3_trace and
// sqlite3_profile (tclsqlite.c DB_TRACE / DB_PROFILE). The getter form
// returns the registered proc name; a single "{}" clears; two or more
// arguments raise tclsqlite.c's Tcl_WrongNumArgs error (trace-1.1 /
// trace-3.1). Registration recognizes the suite's callback bodies — a
// single `lappend ::VAR [string trim $cmd]` statement (trace_proc /
// profile_proc append the trimmed statement text) or the `global VAR` +
// `lappend VAR [string trim $sql]` pair — and emits a Go closure appending
// the statement text to the generated test's Go variable.
func (tp *transpiler) processNamedDBTraceProfile(goName string, rest []tcl.RawWord, kind string) {
	if len(rest) == 0 {
		tp.emitLine("_r = tclTraceName(%s, %q) // lindex result", goName, kind)
		return
	}
	if len(rest) > 1 {
		tp.emitTraceWrongArgs(goName, kind)
		return
	}
	name := strings.TrimSpace(rest[0].Text)
	if name == "" || name == "{}" {
		tp.emitTraceClear(goName, kind)
		return
	}
	body := tp.procBodyFor(name)
	goVar := traceAppendTarget(body)
	if goVar == "" {
		tp.emitLine("// %s.%s %s (proc body not recognized, not transpiled)", goName, kind, name)
		return
	}
	tp.emitLine("tclTraceNameSet(%s, %q, %q)", goName, kind, name)
	tp.emitTraceHook(goName, kind, name)
}

// traceGlobalVar validates the optional `global VAR` statement preceding a
// trace-callback lappend and returns VAR, or "" when the statement pair is
// not the `global VAR` + `lappend VAR ...` shape.
func traceGlobalVar(globalStmt, lapp string) string {
	if !strings.HasPrefix(globalStmt, "global ") {
		return ""
	}
	globalVar := strings.TrimSpace(strings.TrimPrefix(globalStmt, "global "))
	if !strings.HasPrefix(lapp, "lappend "+globalVar+" ") {
		return ""
	}
	return globalVar
}

// traceTargetFromLappend extracts the generated Go variable name from the
// trailing lappend statement: either the `lappend ::VAR` qualified form or
// the `global`-declared unqualified form (globalVar non-empty). Returns ""
// when the appended argument is not the traced SQL text.
func traceTargetFromLappend(lapp, globalVar string) string {
	goVar := ""
	if strings.HasPrefix(lapp, "lappend ::") {
		rest1 := strings.TrimSpace(strings.TrimPrefix(lapp, "lappend ::"))
		varName, expr, ok := splitFirstWord(rest1)
		if ok && traceAppendedText(expr) {
			goVar = strings.TrimPrefix(varName, "::")
		}
	} else if globalVar != "" {
		rest1 := strings.TrimSpace(strings.TrimPrefix(lapp, "lappend "+globalVar+" "))
		if traceAppendedText(rest1) {
			goVar = globalVar
		}
	}
	if goVar == "" {
		return ""
	}
	return tclVarToGo(goVar)
}

// traceAppendTarget recognizes the trace/profile callback bodies used by
// trace.test: `lappend ::VAR [string trim $cmd]` (optionally preceded by a
// `global VAR` statement) and returns the generated Go variable name, or ""
// when the body does not match.
func traceAppendTarget(body string) string {
	if body == "" {
		return ""
	}
	stmts := splitProcBodyStmts(body)
	if len(stmts) == 0 || len(stmts) > 2 {
		return ""
	}
	lapp := stmts[len(stmts)-1]
	globalVar := ""
	if len(stmts) == 2 {
		globalVar = traceGlobalVar(stmts[0], lapp)
		if globalVar == "" {
			return ""
		}
	}
	return traceTargetFromLappend(lapp, globalVar)
}

// traceAppendedText reports whether the appended argument of a trace-callback
// lappend is the traced SQL text: either the raw "$txt" variable or a
// "[string trim $VAR]" application over it (trace.test's trace_proc bodies
// use both shapes).
func traceAppendedText(expr string) bool {
	return expr == "$txt" || (strings.HasPrefix(expr, "[string trim $") && strings.HasSuffix(expr, "]"))
}

// splitFirstWord splits "word rest-of-line" into (word, rest).
func splitFirstWord(s string) (word, rest string, ok bool) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", true
	}
	return s[:i], strings.TrimSpace(s[i+1:]), true
}

// traceV2Mask parses the optional trace_v2 mask argument: a list of event
// names (statement/profile/row/close) or their numeric values (1/2/4/8);
// the default (no mask argument) is SQLITE_TRACE_STMT, the "legacy" default.
// An unknown word raises the bad-trace-type error (trace3-1.2) and returns
// ok=false.
func (tp *transpiler) traceV2Mask(goName string, rest []tcl.RawWord) (mask int, ok bool) {
	if len(rest) <= 1 {
		return exec_trace_stmt, true
	}
	mask = 0
	for _, tok := range strings.Fields(rest[1].Text) {
		switch tok {
		case "statement", "1":
			mask |= exec_trace_stmt
		case "profile", "2":
			mask |= exec_trace_profile
		case "row", "4":
			mask |= exec_trace_row
		case "close", "8":
			mask |= exec_trace_close
		default:
			if tp.catchMode {
				tp.emitLine("_catchErr = tclBadTraceType(%q)", tok)
			} else {
				tp.emitLine("// %s.trace_v2 (bad trace type %q)", goName, tok)
			}
			return 0, false
		}
	}
	return mask, true
}

// emitTraceV2WrongArgs emits the wrong-args error for a trace_v2
// registration with more than two arguments.
func (tp *transpiler) emitTraceV2WrongArgs(goName string) {
	if tp.catchMode {
		tp.emitLine("_catchErr = tclWrongNumArgsMask(\"trace_v2\")")
	} else {
		tp.emitLine("// %s.trace_v2 (wrong # args)", goName)
	}
}

// emitTraceV2Hook registers the trace_v2 closure: each event appends the TCL
// rendering of its argument list to the proc's target variable (statement:
// id+SQL, profile: id+nanoseconds, row/close: id).
func (tp *transpiler) emitTraceV2Hook(goName, goVar string, mask int) {
	tp.emitLine("%s.SetTraceV2Hook(func(event int, id int64, text string) {", goName)
	tp.emitLine("idStr := strconv.FormatInt(id, 10)")
	tp.emitLine("switch event {")
	tp.emitLine("case frigolite.TraceStmt:")
	tp.emitLine("%s = tclListAppend(%s, tclTraceArgs(idStr, text))", goVar, goVar)
	tp.emitLine("case frigolite.TraceProfile:")
	tp.emitLine("%s = tclListAppend(%s, tclTraceArgs(idStr, text))", goVar, goVar)
	tp.emitLine("case frigolite.TraceRow:")
	tp.emitLine("%s = tclListAppend(%s, tclTraceArgs(idStr))", goVar, goVar)
	tp.emitLine("case frigolite.TraceClose:")
	tp.emitLine("%s = tclListAppend(%s, tclTraceArgs(idStr))", goVar, goVar)
	tp.emitLine("}")
	tp.emitLine("}, %d)", mask)
}

// processNamedDBTraceV2 handles `dbN trace_v2 <proc> ?MASK?` — the TCL
// binding of sqlite3_trace_v2 (tclsqlite.c DB_TRACE_V2). The mask defaults
// to SQLITE_TRACE_STMT (the "legacy" default); mask lists accept the names
// statement/profile/row/close and their numeric values; an unknown word
// raises the bad-trace-type error (trace3-1.2). The recognized callback
// body is `lappend ::VAR [string trim $args]` — the proc appends the TCL
// rendering of its argument list, which the emitted closure builds per
// event (statement: id+SQL, profile: id+nanoseconds, row/close: id).
func (tp *transpiler) processNamedDBTraceV2(goName string, rest []tcl.RawWord) {
	if len(rest) == 0 {
		tp.emitLine("_r = tclTraceName(%s, \"trace_v2\") // lindex result", goName)
		return
	}
	name := strings.TrimSpace(rest[0].Text)
	mask, ok := tp.traceV2Mask(goName, rest)
	if !ok {
		return
	}
	if name == "" || name == "{}" {
		tp.emitLine("tclTraceNameSet(%s, \"trace_v2\", \"\")", goName)
		tp.emitLine("%s.SetTraceV2Hook(nil, 0)", goName)
		return
	}
	if len(rest) > 2 {
		tp.emitTraceV2WrongArgs(goName)
		return
	}
	body := tp.procBodyFor(name)
	tp.emitLine("tclTraceNameSet(%s, \"trace_v2\", %q)", goName, name)
	goVar := traceV2AppendTarget(body)
	if goVar == "" {
		tp.emitLine("// %s.trace_v2 %s (proc body not recognized, not transpiled)", goName, name)
		return
	}
	tp.emitTraceV2Hook(goName, goVar, mask)
}

// traceV2AppendTarget recognizes the trace_v2 callback body
// `lappend ::VAR [string trim $args]` and returns the generated Go
// variable name ("" when unrecognized).
func traceV2AppendTarget(body string) string {
	stmts := splitProcBodyStmts(body)
	if len(stmts) == 0 {
		return ""
	}
	lapp := stmts[0]
	if !strings.HasPrefix(lapp, "lappend ::") {
		return ""
	}
	rest1 := strings.TrimSpace(strings.TrimPrefix(lapp, "lappend ::"))
	varName, expr, _ := splitFirstWord(rest1)
	if strings.HasPrefix(expr, "[string trim $") && strings.HasSuffix(expr, "]") {
		return tclVarToGo(strings.TrimPrefix(varName, "::"))
	}
	return ""
}

// splitProcBodyStmts splits a proc body into top-level statements
// (newline- or semicolon-separated, trimmed; empty pieces dropped).
func splitProcBodyStmts(body string) []string {
	raw := strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == ';'
	})
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
