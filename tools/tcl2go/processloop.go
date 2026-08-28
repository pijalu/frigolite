// Package main implements the tcl2go tool.
//
// This file handles TCL for/while/if conditionals and expression building.
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// reIncrMod matches the TCL condition "[incr VAR]" optionally followed by
// "% N" (e.g. "[incr i] % 2" in fts4opt 2.1's delete/replace churn).
var reInfoExistsDyn = regexp.MustCompile(`\[info exists ([A-Za-z_][A-Za-z0-9_]*)\((\$[A-Za-z0-9_]+)\)\]`)
var reIncrMod = regexp.MustCompile(`^\[incr\s+(\w+)\](?:\s*%\s*(\d+))?$`)

// (imports managed by goimports)

// emitContinue emits a Go continue statement. In TCL, `continue` inside a
// `for` loop runs the increment clause before re-evaluating the condition.
// Since the transpiler emits the increment at the end of the loop body (which
// Go's continue would skip), we inline the increment commands first.
func (tp *transpiler) emitContinue() {
	if len(tp.forIncrs) > 0 {
		if incr := tp.forIncrs[len(tp.forIncrs)-1]; len(incr) > 0 {
			// Re-run the increment commands before continuing. They use the
			// same vars slice (already declared), so no redeclaration occurs.
			for _, c := range incr {
				tp.processCommand(c)
			}
		}
	}
	tp.emitLine("continue")
}

func (tp *transpiler) processForCommand(args []tcl.RawWord) {
	if len(args) < 4 {
		return
	}
	initCmds := parseCommands(args[0].Text)
	cond := args[1].Text
	nextCmds := parseCommands(args[2].Text)
	bodyCmds := parseCommands(args[3].Text)

	for _, c := range initCmds {
		tp.processCommand(c)
	}

	goCond := tp.tclCondToGo(cond)
	tp.emitLine("for %s {", goCond)
	tp.indent++

	bodyTP := &transpiler{
		sb:         tp.sb,
		indent:     tp.indent,
		dbVar:      tp.dbVar,
		t:          tp.t,
		varCount:   tp.varCount,
		vars:       tp.vars,
		forIncrs:   append(tp.forIncrs, nextCmds),
		testPrefix: tp.testPrefix, preparedState: tp.preparedState,
		queryVars:    tp.queryVars,
		queryFuncs:   tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
		varConstValues:   tp.varConstValues,
		foreachLitValues: tp.foreachLitValues,
		varsetLoopVars:   tp.varsetLoopVars,
		dbConnVars:       tp.dbConnVars,
		runtimeConnVars:  tp.runtimeConnVars,
		varRenames:       tp.varRenames,
		blobChans:        tp.blobChans,
		blobChannelVars:  tp.blobChannelVars,
		blobVarNames:     tp.blobVarNames,
		usedChannels:     tp.usedChannels,
		blobSeq:          tp.blobSeq,
	}
	bodyTP.processCommands(bodyCmds)
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.queryVars = bodyTP.queryVars
	tp.queryFuncs = bodyTP.queryFuncs
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq

	for _, c := range nextCmds {
		tp.processCommand(c)
	}

	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) processWhile(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	cond := args[0].Text
	goCond := tp.tclCondToGo(cond)
	bodyCmds := tp.parseBracedBody(args, 1)

	// A `while {"SQLITE_ROW" == [sqlite3_step $STMT]} { incr N }` loop is
	// the C-API row-counting idiom: it steps a prepared statement to count
	// its rows. The pure-Go engine has no prepared-statement step loop, so
	// emit a direct row-count query instead (the prepared SQL is known from
	// the earlier sqlite3_prepare[_v2] recording).
	if sqlExpr, countVar, ok := tp.whileStepRowCount(cond, bodyCmds); ok {
		tp.emitLine("r = db.Query(%s)", sqlExpr)
		tp.emitLine("if r.Error != nil {")
		tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
		tp.emitLine("\treturn")
		tp.emitLine("}")
		tp.emitLine("%s = strconv.Itoa(len(r.Rows))", countVar)
		return
	}

	// A `while {1}` loop whose body drives test-harness memory-allocation
	// failure injection (sqlite3_memdebug_fail) has an unterminable break:
	// the break condition ($nFail == 0) depends on the C malloc-failure
	// counter, which the pure-Go engine cannot reproduce. Emit the loop as a
	// comment so the generated test does not hang (printf.test's
	// printf-malloc-* tests).
	if strings.TrimSpace(cond) == "1" && containsMemdebug(bodyCmds) {
		tp.emitLine("// while {1}: sqlite3_memdebug_fail malloc-failure loop (test-harness C API, not transpiled)")
		return
	}

	tp.emitLine("for %s {", goCond)
	tp.indent++

	if bodyCmds != nil {
		bodyTP := &transpiler{
			sb:       tp.sb,
			indent:   tp.indent,
			dbVar:    tp.dbVar,
			t:        tp.t,
			varCount: tp.varCount,
			vars:     tp.vars,
			// A while loop has no increment clause: continue targets this
			// loop, so the innermost entry is empty (plain Go continue).
			forIncrs:   append(tp.forIncrs, nil),
			testPrefix: tp.testPrefix, preparedState: tp.preparedState,
			queryVars:    tp.queryVars,
			specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps,
			blobChans:       tp.blobChans,
			blobChannelVars: tp.blobChannelVars,
			blobVarNames:    tp.blobVarNames,
			blobSeq:         tp.blobSeq,
		}
		bodyTP.processCommands(bodyCmds)
		tp.varCount = bodyTP.varCount
		tp.indent = bodyTP.indent
		tp.queryVars = bodyTP.queryVars
		if len(bodyTP.blobChans) > 0 {
			tp.blobChans = bodyTP.blobChans
		}
		if len(bodyTP.blobChannelVars) > 0 {
			tp.blobChannelVars = bodyTP.blobChannelVars
		}
		if bodyTP.blobVarNames != nil {
			tp.blobVarNames = bodyTP.blobVarNames
		}
		if bodyTP.usedChannels != nil {
			tp.usedChannels = bodyTP.usedChannels
		}
		if bodyTP.blobVarNames != nil {
			tp.blobVarNames = bodyTP.blobVarNames
		}
		if bodyTP.usedChannels != nil {
			tp.usedChannels = bodyTP.usedChannels
		}
		tp.blobSeq = bodyTP.blobSeq
	}

	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) processIf(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	idx := 0
	first := true

	for idx < len(args) {
		if !first {
			// Implicit else: any non-keyword braced word after a complete
			// condition+body pair is TCL's alternate `if {COND} {THEN} {ELSE}`.
			if args[idx].Braced {
				bodyCmds := tp.parseBracedBody(args, idx)
				idx++
				tp.emitLine("} else {")
				tp.indent++
				if bodyCmds != nil {
					tp.runIfBody(bodyCmds)
				}
				tp.indent--
				break
			}
			kw := tp.processIfKeyword(args, &idx)
			if kw == ifKwElse {
				break
			}
			if kw == ifKwElseif {
				continue
			}
			if kw == ifKwNone {
				break
			}
		}
		if idx >= len(args) {
			break
		}
		tp.processIfCondition(args, &idx, first)
		first = false
	}

	tp.emitLine("}")
}

