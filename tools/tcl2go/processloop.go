// Package main implements the tcl2go tool.
//
// This file handles TCL for/while/if control-flow statement transpilation;
// condition-expression translation lives in processcond.go.
package main

import (
	"fmt"
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

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
	if tp.emitWhileStepRowCount(cond, bodyCmds) {
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

	// A `for true` (or `while {1}`) loop inside a catch block is meant to
	// terminate when the body errors — the TCL `catch` block would naturally
	// short-circuit on the first error. Emit an explicit break-on-error
	// at the top so the generated loop mirrors that semantics.
	if tp.catchMode && goCond == "true" {
		tp.emitLine("if _catchErr != nil { break }")
	}

	tp.runWhileBody(bodyCmds)

	tp.indent--
	tp.emitLine("}")
}

// emitWhileStepRowCount emits the direct row-count query form of a
// `while {"SQLITE_ROW" == [sqlite3_step $STMT]} { incr N }` body. Returns
// true when the condition matched the idiom (the loop is fully replaced).
func (tp *transpiler) emitWhileStepRowCount(cond string, bodyCmds [][]tcl.RawWord) bool {
	sqlExpr, countVar, ok := tp.whileStepRowCount(cond, bodyCmds)
	if !ok {
		return false
	}
	tp.emitLine("r = db.Query(%s)", sqlExpr)
	tp.emitLine("if r.Error != nil {")
	tp.emitLine("\tt.Errorf(\"query error: %%v\\n  sql: %%s\", r.Error, %s)", sqlExpr)
	tp.emitLine("\treturn")
	tp.emitLine("}")
	tp.emitLine("%s = strconv.Itoa(len(r.Rows))", countVar)
	return true
}

// runWhileBody transpiles a while body in a fresh sub-transpiler sharing the
// output buffer and state.
func (tp *transpiler) runWhileBody(bodyCmds [][]tcl.RawWord) {
	if bodyCmds == nil {
		return
	}
	bodyTP := &transpiler{
		sb:       tp.sb,
		indent:   tp.indent,
		dbVar:    tp.dbVar,
		t:        tp.t,
		varCount: tp.varCount,
		vars:     tp.vars,
		// Propagate catchMode so body statements (`db eval {SQL}`)
		// can capture Exec errors into _catchErr — required for the
		// `while 1 { db eval INSERT }` infinite-INSERT cap test
		// (tkt2686) to break on "database or disk is full".
		catchMode:    tp.catchMode,
		rollbackFlag: tp.rollbackFlag,
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

func (tp *transpiler) processIf(args []tcl.RawWord) {
	if len(args) < 2 {
		return
	}
	idx := 0
	first := true

	for idx < len(args) {
		if first {
			tp.processIfCondition(args, &idx, first)
			first = false
			continue
		}
		// After the first condition+body pair every word is a follow clause:
		// an implicit-else braced body or an else/elseif keyword. The chain
		// ends when the clause handler reports done (else/implicit-else/
		// unrecognized word); elseif loops back to the next follow clause.
		if tp.processIfFollowClause(args, &idx) {
			break
		}
	}

	tp.emitLine("}")
}

// processIfFollowClause handles the clause following a completed
// condition+body pair: TCL's implicit-else braced body, or an else/elseif
// keyword clause. Returns done=true when the if chain ends here (implicit
// else emitted, else clause emitted, or no keyword recognized — idx
// restored).
func (tp *transpiler) processIfFollowClause(args []tcl.RawWord, idx *int) bool {
	// Implicit else: any non-keyword braced word after a complete
	// condition+body pair is TCL's alternate `if {COND} {THEN} {ELSE}`.
	if args[*idx].Braced {
		bodyCmds := tp.parseBracedBody(args, *idx)
		*idx++
		tp.emitLine("} else {")
		tp.indent++
		if bodyCmds != nil {
			tp.runIfBody(bodyCmds)
		}
		tp.indent--
		return true
	}
	kw := tp.processIfKeyword(args, idx)
	if kw == ifKwElseif {
		return false
	}
	// else ends the chain; an unrecognized word (idx restored) ends it too.
	return true
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

// processIfCondition handles one if/else-if condition plus its body,
// dispatching on the condition's special forms (external-tool guard, catch
// chain, plain catch, codec probe) before the general translation.
func (tp *transpiler) processIfCondition(args []tcl.RawWord, idx *int, first bool) {
	cond := args[*idx].Text
	*idx++
	if tp.emitExternalToolGuardBranch(args, idx, cond) {
		return
	}
	if tp.emitCatchChainBranch(args, idx, cond) {
		return
	}
	if tp.emitCatchVarBranch(args, idx, cond) {
		return
	}
	if tp.emitHasCodecBranch(args, idx, cond, first) {
		return
	}
	tp.emitGenericIfBranch(args, idx, cond, first)
}

// emitExternalToolGuardBranch handles guards that test for an EXTERNAL BINARY
// via [catch {exec unzip}]: the Go port cannot run foreign executables, so
// such blocks are skipped entirely — matching a SQLite build/test environment
// without the tool (zipfile.test's ::UNZIP section). Returns true when
// handled.
func (tp *transpiler) emitExternalToolGuardBranch(args []tcl.RawWord, idx *int, cond string) bool {
	if !strings.Contains(cond, "[catch") || !strings.Contains(cond, "exec ") {
		return false
	}
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
	return true
}

// emitCatchChainBranch handles an if condition made of `[catch {BODY}]`
// atoms joined by ||/&&: every body must run at runtime and the condition is
// whether ANY/ALL of them errored. Returns true when handled.
func (tp *transpiler) emitCatchChainBranch(args []tcl.RawWord, idx *int, cond string) bool {
	chain := parseCatchChain(cond)
	if chain == nil {
		return false
	}
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
	tp.emitIfBranchBody(args, idx, fmt.Sprintf("if %s {", goCond))
	return true
}

// emitCatchVarBranch handles `if {[catch {BODY} var]}` — a runtime catch as
// the condition. The body must execute at runtime (e.g. `if {[catch {db eval
// ROLLBACK} errmsg]}` in trans3.test: the ROLLBACK runs and the condition is
// whether it errored). Returns true when handled.
func (tp *transpiler) emitCatchVarBranch(args []tcl.RawWord, idx *int, cond string) bool {
	catchVar := tp.catchCondVar(cond)
	if catchVar == "" {
		return false
	}
	tp.emitCatchForCondition(catchVar, cond)
	tp.emitIfBranchBody(args, idx, fmt.Sprintf("if %s == \"1\" {", catchVar))
	return true
}

// emitHasCodecBranch handles the `[sqlite3 -has-codec]` probe of the codec
// build flag, always false in this pure-Go port (autovacuum.test's
// ptrmap-page {207,412} branch). tclCondToGo renders the unknown command as
// false only when the whole condition fails resolution; short-circuit here so
// the if/else emits the correct (else) branch deterministically. Returns true
// when handled.
func (tp *transpiler) emitHasCodecBranch(args []tcl.RawWord, idx *int, cond string, first bool) bool {
	if strings.TrimSpace(cond) != "[sqlite3 -has-codec]" {
		return false
	}
	bodyCmds := tp.parseBracedBody(args, *idx)
	*idx++
	if first {
		tp.emitLine("if false { // [sqlite3 -has-codec] always false (no codec build)")
	} else {
		tp.emitLine("} else if false { // [sqlite3 -has-codec] always false (no codec build)")
	}
	tp.indent++
	if bodyCmds != nil {
		tp.runIfBody(bodyCmds)
	}
	tp.indent--
	return true
}

// emitGenericIfBranch emits the general if/else-if branch: the condition is
// translated via tclCondToGo and the (braced or single-command) body is
// transpiled.
func (tp *transpiler) emitGenericIfBranch(args []tcl.RawWord, idx *int, cond string, first bool) {
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

// emitIfBranchBody parses the braced body following a condition word and
// emits the given branch opener plus the transpiled body (the caller owns the
// enclosing indent/closing-brace contract).
func (tp *transpiler) emitIfBranchBody(args []tcl.RawWord, idx *int, opener string) {
	bodyCmds := tp.parseBracedBody(args, *idx)
	*idx++
	tp.emitLine("%s", opener)
	tp.indent++
	if bodyCmds != nil {
		tp.runIfBody(bodyCmds)
	}
	tp.indent--
}

// runIfBody transpiles an if/else body in a fresh sub-transpiler sharing the
// output buffer and state.
func (tp *transpiler) runIfBody(bodyCmds [][]tcl.RawWord) {
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: tp.catchMode, inDBEvalCb: tp.inDBEvalCb, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, dbClosed: tp.dbClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, specialFuncs: tp.specialFuncs, procStringMaps: tp.procStringMaps}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	tp.dbClosed = bodyTP.dbClosed
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
