// Package main implements the tcl2go tool.
//
// This file transpiles TCL authorizer procs (`proc auth {code arg1 arg2 arg3
// arg4 args} { ... }` registered via `db authorizer ::auth`) into Go
// authorizer callbacks. The engine's authorizer framework (internal/auth) is
// available to Go programs via DB.SetAuthorizer.
//
// TCL authorizer procs are DYNAMIC: the test redefines `proc auth` many times
// (auth.test has 145 definitions), and a single `db authorizer ::auth`
// registration (at the top) keeps referencing the proc NAME, so redefining
// the proc changes the callback behavior without re-registering. The
// transpiler mirrors this with a package-level mutable function variable
// `authCurrent`; each `proc auth` redefinition assigns it a fresh Go closure,
// and `db authorizer` installs a fixed dispatcher that calls authCurrent.
package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// authCurrentVar is the Go variable name holding the current authorizer
// callback (assigned by each `proc auth` definition).
const authCurrentVar = "authCurrent"

// processAuthorizerProc recognizes a TCL authorizer proc (proc NAME {PARAMS}
// {BODY}) and emits a reassignment of the package-level authCurrent variable
// to a closure mirroring the body. Returns true when the proc was handled.
func (tp *transpiler) processAuthorizerProc(args []tcl.RawWord) bool {
	if len(args) < 3 {
		return false
	}
	name := strings.TrimSpace(args[0].Text)
	if !isAuthorizerProcName(name) {
		return false
	}
	params := strings.TrimSpace(args[1].Text)
	if !authorizerProcSignature(params) {
		return false
	}
	body := strings.TrimSpace(args[2].Text)
	body = strings.TrimPrefix(body, "{")
	body = strings.TrimSuffix(body, "}")
	if !authorizerProcBodyTranspilable(body) {
		tp.emitLine("// authorizer proc %s (complex body, not transpiled)", name)
		return false
	}
	tp.ensureAuthCurrentDecl()
	tp.emitLine("%s = func(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {", authCurrentVar)
	tp.indent++
	tp.emitAuthorizerBody(body)
	if !authorizerBodyEndsWithBareReturn(body) {
		tp.emitLine("return auth.ResultOK")
	}
	tp.indent--
	tp.emitLine("}")
	tp.emitLine("_ = %s // authorizer proc %s", authCurrentVar, name)
	return true
}

// isAuthorizerProcName reports whether a proc name looks like an authorizer
// proc (auth / authx / auth2 / auth3 or a name ending in "auth").
func isAuthorizerProcName(name string) bool {
	n := strings.ToLower(name)
	return n == "auth" || n == "authx" || n == "auth2" || n == "auth3" ||
		strings.HasSuffix(n, "auth")
}

// authorizerProcSignature reports whether the parameter list matches the
// authorizer proc signature (at least 4 params for code/arg1..arg3; the
// standard is 6: code arg1 arg2 arg3 arg4 args).
func authorizerProcSignature(params string) bool {
	items := tclCmdWords(params)
	// The signature is positional: at least 4 params (code arg1 arg2 arg3);
	// the standard authorizer proc has 5-6 (code arg1 arg2 arg3 arg4 args).
	return len(items) >= 4
}

// authorizerProcBodyTranspilable reports whether a TCL authorizer body uses
// only the transpilable subset: if/elseif/else chains returning SQLITE_*
// constants, with conditions comparing $code/$arg1..$arg4 against string
// literals via == / && / || / !. Bodies that use lappend/set/expr/regexp/
// switch/foreach (other than the ::authargs recording, which is no-oped) are
// rejected (the transpiler falls back to the no-op path; those tests are
// per-test skipped).
func authorizerProcBodyTranspilable(body string) bool {
	// `set ::authargs [list ...]` records the callback args in a TCL
	// namespace variable (the tests read it later as the callback log). The
	// transpiler no-ops that variable; the set lines are skipped during
	// body emission, so they do NOT make the body untranspilable.
	normalized := regexp.MustCompile(`(?m)^\s*set\s+::[A-Za-z0-9_]+\s*\[list[^\]]*\]\s*$`).ReplaceAllString(body, "")
	normalized = regexp.MustCompile(`(?m)^\s*set\s+::[A-Za-z0-9_]+\s*\{\}\)?\s*$`).ReplaceAllString(normalized, "")
	if strings.Contains(normalized, "lappend") ||
		strings.Contains(normalized, "regexp") || strings.Contains(normalized, "switch") ||
		strings.Contains(normalized, "expr") || strings.Contains(normalized, "foreach") ||
		strings.Contains(normalized, "append ") || strings.Contains(normalized, "global") {
		return false
	}
	// Any remaining `set` (not ::authargs) or `return $var` forms are
	// unsupported.
	if strings.Contains(normalized, "set ") {
		return false
	}
	// Every return must be one of the SQLITE_* result constants.
	if regexp.MustCompile(`(?m)return\s+([A-Za-z_]+)`).FindAllString(normalized, -1) != nil {
		for _, m := range regexp.MustCompile(`(?m)return\s+([A-Za-z_]+)`).FindAllStringSubmatch(normalized, -1) {
			v := strings.ToUpper(m[1])
			switch v {
			case "SQLITE_OK", "SQLITE_DENY", "SQLITE_IGNORE":
			default:
				return false
			}
		}
	}
	return true
}