// processIfKeyword handles an `else` / `elseif` keyword clause in an if
// chain, returning which keyword was consumed (or ifKwNone when idx does not
// point at a recognized keyword, in which case idx is restored).
func (tp *transpiler) processIfKeyword(args []tcl.RawWord, idx *int) int {
	keyword := args[*idx].Text
	*idx++
	if keyword == "else" {
		bodyCmds := tp.parseBracedBody(args, *idx)
		if bodyCmds != nil {
			tp.emitLine("} else {")
			tp.indent++
			tp.runIfBody(bodyCmds)
			tp.indent--
		}
		return ifKwElse
	}
	if keyword == "elseif" {
		if *idx >= len(args) {
			return ifKwElse
		}
		cond := args[*idx].Text
		*idx++
		goCond := tp.tclCondToGo(cond)
		bodyCmds := tp.parseBracedBody(args, *idx)
		*idx++
		tp.emitLine("} else if %s {", goCond)
		tp.indent++
		if bodyCmds != nil {
			tp.runIfBody(bodyCmds)
		}
		tp.indent--
		return ifKwElseif
	}
	*idx--
	return ifKwNone
}

// processIfCondition handles one if/else-if condition plus its body.
func (tp *transpiler) processIfCondition(args []tcl.RawWord, idx *int, first bool) {
	cond := args[*idx].Text
	*idx++
	// Guards that test for an EXTERNAL BINARY via [catch {exec unzip}]:
	// the Go port cannot run foreign executables, so such blocks are
	// skipped entirely — matching a SQLite build/test environment without
	// the tool (zipfile.test's ::UNZIP section).
	if strings.Contains(cond, "[catch") && strings.Contains(cond, "exec ") {
		_ = tp.parseBracedBody(args, *idx)
		*idx++
		// Emit only the opener: the caller closes the chain with a single
		// brace. Body commands are dropped — external tools are unavailable.
		tp.emitLine("if false { // external-tool guard ([exec ...]) unavailable")
		tp.indent++
		// Extraction procs (`file mkdir DEST` + `exec ... -d DEST`) cannot
		// run, but later sections depend on the directory existing — create
		// it the way unzip -d would.
		for dest := range tp.unzipDirs {
			tp.emitLine("os.RemoveAll(%q)", dest)
			tp.emitLine("os.MkdirAll(%q, 0755)", dest)
		}
		tp.indent--
		return
	}
	// if {[catch {BODY} var]} — a runtime catch as the condition. The
	// body must execute at runtime (e.g. `if {[catch {db eval ROLLBACK}
	// errmsg]}` in trans3.test: the ROLLBACK runs and the condition is
	// whether it errored).
	if chain := parseCatchChain(cond); chain != nil {
		exprs := make([]string, 0, len(chain.atoms))
		for i, body := range chain.atoms {
			v := fmt.Sprintf("_cc%d", i)
			emitCatchBody(body, "", v, tp)
			exprs = append(exprs, fmt.Sprintf("%s == \"1\"", v))
		}
		goCond := exprs[0]
		for i, op := range chain.ops {
			goCond += " " + op + " " + exprs[i+1]
		}
		bodyCmds := tp.parseBracedBody(args, *idx)
		*idx++
		tp.emitLine("if %s {", goCond)
		tp.indent++
		if bodyCmds != nil {
			tp.runIfBody(bodyCmds)
		}
		tp.indent--
		return
	}
	if catchVar := tp.catchCondVar(cond); catchVar != "" {
		tp.emitCatchForCondition(catchVar, cond)
		bodyCmds := tp.parseBracedBody(args, *idx)
		*idx++
		tp.emitLine("if %s == \"1\" {", catchVar)
		tp.indent++
		if bodyCmds != nil {
			tp.runIfBody(bodyCmds)
		}
		tp.indent--
		return
	}
	goCond := tp.tclCondToGo(cond)
	bodyCmds := tp.parseBracedBody(args, *idx)
	// A non-braced body is a single TCL command (e.g. `if {$i == 8}
	// continue`): parse it directly so the command is emitted instead of
	// being dropped.
	if bodyCmds == nil && *idx < len(args) && !args[*idx].Braced {
		if parsed := parseCommands(args[*idx].Text); len(parsed) > 0 {
			bodyCmds = parsed
		}
	}
	*idx++

	if first {
		tp.emitLine("if %s {", goCond)
	} else {
		tp.emitLine("} else if %s {", goCond)
	}
	tp.indent++

	if bodyCmds != nil {
		tp.runIfBody(bodyCmds)
	}

	tp.indent--
}

