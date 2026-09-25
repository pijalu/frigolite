// Package main implements the tcl2go tool.
//
// This file handles the TCL catch command transpilation: the standalone
// `catch BODY ?VAR?` statement and `[catch {BODY} VAR]` arguments to list
// commands.
package main

import (
	"strings"

	"github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

func (tp *transpiler) processCatch(args []tcl.RawWord) {
	if len(args) < 1 {
		return
	}
	bodyCmds := tp.parseBracedBody(args, 0)
	if bodyCmds == nil {
		tp.emitLine("// catch (non-braced)")
		return
	}

	resultVar := "_catchResult"
	errVar := "_catchErrMsg"
	hasResult := false
	if len(args) >= 2 {
		resultVar = tclVarToGo(args[1].Text)
		hasResult = true
	}
	if len(args) >= 3 {
		errVar = tclVarToGo(args[2].Text)
	}

	tp.emitLine("{")
	tp.indent++
	if hasResult {
		if !tp.isVarDeclared(resultVar) {
			tp.emitLine("var %s string // catch result (\"0\"=ok, \"1\"=error)", resultVar)
		}
		if !tp.isVarDeclared(errVar) {
			tp.emitLine("var %s string // catch error message", errVar)
		}
		tp.emitLine("_ = %s // suppress unused warning", resultVar)
		tp.emitLine("_ = %s // suppress unused warning", errVar)
	}
	tp.emitLine("var _catchErr error")
	if !hasResult {
		tp.emitLine("_ = _catchErr // suppress unused warning")
	}
	// TCL catch compares the body's RESULT; reset the value-builtin
	// accumulator so a body whose commands assign no value (plain set/DDL)
	// yields an empty result rather than a stale `_r`.
	tp.emitLine("_r = \"\"")
	tp.runCatchBody(bodyCmds)
	if hasResult {
		tp.emitCatchResultVarPostamble(resultVar, errVar)
	}
	tp.indent--
	tp.emitLine("}")
}

// emitCatchResultVarPostamble emits the result-variable assignment after a
// `catch BODY VAR` body ran. After body, set the error message if there was
// an error. TCL catch with 2 args (`catch BODY rcVar` / `catch BODY msg` in
// a do_test like memdb1.test 150's `catch {db deserialize
// -unknown 1 $db1} msg; set msg`): the single trailing var holds
// the ERROR MESSAGE on failure ("unknown option: -unknown"),
// not the "1" code — the do_test value is that message.
// Disambiguate by the var name: `msg`/`err*` hold the message;
// anything else (rc) holds the code.
func (tp *transpiler) emitCatchResultVarPostamble(resultVar, errVar string) {
	if resultVar == "msg" || strings.HasPrefix(resultVar, "err") || strings.HasPrefix(resultVar, "_err") {
		tp.emitLine("if _catchErr != nil {")
		tp.indent++
		tp.emitLine("%s = _catchErr.Error()", resultVar)
		tp.indent--
		tp.emitLine("} else {")
		tp.indent++
		// On success TCL sets the var to the body RESULT (quote-1.3.4:
		// `catch {execsql {...}} msg` leaves the query result "hello 10"
		// in msg), not an unconditional empty string.
		tp.emitLine("%s = tclCatchStmtResult(_r)", resultVar)
		tp.indent--
		tp.emitLine("}")
		return
	}
	// Faithful TCL `catch BODY varName` semantics: varName holds
	// the error message on failure, else the body's RESULT (the
	// value the last command left in `_r`; tclCatchStmtResult maps
	// the stmt-API SQLITE_OK sentinel to TCL's empty success
	// result — sqlite3_bind_text leaves no interpreter result,
	// sqllimits1-5.14.8). The pre-2026-09 emission ("1"/"0" catch
	// codes) contradicted TCL: no `catch BODY var` ever yields
	// the numeric code in the variable.
	tp.emitLine("if _catchErr != nil {")
	tp.indent++
	tp.emitLine("%s = _catchErr.Error()", resultVar)
	tp.emitLine("%s = _catchErr.Error()", errVar)
	tp.indent--
	tp.emitLine("} else {")
	tp.indent++
	tp.emitLine("%s = tclCatchStmtResult(_r)", resultVar)
	tp.emitLine("%s = \"\"", errVar)
	tp.indent--
	tp.emitLine("}")
}

// runCatchBody transpiles a catch body in a fresh sub-transpiler with catch
// mode enabled and copies the transpiler state fields back.
func (tp *transpiler) runCatchBody(bodyCmds [][]tcl.RawWord) {
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, quotaCallbacks: tp.quotaCallbacks, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, pendingFileReset: tp.pendingFileReset, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, fixtureVar: tp.fixtureVar}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.dbClosed = bodyTP.dbClosed
	tp.dqsDDL = bodyTP.dqsDDL
	tp.dqsDML = bodyTP.dqsDML
	tp.varCount = bodyTP.varCount
	tp.queryVars = bodyTP.queryVars
	tp.unsetVars = bodyTP.unsetVars
	tp.dbVarFuncs = bodyTP.dbVarFuncs
	tp.constFuncs = bodyTP.constFuncs
	tp.dbAliases = bodyTP.dbAliases
	tp.pendingFileReset = bodyTP.pendingFileReset
	tp.varConstValues = bodyTP.varConstValues
	tp.sqlVarValues = bodyTP.sqlVarValues
	tp.foreachLitValues = bodyTP.foreachLitValues
	tp.varsetLoopVars = bodyTP.varsetLoopVars
	tp.dbConnVars = bodyTP.dbConnVars
	tp.runtimeConnVars = bodyTP.runtimeConnVars
	tp.varRenames = bodyTP.varRenames
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
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

// catchArgBraceEnd returns the index of the '}' closing the leading '{' of
// inner, or -1 when the braces are missing or unbalanced.
func catchArgBraceEnd(inner string) int {
	if !strings.HasPrefix(inner, "{") {
		return -1
	}
	depth := 0
	for i := 0; i < len(inner); i++ {
		if inner[i] == '{' {
			depth++
		} else if inner[i] == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// runListCatchBody transpiles the body of a `[catch {BODY} VAR]` list
// argument in a fresh sub-transpiler with catch mode enabled and copies the
// state fields back that the list-catch path propagates.
func (tp *transpiler) runListCatchBody(bodyCmds [][]tcl.RawWord) {
	bodyTP := &transpiler{sb: tp.sb, indent: tp.indent, dbVar: tp.dbVar, t: tp.t, catchMode: true, vars: tp.vars, forIncrs: tp.forIncrs, testPrefix: tp.testPrefix, preparedState: tp.preparedState, dbClosed: tp.dbClosed, dqsDDL: tp.dqsDDL, dqsDML: tp.dqsDML, dbAliases: tp.dbAliases, queryVars: tp.queryVars, unsetVars: tp.unsetVars, dbVarFuncs: tp.dbVarFuncs, constFuncs: tp.constFuncs, quotaCallbacks: tp.quotaCallbacks, rangeListFuncs: tp.rangeListFuncs, varCount: tp.varCount, pendingFileReset: tp.pendingFileReset, varConstValues: tp.varConstValues, sqlVarValues: tp.sqlVarValues, foreachLitValues: tp.foreachLitValues, varsetLoopVars: tp.varsetLoopVars, dbConnVars: tp.dbConnVars, runtimeConnVars: tp.runtimeConnVars, varRenames: tp.varRenames, connFailedOpen: tp.connFailedOpen, connClosed: tp.connClosed, blobChans: tp.blobChans, blobChannelVars: tp.blobChannelVars, blobVarNames: tp.blobVarNames, usedChannels: tp.usedChannels, blobSeq: tp.blobSeq, fixtureVar: tp.fixtureVar}
	bodyTP.processCommands(bodyCmds)
	tp.indent = bodyTP.indent
	tp.connFailedOpen = bodyTP.connFailedOpen
	tp.connClosed = bodyTP.connClosed
	tp.blobSeq = bodyTP.blobSeq
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
}

// emitListCatchArg handles a `[catch {BODY} VAR]` argument to a `list`
// command. It emits the catch body (side effects) and returns the Go
// expression for the catch result ("" on success, the error message on
// failure) plus the result var's string. Returns ("", false) when the arg is
// not a catch form.
func (tp *transpiler) emitListCatchArg(w tcl.RawWord) (string, bool) {
	text := strings.TrimSpace(w.Text)
	if !strings.HasPrefix(text, "[catch ") || !strings.HasSuffix(text, "]") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "[catch "), "]")
	// inner: {BODY} VAR
	braceEnd := catchArgBraceEnd(inner)
	if braceEnd < 0 {
		return "", false
	}
	bodyText := inner[1:braceEnd]
	resultVar := strings.TrimSpace(inner[braceEnd+1:])
	if resultVar == "" {
		return "", false
	}
	// Emit the catch body and capture its error into the result var.
	bodyCmds := tcl.ParseCommands(bodyText)
	tp.emitLine("_rc := \"0\"")
	tp.emitLine("{")
	tp.indent++
	tp.emitLine("var _catchErr error")
	goResult := tclVarToGo(resultVar)
	if !tp.isVarDeclared(goResult) {
		tp.emitLine("var %s string", goResult)
		tp.vars = append(tp.vars, goResult)
	}
	tp.runListCatchBody(bodyCmds)
	// TCL catch returns "1" on error, "0" on success; the error message goes
	// into the result var (goResult).
	tp.emitLine("if _catchErr != nil { %s = _catchErr.Error() } else { %s = \"\" }", goResult, goResult)
	tp.emitLine("if _catchErr != nil { _rc = \"1\" }")
	tp.indent--
	tp.emitLine("}")
	return "_rc", true
}