// ensureAuthCurrentDecl emits the package-level authCurrent variable and the
// authDispatcher type on first use (once per test file). The dispatcher is
// what db.SetAuthorizer registers; it forwards to the current closure.
func (tp *transpiler) ensureAuthCurrentDecl() {
	if tp.authCurrentDeclared {
		return
	}
	tp.authCurrentDeclared = true
	if tp.authPreamble == nil {
		tp.authPreamble = &strings.Builder{}
	}
	b := tp.authPreamble
	b.WriteString("// authCurrent holds the current TCL authorizer callback; each\n")
	b.WriteString("// `proc auth {...}` definition reassigns it (SQLite redefines the\n")
	b.WriteString("// proc, and `db authorizer ::auth` keeps referencing the proc name).\n")
	b.WriteString("var " + authCurrentVar + " func(auth.Action, string, string, string, string) auth.Result\n\n")
	b.WriteString("type authDispatcher struct{}\n")
	b.WriteString("func (a *authDispatcher) Authorize(action auth.Action, arg1, arg2, arg3, arg4 string) auth.Result {\n")
	b.WriteString("\tif " + authCurrentVar + " == nil {\n")
	b.WriteString("\t\treturn auth.ResultOK\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn " + authCurrentVar + "(action, arg1, arg2, arg3, arg4)\n")
	b.WriteString("}\n\n")
}

// emitAuthorizerBody transpiles the if/elseif/return chain of an authorizer
// proc body. The body consists of `if {COND} { return SQLITE_X }` blocks
// (optionally elseif/else) followed by a final `return SQLITE_OK`.
func (tp *transpiler) emitAuthorizerBody(body string) {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "set ::") {
			// Skip blank lines and the callback-log `set ::authargs` lines.
			i++
			continue
		}
		if strings.HasPrefix(line, "if {") {
			i = tp.emitAuthorizerIfBlock(lines, i)
			continue
		}
		// A bare return — skip `return SQLITE_OK` (the closure's trailing
		// `return auth.ResultOK` handles the default allow case); emit other
		// result returns (e.g. a body that is just `return SQLITE_DENY`).
		if r := authorizerReturnToGo(line); r != "" && !strings.Contains(strings.ToUpper(line), "SQLITE_OK") {
			tp.emitLine("return %s", r)
		}
		i++
	}
}

// emitAuthorizerIfBlock transpiles one `if {COND} { ... }` block (with
// optional elseif/else) starting at lines[i] (the `if {` line). Returns the
// index of the first line after the block's closing brace.
func (tp *transpiler) emitAuthorizerIfBlock(lines []string, i int) int {
	condEnd := strings.LastIndex(lines[i], "}")
	if condEnd < 0 {
		return i + 1
	}
	cond := strings.TrimSpace(lines[i][4:condEnd])
	goCond := authorizerCondToGo(cond)
	if goCond == "" {
		// Unrecognized condition — stop transpiling the body.
		return len(lines)
	}
	tp.emitLine("if %s {", goCond)
	ttp := tp.indent + 1
	next := i + 1
	for next < len(lines) && !strings.Contains(lines[next], "}") {
		if r := authorizerReturnToGo(strings.TrimSpace(lines[next])); r != "" {
			tp.emitIndented(ttp, "return %s", r)
		}
		next++
	}
	if next >= len(lines) {
		return next
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[next]), "}"))
	next++
	switch {
	case strings.HasPrefix(rest, "elseif") || strings.HasPrefix(rest, "} elseif"):
		next = tp.emitAuthorizerElseIf(lines, next, rest, ttp)
	case strings.HasPrefix(rest, "else"):
		next = tp.emitAuthorizerElse(lines, next, ttp)
	default:
		tp.emitLine("}")
	}
	return next
}

// emitAuthorizerElseIf transpiles an `elseif {COND} { ... }` block after an if
// (or preceding elseif). Returns the index after its closing brace.
func (tp *transpiler) emitAuthorizerElseIf(lines []string, i int, rest string, depth int) int {
	cond := authorizerCondToGo(authorizerElseifCond(rest))
	if cond == "" {
		cond = "true"
	}
	tp.emitIndented(depth-1, "} else if %s {", cond)
	next := i
	for next < len(lines) && !strings.Contains(lines[next], "}") {
		if r := authorizerReturnToGo(strings.TrimSpace(lines[next])); r != "" {
			tp.emitIndented(depth, "return %s", r)
		}
		next++
	}
	if next >= len(lines) {
		return next
	}
	innerRest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[next]), "}"))
	next++
	if strings.HasPrefix(innerRest, "elseif") || strings.HasPrefix(innerRest, "} elseif") {
		return tp.emitAuthorizerElseIf(lines, next, innerRest, depth)
	}
	if strings.HasPrefix(innerRest, "else") {
		return tp.emitAuthorizerElse(lines, next, depth)
	}
	tp.emitLine("}")
	return next
}