// runIfBody transpiles an if/else body in a fresh sub-transpiler sharing the
// output buffer and state.
func (tp *transpiler) runIfBody(bodyCmds [][]tcl.RawWord) {
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	if len(bodyTP.blobChans) > 0 {
		tp.blobChans = bodyTP.blobChans
	}
	if len(bodyTP.blobChannelVars) > 0 {
		tp.blobChannelVars = bodyTP.blobChannelVars
	}
	if bodyTP.blobVarNames != nil {
		tp.blobVarNames = bodyTP.blobVarNames
	}
	if bodyTP.usedChannels != nil {
		tp.usedChannels = bodyTP.usedChannels
	}
	tp.blobSeq = bodyTP.blobSeq
}

// ifKeyword constants describe which keyword processIfKeyword consumed.
const (
	ifKwNone   = iota // not a keyword; idx restored
	ifKwElseif        // elseif clause emitted; loop continues
	ifKwElse          // else clause emitted; if chain ends
)

// catchCondVar reports whether the condition text is a runtime catch command
// `[catch {BODY} var]` and returns the Go name of the result variable.
// Returns "" for other conditions.
func (tp *transpiler) catchCondVar(cond string) string {
	c := strings.TrimSpace(cond)
	if !strings.HasPrefix(c, "[") || !strings.HasSuffix(c, "]") {
		return ""
	}
	inner := strings.TrimSpace(c[1 : len(c)-1])
	if !strings.HasPrefix(inner, "catch ") {
		return ""
	}
	rest := strings.TrimSpace(inner[len("catch "):])
	// rest is {BODY} [var] — find the trailing variable name after the body.
	_, tail, ok := catchBodyAndTail(rest)
	if !ok || tail == "" {
		return ""
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return ""
	}
	// The tail must be exactly one clean variable name. A trailing
	// comparison like `[catch {B} v]==0 && ...` is a compound condition,
	// not a plain catch guard — reject so the caller uses general
	// expression translation instead of inventing a bogus variable
	// (zipfile.test: [catch {exec unzip} msg]==0).
	name := strings.TrimPrefix(fields[0], "::")
	if len(fields) > 1 || !isPlainTclName(name) {
		return ""
	}
	return tclVarToGo(name)
}

// isPlainTclName reports whether s is a bare TCL identifier (letters,
// digits, ::, underscore) without operators or brackets.
func isPlainTclName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == ':':
		default:
			return false
		}
	}
	return true
}

// catchBodyAndTail splits a `{BODY} [var]` catch remainder into the braced
// body text (without braces) and the trailing text after the closing brace.
func catchBodyAndTail(rest string) (bodyText, tail string, ok bool) {
	if !strings.HasPrefix(rest, "{") {
		return "", "", false
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(rest[1:i]), strings.TrimSpace(rest[i+1:]), true
			}
		}
	}
	return "", "", false
}

// emitCatchForCondition transpiles `[catch {BODY} var]` used as an if
// condition: the body runs at runtime (catch mode), the result variable
// holds "1" (error) or "0" (ok), and the if uses it. The body's
// `db eval ROLLBACK` sets the enclosing callback's rollback flag.
func (tp *transpiler) emitCatchForCondition(resultVar, cond string) {
	c := strings.TrimSpace(cond)
	inner := strings.TrimSpace(c[1 : len(c)-1])
	rest := strings.TrimSpace(inner[len("catch "):])
	// Extract the braced body and the optional result/error variable name.
	bodyText, tail, ok := catchBodyAndTail(rest)
	if !ok {
		return
	}
	varName := ""
	if tail != "" {
		fields := strings.Fields(tail)
		if len(fields) > 0 {
			varName = tclVarToGo(strings.TrimPrefix(fields[0], "::"))
		}
	}
	emitCatchBody(bodyText, varName, resultVar, tp)
}

// emitCatchBody emits a Go block that runs a catch body at runtime and sets
// the result variable ("1" on error, "0" on success) plus the error message
// variable. Used for `[catch {BODY} var]` conditions inside if statements.
func emitCatchBody(bodyText, varName, resultVar string, tp *transpiler) {
	// Only declare the result variable if it is not already in scope (the
	// common vars msg/r/_res and pre-declared TCL vars are function-scoped).
	if !tp.isVarDeclared(resultVar) {
		tp.emitLine("var %s string", resultVar)
		tp.vars = append(tp.vars, resultVar)
	}
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("var _catchErr error")
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, queryVars: tp.queryVars, queryFuncs: tp.queryFuncs,
		specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps, rollbackFlag: tp.rollbackFlag, varCount: tp.varCount, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed}
	bodyTP.processCommands(parseCommands(bodyText))
	tp.varCount = bodyTP.varCount
	tp.indent = bodyTP.indent
	tp.emitLine("if _catchErr != nil {")
	tp.indent++
	tp.emitLine("%s = \"1\"", resultVar)
	if varName != "" {
		tp.emitLine("%s = _catchErr.Error()", varName)
	}
	tp.indent--
	tp.emitLine("} else {")
	tp.indent++
	tp.emitLine("%s = \"0\"", resultVar)
	if varName != "" {
		tp.emitLine("%s = \"\"", varName)
	}
	tp.indent--
	tp.emitLine("}")
	tp.indent--
	tp.emitLine("}")
}

func (tp *transpiler) tclCondToGo(cond string) string {
	cond = strings.TrimSpace(cond)
	if strings.HasPrefix(cond, "expr ") {
		cond = strings.TrimSpace(cond[5:])
	}
	if strings.HasPrefix(cond, "[expr ") {
		cond = strings.TrimSpace(cond[6:])
	}
	cond = stripCondBraces(cond)
	cond = strings.TrimSuffix(cond, "}")
	cond = strings.ReplaceAll(cond, " eq ", " == ")
	cond = strings.ReplaceAll(cond, " ne ", " != ")

	// `db one {SQL} <op> <num>` — a live query compared against a constant
	// (fts4growth 2.2's break condition: db one {SELECT count(*) ... WHERE
	// level<2}==2). Emit a closure that runs the query and compares the
	// first cell numerically; the SQL may itself contain < so it is
	// extracted by brace matching before the operator is parsed.
	if goExpr := tp.dbOneCondExpr(cond); goExpr != "" {
		return goExpr
	}

	// `[db exists {SQL}]` — a live row-existence probe (json101 21.1 gates
	// the legacy_json_valid branch on pragma_compile_options). Emit a
	// closure that runs the query and reports whether any row came back.
	if goExpr := tp.dbExistsCondExpr(cond); goExpr != "" {
		return goExpr
	}

	// [string is xdigit $x] — TCL's hex-digit predicate (used by unhex.test
	// to build the expected filtered output). Render as a Go check.
	if goExpr := tp.xdigitCondExpr(cond); goExpr != "" {
		return goExpr
	}

	// [incr x] / [incr x] % N — per-iteration counter conditions inside
	// foreach bodies (fts4opt 2.1: if {[incr i] % 2} { DELETE ... }).
	// The counter var is a Go string mirrored through tclIncrMod, which
	// performs TCL's increment side effect and applies the modulo truthiness.
	if m := reIncrMod.FindStringSubmatch(cond); m != nil {
		if m[2] != "" {
			return fmt.Sprintf("tclIncrMod(&%s, %s)", m[1], m[2])
		}
		return fmt.Sprintf("tclIncrMod(&%s, 0)", m[1])
	}

	// [info exists ARR($key)] — dynamic-key array membership. Only applies
	// when ARR is a registered dynamic array (its XxxMap is declared in the
	// preamble); otherwise fall through so the generic string-expression
	// fallback renders the condition (thread004 2.1's unregistered
	// finished($t) must not emit an undeclared map reference).
	if m := reInfoExistsDyn.FindStringSubmatch(cond); m != nil {
		base := strings.TrimPrefix(m[1], "::")
		if tp.arrayMapVars[base] || tp.arrayMapVars["::"+base] {
			mapVar := tclVarToGo(base) + "Map"
			keyExpr := tclVarToGo(strings.TrimPrefix(m[2], "$"))
			return fmt.Sprintf("%s[%s] != \"\"", mapVar, keyExpr)
		}
	}

	// For conditions with comparison operators, generate a proper Go boolean expression.
	if goExpr := tp.buildCondExpr(cond); goExpr != "" {
		return goExpr
	}

	// Fallback: use buildStringExpr for simple conditions (variables, literals).
	result := tp.buildStringExpr(cond)
	if result == `"0"` || result == "0" {
		return "false"
	}
	if result == `"1"` || result == "1" {
		return "true"
	}
	return "tclBool(" + result + ")"
}

// stripCondBraces removes one balanced outer brace layer from a TCL condition
// (e.g. "{$x == 1}" → "$x == 1").
func stripCondBraces(cond string) string {
	if !strings.HasPrefix(cond, "{") || !strings.HasSuffix(cond, "}") {
		return cond
	}
	depth := 0
	balanced := true
	for i, c := range cond {
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
		}
		if depth == 0 && i < len(cond)-1 {
			balanced = false
			break
		}
	}
	if balanced {
		return cond[1 : len(cond)-1]
	}
	return cond
}