// emitAuthorizerElse transpiles an `else { ... }` block after an if/elseif.
// Returns the index after its closing brace.
func (tp *transpiler) emitAuthorizerElse(lines []string, i int, depth int) int {
	tp.emitIndented(depth-1, "} else {")
	next := i
	for next < len(lines) && !strings.Contains(lines[next], "}") {
		if r := authorizerReturnToGo(strings.TrimSpace(lines[next])); r != "" {
			tp.emitIndented(depth, "return %s", r)
		}
		next++
	}
	if next < len(lines) {
		next++
	}
	tp.emitLine("}")
	return next
}

// emitIndented writes a line at the given indent level (without touching the
// transpiler's indent counter, which tracks the enclosing test block).
func (tp *transpiler) emitIndented(indent int, format string, args ...interface{}) {
	tp.sb.WriteString(strings.Repeat("\t", indent))
	tp.sb.WriteString(fmt.Sprintf(format, args...))
	tp.sb.WriteString("\n")
}

// authorizerElseifCond extracts the condition from an `elseif {COND}` line.
func authorizerElseifCond(line string) string {
	idx := strings.Index(line, "{")
	if idx < 0 {
		return ""
	}
	end := strings.LastIndex(line, "}")
	if end < 0 || end <= idx {
		return ""
	}
	return strings.TrimSpace(line[idx+1 : end])
}

// authorizerCondToGo converts a TCL authorizer condition like
// `$code=="SQLITE_INSERT" && $arg1=="t2"` to a Go boolean expression
// `action.String() == "SQLITE_INSERT" && arg1 == "t2"`. Returns "" for
// unrecognized conditions.
func authorizerCondToGo(cond string) string {
	// Split on && (top-level). TCL && is the only connective used in the
	// simple auth procs; || is rare (reject it to stay conservative).
	if strings.Contains(cond, "||") {
		return ""
	}
	parts := strings.Split(cond, "&&")
	var goParts []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return ""
		}
		g := authorizerAtomToGo(p)
		if g == "" {
			return ""
		}
		goParts = append(goParts, g)
	}
	return strings.Join(goParts, " && ")
}

// authorizerAtomToGo converts one comparison atom `$X=="Y"` / `$X eq "Y"` /
// `$X!="Y"` / `$X ne "Y"` to a Go expression. Recognized variables: $code
// (→ action.String()), $arg1..$arg4 (→ arg1..arg4), and the alternate
// authorizer names $op (action), $a0..$a3 (arg1..arg4). Returns "" for
// unrecognized atoms.
func authorizerAtomToGo(atom string) string {
	// Accept ==, eq, !=, ne. Also handle $X == "Y" with optional spaces.
	re := regexp.MustCompile(`^\$([A-Za-z0-9_]+)\s*(==|eq|!=|ne)\s*"([^"]*)"$`)
	m := re.FindStringSubmatch(atom)
	if m == nil {
		return ""
	}
	varName := m[1]
	op := m[2]
	val := m[3]
	goVar := authorizerVarToGo(varName)
	if goVar == "" {
		return ""
	}
	goOp := "=="
	if op == "!=" || op == "ne" {
		goOp = "!="
	}
	return fmt.Sprintf("%s %s %q", goVar, goOp, val)
}

// authorizerVarToGo maps a TCL authorizer parameter name to the Go closure's
// parameter (action or arg1..arg4). The engine passes the action as
// auth.Action; its String() is the SQLITE_* name the TCL compares against.
func authorizerVarToGo(name string) string {
	switch strings.ToLower(name) {
	case "code", "op":
		return "action.String()"
	case "arg1", "a0":
		return "arg1"
	case "arg2", "a1":
		return "arg2"
	case "arg3", "a2":
		return "arg3"
	case "arg4", "a3":
		return "arg4"
	default:
		return ""
	}
}

// authorizerReturnToGo maps a `return SQLITE_X` line to the Go auth.Result
// constant expression, or "" for non-return lines.
func authorizerReturnToGo(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "return ") {
		return ""
	}
	val := strings.TrimSpace(strings.TrimPrefix(line, "return "))
	switch strings.ToUpper(val) {
	case "SQLITE_OK":
		return "auth.ResultOK"
	case "SQLITE_DENY":
		return "auth.ResultDeny"
	case "SQLITE_IGNORE":
		return "auth.ResultIgnore"
	default:
		return ""
	}
}

// authorizerBodyEndsWithBareReturn reports whether the TCL authorizer body's
// final statement is a bare (unconditional) `return SQLITE_X` — the closure
// terminates there, so no trailing `return auth.ResultOK` is needed. A final
// `return SQLITE_OK` still needs the trailing OK (emitAuthorizerBody skips
// bare SQLITE_OK lines), so only non-OK final returns suppress the trailer.
func authorizerBodyEndsWithBareReturn(body string) bool {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "return ") {
			return !strings.Contains(strings.ToUpper(line), "SQLITE_OK")
		}
		return false
	}
	return false
}