// dbOneCondExpr detects `db one {SQL} <op> <number>` conditions and renders
// them as a no-arg Go closure that runs the SQL against the harness db and
// numerically compares the first cell. Returns "" when the condition is not
// that form.
func (tp *transpiler) dbOneCondExpr(cond string) string {
	const prefix = "db one {"
	if strings.HasPrefix(cond, "[") {
		if !strings.HasPrefix(cond, "[db one {") {
			return ""
		}
		cond = strings.TrimPrefix(cond, "[")
	}
	if !strings.HasPrefix(cond, prefix) {
		return ""
	}
	// Find the matching close brace of the SQL body (SQL may contain
	// comparison operators like `level<2`).
	depth := 0
	closeIdx := -1
	for i := len(prefix) - 1; i < len(cond); i++ {
		switch cond[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return ""
	}
	sql := cond[len(prefix):closeIdx]
	rest := strings.TrimSpace(cond[closeIdx+1:])
	if rest == "" {
		// Bare truthiness of the value: non-zero and non-empty.
		return "func() bool { r := db.Query(" + strconv.Quote(sql) + "); if r.Error != nil || len(r.Rows) == 0 || len(r.Rows[0]) == 0 { return false }; s := tclRenderCell(r.Rows[0][0]); return s != \"\" && s != \"0\" }()"
	}
	op := ""
	for _, candidate := range []string{"<=", ">=", "==", "!=", "<", ">"} {
		if i := strings.Index(rest, candidate); i >= 0 {
			op = candidate
			break
		}
	}
	if op == "" {
		return ""
	}
	rhs := strings.TrimSpace(rest[strings.Index(rest, op)+len(op):])
	if rhs == "" {
		return ""
	}
	cmp := map[string]string{"<": "<", ">": ">", "==": "==", "!=": "!=", "<=": "<=", ">=": ">="}[op]
	goSQL := strconv.Quote(sql)
	return "func() bool { r := db.Query(" + goSQL + "); if r.Error != nil || len(r.Rows) == 0 || len(r.Rows[0]) == 0 { return false }; l, err := strconv.ParseFloat(tclRenderCell(r.Rows[0][0]), 64); if err != nil { return false }; rr, rerr := strconv.ParseFloat(" + strconv.Quote(rhs) + ", 64); if rerr != nil { return false }; return l " + cmp + " rr }()"
}

// dbExistsCondExpr detects `[db exists {SQL}]` / `db exists {SQL}`
// conditions and renders them as a no-arg Go closure that runs the SQL
// against the harness db and reports whether the query returned any row
// (TCL `db exists` semantics). Returns "" when the condition is not that
// form.
func (tp *transpiler) dbExistsCondExpr(cond string) string {
	const prefix = "db exists {"
	cond = strings.TrimSpace(cond)
	if strings.HasPrefix(cond, "[") {
		if !strings.HasPrefix(cond, "["+prefix) || !strings.HasSuffix(cond, "]") {
			return ""
		}
		cond = cond[1 : len(cond)-1]
	}
	if !strings.HasPrefix(cond, prefix) {
		return ""
	}
	// Find the matching close brace of the SQL body (brace matching, not
	// Index, because the SQL may itself contain braces).
	depth := 0
	closeIdx := -1
	for i := len(prefix) - 1; i < len(cond); i++ {
		switch cond[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return ""
	}
	// Anything trailing the probe (comparison operators etc.) is not this
	// form — let the general condition builder handle it.
	if strings.TrimSpace(cond[closeIdx+1:]) != "" {
		return ""
	}
	sql := cond[len(prefix):closeIdx]
	return "func() bool { r := db.Query(" + strconv.Quote(sql) + "); return r.Error == nil && len(r.Rows) > 0 }()"
}

// xdigitCondExpr detects `[string is xdigit $x]` and renders it as a Go
// tclIsXdigit call. Returns "" when the condition is not that form.
func (tp *transpiler) xdigitCondExpr(cond string) string {
	if !strings.HasPrefix(cond, "[string is xdigit ") || !strings.HasSuffix(cond, "]") {
		return ""
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(cond, "[string is xdigit "), "]")
	inner = strings.TrimSpace(inner)
	if !strings.HasPrefix(inner, "$") {
		return ""
	}
	goVar := tclVarToGo(strings.TrimPrefix(inner, "$"))
	if !isValidGoIdent(goVar) {
		return ""
	}
	return fmt.Sprintf("tclIsXdigit(%s)", goVar)
}

// buildCondExpr converts a TCL condition expression that contains comparison
// operators into a Go boolean expression. Returns "" when no comparison
// operator is found (the caller should fall back to tclBool).
func (tp *transpiler) buildCondExpr(cond string) string {
	// If the condition contains [cmd] references, try to resolve them into
	// Go expressions and build a real comparison (e.g. {[string length $x]<256}
	// becomes len(x) < 256). Only fall back to buildStringExpr when that fails.
	if strings.Contains(cond, "[") && strings.Contains(cond, "]") {
		return tp.buildCmdCondExpr(cond)
	}

	// If the condition has compound operators (&&, ||, "and", "or"),
	// fall back to tclBool — buildCondExpr only handles single comparisons.
	if isCompoundCond(cond) {
		return ""
	}

	// Find the actual comparison operator, avoiding << and >>.
	op, idx := findComparisonOp(cond)
	if idx < 0 {
		return ""
	}
	left := strings.TrimSpace(cond[:idx])
	right := strings.TrimSpace(cond[idx+len(op):])

	// Detect string literals: if either operand is quoted with " or ',
	// treat the whole comparison as a string comparison.
	leftIsStr := isQuotedOperand(left)
	rightIsStr := isQuotedOperand(right)

	// Braced TCL list operands (e.g. {0 {}}) are string values, not numeric.
	// Treat them as string comparisons so the RHS becomes a Go quoted literal
	// instead of invalid Go (e.g. `c == {0 {} }`).
	leftHasBrace := strings.ContainsAny(left, "{}")
	rightHasBrace := strings.ContainsAny(right, "{}")

	if leftIsStr || rightIsStr || leftHasBrace || rightHasBrace {
		return tp.buildStringCond(op, left, right, leftHasBrace, rightHasBrace)
	}

	// If either side is a float literal (e.g., 8.6), numeric comparison
	// with int temps would fail. Fall back to string comparison with
	// float constants quoted as strings.
	if isFloatLiteral(left) || isFloatLiteral(right) {
		return tp.buildFloatCond(op, left, right)
	}

	return tp.buildNumericCond(op, cond, left, right)
}

// isCompoundCond reports whether a condition contains compound logical
// operators (&&, ||, "and", "or") that buildCondExpr does not handle.
func isCompoundCond(cond string) bool {
	return strings.Contains(cond, "&&") || strings.Contains(cond, "||") ||
		strings.Contains(cond, " and ") || strings.Contains(cond, " or ")
}

// isQuotedOperand reports whether an operand is quoted with " or '.
func isQuotedOperand(s string) bool {
	return (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) ||
		(strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'"))
}

// buildStringCond builds a string comparison: replace $var refs with Go
// variable names directly; braced list operands become Go quoted strings.
func (tp *transpiler) buildStringCond(op, left, right string, leftHasBrace, rightHasBrace bool) string {
	leftGo := replaceVarRefsRaw(left)
	rightGo := replaceVarRefsRaw(right)
	// Braced lists: convert to a Go quoted string.
	if leftHasBrace {
		leftGo = fmt.Sprintf("%q", strings.TrimSpace(strings.Trim(left, "{}")))
	}
	if rightHasBrace {
		rightGo = fmt.Sprintf("%q", strings.TrimSpace(strings.Trim(right, "{}")))
	}
	return fmt.Sprintf("%s %s %s", leftGo, op, rightGo)
}

// buildFloatCond builds a string comparison for float operands, quoting bare
// numeric literals so the comparison is string vs string.
func (tp *transpiler) buildFloatCond(op, left, right string) string {
	leftGo := replaceVarRefsRaw(left)
	rightGo := replaceVarRefsRaw(right)
	// Quote bare numeric literals so comparison is string vs string
	if isFloatLiteral(leftGo) {
		leftGo = fmt.Sprintf("%q", leftGo)
	}
	if isFloatLiteral(rightGo) {
		rightGo = fmt.Sprintf("%q", rightGo)
	}
	return fmt.Sprintf("%s %s %s", leftGo, op, rightGo)
}

// buildNumericCond builds a numeric comparison: extract $var names, create a
// closure with strconv.Atoi conversions, and replace $var refs with _n
// suffixed numeric temps.
func (tp *transpiler) buildNumericCond(op, cond, left, right string) string {
	vars := extractTCLVarNames(cond)
	leftGo := replaceVarRefsNumeric(left)
	rightGo := replaceVarRefsNumeric(right)

	var sb strings.Builder
	sb.WriteString("func() bool { ")
	for _, v := range vars {
		goVar := tclVarToGo(v)
		sb.WriteString(fmt.Sprintf("%s_n, _%s_e := strconv.Atoi(%s); if _%s_e != nil { return false }; ", goVar, goVar, goVar, goVar))
	}
	sb.WriteString(fmt.Sprintf("return %s %s %s }()", leftGo, op, rightGo))
	return sb.String()
}

// buildCmdCondExpr builds a Go boolean expression for a condition that
// contains [cmd] command substitution, e.g. {[string length $x] < 256}.
// Each operand is resolved via cmdExpr to a Go string expression, then the
// comparison is done numerically with strconv.Atoi conversion. Returns ""
// if the condition is not a single comparison with resolvable operands.
func (tp *transpiler) buildCmdCondExpr(cond string) string {
	// Only handle single comparisons. Compound conditions (&&, ||) and
	// logical words fall back to the tclBool path like the original code.
	if strings.Contains(cond, "&&") || strings.Contains(cond, "||") ||
		strings.Contains(cond, " and ") || strings.Contains(cond, " or ") {
		return ""
	}
	op, idx := findComparisonOp(cond)
	if idx < 0 {
		return ""
	}
	left := strings.TrimSpace(cond[:idx])
	right := strings.TrimSpace(cond[idx+len(op):])

	leftGo, ok1 := tp.cmdOperandToGo(left)
	rightGo, ok2 := tp.cmdOperandToGo(right)
	if !ok1 || !ok2 {
		return ""
	}

	// Detect string literals: if either operand is quoted with " or ',
	// treat the whole comparison as a string comparison.
	if isQuotedOperand(left) || isQuotedOperand(right) {
		return fmt.Sprintf("%s %s %s", leftGo, op, rightGo)
	}

	// Numeric comparison via Atoi conversion of both sides.
	return buildCmdNumericCond(op, leftGo, rightGo)
}

// buildCmdNumericCond builds a numeric comparison closure for a condition
// whose operands are Go string expressions (resolved via cmdExpr).
func buildCmdNumericCond(op, leftGo, rightGo string) string {
	var sb strings.Builder
	sb.WriteString("func() bool { ")
	leftName := "l"
	rightName := "r"
	sb.WriteString(fmt.Sprintf("%s_n, %s_e := strconv.Atoi(%s); if %s_e != nil { return false }; ", leftName, leftName, leftGo, leftName))
	sb.WriteString(fmt.Sprintf("%s_n, %s_e := strconv.Atoi(%s); if %s_e != nil { return false }; ", rightName, rightName, rightGo, rightName))
	sb.WriteString(fmt.Sprintf("return %s_n %s %s_n }()", leftName, op, rightName))
	return sb.String()
}

// cmdOperandToGo converts a single TCL condition operand to a Go string
// expression. It handles $var references and [cmd] command substitution.
// Returns "" when the operand cannot be resolved.
func (tp *transpiler) cmdOperandToGo(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}

	// Quoted string literal — strip quotes and re-quote as Go literal.
	if isQuotedOperand(s) {
		return fmt.Sprintf("%q", s[1:len(s)-1]), true
	}

	// Pure command substitution: [cmd ...]
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		cmdText := strings.TrimSpace(s[1 : len(s)-1])
		// [permutation] evaluates to the empty string (no test permutation),
		// so {[permutation]=="prepare"} becomes "" == "prepare" (false).
		// Resolve it here so the empty-string literal is not mistaken for
		// the unresolvable-command sentinel below.
		if strings.TrimSpace(cmdText) == "permutation" {
			return `""`, true
		}
		expr := tp.cmdExpr(cmdText)
		// Unresolvable commands fall back to the raw quoted text; treat those
		// as not-resolvable so the caller falls back to the tclBool path
		// (which preserves skip-guard behavior for unsupported commands).
		if expr == `""` || expr == fmt.Sprintf("%q", cmdText) {
			return "", false
		}
		return expr, true
	}

	// $var reference (possibly with array key).
	if strings.HasPrefix(s, "$") {
		goVar := tclVarToGo(s[1:])
		return goVar, true
	}

	// Bare literal (number or identifier).
	return fmt.Sprintf("%q", s), true
}

// isFloatLiteral returns true if s looks like a floating-point number
// (e.g., "8.6", "3.14"). Used to avoid int comparisons with float constants.
func isFloatLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	hasDigit := false
	hasDot := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			hasDigit = true
		} else if c == '.' && !hasDot {
			hasDot = true
		} else if c == '-' && i == 0 {
			// leading minus is OK
		} else {
			return false
		}
	}
	return hasDigit && hasDot
}

// findComparisonOp finds the first comparison operator in s, avoiding << and >>.
// Returns the operator and its index, or ("", -1) if not found.
func findComparisonOp(s string) (string, int) {
	// Multi-char operators first (unambiguous)
	for _, op := range []string{"<=", ">=", "!=", "=="} {
		if idx := strings.Index(s, op); idx >= 0 {
			return op, idx
		}
	}
	return findSingleCharOp(s)
}

// findSingleCharOp finds a single-char < or > operator, skipping << and >>
// pairs (which are shift operators, not comparisons).
func findSingleCharOp(s string) (string, int) {
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			// NOT part of <<
			if i+1 < len(s) && s[i+1] == '<' {
				i++ // skip the << pair
				continue
			}
			return "<", i
		}
		if s[i] == '>' {
			// NOT part of >>
			if i+1 < len(s) && s[i+1] == '>' {
				i++ // skip the >> pair
				continue
			}
			return ">", i
		}
	}
	return "", -1
}

// replaceVarRefsRaw replaces $var references with Go variable names,
// preserving all other text (operators, numbers, parens) unchanged.
// The replacement is the raw variable name (as a Go string).
func replaceVarRefsRaw(s string) string {
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		if s[pos] != '$' {
			result.WriteByte(s[pos])
			pos++
			continue
		}
		name, next, ok := scanTCLVarRef(s, pos)
		if ok {
			result.WriteString(tclVarToGo(name))
		} else {
			result.WriteByte('$')
		}
		pos = next
	}
	return result.String()
}

// replaceVarRefsNumeric replaces $var references with Go variable names
// suffixed with _n (the numeric temp variable). Use for numeric comparisons.
func replaceVarRefsNumeric(s string) string {
	var result strings.Builder
	pos := 0
	for pos < len(s) {
		if s[pos] != '$' {
			result.WriteByte(s[pos])
			pos++
			continue
		}
		name, next, ok := scanTCLVarRef(s, pos)
		if ok {
			result.WriteString(tclVarToGo(name) + "_n")
		} else {
			result.WriteByte('$')
		}
		pos = next
	}
	return result.String()
}

// scanTCLVarRef scans a $var reference starting at pos (which points at the
// '$'). Returns the variable name (with array-key syntax preserved as
// "name(key)"), the position after the reference (always past the '$'), and
// whether a valid variable reference was found.
func scanTCLVarRef(s string, pos int) (string, int, bool) {
	if s[pos] != '$' {
		return "", pos, false
	}
	if pos+1 >= len(s) {
		return "", pos + 1, false
	}
	pos++
	if pos < len(s) && s[pos] == '{' {
		return scanBracedVarRef(s, pos)
	}
	if pos >= len(s) || !isVarStartChar(s[pos]) {
		return "", pos, false
	}
	varStart := pos
	for pos < len(s) && isVarChar(s[pos]) {
		pos++
	}
	varName := s[varStart:pos]
	// Handle TCL array syntax: $var(key) → var(key)
	return appendArrayKeyToVar(s, pos, varName)
}

// scanBracedVarRef scans a ${varname} reference with pos pointing at the
// opening brace. Returns the variable name and the position after the closing
// brace.
func scanBracedVarRef(s string, pos int) (string, int, bool) {
	pos++
	varStart := pos
	for pos < len(s) && s[pos] != '}' {
		pos++
	}
	varName := s[varStart:pos]
	if pos < len(s) {
		pos++ // skip }
	}
	return varName, pos, true
}

// appendArrayKeyToVar extends varName with a TCL array key when s[pos] starts
// a $var(key) suffix, advancing pos past the closing paren.
func appendArrayKeyToVar(s string, pos int, varName string) (string, int, bool) {
	if pos < len(s) && s[pos] == '(' {
		keyStart := pos + 1
		keyEnd := keyStart
		for keyEnd < len(s) && s[keyEnd] != ')' {
			keyEnd++
		}
		if keyEnd < len(s) {
			key := s[keyStart:keyEnd]
			varName = varName + "(" + key + ")"
			pos = keyEnd + 1 // skip past )
		}
	}
	return varName, pos, true
}

// extractTCLVarNames returns all unique $var names in s (without the $).
func extractTCLVarNames(s string) []string {
	var seen = make(map[string]bool)
	var names []string
	pos := 0
	for pos < len(s) {
		if s[pos] != '$' {
			pos++
			continue
		}
		name, next, ok := scanTCLVarRef(s, pos)
		if ok && name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		pos = next
	}
	return names
}

// whileStepRowCount detects the C-API row-counting idiom
// `while {"SQLITE_ROW" == [sqlite3_step $STMT]} { incr N }` (and the
// `[sqlite3_step $STMT] == "SQLITE_ROW"` form). When the condition compares a
// prepared statement's step result to SQLITE_ROW and the body is a single
// `incr N`, it returns the prepared statement's SQL expression and the count
// variable, so the caller can emit a direct row-count query. The prepared SQL
// comes from the earlier `set ::STMT [sqlite3_prepare[_v2] db $SQL ...]`
// recording.
func (tp *transpiler) whileStepRowCount(cond string, bodyCmds [][]tcl.RawWord) (string, string, bool) {
	cond = strings.TrimSpace(cond)
	cond = stripCondBraces(cond)
	// Match: "SQLITE_ROW" == [sqlite3_step $STMT]  or
	//        [sqlite3_step $STMT] == "SQLITE_ROW"
	if !strings.Contains(cond, `"SQLITE_ROW"`) || !strings.Contains(cond, "sqlite3_step") {
		return "", "", false
	}
	stmtVar := stmtVarFromStepCond(cond)
	if stmtVar == "" {
		return "", "", false
	}
	// The body must be a single `incr N`.
	if len(bodyCmds) != 1 || len(bodyCmds[0]) < 2 || bodyCmds[0][0].Text != "incr" {
		return "", "", false
	}
	countVar := strings.TrimSpace(bodyCmds[0][1].Text)
	countVar = strings.TrimPrefix(countVar, "::")
	if !isValidGoIdent(countVar) {
		return "", "", false
	}
	// Look up the prepared statement's SQL.
	ps := tp.preparedStateRef()
	sql, ok := ps.stmts[stmtVar]
	if !ok {
		return "", "", false
	}
	return preparedStepSQLExpr(sql), countVar, true
}

// stmtVarFromStepCond extracts the prepared-statement variable name from a
// while condition of the form `"SQLITE_ROW" == [sqlite3_step $STMT]`.
func stmtVarFromStepCond(cond string) string {
	idx := strings.Index(cond, "$")
	if idx < 0 {
		return ""
	}
	rest := cond[idx+1:]
	// Skip a TCL namespace prefix (::stmt).
	rest = strings.TrimPrefix(rest, "::")
	end := len(rest)
	for i, c := range rest {
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			end = i
			break
		}
	}
	return rest[:end]
}

// preparedStepSQLExpr renders a prepared statement's recorded SQL as a Go
// expression: a TCL variable reference ($select) becomes the Go variable, a
// literal is quoted.
func preparedStepSQLExpr(sql string) string {
	if strings.HasPrefix(sql, "$") {
		gv := tclVarToGo(strings.TrimPrefix(sql, "$"))
		if isValidGoIdent(gv) {
			return gv
		}
	}
	return fmt.Sprintf("%q", sql)
}

// catchChain describes an if condition made entirely of `[catch {BODY}]`
// atoms joined by || / && (dbdata.test's extension-load guard). Each body
// must run at runtime; the condition is whether ANY/ALL of them errored.
type catchChain struct {
	atoms []string // braced catch bodies
	ops   []string // "||" / "&&" between the atoms
}

// parseCatchChain recognizes conditions consisting solely of bracketed catch
// commands combined with logical operators. Returns nil for anything else so
// the caller falls through to the generic condition translators.
func parseCatchChain(cond string) *catchChain {
	c := strings.TrimSpace(cond)
	if !strings.Contains(c, "[catch") {
		return nil
	}
	var atoms, ops []string
	var cur strings.Builder
	depthSq, depthBr := 0, 0
	flush := func() {
		tok := strings.TrimSpace(cur.String())
		cur.Reset()
		if tok != "" {
			atoms = append(atoms, tok)
		}
	}
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch ch {
		case '[':
			depthSq++
		case ']':
			depthSq--
		case '{':
			depthBr++
		case '}':
			depthBr--
		case '|', '&':
			if depthSq == 0 && depthBr == 0 && i+1 < len(c) && c[i+1] == ch {
				flush()
				ops = append(ops, string(ch)+string(ch))
				i++
				continue
			}
			return nil // single |/& or nested: not a plain chain
		}
		cur.WriteByte(ch)
	}
	flush()
	if len(atoms) < 2 || len(ops) != len(atoms)-1 {
		return nil
	}
	bodies := make([]string, 0, len(atoms))
	for _, a := range atoms {
		if !strings.HasPrefix(a, "[") || !strings.HasSuffix(a, "]") {
			return nil
		}
		inner := strings.TrimSpace(a[1 : len(a)-1])
		if !strings.HasPrefix(inner, "catch ") {
			return nil
		}
		rest := strings.TrimSpace(inner[len("catch "):])
		bodyText, tail, ok := catchBodyAndTail(rest)
		if !ok || tail != "" {
			return nil
		}
		bodies = append(bodies, bodyText)
	}
	return &catchChain{atoms: bodies, ops: ops}
}
